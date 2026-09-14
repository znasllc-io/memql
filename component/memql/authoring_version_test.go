package memql_test

// authoring_version_test.go -- coverage for edit/version semantics + impact
// analysis re-validation (epic memql#954, issue #961, increment 4).
//
//   - PlanVersionSupersession: a new version must point back at the prior
//     bundle, share its owner, and carry a strictly greater version.
//   - AnalyzeImpact / AnalyzeConstructEdit: editing a shared construct
//     re-validates every ACTIVE dependent through Gate 1 (SandboxCompileBundle)
//     with the edit overlaid; the new version goes live only if every dependent
//     still compiles.

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

func bundleRow(id, owner string, version int, supersedes string) memql.AuthoringBundleRow {
	return memql.AuthoringBundleRow{Id: id, OwnerUserId: owner, Version: version, SupersedesBundleId: supersedes, Status: memql.BundleDryRunPassed}
}

// TestPlanVersionSupersession_HappyPath: a well-formed new version supersedes
// the prior.
func TestPlanVersionSupersession_HappyPath(t *testing.T) {
	prior := bundleRow("b1", "user-a", 1, "")
	next := bundleRow("b2", "user-a", 2, "b1")
	sup, err := memql.PlanVersionSupersession(prior, next)
	if err != nil {
		t.Fatalf("PlanVersionSupersession: %v", err)
	}
	if sup.PriorBundleId != "b1" || sup.NewBundleId != "b2" || sup.NewVersion != 2 {
		t.Errorf("unexpected supersession: %+v", sup)
	}
}

// TestPlanVersionSupersession_Rejects: missing back-pointer, cross-owner, or a
// non-increasing version are all rejected.
func TestPlanVersionSupersession_Rejects(t *testing.T) {
	prior := bundleRow("b1", "user-a", 2, "")

	// No back-pointer.
	if _, err := memql.PlanVersionSupersession(prior, bundleRow("b2", "user-a", 3, "")); err == nil {
		t.Error("expected a missing supersedesBundleId to be rejected")
	}
	// Cross-owner.
	if _, err := memql.PlanVersionSupersession(prior, bundleRow("b2", "user-b", 3, "b1")); err == nil {
		t.Error("expected a cross-owner supersession to be rejected")
	}
	// Non-increasing version.
	if _, err := memql.PlanVersionSupersession(prior, bundleRow("b2", "user-a", 2, "b1")); err == nil {
		t.Error("expected a non-increasing version to be rejected")
	}
}

// --- impact analysis ---

// A shared spec the dependents bind against.
const sharedSpecGood = `trait isRefund = row => row.kind == "refund"`

// An edit that still compiles -- dependents must re-validate clean.
const sharedSpecEditGood = `trait isRefund = row => row.kind == "refundEscalation"`

// An edit that does NOT parse -- dependents must FAIL re-validation.
const sharedSpecEditBroken = `trait isRefund = row => row.kind ==`

// dependentConstructs is a dependent bundle: the shared trait and a query
// over the core authoring bundle concept. Gate 1 compiles against a clone of
// the process-wide concept registry, and the query's filter is lowered against
// the concept it binds (memql#5366), so the core concepts are loaded first --
// left to whichever test ran before, the query bound nothing when this file
// ran alone, and failed re-validation for THAT: the good edit read as broken,
// and the broken edit's tests passed for a reason that had nothing to do with
// the edit.
func dependentConstructs(t *testing.T) []memql.SandboxConstruct {
	t.Helper()
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("load the core concepts: %v", err)
	}
	return []memql.SandboxConstruct{
		{Kind: "trait", Name: "isRefund", Source: sharedSpecGood},
		{Kind: "query", Name: "queryRefundsForOwner", Source: `use authoring.concepts.{ bundle }
@actor
query bundle queryRefundsForOwner {
  filter  row => row.ownerUserId == actor.userId
  shape   bundleFull
}`},
	}
}

// TestAnalyzeImpact_GoodEditPasses: editing the shared spec to another valid
// form re-validates every dependent clean -> AllPassed.
func TestAnalyzeImpact_GoodEditPasses(t *testing.T) {
	edited := memql.EditedConstruct{Kind: "trait", Name: "isRefund", NewSource: sharedSpecEditGood}
	res := memql.AnalyzeImpact(edited, map[string][]memql.SandboxConstruct{
		"dep-bundle-1": dependentConstructs(t),
	})
	if !res.AllPassed {
		t.Fatalf("a valid shared-construct edit should pass impact analysis: %+v", res.Dependents)
	}
	if len(res.Dependents) != 1 || res.Dependents[0].BundleId != "dep-bundle-1" || !res.Dependents[0].OK {
		t.Errorf("unexpected dependent result: %+v", res.Dependents)
	}
}

// TestAnalyzeImpact_BrokenEditFails: editing the shared spec to an unparseable
// form fails the dependent's Gate-1 re-validation -> AllPassed false, so the new
// version must NOT go live.
func TestAnalyzeImpact_BrokenEditFails(t *testing.T) {
	edited := memql.EditedConstruct{Kind: "trait", Name: "isRefund", NewSource: sharedSpecEditBroken}
	res := memql.AnalyzeImpact(edited, map[string][]memql.SandboxConstruct{
		"dep-bundle-1": dependentConstructs(t),
	})
	if res.AllPassed {
		t.Fatal("a broken shared-construct edit must FAIL impact analysis (dependents would break)")
	}
	if len(res.Dependents) != 1 || res.Dependents[0].OK {
		t.Fatalf("expected the dependent to fail re-validation: %+v", res.Dependents)
	}
	// And fail it for the EDIT: the one construct that fails is the edited
	// trait, and the query beside it still compiles.
	for _, d := range res.Dependents[0].Diagnostics {
		if d.OK != (d.Name != "isRefund") {
			t.Errorf("%s %s OK=%v: only the edited trait should fail (%s)", d.Kind, d.Name, d.OK, d.Error)
		}
	}
}

// TestAnalyzeImpact_NoDependents: a construct nothing depends on trivially
// passes (nothing to re-validate).
func TestAnalyzeImpact_NoDependents(t *testing.T) {
	edited := memql.EditedConstruct{Kind: "trait", Name: "isRefund", NewSource: sharedSpecEditGood}
	res := memql.AnalyzeImpact(edited, nil)
	if !res.AllPassed || len(res.Dependents) != 0 {
		t.Errorf("no dependents should trivially pass: %+v", res)
	}
}

// fakeImpactStore serves a fixed dependent set.
type fakeImpactStore struct {
	dependents map[string][]memql.SandboxConstruct
}

func (s fakeImpactStore) ActiveDependentBundleIds(_ context.Context, _, _, _ string) ([]string, error) {
	ids := make([]string, 0, len(s.dependents))
	for id := range s.dependents {
		ids = append(ids, id)
	}
	return ids, nil
}
func (s fakeImpactStore) ConstructsAsSandbox(_ context.Context, bundleId string) ([]memql.SandboxConstruct, error) {
	return s.dependents[bundleId], nil
}

// TestAnalyzeConstructEdit_StoreDriven: the engine-backed path loads dependents
// through the store and re-validates them; a broken edit fails the gate.
func TestAnalyzeConstructEdit_StoreDriven(t *testing.T) {
	store := fakeImpactStore{dependents: map[string][]memql.SandboxConstruct{
		"dep-1": dependentConstructs(t),
	}}
	edited := memql.EditedConstruct{Kind: "trait", Name: "isRefund", NewSource: sharedSpecEditBroken}
	res, err := memql.AnalyzeConstructEditWithStore(context.Background(), store, "user-a", edited)
	if err != nil {
		t.Fatalf("AnalyzeConstructEditWithStore: %v", err)
	}
	if res.AllPassed {
		t.Error("a broken edit must fail the impact gate via the store-driven path")
	}

	// Empty owner / missing identity rejected.
	if _, err := memql.AnalyzeConstructEditWithStore(context.Background(), store, "", edited); err == nil {
		t.Error("expected an empty-owner impact analysis to error")
	}
}
