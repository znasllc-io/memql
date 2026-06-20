package memql

// authoring_rearm.go -- boot-time re-arm of already-active authored bundles
// (epic memql#954, issue #1039).
//
// Activation (authoring_activation_engine.go) registers a bundle's constructs
// into the authored runtime + scheduler IN-PROCESS, but that live state is not
// durable: a process restart loses every registration. The persisted truth is
// the v1:authoring:bundle.status == "active" rows in the graph. On boot the
// authored runtime must walk those rows -- across ALL owners -- and re-register
// each active bundle's constructs so its automation fires again, with no manual
// re-approval.
//
// Two seams keep this honest:
//
//  1. CROSS-OWNER ENUMERATION is admin-gated. The active bundles span owners, so
//     the enumerating query (querySystemActiveAuthoringBundles) is the ADMIN /
//     cluster-owner-scoped read -- the only authoring query that is NOT
//     owner-filtered. The re-arm runs it under a cluster-owner envelope.
//
//  2. PER-BUNDLE WORK stays under the BUNDLE AUTHOR's envelope. Loading a
//     bundle's member constructs goes through the SAME owner-scoped query the
//     interactive path uses (queryAuthoringConstructsForBundle, filtered on
//     payload.ownerUserId == actor.userId), run under the author's userId -- so
//     the re-arm never reads another owner's constructs through an admin bypass.
//     The scheduler then runs each firing under AuthorContext (the author's
//     writer envelope), exactly as the live activation path does.
//
// Governance is preserved automatically: the scheduler enforces the global kill
// switch (#1034) and the cluster owner gate (#1030) at FIRE time inside
// runUnderAuthor, so a restart neither double-fires (the owner gate keeps an
// owner's automation on one node) nor bypasses a halt (the kill switch suppresses
// every firing when off). Re-arm only re-establishes the registrations; it does
// not re-fire anything itself.

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
)

// registerBundleConstructs registers an activation plan's constructs into the
// authored runtime: every construct into the owner-scoped registry, and every
// `automation` construct additionally into the live scheduler via the register
// hook so its trigger fires. It is the shared registration step factored out of
// the Gate-3 activation path so the boot re-arm re-establishes EXACTLY the same
// runtime state a fresh activation does (no drift between the two). The first
// failure is returned; partial registration is left in place (the caller logs
// and continues -- one malformed bundle must not abort the whole re-arm).
func registerBundleConstructs(register []*AuthoredConstruct, deps AuthoredRuntimeDeps) error {
	for _, c := range register {
		if err := deps.Registry.Register(c); err != nil {
			return fmt.Errorf("authoring: register %s/%s into runtime: %w", c.Kind, c.Name, err)
		}
		if c.Kind == "automation" && deps.RegisterHook != nil {
			if err := deps.RegisterHook(c); err != nil {
				return fmt.Errorf("authoring: schedule automation %s: %w", c.Name, err)
			}
		}
	}
	return nil
}

// RearmResult reports what a boot re-arm did: how many active bundles were seen,
// how many re-registered successfully, how many were skipped as inert (zero
// member constructs), and the bundle ids that failed (so the boot log records
// exactly which capabilities did NOT come back).
type RearmResult struct {
	Seen      int      `json:"seen"`
	Rearmed   int      `json:"rearmed"`
	Skipped   int      `json:"skipped,omitempty"`
	FailedIds []string `json:"failedIds,omitempty"`
}

// RearmActiveBundles re-registers every already-active authored bundle across
// all owners into the live runtime + scheduler, so a restart restores the
// authored-automation surface without manual re-approval. It is called once at
// engine boot from wireAuthoredRuntime, after the registry + scheduler exist.
//
// It enumerates the active bundles through the admin/system-scoped query, then
// for each bundle loads its constructs under the bundle AUTHOR's envelope and
// re-registers them via the shared registration step. A per-bundle failure is
// logged and skipped (the others still come back); the result records the
// failures. Returns an error only for a failure that prevents enumerating at
// all (e.g. the registry is missing).
func (e *MemQLEngine) RearmActiveBundles(ctx context.Context, deps AuthoredRuntimeDeps) (RearmResult, error) {
	return rearmActiveBundlesWithStore(ctx, &engineRearmStore{engine: e}, deps)
}

// rearmStore is the narrow graph surface the re-arm needs: enumerate the
// system-active bundles (admin-scoped), and load one bundle owner's constructs
// (owner-scoped). *MemQLEngine satisfies it via engineRearmStore; tests fake it.
type rearmStore interface {
	// LoadSystemActiveBundles returns every active bundle across all owners,
	// read under a cluster-owner envelope (the admin-scoped query).
	LoadSystemActiveBundles(ctx context.Context) ([]AuthoringBundleRow, error)
	// LoadConstructsForOwner returns a bundle's member constructs, read under
	// the bundle AUTHOR's envelope (the owner-scoped query) so no cross-owner
	// read happens.
	LoadConstructsForOwner(ctx context.Context, owner, bundleId string) ([]AuthoringConstructRow, error)
}

// errEmptyBundle is an internal sentinel returned by rearmOneBundle when a
// bundle has no member constructs. It signals "nothing to do" (not a failure)
// so the caller can count it as skipped rather than failed. It is not exported.
var errEmptyBundle = fmt.Errorf("authoring: active bundle has no member constructs (inert -- skipped)")

// rearmActiveBundlesWithStore is the store-driven core, split out so it is unit
// testable with a fake rearmStore (no live DB).
func rearmActiveBundlesWithStore(ctx context.Context, store rearmStore, deps AuthoredRuntimeDeps) (RearmResult, error) {
	if deps.Registry == nil {
		return RearmResult{}, fmt.Errorf("authoring: re-arm requires an authored runtime registry")
	}

	bundles, err := store.LoadSystemActiveBundles(ctx)
	if err != nil {
		return RearmResult{}, fmt.Errorf("authoring: enumerate active bundles for re-arm: %w", err)
	}

	result := RearmResult{Seen: len(bundles)}
	for _, bundle := range bundles {
		rearmErr := rearmOneBundle(ctx, store, bundle, deps)
		if rearmErr == errEmptyBundle {
			// Empty active bundle -- inert, not a failure. Don't add to FailedIds
			// and don't emit a per-bundle WARN. The summary log at the end of the
			// boot sequence already includes the Skipped count.
			result.Skipped++
			continue
		}
		if rearmErr != nil {
			result.FailedIds = append(result.FailedIds, bundle.Id)
			if deps.Logger != nil {
				deps.Logger.Warn("authored bundle re-arm failed (other bundles unaffected)",
					"bundleId", bundle.Id, "owner", bundle.OwnerUserId, "error", rearmErr)
			}
			continue
		}
		result.Rearmed++
		if deps.Logger != nil {
			deps.Logger.Info("authored bundle re-armed",
				"bundleId", bundle.Id, "owner", bundle.OwnerUserId, "version", bundle.Version)
		}
	}
	return result, nil
}

// rearmOneBundle re-registers a single active bundle's constructs. An active
// bundle is treated as if it had just passed Gate 2 for the purpose of computing
// the registration plan: the SAME PlanBundleActivation drives the re-register so
// the runtime state matches a fresh activation. The plan's catalog-promotion +
// status-transition outputs are ignored here -- the bundle is already active and
// its deps are already catalogued; re-arm only re-establishes the live
// registrations.
//
// If the bundle has zero member constructs it is treated as inert and skipped
// (returns nil). This handles the mcp-promote-* class of bundles that are
// enumerated by the system-active query but handled by RehydratePromotedConstructs
// on the separate rehydration path, as well as any bundle left partially created
// by a failed write (bundle activated, construct write failed). Returning nil
// keeps the bundle out of the FailedIds list so it does not produce a per-bundle
// WARN flood on every boot.
func rearmOneBundle(ctx context.Context, store rearmStore, bundle AuthoringBundleRow, deps AuthoredRuntimeDeps) error {
	constructs, err := store.LoadConstructsForOwner(ctx, bundle.OwnerUserId, bundle.Id)
	if err != nil {
		return fmt.Errorf("load constructs: %w", err)
	}
	if len(constructs) == 0 {
		// No member constructs: treat as inert. Two legitimate scenarios:
		//   1. mcp-promote-* bundle -- plain-construct promotions handled by
		//      RehydratePromotedConstructs; the automation re-arm should skip them.
		//      (The production store also filters these out by prefix, but this
		//      guard makes fakeRearmStore-based tests and any future store
		//      implementations safe by default.)
		//   2. Partially-created bundle -- bundle was activated but the subsequent
		//      construct write failed, leaving an empty active bundle.
		// Return the sentinel so the caller counts this as Skipped, not Failed.
		return errEmptyBundle
	}

	// Reuse the activation planner to compute the construct set to register.
	// PlanBundleActivation requires the dryRunPassed status (its activation
	// pre-condition); an active bundle has already moved past it, so present the
	// pre-activation status for the pure planning step. This does NOT mutate the
	// persisted row -- it is a local copy used only to derive the Register set.
	planning := bundle
	planning.Status = BundleDryRunPassed
	plan, err := PlanBundleActivation(planning, constructs)
	if err != nil {
		return fmt.Errorf("plan re-register: %w", err)
	}
	if err := registerBundleConstructs(plan.Register, deps); err != nil {
		return err
	}
	return nil
}

// engineRearmStore is the production rearmStore over a live engine. The two
// reads run under deliberately different envelopes: the enumeration under a
// cluster-owner envelope (admin-scoped query), the per-bundle construct load
// under the bundle author's envelope (owner-scoped query).
type engineRearmStore struct {
	engine *MemQLEngine
}

func (s *engineRearmStore) LoadSystemActiveBundles(ctx context.Context) ([]AuthoringBundleRow, error) {
	// The system-scoped query is cluster-owner gated; run it under a
	// cluster-owner envelope so it returns every owner's active bundles.
	ownerCtx := rearmClusterOwnerContext(ctx)
	res, err := s.engine.Execute(ownerCtx, "querySystemActiveAuthoringBundles()")
	if err != nil {
		return nil, err
	}
	if res == nil || res.Bundle == nil {
		return nil, nil
	}
	out := make([]AuthoringBundleRow, 0, len(res.Bundle.Nodes))
	for _, node := range res.Bundle.Nodes {
		// Skip durable-promote bundles: they are NOT automation bundles and are
		// rehydrated by RehydratePromotedConstructs on a separate boot path.
		// Including them here would cause a spurious WARN for every mcp-promote-*
		// bundle that exists (they have no automation constructs, so re-arm has
		// nothing to do for them). Mirror the same prefix filter that
		// enginePromoteRehydrateStore.LoadPromotedBundles applies in reverse.
		if strings.HasPrefix(node.GetId(), durablePromoteBundlePrefix) {
			continue
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

func (s *engineRearmStore) LoadConstructsForOwner(ctx context.Context, owner, bundleId string) ([]AuthoringConstructRow, error) {
	// Load under the bundle AUTHOR's envelope: the owner-scoped query filters on
	// payload.ownerUserId == actor.userId, so stamping the author's userId reads
	// exactly that owner's constructs -- no cross-owner admin bypass.
	authorCtx := auth.ContextWithAccess(ctx, &auth.AccessContext{
		UserId: owner,
		Role:   auth.RoleWriter,
	})
	q := fmt.Sprintf(`queryAuthoringConstructsForBundle({"bundleId":%q})`, bundleId)
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

// rearmClusterOwnerContext stamps a cluster-owner envelope onto ctx so the
// admin-scoped system query (actor.isClusterOwner == true) returns rows. This is
// the ONE admin elevation in the re-arm, scoped to the cross-owner enumeration
// only -- every per-bundle read + every fired automation drops back to the
// author's envelope.
func rearmClusterOwnerContext(ctx context.Context) context.Context {
	return auth.ContextWithAccess(ctx, &auth.AccessContext{
		UserId: "_system:authored-rearm",
		Role:   auth.RoleOwner,
	})
}
