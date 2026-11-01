//go:build agent

package worker

// A FAILED CALL NAMES ITS MACHINE (memql#6414, point 4).
//
// The degraded check folds v1:router:call rows by executionSurface, and a row
// got one only when the machine answered. Over 14 days on a real cluster 0 of
// 175 failed fleet rows carried one, so a machine failing every call it was
// given stayed first in the order. These cases run the whole path in process:
// the real FleetInference placing the call, the real fleet provider, the
// router's observer writing the row, and FoldFleetHealth reading that row back
// for admission.
//
// TO CONFIRM THEY ARE LOAD-BEARING: make FleetInference.Call return an empty
// result beside the error again, and the first test stops at its first row. Make
// rowCreatedAt read only a string again, and it stops at the call after the four
// failures, with every row naming its machine: no row was recent enough to count.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/router"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

const (
	failedCallOwner = "v1:identity:user:alice"
	failedCallNode  = "agent-test"
	steadyAnswer    = "served by the steady machine"
	flakySurface    = FleetSurfacePrefix + "flaky"
	steadySurface   = FleetSurfacePrefix + "steady"
)

// fleetLedger stands where the engine stands under the router. It keeps the
// rows the router wrote, with the createdAt a stored row comes back with, a
// time.Time, so admission reads the evidence the router actually produced.
type fleetLedger struct {
	writes chan string
	mu     sync.Mutex
	rows   []map[string]any
}

func (l *fleetLedger) Execute(_ context.Context, query string) (*memqlengine.ExecuteResult, error) {
	l.writes <- query
	return &memqlengine.ExecuteResult{}, nil
}

// next waits for the router's next row and files it, newest first, as
// routerCallsForSignals returns them.
func (l *fleetLedger) next(t *testing.T) map[string]any {
	t.Helper()
	select {
	case query := <-l.writes:
		expr, err := langparser.ParseExpression(query)
		if err != nil {
			t.Fatal(err)
		}
		call, ok := expr.(*langparser.FunctionCallExpr)
		if !ok || call.Name != "recordRouterCall" {
			t.Fatalf("unexpected ledger invocation: %s", query)
		}
		row := call.Args
		row["createdAt"] = fleetNow().UTC()
		l.mu.Lock()
		l.rows = append([]map[string]any{row}, l.rows...)
		l.mu.Unlock()
		return row
	case <-time.After(3 * time.Second):
		t.Fatal("the router never wrote a row for the call")
		return nil
	}
}

func (l *fleetLedger) evidence() []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]map[string]any(nil), l.rows...)
}

// ledgerFleetStore answers admission's health question from those rows.
type ledgerFleetStore struct {
	*fakeStore
	ledger *fleetLedger
}

func (s *ledgerFleetStore) ModelMemoryNeeds(context.Context) (map[string]uint64, error) {
	return nil, nil
}

func (s *ledgerFleetStore) MachineHealth(_ context.Context, now time.Time) (memqlengine.FleetHealthIndex, error) {
	return memqlengine.FoldFleetHealth(s.ledger.evidence(), now), nil
}

// failingFleet is two machines on this replica holding one model: flaky, first
// in the order, fails every call, and steady answers.
type failingFleet struct {
	f                       *FleetInference
	rtr                     *router.Router
	ledger                  *fleetLedger
	flakyCalls, steadyCalls *atomic.Int32
	asked                   atomic.Int32
}

func newFailingFleet(t *testing.T) *failingFleet {
	t.Helper()
	ledger := &fleetLedger{writes: make(chan string, 16)}
	attrs := map[string]ModelAttributes{hopModel: {ContextWindow: 131072}}
	own := func(c *Candidate) {
		c.OwnerUserId = failedCallOwner
		c.ConnectedNodeId = failedCallNode
	}
	flakyMachine, steadyMachine := modelMachine("flaky", attrs, own), modelMachine("steady", attrs, own)
	store := &ledgerFleetStore{ledger: ledger, fakeStore: &fakeStore{fakeFleet: &fakeFleet{
		machines: []Candidate{flakyMachine, steadyMachine},
	}}}
	f := newFleetInference(t, store)
	f.selfNodeId = failedCallNode

	h := &failingFleet{f: f, ledger: ledger, flakyCalls: &atomic.Int32{}, steadyCalls: &atomic.Int32{}}
	h.add(flakyMachine, h.flakyCalls, runtimeFailure)
	h.add(steadyMachine, h.steadyCalls, workerservice.ModelCallOutcome{Content: steadyAnswer, FinishReason: workerservice.ModelFinishStop})

	providers := memqlengine.NewProviderRegistryForTest()
	providers.SetFleetInference(f)
	rules := memqlengine.NewRuleRegistry()
	if err := rules.Register(&memqlengine.RuleConfig{
		Name: memqlengine.DefaultRuleName, When: memqlengine.RuleWhen{Present: map[string]bool{}},
		Policy: "localOnly", Locked: true, OnUnavailable: memqlengine.OnUnavailableDegrade,
		SourceFile: "dsl/rules/rules.memql",
	}); err != nil {
		t.Fatal(err)
	}
	if err := rules.Finalize(); err != nil {
		t.Fatal(err)
	}
	policies := memqlengine.NewPolicyRegistryForTest(map[string][]string{"localOnly": {memqlengine.FleetReferencePrefix + hopModel}})
	h.rtr = router.New(providers, policies, rules, ledger, nil)
	return h
}

func (h *failingFleet) add(c Candidate, calls *atomic.Int32, out workerservice.ModelCallOutcome) {
	w := &workerservice.Worker{
		RegistrationId: c.RegistrationId, OwnerUserId: c.OwnerUserId, Name: c.Name,
		Capabilities: c.Capabilities, Labels: c.Labels,
		Concurrency: map[string]uint32{workerservice.ModelCapability: 2},
	}
	w.SetModelCallFunc(ending(calls, out))
	h.f.registry.Add(w)
}

// ending is a machine that ends every call with one outcome.
func ending(calls *atomic.Int32, out workerservice.ModelCallOutcome) workerservice.ModelCallFunc {
	return func(_ context.Context, req workerservice.ModelCallRequest) (*workerservice.ModelCallHandle, error) {
		calls.Add(1)
		handle, _, finish := workerservice.NewModelCallLoopback(req, func(string) {})
		go finish(out)
		return handle, nil
	}
}

// forgetHealth drops the 30-second copy of the evidence, so the next plan reads
// the rows written so far rather than waiting that long on the test's clock.
func (h *failingFleet) forgetHealth() {
	h.f.capacity.mu.Lock()
	defer h.f.capacity.mu.Unlock()
	h.f.capacity.healthAt = time.Time{}
}

// prompt never repeats. The process-wide breaker trips on an identical request,
// and refuses it before any machine is reached.
func (h *failingFleet) prompt(t *testing.T) []common.ChatMessage {
	t.Helper()
	return []common.ChatMessage{{Role: "user", Content: fmt.Sprintf("%s, call %d", t.Name(), h.asked.Add(1))}}
}

func (h *failingFleet) ask(t *testing.T, ctx context.Context) (string, error) {
	t.Helper()
	h.forgetHealth()
	resolved, err := h.rtr.ResolveFor(ctx, router.ResolveRequest{UserId: failedCallOwner, Level: airoute.LevelFast, Modality: airoute.ModalityChat, PromptName: "failedCallFixture"})
	if err != nil {
		t.Fatalf("ResolveFor: %v", err)
	}
	return resolved.Client.(common.ChatAIProvider).CallChat(ctx, h.prompt(t))
}

// failedCallCtx is the caller's context on the background lane. The cases that go
// through the fleet provider pass the process-wide rate ceiling, and the
// interactive lane's budget of 20 calls in 10 seconds is the rest of this
// package's.
func failedCallCtx(t *testing.T) context.Context {
	t.Helper()
	return memqlengine.ContextWithBackgroundLane(authorityCtx(t, failedCallOwner))
}

var runtimeFailure = workerservice.ModelCallOutcome{Error: "the runtime stopped", ErrorCode: "runtime_error", FinishReason: workerservice.ModelFinishError}

// THE ACCEPTANCE. The machine holding the model first takes every call until
// the record says otherwise, then the record says otherwise.
func TestAMachineFailingMostOfItsCallsIsSetAsideBehindAHealthyOne(t *testing.T) {
	h := newFailingFleet(t)
	ctx := failedCallCtx(t)

	for i := 0; i < 4; i++ {
		if _, err := h.ask(t, ctx); err == nil {
			t.Fatalf("call %d: the flaky machine failed it and the caller was told it worked", i)
		}
		row := h.ledger.next(t)
		if row["outcome"] != "error" || row["executionSurface"] != flakySurface {
			t.Fatalf("call %d wrote outcome=%v executionSurface=%v, want error on %q: "+
				"a failed row that names no machine is a failure the degraded check cannot see",
				i, row["outcome"], row["executionSurface"], flakySurface)
		}
	}
	if h.flakyCalls.Load() != 4 || h.steadyCalls.Load() != 0 {
		t.Fatalf("flaky took %d calls and steady %d, want the first machine in the order to take all 4", h.flakyCalls.Load(), h.steadyCalls.Load())
	}

	answer, err := h.ask(t, ctx)
	if err != nil {
		t.Fatalf("the call after four failures: %v", err)
	}
	if answer != steadyAnswer || h.flakyCalls.Load() != 4 || h.steadyCalls.Load() != 1 {
		t.Fatalf("answer %q, flaky %d calls, steady %d: the failing machine was not set aside behind the healthy one", answer, h.flakyCalls.Load(), h.steadyCalls.Load())
	}
	row := h.ledger.next(t)
	if row["outcome"] != "ok" || row["executionSurface"] != steadySurface {
		t.Fatalf("the served row wrote outcome=%v executionSurface=%v", row["outcome"], row["executionSurface"])
	}

	// And it says why, in the sentence admission records as the reason.
	health := memqlengine.FoldFleetHealth(h.ledger.evidence(), fleetNow())
	if degraded, why := health.For("flaky", hopModel).Degraded(hopModel); !degraded || why != "degraded: 4 of its last 4 calls failed" {
		t.Fatalf("flaky: degraded=%v %q", degraded, why)
	}

	// A machine's failures are not calls it served: the figures a person reads
	// count the answers.
	if calls, _, _ := health.Performance("flaky"); calls != 0 {
		t.Fatalf("flaky's performance counts %d calls, want 0: it answered none", calls)
	}
	if calls, _, _ := health.Performance("steady"); calls != 1 {
		t.Fatalf("steady's performance counts %d calls, want its one answer", calls)
	}
}

// The streaming path records the row on another goroutine, when the error chunk
// arrives. Ask reaches a model this way.
func TestAFailedStreamingCallNamesItsMachineToo(t *testing.T) {
	h := newFailingFleet(t)
	ctx := failedCallCtx(t)
	resolved, err := h.rtr.ResolveFor(ctx, router.ResolveRequest{UserId: failedCallOwner, Level: airoute.LevelFast, Modality: airoute.ModalityStreamingTools, PromptName: "failedCallFixture"})
	if err != nil {
		t.Fatalf("ResolveFor: %v", err)
	}
	stream, err := resolved.Client.(common.ChatStreamWithToolsProvider).CallChatStreamWithTools(ctx, h.prompt(t), nil)
	if err != nil {
		t.Fatalf("CallChatStreamWithTools: %v", err)
	}
	var streamErr error
	for chunk := range stream {
		if chunk.Error != nil {
			streamErr = chunk.Error
		}
	}
	if streamErr == nil {
		t.Fatal("the stream carried no error for a call the machine failed")
	}
	if row := h.ledger.next(t); row["outcome"] != "error" || row["executionSurface"] != flakySurface {
		t.Fatalf("the stream's row wrote outcome=%v executionSurface=%v, want error on %q", row["outcome"], row["executionSurface"], flakySurface)
	}
}

// The seam itself, over each way a machine ends a call. The result beside the
// error names the machine and carries nothing else.
func TestAFailedCallNamesTheMachineItWasPlacedOn(t *testing.T) {
	t.Run("on this replica", func(t *testing.T) {
		h := newFailingFleet(t)
		res, err := h.f.Call(authorityCtx(t, failedCallOwner), memqlengine.FleetCallRequest{ActingUserId: failedCallOwner, ModelId: hopModel, Kind: memqlengine.FleetKindChat})
		var failed *workerservice.ModelCallFailed
		if !errors.As(err, &failed) || failed.Code() != "runtime_error" {
			t.Fatalf("err = %v, want the machine's own failure with its code", err)
		}
		if res.ExecutionSurface != flakySurface || res.MachineLabel != "flaky" || res.Content != "" {
			t.Fatalf("result beside the failure = %+v, want only the machine named", res)
		}
	})
	t.Run("across the hop", func(t *testing.T) {
		h := newModelHop(t, func(context.Context, workerservice.ModelCallRequest, func(workerservice.ModelCallDelta)) workerservice.ModelCallOutcome {
			return runtimeFailure
		}, nil)
		f := newFleetInference(t, h.store)
		f.selfNodeId, f.forward = nodeA, h.link.router
		res, err := f.Call(authorityCtx(t, h.owner), memqlengine.FleetCallRequest{ActingUserId: h.owner, ModelId: hopModel, Kind: memqlengine.FleetKindChat})
		if err == nil {
			t.Fatal("the machine failed the call across the hop and the caller was told it worked")
		}
		if res.ExecutionSurface != FleetSurfacePrefix+"laptop" {
			t.Fatalf("result beside the failure = %+v, want the machine on the other replica named", res)
		}
	})
	t.Run("cancelled while it ran", func(t *testing.T) {
		h := newModelHop(t, func(ctx context.Context, _ workerservice.ModelCallRequest, emit func(workerservice.ModelCallDelta)) workerservice.ModelCallOutcome {
			emit(workerservice.ModelCallDelta{Seq: 1, Content: "a partial answer"})
			<-ctx.Done()
			return workerservice.ModelCallOutcome{FinishReason: workerservice.ModelFinishCancelled}
		}, nil)
		f := newFleetInference(t, h.store)
		f.selfNodeId, f.forward = nodeA, h.link.router
		ctx, cancel := context.WithCancel(authorityCtx(t, h.owner))
		defer cancel()
		res, err := f.Call(ctx, memqlengine.FleetCallRequest{ActingUserId: h.owner, ModelId: hopModel, Kind: memqlengine.FleetKindChat, OnDelta: func(string) { cancel() }})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want the cancellation", err)
		}
		if res.ExecutionSurface != FleetSurfacePrefix+"laptop" {
			t.Fatalf("result beside the cancellation = %+v, want the machine it was running on", res)
		}
	})
}

// A forward's sender has only the machine's own word that the call arrived. The
// holding replica may still be queueing it behind a full slot when the caller's
// deadline fires, and a machine that never saw a call cannot have failed it. On a
// two-replica fleet, charging it would make a busy machine read as a failing one
// depending on which replica the caller landed on.
func TestAForwardedCallNothingCameBackFromNamesNoMachine(t *testing.T) {
	machineCalls := &atomic.Int32{}
	h := newModelHop(t, func(context.Context, workerservice.ModelCallRequest, func(workerservice.ModelCallDelta)) workerservice.ModelCallOutcome {
		machineCalls.Add(1)
		return workerservice.ModelCallOutcome{FinishReason: workerservice.ModelFinishStop}
	}, nil)
	f := newFleetInference(t, h.store)
	f.selfNodeId, f.forward = nodeA, h.link.router
	w := h.link.handler.registry.WorkerById("laptop")
	for range 2 {
		if err := w.AcquireModel(context.Background(), hopModel, 0); err != nil {
			t.Fatal(err)
		}
		defer w.ReleaseModel(hopModel)
	}
	ctx, cancel := context.WithTimeout(authorityCtx(t, h.owner), 200*time.Millisecond)
	defer cancel()
	res, err := f.Call(ctx, memqlengine.FleetCallRequest{ActingUserId: h.owner, ModelId: hopModel, Kind: memqlengine.FleetKindChat})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's deadline", err)
	}
	if machineCalls.Load() != 0 {
		t.Fatalf("the machine ran %d calls, want none: its slots were taken", machineCalls.Load())
	}
	if res.ExecutionSurface != "" {
		t.Fatalf("result beside the deadline = %+v, want no machine named: none is known to have seen the call", res)
	}
}

func declined(code string) workerservice.ModelCallOutcome {
	return workerservice.ModelCallOutcome{Error: "declined", ErrorCode: code, FinishReason: workerservice.ModelFinishError}
}

// A machine that declines a call before handing it to a runtime is not failing:
// it is at its cap, its advertisement was a beat stale, or the call itself was
// wrong. The degraded check's verdict is machine-wide, so charging it would set a
// healthy machine aside for every model it holds.
func TestAMachineThatDeclinedACallIsNotNamed(t *testing.T) {
	for _, code := range []string{
		"model_not_offered", "model_concurrency_exceeded", "unsupported_kind", "modality_unsupported",
		"payload_unavailable", "schema_unsupported", "tools_unsupported", "duplicate_request",
	} {
		t.Run(code+" on this replica", func(t *testing.T) {
			h := newFailingFleet(t)
			h.f.registry.WorkerById("flaky").SetModelCallFunc(ending(h.flakyCalls, declined(code)))
			res, err := h.f.Call(authorityCtx(t, failedCallOwner), memqlengine.FleetCallRequest{ActingUserId: failedCallOwner, ModelId: hopModel, Kind: memqlengine.FleetKindChat})
			var failed *workerservice.ModelCallFailed
			if !errors.As(err, &failed) || failed.Code() != code {
				t.Fatalf("err = %v, want the machine's own decline with its code", err)
			}
			if res.ExecutionSurface != "" {
				t.Fatalf("result beside the decline = %+v, want no machine named", res)
			}
		})
		t.Run(code+" across the hop", func(t *testing.T) {
			h := newModelHop(t, func(context.Context, workerservice.ModelCallRequest, func(workerservice.ModelCallDelta)) workerservice.ModelCallOutcome {
				return declined(code)
			}, nil)
			f := newFleetInference(t, h.store)
			f.selfNodeId, f.forward = nodeA, h.link.router
			res, err := f.Call(authorityCtx(t, h.owner), memqlengine.FleetCallRequest{ActingUserId: h.owner, ModelId: hopModel, Kind: memqlengine.FleetKindChat})
			if err == nil {
				t.Fatal("the machine declined the call and the caller was told it worked")
			}
			if res.ExecutionSurface != "" {
				t.Fatalf("result beside the decline = %+v, want no machine named", res)
			}
		})
	}
}

// Everything else a machine ends a call with counts, a code this engine has never
// seen included: it says the machine took the call and it did not end well.
func TestAMachineThatFailedACallIsNamedWhateverItsCode(t *testing.T) {
	for _, code := range []string{"runtime_error", "timeout", "worker_stopped", "", "a_code_from_a_newer_cockpit"} {
		t.Run("code "+code, func(t *testing.T) {
			h := newFailingFleet(t)
			h.f.registry.WorkerById("flaky").SetModelCallFunc(ending(h.flakyCalls, declined(code)))
			res, err := h.f.Call(authorityCtx(t, failedCallOwner), memqlengine.FleetCallRequest{ActingUserId: failedCallOwner, ModelId: hopModel, Kind: memqlengine.FleetKindChat})
			if err == nil {
				t.Fatal("the machine failed the call and the caller was told it worked")
			}
			if res.ExecutionSurface != flakySurface {
				t.Fatalf("result beside the failure = %+v, want %q", res, flakySurface)
			}
		})
	}
}

// The same declines through the whole path: they leave no evidence, so a machine
// at its cap keeps its place in the order.
func TestAMachineDecliningItsCallsIsNotSetAside(t *testing.T) {
	h := newFailingFleet(t)
	h.f.registry.WorkerById("flaky").SetModelCallFunc(ending(h.flakyCalls, declined("model_concurrency_exceeded")))
	ctx := failedCallCtx(t)
	for i := 0; i < 5; i++ {
		if _, err := h.ask(t, ctx); err == nil {
			t.Fatalf("call %d: the machine declined it and the caller was told it worked", i)
		}
		if row := h.ledger.next(t); row["outcome"] != "error" || row["executionSurface"] != "" {
			t.Fatalf("call %d wrote outcome=%v executionSurface=%v, want error naming no machine", i, row["outcome"], row["executionSurface"])
		}
	}
	if h.flakyCalls.Load() != 5 || h.steadyCalls.Load() != 0 {
		t.Fatalf("flaky took %d calls and steady %d, want the machine at its cap still first", h.flakyCalls.Load(), h.steadyCalls.Load())
	}
}

// modelCallDeclines copies what the Cockpit knows, and nothing else ties the two
// together. A new refusal missing from the list would read as a machine failing,
// so every code the Cockpit refuses a call with has to be on it. CodeWorkerStopped
// is the one exception, on purpose: the stream went, and that is the machine.
func TestEveryRefusalTheCockpitSendsIsADecline(t *testing.T) {
	src, err := os.ReadFile("../../../cockpit/internal/worker/modelcall/session.go")
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^\s*(Code\w+)\s*=\s*"([a-z_]+)"`).FindAllStringSubmatch(string(src), -1) {
		values[m[1]] = m[2]
	}
	sent := regexp.MustCompile(`(?:refuse\(|sendRefusal\(sender, requestID, )(Code\w+)`).FindAllStringSubmatch(string(src), -1)
	if len(sent) == 0 || len(values) == 0 {
		t.Fatal("scanned the Cockpit's session and found no refusals: the scan is broken, not the list")
	}
	for _, m := range sent {
		if m[1] == "CodeWorkerStopped" {
			continue
		}
		if !modelCallDeclines[values[m[1]]] {
			t.Errorf("the Cockpit refuses a call with %s (%q), and modelCallDeclines does not list it", m[1], values[m[1]])
		}
	}
}

// A lent machine's failure says whose it was, under the rule a served call
// follows (epic memql#5327, design D15): "whose machine, if not yours".
func TestAFailedCallOnALentMachineNamesItsOwner(t *testing.T) {
	h := newLentPinHop(t, lendToEveryone, nil)
	h.link.handler.registry.WorkerById("studio").SetModelCallFunc(ending(h.studioCalls, runtimeFailure))
	res, err := h.call(t, sharedHopCaller, lentPin)
	if err == nil {
		t.Fatal("the lent machine failed the call and the caller was told it worked")
	}
	if res.ExecutionSurface != FleetSurfacePrefix+"studio" || res.MachineOwnerUserId != sharedHopOwner {
		t.Fatalf("result beside the failure = %+v, want the lent machine and its owner %q", res, sharedHopOwner)
	}
}

// A machine that refused before start ran nothing, so nothing is said about it:
// the row names the machine that took the call, and none when none did.
func TestARefusalBeforeStartNamesNoMachine(t *testing.T) {
	refuse := func(context.Context, workerservice.ModelCallRequest) (*workerservice.ModelCallHandle, error) {
		return nil, errors.New("refused before start")
	}
	t.Run("every machine refused", func(t *testing.T) {
		h := newFailingFleet(t)
		h.f.registry.WorkerById("flaky").SetModelCallFunc(refuse)
		h.f.registry.WorkerById("steady").SetModelCallFunc(refuse)
		res, err := h.f.Call(authorityCtx(t, failedCallOwner), memqlengine.FleetCallRequest{ActingUserId: failedCallOwner, ModelId: hopModel, Kind: memqlengine.FleetKindChat})
		var unavailable *memqlengine.FleetUnavailable
		if !errors.As(err, &unavailable) {
			t.Fatalf("err = %v, want the typed refusal", err)
		}
		if res.ExecutionSurface != "" || res.MachineLabel != "" || res.MachineOwnerUserId != "" {
			t.Fatalf("result beside the refusal = %+v, want no machine named: none ran the call", res)
		}
	})
	t.Run("the next machine then failed", func(t *testing.T) {
		h := newFailingFleet(t)
		h.f.registry.WorkerById("flaky").SetModelCallFunc(refuse)
		h.f.registry.WorkerById("steady").SetModelCallFunc(ending(h.steadyCalls, runtimeFailure))
		res, err := h.f.Call(authorityCtx(t, failedCallOwner), memqlengine.FleetCallRequest{ActingUserId: failedCallOwner, ModelId: hopModel, Kind: memqlengine.FleetKindChat})
		if err == nil {
			t.Fatal("the second machine failed the call and the caller was told it worked")
		}
		if res.ExecutionSurface != steadySurface {
			t.Fatalf("result beside the failure = %+v, want %q: the machine that refused ran nothing", res, steadySurface)
		}
	})
}
