package router

// structured_ledger_test.go -- what a STRUCTURED call's decision row says
// about an app door, which is what Settings > Decisions and Fleet History read.
//
// The structured surface -- the classifiers, goal triage, the answer
// validator, compose -- wrote no v1:router:call row before it walked the route
// (fallback_structured.go). Live on 2026-09-28 the cluster made 18 model calls
// in an hour and wrote 6 decision rows, all agent replies: an app door a
// structured call skipped, and an app session a structured call opened and
// lost, were each invisible there. These pin the row's CONTENT for the app
// door; structured_fallback_test.go pins the walk.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

// failingApps is an open app door whose session fails -- the shape of every
// app-door failure the live cluster produced (a gate refusal, a workspace the
// machine refused, a session nobody held).
type failingApps struct {
	stubAppInference
	err error
}

func (f *failingApps) Call(_ context.Context, _ memql.AppCallRequest) (memql.AppCallResult, error) {
	f.calls++
	return memql.AppCallResult{}, f.err
}

func structuredTriageRequest() ResolveRequest {
	return ResolveRequest{
		Level: airoute.LevelFast, Modality: airoute.ModalityStructured, PromptName: "goalComplexityTriage",
		Needs: airoute.Needs{MinContextTokens: 8000, Structured: true},
	}
}

func callStructured(t *testing.T, ctx context.Context, client any) (string, error) {
	t.Helper()
	p, ok := client.(common.ChatStructuredProvider)
	if !ok {
		t.Fatalf("the structured client %T does not serve structured calls", client)
	}
	return p.CallChatStructured(ctx, []common.ChatMessage{{Role: "user", Content: "classify"}},
		common.StructuredSchema{Name: "triage"})
}

func consideredArg(args map[string]any, entry string) (map[string]any, bool) {
	list, _ := args["considered"].([]any)
	for _, raw := range list {
		m, _ := raw.(map[string]any)
		if m["entry"] == entry {
			return m, true
		}
	}
	return nil, false
}

// THE SKIP IS ON THE ROW. An owner-less structured call passes the app door
// and is served by the vendor behind it; the row names the vendor as the
// server and keeps the app door in `considered` with its reason.
func TestAStructuredCallRecordsTheAppDoorItPassedAndWhy(t *testing.T) {
	r, _, _, _ := appFirstRouter(t)
	ledger := &countingLedger{writes: make(chan string, 4)}
	r.engine = ledger
	ctx := sweepContext()

	resolved, err := r.ResolveFor(ctx, structuredTriageRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := callStructured(t, ctx, resolved.Client); err != nil {
		t.Fatal(err)
	}

	args := ledger.args(t)
	if args["outcome"] != "ok" || args["providerName"] != "streamClaudeSonnet" || args["door"] != DoorFederation {
		t.Errorf("row = outcome %v provider %v door %v, want ok / streamClaudeSonnet / federation",
			args["outcome"], args["providerName"], args["door"])
	}
	if args["promptName"] != "goalComplexityTriage" || args["streaming"] != false {
		t.Errorf("row promptName=%v streaming=%v, want the prompt named and not streaming", args["promptName"], args["streaming"])
	}
	passed, ok := consideredArg(args, "app:claude-code")
	if !ok {
		t.Fatalf("the row does not keep the app door it passed: %v", args["considered"])
	}
	if reason, _ := passed["reason"].(string); !strings.Contains(reason, memql.AppNoOwnerReason) {
		t.Errorf("the app door's reason on the row is %q, want %q", reason, memql.AppNoOwnerReason)
	}
	if args["callerKind"] != auth.CallerKindSystem {
		t.Errorf("callerKind = %v, want %q", args["callerKind"], auth.CallerKindSystem)
	}
}

// THE FAILURE IS ON THE ROW. A person's structured call the route sends to an
// open app door, whose session then fails, writes an error row naming the app
// door and carrying the failure -- the reason a person reads in Fleet History.
func TestAStructuredCallWhoseAppDoorFailsRecordsWhy(t *testing.T) {
	providers := memql.NewProviderRegistryForTest()
	providers.SetFleetInference(&stubFleetInference{})
	apps := &failingApps{
		stubAppInference: stubAppInference{doors: []memql.AppDoor{openApp("claude-code")}},
		err:              errors.New("no machine can run this app right now: app_no_owner: the session was refused on the machine"),
	}
	providers.SetAppInference(apps)
	policies := memql.NewPolicyRegistryForTest(map[string][]string{
		"appFirst": {memql.AppReferencePrefix + "claude-code"},
	})
	r := New(providers, policies, testRules(t, defaultRule("appFirst")), nil, nil)
	ledger := &countingLedger{writes: make(chan string, 4)}
	r.engine = ledger
	ctx := auth.ContextWithUserActor(context.Background(), "alice")

	resolved, err := r.ResolveFor(ctx, structuredTriageRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := callStructured(t, ctx, resolved.Client); err == nil {
		t.Fatal("the failing app session's error was swallowed")
	}

	args := ledger.args(t)
	if args["outcome"] != "error" || args["providerName"] != "app:claude-code" || args["door"] != DoorApp {
		t.Errorf("row = outcome %v provider %v door %v, want error / app:claude-code / app",
			args["outcome"], args["providerName"], args["door"])
	}
	if msg, _ := args["errorMessage"].(string); !strings.Contains(msg, "the session was refused on the machine") {
		t.Errorf("errorMessage = %q, want the app door's own failure", msg)
	}
	if apps.calls != 1 {
		t.Errorf("app calls = %d, want 1", apps.calls)
	}
}

// usageCloud reports what it measured, the way the fleet and Anthropic
// structured providers do.
type usageCloud struct{ everyCloud }

func (u *usageCloud) CallChatStructuredWithUsage(context.Context, []common.ChatMessage, common.StructuredSchema) (string, common.ChatUsage, error) {
	u.calls++
	return `{"ok":true}`, common.ChatUsage{InputTokens: 1234, OutputTokens: 56, Model: "claude-sonnet-served", Reported: true}, nil
}

// THE OBSERVER HIDES NOTHING THE CALLER READ BEFORE IT. The engine's
// structured path asks the provider for its measured usage (the work spine's
// modelCall row and the run's spend read it); a wrapper that answered only
// CallChatStructured would silently turn every measured call into an
// unmeasured one. And the row carries the measured counts, not an estimate.
func TestTheStructuredObserverKeepsTheProvidersMeasuredUsage(t *testing.T) {
	providers := memql.NewProviderRegistryForTest()
	cloud := &usageCloud{}
	providers.RegisterWithParamsForTest("streamClaudeSonnet", "AnthropicStream", "claude-sonnet",
		map[string]any{"contextWindow": 200000}, cloud)
	policies := memql.NewPolicyRegistryForTest(map[string][]string{"vendor": {"streamClaudeSonnet"}})
	r := New(providers, policies, testRules(t, defaultRule("vendor")), nil, nil)
	ledger := &countingLedger{writes: make(chan string, 4)}
	r.engine = ledger
	ctx := auth.ContextWithUserActor(context.Background(), "alice")

	resolved, err := r.ResolveFor(ctx, structuredTriageRequest())
	if err != nil {
		t.Fatal(err)
	}
	withUsage, ok := resolved.Client.(common.ChatStructuredUsageProvider)
	if !ok {
		t.Fatalf("the structured client %T hides the provider's measured usage", resolved.Client)
	}
	_, usage, err := withUsage.CallChatStructuredWithUsage(ctx, nil, common.StructuredSchema{Name: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if !usage.Reported || usage.InputTokens != 1234 || usage.OutputTokens != 56 {
		t.Errorf("usage = %+v, want the provider's own measurement", usage)
	}
	args := ledger.args(t)
	if args["inputTokens"] != int64(1234) && args["inputTokens"] != float64(1234) && args["inputTokens"] != 1234 {
		t.Errorf("row inputTokens = %v (%T), want the measured 1234", args["inputTokens"], args["inputTokens"])
	}
	if args["tokensEstimated"] != false {
		t.Errorf("row tokensEstimated = %v, want false for a measured call", args["tokensEstimated"])
	}
}

// A CALL THAT NAMES NO FOOTPRINT STILL LANDS. `touches` is an array on the
// concept, and a request with no footprint -- most engine prompt calls, every
// structured one -- rendered it as `null`, which the concept's validation
// refuses: the whole row was logged once and dropped. The agent reply always
// carries a footprint, which is why no live row had been lost yet; the
// structured observer is the first writer that commonly has none.
func TestALedgerRowWithNoFootprintRendersAnEmptyList(t *testing.T) {
	// A vendor-only route: the row's shape is the point, and a local call
	// would spend the process-wide local rate ceiling every test here shares.
	providers := memql.NewProviderRegistryForTest()
	providers.RegisterWithParamsForTest("streamClaudeSonnet", "AnthropicStream", "claude-sonnet",
		map[string]any{"contextWindow": 200000}, &everyCloud{})
	r := New(providers, memql.NewPolicyRegistryForTest(map[string][]string{"vendor": {"streamClaudeSonnet"}}),
		testRules(t, defaultRule("vendor")), nil, nil)
	ledger := &countingLedger{writes: make(chan string, 4)}
	r.engine = ledger
	ctx := auth.ContextWithUserActor(context.Background(), "alice")

	resolved, err := r.ResolveFor(ctx, structuredTriageRequest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := callStructured(t, ctx, resolved.Client); err != nil {
		t.Fatal(err)
	}
	args := ledger.args(t)
	touches, ok := args["touches"].([]any)
	if !ok || touches == nil {
		t.Fatalf("touches = %#v, want an empty list: null is refused by the concept and drops the row", args["touches"])
	}
}
