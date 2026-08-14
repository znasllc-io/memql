package memql

// authoring_promote_durable.go -- DB-persisted, reviewable, restart-durable
// promotion of a session-authored construct into the shared registry
// (memql#1557, approach (b)).
//
// PromoteAuthoredConstruct (authoring_session.go) makes a validated session
// construct durable for the PROCESS: it upserts the compiled *Function / *Spec /
// *Concept into the shared e.functions / e.specs / e.concepts (core-first,
// never-shadow) so every session can call it. But that registration is in-memory
// only -- a restart loses it.
//
// This file adds the DB-persisted counterpart WITHOUT routing a plain construct
// through the automation-only Gate-2 / ActivateApprovedBundle pipeline (that
// path is structurally automation-only: PlanBundleActivation registers into the
// owner-scoped authored runtime + scheduler, which does NOT make a plain
// construct callable-by-all). Instead it:
//
//	a. relies on the Gate-1 compile/bind already performed by AuthorSessionBundle
//	   (the construct arrives already compiled -- *Function, *Spec or *Concept on
//	   .Compiled);
//	b. persists a v1:authoring:bundle + v1:authoring:construct row pair via the
//	   EXISTING #954 mutations (createAuthoringBundle /
//	   createAuthoringConstruct) -- NO new DSL -- capturing source + kind
//	   + owner + a status marker for reviewability/audit;
//	c. calls the EXISTING PromoteAuthoredConstruct so it is immediately callable
//	   by all sessions exactly as today (shared registry, core-first never-shadow);
//	d. emits an audit/log event.
//
// On boot, RehydratePromotedConstructs walks the persisted promoted bundles
// (the non-automation ones authored through this path) and recompiles each
// member into the shared registry via the same PromoteAuthoredConstruct logic,
// so promotions survive a restart -- idempotent + core-first.

import (
	"context"
	"errors"
	"fmt"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/core/id"
)

// errRehydrateSkippedDisabled is recompileAndPromoteRow's intentional-skip
// signal for a stored @disabled row (#2607/#2643): the walk counts it as
// SkippedDisabled, and the quarantine defer lets it pass -- an intentional
// disable must never read as a re-hydration failure.
var errRehydrateSkippedDisabled = errors.New("authored construct is @disabled; skipped at re-hydration (name reserved)")

// durablePromoteBundlePrefix marks a v1:authoring:bundle row as one created by
// the durable-promote path (vs. the planner-authored-automation path). The boot
// re-hydration matches on this prefix so it only recompiles promoted PLAIN
// constructs into the shared registry; automation bundles keep their existing
// owner-scoped re-arm path (authoring_rearm.go).
const durablePromoteBundlePrefix = "mcp-promote-"

// durablePromoteBundleTitle is the human-readable title stamped on a
// durable-promote bundle (surfaced on the #954 management/review surfaces).
const durablePromoteBundleTitle = "MCP durable promotion"

// AuditActionConstructPromoted is the audit action stamped when a plain
// construct is durably promoted into the shared registry. lower_snake_case to
// match the v1:identity:auditEvent.action convention.
const AuditActionConstructPromoted = "authored_construct_promoted"

// PromoteConstructDurable is the durable, reviewable, restart-surviving promote
// (memql#1557). It persists the construct as a v1:authoring:bundle +
// v1:authoring:construct row pair (reuse + audit), then makes it callable by
// every session via the existing shared-registry PromoteAuthoredConstruct.
//
// owner is the AUTHENTICATED actor promoting; it is the per-row authz owner the
// persisted rows are stamped to (the mutations stamp ownerUserId from
// actor.userId, so the write runs under the owner's envelope). The OWNER-ONLY
// gate is enforced by the caller (the MCP promote handler / promoteGate); the
// core-first never-shadow invariant is enforced by PromoteAuthoredConstruct.
//
// Ordering: it registers into the shared registry FIRST -- that is the
// authoritative gate (it enforces core-first / never-shadow and rejects a
// non-compiled construct), so a REFUSED promotion never leaves an orphan
// reviewable record. Only after the registration succeeds does it persist the
// reviewable v1:authoring:bundle + construct rows. A persist failure surfaces as
// an error (the in-process promotion is live but not yet durable -- the boot
// re-hydration simply won't find it, consistent with "not durable").
//
// A CONCEPT re-promote goes through the memql#3757 classifier under the STRICT
// posture: this entry point carries no override, so a breaking schema change is
// refused. The bundle-level PromoteBundleDurable is where an operator's
// `allow_breaking` arrives from the wire.
func (e *MemQLEngine) PromoteConstructDurable(ctx context.Context, owner string, c *AuthoredConstruct) error {
	return e.promoteConstructDurableWithStore(ctx, &enginePromoteStore{engine: e}, nil, owner, c)
}

// PromoteBundleResult reports the outcome of a bundle-level durable promote
// (issue znasllc-io/memql-cockpit#232): the per-construct compile/bind
// diagnostics, the constructs that were durably promoted into the shared
// registry, and an overall OK. OK is true only when validation passed AND every
// plain construct promoted. On a validation failure nothing is promoted and the
// diagnostics explain why; on a mid-promote failure Promoted holds the ones that
// did land before the failing one.
type PromoteBundleResult struct {
	OK          bool                `json:"ok"`
	Promoted    []DefinedConstruct  `json:"promoted,omitempty"`
	Diagnostics []SandboxDiagnostic `json:"diagnostics,omitempty"`
	// ConceptDiffs carries the memql#3757 classification of every CONCEPT the
	// bundle re-promotes over a version this cluster already runs -- both the
	// additive ones that landed and, on a refusal, the breaking one that did
	// not. Empty for a first promote and for an unchanged re-promote, which is
	// the honest answer in both cases: there was no prior version to diff, or no
	// difference to report.
	//
	// STRUCTURED rather than folded into Error, because the two consumers of
	// this (the SDK, memql#3760; the editor, memql#3763) must not have to parse
	// prose out of an error string to find out which field is the problem.
	ConceptDiffs []ConceptSchemaDiff `json:"conceptDiffs,omitempty"`
}

// PromoteBundleDurable validates a `.memql` bundle through the Gate-1 sandbox,
// then durably promotes every PLAIN construct (concept / query / mutation /
// logic / spec / trait) it defines into the shared engine registry (issue
// znasllc-io/memql-cockpit#232). It is the bundle-level entry point the
// owner-gated gRPC durable-promote handler drives, reusing the exact validate +
// compile path AuthorSessionBundle uses (so a construct arrives compiled) and
// then the per-construct PromoteConstructDurable (which persists the reviewable
// rows, registers core-first never-shadow, and broadcasts the live cross-node
// propagation event).
//
// owner is the AUTHENTICATED actor; the OWNER-ONLY gate is enforced by the
// caller (the gRPC handler matches the MCP promote owner gate). A bundle that
// fails validation promotes nothing and returns OK=false with the diagnostics +
// a non-nil error; a per-construct promote failure stops at that construct and
// returns the error (the ones already promoted stay -- each is independently
// durable + propagated).
//
// allowBreaking is the memql#3757 override, and it is the operator's explicit
// "yes, I mean it" for a CONCEPT re-promote whose schema change would strand
// rows already written. False is the normal call: a breaking change is refused
// with the classified diff on ConceptDiffs and named in the error. True lands it
// and AUDITS it as an override. It affects nothing else -- every other refusal
// this path can produce (core-first, a failed compile, a broken invariant) is
// unmoved by it, because none of them is a judgement about data.
func (e *MemQLEngine) PromoteBundleDurable(ctx context.Context, owner, bundleSource string, allowBreaking bool) (PromoteBundleResult, error) {
	return e.promoteBundleDurableWithStore(ctx, &enginePromoteStore{engine: e}, owner, bundleSource, allowBreaking)
}

// promoteBundleDurableWithStore is the store-driven core of PromoteBundleDurable,
// split out so it is unit testable with a fake promoteStore (no live DB), exactly
// like promoteConstructDurableWithStore.
func (e *MemQLEngine) promoteBundleDurableWithStore(ctx context.Context, store promoteStore, owner, bundleSource string, allowBreaking bool) (PromoteBundleResult, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return PromoteBundleResult{}, fmt.Errorf("authoring: durable promote requires an authenticated owner")
	}

	// ONE gate for the whole bundle: the operator's override applies to the
	// promote they asked for, and the diffs from every concept in it collect in
	// one place so the reply reports the bundle, not a construct.
	gate := &conceptPromoteGate{allowBreaking: allowBreaking}

	// Validate + compile via the SAME session-define path (Gate-1 sandbox, then
	// compile each function-family/spec construct into its executable form). A
	// throwaway owner-scoped registry holds the compiled forms; nothing about it
	// is durable -- it exists only to carry the *Function / *Spec into the
	// per-construct PromoteConstructDurable.
	reg := NewAuthoredRuntimeRegistry()
	defineRes, err := AuthorSessionBundle(reg, owner, bundleSource)
	result := PromoteBundleResult{OK: defineRes.OK, Diagnostics: defineRes.Diagnostics}
	if err != nil {
		// Validation/compile failure: nothing registered, nothing to promote.
		return result, err
	}

	// Promote each PLAIN construct (concept + the function family + spec/trait)
	// into the shared registry. The other recognized kinds are NOT durably
	// promoted by THIS path, by deliberate design (E1 #2372):
	//
	//   - shape registers as session metadata only;
	//   - automation / action / capability are the world-touching deployment
	//     kinds. Their LIVE activation (an automation's scheduler trigger
	//     registration + boot re-arm) is the separate owner-gated Gate-3 path
	//     (ActivateApprovedBundle, authoring_activation_engine.go), which owns
	//     the scheduler RegisterHook + the reviewable dryRunPassed lifecycle.
	//     Forking scheduler wiring into this plain-construct fast-path would
	//     double-register automations, so they are intentionally left to that
	//     path here.
	//
	// Crucially this is NOT a silent drop: the validation above sliced + Gate-1
	// compiled EVERY kind (including automation/action/capability), so each
	// appears in result.Diagnostics with its OK/skip status -- a broken
	// automation in the bundle fails the whole promote loudly rather than being
	// ignored while the plain constructs promote (the pre-E1 silent-skip bug).
	for _, c := range SplitBundleSource(bundleSource) {
		if !isDurablePromotableKind(c.Kind) {
			continue
		}
		ac, ok := reg.Lookup(owner, c.Kind, c.Name)
		if !ok {
			// The compile step above did not register it (an unsupported kind
			// the splitter surfaced but the compiler skipped); leave it out.
			continue
		}
		if perr := e.promoteConstructDurableWithStore(ctx, store, gate, owner, ac); perr != nil {
			result.OK = false
			// The diffs go out on the FAILURE reply too, and that is the whole
			// point of collecting them structurally: a breaking refusal IS a
			// diff, and a client that could only read the error string would
			// have to regex a field name out of a rendered block.
			result.ConceptDiffs = gate.collected()
			return result, fmt.Errorf("authoring: durable promote %s %q: %w", c.Kind, c.Name, perr)
		}
		result.Promoted = append(result.Promoted, DefinedConstruct{Kind: c.Kind, Name: c.Name})
	}
	result.ConceptDiffs = gate.collected()
	return result, nil
}

// promoteStore is the narrow graph surface the durable promote needs: persist a
// bundle row + a construct row through the existing #954 mutations. *MemQLEngine
// satisfies it via enginePromoteStore; tests fake it.
type promoteStore interface {
	// CreatePromoteBundle persists a v1:authoring:bundle row for a durable
	// promotion (status "active", marked via the durable-promote prefix/title).
	CreatePromoteBundle(ctx context.Context, bundleId, title, summary string) error
	// CreatePromoteConstruct persists a v1:authoring:construct row capturing the
	// promoted construct's kind + name + source (status "active").
	CreatePromoteConstruct(ctx context.Context, constructId, bundleId, kind, name, targetNamespace, source string) error
}

// promoteConstructDurableWithStore is the store-driven core, split out so it is
// unit testable with a fake promoteStore (no live DB).
func (e *MemQLEngine) promoteConstructDurableWithStore(ctx context.Context, store promoteStore, gate *conceptPromoteGate, owner string, c *AuthoredConstruct) error {
	if c == nil {
		return fmt.Errorf("authoring: durable promote requires a construct")
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return fmt.Errorf("authoring: durable promote requires an authenticated owner")
	}
	if !isDurablePromotableKind(c.Kind) {
		return fmt.Errorf("authoring: durable promotion of %s constructs is not supported (concept + function-family + spec only)", c.Kind)
	}
	// Gate 1 was already run by AuthorSessionBundle when the construct was
	// defined (it arrives compiled). Confirm the compiled form is present so we
	// never persist a reviewable record for something that won't register.
	if c.Compiled == nil {
		return fmt.Errorf("authoring: durable promote %s %q: construct is not compiled (define it first)", c.Kind, c.Name)
	}

	// Register into the shared registry FIRST (core-first, never-shadow). This is
	// the existing process-shared promote -- identical call-by-name semantics --
	// and the authoritative gate, so a refused promotion never persists.
	//
	// The memql#3757 concept schema classification runs INSIDE this call, which
	// is what puts it ahead of everything below: a breaking change refused here
	// writes no reviewable row and publishes no propagation broadcast, so a
	// refusal on one node cannot leave a peer holding the schema it refused.
	if err := e.promoteAuthoredConstructWithGate(ctx, gate, c); err != nil {
		return err
	}

	// Persist under the owner's envelope so the #954 mutations stamp
	// ownerUserId == owner (per-row authz owner) -- mirrors how the activation
	// store runs its writes.
	persistCtx := auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
	bundleId := durablePromoteBundlePrefix + id.NewShortId()
	constructId := durablePromoteBundlePrefix + c.Kind + "-" + id.NewShortId()
	summary := fmt.Sprintf("Durable promotion of %s %q into the shared registry (callable by all sessions).", c.Kind, c.Name)

	if err := store.CreatePromoteBundle(persistCtx, bundleId, durablePromoteBundleTitle, summary); err != nil {
		return fmt.Errorf("authoring: persist promote bundle: %w", err)
	}
	targetNamespace := promoteTargetNamespace(c)
	if err := store.CreatePromoteConstruct(persistCtx, constructId, bundleId, c.Kind, c.Name, targetNamespace, c.Source); err != nil {
		return fmt.Errorf("authoring: persist promote construct: %w", err)
	}

	if e.Component != nil && e.Logger != nil {
		e.Logger.Info("authored construct durably promoted into the shared registry",
			"owner", owner, "kind", c.Kind, "name", c.Name, "bundleId", bundleId, "action", AuditActionConstructPromoted)
	}

	// Live cross-node propagation (issue znasllc-io/memql-cockpit#232). The
	// construct is now durable + callable ON THIS NODE, but every other node's
	// shared registry is still in-memory-only and does not know about it -- so
	// without this broadcast the promote would not become callable elsewhere
	// until a restart re-hydrates it. Emit the dedicated authoring-promote
	// broadcast carrying the persisted bundle id + owner; every node receives it
	// (single broadcast routing rule) and re-hydrates this bundle's constructs
	// from the shared DB into its own registry within seconds. Best-effort: a
	// missing event bus (a non-mesh / unit-test engine) degrades to the local
	// promote only -- the persisted row still re-hydrates on the next boot.
	e.publishAuthoringPromote(bundleId, owner)
	return nil
}

// publishAuthoringPromote emits the dedicated authoring-promote broadcast event
// (issue znasllc-io/memql-cockpit#232) for a durably-promoted bundle on the
// separate authoring.promote.<bundleId> channel. ONLY the authoring-promote
// subscriber consumes this channel, and a single broadcast routing rule
// (authoring.promote.*) forwards it to every node -- so a promote on one node
// re-hydrates the bundle into every node's shared registry with zero side
// effects (no automations, no other consumers). The payload carries the bundle
// id + owner so the receiving node can load the bundle's persisted constructs.
func (e *MemQLEngine) publishAuthoringPromote(bundleId, owner string) {
	if e == nil || e.eventBus == nil {
		return
	}
	bundleId = strings.TrimSpace(bundleId)
	if bundleId == "" {
		return
	}
	e.eventBus.Publish(events.NewEvent(
		events.TopicAuthoringPromoteForBundle(bundleId),
		events.KindAuthoringPromote,
		map[string]any{"bundleId": bundleId, "owner": owner},
	))
}

// isDurablePromotableKind reports whether a construct kind can be durably
// promoted into the shared registry -- concept, the function family, and
// spec/trait -- matching PromoteAuthoredConstruct's supported set.
//
// `concept` is the memql#3746 addition and the anchor of the training epic: a
// customer's domain arrives as nouns first, and every other kind binds to one.
//
// The DEMOTE path shares this predicate, and for `concept` the two now differ:
// promotable, not yet demotable. That asymmetry is deliberate rather than an
// oversight -- withdrawing a concept would strand rows already written under it,
// so its semantics are their own decision (memql#3756). The predicate is left
// shared because the refusal it lets through is the RIGHT one: a demote of a
// concept reaches DemoteAuthoredConstruct's explicit concept branch, which names
// the reason, instead of dying against a generic "unsupported kind" here.
func isDurablePromotableKind(kind string) bool {
	switch kind {
	case "concept", "query", "mutation", "logic", "spec", "trait":
		return true
	default:
		return false
	}
}

// promoteTargetNamespace derives the v1:authoring:construct.targetNamespace for a
// durable promotion. A promoted plain construct registers into the SHARED engine
// registry (not an owner-scoped namespace), so the namespace is informational:
// the construct's authored:<kind>:<name> tag, mirroring the compile path.
func promoteTargetNamespace(c *AuthoredConstruct) string {
	return "authored:" + c.Kind + ":" + c.Name
}

// RehydratePromotedConstructs re-registers every durably-promoted PLAIN
// construct into the shared engine registry on boot, so a promotion survives a
// restart (memql#1557). It enumerates the system-active authoring bundles
// (admin-scoped, the same query the automation re-arm uses), keeps only the ones
// the durable-promote path created, recompiles each member construct's source,
// and registers it via the same PromoteAuthoredConstruct logic.
//
// It is idempotent (re-registering a still-registered promoted construct is a
// no-op-equivalent upsert) and core-first (PromoteAuthoredConstruct refuses to
// shadow a sealed core construct). A per-construct failure is logged and skipped
// (the others still come back). It does NOT touch automation bundles -- those
// keep their owner-scoped re-arm path (RearmActiveBundles).
func (e *MemQLEngine) RehydratePromotedConstructs(ctx context.Context) (RehydrateResult, error) {
	return e.rehydratePromotedNow(ctx, &enginePromoteRehydrateStore{engine: e})
}

// RehydrateResult reports what a boot re-hydration did: how many promoted
// constructs were seen, how many re-registered, and the (kind:name) of the ones
// that failed (so the boot log records exactly which promotions did not come
// back).
type RehydrateResult struct {
	Seen       int      `json:"seen"`
	Rehydrated int      `json:"rehydrated"`
	Failed     []string `json:"failed,omitempty"`
	// SkippedDisabled counts stored @disabled rows: intentional skips (name
	// reserved, nothing registered), neither re-hydrated nor failed
	// (memql#2643).
	SkippedDisabled int `json:"skippedDisabled,omitempty"`
}

// promoteRehydrateStore is the narrow graph surface the boot re-hydration needs:
// enumerate the system-active durable-promote bundles, and load one bundle's
// member constructs. *MemQLEngine satisfies it via enginePromoteRehydrateStore;
// tests fake it.
type promoteRehydrateStore interface {
	// LoadPromotedBundles returns every active durable-promote bundle across all
	// owners (admin-scoped), filtered to the ones this path created.
	LoadPromotedBundles(ctx context.Context) ([]AuthoringBundleRow, error)
	// LoadConstructsForBundle returns a bundle's member constructs, read under
	// the bundle owner's envelope (owner-scoped query).
	LoadConstructsForBundle(ctx context.Context, owner, bundleId string) ([]AuthoringConstructRow, error)
}

// rehydratePromotedConstructsWithStore is the store-driven core, split out so it
// is unit testable with a fake promoteRehydrateStore (no live DB).
func rehydratePromotedConstructsWithStore(ctx context.Context, store promoteRehydrateStore, opts ...rehydrateOption) (RehydrateResult, error) {
	cfg := rehydrateConfig{promote: nil}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.promote == nil {
		return RehydrateResult{}, fmt.Errorf("authoring: re-hydration requires a recompile-and-promote step")
	}
	bundles, err := store.LoadPromotedBundles(ctx)
	if err != nil {
		return RehydrateResult{}, fmt.Errorf("authoring: enumerate promoted bundles for re-hydration: %w", err)
	}
	var result RehydrateResult
	for _, bundle := range bundles {
		constructs, cerr := store.LoadConstructsForBundle(ctx, bundle.OwnerUserId, bundle.Id)
		if cerr != nil {
			result.Failed = append(result.Failed, "bundle:"+bundle.Id)
			continue
		}
		for _, row := range constructs {
			if !isDurablePromotableKind(row.Kind) {
				continue
			}
			if isRetiredConstructStatus(row.Status) {
				// A demoted (retired) construct must NOT re-register after a
				// restart -- skip it so the demote survives the boot walk.
				continue
			}
			result.Seen++
			if perr := cfg.promote(ctx, row); perr != nil {
				if errors.Is(perr, errRehydrateSkippedDisabled) {
					// Intentional skip (#2607/#2643): the stored row is
					// @disabled -- name reserved, nothing registered, and
					// deliberately NOT a Failed entry.
					result.SkippedDisabled++
					continue
				}
				result.Failed = append(result.Failed, row.Kind+":"+row.Name)
				continue
			}
			result.Rehydrated++
		}
	}
	return result, nil
}

// rehydrateConfig + rehydrateOption let the engine inject its
// recompile-and-promote step while keeping the walk logic store-driven and
// testable.
type rehydrateConfig struct {
	promote func(ctx context.Context, row AuthoringConstructRow) error
}

type rehydrateOption func(*rehydrateConfig)

func withRehydratePromote(fn func(ctx context.Context, row AuthoringConstructRow) error) rehydrateOption {
	return func(c *rehydrateConfig) { c.promote = fn }
}

// explainStaleGrammarStamp decorates a recompile failure with the
// grammar-version diagnosis when the row's stamp predates this engine's grammar
// (S6, memql#2361; ordering fixed in memql#3089).
//
// This is the whole of what the stamp is FOR. It does not decide whether the row
// loads -- the compile already decided that -- it decides what the operator is
// told about a row that failed. A stale stamp turns "parse error at line 7,
// column 71" into "authored under grammar X, this engine runs Y, run
// memqlmigrate", which is the actionable form.
//
// Two cases deliberately pass through undecorated:
//
//   - a row with NO stamp (legacy, pre-#2361). Nothing is known about which
//     grammar it was written against, so naming a migration would be a guess.
//   - a row stamped with the CURRENT grammar. Its source failing to compile is a
//     real defect in the source or a missing dependency, and blaming a grammar
//     move that did not happen would send the reader somewhere useless.
func explainStaleGrammarStamp(row AuthoringConstructRow, err error) error {
	if err == nil || row.GrammarVersion == "" || row.GrammarVersion == languageParser.GrammarVersion {
		return err
	}
	return fmt.Errorf("authored construct %s %q was authored under grammar %q; this engine runs %q -- run `memqlmigrate --rewrite` for the intervening grammar epics against the bundle source and re-promote (S6, memql#2361). It no longer recompiles: %w",
		row.Kind, row.Name, row.GrammarVersion, languageParser.GrammarVersion, err)
}

// recompileAndPromoteRow recompiles one persisted promoted construct's source
// back into its compiled *Function / *Spec and registers it into the shared
// registry via PromoteAuthoredConstruct (core-first, never-shadow). It is the
// non-automation counterpart to registerBundleConstructs.
func (e *MemQLEngine) recompileAndPromoteRow(ctx context.Context, row AuthoringConstructRow) (err error) {
	// Durable-bundle re-hydration posture (epic #2351 / S2, memql#2357): a
	// STORED authored construct whose source no longer recompiles is
	// QUARANTINED, not fatal. Stored rows can rot when the grammar moves (a
	// construct authored under an older grammar, a since-deleted concept
	// dependency); failing boot on a rotted stored row would let one bad
	// row brick a whole fleet -- the opposite of the strict-boot posture
	// for IN-TREE constructs, which must all load. So on any recompile /
	// promote failure we ERROR-log the construct + record a quarantine
	// entry on the engine's load report and let the walk continue. The
	// grammar-version stamp (#2361) will let a future engine recognise
	// which stored rows predate a grammar move. This defer covers BOTH the
	// boot walk and the live authoring-promote propagation path, which both
	// funnel here.
	defer func() {
		if err != nil && !errors.Is(err, errRehydrateSkippedDisabled) {
			e.quarantineRehydratedConstruct(row, err)
		}
	}()
	// S6 (#2361), ORDERING INVERTED by memql#3089: the grammar stamp does not
	// GATE the recompile, it EXPLAINS one that failed.
	//
	// It used to compare the stamp and return BEFORE attempting any compile.
	// That ordering is what made a GrammarVersion bump destructive: every
	// durable v1:authoring:construct row carrying the prior stamp was
	// unregistered on first boot even though its source still parsed perfectly.
	// So the price of bumping the constant was losing every stored construct,
	// and the constant duly stopped being bumped -- six grammar moves in six
	// weeks, in both directions, with this comparison always equal and the guard
	// therefore never firing at all. An epoch allowlist and a restamp pass were
	// both proposed to route around that wire; fixing the ordering removes the
	// need for either.
	//
	// Now: COMPILE FIRST. If the source still compiles, the row registers
	// whatever its stamp says -- a valid stored construct can never be lost to a
	// bump. If the compile FAILS and the stamp is stale, the stale stamp is the
	// actionable diagnosis and replaces whatever downstream parse error the
	// stale source produced with the named migration command, which is exactly
	// what the paragraph above says the mechanism is for. A compile failure
	// under the CURRENT stamp keeps its own error, because a grammar move is not
	// what went wrong.
	//
	// Applied at each compile-failure site below via explainStaleGrammarStamp.
	sc := SandboxConstruct{Name: row.Name, Kind: row.Kind, Source: row.Source}
	c := &AuthoredConstruct{
		OwnerUserId: row.OwnerUserId,
		Kind:        row.Kind,
		Name:        row.Name,
		BundleId:    row.BundleId,
		Source:      row.Source,
		Status:      AuthoredActive,
	}
	switch row.Kind {
	case "query", "mutation", "logic":
		// Bind against a clone of THIS ENGINE's live concept registry.
		//
		// This used to clone the package-level default, justified by "a promoted
		// construct's concept deps are core; bundle-defined concepts are not
		// durably promoted by this path". That was true until memql#3746 made
		// concepts promotable, and is now exactly backwards: the first thing
		// anyone promotes a concept FOR is a query or mutation bound to it, and
		// a promoted concept lives in whichever registry THIS engine holds. In
		// production the two are the same object so nothing changes; on an
		// engine bound to a clone (BuildOfflineSense, tests) it is the
		// difference between the binding resolving and not.
		fn, err := compileAuthoredFunction(sc, e.conceptBindingRegistry())
		if err != nil {
			return explainStaleGrammarStamp(row, fmt.Errorf("recompile %s %q: %w", row.Kind, row.Name, err))
		}
		c.Compiled = fn
	case "spec", "trait":
		spec, err := compileAuthoredSpec(sc)
		if err != nil {
			return explainStaleGrammarStamp(row, fmt.Errorf("recompile %s %q: %w", row.Kind, row.Name, err))
		}
		if spec == nil {
			// compileAuthoredSpec's (nil, nil) is the #2607 intentional-skip
			// contract: the stored row is @disabled. An intentional disable
			// must NOT read as a failure -- skip re-registration without the
			// quarantine channel, and reserve the name so an authored promote
			// cannot claim it (the resurrection guard, memql#2643).
			//
			// The reservation may only claim UNOWNED territory. A stale
			// @disabled row must never disable a name that is LIVE in the
			// registry (a core spec of the same name, or the author's own
			// corrected re-promote on a later walk tick), and must never
			// overwrite an existing reservation's provenance (a core
			// retirement is permanent by design; planting the authored
			// marker over it would let a later promote lift it -- or let
			// demote remove a core spec).
			if e.specs != nil {
				_, live := e.specs.Lookup(row.Name)
				if !live && !e.specs.IsDisabled(row.Name) {
					e.specs.MarkDisabled(row.Name)
					// Record the reservation as AUTHORED (unlike a
					// core-loader reservation) so the owner's lifecycle can
					// release it: promoting the corrected (enabled) source
					// re-enables the name, demoting it retires it. Without
					// the marker the name would be permanently dead -- no
					// API path lifts a core reservation.
					// A RESERVATION, not a registration: nothing is in the
					// registry under this name, so the marker carries no
					// source hash. The catalog never reads it -- it looks the
					// marker up only for names it found in a registry.
					e.promotedAuthored.Store("spec:"+row.Name, promotedMarker{})
				}
			}
			if e.Component != nil && e.Logger != nil {
				e.Logger.Info("durable authored construct is @disabled; skipped at re-hydration (nothing registered)",
					"component", "memql.engine", "kind", row.Kind, "name", row.Name, "bundleId", row.BundleId, "owner", row.OwnerUserId)
			}
			return errRehydrateSkippedDisabled
		}
		c.Compiled = spec
	case "concept":
		// memql#3746. The stored source is re-compiled through the same
		// buildCandidateConcept the session define and Gate 1 use, so a
		// re-hydrated concept is subject to every check a freshly-promoted one
		// is -- including the reserved-property refusal, which no separate
		// re-hydration-side guard therefore has to restate.
		//
		// The persisted row's Name is the DECLARATION name; the canonical id is
		// re-derived from the source's @namespace, exactly as at promote time.
		// That keeps the id a property of the source rather than of the row, so
		// there is no second place for it to be stored wrong.
		concept, err := compileAuthoredConcept(sc)
		if err != nil {
			return explainStaleGrammarStamp(row, fmt.Errorf("recompile %s %q: %w", row.Kind, row.Name, err))
		}
		c.Compiled = concept
	default:
		return fmt.Errorf("re-hydration of %s constructs is not supported", row.Kind)
	}
	// REPLAY, not a decision (memql#3757). This one function serves both paths
	// that re-apply a promote somebody already took a decision about -- the boot
	// walk over the durable bundles, and the live cross-node propagation
	// subscriber -- and neither may re-run the concept schema classifier.
	//
	// Boot: a re-promote writes a NEW bundle row rather than editing the old
	// one, so the walk replays the versions in sequence. Classifying v2 against
	// v1 would let a breaking change an operator deliberately overrode refuse to
	// come back after a restart -- the cluster would silently revert to the
	// older concept.
	//
	// Propagation: the originating node already classified. A peer re-deciding
	// could only ever disagree with it, and a peer that refuses what the origin
	// accepted is a split registry, which is the exact failure the broadcast
	// exists to prevent.
	return e.promoteAuthoredConstructWithGate(ctx, conceptPromoteReplay(), c)
}

// quarantineRehydratedConstruct records a durable-bundle re-hydration
// failure (epic #2351 / S2, memql#2357): it ERROR-logs the construct that
// could not be recompiled + registered from its STORED source and appends
// a QuarantinedConstruct to the engine's load report. It deliberately does
// NOT fail boot -- a rotted stored authored row must not brick a fleet the
// way an in-tree construct drop does. Callable from the boot walk and the
// live authoring-promote propagation subscriber; the report append is
// mutex-guarded for the concurrent-propagation case.
func (e *MemQLEngine) quarantineRehydratedConstruct(row AuthoringConstructRow, cause error) {
	if e == nil || cause == nil {
		return
	}
	if e.Component != nil && e.Logger != nil {
		e.Logger.Error("durable authored construct quarantined at re-hydration (stored source failed to recompile; not re-registered, boot continues)",
			"component", "memql.engine",
			"kind", row.Kind,
			"name", row.Name,
			"bundleId", row.BundleId,
			"owner", row.OwnerUserId,
			"error", cause)
	}
	e.loadReport.AddQuarantine(QuarantinedConstruct{
		Kind:     row.Kind,
		Name:     row.Name,
		BundleId: row.BundleId,
		Owner:    row.OwnerUserId,
		Err:      cause.Error(),
	})
}

// --- production stores over a live engine ---

// enginePromoteStore is the production promoteStore over a live engine. It runs
// the #954 lifecycle mutations through the engine's normal Execute path, exactly
// like engineActivationStore.
//
// # Call form: named args, not an object literal (memql#3757)
//
// The three writes below used to marshal their arguments to JSON and hand the
// result over as `createAuthoringBundle({...})`. That form was REMOVED from the
// language in #2335, and the parser now refuses it by name -- so every durable
// promote against a real engine failed at `persist promote bundle` with a parse
// error, after the construct had already been registered into the shared
// registry. In-process promotion worked; nothing was ever persisted; nothing was
// re-hydrated at the next boot.
//
// It survived because the only coverage this store had was a fake. Every test in
// the durable-promote family substitutes fakePromoteStore, which takes the
// arguments as Go values and never builds a call string at all, so the one line
// that had to change when the grammar moved was the one line no test executed.
// The neighbouring `activateAuthoringBundle` call was already in the named-arg
// form and worked -- the two forms sat four lines apart.
//
// Found by the memql#3757 row-count test, which is the first thing in this
// package to drive the real store against a real database.
type enginePromoteStore struct {
	engine *MemQLEngine
}

// mutationCall renders `mutation <name>(k: "v", ...)` -- the #2335 kind-prefixed
// named-argument form -- with every value quoted through the parser's own
// quoter, so a construct's SOURCE (which is arbitrary text full of quotes,
// braces and newlines) can ride through as an argument.
func mutationCall(name string, args ...[2]string) string {
	var b strings.Builder
	b.WriteString("mutation ")
	b.WriteString(name)
	b.WriteByte('(')
	for i, kv := range args {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(kv[0])
		b.WriteString(": ")
		b.WriteString(languageParser.QuoteString(kv[1]))
	}
	b.WriteByte(')')
	return b.String()
}

func (s *enginePromoteStore) CreatePromoteBundle(ctx context.Context, bundleId, title, summary string) error {
	if _, err := s.engine.Execute(ctx, mutationCall("createAuthoringBundle",
		[2]string{"bundleId", bundleId},
		[2]string{"title", title},
		[2]string{"summary", summary},
	)); err != nil {
		return err
	}
	// createAuthoringBundle inserts status "draft"; transition it to
	// active so the boot re-hydration's system-active enumeration picks it up.
	if _, err := s.engine.Execute(ctx, fmt.Sprintf(`mutation activateAuthoringBundle(bundleId:%s)`, languageParser.QuoteString(bundleId))); err != nil {
		return err
	}
	return nil
}

func (s *enginePromoteStore) CreatePromoteConstruct(ctx context.Context, constructId, bundleId, kind, name, targetNamespace, source string) error {
	if _, err := s.engine.Execute(ctx, mutationCall("createAuthoringConstruct",
		[2]string{"constructId", constructId},
		[2]string{"bundleId", bundleId},
		[2]string{"kind", kind},
		[2]string{"name", name},
		[2]string{"targetNamespace", targetNamespace},
		[2]string{"source", source},
		// S6 (#2361): stamp the grammar epoch the source was authored
		// under, so a future engine can tell a rotted row from a stale one.
		[2]string{"grammarVersion", languageParser.GrammarVersion},
	)); err != nil {
		return err
	}
	// Flip the construct to active so it reads as a live promoted record.
	if _, err := s.engine.Execute(ctx, mutationCall("setConstructStatus",
		[2]string{"constructId", constructId},
		[2]string{"status", "active"},
	)); err != nil {
		return err
	}
	return nil
}

// enginePromoteRehydrateStore is the production promoteRehydrateStore. The
// enumeration runs under a cluster-owner envelope (admin-scoped system query);
// the per-bundle construct load runs under the bundle owner's envelope
// (owner-scoped query) -- mirroring engineRearmStore.
type enginePromoteRehydrateStore struct {
	engine *MemQLEngine
}

func (s *enginePromoteRehydrateStore) LoadPromotedBundles(ctx context.Context) ([]AuthoringBundleRow, error) {
	ownerCtx := rearmClusterOwnerContext(ctx)
	res, err := s.engine.Execute(ownerCtx, "systemActiveAuthoringBundles()")
	if err != nil {
		return nil, err
	}
	if res == nil || res.Bundle == nil {
		return nil, nil
	}
	out := make([]AuthoringBundleRow, 0, len(res.Bundle.Nodes))
	for _, node := range res.Bundle.Nodes {
		if !strings.HasPrefix(node.GetId(), durablePromoteBundlePrefix) {
			continue // not a durable-promote bundle (automation bundles re-arm elsewhere)
		}
		payload, err := nodePayloadJSON(node)
		if err != nil {
			return nil, err
		}
		row, err := parseBundleRow(node.GetId(), payload)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

func (s *enginePromoteRehydrateStore) LoadConstructsForBundle(ctx context.Context, owner, bundleId string) ([]AuthoringConstructRow, error) {
	authorCtx := auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
	q := fmt.Sprintf(`query authoringConstructsForBundle(bundleId:%s)`, languageParser.QuoteString(bundleId))
	res, err := s.engine.Execute(authorCtx, q)
	if err != nil {
		return nil, err
	}
	if res == nil || res.Bundle == nil {
		return nil, nil
	}
	out := make([]AuthoringConstructRow, 0, len(res.Bundle.Nodes))
	for _, node := range res.Bundle.Nodes {
		payload, err := nodePayloadJSON(node)
		if err != nil {
			return nil, err
		}
		row, err := parseConstructRow(node.GetId(), payload)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

// rehydratePromotedNow is the engine-bound invocation used at boot: it wires the
// engine's recompileAndPromoteRow into the store-driven walk.
func (e *MemQLEngine) rehydratePromotedNow(ctx context.Context, store promoteRehydrateStore) (RehydrateResult, error) {
	return rehydratePromotedConstructsWithStore(ctx, store, withRehydratePromote(e.recompileAndPromoteRow))
}
