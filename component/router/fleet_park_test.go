package router

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// stubFleetInference is a fleet with a fixed catalog.
type stubFleetInference struct {
	models []memql.FleetModel
	calls  int
}

func (s *stubFleetInference) Catalog(context.Context, string) ([]memql.FleetModel, error) {
	return advertisedOnEveryMachine(s.models), nil
}

// advertisedOnEveryMachine gives each fixture machine its model's advertised
// attributes, the catalog Project builds when every machine reports the same
// label: eligibility is decided per online machine.
func advertisedOnEveryMachine(models []memql.FleetModel) []memql.FleetModel {
	out := make([]memql.FleetModel, 0, len(models))
	for _, m := range models {
		out = append(out, memql.FleetModelAdvertisedOnEveryMachineForTest(m))
	}
	return out
}

func (s *stubFleetInference) ModelPreference(context.Context, string) ([]string, error) {
	return nil, nil
}

func (s *stubFleetInference) Call(_ context.Context, req memql.FleetCallRequest) (memql.FleetCallResult, error) {
	s.calls++
	return memql.FleetCallResult{Content: "local answer", ExecutionSurface: "fleet:laptop"}, nil
}

// countingCloud stands in for a paid provider and RECORDS every call, which is
// the whole point: the no-silent-spend property is "this counter stayed at
// zero", and a stub that could not count would prove nothing.
type countingCloud struct{ calls int }

func (c *countingCloud) Call(context.Context, string) (any, error) { return "cloud", nil }
func (c *countingCloud) CallChat(context.Context, []common.ChatMessage) (string, error) {
	c.calls++
	return "cloud answer", nil
}

func fleetModel(id string, online bool) memql.FleetModel {
	return memql.FleetModel{
		ModelId:          id,
		ContextWindow:    131072,
		StructuredOutput: true,
		Machines:         []memql.FleetMachine{{RegistrationId: "laptop", Name: "laptop", Online: online}},
	}
}

// newParkRouter builds a router over a registry with a fleet and, optionally,
// a cloud provider registered under `cloudName`.
func newParkRouter(t *testing.T, models []memql.FleetModel, chain []string, cloudName string) (*Router, *countingCloud, *stubFleetInference) {
	t.Helper()
	cloud := &countingCloud{}
	fleet := &stubFleetInference{models: models}

	providers := memql.NewProviderRegistryForTest()
	providers.SetFleetInference(fleet)
	if cloudName != "" {
		providers.RegisterForTest(cloudName, "AnthropicStream", "claude-sonnet", cloud)
	}
	// `federationStrongest` is the DECLARED chain a one-shot cloud consent
	// resolves through (epic memql#5137, D3). It replaced `providers.Default()`,
	// which -- since every concrete record is a paid vendor model -- meant a
	// consent landed on whichever entry the registry had picked first, a
	// different decision from the one the person thought they were making.
	//
	// Declaring it in the FIXTURE rather than pinning a default is the point:
	// the test exercises the same path production does.
	policyChains := map[string][]string{"testPolicy": chain}
	if cloudName != "" {
		policyChains["federationStrongest"] = []string{cloudName}
	}
	policies := memql.NewPolicyRegistryForTest(policyChains)
	return New(providers, policies, testRules(t, defaultRule("testPolicy")), nil, nil), cloud, fleet
}

// D2, made structural. An unavailable fleet primary with NO authored fallback
// must refuse -- and the refusal must not have been reached by choosing a paid
// provider the policy never named.
func TestAnUnavailableFleetWithNoAuthoredFallbackRefusesRatherThanSpending(t *testing.T) {
	r, cloud, _ := newParkRouter(t,
		[]memql.FleetModel{fleetModel("llama3.1:8b", false)},
		[]string{"fleet:llama3.1:8b"},
		"streamClaudeSonnet", // configured, but NOT in the chain
	)

	_, _, err := r.ResolveChat(ResolveRequest{UserId: "alice"})
	if err == nil {
		t.Fatal("an unavailable fleet primary with no authored fallback must refuse")
	}
	var refusal *InferenceUnavailable
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want the typed every_door_shut refusal -- 'router resolved "+
			"no provider' describes a registry lookup and tells an operator nothing they can act on", err)
	}
	if refusal.Code != work.RefusalEveryDoorShut {
		t.Fatalf("code = %q", refusal.Code)
	}
	if len(refusal.Doors) != 1 || refusal.Doors[0].Name != "fleet:llama3.1:8b" {
		t.Fatalf("the refusal must name the door it tried, got %+v", refusal.Doors)
	}
	if refusal.Doors[0].Door != DoorLocal {
		t.Fatalf("door = %q, want %q", refusal.Doors[0].Door, DoorLocal)
	}
	// THE MACHINE-LEVEL DETAIL SURVIVES the move to a door-shaped report.
	// The door line says which door; this says why it could not open, and it
	// is the half somebody can act on.
	if why := refusal.Doors[0].Considered["laptop"]; why != "offline" {
		t.Fatalf("the refusal must name every machine considered and why; laptop = %q", why)
	}
	if cloud.calls != 0 {
		t.Fatalf("a paid provider was called %d times for a plan whose policy never named one -- "+
			"this is the silent cloud spend D2 exists to prevent", cloud.calls)
	}
}

// An authored fallback is honoured EXACTLY: it runs, with no park and no
// prompt, because the operator wrote it down.
func TestAnAuthoredFallbackRunsWithNoParkAndNoPrompt(t *testing.T) {
	r, cloud, _ := newParkRouter(t,
		[]memql.FleetModel{fleetModel("llama3.1:8b", false)},
		[]string{"fleet:llama3.1:8b", "streamClaudeSonnet"},
		"streamClaudeSonnet",
	)

	client, resolved, err := r.ResolveChat(ResolveRequest{UserId: "alice"})
	if err != nil {
		t.Fatalf("an authored fallback must resolve without a refusal: %v", err)
	}
	if resolved.ProviderName != "streamClaudeSonnet" {
		t.Fatalf("resolved %q, want the authored fallback", resolved.ProviderName)
	}
	if _, err := client.CallChat(context.Background(), []common.ChatMessage{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("CallChat: %v", err)
	}
	if cloud.calls != 1 {
		t.Fatalf("the authored fallback must actually run, got %d calls", cloud.calls)
	}
}

// An ONLINE fleet primary serves the call, and the cloud provider sitting in
// the chain behind it is not touched.
func TestAnOnlineFleetPrimaryServesTheCall(t *testing.T) {
	r, cloud, fleet := newParkRouter(t,
		[]memql.FleetModel{fleetModel("llama3.1:8b", true)},
		[]string{"fleet:llama3.1:8b", "streamClaudeSonnet"},
		"streamClaudeSonnet",
	)

	client, resolved, err := r.ResolveChat(ResolveRequest{UserId: "alice"})
	if err != nil {
		t.Fatalf("ResolveChat: %v", err)
	}
	if !strings.HasPrefix(resolved.ProviderName, "fleet:") {
		t.Fatalf("resolved %q, want the fleet primary", resolved.ProviderName)
	}
	if _, err := client.CallChat(context.Background(), []common.ChatMessage{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("CallChat: %v", err)
	}
	if fleet.calls != 1 {
		t.Fatalf("the local model must serve the call, got %d fleet calls", fleet.calls)
	}
	if cloud.calls != 0 {
		t.Fatalf("the cloud fallback must stay untouched while the primary works, got %d", cloud.calls)
	}
}

// A fleet with NO machines at all is a different problem from a fleet whose
// machines did not match, and the refusal must say which.
func TestARefusalDistinguishesNoMachinesFromNoMatch(t *testing.T) {
	r, _, _ := newParkRouter(t, nil, []string{"fleet:llama3.1:8b"}, "")
	_, _, err := r.ResolveChat(ResolveRequest{UserId: "alice"})
	var refusal *InferenceUnavailable
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want the typed refusal", err)
	}
	if len(refusal.Doors) != 1 {
		t.Fatalf("doors = %+v, want the one the chain named", refusal.Doors)
	}
	// A user with NO machines and a user whose machines did not match are
	// different problems with different fixes, and the report must say which:
	// here there is no machine to name, so the considered list is empty and
	// the door's own reason carries the answer.
	if len(refusal.Doors[0].Considered) != 0 {
		t.Fatalf("considered = %v, want none for a user with no machines", refusal.Doors[0].Considered)
	}
	if !strings.Contains(refusal.Doors[0].Reason, "no machine offers") {
		t.Fatalf("the reason must say the fleet offers nothing, got %q", refusal.Doors[0].Reason)
	}
}

// Approve-cloud is offered only when a cloud provider is actually configured.
// A button that cannot work converts "your machines are asleep" into "you
// clicked the fix and it did not fix it".
func TestApproveCloudIsOnlyOfferedWhenACloudProviderExists(t *testing.T) {
	withCloud := memql.NewProviderRegistryForTest()
	withCloud.RegisterForTest("streamClaudeSonnet", "AnthropicStream", "claude-sonnet", &countingCloud{})
	if !withCloud.HasCloudProviderConfigured() {
		t.Fatal("a registered, available cloud provider must be reported as configured")
	}

	fleetOnly := memql.NewProviderRegistryForTest()
	fleetOnly.SetFleetInference(&stubFleetInference{models: []memql.FleetModel{fleetModel("llama3.1:8b", true)}})
	if fleetOnly.HasCloudProviderConfigured() {
		t.Fatal("a fully-local cluster must not be reported as having a cloud provider -- the park " +
			"card would offer an approve-cloud button that cannot work")
	}
}

// Consent defaults to ABSENT on every context that was not stamped, which is
// the direction that cannot spend money by omission.
func TestCloudConsentDefaultsToAbsent(t *testing.T) {
	if memql.CloudConsentFromContext(context.Background()) {
		t.Fatal("an unstamped context must not carry consent")
	}
	//nolint:staticcheck // deliberately passing a nil context to assert the guard
	if memql.CloudConsentFromContext(nil) {
		t.Fatal("a nil context must not carry consent")
	}
	if !memql.CloudConsentFromContext(memql.ContextWithCloudConsent(context.Background())) {
		t.Fatal("an explicitly stamped context must carry consent")
	}
}

// The refusal reads as unavailable to a caller that only wants to know that.
func TestTheRefusalUnwrapsToTheUnavailableSentinel(t *testing.T) {
	err := error(&memql.FleetUnavailable{ModelId: "llama3.1:8b"})
	if !errors.Is(err, memql.ErrFleetUnavailable) {
		t.Fatal("the typed refusal must read as unavailable without a type assertion")
	}
}

// The rendered payload is what the park card reads, so its machine list must
// be ordered -- the order a person reads should be the order every reader gets.
func TestTheRefusalRendersAStableMachineList(t *testing.T) {
	refusal := &memql.FleetUnavailable{
		ModelId:    "llama3.1:8b",
		Total:      3,
		Considered: map[string]string{"zeta": "offline", "alpha": "does not offer the model", "mid": "busy"},
	}
	first := refusal.AsMap()["machinesRuledOut"].([]map[string]any)
	second := refusal.AsMap()["machinesRuledOut"].([]map[string]any)
	if len(first) != 3 {
		t.Fatalf("machines = %d, want 3", len(first))
	}
	for i := range first {
		if first[i]["machine"] != second[i]["machine"] {
			t.Fatal("the rendered machine list must be stable across calls")
		}
	}
	if first[0]["machine"] != "alpha" {
		t.Fatalf("first machine = %v, want a sorted list", first[0]["machine"])
	}
}

// Consent is a person's decision, and it is the ONLY thing that lets a call
// reach a paid provider the policy never named.
func TestExplicitConsentIsTheOnlyWayPastAnUnavailableFleet(t *testing.T) {
	models := []memql.FleetModel{fleetModel("llama3.1:8b", false)}

	// Without consent: refused, and nothing paid is touched.
	r, cloud, _ := newParkRouter(t, models, []string{"fleet:llama3.1:8b"}, "streamClaudeSonnet")
	if _, _, err := r.ResolveChat(ResolveRequest{UserId: "alice"}); err == nil {
		t.Fatal("without consent this must refuse")
	}
	if cloud.calls != 0 {
		t.Fatalf("a paid provider ran %d times with no consent and no authored fallback", cloud.calls)
	}

	// With consent: the federationStrongest chain serves it, once.
	r2, cloud2, _ := newParkRouter(t, models, []string{"fleet:llama3.1:8b"}, "streamClaudeSonnet")
	client, resolved, err := r2.ResolveChat(ResolveRequest{UserId: "alice", CloudConsent: true})
	if err != nil {
		t.Fatalf("consent must be honoured: %v", err)
	}
	if resolved.ProviderName != "streamClaudeSonnet" {
		t.Fatalf("resolved %q, want the cluster default", resolved.ProviderName)
	}
	if _, err := client.CallChat(context.Background(), []common.ChatMessage{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("CallChat: %v", err)
	}
	if cloud2.calls != 1 {
		t.Fatalf("cloud calls = %d, want exactly one", cloud2.calls)
	}
}

// Consent given on a cluster with nothing to spend it on keeps the refusal --
// and says so, which is more useful than a generic "no provider": the person
// said yes to something the cluster does not have.
func TestConsentOnAFullyLocalClusterStillRefusesAndExplains(t *testing.T) {
	r, _, _ := newParkRouter(t, []memql.FleetModel{fleetModel("llama3.1:8b", false)},
		[]string{"fleet:llama3.1:8b"}, "")
	_, _, err := r.ResolveChat(ResolveRequest{UserId: "alice", CloudConsent: true})
	var refusal *InferenceUnavailable
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want the refusal to stand", err)
	}
	found := false
	for _, d := range refusal.Doors {
		if strings.Contains(d.Reason, "none configured") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the refusal must say why the consent could not be used, got %v", refusal.Doors)
	}
}

// TestConsentSurvivesWithNoPolicyCorpus is memql-2a's objection, made a test
// (epic memql#5137, D3).
//
// CONSENT EXISTS FOR THE CASE WHERE THE CHAIN ALREADY REFUSED. Routing it
// through the `federationStrongest` POLICY is the reviewable answer and is what
// runs on a healthy cluster -- but it made the one-shot human escape depend on
// the rule and policy corpus being loaded. On a cluster whose corpus failed to
// load, the person says yes and nothing happens, which is worse than the
// registry default it replaced.
//
// So the chain falls back to a direct strongest-federated pick. That is not a
// default: nothing reaches it without an explicit yes on the call in front of
// the person, which is the whole difference between it and what was deleted.
func TestConsentSurvivesWithNoPolicyCorpus(t *testing.T) {
	models := []memql.FleetModel{fleetModel("llama3.1:8b", false)}
	cloud := &countingCloud{}
	fleet := &stubFleetInference{models: models}

	providers := memql.NewProviderRegistryForTest()
	providers.SetFleetInference(fleet)
	providers.RegisterForTest("streamClaudeSonnet", "AnthropicStream", "claude-sonnet", cloud)

	// The policy registry holds a local-only chain and NOTHING ELSE -- no
	// federationStrongest, which is the state of a cluster whose corpus did not
	// load. The rule corpus is one rule naming it, so the request resolves the
	// way every request does and the missing thing is the FALLBACK policy
	// rather than the routing.
	policies := memql.NewPolicyRegistryForTest(map[string][]string{"testPolicy": {"fleet:llama3.1:8b"}})
	r := New(providers, policies, testRules(t, defaultRule("testPolicy")), nil, nil)

	req := func(consent bool) ResolveRequest {
		return ResolveRequest{Level: airoute.LevelStrong, UserId: "alice", CloudConsent: consent}
	}

	// Without consent it still refuses: the fallback is reached by consent, not
	// by the absence of a policy.
	if _, _, err := r.ResolveChat(req(false)); err == nil {
		t.Fatal("with no consent this must still refuse")
	}
	if cloud.calls != 0 {
		t.Fatalf("a paid provider ran %d times with no consent", cloud.calls)
	}

	// With consent it resolves, corpus or no corpus.
	_, resolved, err := r.ResolveChat(req(true))
	if err != nil {
		t.Fatalf("an explicit consent must be honoured even with no policy corpus loaded: %v", err)
	}
	if resolved.ProviderName != "streamClaudeSonnet" {
		t.Errorf("consent resolved to %q, want the only federated record available", resolved.ProviderName)
	}
}
