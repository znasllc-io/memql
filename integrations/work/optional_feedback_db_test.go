package work

import (
	"context"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

func TestOptionalFeedbackDBTimeoutAndLateAnswerAcrossReplicas(t *testing.T) {
	db, bff, _, _ := compileDB(t)
	person, cancel := context.WithTimeout(actorCtx("compile-alice"), 30*time.Second)
	defer cancel()
	nodes, err := bff.handleCreateGoal(person, map[string]any{"statement": "Investigate the current situation", "ceilings": map[string]any{"maxModelCalls": 3}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	receipt := decodeReply(t, nodes)
	runID, goalID := receipt["runId"].(string), receipt["goalId"].(string)
	peers := []*Integration{}
	engines := []*memql.MemQLEngine{}
	now := time.Now().UTC().Add(-2 * time.Minute)
	for range 2 {
		pool := workHeadsPeer(t, db)
		engine := dispatchDBEngine(t, pool)
		peer := New(engine, testLogger(), func() *bun.DB { return pool })
		peer.SetGateDB(testGates(t))
		peer.SetNow(func() time.Time { return now })
		peer.rowsInFlight = func(ctx context.Context) ([]map[string]any, error) {
			row, err := peer.store().runForOwner(memql.ContextWithFreshRead(ctx), runID)
			return []map[string]any{row}, err
		}
		peers = append(peers, peer)
		engines = append(engines, engine)
	}
	if err := peers[0].store().updateRun(person, runID, map[string]any{"status": "running", "automationName": "test", "templateFingerprint": "optional-test", "spent": map[string]any{"modelCalls": 2, "tokens": 400}}); err != nil {
		t.Fatal(err)
	}
	before, err := peers[0].store().runForOwner(memql.ContextWithFreshRead(person), runID)
	if err != nil {
		t.Fatal(err)
	}
	if rowString(rowMap(before, "executionAuthority"), "roleCeiling") != "writer" {
		t.Fatalf("missing recorded authority: %v", before["executionAuthority"])
	}
	goalBefore, err := peers[0].store().goalForOwner(memql.ContextWithFreshRead(person), goalID)
	if err != nil || rowInt(rowMap(goalBefore, "ceilings"), "maxModelCalls") != 3 {
		t.Fatalf("missing goal budget: %v %v", goalBefore, err)
	}
	rc := common.RunContext{RunId: runID, GoalId: goalID, OwnerUserId: canonicalUser("compile-alice"), StepKey: "reason", Mode: common.RunModeLive, Spine: rowMap(before, "spine")}
	ctx := common.ContextWithRun(person, rc)
	saved := []common.ChatMessage{{Role: "system", Content: "Instructions"}, {Role: "assistant", ToolCalls: []common.ToolCall{{ID: "done", Name: "fetchUrl"}}}, {Role: "tool", ToolCallId: "done", Content: `{"result":"already collected"}`}}
	if err := engines[0].SaveWorkContinuation(ctx, saved); err != nil {
		t.Fatal(err)
	}
	id, err := peers[0].AskFeedbackWithPolicy(ctx, rc.OwnerUserId, runID, "Which region should I check?", "text", nil, time.Time{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if resumed, err := peers[1].continueOptionalFeedback(person, runID, now.Add(59*time.Second)); resumed || err != nil {
		t.Fatalf("early continuation: %v %v", resumed, err)
	}
	now = now.Add(61 * time.Second)
	var wg sync.WaitGroup
	resumed := make([]bool, 2)
	errs := make([]error, 2)
	for n, p := range peers {
		wg.Add(1)
		go func(n int, p *Integration) {
			defer wg.Done()
			result, err := p.SweepWaiting(person, time.Hour)
			resumed[n], errs[n] = result.Resumed == 1, err
		}(n, p)
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || resumed[0] == resumed[1] {
		t.Fatalf("timeout must continue once: %v %v", resumed, errs)
	}
	after, err := peers[1].store().runForOwner(memql.ContextWithFreshRead(person), runID)
	if err != nil || after["status"] != "running" || rowString(after, "humanResumeId") != memql.BareShortId(id) {
		t.Fatalf("lost continuation: %v %v", after, err)
	}
	for _, key := range []string{"spent", "executionAuthority", "routing", "rerun", "templateFingerprint"} {
		if !reflect.DeepEqual(before[key], after[key]) {
			t.Fatalf("timeout changed %s: %v => %v", key, before[key], after[key])
		}
	}
	goalAfter, err := peers[1].store().goalForOwner(memql.ContextWithFreshRead(person), goalID)
	if err != nil || !reflect.DeepEqual(goalBefore["ceilings"], goalAfter["ceilings"]) {
		t.Fatalf("timeout changed goal budget: %v %v", goalAfter, err)
	}
	proof, err := engines[1].PrepareWorkContinuation(ctx, memql.CheckpointResumeRequest{RunID: runID, StepKey: "reason", ApprovalID: id})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := engines[1].RestoreWorkContinuation(proof, nil)
	if err != nil || !reflect.DeepEqual(restored, saved) {
		t.Fatalf("lost completed effect: %+v %v", restored, err)
	}
	dispatchers := []*capturingDispatcher{{}, {}}
	for n, p := range peers {
		p.SetDispatcher(dispatchers[n])
		p.SetRunClaimer(automations.NewClusterExecutionGuard(func() *bun.DB { return db }, testLogger()).StrictClaimer())
	}
	req, ok := runEventFields(remedyEvent(merged(after, map[string]any{"id": runID})))
	if !ok {
		t.Fatal("missing dispatch")
	}
	for range 3 {
		for _, p := range peers {
			wg.Add(1)
			go func(p *Integration) { defer wg.Done(); p.dispatchRun(person, req) }(p)
		}
		wg.Wait()
	}
	if n := len(dispatchers[0].seen()) + len(dispatchers[1].seen()); n != 1 {
		t.Fatalf("dispatched %d times", n)
	}
	if same, err := peers[1].AskFeedbackWithPolicy(ctx, rc.OwnerUserId, runID, "Which region should I check?", "text", nil, time.Time{}, true); err != nil || same != id {
		t.Fatalf("asked again: %s %v", same, err)
	}
	answer := map[string]any{"approvalId": id, "decision": "answered", "answer": map[string]any{"text": "Use my saved office region"}}
	for range 2 {
		nodes, err := peers[0].handleDecideApproval(person, answer, 0)
		if err != nil || decodeReply(t, nodes)["runResumed"] != false {
			t.Fatalf("late answer restarted work: %v %v", nodes, err)
		}
	}
	guidance, err := engines[1].ReadWorkGuidance(ctx)
	if err != nil || len(guidance) != 1 || !strings.Contains(guidance[0].Prompt, "saved office region") {
		t.Fatalf("late answer lost at receiving replica: %+v %v", guidance, err)
	}
	if _, err := peers[1].handleDecideApproval(actorCtx("compile-bob"), answer, 0); err == nil {
		t.Fatal("another owner answered")
	}
	// A rerun must not inherit late input from an older execution attempt.
	if err := peers[0].store().updateRun(person, runID, map[string]any{"rerun": map[string]any{"requestId": "new-attempt"}}); err != nil {
		t.Fatal(err)
	}
	rc.RerunRequestId = "new-attempt"
	guidance, err = engines[1].ReadWorkGuidance(common.ContextWithRun(person, rc))
	if err != nil || len(guidance) != 0 {
		t.Fatalf("stale answer crossed rerun: %+v %v", guidance, err)
	}
}

func TestOptionalFeedbackDBRequiredAndCancelledWaitsDoNotContinue(t *testing.T) {
	_, i, _, _ := compileDB(t)
	i.SetGateDB(testGates(t))
	for _, mode := range []string{"required", "cancelled", "missing checkpoint", "changed template"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now().UTC().Add(-2 * time.Minute)
			i.SetNow(func() time.Time { return now })
			person := actorCtx("compile-alice")
			nodes, err := i.handleCreateGoal(person, map[string]any{"statement": "Investigate a situation"}, 0)
			if err != nil {
				t.Fatal(err)
			}
			result := decodeReply(t, nodes)
			runID := result["runId"].(string)
			if err := i.store().updateRun(person, runID, map[string]any{"status": "running", "templateFingerprint": "original"}); err != nil {
				t.Fatal(err)
			}
			ctx := common.ContextWithRun(person, common.RunContext{RunId: runID, GoalId: result["goalId"].(string), OwnerUserId: canonicalUser("compile-alice"), StepKey: "reason", Mode: common.RunModeLive})
			_, err = i.AskFeedbackWithPolicy(ctx, canonicalUser("compile-alice"), runID, "Which resource?", "text", nil, now.Add(time.Second), mode != "required")
			if err != nil {
				t.Fatal(err)
			}
			if mode == "cancelled" {
				err = i.store().updateRun(person, runID, map[string]any{"cancelRequested": true})
			}
			if mode == "changed template" {
				err = i.store().updateRun(person, runID, map[string]any{"templateFingerprint": "changed"})
			}
			if err != nil {
				t.Fatal(err)
			}
			resumed, err := i.continueOptionalFeedback(person, runID, now.Add(2*time.Minute))
			if resumed || (err != nil && mode != "missing checkpoint") {
				t.Fatalf("%s continued: %v %v", mode, resumed, err)
			}
			if mode == "missing checkpoint" && err == nil {
				t.Fatal("missing checkpoint not reported")
			}
			row, err := i.store().runForOwner(memql.ContextWithFreshRead(person), runID)
			if err != nil || row["status"] != "waiting" {
				t.Fatalf("wait changed: %v %v", row, err)
			}
		})
	}
}
