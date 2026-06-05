package memql_test

// authoring_activation_test.go -- coverage for Gate 3 (approval) + the
// activation transition (epic memql#954, issue #961, increment 1).
//
// Two layers:
//   - the pure plan (BuildApprovalArtifact / PlanBundleActivation): the
//     approval artifact framing + the register/promote decisions, no engine;
//   - the activation orchestrator end-to-end through a fake activationStore +
//     the REAL AuthoredRuntimeRegistry: a dryRunPassed bundle -> active, its
//     constructs registered + resolvable, net-new deps promoted, an activation
//     audit emitted. The fake store stands in for the persisted graph so the
//     test runs with no DB (mirroring the #957/#958 no-DB engine idiom).

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// --- fakes ---

// fakeActivationStore stands in for the persisted authoring graph. It records
// every lifecycle mutation so a test can assert the transition order, and
// serves the bundle + constructs from in-memory maps.
type fakeActivationStore struct {
	bundles    map[string]memql.AuthoringBundleRow
	constructs map[string][]memql.AuthoringConstructRow

	bundleActive  []string
	bundleRetired []string
	constructSet  map[string]string // constructId -> last status
}

func newFakeStore() *fakeActivationStore {
	return &fakeActivationStore{
		bundles:      map[string]memql.AuthoringBundleRow{},
		constructs:   map[string][]memql.AuthoringConstructRow{},
		constructSet: map[string]string{},
	}
}

func (s *fakeActivationStore) LoadBundle(_ context.Context, id string) (memql.AuthoringBundleRow, error) {
	return s.bundles[id], nil
}
func (s *fakeActivationStore) LoadConstructs(_ context.Context, id string) ([]memql.AuthoringConstructRow, error) {
	return s.constructs[id], nil
}
func (s *fakeActivationStore) SetBundleActive(_ context.Context, id string) error {
	s.bundleActive = append(s.bundleActive, id)
	b := s.bundles[id]
	b.Status = memql.BundleActive
	s.bundles[id] = b
	return nil
}
func (s *fakeActivationStore) SetBundleRetired(_ context.Context, id string) error {
	s.bundleRetired = append(s.bundleRetired, id)
	b := s.bundles[id]
	b.Status = memql.BundleRetired
	s.bundles[id] = b
	return nil
}
func (s *fakeActivationStore) SetConstructStatus(_ context.Context, id, status string) error {
	s.constructSet[id] = status
	return nil
}

// recordingAuditSink captures emitted audit events.
type recordingAuditSink struct{ events []memql.AuthoredAuditEvent }

func (r *recordingAuditSink) EmitAuthoredAudit(_ context.Context, ev memql.AuthoredAuditEvent) {
	r.events = append(r.events, ev)
}

// --- pure plan ---

func dryRunPassedBundle(owner string) memql.AuthoringBundleRow {
	return memql.AuthoringBundleRow{
		Id:          "v1:authoring:bundle:b1",
		OwnerUserId: owner,
		Title:       "Draft a reply on refund escalation",
		Summary:     "When a refund escalation lands, draft a reply.",
		Status:      memql.BundleDryRunPassed,
		Version:     1,
		DryRunReport: memql.BundleDryRunReport{
			OK:    true,
			Trace: []memql.DryRunStep{{StepId: "draft", StepType: "si", Status: "ok"}},
			SideEffectManifest: memql.SideEffectManifest{
				AiCalls: []memql.RecordedAiCall{{StepId: "draft", Function: "si", PromptTokens: 100, OutputTokens: 50}},
			},
			CostEstimate: memql.CostEstimate{Tokens: 150, Usd: 0.0021},
		},
	}
}

func bundleConstructs(owner, bundleId string) []memql.AuthoringConstructRow {
	return []memql.AuthoringConstructRow{
		{Id: "c-auto", OwnerUserId: owner, BundleId: bundleId, Kind: "automation", Name: "draftRefundReply", TargetNamespace: "authored", Source: `@enabled
@trigger(event="graph.node.created.*.v1:cognition:utterance")
automation draftRefundReply { step s { } }`},
		{Id: "c-spec", OwnerUserId: owner, BundleId: bundleId, Kind: "spec", Name: "isRefundEscalation", TargetNamespace: "authored", Source: `spec isRefundEscalation { payload.kind == "refund" }`},
	}
}

// TestBuildApprovalArtifact_FromDryRunReport: the Gate-3 artifact carries the
// dry-run trace + manifest + cost from a dryRunPassed bundle.
func TestBuildApprovalArtifact_FromDryRunReport(t *testing.T) {
	art, err := memql.BuildApprovalArtifact(dryRunPassedBundle("user-a"))
	if err != nil {
		t.Fatalf("BuildApprovalArtifact: %v", err)
	}
	if len(art.Trace) != 1 || art.CostEstimate.Tokens != 150 {
		t.Errorf("artifact missing dry-run evidence: %+v", art)
	}
	if len(art.SideEffectManifest.AiCalls) != 1 {
		t.Errorf("artifact missing side-effect manifest: %+v", art.SideEffectManifest)
	}
}

// TestBuildApprovalArtifact_RejectsNonDryRunPassed: you can only approve what
// passed Gate 2.
func TestBuildApprovalArtifact_RejectsNonDryRunPassed(t *testing.T) {
	b := dryRunPassedBundle("user-a")
	b.Status = memql.BundleValidated
	if _, err := memql.BuildApprovalArtifact(b); err == nil {
		t.Error("expected a non-dryRunPassed bundle to be rejected for approval")
	}
}

// TestPlanBundleActivation_RegistersAllPromotesDeps: the plan registers every
// member construct but only promotes net-new deps (not the headline automation).
func TestPlanBundleActivation_RegistersAllPromotesDeps(t *testing.T) {
	bundle := dryRunPassedBundle("user-a")
	plan, err := memql.PlanBundleActivation(bundle, bundleConstructs("user-a", bundle.Id))
	if err != nil {
		t.Fatalf("PlanBundleActivation: %v", err)
	}
	if len(plan.Register) != 2 {
		t.Errorf("expected 2 registered constructs, got %d", len(plan.Register))
	}
	if len(plan.PromoteToCatalog) != 1 || plan.PromoteToCatalog[0].Name != "isRefundEscalation" {
		t.Errorf("expected only the net-new spec promoted, got %+v", plan.PromoteToCatalog)
	}
	for _, c := range plan.Register {
		if c.OwnerUserId != "user-a" || c.Version != 1 {
			t.Errorf("registered construct not owner/version stamped: %+v", c)
		}
	}
}

// TestPlanBundleActivation_RejectsForeignConstruct: a construct from another
// bundle or owner is rejected (no cross-bundle activation).
func TestPlanBundleActivation_RejectsForeignConstruct(t *testing.T) {
	bundle := dryRunPassedBundle("user-a")
	cs := bundleConstructs("user-a", bundle.Id)
	cs[0].OwnerUserId = "user-b"
	if _, err := memql.PlanBundleActivation(bundle, cs); err == nil {
		t.Error("expected a foreign-owner construct to be rejected")
	}
}

// --- orchestrator end-to-end ---

// TestActivateApprovedBundle_HappyPath: a dryRunPassed bundle activates end to
// end -- status->active, constructs->active + registered + resolvable, net-new
// dep promoted, scheduler hook fired for the automation, activation audited.
func TestActivateApprovedBundle_HappyPath(t *testing.T) {
	owner := "user-a"
	store := newFakeStore()
	bundle := dryRunPassedBundle(owner)
	store.bundles[bundle.Id] = bundle
	store.constructs[bundle.Id] = bundleConstructs(owner, bundle.Id)

	registry := memql.NewAuthoredRuntimeRegistry()
	var scheduled []string
	var promoted []string
	audit := &recordingAuditSink{}

	deps := memql.AuthoredRuntimeDeps{
		Registry: registry,
		RegisterHook: func(c *memql.AuthoredConstruct) error {
			scheduled = append(scheduled, c.Name)
			return nil
		},
		PromoteCatalog: func(_ context.Context, constructID, kind, name, source, fromBundleID string) (memql.CatalogPromotion, error) {
			promoted = append(promoted, name)
			return memql.CatalogPromotion{ConstructID: constructID, Kind: kind, Name: name, FromBundleID: fromBundleID}, nil
		},
		Audit: audit,
	}

	res, err := memql.ActivateApprovedBundleWithStore(context.Background(), store, owner, bundle.Id, deps)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}

	// Bundle + constructs flipped active.
	if len(store.bundleActive) != 1 || store.bundleActive[0] != bundle.Id {
		t.Errorf("bundle not activated: %+v", store.bundleActive)
	}
	if store.constructSet["c-auto"] != "active" || store.constructSet["c-spec"] != "active" {
		t.Errorf("constructs not flipped active: %+v", store.constructSet)
	}

	// Constructs registered + resolvable in the owner-scoped runtime.
	if _, ok := registry.Resolve(owner, "automation", "draftRefundReply"); !ok {
		t.Error("automation not resolvable after activation")
	}
	if _, ok := registry.Resolve(owner, "spec", "isRefundEscalation"); !ok {
		t.Error("spec not resolvable after activation")
	}

	// Only the automation went to the scheduler; only the net-new dep promoted.
	if len(scheduled) != 1 || scheduled[0] != "draftRefundReply" {
		t.Errorf("scheduler hook should fire for the automation only, got %+v", scheduled)
	}
	if len(promoted) != 1 || promoted[0] != "isRefundEscalation" {
		t.Errorf("only the net-new dep should be promoted, got %+v", promoted)
	}
	if len(res.Promoted) != 1 {
		t.Errorf("result should record 1 promotion, got %d", len(res.Promoted))
	}

	// Activation audited.
	if len(audit.events) != 1 || audit.events[0].Action != memql.AuditActionBundleActivated {
		t.Errorf("expected one activation audit event, got %+v", audit.events)
	}
	if audit.events[0].BundleId != bundle.Id || audit.events[0].OwnerUserId != owner {
		t.Errorf("audit event not stamped with bundle/owner: %+v", audit.events[0])
	}
}

// TestActivateApprovedBundle_RejectsForeignOwner: an actor who is not the bundle
// owner cannot activate it (no privilege escalation), and NOTHING is mutated.
func TestActivateApprovedBundle_RejectsForeignOwner(t *testing.T) {
	store := newFakeStore()
	bundle := dryRunPassedBundle("user-a")
	store.bundles[bundle.Id] = bundle
	store.constructs[bundle.Id] = bundleConstructs("user-a", bundle.Id)

	deps := memql.AuthoredRuntimeDeps{Registry: memql.NewAuthoredRuntimeRegistry()}
	if _, err := memql.ActivateApprovedBundleWithStore(context.Background(), store, "user-b", bundle.Id, deps); err == nil {
		t.Fatal("expected a foreign-owner activation to be rejected")
	}
	if len(store.bundleActive) != 0 {
		t.Errorf("no mutation should run on a rejected activation, got %+v", store.bundleActive)
	}
}

// TestActivateApprovedBundle_RejectsNonDryRunPassed: only a dryRunPassed bundle
// can activate.
func TestActivateApprovedBundle_RejectsNonDryRunPassed(t *testing.T) {
	store := newFakeStore()
	bundle := dryRunPassedBundle("user-a")
	bundle.Status = memql.BundleDraft
	store.bundles[bundle.Id] = bundle

	deps := memql.AuthoredRuntimeDeps{Registry: memql.NewAuthoredRuntimeRegistry()}
	if _, err := memql.ActivateApprovedBundleWithStore(context.Background(), store, "user-a", bundle.Id, deps); err == nil {
		t.Error("expected a draft bundle activation to be rejected")
	}
}
