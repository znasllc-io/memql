package memql

// authoring_demote_durable.go -- DB-persisted, restart-durable DEMOTE: the exact
// inverse of the durable PROMOTE path (memql#2163).
//
// PromoteConstructDurable (authoring_promote_durable.go) makes an author-promoted
// plain construct durable + callable-by-all + restart-surviving + cross-node-live.
// This file is its mirror: it RETIRES a previously durably-promoted construct so
// it is no longer callable by any session, never re-hydrates after a restart, and
// is removed from every node's shared registry within seconds. It:
//
//	a. removes the construct from THIS node's shared registry via the in-process
//	   DemoteAuthoredConstruct (authoring_session.go), which is SAFETY-GATED to
//	   author-promoted names only -- it can never unregister a sealed core
//	   construct;
//	b. flips the persisted v1:authoring:construct row to "retired"
//	   (setConstructStatus), and retires the owning v1:authoring:bundle once all
//	   its constructs are retired (retireAuthoringBundle / retiredAt stamp), so
//	   the boot re-hydration (which skips retired rows) never re-registers it;
//	c. audit-logs the demote;
//	d. emits the dedicated authoring.demote.<bundleId> broadcast so every other
//	   node removes the construct from its own shared registry with no restart.
//
// Ordering mirrors the promote path inverted: it removes from the shared registry
// FIRST (the authoritative safety gate -- a name that is NOT author-promoted is
// refused there, so a refused demote never retires a persisted row), then flips
// the persisted rows. A persist failure surfaces as an error (the construct is
// live-removed in-process but the row is still "active" -- the boot re-hydration
// would bring it back, consistent with "not durable").

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/events"
)

// AuditActionConstructDemoted is the audit action stamped when a plain construct
// is durably demoted out of the shared registry. lower_snake_case to match the
// v1:identity:auditEvent.action convention (mirrors AuditActionConstructPromoted).
const AuditActionConstructDemoted = "authored_construct_demoted"

// isRetiredConstructStatus reports whether a persisted construct/bundle status
// marks it retired. Used by the re-hydration walks (boot + cross-node) to skip a
// demoted construct so it never re-registers, and by the demote bundle-retire
// rollup.
func isRetiredConstructStatus(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), string(BundleRetired))
}

// DemoteConstructDurable is the durable, restart-surviving DEMOTE of a single
// previously durably-promoted plain construct (memql#2163), the inverse of
// PromoteConstructDurable. It removes the construct from the shared registry
// (in-process, author-promoted-only safety gate), then flips the persisted
// construct row to "retired" and retires the owning bundle once every member is
// retired, audit-logs, and broadcasts the live cross-node removal.
//
// owner is the AUTHENTICATED actor; the OWNER-ONLY gate is enforced by the caller
// (the gRPC durable-demote handler matches the promote owner gate). The persisted
// row writes run under the owner's envelope, exactly like the promote path.
func (e *MemQLEngine) DemoteConstructDurable(ctx context.Context, owner, kind, name string) error {
	return e.demoteConstructDurableWithStore(ctx, &engineDemoteStore{engine: e}, owner, kind, name)
}

// DemoteBundleResult reports the outcome of a bundle-level durable demote
// (memql#2163), mirroring PromoteBundleResult. OK is true only when every plain
// construct named in the source was demoted. On a mid-demote failure Demoted
// holds the ones that were removed before the failing one.
type DemoteBundleResult struct {
	OK          bool                `json:"ok"`
	Demoted     []DefinedConstruct  `json:"demoted,omitempty"`
	Diagnostics []SandboxDiagnostic `json:"diagnostics,omitempty"`
}

// DemoteBundleDurable durably demotes every PLAIN construct (query / mutation /
// logic / spec / trait) named in a `.memql` bundle source out of the shared
// engine registry (memql#2163), the inverse of PromoteBundleDurable. It is the
// bundle-level entry point the owner-gated gRPC durable-demote handler drives.
//
// Unlike the promote path there is NO Gate-1 compile: a demote only needs the
// names + kinds the source declares (it removes by name), never the construct's
// compiled form -- so a construct whose source no longer compiles can still be
// demoted. The split reuses the same SplitBundleSource the promote path uses, so
// the demote names exactly match what a prior promote of the same source landed.
//
// owner is the AUTHENTICATED actor; the OWNER-ONLY gate is enforced by the
// caller. A per-construct demote failure stops at that construct and returns the
// error (the ones already demoted stay removed -- each is independently durable +
// propagated).
func (e *MemQLEngine) DemoteBundleDurable(ctx context.Context, owner, bundleSource string) (DemoteBundleResult, error) {
	return e.demoteBundleDurableWithStore(ctx, &engineDemoteStore{engine: e}, owner, bundleSource)
}

// demoteBundleDurableWithStore is the store-driven core of DemoteBundleDurable,
// split out so it is unit testable with a fake demoteStore (no live DB), exactly
// like promoteBundleDurableWithStore.
func (e *MemQLEngine) demoteBundleDurableWithStore(ctx context.Context, store demoteStore, owner, bundleSource string) (DemoteBundleResult, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return DemoteBundleResult{}, fmt.Errorf("authoring: durable demote requires an authenticated owner")
	}

	constructs := SplitBundleSource(bundleSource)
	demotable := make([]SandboxConstruct, 0, len(constructs))
	for _, c := range constructs {
		if isDurablePromotableKind(c.Kind) {
			demotable = append(demotable, c)
		}
	}
	if len(demotable) == 0 {
		return DemoteBundleResult{}, fmt.Errorf("authoring: durable demote found no demotable constructs (function-family + spec only) in the bundle source")
	}

	result := DemoteBundleResult{OK: true}
	for _, c := range demotable {
		if derr := e.demoteConstructDurableWithStore(ctx, store, owner, c.Kind, c.Name); derr != nil {
			result.OK = false
			return result, fmt.Errorf("authoring: durable demote %s %q: %w", c.Kind, c.Name, derr)
		}
		result.Demoted = append(result.Demoted, DefinedConstruct{Kind: c.Kind, Name: c.Name})
	}
	return result, nil
}

// demoteStore is the narrow graph surface the durable demote needs: locate the
// persisted promote bundles + their constructs (to find the (kind, name) row),
// retire a construct row, and retire a fully-demoted bundle. *MemQLEngine
// satisfies it via engineDemoteStore; tests fake it.
type demoteStore interface {
	// LoadPromotedBundles returns every active durable-promote bundle across all
	// owners (admin-scoped), filtered to the ones the durable path created. Reused
	// verbatim from the rehydrate store shape.
	LoadPromotedBundles(ctx context.Context) ([]AuthoringBundleRow, error)
	// LoadConstructsForBundle returns a bundle's member constructs (carrying
	// status), read under the bundle owner's envelope.
	LoadConstructsForBundle(ctx context.Context, owner, bundleId string) ([]AuthoringConstructRow, error)
	// RetireConstruct flips a v1:authoring:construct row to status "retired".
	RetireConstruct(ctx context.Context, owner, constructId string) error
	// RetireBundle retires a v1:authoring:bundle (status -> retired, retiredAt
	// stamped) once all of its constructs are retired.
	RetireBundle(ctx context.Context, owner, bundleId string) error
}

// demoteConstructDurableWithStore is the store-driven core, split out so it is
// unit testable with a fake demoteStore (no live DB).
func (e *MemQLEngine) demoteConstructDurableWithStore(ctx context.Context, store demoteStore, owner, kind, name string) error {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return fmt.Errorf("authoring: durable demote requires an authenticated owner")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("authoring: durable demote requires a construct name")
	}
	if !isDurablePromotableKind(kind) {
		return fmt.Errorf("authoring: durable demotion of %s constructs is not supported (function-family + spec only)", kind)
	}

	// Remove from the shared registry FIRST (the authoritative safety gate). A
	// name that is NOT author-promoted is refused here, so a refused demote never
	// retires a persisted row.
	if err := e.DemoteAuthoredConstruct(ctx, kind, name); err != nil {
		return err
	}

	// Persist under the owner's envelope so the #954 lifecycle mutations run with
	// ownerUserId == owner -- mirrors how the promote store runs its writes.
	persistCtx := auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})

	// Locate the persisted construct row(s) for (kind, name) among the durable
	// promote bundles, flip each to retired, then retire any bundle whose every
	// member is now retired.
	bundles, err := store.LoadPromotedBundles(persistCtx)
	if err != nil {
		return fmt.Errorf("authoring: enumerate promoted bundles for demote: %w", err)
	}
	var bundleId string
	for _, bundle := range bundles {
		rows, lerr := store.LoadConstructsForBundle(persistCtx, bundle.OwnerUserId, bundle.Id)
		if lerr != nil {
			return fmt.Errorf("authoring: load constructs for demote: %w", lerr)
		}
		matched := false
		for _, row := range rows {
			if row.Kind != kind || row.Name != name || isRetiredConstructStatus(row.Status) {
				continue
			}
			if rerr := store.RetireConstruct(persistCtx, bundle.OwnerUserId, row.Id); rerr != nil {
				return fmt.Errorf("authoring: retire construct row: %w", rerr)
			}
			matched = true
			bundleId = bundle.Id
		}
		if !matched {
			continue
		}
		// Retire the bundle once every member construct is retired. Re-load so the
		// just-retired row is reflected.
		fresh, ferr := store.LoadConstructsForBundle(persistCtx, bundle.OwnerUserId, bundle.Id)
		if ferr != nil {
			return fmt.Errorf("authoring: re-load constructs after demote: %w", ferr)
		}
		if allConstructsRetired(fresh) {
			if berr := store.RetireBundle(persistCtx, bundle.OwnerUserId, bundle.Id); berr != nil {
				return fmt.Errorf("authoring: retire bundle: %w", berr)
			}
		}
	}

	if e.Component != nil && e.Logger != nil {
		e.Logger.Info("authored construct durably demoted out of the shared registry",
			"owner", owner, "kind", kind, "name", name, "bundleId", bundleId, "action", AuditActionConstructDemoted)
	}

	// Live cross-node propagation (memql#2163): broadcast the demoted bundle id +
	// owner so every other node removes the construct from its own shared registry
	// within seconds (no restart). Best-effort: a missing event bus degrades to
	// the local demote only -- the retired row keeps the construct out on the next
	// boot.
	e.publishAuthoringDemote(bundleId, owner)
	return nil
}

// allConstructsRetired reports whether every demotable construct row in the slice
// is retired. A bundle with no demotable members reports false (nothing to
// retire the bundle for).
func allConstructsRetired(rows []AuthoringConstructRow) bool {
	any := false
	for _, row := range rows {
		if !isDurablePromotableKind(row.Kind) {
			continue
		}
		any = true
		if !isRetiredConstructStatus(row.Status) {
			return false
		}
	}
	return any
}

// publishAuthoringDemote emits the dedicated authoring-demote broadcast event
// (memql#2163) for a durably-demoted bundle on the separate
// authoring.demote.<bundleId> channel, the inverse of publishAuthoringPromote.
// ONLY the authoring-demote subscriber consumes this channel, and a single
// broadcast routing rule (authoring.demote.*) forwards it to every node -- so a
// demote on one node removes the construct from every node's shared registry with
// zero side effects.
func (e *MemQLEngine) publishAuthoringDemote(bundleId, owner string) {
	if e == nil || e.eventBus == nil {
		return
	}
	bundleId = strings.TrimSpace(bundleId)
	if bundleId == "" {
		return
	}
	e.eventBus.Publish(events.NewEvent(
		events.TopicAuthoringDemoteForBundle(bundleId),
		events.KindAuthoringDemote,
		map[string]any{"bundleId": bundleId, "owner": owner},
	))
}

// --- production store over a live engine ---

// engineDemoteStore is the production demoteStore over a live engine. The bundle
// enumeration + construct load reuse the rehydrate store's queries verbatim (so
// the demote sees the exact same durable-promote rows the promote path created);
// the retire writes run the #954 lifecycle mutations through Execute, exactly
// like enginePromoteStore.
type engineDemoteStore struct {
	engine *MemQLEngine
}

func (s *engineDemoteStore) LoadPromotedBundles(ctx context.Context) ([]AuthoringBundleRow, error) {
	return (&enginePromoteRehydrateStore{engine: s.engine}).LoadPromotedBundles(ctx)
}

func (s *engineDemoteStore) LoadConstructsForBundle(ctx context.Context, owner, bundleId string) ([]AuthoringConstructRow, error) {
	return (&enginePromoteRehydrateStore{engine: s.engine}).LoadConstructsForBundle(ctx, owner, bundleId)
}

func (s *engineDemoteStore) RetireConstruct(ctx context.Context, owner, constructId string) error {
	authorCtx := auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
	return (&engineActivationStore{engine: s.engine}).SetConstructStatus(authorCtx, constructId, string(BundleRetired))
}

func (s *engineDemoteStore) RetireBundle(ctx context.Context, owner, bundleId string) error {
	authorCtx := auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
	return (&engineActivationStore{engine: s.engine}).SetBundleRetired(authorCtx, bundleId)
}
