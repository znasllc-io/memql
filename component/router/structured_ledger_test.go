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
	"fmt"
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

// codedRefusal is a refusal with a stable code, the shape the agent's app gate
// (kill switch, a pin the owner did not make, no owner) and the engine's own
// typed refusals share. Its MESSAGE names a person, which is exactly why the
// decision row must carry the code and the router's own words rather than it.
type codedRefusal struct{ code, message string }

func (r *codedRefusal) Error() string { return r.code + ": " + r.message }
func (r *codedRefusal) Code() string  { return r.code }

// killSwitchRefusal is the agent's refusal as the chat door returns it: the
// engine's sentinel in front of the gate's coded refusal.
func killSwitchRefusal() error {
	return fmt.Errorf("%w: %w", memql.ErrAppUnavailable, &codedRefusal{
		code:    "kill_switch_engaged",
		message: "computer use is switched off for alice@example.com (preferences.computerUseEnabled is false)",
	})
}

// failedLine is the app door's `considered` line on a rendered row.
func failedLine(t *testing.T, args map[string]any) string {
	t.Helper()
	entry, ok := consideredArg(args, "app:claude-code")
	if !ok {
		t.Fatalf("the row does not name the app door it tried: %v", args["considered"])
	}
	reason, _ := entry["reason"].(string)
	return reason
}

// THE FAILURE IS ON THE ROW, IN THE FIELD THE DECISIONS LIST READS. A person's
// structured call the route sends to an open app door, whose gate then
// refuses, writes an error row naming the app door. The reason goes on the app
// door's `considered` line -- routerDecision projects `considered` and
// deliberately not the error message -- as the refusal's stable CODE and the
// router's own words, never the refusal's message, which names a person.
// Before this the line said "selected", so Settings > Decisions showed a
// failed call through a signed-in app with no reason at all.
func TestAStructuredCallWhoseAppDoorFailsRecordsWhy(t *testing.T) {
	providers := memql.NewProviderRegistryForTest()
	providers.SetFleetInference(&stubFleetInference{})
	apps := &failingApps{
		stubAppInference: stubAppInference{doors: []memql.AppDoor{openApp("claude-code")}},
		err:              killSwitchRefusal(),
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
	line := failedLine(t, args)
	if !strings.HasPrefix(line, "failed when called: kill_switch_engaged") {
		t.Errorf("the app door's line is %q, want it to say it failed when called, with the refusal's code", line)
	}
	if strings.Contains(line, "alice@example.com") || strings.Contains(line, "preferences.computerUseEnabled") {
		t.Errorf("the app door's line %q carries the refusal's message; a decision row carries the code and the router's words", line)
	}
	if apps.calls != 1 {
		t.Errorf("app calls = %d, want 1", apps.calls)
	}
}

// ledgerRows drains n rendered rows. The ledger writes on its own goroutines,
// so the rows of one walk arrive in no promised order; they are keyed by
// outcome, which is unique within one fallback.
func ledgerRows(t *testing.T, ledger *countingLedger, n int) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for i := 0; i < n; i++ {
		args := ledger.args(t)
		out[fmt.Sprint(args["outcome"])] = args
	}
	return out
}

// A FAILED SOURCE IS NAMED ON EVERY ROW OF THE WALK, the served one included.
// The route's first source is refused when called and the vendor behind it
// serves. The row a person reads first is the SERVED one (Decisions lists
// every outcome), and before this its walk still said the source in front
// was "selected" -- a call that went to a paid vendor with nothing saying
// the source the route preferred had failed.
//
// Scripted `fleet:` sources rather than a real app door: every app or fleet
// call spends the process-wide LLM rate ceiling this package's tests share,
// and the walk is door-agnostic. The app door's own failure is the test
// above; its codes are TestAnUntypedFailureIsNamedByItsCategory's.
func TestASourceThatFailsWhenCalledIsNamedOnEveryRowOfTheWalk(t *testing.T) {
	refused := &codedRefusal{code: memql.RefusalCodeNoLocalModel, message: "laptop (alice's) is asleep"}
	for _, surface := range []struct {
		name     string
		modality airoute.Modality
		call     func(context.Context, any) error
	}{
		{"structured", airoute.ModalityStructured, func(ctx context.Context, client any) error {
			_, err := callStructured(t, ctx, client)
			return err
		}},
		{"chat", airoute.ModalityChat, func(ctx context.Context, client any) error {
			_, err := client.(common.ChatAIProvider).CallChat(ctx, []common.ChatMessage{{Role: "user", Content: "hi"}})
			return err
		}},
	} {
		t.Run(surface.name, func(t *testing.T) {
			first := &scriptedSource{fail: refused}
			vendor := &scriptedSource{answer: `{"ok":true}`}
			r := scriptedRouter(t, []string{"fleet:first", "vendor"},
				map[string]*scriptedSource{"fleet:first": first, "vendor": vendor})
			ledger := &countingLedger{writes: make(chan string, 8)}
			r.engine = ledger
			ctx := auth.ContextWithUserActor(context.Background(), "alice")

			req := structuredRequest()
			req.Modality = surface.modality
			resolved, err := r.ResolveFor(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			if err := surface.call(ctx, resolved.Client); err != nil {
				t.Fatalf("the vendor behind the refused source did not serve: %v", err)
			}
			rows := ledgerRows(t, ledger, 3)
			for _, outcome := range []string{"ok", "error", "fallback_used"} {
				args, ok := rows[outcome]
				if !ok {
					t.Fatalf("no %s row among %v", outcome, rows)
				}
				entry, _ := consideredArg(args, "fleet:first")
				line, _ := entry["reason"].(string)
				if !strings.HasPrefix(line, "failed when called: "+memql.RefusalCodeNoLocalModel) || strings.Contains(line, "alice") {
					t.Errorf("the %s row's line for the failed source is %q, want the failure, its code and none of the refusal's text", outcome, line)
				}
			}
			served, _ := consideredArg(rows["ok"], "vendor")
			if served["reason"] != "selected from fallback chain" {
				t.Errorf("the served row's vendor line = %v, want it selected from the fallback chain", served)
			}
			// The caller's resolution is not the walk's: it still reads as
			// resolved, and a second call on the same client starts clean.
			if line, _ := consideredReason(resolved.Resolution.Decision.Considered, "fleet:first"); line != "selected" {
				t.Errorf("the walk rewrote the caller's resolution: fleet:first = %q", line)
			}
		})
	}
}

// AN UNTYPED FAILURE SAYS ITS CATEGORY, NOT ITS TEXT. A vendor error can quote
// anything the vendor chose to send back; the decision row names the
// category the ledger already derives (rate_limit, timeout, auth, ...).
func TestAnUntypedFailureIsNamedByItsCategory(t *testing.T) {
	if got := FailureReason(errors.New("429 Too Many Requests: slow down, request body was: classify this secret")); !strings.HasPrefix(got, "failed when called: rate_limit") || strings.Contains(got, "secret") {
		t.Errorf("FailureReason(429) = %q, want the rate_limit category and none of the text", got)
	}
	if got := FailureReason(&memql.AppUnavailable{AppId: "claude-code", NoOwner: true}); got != "failed when called: app_no_owner: "+memql.AppNoOwnerReason {
		t.Errorf("FailureReason(no owner) = %q", got)
	}
	if got := FailureReason(fmt.Errorf("%w: claude-code on laptop: worker: session refused its workspace /Users/alice/private", memql.ErrAppUnavailable)); !strings.HasPrefix(got, "failed when called: "+memql.AppRefusalCode) || strings.Contains(got, "/Users/alice") {
		t.Errorf("FailureReason(app session failed) = %q, want the app refusal code and none of the text", got)
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
