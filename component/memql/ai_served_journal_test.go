package memql

// ai_served_journal_test.go -- the work journal names the source that SERVED a
// call, and the decision behind it (synthesis2 fix 8).
//
// The router's wrapper walks the rest of the route when the resolution's pick
// fails at call time, so the source that answered can be the second one. Both
// covered seams used to journal the resolution's pick -- and the ai() runtime
// did not even call through the wrapper, it called the pick's raw registry
// client -- so a run's journal named, as having answered, a source that had
// not, and carried nothing about the route: not which sources were passed over
// and why, and not which decision record stands behind the call.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"text/template"

	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/common"
)

const plannerAppSkip = "app sources run on the agent holding the machine; this planner node cannot open one"

// routedStub stands in for the router's wrapped client: it answers every
// surface the two seams call and reports, like the router's wrappers do, which
// source the call ended on.
type routedStub struct {
	answer string
	served airoute.Served
	calls  int
}

func (s *routedStub) Call(context.Context, string) (any, error) {
	s.calls++
	return s.answer, nil
}
func (s *routedStub) CallChatStructured(context.Context, []common.ChatMessage, common.StructuredSchema) (string, error) {
	s.calls++
	return s.answer, nil
}
func (s *routedStub) LastServed() (airoute.Served, bool) { return s.served, true }

// The resolution's pick, and the source that actually served after it failed:
// the decision a planner node's triage makes today, with the app source ahead
// of both passed over.
func firstPick() airoute.Resolution {
	return airoute.Resolution{
		ProviderName: "fleet:qwen3.5:9b",
		Model:        "qwen3.5:9b",
		Decision: airoute.Decision{
			Level: airoute.LevelFast, RequestedLevel: airoute.LevelFast, ServedLevel: airoute.LevelFast,
			Rule: "fastLocalFirst", Policy: "fastLocalFirst", Door: airoute.DoorLocal, Outcome: airoute.OutcomeOK,
			Considered: []airoute.ConsideredEntry{
				{Entry: "app:claude-code", Door: airoute.DoorApp, Reason: plannerAppSkip},
				{Entry: "fleet:qwen3.5:9b", Door: airoute.DoorLocal, Reason: "selected"},
			},
		},
	}
}

func servedBySecond() airoute.Served {
	decision := firstPick().Decision
	decision.Considered = append(append([]airoute.ConsideredEntry(nil), decision.Considered...),
		airoute.ConsideredEntry{Entry: "fleet:qwen3.5:4b", Door: airoute.DoorLocal, Reason: "selected from fallback chain"})
	return airoute.Served{
		RouterCallId: "v1:router:call:served1",
		ProviderName: "fleet:qwen3.5:4b",
		Model:        "qwen3.5:4b",
		Decision:     decision,
	}
}

// assertServedRow checks one journal row names what served and why.
func assertServedRow(t *testing.T, row JournaledCall) {
	t.Helper()
	if row.Provider != "fleet:qwen3.5:4b" || row.Model != "qwen3.5:4b" {
		t.Errorf("journal row names %s / %s, want the source that SERVED (fleet:qwen3.5:4b), not the resolution's pick", row.Provider, row.Model)
	}
	if row.Served != "local" {
		t.Errorf("served=%q, want local: a local model answered", row.Served)
	}
	if row.RouterCallId != "v1:router:call:served1" {
		t.Errorf("routerCallId=%q, want the decision record behind the call", row.RouterCallId)
	}
	if row.Decision == nil {
		t.Fatal("the journal row carries no decision")
	}
	if row.Decision.Door != airoute.DoorLocal || row.Decision.Rule != "fastLocalFirst" {
		t.Errorf("decision door=%q rule=%q", row.Decision.Door, row.Decision.Rule)
	}
	var sawApp, sawServed bool
	for _, c := range row.Decision.Considered {
		switch c.Entry {
		case "app:claude-code":
			sawApp = c.Door == airoute.DoorApp && strings.Contains(c.Reason, plannerAppSkip)
		case "fleet:qwen3.5:4b":
			sawServed = c.Door == airoute.DoorLocal
		}
	}
	if !sawApp || !sawServed {
		t.Errorf("considered=%+v, want the app source passed over with its reason and the local model that served", row.Decision.Considered)
	}
}

func triageRun() common.RunContext {
	return common.RunContext{RunId: "triage-run", GoalId: "g", StepKey: "triage", OwnerUserId: "alice", Mode: common.RunModeLive}
}

// A STRUCTURED CALL journals the source that served, its router-call row and
// the decision -- and hands the same attribution back to its caller.
func TestAStructuredJournalRowNamesTheSourceThatServedAndItsDecision(t *testing.T) {
	routed := &routedStub{answer: `{"complexity":"trivial"}`, served: servedBySecond()}
	e := &MemQLEngine{providers: newProviderRegistry(), prompts: newPromptRegistry(), modelSeam: &modelSeam{}}
	e.SetAIResolver(testAIResolver{fn: func(context.Context, airoute.ResolveRequest) (ResolvedProvider, error) {
		return ResolvedProvider{Client: routed, Resolution: firstPick()}, nil
	}})
	journal := newCountingJournal()
	e.SetModelCallJournal(journal)

	ctx := common.ContextWithRun(userCtx("alice"), triageRun())
	out, err := e.CallAIStructured(ctx,
		airoute.ResolveRequest{Level: airoute.LevelFast, PromptName: "goalComplexityTriage"},
		[]common.ChatMessage{{Role: "user", Content: "classify: hi"}},
		common.StructuredSchema{Name: "triage", Schema: json.RawMessage(`{"type":"object"}`), Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.Resolution.ProviderName != "fleet:qwen3.5:4b" || out.Served != "local" {
		t.Errorf("result names %s (%s), want the source that served", out.Resolution.ProviderName, out.Served)
	}
	rows := journal.rows["triage-run"]
	if len(rows) != 1 {
		t.Fatalf("journal has %d rows, want 1", len(rows))
	}
	assertServedRow(t, rows[0])
}

// THE ai() PROMPT FORM calls the ROUTED client -- never the pick's raw registry
// client -- and journals and caches under the source that served.
func TestTheAIRuntimeCallsTheRoutedClientAndJournalsTheSourceThatServed(t *testing.T) {
	prompts := newPromptRegistry()
	prompts.set(&PromptTemplate{
		Level:          "fast",
		Name:           "probe",
		TemplateSource: "say {{.word}}",
		tmpl:           template.Must(template.New("probe").Parse("say {{.word}}")),
	})
	providers := newProviderRegistry()
	raw := &mockAIProvider{}
	pickEntry := &ProviderConfigEntry{
		Config:    ProviderConfig{Name: "fleet:qwen3.5:9b", Type: "fleet", Model: "qwen3.5:9b"},
		Client:    raw,
		Available: true,
	}
	routed := &routedStub{answer: "hello", served: servedBySecond()}
	rt := newAIRuntime(nil, prompts, providers, aiCacheConfig{MaxEntries: 16, MaxTTLSeconds: 60, DefaultEnabled: true})
	rt.resolve = func(context.Context, airoute.ResolveRequest) (ResolvedProvider, error) {
		return ResolvedProvider{Client: routed, Entry: pickEntry, Resolution: firstPick()}, nil
	}
	journal := newCountingJournal()
	rt.seam = &modelSeam{journal: journal}

	got, err := rt.Invoke(common.ContextWithRun(userCtx("alice"), triageRun()), &AIInvocation{TemplateId: "probe"}, map[string]any{"word": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello" || routed.calls != 1 {
		t.Fatalf("got %v after %d routed calls, want the routed client's answer", got, routed.calls)
	}
	if raw.calls != 0 {
		t.Fatalf("the pick's raw registry client was called %d times: the call bypassed the route", raw.calls)
	}
	rows := journal.rows["triage-run"]
	if len(rows) != 1 {
		t.Fatalf("journal has %d rows, want 1", len(rows))
	}
	assertServedRow(t, rows[0])

	// The cache key folds in the provider, and the answer is the serving
	// source's: keyed under the pick, a later hit would be recorded as the
	// pick's answer when the pick never gave one.
	if _, ok := rt.cache.get(buildAICacheKey("probe", "fleet:qwen3.5:4b", "say hello")); !ok {
		t.Error("the answer was not cached under the source that served it")
	}
	if _, ok := rt.cache.get(buildAICacheKey("probe", "fleet:qwen3.5:9b", "say hello")); ok {
		t.Error("the answer was cached under the resolution's pick, which failed and never answered")
	}
}
