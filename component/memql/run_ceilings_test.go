package memql

import (
	"context"
	"errors"
	"strings"
	"testing"
	"text/template"

	"github.com/znasllc-io/memql/core/common"
)

// run_ceilings_test.go -- the seam that makes a run's ceilings mean something
// (memql#5580).
//
// These tests are about what the SEAM does with a verdict, exactly as
// model_journal_test.go is: the guard is a double, and the guard's own
// arithmetic is component/work's (budget_test.go, spend_test.go) and
// integrations/work's (runceilings_test.go).

// fakeGuard is a RunCeilingGuard whose answer a test sets. It records what it
// was asked and what it was charged, so "refused" can be told apart from
// "never asked".
type fakeGuard struct {
	breach  *RunCeilingBreach
	admits  int
	charges []ModelSpend
	spent   RunSpend
	// lastEstimate is the estimatedTokens of the most recent admit: the
	// check must happen BEFORE the spend, so it has to be told the size of
	// the call about to be made.
	lastEstimate int
}

func (g *fakeGuard) Admit(_ context.Context, _ common.RunContext, estimatedTokens int) *RunCeilingBreach {
	g.admits++
	g.lastEstimate = estimatedTokens
	return g.breach
}

func (g *fakeGuard) Charge(_ context.Context, _ common.RunContext, spend ModelSpend) {
	g.charges = append(g.charges, spend)
}

func (g *fakeGuard) Spent(context.Context, common.RunContext) RunSpend { return g.spent }

func aRun() common.RunContext {
	return common.RunContext{RunId: "v1:work:run:r1", GoalId: "v1:work:goal:g1", StepKey: "step-a", Mode: common.RunModeLive, OwnerUserId: "u1"}
}

// -----------------------------------------------------------------------------
// the headline: a run past a ceiling has its next model call REFUSED
// -----------------------------------------------------------------------------

// A run that passed a ceiling is refused, the provider is never asked, and the
// breach is legible on the error: which ceiling, what the limit was, what the
// run had spent.
func TestARunPastItsCeilingIsRefusedAndTheBreachIsLegible(t *testing.T) {
	guard := &fakeGuard{
		breach: &RunCeilingBreach{
			Ceiling: "modelCalls",
			Limit:   "3 calls",
			Actual:  "3 made",
			Reason:  "the run reached its model-call cap",
		},
		spent: RunSpend{ModelCalls: 3, Tokens: 900, Cost: 0.42},
	}
	journal := newCountingJournal()
	seam := &modelSeam{journal: journal, ceilings: guard}
	live := &counter{answer: "should never be asked"}

	_, err := seam.serve(common.ContextWithRun(context.Background(), aRun()), aRequest("go on then"), "p", live.live)
	if err == nil {
		t.Fatal("a run past its ceiling must be refused, not served")
	}
	if live.calls != 0 {
		t.Fatalf("the provider was called %d times; a refusal happens BEFORE the spend", live.calls)
	}

	var breach *RunCeilingError
	if !errors.As(err, &breach) {
		t.Fatalf("the refusal must be TYPED so the park can read the figures rather than parse a sentence; got %T: %v", err, err)
	}
	if breach.Breach.Ceiling != "modelCalls" || breach.Breach.Limit != "3 calls" || breach.Breach.Actual != "3 made" {
		t.Fatalf("the breach lost its figures: %+v", breach.Breach)
	}
	if breach.RunId != "v1:work:run:r1" || breach.StepKey != "step-a" {
		t.Fatalf("the refusal must name the run and the step it stopped at: %+v", breach)
	}
	if breach.Spent.ModelCalls != 3 || breach.Spent.Tokens != 900 {
		t.Fatalf("the refusal carries what the run had spent, for the park to put on the row: %+v", breach.Spent)
	}
	// The MESSAGE must name the ceiling too -- it is what lands in
	// v1:work:run.errorMessage, which a person reads without opening the
	// approval.
	for _, want := range []string{"modelCalls", "3 calls", "3 made"} {
		if !strings.Contains(breach.Error(), want) {
			t.Errorf("the rendered refusal does not name %q: %s", want, breach.Error())
		}
	}
}

// THE REFUSAL MUST NOT READ AS THE PROCESS COST CEILING. component/work's
// DoorsFrom falls back to a substring match over the router's refusal codes,
// and a run ceiling that answered as `ceiling_reached` would park on an
// inferenceUnavailable approval asking a person to raise the wrong limit.
func TestARunCeilingRefusalIsNotTheProcessCeilingRefusal(t *testing.T) {
	err := &RunCeilingError{
		RunId:  "v1:work:run:r1",
		Breach: RunCeilingBreach{Ceiling: "cost", Limit: "$1.00", Actual: "$1.00", Reason: "the run reached its cost ceiling"},
	}
	for _, code := range []string{"ceiling_reached", "every_door_shut", "no_local_model_available", "no_app_available"} {
		if strings.Contains(err.Error(), code) {
			t.Fatalf("the run-ceiling refusal carries the router's refusal code %q, so the two ceilings stop being tellable apart: %s", code, err.Error())
		}
	}
}

// A run under its ceilings is untouched: the provider answers and the call is
// charged exactly once.
func TestARunUnderItsCeilingsIsUnaffected(t *testing.T) {
	guard := &fakeGuard{}
	seam := &modelSeam{journal: newCountingJournal(), ceilings: guard}
	live := &counter{answer: "the answer"}

	got, err := seam.serve(common.ContextWithRun(context.Background(), aRun()), aRequest("a question"), "p", live.live)
	if err != nil {
		t.Fatalf("a run under its ceilings must not be refused: %v", err)
	}
	if got != "the answer" || live.calls != 1 {
		t.Fatalf("answer = %v after %d live calls; want the provider's answer after one", got, live.calls)
	}
	if guard.admits == 0 {
		t.Fatal("the guard was never asked, so nothing would have refused this run at any spend")
	}
	if len(guard.charges) != 1 {
		t.Fatalf("charges = %+v; one answer is one charge", guard.charges)
	}
	if guard.charges[0].Served != ServedLive {
		t.Fatalf("a provider answer must be charged as metered: %+v", guard.charges[0])
	}
}

// THE CHECK HAPPENS BEFORE THE SPEND, so it is told the size of the call about
// to be made. A zero estimate would let the last call over a token budget
// through every time.
func TestTheAdmitIsToldTheSizeOfTheCallAboutToBeMade(t *testing.T) {
	guard := &fakeGuard{}
	seam := &modelSeam{journal: newCountingJournal(), ceilings: guard}
	live := &counter{answer: "ok"}
	if _, err := seam.serve(common.ContextWithRun(context.Background(), aRun()), aRequest("a fairly long question, as questions go"), "p", live.live); err != nil {
		t.Fatal(err)
	}
	if guard.lastEstimate <= 0 {
		t.Fatalf("estimatedTokens = %d; a before-the-spend check with a zero estimate is an after-the-spend check", guard.lastEstimate)
	}
}

// -----------------------------------------------------------------------------
// cached and replayed answers
// -----------------------------------------------------------------------------

// A JOURNAL HIT IS CHARGED AS ONE CALL AND NO MONEY. It is an answered request
// -- so the loop caps count it -- and MemQL was not billed for it, so the
// dollar ceilings do not. The recorded token counts are the ORIGINAL call's,
// which is why the charge reports them under `journal` rather than as metered.
func TestAJournalHitCostsALoopCallAndNoMoney(t *testing.T) {
	journal := newCountingJournal()
	guard := &fakeGuard{}
	seam := &modelSeam{journal: journal, ceilings: guard}

	// Record a run, then replay it.
	recorded := common.RunContext{RunId: "run-1", GoalId: "g1", StepKey: "a", Mode: common.RunModeLive, OwnerUserId: "u1"}
	live := &counter{answer: "recorded"}
	if _, err := seam.serve(common.ContextWithRun(context.Background(), recorded), aRequest("a"), "p", live.live); err != nil {
		t.Fatal(err)
	}
	guard.charges = nil

	replay := common.RunContext{RunId: "run-2", GoalId: "g1", SourceRunId: "run-1", SourceGoalId: "g1", StepKey: "a", Mode: common.RunModeReplay, OwnerUserId: "u1"}
	if _, err := seam.serve(common.ContextWithRun(context.Background(), replay), aRequest("a"), "p", live.live); err != nil {
		t.Fatal(err)
	}
	if live.calls != 1 {
		t.Fatalf("the replay called a provider; live calls = %d", live.calls)
	}
	if len(guard.charges) != 1 || guard.charges[0].Served != ServedJournal {
		t.Fatalf("a replayed answer must be charged once, as `journal`: %+v", guard.charges)
	}
	if guard.charges[0].Cost != 0 {
		t.Fatalf("nothing was billed for a replayed answer: %+v", guard.charges[0])
	}
}

// THE CEILING IS ASKED ABOVE THE JOURNAL, not below it. A run past its loop cap
// must be refused whether the answer would have come from a provider or from
// the journal -- otherwise a replay of a runaway run runs away again, for free.
func TestARunPastItsCeilingIsRefusedEvenWhenTheJournalCouldServe(t *testing.T) {
	journal := newCountingJournal()
	seam := &modelSeam{journal: journal, ceilings: &fakeGuard{}}
	recorded := common.RunContext{RunId: "run-1", GoalId: "g1", StepKey: "a", Mode: common.RunModeLive, OwnerUserId: "u1"}
	live := &counter{answer: "recorded"}
	if _, err := seam.serve(common.ContextWithRun(context.Background(), recorded), aRequest("a"), "p", live.live); err != nil {
		t.Fatal(err)
	}

	guard := &fakeGuard{breach: &RunCeilingBreach{Ceiling: "modelCalls", Limit: "1 call", Actual: "1 made", Reason: "cap"}}
	seam.ceilings = guard
	replay := common.RunContext{RunId: "run-2", GoalId: "g1", SourceRunId: "run-1", SourceGoalId: "g1", StepKey: "a", Mode: common.RunModeReplay, OwnerUserId: "u1"}
	if _, err := seam.serve(common.ContextWithRun(context.Background(), replay), aRequest("a"), "p", live.live); err == nil {
		t.Fatal("a run past its loop cap was served from the journal; a replayed answer is still an answered request")
	}
	if len(guard.charges) != 0 {
		t.Fatalf("a refused call charges nothing: %+v", guard.charges)
	}
}

// A FAILED PROVIDER CALL IS STILL A CALL. A loop that fails forever is a
// runaway loop, and the journal already records the failures for the same
// reason.
func TestAFailedCallIsCharged(t *testing.T) {
	guard := &fakeGuard{}
	seam := &modelSeam{journal: newCountingJournal(), ceilings: guard}
	live := &counter{err: errors.New("the provider said no")}
	if _, err := seam.serve(common.ContextWithRun(context.Background(), aRun()), aRequest("q"), "p", live.live); err == nil {
		t.Fatal("expected the provider's error")
	}
	if len(guard.charges) != 1 {
		t.Fatalf("a failed call must still burn a loop call: %+v", guard.charges)
	}
}

// -----------------------------------------------------------------------------
// the boundaries
// -----------------------------------------------------------------------------

// A call with NO RUN on its context is neither admitted nor charged. That is
// most calls in the product -- a chat turn, a suggest, a safety
// classification -- and a run ceiling is meaningless for them.
func TestACallOutsideARunIsNeitherAdmittedNorCharged(t *testing.T) {
	guard := &fakeGuard{breach: &RunCeilingBreach{Ceiling: "modelCalls", Reason: "would refuse everything"}}
	seam := &modelSeam{journal: newCountingJournal(), ceilings: guard}
	live := &counter{answer: "fine"}
	got, err := seam.serve(context.Background(), aRequest("q"), "p", live.live)
	if err != nil || got != "fine" {
		t.Fatalf("a call outside a run must pass straight through; got %v, %v", got, err)
	}
	if guard.admits != 0 || len(guard.charges) != 0 {
		t.Fatalf("the guard was consulted for a call that belongs to no run: admits=%d charges=%+v", guard.admits, guard.charges)
	}
}

// A NODE WITH NO GUARD WIRED runs as it did before this existed. It is not a
// silent pass -- app/ logs which of the two seams it wired -- and a node with
// no work integration hosts no work runs to bound.
func TestNoGuardWiredIsAWorkingConfiguration(t *testing.T) {
	seam := &modelSeam{journal: newCountingJournal()}
	live := &counter{answer: "fine"}
	if _, err := seam.serve(common.ContextWithRun(context.Background(), aRun()), aRequest("q"), "p", live.live); err != nil {
		t.Fatalf("a node with no guard must still serve: %v", err)
	}
}

// A CEILING THAT COULD NOT BE EVALUATED REFUSES, and says which of the two it
// is. Passing here would make "nobody could read the limit" and "nobody set a
// limit" the same answer.
func TestAnUnevaluatedCeilingRefuses(t *testing.T) {
	guard := &fakeGuard{breach: &RunCeilingBreach{
		Ceiling: "unevaluated",
		Limit:   "unknown",
		Actual:  "unknown",
		Reason:  "this run's ceilings could not be read",
	}}
	seam := &modelSeam{journal: newCountingJournal(), ceilings: guard}
	live := &counter{answer: "should not be asked"}
	_, err := seam.serve(common.ContextWithRun(context.Background(), aRun()), aRequest("q"), "p", live.live)
	if err == nil {
		t.Fatal("a ceiling that cannot be evaluated must not silently pass")
	}
	var breach *RunCeilingError
	if !errors.As(err, &breach) || breach.Breach.Ceiling != "unevaluated" {
		t.Fatalf("the refusal must name itself as unevaluated rather than as a real ceiling: %v", err)
	}
	if live.calls != 0 {
		t.Fatal("the provider was called despite an unevaluated ceiling")
	}
}

// -----------------------------------------------------------------------------
// the in-process response cache, which sits ABOVE the seam
// -----------------------------------------------------------------------------

// aCachingRuntime is the ai() runtime with the exact-hash cache on and one
// prompt registered.
func aCachingRuntime(t *testing.T, guard RunCeilingGuard) (*aiRuntime, *mockAIProvider) {
	t.Helper()
	prompts := newPromptRegistry()
	prompts.set(&PromptTemplate{
		Level:           "fast",
		Name:            "ceilingPrompt",
		TemplateSource:  "hello {{.name}}",
		tmpl:            template.Must(template.New("ceiling").Parse("hello {{.name}}")),
		DefaultProvider: "mock",
	})
	providers := newProviderRegistry()
	mock := &mockAIProvider{}
	providers.setEntry(&ProviderConfigEntry{
		Config:    ProviderConfig{Name: "mock", Type: "test"},
		Client:    mock,
		Available: true,
	})
	runtime := newTestAIRuntime(prompts, providers, aiCacheConfig{DefaultEnabled: true, MaxTTLSeconds: 120})
	if runtime == nil {
		t.Fatal("no runtime")
	}
	runtime.seam = &modelSeam{ceilings: guard}
	return runtime, mock
}

// A CACHE HIT COSTS A LOOP CALL AND NO MONEY. The in-process exact-hash cache
// answers ABOVE the model seam, so without this a run could be served warm
// answers forever without burning maxModelCalls -- the runaway the cap exists
// to stop, minus the bill. The overcharge we chose is on the loop side.
func TestACacheHitBurnsALoopCallAndNoMoney(t *testing.T) {
	guard := &fakeGuard{}
	runtime, mock := aCachingRuntime(t, guard)
	ctx := common.ContextWithRun(context.Background(), aRun())
	inv := &AIInvocation{TemplateId: "ceilingPrompt"}
	data := map[string]any{"name": "Ada"}

	if _, err := runtime.Invoke(ctx, inv, data); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Invoke(ctx, inv, data); err != nil {
		t.Fatal(err)
	}
	if mock.calls != 1 {
		t.Fatalf("provider calls = %d; the second invocation must be a cache hit", mock.calls)
	}
	if len(guard.charges) != 2 {
		t.Fatalf("charges = %+v; a cache hit is still an answered request", guard.charges)
	}
	if guard.charges[1].Served != ServedCache {
		t.Fatalf("the second charge must be a cache hit: %+v", guard.charges[1])
	}
	if guard.charges[1].Cost != 0 || guard.charges[1].InputTokens != 0 || guard.charges[1].OutputTokens != 0 {
		t.Fatalf("nothing was billed for a cache hit, and its tokens belong to the original call: %+v", guard.charges[1])
	}
}

// THE CEILING IS ASKED ABOVE THE CACHE. A run past its cap must be refused
// even when the answer is sitting warm in memory.
func TestARunPastItsCeilingIsRefusedBeforeTheCacheIsConsulted(t *testing.T) {
	guard := &fakeGuard{}
	runtime, mock := aCachingRuntime(t, guard)
	ctx := common.ContextWithRun(context.Background(), aRun())
	inv := &AIInvocation{TemplateId: "ceilingPrompt"}
	data := map[string]any{"name": "Ada"}

	if _, err := runtime.Invoke(ctx, inv, data); err != nil {
		t.Fatal(err)
	}
	guard.breach = &RunCeilingBreach{Ceiling: "modelCalls", Limit: "1 call", Actual: "1 made", Reason: "cap"}
	guard.charges = nil

	_, err := runtime.Invoke(ctx, inv, data)
	if err == nil {
		t.Fatal("a run past its cap was served from the in-process cache")
	}
	var breach *RunCeilingError
	if !errors.As(err, &breach) || breach.Breach.Ceiling != "modelCalls" {
		t.Fatalf("the refusal must be the typed breach: %v", err)
	}
	if len(guard.charges) != 0 {
		t.Fatalf("a refused call charges nothing: %+v", guard.charges)
	}
	if mock.calls != 1 {
		t.Fatalf("provider calls = %d; the refusal must not have reached a provider either", mock.calls)
	}
}

// A run with no ceilings, and a call outside a run, both stay exactly as they
// were: the cache goes on caching and nothing is refused.
func TestTheCachePathIsUnchangedOutsideARun(t *testing.T) {
	guard := &fakeGuard{breach: &RunCeilingBreach{Ceiling: "modelCalls", Reason: "would refuse everything"}}
	runtime, mock := aCachingRuntime(t, guard)
	inv := &AIInvocation{TemplateId: "ceilingPrompt"}
	data := map[string]any{"name": "Ada"}
	for i := 0; i < 2; i++ {
		if _, err := runtime.Invoke(context.Background(), inv, data); err != nil {
			t.Fatalf("a call outside a run must not be refused: %v", err)
		}
	}
	if mock.calls != 1 {
		t.Fatalf("provider calls = %d; the cache must still cache", mock.calls)
	}
	if guard.admits != 0 || len(guard.charges) != 0 {
		t.Fatalf("the guard was consulted outside a run: admits=%d charges=%+v", guard.admits, guard.charges)
	}
}
