package work

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/work/goaleval"
)

func TestGoalEvaluationPublicationIsOwnedAndImmutableAcrossReplicas(t *testing.T) {
	a := openActsDB(t)
	routing := map[string]any{"source": "fleet:qwen3.8:27b", "by": a.owner, "noPaidProviders": true}
	runID := a.openRun(t, map[string]any{"status": "cancelled", "routing": routing})
	run := a.query(t, a.owner, "query "+call("workRunForOwner", map[string]any{"runId": runID}))[0]
	catalog, err := goaleval.Load(os.DirFS("../.."), "test/proving/goals/catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	scenario := catalog.Scenarios[0]
	missing := goaleval.Measure{Basis: "unavailable", Source: "blocked test fixture made no model call"}
	result := goaleval.Result{SchemaVersion: 1, ID: "evaluation-owned", ScenarioID: scenario.ID, ScenarioVersion: scenario.Version, ScenarioFingerprint: scenario.Fingerprint(), FixtureVersion: scenario.Fixture.Version, EngineCommit: strings.Repeat("e", 40), HarnessCommit: strings.Repeat("f", 40), GoalID: argString(run, "goalId"), RunID: runID, Outcome: "blocked", StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC(), InputTokens: missing, OutputTokens: missing, CostUSD: missing, Artifacts: []string{"fixture:blocked-diagnostic"}, Setup: goaleval.Setup{Name: "Local baseline", Routes: []goaleval.Route{{Role: "reasoning", Level: "reasoning", Rule: "owner pin", Doors: []goaleval.Door{{Source: "fleet", Model: "qwen3.8:27b", Configuration: map[string]any{"noPaidProviders": true}}}}}}}
	result.Setup.RoutingDigest = result.Setup.Fingerprint()
	object := func(v any) map[string]any {
		t.Helper()
		b, e := json.Marshal(v)
		if e != nil {
			t.Fatal(e)
		}
		var m map[string]any
		if e = json.Unmarshal(b, &m); e != nil {
			t.Fatal(e)
		}
		return m
	}
	args := map[string]any{"result": object(result), "scenario": object(scenario)}
	first := New(a.eng, testLogger())
	first.SetGateDB(testGates(t))
	second := New(openWorkTestEngine(t), testLogger())
	second.SetGateDB(testGates(t))
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, replica := range []*Integration{first, second} {
		wg.Go(func() { _, e := replica.handleRecordGoalEvaluation(actorCtx(a.owner), args, 0); errs <- e })
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	rows := a.query(t, a.owner, "query "+call("workObservationsForOwnerRun", map[string]any{"runId": runID}))
	if len(rows) != 1 {
		t.Fatalf("replicas published %d records", len(rows))
	}
	if _, err := second.handleRecordGoalEvaluation(actorCtx("another-owner"), args, 0); err == nil {
		t.Fatal("foreign run accepted")
	}
	result.Artifacts = []string{"fixture:different-claim"}
	args["result"] = object(result)
	if _, err := second.handleRecordGoalEvaluation(actorCtx(a.owner), args, 0); err == nil {
		t.Fatal("published evidence was rewritten")
	}
}
