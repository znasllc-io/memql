package memql

// authoring_promote_durable.go -- DB-persisted, reviewable, restart-durable
// promotion of a session-authored construct into the shared registry
// (memql#1557, approach (b)).
//
// PromoteAuthoredConstruct (authoring_session.go) makes a validated session
// construct durable for the PROCESS: it upserts the compiled *Function / *Spec
// into the shared e.functions / e.specs (core-first, never-shadow) so every
// session can call it. But that registration is in-memory only -- a restart
// loses it.
//
// This file adds the DB-persisted counterpart WITHOUT routing a plain construct
// through the automation-only Gate-2 / ActivateApprovedBundle pipeline (that
// path is structurally automation-only: PlanBundleActivation registers into the
// owner-scoped authored runtime + scheduler, which does NOT make a plain
// construct callable-by-all). Instead it:
//
//	a. relies on the Gate-1 compile/bind already performed by AuthorSessionBundle
//	   (the construct arrives already compiled -- *Function or *Spec on .Compiled);
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
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/core/id"
)

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
func (e *MemQLEngine) PromoteConstructDurable(ctx context.Context, owner string, c *AuthoredConstruct) error {
	return e.promoteConstructDurableWithStore(ctx, &enginePromoteStore{engine: e}, owner, c)
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
}

// PromoteBundleDurable validates a `.memql` bundle through the Gate-1 sandbox,
// then durably promotes every PLAIN construct (query / mutation / logic / spec /
// trait) it defines into the shared engine registry (issue
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
func (e *MemQLEngine) PromoteBundleDurable(ctx context.Context, owner, bundleSource string) (PromoteBundleResult, error) {
	return e.promoteBundleDurableWithStore(ctx, &enginePromoteStore{engine: e}, owner, bundleSource)
}

// promoteBundleDurableWithStore is the store-driven core of PromoteBundleDurable,
// split out so it is unit testable with a fake promoteStore (no live DB), exactly
// like promoteConstructDurableWithStore.
func (e *MemQLEngine) promoteBundleDurableWithStore(ctx context.Context, store promoteStore, owner, bundleSource string) (PromoteBundleResult, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return PromoteBundleResult{}, fmt.Errorf("authoring: durable promote requires an authenticated owner")
	}

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

	// Promote each PLAIN construct (the function family + spec/trait). Concepts
	// and shapes register as session metadata but are not durably promotable
	// here -- skip them, exactly as the durable-promote kind gate does.
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
		if perr := e.promoteConstructDurableWithStore(ctx, store, owner, ac); perr != nil {
			result.OK = false
			return result, fmt.Errorf("authoring: durable promote %s %q: %w", c.Kind, c.Name, perr)
		}
		result.Promoted = append(result.Promoted, DefinedConstruct{Kind: c.Kind, Name: c.Name})
	}
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
func (e *MemQLEngine) promoteConstructDurableWithStore(ctx context.Context, store promoteStore, owner string, c *AuthoredConstruct) error {
	if c == nil {
		return fmt.Errorf("authoring: durable promote requires a construct")
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return fmt.Errorf("authoring: durable promote requires an authenticated owner")
	}
	if !isDurablePromotableKind(c.Kind) {
		return fmt.Errorf("authoring: durable promotion of %s constructs is not supported (function-family + spec only)", c.Kind)
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
	if err := e.PromoteAuthoredConstruct(ctx, c); err != nil {
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
// promoted into the shared registry (the function family + spec/trait), matching
// PromoteAuthoredConstruct's supported set.
func isDurablePromotableKind(kind string) bool {
	switch kind {
	case "query", "mutation", "logic", "spec", "trait":
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
			result.Seen++
			if perr := cfg.promote(ctx, row); perr != nil {
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

// recompileAndPromoteRow recompiles one persisted promoted construct's source
// back into its compiled *Function / *Spec and registers it into the shared
// registry via PromoteAuthoredConstruct (core-first, never-shadow). It is the
// non-automation counterpart to registerBundleConstructs.
func (e *MemQLEngine) recompileAndPromoteRow(ctx context.Context, row AuthoringConstructRow) error {
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
		// Bind against the live concept registry clone (core + nothing else --
		// a promoted construct's concept deps are core; bundle-defined concepts
		// are not durably promoted by this path).
		_, concepts := compileBundle([]SandboxConstruct{sc})
		fn, err := compileAuthoredFunction(sc, concepts)
		if err != nil {
			return fmt.Errorf("recompile %s %q: %w", row.Kind, row.Name, err)
		}
		c.Compiled = fn
	case "spec", "trait":
		spec, err := compileAuthoredSpec(sc)
		if err != nil {
			return fmt.Errorf("recompile %s %q: %w", row.Kind, row.Name, err)
		}
		c.Compiled = spec
	default:
		return fmt.Errorf("re-hydration of %s constructs is not supported", row.Kind)
	}
	return e.PromoteAuthoredConstruct(ctx, c)
}

// --- production stores over a live engine ---

// enginePromoteStore is the production promoteStore over a live engine. It runs
// the #954 lifecycle mutations through the engine's normal Execute path, exactly
// like engineActivationStore.
type enginePromoteStore struct {
	engine *MemQLEngine
}

func (s *enginePromoteStore) CreatePromoteBundle(ctx context.Context, bundleId, title, summary string) error {
	args, err := json.Marshal(map[string]any{
		"bundleId": bundleId,
		"title":    title,
		"summary":  summary,
	})
	if err != nil {
		return err
	}
	if _, err := s.engine.Execute(ctx, "createAuthoringBundle("+string(args)+")"); err != nil {
		return err
	}
	// createAuthoringBundle inserts status "draft"; transition it to
	// active so the boot re-hydration's system-active enumeration picks it up.
	if _, err := s.engine.Execute(ctx, fmt.Sprintf(`activateAuthoringBundle({"bundleId":%q})`, bundleId)); err != nil {
		return err
	}
	return nil
}

func (s *enginePromoteStore) CreatePromoteConstruct(ctx context.Context, constructId, bundleId, kind, name, targetNamespace, source string) error {
	args, err := json.Marshal(map[string]any{
		"constructId":     constructId,
		"bundleId":        bundleId,
		"kind":            kind,
		"name":            name,
		"targetNamespace": targetNamespace,
		"source":          source,
	})
	if err != nil {
		return err
	}
	if _, err := s.engine.Execute(ctx, "createAuthoringConstruct("+string(args)+")"); err != nil {
		return err
	}
	// Flip the construct to active so it reads as a live promoted record.
	cargs, err := json.Marshal(map[string]string{"constructId": constructId, "status": "active"})
	if err != nil {
		return err
	}
	if _, err := s.engine.Execute(ctx, "setConstructStatus("+string(cargs)+")"); err != nil {
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
	q := fmt.Sprintf(`authoringConstructsForBundle({"bundleId":%q})`, bundleId)
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
