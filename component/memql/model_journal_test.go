package memql

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"text/template"

	"github.com/znasllc-io/memql/core/common"
)

// model_journal_test.go -- the headline claim of memql#4999 and the four
// properties that make it worth anything.
//
// THE HEADLINE: a replay of one run makes ZERO provider calls. It is asserted
// against a COUNTING PROVIDER rather than a mock's expectations, because the
// claim is about behaviour -- nothing reached a provider -- and a mock that
// was simply never told to expect a call would pass while a call was made.
//
// Every test here runs with no database: the seam is a decision plus a journal
// interface, and both halves are values.

// countingJournal is an in-memory ModelCallJournal. Reads and writes are
// counted separately from the provider so a test can tell "served from the
// journal" apart from "never asked anything at all".
type countingJournal struct {
	rows      map[string][]JournaledCall // runId -> calls
	lookups   int
	records   int
	recordErr error
}

func newCountingJournal() *countingJournal {
	return &countingJournal{rows: map[string][]JournaledCall{}}
}

func (j *countingJournal) Lookup(_ context.Context, _, runId, hash string) (*JournaledCall, bool) {
	j.lookups++
	for i := range j.rows[runId] {
		if j.rows[runId][i].RequestHash == hash {
			return &j.rows[runId][i], true
		}
	}
	return nil, false
}

func (j *countingJournal) Record(_ context.Context, _ string, call JournaledCall) error {
	j.records++
	if j.recordErr != nil {
		return j.recordErr
	}
	j.rows[call.RunId] = append(j.rows[call.RunId], call)
	return nil
}

func (j *countingJournal) served(runId string) []string {
	out := make([]string, 0, len(j.rows[runId]))
	for _, c := range j.rows[runId] {
		out = append(out, c.Served)
	}
	return out
}

func aRequest(prompt string) common.ModelRequest {
	return common.ModelRequest{
		Provider: "chat54Mini",
		Model:    "gpt-5.4-mini",
		Messages: []common.ChatMessage{{Role: "user", Content: prompt}},
	}
}

// counter counts live calls and answers with a fixed string.
type counter struct {
	calls  int
	answer string
	err    error
}

func (c *counter) live(_ context.Context) (modelCallOutcome, error) {
	c.calls++
	if c.err != nil {
		return modelCallOutcome{}, c.err
	}
	return modelCallOutcome{Value: c.answer}, nil
}

// -----------------------------------------------------------------------------
// the headline
// -----------------------------------------------------------------------------

// A replay of a recorded run reaches NO provider. This is the claim the design
// record makes and the one component/proving reports.
func TestAReplayMakesZeroProviderCalls(t *testing.T) {
	journal := newCountingJournal()
	seam := &modelSeam{journal: journal}

	// The recorded run: three distinct requests, each answered live.
	recorded := common.RunContext{RunId: "run-1", GoalId: "goal-1", Mode: common.RunModeLive, OwnerUserId: "u1"}
	live := &counter{answer: "recorded answer"}
	for _, step := range []string{"a", "b", "c"} {
		rc := recorded
		rc.StepKey = step
		if _, err := seam.serve(common.ContextWithRun(context.Background(), rc), aRequest(step), "p", live.live); err != nil {
			t.Fatalf("recording step %s: %v", step, err)
		}
	}
	if live.calls != 3 {
		t.Fatalf("the recorded run made %d provider calls, want 3 -- the fixture is wrong, not the claim", live.calls)
	}

	// The replay: same requests, a new run reading run-1's journal.
	replayLive := &counter{answer: "SHOULD NOT BE CALLED"}
	replay := common.RunContext{
		RunId: "run-2", GoalId: "goal-1", Mode: common.RunModeReplay, ReplayPolicy: common.ReplayStrict,
		SourceRunId: "run-1", SourceGoalId: "goal-1", OwnerUserId: "u1",
	}
	for _, step := range []string{"a", "b", "c"} {
		rc := replay
		rc.StepKey = step
		got, err := seam.serve(common.ContextWithRun(context.Background(), rc), aRequest(step), "p", replayLive.live)
		if err != nil {
			t.Fatalf("replaying step %s: %v", step, err)
		}
		if got != "recorded answer" {
			t.Errorf("step %s served %q, want the recorded answer", step, got)
		}
	}

	if replayLive.calls != 0 {
		t.Errorf("THE HEADLINE FAILED: a replay made %d provider calls, want 0", replayLive.calls)
	}
	// And the replay is inspectable: it wrote its own rows, marked journal.
	if got := journal.served("run-2"); len(got) != 3 {
		t.Errorf("the replay wrote %d rows, want 3 -- a replay with an empty journal cannot be compared\n"+
			"against the run it replays, which is most of what a replay is for", len(got))
	} else {
		for i, s := range got {
			if s != "journal" {
				t.Errorf("replay row %d recorded served=%q, want \"journal\"", i, s)
			}
		}
	}
}

// THE NEGATIVE CONTROL for the headline. A counter that never rises on any
// path reads as zero forever, so the same fixture must produce a NON-ZERO
// count when the journal is not serving.
func TestControlALiveRunDoesReachTheProvider(t *testing.T) {
	seam := &modelSeam{journal: newCountingJournal()}
	live := &counter{answer: "x"}
	rc := common.RunContext{RunId: "run-1", GoalId: "goal-1", StepKey: "a", Mode: common.RunModeLive, OwnerUserId: "u1"}
	for i := 0; i < 3; i++ {
		if _, err := seam.serve(common.ContextWithRun(context.Background(), rc), aRequest(fmt.Sprint(i)), "p", live.live); err != nil {
			t.Fatalf("live call %d: %v", i, err)
		}
	}
	if live.calls == 0 {
		t.Fatal("the counting provider never rose on a LIVE run either -- the headline's zero would be " +
			"an artefact of an instrument that cannot count")
	}
}

// -----------------------------------------------------------------------------
// divergence
// -----------------------------------------------------------------------------

// A strict replay whose journal misses must FAIL, pinned to the step, and must
// NOT quietly make a fresh call. A replay exists to prove a recorded run
// reproduces; calling a provider on a miss answers "yes" having reproduced
// nothing.
func TestAStrictReplayMissDivergesAtTheStepAndCallsNothing(t *testing.T) {
	journal := newCountingJournal()
	seam := &modelSeam{journal: journal}

	rc := common.RunContext{
		RunId: "run-2", GoalId: "goal-1", StepKey: "step-b", Mode: common.RunModeReplay,
		ReplayPolicy: common.ReplayStrict, SourceRunId: "run-1", SourceGoalId: "goal-1", OwnerUserId: "u1",
	}
	live := &counter{answer: "fresh"}
	_, err := seam.serve(common.ContextWithRun(context.Background(), rc), aRequest("nothing recorded"), "p", live.live)

	if err == nil {
		t.Fatal("a strict replay with no journaled match returned no error -- it was silently downgraded to a live call")
	}
	var diverged *DivergenceError
	if !errors.As(err, &diverged) {
		t.Fatalf("the error is not a DivergenceError, so no caller can tell a divergence from a provider outage: %v", err)
	}
	if diverged.StepKey != "step-b" {
		t.Errorf("the divergence names step %q, want the step that differed", diverged.StepKey)
	}
	if diverged.RunId != "run-2" || diverged.RequestHash == "" {
		t.Errorf("the divergence does not carry enough to find it: %+v", diverged)
	}
	if live.calls != 0 {
		t.Errorf("a strict divergence made %d provider calls, want 0", live.calls)
	}
}

// permissive is the documented way to ask for the other behaviour, and it must
// journal the fresh call so the next replay of THIS run hits.
func TestAPermissiveReplayMissCallsLiveAndJournalsIt(t *testing.T) {
	journal := newCountingJournal()
	seam := &modelSeam{journal: journal}
	rc := common.RunContext{
		RunId: "run-2", GoalId: "goal-1", StepKey: "step-b", Mode: common.RunModeReplay,
		ReplayPolicy: common.ReplayPermissive, SourceRunId: "run-1", SourceGoalId: "goal-1", OwnerUserId: "u1",
	}
	live := &counter{answer: "fresh"}
	got, err := seam.serve(common.ContextWithRun(context.Background(), rc), aRequest("new"), "p", live.live)
	if err != nil {
		t.Fatalf("permissive replay: %v", err)
	}
	if got != "fresh" || live.calls != 1 {
		t.Errorf("permissive miss returned %v after %d calls, want the fresh answer after 1", got, live.calls)
	}
	if s := journal.served("run-2"); len(s) != 1 || s[0] != "live" {
		t.Errorf("the fresh call was journaled as %v, want one row marked live", s)
	}
}

// THE CROSS-GOAL RULE OUTRANKS THE MODE. A journal entry recorded for another
// goal is never served, in any mode -- the one rule DecideServe applies before
// looking at the mode at all.
func TestAJournalFromAnotherGoalIsNeverServed(t *testing.T) {
	journal := newCountingJournal()
	journal.rows["run-1"] = []JournaledCall{{
		RunId: "run-1", RequestHash: aRequest("q").Hash(), Response: map[string]any{"text": "other goal's answer"},
	}}
	seam := &modelSeam{journal: journal}

	rc := common.RunContext{
		RunId: "run-2", GoalId: "goal-2", StepKey: "a", Mode: common.RunModeReplay,
		ReplayPolicy: common.ReplayPermissive, SourceRunId: "run-1", SourceGoalId: "goal-1", OwnerUserId: "u1",
	}
	live := &counter{answer: "correctly asked afresh"}
	got, err := seam.serve(common.ContextWithRun(context.Background(), rc), aRequest("q"), "p", live.live)
	if err != nil {
		t.Fatalf("cross-goal replay: %v", err)
	}
	if live.calls != 1 || got != "correctly asked afresh" {
		t.Errorf("a journal entry from goal-1 was served to a run on goal-2 (calls=%d, answer=%v)", live.calls, got)
	}
}

// A fork serves the shared prefix and runs live from the fork step on.
func TestAForkServesThePrefixOnly(t *testing.T) {
	journal := newCountingJournal()
	hashes := map[string]string{}
	for _, step := range []string{"a", "b", "c"} {
		hashes[step] = aRequest(step).Hash()
		journal.rows["run-1"] = append(journal.rows["run-1"], JournaledCall{
			RunId: "run-1", StepKey: step, RequestHash: hashes[step],
			Response: map[string]any{"text": "recorded " + step},
		})
	}
	seam := &modelSeam{journal: journal}
	base := common.RunContext{
		RunId: "run-2", GoalId: "goal-1", Mode: common.RunModeFork,
		SourceRunId: "run-1", SourceGoalId: "goal-1", OwnerUserId: "u1",
		ForkAtStepKey: "b", StepOrder: []string{"a", "b", "c"},
	}
	live := &counter{answer: "live answer"}
	for _, step := range []string{"a", "b", "c"} {
		rc := base
		rc.StepKey = step
		got, err := seam.serve(common.ContextWithRun(context.Background(), rc), aRequest(step), "p", live.live)
		if err != nil {
			t.Fatalf("fork step %s: %v", step, err)
		}
		want := "live answer"
		if step == "a" {
			want = "recorded a"
		}
		if got != want {
			t.Errorf("fork step %s served %q, want %q", step, got, want)
		}
	}
	if live.calls != 2 {
		t.Errorf("a fork at step b made %d live calls, want 2 (b and c)", live.calls)
	}
}

// -----------------------------------------------------------------------------
// the calls that are NOT part of a run
// -----------------------------------------------------------------------------

// Most model calls in the product are not part of a run: a chat turn, a
// suggest, a safety classification. They must pass straight through and be
// journaled nowhere -- a journal entry only means anything relative to a run
// that can be replayed.
func TestACallOutsideARunIsNeitherServedNorJournaled(t *testing.T) {
	journal := newCountingJournal()
	seam := &modelSeam{journal: journal}
	live := &counter{answer: "answer"}
	got, err := seam.serve(context.Background(), aRequest("q"), "p", live.live)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	if got != "answer" || live.calls != 1 {
		t.Errorf("a call with no run did not pass through (calls=%d, answer=%v)", live.calls, got)
	}
	if journal.records != 0 || journal.lookups != 0 {
		t.Errorf("a call with no run touched the journal: %d lookups, %d records", journal.lookups, journal.records)
	}
}

// A live run reads no journal. Looking one up would answer a question nothing
// asked, at the price of a database round trip per model call.
func TestALiveRunDoesNotReadTheJournal(t *testing.T) {
	journal := newCountingJournal()
	seam := &modelSeam{journal: journal}
	rc := common.RunContext{RunId: "run-1", GoalId: "g", StepKey: "a", Mode: common.RunModeLive, OwnerUserId: "u1"}
	if _, err := seam.serve(common.ContextWithRun(context.Background(), rc), aRequest("q"), "p", (&counter{answer: "x"}).live); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if journal.lookups != 0 {
		t.Errorf("a live run read the journal %d times", journal.lookups)
	}
	if journal.records != 1 {
		t.Errorf("a live run recorded %d calls, want 1 -- a live run is what FILLS the journal", journal.records)
	}
}

// An engine with no journal wired is a LIVE-ONLY engine, which is what every
// binary that does not host the work integration runs as. A live run must be
// unaffected by its absence.
func TestNoJournalIsAWorkingConfigurationForALiveRun(t *testing.T) {
	seam := &modelSeam{}
	rc := common.RunContext{RunId: "run-1", GoalId: "g", StepKey: "a", Mode: common.RunModeLive, OwnerUserId: "u1"}
	live := &counter{answer: "x"}
	got, err := seam.serve(common.ContextWithRun(context.Background(), rc), aRequest("q"), "p", live.live)
	if err != nil || got != "x" || live.calls != 1 {
		t.Errorf("an engine with no journal did not run live: got %v, err %v, calls %d", got, err, live.calls)
	}
	var nilSeam *modelSeam
	if _, err := nilSeam.serve(context.Background(), aRequest("q"), "p", live.live); err != nil {
		t.Errorf("a nil seam returned an error rather than calling through: %v", err)
	}
}

// But a REPLAY against no journal must say THAT, and it must not say the
// prompt changed.
//
// This was the seam's own first defect, caught by this file: with no journal
// wired every lookup misses, and a strict replay reported DecideServe's
// reason -- "the prompt, the model or the settings changed since the recorded
// run". Confident, checkable, and wrong: nothing changed, the node simply has
// no journal. It is the same shape of fault as install-cluster-e2e's
// `result.workloadsReady did not satisfy resultTrue` -- an accurate report of
// the predicate that failed, pointing at the wrong thing.
func TestAReplayWithNoJournalWiredSaysSoRatherThanBlamingThePrompt(t *testing.T) {
	seam := &modelSeam{}
	rc := common.RunContext{
		RunId: "run-2", GoalId: "g", StepKey: "a", Mode: common.RunModeReplay,
		ReplayPolicy: common.ReplayStrict, SourceRunId: "run-1", OwnerUserId: "u1",
	}
	live := &counter{answer: "x"}
	_, err := seam.serve(common.ContextWithRun(context.Background(), rc), aRequest("q"), "p", live.live)
	if err == nil {
		t.Fatal("a strict replay with no journal ran live -- the one thing strict mode forbids")
	}
	var diverged *DivergenceError
	if errors.As(err, &diverged) {
		t.Errorf("a missing journal was reported as a DIVERGENCE, which blames the prompt: %v", err)
	}
	for _, want := range []string{"no model-call journal", "run-1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
	if live.calls != 0 {
		t.Errorf("it made %d provider calls anyway", live.calls)
	}
}

// -----------------------------------------------------------------------------
// what a failed call and a failed write each do
// -----------------------------------------------------------------------------

// A FAILED CALL IS JOURNALED. A replay whose journal skips the failures
// reproduces a run that never happened, and "the third attempt is where it
// broke" is exactly the question a journal is read to answer.
func TestAFailedProviderCallIsJournaled(t *testing.T) {
	journal := newCountingJournal()
	seam := &modelSeam{journal: journal}
	rc := common.RunContext{RunId: "run-1", GoalId: "g", StepKey: "a", Mode: common.RunModeLive, OwnerUserId: "u1"}
	boom := errors.New("provider timed out")
	if _, err := seam.serve(common.ContextWithRun(context.Background(), rc), aRequest("q"), "p", (&counter{err: boom}).live); !errors.Is(err, boom) {
		t.Fatalf("the provider error did not reach the caller: %v", err)
	}
	if len(journal.rows["run-1"]) != 1 {
		t.Fatalf("a failed call wrote %d rows, want 1", len(journal.rows["run-1"]))
	}
	if got := journal.rows["run-1"][0].Error; got != boom.Error() {
		t.Errorf("the journaled row records error %q, want the provider's", got)
	}
}

// The journal is a record of work, not a precondition for it. A run whose
// journal write fails has still done the work and still has an answer.
func TestAFailedJournalWriteDoesNotFailTheCall(t *testing.T) {
	journal := newCountingJournal()
	journal.recordErr = errors.New("database unreachable")
	seam := &modelSeam{journal: journal}
	rc := common.RunContext{RunId: "run-1", GoalId: "g", StepKey: "a", Mode: common.RunModeLive, OwnerUserId: "u1"}
	got, err := seam.serve(common.ContextWithRun(context.Background(), rc), aRequest("q"), "p", (&counter{answer: "the answer"}).live)
	if err != nil {
		t.Fatalf("a journal write failure failed the call: %v", err)
	}
	if got != "the answer" {
		t.Errorf("got %v, want the provider's answer", got)
	}
}

// A journal-served call is recorded at ZERO cost. Copying the original's cost
// would make a replay look exactly as expensive as the run it is proving was
// free.
func TestAJournalServedCallCostsNothing(t *testing.T) {
	journal := newCountingJournal()
	journal.rows["run-1"] = []JournaledCall{{
		RunId: "run-1", RequestHash: aRequest("q").Hash(), Cost: 0.42, InputTokens: 100, OutputTokens: 50,
		Response: map[string]any{"text": "a"},
	}}
	seam := &modelSeam{journal: journal}
	rc := common.RunContext{
		RunId: "run-2", GoalId: "g", StepKey: "a", Mode: common.RunModeReplay,
		SourceRunId: "run-1", SourceGoalId: "g", OwnerUserId: "u1",
	}
	if _, err := seam.serve(common.ContextWithRun(context.Background(), rc), aRequest("q"), "p", (&counter{}).live); err != nil {
		t.Fatalf("replay: %v", err)
	}
	row := journal.rows["run-2"][0]
	if row.Cost != 0 {
		t.Errorf("a journal-served call recorded cost %v, want 0 -- nothing was billed for it", row.Cost)
	}
	if row.InputTokens != 100 || row.OutputTokens != 50 {
		t.Errorf("the recorded token counts were not carried over: %+v", row)
	}
}

// -----------------------------------------------------------------------------
// end to end through the ai() path
// -----------------------------------------------------------------------------

// The seam is only worth anything if it is actually INSTALLED. This drives the
// real ai() runtime -- prompt registry, provider registry, cache and all --
// and asserts the provider is not reached on a replay.
func TestTheAIRuntimeServesAReplayWithoutCallingItsProvider(t *testing.T) {
	newRuntime := func(journal ModelCallJournal) (*aiRuntime, *mockAIProvider) {
		prompts := newPromptRegistry()
		prompts.set(&PromptTemplate{
			Name:            "probe",
			TemplateSource:  "say {{.word}}",
			tmpl:            template.Must(template.New("probe").Parse("say {{.word}}")),
			DefaultProvider: "mock",
		})
		providers := newProviderRegistry("")
		mock := &mockAIProvider{}
		providers.setEntry(&ProviderConfigEntry{
			Config:    ProviderConfig{Name: "mock", Type: "test", Model: "test-1"},
			Client:    mock,
			Available: true,
		})
		// Caches OFF: a cache hit is not a model call, and leaving them on
		// would let the second run pass for the wrong reason.
		rt := newAIRuntime(nil, prompts, providers, aiCacheConfig{})
		rt.seam = &modelSeam{journal: journal}
		return rt, mock
	}

	journal := newCountingJournal()
	inv := &AIInvocation{TemplateId: "probe"}
	data := map[string]any{"word": "hello"}

	recording, recordingProvider := newRuntime(journal)
	rec := common.RunContext{RunId: "run-1", GoalId: "g", StepKey: "s", Mode: common.RunModeLive, OwnerUserId: "u1"}
	if _, err := recording.Invoke(common.ContextWithRun(context.Background(), rec), inv, data); err != nil {
		t.Fatalf("recording run: %v", err)
	}
	if recordingProvider.calls != 1 {
		t.Fatalf("the recording run made %d provider calls, want 1", recordingProvider.calls)
	}

	replaying, replayProvider := newRuntime(journal)
	rep := common.RunContext{
		RunId: "run-2", GoalId: "g", StepKey: "s", Mode: common.RunModeReplay, ReplayPolicy: common.ReplayStrict,
		SourceRunId: "run-1", SourceGoalId: "g", OwnerUserId: "u1",
	}
	got, err := replaying.Invoke(common.ContextWithRun(context.Background(), rep), inv, data)
	if err != nil {
		t.Fatalf("replay through the ai() runtime: %v", err)
	}
	if replayProvider.calls != 0 {
		t.Errorf("the replay made %d provider calls through the real ai() path, want 0", replayProvider.calls)
	}
	if got != "say hello" {
		t.Errorf("the replay served %v, want the recorded answer", got)
	}
}

// A divergence must reach the ai() caller AS a divergence, not wrapped as
// "ai call via provider failed" -- which names a provider that was never
// asked and buries the step key.
func TestADivergenceIsNotReportedAsAProviderFailure(t *testing.T) {
	prompts := newPromptRegistry()
	prompts.set(&PromptTemplate{
		Name:            "probe",
		TemplateSource:  "x",
		tmpl:            template.Must(template.New("probe").Parse("x")),
		DefaultProvider: "mock",
	})
	providers := newProviderRegistry("")
	providers.setEntry(&ProviderConfigEntry{
		Config: ProviderConfig{Name: "mock", Type: "test"}, Client: &mockAIProvider{}, Available: true,
	})
	rt := newAIRuntime(nil, prompts, providers, aiCacheConfig{})
	rt.seam = &modelSeam{journal: newCountingJournal()}

	rc := common.RunContext{
		RunId: "run-2", GoalId: "g", StepKey: "the-step", Mode: common.RunModeReplay,
		ReplayPolicy: common.ReplayStrict, SourceRunId: "run-1", SourceGoalId: "g", OwnerUserId: "u1",
	}
	_, err := rt.Invoke(common.ContextWithRun(context.Background(), rc), &AIInvocation{TemplateId: "probe"}, map[string]any{})
	var diverged *DivergenceError
	if !errors.As(err, &diverged) {
		t.Fatalf("the ai() path did not surface a divergence: %v", err)
	}
	if diverged.StepKey != "the-step" {
		t.Errorf("the divergence lost its step key: %+v", diverged)
	}
}
