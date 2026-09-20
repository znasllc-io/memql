package router

// The three-step default chain, door by door (epic memql#5096, task
// memql#5101, design D4).
//
// Each door open ALONE, every door shut, and the federation hop refused by the
// ceiling. The table is the point: a chain is a sequence of decisions and the
// interesting failures are the ones where the wrong step is taken, which only
// shows up when each step is exercised on its own.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// stubAppInference is an app door with a fixed set.
type stubAppInference struct {
	doors []memql.AppDoor
	calls int
}

func (s *stubAppInference) Doors(context.Context, string) ([]memql.AppDoor, error) {
	return s.doors, nil
}

func (s *stubAppInference) AppOrder(context.Context, string) ([]string, error) { return nil, nil }

func (s *stubAppInference) Call(_ context.Context, req memql.AppCallRequest) (memql.AppCallResult, error) {
	s.calls++
	return memql.AppCallResult{Content: "app answer", ExecutionSurface: "app:" + req.AppId + "@laptop"}, nil
}

func openApp(appId string) memql.AppDoor {
	return memql.AppDoor{AppId: appId, Machines: []memql.AppMachine{{
		RegistrationId: "laptop", Name: "laptop", Online: true, LocalStream: true,
		StructuredResult: true, FollowUps: true,
	}}}
}

func shutApp(appId string) memql.AppDoor {
	d := openApp(appId)
	d.Machines[0].Online = false
	return d
}

// threeStepRouter builds the router over the shipped default shape:
// fleet:strongest -> app:* -> a vendor entry, reached through the one rule
// every call falls to.
func threeStepRouter(t *testing.T, models []memql.FleetModel, doors []memql.AppDoor, withCloud bool) (*Router, *countingCloud, *stubAppInference) {
	t.Helper()
	providers := memql.NewProviderRegistryForTest()
	providers.SetFleetInference(&stubFleetInference{models: models})
	apps := &stubAppInference{doors: doors}
	providers.SetAppInference(apps)
	cloud := &countingCloud{}
	chain := []string{memql.FleetStrongest, memql.AppWildcard}
	if withCloud {
		providers.RegisterForTest("streamClaudeSonnet", "AnthropicStream", "claude-sonnet", cloud)
		chain = append(chain, "streamClaudeSonnet")
	}
	policies := memql.NewPolicyRegistryForTest(map[string][]string{"defaultChain": chain})
	return New(providers, policies, testRules(t, defaultRule("defaultChain")), nil, nil), cloud, apps
}

func TestTheLocalDoorIsTriedFirst(t *testing.T) {
	r, cloud, apps := threeStepRouter(t,
		[]memql.FleetModel{fleetModel("llama3.1:8b", true)},
		[]memql.AppDoor{openApp("claude-code")},
		true,
	)
	_, resolved, err := r.ResolveChat(ResolveRequest{UserId: "alice"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// The DECISION NAMES THE MODEL, not the selector. `fleet:strongest` is a
	// question; `fleet:llama3.1:8b` is the answer, and the answer is what a
	// ledger row has to carry to be worth reading.
	if resolved.ProviderName != "fleet:llama3.1:8b" {
		t.Fatalf("resolved %q, want the local door first, named by model", resolved.ProviderName)
	}
	if resolved.Decision.Door != DoorLocal {
		t.Fatalf("decision door = %q, want %q", resolved.Decision.Door, DoorLocal)
	}
	if apps.calls != 0 || cloud.calls != 0 {
		t.Fatalf("nothing past the local door may be touched while it is open (app=%d cloud=%d)",
			apps.calls, cloud.calls)
	}
}

func TestTheAppDoorIsTriedWhenNoLocalModelIsAvailable(t *testing.T) {
	r, cloud, _ := threeStepRouter(t,
		[]memql.FleetModel{fleetModel("llama3.1:8b", false)}, // offline
		[]memql.AppDoor{openApp("claude-code")},
		true,
	)
	client, resolved, err := r.ResolveChat(ResolveRequest{UserId: "alice"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.ProviderName != memql.AppWildcard {
		t.Fatalf("resolved %q, want the app door", resolved.ProviderName)
	}
	if _, err := client.CallChat(context.Background(), []common.ChatMessage{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("CallChat: %v", err)
	}
	if cloud.calls != 0 {
		t.Fatalf("the federation hop must stay untouched while an app door is open, got %d", cloud.calls)
	}
}

func TestFederationIsTriedWhenBothLocalDoorsAreShut(t *testing.T) {
	r, cloud, apps := threeStepRouter(t,
		[]memql.FleetModel{fleetModel("llama3.1:8b", false)},
		[]memql.AppDoor{shutApp("claude-code")},
		true,
	)
	client, resolved, err := r.ResolveChat(ResolveRequest{UserId: "alice"})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if resolved.ProviderName != "streamClaudeSonnet" {
		t.Fatalf("resolved %q, want the vendor entry", resolved.ProviderName)
	}
	if _, err := client.CallChat(context.Background(), []common.ChatMessage{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("CallChat: %v", err)
	}
	if cloud.calls != 1 {
		t.Fatalf("cloud calls = %d, want exactly one", cloud.calls)
	}
	if apps.calls != 0 {
		t.Fatalf("a shut app door must not be dispatched to, got %d", apps.calls)
	}
}

// THE PARK CASE. Work parks only when EVERY door is shut, and the refusal
// names each one so a person knows which of the four fixes applies.
func TestWorkParksOnlyWhenEveryDoorIsShut(t *testing.T) {
	r, cloud, _ := threeStepRouter(t,
		[]memql.FleetModel{fleetModel("llama3.1:8b", false)},
		[]memql.AppDoor{shutApp("claude-code")},
		false, // no vendor entry in the chain at all
	)
	_, _, err := r.ResolveChat(ResolveRequest{UserId: "alice"})
	if err == nil {
		t.Fatal("every door shut must refuse")
	}
	var refusal *InferenceUnavailable
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want the typed refusal", err)
	}
	if refusal.Code != work.RefusalEveryDoorShut {
		t.Fatalf("code = %q, want %q", refusal.Code, work.RefusalEveryDoorShut)
	}
	if got := refusal.DoorsShut(); len(got) != 2 || got[0] != DoorApp || got[1] != DoorLocal {
		t.Fatalf("doorsShut = %v, want both local doors named", got)
	}
	// EVERY door carries a reason. A door with none reads as a bug in the
	// report, and a report nobody trusts is a report nobody reads.
	for _, d := range refusal.Doors {
		if strings.TrimSpace(d.Reason) == "" {
			t.Errorf("door %q has no reason", d.Name)
		}
	}
	if cloud.calls != 0 {
		t.Fatalf("nothing may be spent while parking, got %d calls", cloud.calls)
	}

	// THE ERROR MESSAGE LEADS WITH THE CODE. That is a contract, not a
	// style: work.InferenceRefusalCode matches on this string to decide
	// whether a failed run parks or fails, across a module boundary where
	// the typed error is not available.
	code, ok := work.InferenceRefusalCode(err.Error())
	if !ok || code != work.RefusalEveryDoorShut {
		t.Fatalf("InferenceRefusalCode(%q) = (%q, %v), want the code the run parks on",
			err.Error(), code, ok)
	}
}

// The ceiling gates the FEDERATION HOP, and only the hop: a chain that starts
// at a vendor is a decision an operator wrote down, and refusing it here would
// break every cloud-quality policy in the tree.
func TestTheCeilingGatesTheFederationHopAndNotADirectVendorChain(t *testing.T) {
	// A guard whose cumulative call ceiling is already reached.
	reached := func(context.Context) (string, bool) {
		return "cumulative LLM call ceiling reached: 100 calls admitted this process", true
	}

	providers := memql.NewProviderRegistryForTest()
	providers.SetFleetInference(&stubFleetInference{models: []memql.FleetModel{fleetModel("llama3.1:8b", false)}})
	cloud := &countingCloud{}
	providers.RegisterForTest("streamClaudeSonnet", "AnthropicStream", "claude-sonnet", cloud)
	policies := memql.NewPolicyRegistryForTest(map[string][]string{
		"threeStep": {memql.FleetStrongest, "streamClaudeSonnet"},
		"direct":    {"streamClaudeSonnet"},
	})
	// Two chains, chosen by two RULES rather than by a caller naming a policy.
	rules := testRules(t,
		defaultRule("threeStep"),
		&memql.RuleConfig{Name: "directChain", When: when("tag", "direct"), Policy: "direct", Precedence: 10, Locked: true},
	)
	r := New(providers, policies, rules, nil, nil)
	r.ceilingCheck = reached

	// The HOP: local first, then a vendor. Refused at the ceiling.
	_, _, err := r.ResolveChat(ResolveRequest{UserId: "alice"})
	var refusal *InferenceUnavailable
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want a ceiling refusal", err)
	}
	if refusal.Code != work.RefusalCeilingReached {
		t.Fatalf("code = %q, want %q", refusal.Code, work.RefusalCeilingReached)
	}
	if !strings.Contains(refusal.CeilingReason, "MEMQL_LLM_MAX_TOTAL_CALLS") &&
		!strings.Contains(refusal.CeilingReason, "ceiling reached") {
		t.Errorf("the refusal must carry the guard's own sentence, which names what to change: %q",
			refusal.CeilingReason)
	}
	if cloud.calls != 0 {
		t.Fatalf("the ceiling must refuse BEFORE the hop, got %d calls", cloud.calls)
	}

	// The DIRECT chain: a vendor with no local door before it. Unaffected.
	_, resolved, err := r.ResolveChat(ResolveRequest{UserId: "alice", Tags: []string{"direct"}})
	if err != nil {
		t.Fatalf("a chain that starts at a vendor is an operator's decision and must resolve: %v", err)
	}
	if resolved.ProviderName != "streamClaudeSonnet" {
		t.Fatalf("resolved %q", resolved.ProviderName)
	}
}

// A ceiling refusal and a park are DIFFERENT ANSWERS. One is a condition the
// world changes; the other only a person changes. The code is what tells them
// apart downstream, and it must not collapse.
func TestACeilingRefusalIsNotAPark(t *testing.T) {
	if work.RefusalCeilingReached == work.RefusalEveryDoorShut {
		t.Fatal("the two codes must stay distinct: one is re-checked on a timer, the other is not")
	}
	ceiling := &InferenceUnavailable{Code: work.RefusalCeilingReached, CeilingReason: "cap hit"}
	shut := &InferenceUnavailable{Code: work.RefusalEveryDoorShut}
	if !strings.HasPrefix(ceiling.Error(), work.RefusalCeilingReached) {
		t.Errorf("ceiling error = %q, must lead with its code", ceiling.Error())
	}
	if !strings.HasPrefix(shut.Error(), work.RefusalEveryDoorShut) {
		t.Errorf("shut error = %q, must lead with its code", shut.Error())
	}
}

// A tool turn NO LONGER walks past an open app door (epic memql#5391, design
// D7), and this test is the old one rewritten rather than deleted, because the
// premise it used to assert is the thing that changed.
//
// It used to read: "a tool turn walks past every app door with no special case,
// because the app provider does not implement the tool surfaces (design D3)".
// That was the right answer to the wrong question. The app still cannot serve
// MemQL's TURN -- two agents driving one conversation -- but it can be handed
// the whole STEP, and on a cluster whose only open door is a signed-in Claude
// Code, walking past meant Ask stayed dark.
//
// So what happens at an open app door on a tool turn now depends on ONE thing:
// whether the call carries a step to hand over.
func TestAToolTurnTakesTheSessionDoorOrRefusesForWantOfAStep(t *testing.T) {
	// No step: refused AT RESOLUTION, naming the modality, having opened
	// nothing. A bare Go model call with tools has nothing to delegate, and
	// that is a fact about the call site -- which is why it is not reported as
	// a shut door.
	r, _, apps := threeStepRouter(t, nil, []memql.AppDoor{openApp("claude-code")}, false)
	_, _, err := r.ResolveWithTools(ResolveRequest{UserId: "alice"})
	if err == nil {
		t.Fatalf("a stepless tool turn at an open app door must refuse")
	}
	var refusal *InferenceUnavailable
	if errors.As(err, &refusal) {
		t.Fatalf("the stepless refusal is a CALL-SITE fault, not a shut-door report: %v", err)
	}
	if !strings.Contains(err.Error(), "no work step to hand over") {
		t.Fatalf("the refusal must say what is missing, got %q", err)
	}
	if apps.calls != 0 {
		t.Fatalf("no app session may be opened for a stepless tool turn, got %d", apps.calls)
	}

	// With a step: the door is TAKEN, and the decision says `session` rather
	// than `app` -- the two are different answers about what the app did.
	_, resolved, err := r.ResolveWithTools(ResolveRequest{
		UserId: "alice",
		RunId:  "v1:work:run:r1",
		StepId: "v1:work:step:s1",
	})
	if err != nil {
		t.Fatalf("a tool turn with a step must take the app door as a session: %v", err)
	}
	if resolved.Decision.Door != DoorSession {
		t.Fatalf("door = %q, want %q", resolved.Decision.Door, DoorSession)
	}
	if apps.calls != 0 {
		t.Fatalf("resolution alone opens no session, got %d", apps.calls)
	}
}

func TestDoorForClassifiesEveryReferenceShape(t *testing.T) {
	for _, tt := range []struct {
		name string
		want string
	}{
		{memql.FleetWildcard, DoorLocal},
		{memql.FleetStrongest, DoorLocal},
		{memql.FleetFastest, DoorLocal},
		{"fleet:llama3.1:8b", DoorLocal},
		{memql.AppWildcard, DoorApp},
		{"app:claude-code", DoorApp},
		{"streamClaudeSonnet", DoorFederation},
		{"chat54Mini", DoorFederation},
		{"federation:cheapest", DoorFederation},
		{"federation:streamClaudeSonnet", DoorFederation},
		// Deliberately over-approximating: a raw API key reaches a vendor
		// through the same entry and spends money the same way, so the
		// ceiling must gate it too.
		{"", DoorFederation},
	} {
		if got := doorFor(tt.name); got != tt.want {
			t.Errorf("doorFor(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}
