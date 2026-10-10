package compose

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/common"
)

func TestSectionsDBReadsOwnerReceiptsOnAnotherEngine(t *testing.T) {
	producer := New(materializeDBEngine(t), nil)
	consumer := New(materializeDBEngine(t), nil)
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	owner, runId := "section-owner-"+suffix, "section-run-"+suffix
	ctx := auth.ContextWithUserActor(context.Background(), owner)
	write := func(name string, args map[string]any) {
		t.Helper()
		if err := producer.store().writeInternal(ctx, "mutation "+call(name, args)); err != nil {
			t.Fatal(err)
		}
	}
	for n, key := range []string{"evidence", "chapterOne", "chapterTwo"} {
		stepID := suffix + "-" + key
		write("createWorkStep", map[string]any{"stepId": stepID, "runId": runId, "key": key, "seq": n, "stepType": "call", "status": "running", "attempt": 1})
		write("updateWorkStep", map[string]any{"stepId": stepID, "status": "done", "result": map[string]any{"value": map[string]any{"reply": "## " + key + "\n\nModel text"}}, "finishedAt": "2026-10-10T15:00:00Z"})
		write("createWorkModelCall", map[string]any{"modelCallId": stepID, "runId": runId, "stepKey": key, "requestHash": stepID, "provider": "local", "model": "recorded-model", "served": "live", "inputTokens": 100, "outputTokens": 20, "response": map[string]any{"text": "PRIVATE MODEL RESPONSE"}})
	}
	rc := common.RunContext{OwnerUserId: owner, RunId: runId, GoalId: "section-goal-" + suffix, StepKey: "assemble", Mode: common.RunModeLive}
	captured, err := consumer.captureSections(ctx, rc, []string{"chapterTwo", "chapterOne"})
	if err != nil {
		t.Fatal(err)
	}
	if captured.Body != "## chapterTwo\n\nModel text\n\n## chapterOne\n\nModel text" || len(captured.Models) != 2 || len(captured.Sources) != 2 {
		t.Fatalf("ordered persisted assembly lost: %+v", captured)
	}
	rows, err := consumer.store().query(ctx, "query "+call("workModelCallsForOwnerStep", map[string]any{"runId": runId, "stepKey": "chapterOne"}))
	if err != nil || len(rows) != 1 {
		t.Fatalf("model receipts: %v %v", rows, err)
	}
	if _, leaked := rows[0]["response"]; leaked {
		t.Fatal("lightweight contribution read returned a model response")
	}
	stranger := auth.ContextWithUserActor(context.Background(), owner+"-other")
	hidden, err := consumer.store().query(stranger, "query "+call("workModelCallsForOwnerStep", map[string]any{"runId": runId, "stepKey": "chapterOne"}))
	if err != nil || len(hidden) != 0 {
		t.Fatalf("model contributions leaked across owners: %v %v", hidden, err)
	}
	if _, err = consumer.captureSections(stranger, rc, []string{"chapterOne"}); err == nil {
		t.Fatal("section content leaked across owners")
	}
	u := &materializeUploader{}
	consumer.SetUploader(u, "files")
	out, err := consumer.materialize(common.ContextWithRun(ctx, rc), owner, "", sectionArgsForDB())
	if err != nil || !strings.Contains(string(u.bytes), "chapterOne") || strings.Contains(string(u.bytes), "evidence") {
		t.Fatalf("file did not preserve only selected chapters: %v %v", out, err)
	}
	if err == nil {
		row, readErr := consumer.store().compositionById(ctx, stringOf(out["compositionId"]))
		if readErr != nil || row["status"] != "ready" {
			t.Fatalf("missing ready receipt: %v %v", row, readErr)
		}
	}
}
func sectionArgsForDB() materializeArgs {
	a := materializeDraft()
	a.Draft = ""
	a.SectionKeys = []string{"chapterOne", "chapterTwo"}
	return a
}
