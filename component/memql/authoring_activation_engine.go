package memql

// authoring_activation_engine.go -- the engine-backed Gate-3 activation
// orchestrator (epic memql#954, issue #961, increment 1).
//
// authoring_activation.go holds the pure plan (BuildApprovalArtifact,
// PlanBundleActivation) -- the testable decisions. This file drives them
// against a live engine: load the persisted bundle + its constructs, run the
// lifecycle mutations (status transitions), register the closure into the
// owner-scoped authored runtime, promote net-new deps into the catalog, and
// emit the activation audit event.
//
// The graph reads/writes go through a narrow ActivationStore the tests fake (no
// DB needed), mirroring the worker dispatcher's store seam. The authored-runtime
// registration is the real AuthoredRuntimeRegistry plus an injected scheduler
// register hook (app wires it to AuthoredScheduler.Activate). The catalog
// promotion reuses the real PromoteConstructToCatalog (#957). Audit goes to the
// injected AuthoredAuditSink (app wires it to the identity audit logger).

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// ActivationStore is the narrow graph surface the activation orchestrator needs:
// load a bundle + its constructs, and run the lifecycle mutations. *MemQLEngine
// satisfies it via engineActivationStore; tests fake it.
type ActivationStore interface {
	// LoadBundle returns the persisted bundle row by id, scoped to owner.
	LoadBundle(ctx context.Context, bundleId string) (AuthoringBundleRow, error)
	// LoadConstructs returns the bundle's member construct rows.
	LoadConstructs(ctx context.Context, bundleId string) ([]AuthoringConstructRow, error)
	// SetBundleActive runs mutationActivateAuthoringBundle.
	SetBundleActive(ctx context.Context, bundleId string) error
	// SetBundleRetired runs mutationRetireAuthoringBundle.
	SetBundleRetired(ctx context.Context, bundleId string) error
	// SetConstructStatus runs mutationSetConstructStatus.
	SetConstructStatus(ctx context.Context, constructId, status string) error
}

// AuthoredRuntimeDeps bundles the live runtime collaborators the activation
// orchestrator drives: the owner-scoped registry, the scheduler register /
// deregister hooks, the catalog promoter, and the audit sink. Every field is
// optional EXCEPT Registry -- a nil scheduler hook / promoter / audit sink
// degrades gracefully (the registry registration still makes constructs
// resolvable), so a binary that does not link the scheduler still activates
// the runtime layer.
type AuthoredRuntimeDeps struct {
	// Registry is the owner-scoped authored-construct runtime. Required.
	Registry *AuthoredRuntimeRegistry
	// RegisterHook hands an activated automation to the live scheduler. nil =
	// scheduler not linked (registry-only activation).
	RegisterHook AuthoredRegisterHook
	// DeregisterHook tears down a retired automation's scheduler entry.
	DeregisterHook AuthoredDeregisterHook
	// PromoteCatalog promotes a net-new dependency construct into the owner's
	// catalog. nil = catalog promotion skipped (the constructs are still live;
	// they just aren't reusable by later bundles yet). Wired to
	// MemQLEngine.PromoteConstructToCatalog.
	PromoteCatalog func(ctx context.Context, constructID, kind, name, source, fromBundleID string) (CatalogPromotion, error)
	// Audit receives activation/retirement events. nil = slog-only.
	Audit AuthoredAuditSink
	// Logger for the orchestrator's own transition logs. nil tolerated.
	Logger *slog.Logger
}

// ActivationResult reports what an activation did: the plan that was applied
// plus the catalog promotions that succeeded. Returned so the caller (and
// tests) can assert the end state.
type ActivationResult struct {
	Plan     BundleActivationPlan `json:"plan"`
	Promoted []CatalogPromotion   `json:"promoted"`
	Retired  string               `json:"retiredBundleId,omitempty"`
}

// ActivateApprovedBundle is the Gate-3 -> active transition: it loads the
// dryRunPassed bundle, applies the activation plan (status transitions, runtime
// registration, catalog promotion of net-new deps), retires any superseded
// prior version, and emits the activation audit event. It is idempotent-safe to
// the extent the underlying mutations are (re-activating an active bundle is
// rejected up front).
//
// Owner is the AUTHENTICATED actor approving -- it MUST equal the bundle owner
// (no cross-owner activation); a mismatch is rejected before any mutation runs.
// This is the authority gate at the bundle grain (the per-call capability gate
// for the automation's RUNTIME actions lands in increment 2).
func (e *MemQLEngine) ActivateApprovedBundle(ctx context.Context, owner, bundleId string, deps AuthoredRuntimeDeps) (ActivationResult, error) {
	return ActivateApprovedBundleWithStore(ctx, &engineActivationStore{engine: e}, owner, bundleId, deps)
}

// activateApprovedBundle is the store-driven core, split out so it is unit
// testable with a fake ActivationStore (no live DB).
func ActivateApprovedBundleWithStore(ctx context.Context, store ActivationStore, owner, bundleId string, deps AuthoredRuntimeDeps) (ActivationResult, error) {
	if deps.Registry == nil {
		return ActivationResult{}, fmt.Errorf("authoring: activation requires an authored runtime registry")
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return ActivationResult{}, fmt.Errorf("authoring: activation requires an authenticated owner")
	}

	bundle, err := store.LoadBundle(ctx, bundleId)
	if err != nil {
		return ActivationResult{}, fmt.Errorf("authoring: load bundle %q: %w", bundleId, err)
	}
	if bundle.Id == "" {
		return ActivationResult{}, fmt.Errorf("authoring: bundle %q not found (or not owned by %q)", bundleId, owner)
	}
	// Authority gate: the approver must be the bundle owner. The authored
	// automation runs under the author's envelope, so only the author can
	// activate it -- no privilege escalation by activating someone else's bundle.
	if bundle.OwnerUserId != owner {
		return ActivationResult{}, fmt.Errorf("authoring: actor %q cannot activate bundle owned by %q", owner, bundle.OwnerUserId)
	}

	constructs, err := store.LoadConstructs(ctx, bundleId)
	if err != nil {
		return ActivationResult{}, fmt.Errorf("authoring: load constructs for %q: %w", bundleId, err)
	}

	plan, err := PlanBundleActivation(bundle, constructs)
	if err != nil {
		return ActivationResult{}, err
	}

	// 1. Lifecycle: bundle dryRunPassed -> active.
	if err := store.SetBundleActive(ctx, bundleId); err != nil {
		return ActivationResult{}, fmt.Errorf("authoring: activate bundle %q: %w", bundleId, err)
	}
	// 2. Member constructs draft -> active.
	for _, id := range plan.ConstructIds {
		if err := store.SetConstructStatus(ctx, id, "active"); err != nil {
			return ActivationResult{}, fmt.Errorf("authoring: activate construct %q: %w", id, err)
		}
	}

	// 3. Register the closure into the owner-scoped authored runtime. The
	// registry registration makes every construct resolvable; the automation
	// constructs additionally go to the scheduler so their triggers fire. The
	// boot re-arm (authoring_rearm.go) reuses this same step so a restart
	// restores identical runtime state.
	if err := registerBundleConstructs(plan.Register, deps); err != nil {
		return ActivationResult{}, err
	}

	// 4. Promote net-new deps into the owner's reusable catalog. Best-effort per
	// construct: a promotion failure does not unwind the activation (the
	// capability is live; the dep just isn't reusable yet) but IS surfaced.
	result := ActivationResult{Plan: plan, Retired: plan.SupersedesBundleId}
	if deps.PromoteCatalog != nil {
		for _, t := range plan.PromoteToCatalog {
			promo, perr := deps.PromoteCatalog(ctx, t.ConstructId, t.Kind, t.Name, t.Source, bundleId)
			if perr != nil {
				if deps.Logger != nil {
					deps.Logger.Warn("authored dep catalog promotion failed (capability is live; dep not yet reusable)",
						"bundleId", bundleId, "construct", t.Kind+":"+t.Name, "error", perr)
				}
				continue
			}
			result.Promoted = append(result.Promoted, promo)
		}
	}

	// 5. Retire the superseded prior version (edit/version path). Its constructs
	// are unregistered so the old behavior stops firing the instant the new one
	// goes live.
	if plan.SupersedesBundleId != "" {
		if err := retireBundleRuntime(ctx, store, plan.SupersedesBundleId, owner, deps); err != nil {
			if deps.Logger != nil {
				deps.Logger.Warn("authored superseded-bundle retirement failed (new version is live)",
					"bundleId", bundleId, "supersedes", plan.SupersedesBundleId, "error", err)
			}
		}
	}

	// 6. Audit: every activation is recorded.
	emitAuthoredAudit(ctx, deps, AuthoredAuditEvent{
		Action:      AuditActionBundleActivated,
		OwnerUserId: owner,
		BundleId:    bundleId,
		Detail:      plan.auditDetail(),
		OccurredAt:  time.Now().UTC(),
	})
	if deps.Logger != nil {
		deps.Logger.Info("authored bundle activated",
			"bundleId", bundleId, "owner", owner, "version", plan.Version,
			"registered", len(plan.Register), "promoted", len(result.Promoted))
	}
	return result, nil
}

// retireBundleRuntime tears down an active bundle: flip status -> retired,
// unregister its constructs from the runtime (+ scheduler), and emit the
// retirement audit. Used both for an explicit retire and for retiring a
// superseded prior version during a version bump.
func retireBundleRuntime(ctx context.Context, store ActivationStore, bundleId, owner string, deps AuthoredRuntimeDeps) error {
	constructs, err := store.LoadConstructs(ctx, bundleId)
	if err != nil {
		return fmt.Errorf("authoring: load constructs to retire %q: %w", bundleId, err)
	}
	if err := store.SetBundleRetired(ctx, bundleId); err != nil {
		return fmt.Errorf("authoring: retire bundle %q: %w", bundleId, err)
	}
	for _, c := range constructs {
		_ = store.SetConstructStatus(ctx, c.Id, "retired")
		if c.Kind == "automation" && deps.DeregisterHook != nil {
			deps.DeregisterHook(c.OwnerUserId, c.Name)
		}
		if deps.Registry != nil {
			// Retire from the registry. A construct already gone (e.g. superseded
			// in place by a newer version's Register) is a no-op for our purposes.
			_ = deps.Registry.Retire(c.OwnerUserId, c.Kind, c.Name)
		}
	}
	emitAuthoredAudit(ctx, deps, AuthoredAuditEvent{
		Action:      AuditActionBundleRetired,
		OwnerUserId: owner,
		BundleId:    bundleId,
		Detail:      map[string]any{"constructCount": len(constructs)},
		OccurredAt:  time.Now().UTC(),
	})
	return nil
}

// RetireActiveBundle is the explicit retirement entry point (a user removing a
// capability). It flips the bundle to retired, unregisters its constructs, and
// audits. owner must be the bundle owner.
func (e *MemQLEngine) RetireActiveBundle(ctx context.Context, owner, bundleId string, deps AuthoredRuntimeDeps) error {
	store := &engineActivationStore{engine: e}
	bundle, err := store.LoadBundle(ctx, bundleId)
	if err != nil {
		return fmt.Errorf("authoring: load bundle %q: %w", bundleId, err)
	}
	if bundle.Id == "" {
		return fmt.Errorf("authoring: bundle %q not found", bundleId)
	}
	if bundle.OwnerUserId != strings.TrimSpace(owner) {
		return fmt.Errorf("authoring: actor %q cannot retire bundle owned by %q", owner, bundle.OwnerUserId)
	}
	return retireBundleRuntime(ctx, store, bundleId, bundle.OwnerUserId, deps)
}

// emitAuthoredAudit sends an audit event to the sink when one is wired.
func emitAuthoredAudit(ctx context.Context, deps AuthoredRuntimeDeps, ev AuthoredAuditEvent) {
	if deps.Audit != nil {
		deps.Audit.EmitAuthoredAudit(ctx, ev)
	}
}

// engineActivationStore is the production ActivationStore over a live engine.
type engineActivationStore struct {
	engine *MemQLEngine
}

func (s *engineActivationStore) LoadBundle(ctx context.Context, bundleId string) (AuthoringBundleRow, error) {
	q := fmt.Sprintf(`queryAuthoringBundleById({"bundleId":%q})`, bundleId)
	res, err := s.engine.Execute(ctx, q)
	if err != nil {
		return AuthoringBundleRow{}, err
	}
	if res == nil || res.Bundle == nil || len(res.Bundle.Nodes) == 0 {
		return AuthoringBundleRow{}, nil
	}
	node := res.Bundle.Nodes[0]
	payload, err := nodePayloadJSON(node)
	if err != nil {
		return AuthoringBundleRow{}, err
	}
	return parseBundleRow(node.GetId(), payload)
}

func (s *engineActivationStore) LoadConstructs(ctx context.Context, bundleId string) ([]AuthoringConstructRow, error) {
	q := fmt.Sprintf(`queryAuthoringConstructsForBundle({"bundleId":%q})`, bundleId)
	res, err := s.engine.Execute(ctx, q)
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

func (s *engineActivationStore) SetBundleActive(ctx context.Context, bundleId string) error {
	_, err := s.engine.Execute(ctx, fmt.Sprintf(`mutationActivateAuthoringBundle({"bundleId":%q})`, bundleId))
	return err
}

func (s *engineActivationStore) SetBundleRetired(ctx context.Context, bundleId string) error {
	_, err := s.engine.Execute(ctx, fmt.Sprintf(`mutationRetireAuthoringBundle({"bundleId":%q})`, bundleId))
	return err
}

func (s *engineActivationStore) SetConstructStatus(ctx context.Context, constructId, status string) error {
	args, err := json.Marshal(map[string]string{"constructId": constructId, "status": status})
	if err != nil {
		return err
	}
	_, err = s.engine.Execute(ctx, "mutationSetConstructStatus("+string(args)+")")
	return err
}

// nodePayloadJSON renders a query result node's structpb payload as JSON bytes
// so it can decode into the typed row structs. An empty payload returns nil.
func nodePayloadJSON(node *memqlv1.MemoryNode) ([]byte, error) {
	if node == nil || node.GetPayload() == nil {
		return nil, nil
	}
	b, err := node.GetPayload().MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("authoring: marshal node payload: %w", err)
	}
	return b, nil
}
