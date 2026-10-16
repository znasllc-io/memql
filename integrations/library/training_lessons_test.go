package library

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work/goaleval"
	"github.com/znasllc-io/memql/core/common"
)

func manualLessonDraft() map[string]any {
	return map[string]any{
		"title": "Identify the refused gate", "text": "After a worker refusal, identify whether task permission or worker policy blocked the operation before requesting broader access.", "scope": "owner",
		"applies": "A tool returned an explicit permission or policy refusal.", "exceptions": "If an optional diagnostic is unnecessary, continue without it. Do not probe hidden policy or bypass a denial.",
		"observed": "A granted task permission was followed by a separate worker-policy refusal; recovery was assisted.", "alternative": "Read the refusal and use an already permitted alternative, or name the operator change needed.", "principle": "Task permission and worker capability are separate gates.", "factConfidence": "High for the recorded refusal; cause beyond the receipt is unknown.", "lessonConfidence": "Candidate; comparative benefit unmeasured.", "scenario": "tool-unavailable", "counterexample": "implement-and-check",
	}
}
func TestManualLessonDecisionsAcrossReplicas(t *testing.T) {
	f := newRevisionDB(t)
	run := f.owner + "lesson-run"
	goal := f.owner + "lesson-goal"
	f.query(f.engine, auth.ContextWithInternalOrigin(f.ctx), "mutation", "createWorkGoal", map[string]any{"goalId": goal, "statement": "Inspect the refusal and continue correctly", "origin": "user"})
	f.query(f.engine, auth.ContextWithInternalOrigin(f.ctx), "mutation", "createWorkRun", map[string]any{"runId": run, "goalId": goal, "automationName": "fixture", "templateFingerprint": "fixture", "status": "succeeded", "mode": "live", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	source := func(context.Context, string, string) (map[string]any, error) {
		return map[string]any{"runId": run, "conversationId": "fixture-conversation", "turnId": "guidance", "quote": "Check which gate refused the action.", "status": "applied"}, nil
	}
	f.first.lessonSource = source
	f.second.lessonSource = source
	args := map[string]any{"action": "propose", "requestId": "manual-proposal", "conversationId": "fixture-conversation", "turnId": "guidance", "draft": manualLessonDraft()}
	save := func(lib *Integration, args map[string]any) trainingLesson {
		rows, err := lib.handleSaveTrainingLesson(f.ctx, args, 0)
		require.NoError(t, err)
		var l trainingLesson
		require.NoError(t, json.Unmarshal(rows[0].Payload, &l))
		return l
	}
	first := save(f.first, args)
	retry := save(f.second, args)
	require.Equal(t, first.Receipt, retry.Receipt)
	require.False(t, first.Enabled)
	require.Equal(t, "candidate", first.Review)
	events, err := f.second.lessonEvents(f.ctx, first.ID)
	require.NoError(t, err)
	require.Len(t, events, 1)
	// Concurrent decisions read the same head. The database lock admits only one.
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for n, lib := range []*Integration{f.first, f.second} {
		wg.Add(1)
		go func(n int, lib *Integration) {
			defer wg.Done()
			_, err := lib.handleSaveTrainingLesson(f.ctx, map[string]any{"action": []string{"review", "reject"}[n], "requestId": []string{"review-on-node-a", "reject-on-node-b"}[n], "lessonId": first.ID, "expectedReceipt": first.Receipt, "reason": "Manual review of the exact scoped candidate"}, 0)
			errs <- err
		}(n, lib)
	}
	wg.Wait()
	close(errs)
	success := 0
	for err := range errs {
		if err == nil {
			success++
		}
	}
	require.Equal(t, 1, success)
	events, err = f.first.lessonEvents(f.ctx, first.ID)
	require.NoError(t, err)
	require.Len(t, events, 2)
	head := events[0]
	edited := manualLessonDraft()
	edited["exceptions"] = "An optional diagnostic may be skipped. A refusal never permits bypass."
	second := save(f.second, map[string]any{"action": "edit", "requestId": "manual-edit", "lessonId": first.ID, "expectedReceipt": head.Receipt, "draft": edited})
	require.Equal(t, 2, second.Version)
	require.Equal(t, "candidate", second.Review)
	require.False(t, second.Enabled)
	require.NotEqual(t, first.Digest, second.Digest)
	_, err = f.first.handleSaveTrainingLesson(f.ctx, map[string]any{"action": "review", "requestId": "stale-review", "lessonId": first.ID, "expectedReceipt": first.Receipt, "reason": "old revision"}, 0)
	require.ErrorContains(t, err, "changed")
	reviewed := save(f.first, map[string]any{"action": "review", "requestId": "current-review", "lessonId": first.ID, "expectedReceipt": second.Receipt, "reason": "Content reviewed; benefit is not measured"})
	_, err = f.second.handleSaveTrainingLesson(f.ctx, map[string]any{"action": "enable", "requestId": "premature-enable", "lessonId": first.ID, "expectedReceipt": reviewed.Receipt, "evaluationIds": []string{}, "reason": "Test"}, 0)
	require.ErrorContains(t, err, "two distinct")
	foreign := auth.ContextWithInternalOrigin(revisionActor(f.owner+"foreign", auth.RoleOwner))
	hidden, err := f.second.lessonEvents(foreign, first.ID)
	require.NoError(t, err)
	require.Empty(t, hidden)
	_, err = f.second.handleSaveTrainingLesson(foreign, map[string]any{"action": "review", "requestId": "foreign-review", "lessonId": first.ID, "expectedReceipt": reviewed.Receipt, "reason": "forged"}, 0)
	require.Error(t, err)
	_, err = f.second.handleSaveTrainingLesson(common.ContextWithRun(f.ctx, common.RunContext{RunId: run}), args, 0)
	require.ErrorContains(t, err, "work run")
	// Synthetic result fixtures isolate the review gate. The evaluator publisher
	// and provider delivery join have their own tests; these are not live outcomes.
	catalog, err := goaleval.Load(os.DirFS("../.."), "test/proving/goals/catalog.json")
	require.NoError(t, err)
	refs := []string{}
	priorEvaluation := f.owner + "prior-version-evaluation"
	for n, scenarioID := range []string{reviewed.Draft.Scenario, reviewed.Draft.Counterexample} {
		var scenario goaleval.Scenario
		for _, s := range catalog.Scenarios {
			if s.ID == scenarioID {
				scenario = s
			}
		}
		unknown := goaleval.Measure{Basis: "unavailable", Source: "Synthetic review-gate fixture"}
		r := goaleval.Result{SchemaVersion: 1, ID: fmt.Sprintf("synthetic-%d", n), ScenarioID: scenario.ID, ScenarioVersion: scenario.Version, ScenarioFingerprint: scenario.Fingerprint(), FixtureVersion: scenario.Fixture.Version, EngineCommit: strings.Repeat("e", 40), HarnessCommit: strings.Repeat("f", 40), GoalID: goal, RunID: run, Outcome: "autonomous", StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC(), InputTokens: unknown, OutputTokens: unknown, CostUSD: unknown, Artifacts: []string{"fixture:synthetic-oracle"}, Setup: goaleval.Setup{Name: "Synthetic", Routes: []goaleval.Route{{Role: "reasoning", Level: "reasoning", Rule: "fixture", Doors: []goaleval.Door{{Source: "fleet", Model: "fixture", Configuration: map[string]any{}}}}}}, Bindings: []goaleval.Binding{{ID: reviewed.ID, Version: "2", Digest: reviewed.Digest, Source: "observation:synthetic-delivery"}}, ModelsUsed: []goaleval.ModelUse{{Role: "reasoning", Level: "reasoning", Source: "fleet", Model: "fixture", Calls: 1, Evidence: []string{"fixture:synthetic-call"}, InputTokens: unknown, OutputTokens: unknown, CostUSD: unknown}}}
		r.Setup.RoutingDigest = r.Setup.Fingerprint()
		for _, c := range scenario.Criteria {
			r.Assertions = append(r.Assertions, goaleval.Assertion{CriterionID: c.ID, Verdict: "pass", Evidence: []string{"fixture:synthetic-oracle"}})
		}
		require.NoError(t, r.Validate(scenario))
		ref := f.owner + fmt.Sprintf("evaluation-%d", n)
		refs = append(refs, ref)
		f.query(f.engine, memql.ContextWithGoalEvaluationWrite(auth.ContextWithInternalOrigin(f.ctx)), "mutation", "createWorkObservation", map[string]any{"observationId": ref, "runId": run, "kind": "decision", "content": "Synthetic evaluation gate fixture", "data": map[string]any{"oracleAuthority": "external-harness", "goalEvaluation": r, "scenario": scenario}})
		if n == 0 {
			old := r
			old.ID = "prior-content-version"
			old.Bindings = []goaleval.Binding{{ID: first.ID, Version: "1", Digest: first.Digest, Source: "observation:synthetic-prior-delivery"}}
			f.query(f.engine, memql.ContextWithGoalEvaluationWrite(auth.ContextWithInternalOrigin(f.ctx)), "mutation", "createWorkObservation", map[string]any{"observationId": priorEvaluation, "runId": run, "kind": "decision", "content": "Synthetic prior-version evaluation", "data": map[string]any{"oracleAuthority": "external-harness", "goalEvaluation": old, "scenario": scenario}})
		}
	}
	_, err = f.second.handleSaveTrainingLesson(f.ctx, map[string]any{"action": "enable", "requestId": "wrong-revision-enable", "lessonId": first.ID, "expectedReceipt": reviewed.Receipt, "reason": "Try prior revision evidence", "evaluationIds": []string{priorEvaluation, refs[1]}}, 0)
	require.ErrorContains(t, err, "exact lesson revision")
	enabled := save(f.first, map[string]any{"action": "enable", "requestId": "manual-enable", "lessonId": first.ID, "expectedReceipt": reviewed.Receipt, "reason": "Two synthetic gate fixtures passed", "evaluationIds": refs})
	require.True(t, enabled.Enabled)
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { db.Close() })
	provider := &trainingVector{}
	previous, had := memql.ActiveEmbedderBinding()
	memql.SetActiveEmbedderBinding(memql.EmbedderBinding{ProviderRef: f.owner, Dimensions: 1024})
	t.Cleanup(func() {
		if had {
			memql.SetActiveEmbedderBinding(previous)
		} else {
			memql.ClearActiveEmbedderBinding()
		}
	})
	table, err := memql.EnsureEmbeddingVectorTable(f.ctx, db.DB, f.owner, 1024)
	require.NoError(t, err)
	t.Cleanup(func() {
		db.ExecContext(context.Background(), "DROP TABLE "+table)
		db.ExecContext(context.Background(), `DELETE FROM embedding_cache WHERE provider=?`, f.owner)
	})
	for n, lib := range []*Integration{f.first, f.second} {
		e := []*memql.MemQLEngine{f.engine, f.other}[n]
		require.NoError(t, lib.setTrainingReader(memql.PluginContext{Logger: e.Logger, BunDB: func() *bun.DB { return db }, ConceptDataIsStaged: e.ConceptDataIsStaged, AdmitSourceRow: memql.AdmitSourceRow, ResolveEmbeddingProvider: func(context.Context, string) (memql.EmbeddingAIProvider, error) { return provider, nil }}))
	}
	_, err = f.first.handleIndexTrainingLesson(f.ctx, map[string]any{"lessonId": first.ID, "expectedReceipt": enabled.Receipt}, 0)
	require.NoError(t, err)
	runtime := common.ContextWithRun(auth.ContextWithInternalOrigin(f.ctx), common.RunContext{RunId: run, GoalId: goal, OwnerUserId: f.owner, StepKey: "inspect", Mode: common.RunModeLive})
	search := func() string {
		rows, err := f.second.handleOwnedTrainingLessons(runtime, map[string]any{"text": "Inspect the refusal", "limit": 3, "minimumSimilarity": 0.6, "maxTokens": 1200}, 0)
		require.NoError(t, err)
		b, _ := json.Marshal(rows)
		return string(b)
	}
	require.Contains(t, search(), "manual-coaching")
	retired := save(f.second, map[string]any{"action": "retire", "requestId": "manual-retire", "lessonId": first.ID, "expectedReceipt": enabled.Receipt, "reason": "Keep this candidate for history only"})
	require.Equal(t, "retired", retired.Review)
	require.False(t, retired.Enabled)
	require.NotContains(t, search(), "manual-coaching", "retirement on another replica withholds the indexed older revision")
	// An unpublished revision is visible only to an explicitly pinned evaluation.
	candidate := save(f.first, map[string]any{"action": "edit", "requestId": "candidate-for-evaluation", "lessonId": first.ID, "expectedReceipt": retired.Receipt, "draft": edited})
	require.False(t, candidate.Enabled)
	require.NotContains(t, search(), "manual-coaching")
	evalGoal, evalRun := goal+"-eval", run+"-eval"
	f.query(f.engine, auth.ContextWithInternalOrigin(f.ctx), "mutation", "createWorkGoal", map[string]any{"goalId": evalGoal, "statement": "Evaluate a pinned candidate", "origin": "user", "input": map[string]any{"lessonEvaluation": map[string]any{"lessonId": candidate.ID, "receipt": candidate.Receipt, "scenarioId": candidate.Draft.Scenario, "fixtureVersion": "1.0.0"}}})
	f.query(f.engine, auth.ContextWithInternalOrigin(f.ctx), "mutation", "createWorkRun", map[string]any{"runId": evalRun, "goalId": evalGoal, "automationName": "fixture", "templateFingerprint": "fixture", "status": "running", "mode": "live", "startedAt": time.Now().UTC().Format(time.RFC3339Nano), "routing": map[string]any{"source": "fleet:fixture", "noPaidProviders": true}})
	evalContext := common.ContextWithRun(auth.ContextWithInternalOrigin(f.ctx), common.RunContext{RunId: evalRun, GoalId: evalGoal, OwnerUserId: f.owner, StepKey: "inspect", Mode: common.RunModeLive})
	rows, err := f.second.handleOwnedTrainingLessons(evalContext, map[string]any{"text": "Inspect the refusal", "limit": 3, "minimumSimilarity": 0.6, "maxTokens": 1200}, 0)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	var passage map[string]any
	require.NoError(t, json.Unmarshal(rows[0].Payload, &passage))
	require.Equal(t, "manual-coaching-evaluation", passage["source"])
	require.Equal(t, candidate.Digest, passage["lessonDigest"])
	require.NotEmpty(t, passage["selectionReceiptId"])
	selection, err := f.first.lessonRows(f.ctx, "workTrainingEvidenceById", map[string]any{"observationId": passage["selectionReceiptId"]})
	require.NoError(t, err)
	require.Len(t, selection, 1)
	selectionData := selection[0]["data"].(map[string]any)["trainingSelection"].(map[string]any)
	require.Equal(t, "evaluation-only", selectionData["status"])
	require.NotContains(t, search(), "manual-coaching", "evaluation must not enable ordinary retrieval")
	save(f.first, map[string]any{"action": "reject", "requestId": "withdraw-evaluation-candidate", "lessonId": candidate.ID, "expectedReceipt": candidate.Receipt, "reason": "Withdraw the candidate"})
	_, err = f.second.handleOwnedTrainingLessons(evalContext, map[string]any{"text": "Inspect the refusal", "limit": 3, "minimumSimilarity": 0.6, "maxTokens": 1200}, 0)
	require.ErrorContains(t, err, "changed or was withdrawn")
	// The exact stored event cannot be rewritten through the ordinary mutation.
	_, err = f.engine.Execute(auth.ContextWithInternalOrigin(f.ctx), `mutation createWorkObservation(observationId: "`+first.Receipt+`", runId: "`+run+`", kind: "decision", content: "forged", data: {trainingLesson: {enabled: true}})`)
	require.ErrorContains(t, err, "immutable")
}
