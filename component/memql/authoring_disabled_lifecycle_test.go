package memql

// authoring_disabled_lifecycle_test.go -- the authored-construct lifecycle for
// @disabled (memql#2643, follow-up to the #2607 intentional-skip contract).
//
// compileAuthoredSpec stores (*Spec)(nil) for a @disabled authored spec. That
// state is INTENTIONAL, not a failure, and every authoring surface must say so:
//
//   - promotion refuses it with an actionable "@disabled; enable it" message,
//     never the misleading "construct is not compiled";
//   - durable re-hydration of a stored @disabled row skips WITHOUT the
//     quarantine channel (no ERROR log, no LoadReport entry) and reserves the
//     name against authored promotion (the resurrection guard);
//   - the sandbox names a @disabled capability instead of claiming "no
//     capability declaration found in source";
//   - the disabled-CORE-name promote guard is pinned (the PR #2642 review
//     added it untested).

import (
	"context"
	"strings"
	"testing"
)

const sessionDisabledSpecSrc = `@disabled
@description("session spec, deliberately disabled")
spec actorEnvelope mcpDisabledSpec {
  return role == "admin"
}`

const sessionDisabledCapSrc = `@disabled
@sideEffect("read")
capability fs.readFile {
  args {
    subject string @required
  }
}`

// TestPromoteAuthoredConstruct_DisabledSpec_ActionableMessage: promoting a
// @disabled authored spec is refused with a message naming the ACTUAL state.
// The spec compiled fine and was deliberately disabled -- "construct is not
// compiled" points the author at the wrong fix.
func TestPromoteAuthoredConstruct_DisabledSpec_ActionableMessage(t *testing.T) {
	e := &MemQLEngine{specs: newSpecRegistry()}
	reg := NewAuthoredRuntimeRegistry()
	if _, err := AuthorSessionBundle(reg, "owner-1", sessionDisabledSpecSrc); err != nil {
		t.Fatalf("author @disabled session spec (Gate-1 must pass an intentional disable): %v", err)
	}
	c, ok := reg.Lookup("owner-1", "spec", "mcpDisabledSpec")
	if !ok {
		t.Fatal("@disabled session spec not registered as session metadata")
	}

	err := e.PromoteAuthoredConstruct(context.Background(), c)
	if err == nil {
		t.Fatal("promoting a @disabled spec must be refused")
	}
	if !strings.Contains(err.Error(), "@disabled") {
		t.Errorf("refusal must name the @disabled state, got: %v", err)
	}
	if strings.Contains(err.Error(), "not compiled") {
		t.Errorf("refusal claims a compile failure for an intentional disable: %v", err)
	}
	if _, ok := e.specs.Lookup("mcpDisabledSpec"); ok {
		t.Error("a refused promote must register nothing")
	}
}

// TestPromoteConstructDurable_DisabledSpec_NothingPersisted: the durable
// promote funnels through the same refusal, so a @disabled construct never
// persists a reviewable row (a refused promotion never persists).
func TestPromoteConstructDurable_DisabledSpec_NothingPersisted(t *testing.T) {
	e := &MemQLEngine{specs: newSpecRegistry()}
	reg := NewAuthoredRuntimeRegistry()
	if _, err := AuthorSessionBundle(reg, "owner-1", sessionDisabledSpecSrc); err != nil {
		t.Fatalf("author @disabled session spec: %v", err)
	}
	c, _ := reg.Lookup("owner-1", "spec", "mcpDisabledSpec")

	store := &fakePromoteStore{}
	err := e.promoteConstructDurableWithStore(context.Background(), store, "owner-1", c)
	if err == nil || !strings.Contains(err.Error(), "@disabled") {
		t.Fatalf("durable promote of a @disabled spec must be refused with the @disabled message, got: %v", err)
	}
	if len(store.bundles) != 0 || len(store.constructs) != 0 {
		t.Error("a refused (@disabled) durable promote must persist nothing")
	}
}

// TestPromoteAuthoredConstruct_DisabledCoreName_Refused pins the
// disabled-core-name reservation guard (PR #2642 review round, previously
// untested): an authored spec cannot claim a name the core tree deliberately
// retired with @disabled (the ToolRegistry resurrection precedent,
// #2606/#2607).
func TestPromoteAuthoredConstruct_DisabledCoreName_Refused(t *testing.T) {
	e := &MemQLEngine{specs: newSpecRegistry()}
	e.specs.MarkDisabled("retiredCoreSpec")

	c := &AuthoredConstruct{OwnerUserId: "owner-1", Kind: "spec", Name: "retiredCoreSpec", Status: AuthoredActive,
		Compiled: &Spec{Name: "retiredCoreSpec"}}
	err := e.PromoteAuthoredConstruct(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "@disabled core construct owns that name") {
		t.Fatalf("promote onto a retired core name must be refused with the reservation message, got: %v", err)
	}
	if _, ok := e.specs.Lookup("retiredCoreSpec"); ok {
		t.Error("a refused promote must register nothing")
	}
}

// TestRehydrate_DisabledStoredSpec_SkipsWithoutQuarantine: a stored @disabled
// authored spec row (persisted by an older engine or an activated bundle)
// re-hydrates as an intentional skip: no Failed entry, no LoadReport
// quarantine (quarantineRehydratedConstruct is the ONLY error-log site on this
// path, so an empty quarantine list also proves no ERROR log), the name
// reserved against authored promotion, and nothing registered.
func TestRehydrate_DisabledStoredSpec_SkipsWithoutQuarantine(t *testing.T) {
	e := &MemQLEngine{specs: newSpecRegistry()}
	e.loadReport = newLoadReport()
	store := &fakeRehydrateStore{
		bundles: []AuthoringBundleRow{{Id: durablePromoteBundlePrefix + "b1", OwnerUserId: "owner-1", Status: BundleActive}},
		constructs: map[string][]AuthoringConstructRow{
			durablePromoteBundlePrefix + "b1": {{
				Id: "c1", OwnerUserId: "owner-1", BundleId: durablePromoteBundlePrefix + "b1",
				Kind: "spec", Name: "mcpDisabledSpec", Source: sessionDisabledSpecSrc,
			}},
		},
	}

	res, err := e.rehydratePromotedNow(context.Background(), store)
	if err != nil {
		t.Fatalf("re-hydrate: %v", err)
	}
	if len(res.Failed) != 0 {
		t.Fatalf("an intentional disable must not read as a failure: %+v", res.Failed)
	}
	if res.Rehydrated != 0 {
		t.Errorf("a @disabled row registers nothing, so it must not count as re-hydrated: %+v", res)
	}
	if res.SkippedDisabled != 1 {
		t.Errorf("re-hydrate result = %+v, want skippedDisabled=1", res)
	}
	if len(e.loadReport.Quarantined) != 0 {
		t.Errorf("intentional disable leaked into the quarantine channel: %+v", e.loadReport.Quarantined)
	}
	if !e.specs.IsDisabled("mcpDisabledSpec") {
		t.Error("re-hydration must reserve the @disabled name (resurrection guard)")
	}
	if _, ok := e.specs.Lookup("mcpDisabledSpec"); ok {
		t.Error("a @disabled row must not register into the shared registry")
	}
}

// TestSandboxCompile_DisabledCapability_NamesTheState: authoring a @disabled
// capability in a bundle is refused at Gate-1 (it compiles to nothing, so the
// bundle would carry dead source), but the diagnostic must name the actual
// state -- the source plainly CONTAINS a capability declaration.
func TestSandboxCompile_DisabledCapability_NamesTheState(t *testing.T) {
	report, _ := compileBundle(SplitBundleSource(sessionDisabledCapSrc))
	if report.OK {
		t.Fatal("a bundle authoring a @disabled capability must fail Gate-1")
	}
	var d *SandboxDiagnostic
	for i := range report.Diagnostics {
		if report.Diagnostics[i].Kind == "capability" {
			d = &report.Diagnostics[i]
		}
	}
	if d == nil {
		t.Fatalf("no capability diagnostic in report: %+v", report.Diagnostics)
	}
	if !strings.Contains(d.Error, `capability "fs.readFile" is @disabled`) {
		t.Errorf("diagnostic must name the @disabled capability, got: %q", d.Error)
	}
	if strings.Contains(d.Error, "no capability declaration found") {
		t.Errorf("diagnostic claims the declaration is missing when it is @disabled: %q", d.Error)
	}
}

const sessionCorrectedSpecSrc = `@description("session spec, re-enabled")
spec actorEnvelope mcpDisabledSpec {
  return role == "admin"
}`

// TestRehydrateBundle_DisabledStoredSpec_SkipsWithoutFailed: the LIVE
// propagation walk (the authoring.promote broadcast path peer replicas run)
// must classify a stored @disabled row exactly like the boot walk --
// skippedDisabled, never Failed, never quarantined. The two walks share
// recompileAndPromoteRow; the classification must not diverge.
func TestRehydrateBundle_DisabledStoredSpec_SkipsWithoutFailed(t *testing.T) {
	e := &MemQLEngine{specs: newSpecRegistry()}
	e.loadReport = newLoadReport()
	bundleId := durablePromoteBundlePrefix + "b1"
	store := &fakeRehydrateStore{
		bundles: []AuthoringBundleRow{{Id: bundleId, OwnerUserId: "owner-1", Status: BundleActive}},
		constructs: map[string][]AuthoringConstructRow{
			bundleId: {{
				Id: "c1", OwnerUserId: "owner-1", BundleId: bundleId,
				Kind: "spec", Name: "mcpDisabledSpec", Source: sessionDisabledSpecSrc,
			}},
		},
	}

	res, err := e.rehydratePromotedBundleNow(context.Background(), store, "owner-1", bundleId)
	if err != nil {
		t.Fatalf("bundle re-hydrate: %v", err)
	}
	if len(res.Failed) != 0 {
		t.Fatalf("an intentional disable must not read as a failure on the propagation path: %+v", res.Failed)
	}
	if res.SkippedDisabled != 1 || res.Rehydrated != 0 {
		t.Errorf("propagation walk result = %+v, want skippedDisabled=1 rehydrated=0 (boot-walk parity)", res)
	}
	if len(e.loadReport.Quarantined) != 0 {
		t.Errorf("intentional disable leaked into the quarantine channel: %+v", e.loadReport.Quarantined)
	}
	if !e.specs.IsDisabled("mcpDisabledSpec") {
		t.Error("propagation walk must reserve the @disabled name")
	}
}

// TestRehydrate_DisabledName_ReenableByCorrectedPromote: the reservation a
// stored @disabled AUTHORED row places at re-hydration must be releasable by
// its own lifecycle -- promoting the CORRECTED (enabled) source, exactly what
// the promote refusal message instructs, lifts it. Only a CORE @disabled name
// (no authored marker) is permanently reserved.
func TestRehydrate_DisabledName_ReenableByCorrectedPromote(t *testing.T) {
	e := &MemQLEngine{specs: newSpecRegistry()}
	e.loadReport = newLoadReport()
	store := &fakeRehydrateStore{
		bundles: []AuthoringBundleRow{{Id: durablePromoteBundlePrefix + "b1", OwnerUserId: "owner-1", Status: BundleActive}},
		constructs: map[string][]AuthoringConstructRow{
			durablePromoteBundlePrefix + "b1": {{
				Id: "c1", OwnerUserId: "owner-1", BundleId: durablePromoteBundlePrefix + "b1",
				Kind: "spec", Name: "mcpDisabledSpec", Source: sessionDisabledSpecSrc,
			}},
		},
	}
	if _, err := e.rehydratePromotedNow(context.Background(), store); err != nil {
		t.Fatalf("re-hydrate: %v", err)
	}

	reg := NewAuthoredRuntimeRegistry()
	if _, err := AuthorSessionBundle(reg, "owner-1", sessionCorrectedSpecSrc); err != nil {
		t.Fatalf("author corrected session spec: %v", err)
	}
	c, _ := reg.Lookup("owner-1", "spec", "mcpDisabledSpec")
	if err := e.PromoteAuthoredConstruct(context.Background(), c); err != nil {
		t.Fatalf("promoting the corrected (enabled) source must lift the authored @disabled reservation, got: %v", err)
	}
	if _, ok := e.specs.Lookup("mcpDisabledSpec"); !ok {
		t.Error("corrected spec not registered after re-enable")
	}
	if e.specs.IsDisabled("mcpDisabledSpec") {
		t.Error("reservation must be lifted by the corrected promote")
	}
}

// TestRehydrate_DisabledName_RetireByDemote: the demote is the documented
// retire path for the authored reservation -- it releases the name and clears
// the marker, and a second demote errors (nothing left to own).
func TestRehydrate_DisabledName_RetireByDemote(t *testing.T) {
	e := &MemQLEngine{specs: newSpecRegistry()}
	e.loadReport = newLoadReport()
	store := &fakeRehydrateStore{
		bundles: []AuthoringBundleRow{{Id: durablePromoteBundlePrefix + "b1", OwnerUserId: "owner-1", Status: BundleActive}},
		constructs: map[string][]AuthoringConstructRow{
			durablePromoteBundlePrefix + "b1": {{
				Id: "c1", OwnerUserId: "owner-1", BundleId: durablePromoteBundlePrefix + "b1",
				Kind: "spec", Name: "mcpDisabledSpec", Source: sessionDisabledSpecSrc,
			}},
		},
	}
	if _, err := e.rehydratePromotedNow(context.Background(), store); err != nil {
		t.Fatalf("re-hydrate: %v", err)
	}

	if err := e.DemoteAuthoredConstruct(context.Background(), "spec", "mcpDisabledSpec"); err != nil {
		t.Fatalf("demoting the authored @disabled reservation must succeed (the retire path), got: %v", err)
	}
	if e.specs.IsDisabled("mcpDisabledSpec") {
		t.Error("demote must release the @disabled reservation")
	}
	if err := e.DemoteAuthoredConstruct(context.Background(), "spec", "mcpDisabledSpec"); err == nil {
		t.Error("second demote must error: the name is no longer author-owned")
	}
}

// TestPromoteAuthoredConstruct_CoreDisabledName_StillRefused: a CORE @disabled
// name (marked by the loaders, no authored marker) stays permanently reserved
// -- the re-enable path applies ONLY to reservations an authored row placed.
func TestPromoteAuthoredConstruct_CoreDisabledName_StillRefused(t *testing.T) {
	e := &MemQLEngine{specs: newSpecRegistry()}
	e.specs.MarkDisabled("retiredCoreSpec")
	c := &AuthoredConstruct{OwnerUserId: "owner-1", Kind: "spec", Name: "retiredCoreSpec", Status: AuthoredActive,
		Compiled: &Spec{Name: "retiredCoreSpec"}}
	if err := e.PromoteAuthoredConstruct(context.Background(), c); err == nil || !strings.Contains(err.Error(), "@disabled core construct owns that name") {
		t.Fatalf("core-retired name must stay refused, got: %v", err)
	}
	if !e.specs.IsDisabled("retiredCoreSpec") {
		t.Error("a refused promote must not lift a core reservation")
	}
}

// TestRehydrate_DisabledRow_DoesNotPoisonActiveCoreSpec: a stored @disabled
// AUTHORED row whose name a LIVE core spec owns (cross-version drift) must not
// touch it: no IsDisabled mark (the validator would refuse calls), no authored
// marker (demote could then REMOVE core -- the never-shadow invariant in
// reverse). The row is still an intentional skip, not a failure.
func TestRehydrate_DisabledRow_DoesNotPoisonActiveCoreSpec(t *testing.T) {
	e := &MemQLEngine{specs: newSpecRegistry()}
	e.loadReport = newLoadReport()
	if err := e.specs.Upsert("mcpDisabledSpec", &Spec{Name: "mcpDisabledSpec", ExprSource: "core"}); err != nil {
		t.Fatalf("seed core spec: %v", err)
	}
	store := &fakeRehydrateStore{
		bundles: []AuthoringBundleRow{{Id: durablePromoteBundlePrefix + "b1", OwnerUserId: "owner-1", Status: BundleActive}},
		constructs: map[string][]AuthoringConstructRow{
			durablePromoteBundlePrefix + "b1": {{
				Id: "c1", OwnerUserId: "owner-1", BundleId: durablePromoteBundlePrefix + "b1",
				Kind: "spec", Name: "mcpDisabledSpec", Source: sessionDisabledSpecSrc,
			}},
		},
	}
	res, err := e.rehydratePromotedNow(context.Background(), store)
	if err != nil {
		t.Fatalf("re-hydrate: %v", err)
	}
	if len(res.Failed) != 0 || res.SkippedDisabled != 1 {
		t.Errorf("result = %+v, want skippedDisabled=1 failed=[]", res)
	}
	if e.specs.IsDisabled("mcpDisabledSpec") {
		t.Error("a stale @disabled authored row disabled a LIVE core spec")
	}
	if got, ok := e.specs.Lookup("mcpDisabledSpec"); !ok || got.ExprSource != "core" {
		t.Error("core spec no longer intact after re-hydrating the stale row")
	}
	if err := e.DemoteAuthoredConstruct(context.Background(), "spec", "mcpDisabledSpec"); err == nil {
		t.Fatal("the stale row must not plant a marker that lets demote remove a core spec")
	}
	if _, ok := e.specs.Lookup("mcpDisabledSpec"); !ok {
		t.Error("core spec was removed")
	}
}

// TestRehydrate_DisabledRow_DoesNotLiftCoreReservation: when the CORE tree
// already retired the name (MarkDisabled by the loaders, permanent by design),
// a stored @disabled authored row of the same name must not overwrite the
// reservation's provenance -- the corrected promote stays refused.
func TestRehydrate_DisabledRow_DoesNotLiftCoreReservation(t *testing.T) {
	e := &MemQLEngine{specs: newSpecRegistry()}
	e.loadReport = newLoadReport()
	e.specs.MarkDisabled("mcpDisabledSpec")
	store := &fakeRehydrateStore{
		bundles: []AuthoringBundleRow{{Id: durablePromoteBundlePrefix + "b1", OwnerUserId: "owner-1", Status: BundleActive}},
		constructs: map[string][]AuthoringConstructRow{
			durablePromoteBundlePrefix + "b1": {{
				Id: "c1", OwnerUserId: "owner-1", BundleId: durablePromoteBundlePrefix + "b1",
				Kind: "spec", Name: "mcpDisabledSpec", Source: sessionDisabledSpecSrc,
			}},
		},
	}
	if _, err := e.rehydratePromotedNow(context.Background(), store); err != nil {
		t.Fatalf("re-hydrate: %v", err)
	}

	reg := NewAuthoredRuntimeRegistry()
	if _, err := AuthorSessionBundle(reg, "owner-1", sessionCorrectedSpecSrc); err != nil {
		t.Fatalf("author corrected session spec: %v", err)
	}
	c, _ := reg.Lookup("owner-1", "spec", "mcpDisabledSpec")
	err := e.PromoteAuthoredConstruct(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "@disabled core construct owns that name") {
		t.Fatalf("the core reservation must survive a rehydrated authored row of the same name, got: %v", err)
	}
	if !e.specs.IsDisabled("mcpDisabledSpec") {
		t.Error("core reservation was lifted")
	}
}

// TestRehydrate_ReenabledSpec_SurvivesRefire: the durable promote of the
// corrected source does not rewrite the OLD stored @disabled row, so the next
// boot walk / promote broadcast re-fires over it. That stale row must not
// re-disable the now-live re-enabled spec -- the re-enable has to survive
// every subsequent replication tick.
func TestRehydrate_ReenabledSpec_SurvivesRefire(t *testing.T) {
	e := &MemQLEngine{specs: newSpecRegistry()}
	e.loadReport = newLoadReport()
	bundleId := durablePromoteBundlePrefix + "b1"
	store := &fakeRehydrateStore{
		bundles: []AuthoringBundleRow{{Id: bundleId, OwnerUserId: "owner-1", Status: BundleActive}},
		constructs: map[string][]AuthoringConstructRow{
			bundleId: {{
				Id: "c1", OwnerUserId: "owner-1", BundleId: bundleId,
				Kind: "spec", Name: "mcpDisabledSpec", Source: sessionDisabledSpecSrc,
			}},
		},
	}
	if _, err := e.rehydratePromotedNow(context.Background(), store); err != nil {
		t.Fatalf("re-hydrate: %v", err)
	}

	reg := NewAuthoredRuntimeRegistry()
	if _, err := AuthorSessionBundle(reg, "owner-1", sessionCorrectedSpecSrc); err != nil {
		t.Fatalf("author corrected session spec: %v", err)
	}
	c, _ := reg.Lookup("owner-1", "spec", "mcpDisabledSpec")
	if err := e.PromoteAuthoredConstruct(context.Background(), c); err != nil {
		t.Fatalf("re-enable promote: %v", err)
	}

	// The stale @disabled row re-fires (boot walk or promote broadcast).
	res, err := e.rehydratePromotedBundleNow(context.Background(), store, "owner-1", bundleId)
	if err != nil {
		t.Fatalf("re-fire: %v", err)
	}
	if len(res.Failed) != 0 {
		t.Errorf("re-fire result = %+v, want no failures", res)
	}
	if e.specs.IsDisabled("mcpDisabledSpec") {
		t.Error("the stale @disabled row re-disabled the live re-enabled spec")
	}
	if _, ok := e.specs.Lookup("mcpDisabledSpec"); !ok {
		t.Error("re-enabled spec lost after re-fire")
	}
}
