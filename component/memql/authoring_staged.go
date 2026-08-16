package memql

// authoring_staged.go -- the STAGED tier (epic memql#3928): a durable authored
// construct that resolves for its AUTHOR alone.
//
// A construct had two conditions and nothing between them. Session-define
// (AuthorSessionBundle) is owner-scoped but dies with the connection --
// docs/public/language/training.md names that silent loss as the most expensive
// confusion on the surface. Durable promote is the opposite extreme: persisted,
// shared, broadcast to every node, replayed at boot. Nothing let an author keep
// a construct that works for them alone.
//
// Staged is that middle. It is the promote path with ONE call removed:
//
//	promote:  compile -> register SHARED -> persist row (active) -> broadcast
//	stage:    compile -> register OWNER   -> persist row (staged) -> .
//
// THE OMISSION IS THE TIER. There is no separate staged pipeline, no second
// compile, no second store, and no staged event topic -- because a staged
// construct is not a different KIND of thing from a promoted one, it is the same
// thing that has not been shown to anybody yet. Training it (TrainStagedConstruct
// below) flips the SAME row to active, registers it shared, and fires the same
// broadcast the promote path fires, at which point the two are indistinguishable
// -- which is the point.
//
// CONCEPT IS NOT STAGEABLE, and is refused by name rather than skipped. A
// concept has no owner-scoped resolution path at all: it registers into the one
// shared concept registry, and merging it rebuilds relationship + node-type
// state cluster-wide (authoring_promote_concept.go). Staging one would persist a
// row and register an entry that NOTHING would ever read -- a construct that
// silently does nothing, which is the exact failure mode this epic exists to
// remove. So the refusal is loud, names the reason, and points at training as
// the thing the author can actually do.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/id"
)

// ConstructStaged is the v1:authoring:construct.status value for the staged
// tier.
//
// A plain string rather than a BundleStatus, because it is not one: `staged`
// exists on the CONSTRUCT enum only. A bundle has no staged state and needs
// none -- the stage path writes its bundle row ACTIVE exactly as the promote
// path does, precisely so the boot walk's system-active enumeration finds it
// with no query change. Typing this as a BundleStatus would claim a bundle can
// be staged, which nothing would ever set and every bundle-status switch would
// then have to pretend to handle.
const ConstructStaged = "staged"

// stagedBundleTitle is the human-readable title stamped on a staged bundle,
// distinct from durablePromoteBundleTitle so the #954 review surfaces can tell
// the two tiers apart at a glance. The bundle ID PREFIX is deliberately the
// same (durablePromoteBundlePrefix): the boot walk matches on it, and a staged
// bundle must be found by exactly the walk that finds a promoted one -- the
// three-way route then reads the CONSTRUCT's status, which is where the tier
// actually lives.
const stagedBundleTitle = "Staged construct"

// AuditActionConstructStaged / AuditActionConstructTrained are the audit actions
// stamped on the two staged-tier transitions. lower_snake_case, matching the
// v1:identity:auditEvent.action convention the promote/demote actions use.
const (
	AuditActionConstructStaged  = "authored_construct_staged"
	AuditActionConstructTrained = "authored_construct_trained"
)

// stagedRegistry returns the engine's staged registry, creating it on first use.
// The WRITE-side accessor: readers take e.stagedAuthored directly and tolerate
// nil, so a node that has staged nothing never allocates one.
func (e *MemQLEngine) stagedRegistry() *AuthoredRuntimeRegistry {
	e.stagedOnce.Do(func() {
		if e.stagedAuthored == nil {
			e.stagedAuthored = NewAuthoredRuntimeRegistry()
		}
	})
	return e.stagedAuthored
}

// isStageableKind reports whether a construct kind can be STAGED.
//
// The durable-promotable set minus `concept` -- see the file header for why the
// exclusion is structural rather than a scoping decision that could be reversed
// by adding a case here.
func isStageableKind(kind string) bool {
	switch kind {
	case "query", "mutation", "logic", "spec", "trait":
		return true
	default:
		return false
	}
}

// StageBundleResult reports the outcome of a bundle-level stage, mirroring
// PromoteBundleResult. It carries no ConceptDiffs field, and its absence is the
// contract: a bundle that reaches the staging loop declares no concept, so there
// is nothing for a schema classifier to have decided.
type StageBundleResult struct {
	OK          bool                `json:"ok"`
	Staged      []DefinedConstruct  `json:"staged,omitempty"`
	Diagnostics []SandboxDiagnostic `json:"diagnostics,omitempty"`
}

// StageBundleDurable validates a `.memql` bundle through the Gate-1 sandbox,
// then durably STAGES every stageable construct (query / mutation / logic /
// spec / trait) it defines into the engine's owner-scoped staged registry.
//
// owner is the AUTHENTICATED actor; every construct is keyed to it, so one
// author's staged construct can never resolve for another. Unlike promote this
// is NOT owner-role-gated at the caller: staging registers nothing shared and
// broadcasts nothing, so its blast radius is a database row rather than a change
// to what the cluster runs, and it sits at the same bar as `define`
// (owner-or-developer).
//
// A bundle that fails validation stages nothing and returns OK=false with the
// diagnostics + a non-nil error. A bundle declaring a CONCEPT is refused whole,
// before anything is staged, rather than staging the verbs and dropping the noun.
func (e *MemQLEngine) StageBundleDurable(ctx context.Context, owner, bundleSource, origin string) (StageBundleResult, error) {
	return e.stageBundleDurableWithStore(ctx, &enginePromoteStore{engine: e}, owner, bundleSource, origin)
}

// stageBundleDurableWithStore is the store-driven core of StageBundleDurable,
// split out so it is unit testable with a fake promoteStore (no live DB),
// exactly like promoteBundleDurableWithStore.
func (e *MemQLEngine) stageBundleDurableWithStore(ctx context.Context, store promoteStore, owner, bundleSource, origin string) (StageBundleResult, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return StageBundleResult{}, fmt.Errorf("authoring: durable stage requires an authenticated owner")
	}

	sliced := SplitBundleSource(bundleSource)

	// REFUSE BEFORE STAGING ANYTHING. The check runs over the sliced source
	// ahead of the loop rather than inside it, so a bundle holding a concept and
	// three queries stages zero constructs instead of three -- a half-staged
	// bundle is worse than a refused one, because the author's next act is to
	// fix the concept and re-stage, and the three that landed would then be
	// re-staged over themselves with no record that they had ever been split.
	for _, c := range sliced {
		if c.Kind == "concept" {
			return StageBundleResult{}, fmt.Errorf("authoring: durable stage of concept %q is not supported: a concept registers into the one shared concept registry and its merge rebuilds relationship state cluster-wide, so there is no owner-scoped form of it to stage -- train the concept (durable promote) and stage the constructs bound to it", c.Name)
		}
	}

	// Validate + compile via the SAME session-define path the promote tier uses
	// (Gate-1 sandbox, then compile each construct into its executable form). A
	// throwaway owner-scoped registry holds the compiled forms; it is not the
	// staged registry, and nothing about it is durable -- it exists only to carry
	// the *Function / *Spec into the per-construct stage below.
	reg := NewAuthoredRuntimeRegistry()
	defineRes, err := AuthorSessionBundle(reg, owner, bundleSource, origin)
	result := StageBundleResult{OK: defineRes.OK, Diagnostics: defineRes.Diagnostics}
	if err != nil {
		return result, err
	}

	for _, c := range sliced {
		if !isStageableKind(c.Kind) {
			// The kinds the promote path also leaves alone (shape registers as
			// session metadata; automation / action / capability are the Gate-3
			// deployment kinds). Not a silent drop: the validation above
			// compiled every kind, so each appears in result.Diagnostics.
			continue
		}
		ac, ok := reg.Lookup(owner, c.Kind, c.Name)
		if !ok {
			continue
		}
		if serr := e.stageConstructDurableWithStore(ctx, store, owner, ac); serr != nil {
			result.OK = false
			return result, fmt.Errorf("authoring: durable stage %s %q: %w", c.Kind, c.Name, serr)
		}
		result.Staged = append(result.Staged, DefinedConstruct{Kind: c.Kind, Name: c.Name})
	}
	return result, nil
}

// stageConstructDurableWithStore stages ONE compiled construct: register it into
// the owner-scoped staged registry (the authoritative gate -- a refused stage
// persists nothing), then persist the reviewable bundle + construct row pair
// with status "staged".
//
// The step the promote path takes and this one does not is publishAuthoringPromote.
func (e *MemQLEngine) stageConstructDurableWithStore(ctx context.Context, store promoteStore, owner string, c *AuthoredConstruct) error {
	if c == nil {
		return fmt.Errorf("authoring: durable stage requires a construct")
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return fmt.Errorf("authoring: durable stage requires an authenticated owner")
	}
	if !isStageableKind(c.Kind) {
		return fmt.Errorf("authoring: durable staging of %s constructs is not supported (function-family + spec only)", c.Kind)
	}
	if c.Compiled == nil {
		return fmt.Errorf("authoring: durable stage %s %q: construct is not compiled (define it first)", c.Kind, c.Name)
	}

	if err := e.stageAuthoredConstruct(owner, c); err != nil {
		return err
	}

	// Persist under the owner's envelope so the #954 mutations stamp
	// ownerUserId == owner (per-row authz owner) -- the promote path's shape.
	persistCtx := auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
	bundleId := durablePromoteBundlePrefix + id.NewShortId()
	constructId := durablePromoteBundlePrefix + c.Kind + "-" + id.NewShortId()
	summary := fmt.Sprintf("Staged %s %q (durable, callable by its author only until it is trained).", c.Kind, c.Name)

	if err := store.CreatePromoteBundle(persistCtx, bundleId, stagedBundleTitle, summary); err != nil {
		return fmt.Errorf("authoring: persist staged bundle: %w", err)
	}
	if err := store.CreatePromoteConstruct(persistCtx, constructId, bundleId, c.Kind, c.Name, promoteTargetNamespace(c), c.Source, ConstructStaged); err != nil {
		return fmt.Errorf("authoring: persist staged construct: %w", err)
	}

	if e.Component != nil && e.Logger != nil {
		e.Logger.Info("authored construct staged (durable, owner-scoped; not shared and not broadcast)",
			"owner", owner, "kind", c.Kind, "name", c.Name, "bundleId", bundleId, "action", AuditActionConstructStaged)
	}
	return nil
}

// stageAuthoredConstruct registers a compiled construct into the owner-scoped
// staged registry, refusing anything the shared registries already own.
//
// CORE-FIRST IS UNCHANGED, and the refusal is HONEST rather than defensive: the
// overlay builders layer staged strictly after e.functions / e.specs, so a
// staged construct whose name is already live there would never resolve. Letting
// the stage succeed would hand the author a construct that persists, re-hydrates
// at every boot, appears in their catalog, and does nothing -- so it is refused
// at the point the author can still act on it, with the two cases named
// separately because the answer differs: a CORE name can never be claimed, while
// an ALREADY-TRAINED name is claimable again after a demote.
func (e *MemQLEngine) stageAuthoredConstruct(owner string, c *AuthoredConstruct) error {
	switch c.Kind {
	case "query", "mutation", "logic":
		fn, ok := c.Compiled.(*Function)
		if !ok || fn == nil {
			return fmt.Errorf("authoring: stage %s %q: construct is not compiled", c.Kind, c.Name)
		}
		if e.functions != nil {
			if existing, err := e.functions.Get(fn.Name); err == nil && existing != nil {
				return stagedNameTakenError(e, c.Kind, c.Name, "function:"+fn.Name)
			}
		}
	case "spec", "trait":
		spec, ok := c.Compiled.(*Spec)
		if ok && spec == nil {
			// compileAuthoredSpec's (nil, nil) is the #2607 intentional-skip
			// contract for @disabled. Name the state, not a compile failure.
			return fmt.Errorf("authoring: stage %s %q: construct is @disabled; enable it (remove @disabled from the source) before staging", c.Kind, c.Name)
		}
		if !ok {
			return fmt.Errorf("authoring: stage %s %q: construct is not compiled", c.Kind, c.Name)
		}
		if e.specs != nil {
			if _, exists := e.specs.Lookup(spec.Name); exists {
				return stagedNameTakenError(e, c.Kind, c.Name, "spec:"+spec.Name)
			}
			if e.specs.IsDisabled(spec.Name) {
				return fmt.Errorf("authoring: stage %s %q: a @disabled construct owns that name (re-enable or rename)", c.Kind, c.Name)
			}
		}
	default:
		return fmt.Errorf("authoring: staging of %s constructs is not supported (function-family + spec only)", c.Kind)
	}

	staged := c.clone()
	staged.OwnerUserId = owner
	// SUPERSEDE THE ENTRY ALREADY THERE. AuthoredRuntimeRegistry.Register treats
	// a repeat key as a version replacement and REFUSES one whose version does
	// not strictly exceed the existing entry's -- a stale re-activation must not
	// silently downgrade a live construct.
	//
	// Every construct reaching here carries version 1, because the compile
	// registry stageBundleDurableWithStore builds is fresh per call and the boot
	// walk builds its AuthoredConstruct by hand. So without this bump, re-staging
	// a construct -- the iteration loop the whole tier exists for -- would be
	// refused with a version error, and so would the second of two persisted
	// rows at boot.
	staged.Version = 1
	if existing, ok := e.stagedRegistry().Lookup(owner, c.Kind, c.Name); ok && existing != nil {
		staged.Version = existing.Version + 1
	}
	// AuthoredActive, on a row whose PERSISTED status is "staged", and the two
	// are not in conflict: AuthoredConstructStatus is the RUNTIME lifecycle
	// ("registered and resolvable at execution time"), which a staged construct
	// is -- for its author. The tier is expressed by WHICH registry holds the
	// entry, not by its status, and the overlay builders filter on AuthoredActive
	// exactly as they do for the session registry.
	staged.Status = AuthoredActive
	return e.stagedRegistry().Register(staged)
}

// stagedNameTakenError distinguishes the two ways a shared registry can already
// own a name, because only one of them is the author's to clear.
func stagedNameTakenError(e *MemQLEngine, kind, name, markerKey string) error {
	if _, promoted := e.promotedAuthored.Load(markerKey); promoted {
		return fmt.Errorf("authoring: stage %s %q: that name is already TRAINED on this cluster, and a trained construct resolves ahead of a staged one -- demote it first if you mean to iterate on it privately", kind, name)
	}
	return fmt.Errorf("authoring: stage %s %q: a core construct already owns that name (staging cannot redefine core)", kind, name)
}

// recompileAndStageRow recompiles one persisted STAGED row's source back into
// its compiled *Function / *Spec and registers it into the owner-scoped staged
// registry. The staged counterpart to recompileAndPromoteRow, and the branch the
// three-way re-hydration route takes for a row whose status is "staged".
//
// It inherits recompileAndPromoteRow's re-hydration posture verbatim -- see the
// long note there for why a stored row that no longer compiles is QUARANTINED
// rather than fatal, and why the grammar stamp EXPLAINS a failed compile instead
// of gating one.
//
// It does NOT inherit that path's @disabled name RESERVATION, and the difference
// is the tier. A stored @disabled row on the promote path reserves the name in
// the SHARED spec registry so an authored promote cannot claim it. A staged row
// is owner-private; reserving a shared name from one would let one author's
// disabled draft deny that name to the whole cluster, which is a bigger claim
// than staging is allowed to make. So a staged @disabled row skips quietly --
// nothing registered, nothing reserved -- which is also what its author sees
// when they session-define the same source.
// The context is unused: unlike the promote path there is no concept branch, so
// nothing here reaches a schema classifier or a row count. The parameter stays
// so the two steps share one signature and the walk can hold either in the same
// field.
func (e *MemQLEngine) recompileAndStageRow(_ context.Context, row AuthoringConstructRow) (err error) {
	defer func() {
		if err != nil && !errors.Is(err, errRehydrateSkippedDisabled) {
			e.quarantineRehydratedConstruct(row, err)
		}
	}()
	if !isStageableKind(row.Kind) {
		return fmt.Errorf("re-hydration of a staged %s is not supported (function-family + spec only)", row.Kind)
	}

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
		// Bound against THIS ENGINE's live concept registry, for the reason
		// spelled out on the promote path: a staged query's concept deps may
		// include a concept this engine promoted at runtime, which lives in
		// whichever registry the engine holds rather than the package default.
		fn, cerr := compileAuthoredFunction(sc, e.conceptBindingRegistry())
		if cerr != nil {
			return explainStaleGrammarStamp(row, fmt.Errorf("recompile staged %s %q: %w", row.Kind, row.Name, cerr))
		}
		c.Compiled = fn
	case "spec", "trait":
		spec, cerr := compileAuthoredSpec(sc)
		if cerr != nil {
			return explainStaleGrammarStamp(row, fmt.Errorf("recompile staged %s %q: %w", row.Kind, row.Name, cerr))
		}
		if spec == nil {
			// compileAuthoredSpec's (nil, nil) is the #2607 intentional-skip
			// contract for @disabled. No shared reservation -- see the doc
			// comment above.
			if e.Component != nil && e.Logger != nil {
				e.Logger.Info("staged authored construct is @disabled; skipped at re-hydration (nothing registered, no name reserved)",
					"component", "memql.engine", "kind", row.Kind, "name", row.Name, "bundleId", row.BundleId, "owner", row.OwnerUserId)
			}
			return errRehydrateSkippedDisabled
		}
		c.Compiled = spec
	}
	return e.stageAuthoredConstruct(row.OwnerUserId, c)
}

// --- the transitions out of staged ------------------------------------------

// TrainStagedConstruct promotes a STAGED construct into the shared registry:
// the state transition that is the whole point of the tier.
//
// It flips the persisted row(s) from "staged" to "active", registers the
// compiled form into the shared registry via the same
// promoteAuthoredConstructWithGate every other promote route funnels through,
// drops the entry from the owner-scoped staged registry, and fires the EXISTING
// publishAuthoringPromote broadcast -- so `authoring.promote.*` propagates it to
// every node exactly as a first-time durable promote does.
//
// It flips the SAME row rather than writing a second one. A second row would
// leave the boot walk replaying both, one staged and one active, and the
// three-way route would then register the construct twice -- once owner-scoped,
// once shared -- with the shared one silently winning. One construct, one
// lifecycle, one row.
//
// The gate is a fresh strict gate, NOT conceptPromoteReplay(): this is a
// DECISION being taken for the first time (nobody has classified this source
// against a running version yet), where the two replay paths re-apply a decision
// somebody already took. That concept is unreachable through here today --
// nothing stageable is a concept -- does not change which gate is correct, and
// wiring the replay gate in would silently disable the memql#3757 classifier the
// moment the staged set grew.
func (e *MemQLEngine) TrainStagedConstruct(ctx context.Context, owner, kind, name string) error {
	return e.trainStagedConstructWithStore(ctx, e.stagedRowStoreOrDefault(), owner, kind, name)
}

// stagedRowStoreOrDefault resolves the staged tier's row store: the test seam
// when one is set, the production store over this engine otherwise.
func (e *MemQLEngine) stagedRowStoreOrDefault() stagedRowStore {
	if e.stagedRows != nil {
		return e.stagedRows
	}
	return &engineStagedStore{engine: e}
}

// trainStagedConstructWithStore is the store-driven core, split out so it is
// unit testable with a fake stagedRowStore (no live DB).
func (e *MemQLEngine) trainStagedConstructWithStore(ctx context.Context, store stagedRowStore, owner, kind, name string) error {
	owner = strings.TrimSpace(owner)
	kind = strings.TrimSpace(kind)
	name = strings.TrimSpace(name)
	if owner == "" {
		return fmt.Errorf("authoring: train requires an authenticated owner")
	}
	if name == "" {
		return fmt.Errorf("authoring: train requires a construct name")
	}

	c, ok := e.lookupStaged(owner, kind, name)
	if !ok {
		return fmt.Errorf("authoring: train %s %q: no staged construct of that name is registered for this owner (stage it first)", kind, name)
	}

	// Register into the SHARED registry first -- the authoritative gate, exactly
	// as on the promote path, so a refused training writes no row transition and
	// publishes no broadcast.
	if err := e.promoteAuthoredConstructWithGate(ctx, &conceptPromoteGate{}, c); err != nil {
		return err
	}

	persistCtx := auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
	bundleId, err := setStagedRowsStatus(persistCtx, store, owner, c.Kind, c.Name, string(BundleActive))
	if err != nil {
		return err
	}

	// Drop the owner-scoped entry: the shared registration now serves this owner
	// too (it resolves AHEAD of staged), and an entry left behind would be a
	// second copy of the same construct that nothing reads and that the next
	// boot would not recreate.
	if e.stagedAuthored != nil {
		_ = e.stagedAuthored.Retire(owner, c.Kind, c.Name)
	}

	if e.Component != nil && e.Logger != nil {
		e.Logger.Info("staged construct trained into the shared registry",
			"owner", owner, "kind", c.Kind, "name", c.Name, "bundleId", bundleId, "action", AuditActionConstructTrained)
	}

	// The same broadcast the durable promote fires, on the same channel, with
	// the same payload. Peers re-hydrate this bundle from the shared DB -- and
	// find its constructs ACTIVE, because the row transition above is already
	// committed.
	e.publishAuthoringPromote(bundleId, owner)
	return nil
}

// demoteStagedConstructWithStore withdraws a STAGED construct: drop it from the
// owner-scoped registry and retire its persisted row(s).
//
// The inverse of stage, and it inherits stage's omissions exactly. Nothing is
// removed from the shared registry, because nothing was ever added to it;
// nothing is broadcast, because no peer ever learned of it. The terminal row
// state is "retired" -- the same state a demoted promoted construct reaches, and
// for the same reason: it is the status every re-hydration walk skips.
func (e *MemQLEngine) demoteStagedConstructWithStore(ctx context.Context, store demoteStore, owner, kind, name string) error {
	if e.stagedAuthored == nil {
		return fmt.Errorf("authoring: demote %s %q: no staged construct of that name is registered for this owner", kind, name)
	}
	c, ok := e.lookupStaged(owner, kind, name)
	if !ok {
		return fmt.Errorf("authoring: demote %s %q: no staged construct of that name is registered for this owner", kind, name)
	}
	if err := e.stagedAuthored.Retire(owner, c.Kind, c.Name); err != nil {
		return err
	}

	persistCtx := auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
	bundles, err := store.LoadPromotedBundles(persistCtx)
	if err != nil {
		return fmt.Errorf("authoring: enumerate bundles for staged demote: %w", err)
	}
	for _, bundle := range bundles {
		if bundle.OwnerUserId != owner {
			continue
		}
		rows, lerr := store.LoadConstructsForBundle(persistCtx, bundle.OwnerUserId, bundle.Id)
		if lerr != nil {
			return fmt.Errorf("authoring: load constructs for staged demote: %w", lerr)
		}
		matched := false
		for _, row := range rows {
			if row.Kind != c.Kind || row.Name != c.Name || !isStagedConstructStatus(row.Status) {
				continue
			}
			if rerr := store.RetireConstruct(persistCtx, bundle.OwnerUserId, row.Id); rerr != nil {
				return fmt.Errorf("authoring: retire staged construct row: %w", rerr)
			}
			matched = true
		}
		if !matched {
			continue
		}
		fresh, ferr := store.LoadConstructsForBundle(persistCtx, bundle.OwnerUserId, bundle.Id)
		if ferr != nil {
			return fmt.Errorf("authoring: re-load constructs after staged demote: %w", ferr)
		}
		if allConstructsRetired(fresh) {
			if berr := store.RetireBundle(persistCtx, bundle.OwnerUserId, bundle.Id); berr != nil {
				return fmt.Errorf("authoring: retire bundle after staged demote: %w", berr)
			}
		}
	}

	if e.Component != nil && e.Logger != nil {
		e.Logger.Info("staged construct demoted (owner-scoped withdrawal; nothing shared to remove, nothing to broadcast)",
			"owner", owner, "kind", c.Kind, "name", c.Name, "action", AuditActionConstructDemoted)
	}
	return nil
}

// lookupStaged resolves a staged construct for an owner, by kind when one is
// given and by name alone when it is not.
//
// The kind is OPTIONAL for the same reason the MCP promote tool's is: a caller
// naming a construct in an editor knows its name and may not know which of
// `query` / `mutation` / `logic` the catalog files it under. An ambiguous
// name-only lookup takes the first in the registry's stable (kind, name) order
// rather than failing, because two staged constructs of the SAME name and
// different kinds is not a shape the stage path can produce for one owner -- the
// name is what the shared registries key on.
func (e *MemQLEngine) lookupStaged(owner, kind, name string) (*AuthoredConstruct, bool) {
	if e.stagedAuthored == nil {
		return nil, false
	}
	if kind != "" {
		return e.stagedAuthored.Lookup(owner, kind, name)
	}
	for _, c := range e.stagedAuthored.ListForOwner(owner) {
		if c.Name == name {
			return c, true
		}
	}
	return nil, false
}

// setStagedRowsStatus walks the durable bundles for one owner and writes status
// onto every row matching (kind, name) that is currently STAGED, returning the
// bundle id the rows were found in (the id the promote broadcast carries).
//
// Every matching row is written, not just the first, on the same reasoning the
// demote walk records: re-staging the same source appends a row rather than
// editing one, and a row left staged would come back owner-scoped on the next
// boot while its twin came back shared.
func setStagedRowsStatus(ctx context.Context, store stagedRowStore, owner, kind, name, status string) (string, error) {
	bundles, err := store.LoadPromotedBundles(ctx)
	if err != nil {
		return "", fmt.Errorf("authoring: enumerate bundles for staged transition: %w", err)
	}
	bundleId := ""
	for _, bundle := range bundles {
		if bundle.OwnerUserId != owner {
			continue
		}
		rows, lerr := store.LoadConstructsForBundle(ctx, bundle.OwnerUserId, bundle.Id)
		if lerr != nil {
			return "", fmt.Errorf("authoring: load constructs for staged transition: %w", lerr)
		}
		for _, row := range rows {
			if row.Kind != kind || row.Name != name || !isStagedConstructStatus(row.Status) {
				continue
			}
			if serr := store.SetConstructStatus(ctx, bundle.OwnerUserId, row.Id, status); serr != nil {
				return "", fmt.Errorf("authoring: set staged construct row status: %w", serr)
			}
			bundleId = bundle.Id
		}
	}
	if bundleId == "" {
		return "", fmt.Errorf("authoring: no staged %s row named %q found for this owner; the in-memory registration has been made shared but nothing was persisted -- re-stage and retry", kind, name)
	}
	return bundleId, nil
}

// stagedRowStore is the narrow graph surface the staged tier's TRANSITIONS need:
// locate a construct's persisted rows, and write a status onto one.
//
// It is not the demoteStore, whose RetireConstruct hard-codes "retired" -- the
// train transition writes "active", and giving demoteStore a general setter
// would let a caller reach the demote path's row writer with an arbitrary
// status. *MemQLEngine satisfies this via engineStagedStore; tests fake it.
type stagedRowStore interface {
	LoadPromotedBundles(ctx context.Context) ([]AuthoringBundleRow, error)
	LoadConstructsForBundle(ctx context.Context, owner, bundleId string) ([]AuthoringConstructRow, error)
	SetConstructStatus(ctx context.Context, owner, constructId, status string) error
}

// engineStagedStore is the production stagedRowStore over a live engine. The
// enumeration + per-bundle load are the promote-rehydrate store's, verbatim; the
// status write runs under the row owner's envelope like every other #954
// lifecycle write.
type engineStagedStore struct {
	engine *MemQLEngine
}

func (s *engineStagedStore) LoadPromotedBundles(ctx context.Context) ([]AuthoringBundleRow, error) {
	return (&enginePromoteRehydrateStore{engine: s.engine}).LoadPromotedBundles(ctx)
}

func (s *engineStagedStore) LoadConstructsForBundle(ctx context.Context, owner, bundleId string) ([]AuthoringConstructRow, error) {
	return (&enginePromoteRehydrateStore{engine: s.engine}).LoadConstructsForBundle(ctx, owner, bundleId)
}

func (s *engineStagedStore) SetConstructStatus(ctx context.Context, owner, constructId, status string) error {
	authorCtx := auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
	return (&engineActivationStore{engine: s.engine}).SetConstructStatus(authorCtx, constructId, status)
}
