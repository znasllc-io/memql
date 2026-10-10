package work

import (
	"context"
	"encoding/json"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/common"
	"strings"
	"testing"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/safety"
	work "github.com/znasllc-io/memql/component/work"
)

// decodeReply unwraps a capability's single reply node.
func decodeReply(t *testing.T, nodes []memorynodes.MemoryNode) map[string]any {
	t.Helper()
	if len(nodes) != 1 {
		t.Fatalf("expected exactly 1 reply node, got %d", len(nodes))
	}
	var out map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &out); err != nil {
		t.Fatalf("reply payload did not decode: %v", err)
	}
	return out
}

// TestDecideApprovalHonoursTheArtifactHash is the gate on the guarantee that
// gives v1:work:approval its meaning: an approval is a decision about a
// SPECIFIC thing, and it never carries to a modified one.
//
// Table-driven over the kinds, because the recompute is deliberately
// conditional and the condition is the part a reader gets wrong: a
// subject-derived hash is recomputed, and an externally-supplied one
// (sideEffect's correlation key) is passed through, with the modified-artifact
// protection living at the next dispatch instead.
func TestDecideApprovalHonoursTheArtifactHash(t *testing.T) {
	approvedSubject := map[string]any{"patches": []any{map[string]any{"op": "relativize", "path": "/tmp/x"}}}
	changedSubject := map[string]any{"patches": []any{map[string]any{"op": "relativize", "path": "/tmp/DIFFERENT"}}}

	cases := []struct {
		name         string
		kind         string
		subject      map[string]any
		storedHash   string
		decision     string
		wantErr      bool
		wantErrIs    error
		wantDecision bool // a decideWorkApproval call was made
	}{
		{
			name:         "derived hash still matches -- approve",
			kind:         work.ApprovalKindPlanReview,
			subject:      approvedSubject,
			storedHash:   artifactHashOf(approvedSubject),
			decision:     "approved",
			wantDecision: true,
		},
		{
			name:       "derived hash no longer matches -- REFUSED",
			kind:       work.ApprovalKindPlanReview,
			subject:    changedSubject,
			storedHash: artifactHashOf(approvedSubject),
			decision:   "approved",
			wantErr:    true,
			wantErrIs:  work.ErrArtifactChanged,
		},
		{
			name:         "sideEffect carries a correlation key, not a subject digest -- passes through",
			kind:         work.ApprovalKindSideEffect,
			subject:      map[string]any{"command": "rm -rf /tmp/scratch"},
			storedHash:   "a-correlation-key-that-is-not-a-subject-digest",
			decision:     "approved",
			wantDecision: true,
		},
		{
			name:         "a rejection is recorded, not refused",
			kind:         work.ApprovalKindPlanReview,
			subject:      approvedSubject,
			storedHash:   artifactHashOf(approvedSubject),
			decision:     "rejected",
			wantDecision: true,
		},
		{
			name:       "an unknown decision is refused before anything is written",
			kind:       work.ApprovalKindPlanReview,
			subject:    approvedSubject,
			storedHash: artifactHashOf(approvedSubject),
			decision:   "maybe",
			wantErr:    true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i, eng := newTestIntegration(t)
			ctx := callerContext("u-alice")
			eng.reply("workApprovalsForOwner", map[string]any{
				"id": "v1:work:approval:a1", "runId": "v1:work:run:r1", "ownerUserId": "u-alice",
				"kind": tc.kind, "subject": tc.subject, "artifactHash": tc.storedHash,
			})
			eng.reply("workRunForOwner", map[string]any{
				"id": "v1:work:run:r1", "ownerUserId": "u-alice", "status": runStatusWaiting,
				"waitingOn": map[string]any{"kind": "approval", "subject": "v1:work:approval:a1"},
			})

			_, err := i.handleDecideApproval(ctx, map[string]any{
				"approvalId": "v1:work:approval:a1", "decision": tc.decision,
			}, 0)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected a refusal, got none")
				}
				if tc.wantErrIs != nil && !strings.Contains(err.Error(), tc.wantErrIs.Error()) {
					t.Errorf("error %q does not carry the typed reason %q -- a caller cannot tell 'changed' from 'already rejected'", err, tc.wantErrIs)
				}
				if n := len(eng.callsTo("decideWorkApproval")); n != 0 {
					t.Errorf("a decision was recorded despite the refusal (%d calls)", n)
				}
				return
			}
			if err != nil {
				t.Fatalf("decideApproval: %v", err)
			}
			if got := len(eng.callsTo("decideWorkApproval")); (got == 1) != tc.wantDecision {
				t.Errorf("decideWorkApproval called %d times, wantDecision=%v", got, tc.wantDecision)
			}
		})
	}
}

// TestDecideApprovalResumesOrStopsTheRun.
//
// A rejected approval must not leave the run parked: it has no timer, so the
// timer sweep never resumes it, and the abandoned sweep deliberately leaves
// waiting runs alone. It would wait forever.
func TestDecideApprovalResumesOrStopsTheRun(t *testing.T) {
	subject := map[string]any{"question": "which invoice?"}
	cases := []struct {
		decision   string
		answer     map[string]any
		wantStatus string
		wantCode   string
	}{
		{decision: "approved", wantStatus: runStatusRunning},
		{decision: "answered", answer: map[string]any{"pick": "INV-3"}, wantStatus: runStatusRunning},
		{decision: "rejected", wantStatus: runStatusFailed, wantCode: "approval_rejected"},
	}
	for _, tc := range cases {
		t.Run(tc.decision, func(t *testing.T) {
			i, eng := newTestIntegration(t)
			eng.reply("workApprovalsForOwner", map[string]any{
				"id": "v1:work:approval:a1", "runId": "v1:work:run:r1", "ownerUserId": "u-alice",
				"kind": work.ApprovalKindFeedback, "subject": subject, "artifactHash": artifactHashOf(subject),
			})
			eng.reply("workRunForOwner", map[string]any{
				"id": "v1:work:run:r1", "ownerUserId": "u-alice", "status": runStatusWaiting,
				"waitingOn": map[string]any{"kind": "approval", "subject": "v1:work:approval:a1"},
			})
			args := map[string]any{"approvalId": "v1:work:approval:a1", "decision": tc.decision}
			if tc.answer != nil {
				args["answer"] = tc.answer
			}
			nodes, err := i.handleDecideApproval(callerContext("u-alice"), args, 0)
			if err != nil {
				t.Fatalf("decideApproval: %v", err)
			}
			update := eng.callTo(t, "updateWorkRun").Args(t)
			if update["status"] != tc.wantStatus {
				t.Errorf("run status = %v, want %v", update["status"], tc.wantStatus)
			}
			if tc.wantCode != "" && update["errorCode"] != tc.wantCode {
				t.Errorf("errorCode = %v, want %v", update["errorCode"], tc.wantCode)
			}
			// waitingOn must be written as an EMPTY OBJECT, not omitted:
			// updateWorkRun is a read-merge, so an omitted argument keeps the
			// stale wait and the run reads as parked while running.
			wait, present := update["waitingOn"]
			if !present {
				t.Fatal("waitingOn was omitted; the read-merge would keep the stale wait")
			}
			if m, ok := wait.(map[string]any); !ok || len(m) != 0 {
				t.Errorf("waitingOn = %v, want an empty object", wait)
			}
			if reply := decodeReply(t, nodes); reply["runResumed"] != true {
				t.Errorf("runResumed = %v", reply["runResumed"])
			}
		})
	}
}

// TestDecideApprovalLeavesARunParkedOnSomethingElseAlone. A stale decision
// must not un-park a run that has since moved on to a different wait.
func TestDecideApprovalLeavesARunParkedOnSomethingElseAlone(t *testing.T) {
	subject := map[string]any{"q": "x"}
	i, eng := newTestIntegration(t)
	eng.reply("workApprovalsForOwner", map[string]any{
		"id": "v1:work:approval:a1", "runId": "v1:work:run:r1", "ownerUserId": "u-alice",
		"kind": work.ApprovalKindFeedback, "subject": subject, "artifactHash": artifactHashOf(subject),
	})
	eng.reply("workRunForOwner", map[string]any{
		"id": "v1:work:run:r1", "ownerUserId": "u-alice", "status": runStatusWaiting,
		"waitingOn": map[string]any{"kind": "approval", "subject": "v1:work:approval:SOMETHING-ELSE"},
	})
	nodes, err := i.handleDecideApproval(callerContext("u-alice"), map[string]any{
		"approvalId": "v1:work:approval:a1", "decision": "approved",
	}, 0)
	if err != nil {
		t.Fatalf("decideApproval: %v", err)
	}
	if n := len(eng.callsTo("updateWorkRun")); n != 0 {
		t.Errorf("the run was un-parked from a wait it was not on (%d updates)", n)
	}
	if n := len(eng.callsTo("decideWorkApproval")); n != 1 {
		t.Errorf("the decision itself should still be recorded, got %d calls", n)
	}
	if reply := decodeReply(t, nodes); reply["runResumed"] != false {
		t.Errorf("runResumed = %v, want false", reply["runResumed"])
	}
}

// THE FAILURE PATH'S QUESTIONS ARE DECIDABLE (memql#5664). The row the failure
// path raises -- built by component/work's FailureApproval, whose subject and
// hash are one map -- is fed to the decide handler as the graph returns it,
// JSON round trip included. Approve and both answers land; Abandon stops the
// run rather than resuming it; and a subject edited since the question was
// asked is still refused, so the fix did not buy decidability by dropping the
// guarantee. component/automations' TestAFailureApprovalHashesTheSubjectItStores
// pins that the failure path writes exactly this shape.
func TestDecideApprovalLandsOnAFailurePathQuestion(t *testing.T) {
	ev := work.Evidence{Tier: "escalate", Reason: "the work no longer fits", RuleId: work.RuleIdContextExhausted, Source: work.EvidenceSourceRules}
	stored := func(t *testing.T, kind string, edit func(map[string]any)) map[string]any {
		t.Helper()
		req := work.FailureApproval(kind, "v1:work:run:r1", "draft", work.SymptomHuman, "prompt is too long", "q", ev, testNow, time.Hour)
		raw, err := json.Marshal(map[string]any{
			"id": "v1:work:approval:a1", "runId": req.RunId, "ownerUserId": "u-alice", "stepKey": req.StepKey,
			"kind": req.Kind, "subject": req.Subject, "artifactHash": req.ArtifactHash,
			"question": req.Question, "options": req.Options,
		})
		if err != nil {
			t.Fatal(err)
		}
		var row map[string]any
		if err := json.Unmarshal(raw, &row); err != nil {
			t.Fatal(err)
		}
		if edit != nil {
			edit(row["subject"].(map[string]any))
		}
		return row
	}
	for _, tc := range []struct {
		name       string
		kind       string
		decision   string
		answer     map[string]any
		edit       func(map[string]any)
		wantStatus string
		wantErrIs  error
	}{
		{name: "approved", kind: work.ApprovalKindFeedback, decision: "approved", wantStatus: runStatusRunning},
		{name: "answered Retry", kind: work.ApprovalKindFeedback, decision: "answered", answer: map[string]any{"value": work.FailureAnswerRetry, "label": "Retry"}, wantStatus: runStatusRunning},
		{name: "answered Abandon stops the run", kind: work.ApprovalKindFeedback, decision: "answered", answer: map[string]any{"value": work.FailureAnswerAbandon, "label": "Abandon"}, wantStatus: runStatusFailed},
		{name: "a planReview approved", kind: work.ApprovalKindPlanReview, decision: "approved", wantStatus: runStatusRunning},
		{name: "a budget question approved", kind: work.ApprovalKindBudget, decision: "approved", wantStatus: runStatusRunning},
		{name: "rejected", kind: work.ApprovalKindFeedback, decision: "rejected", wantStatus: runStatusFailed},
		{name: "the failure changed since -- REFUSED", kind: work.ApprovalKindFeedback, decision: "approved",
			edit: func(s map[string]any) { s["errorMessage"] = "a different failure" }, wantErrIs: work.ErrArtifactChanged},
		{name: "answered over a changed failure -- REFUSED", kind: work.ApprovalKindFeedback, decision: "answered",
			answer: map[string]any{"value": work.FailureAnswerRetry}, edit: func(s map[string]any) { s["symptom"] = "transient" }, wantErrIs: work.ErrArtifactChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i, eng := newTestIntegration(t)
			eng.reply("workApprovalsForOwner", stored(t, tc.kind, tc.edit))
			eng.reply("workRunForOwner", map[string]any{
				"id": "v1:work:run:r1", "ownerUserId": "u-alice", "status": runStatusWaiting,
				"waitingOn": map[string]any{"kind": "approval", "subject": "v1:work:approval:a1", "approvalKind": tc.kind},
			})
			args := map[string]any{"approvalId": "v1:work:approval:a1", "decision": tc.decision}
			if tc.answer != nil {
				args["answer"] = tc.answer
			}
			nodes, err := i.handleDecideApproval(callerContext("u-alice"), args, 0)
			if tc.wantErrIs != nil {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrIs.Error()) {
					t.Fatalf("err = %v, want %q: a decision must never carry to a failure it was not about", err, tc.wantErrIs)
				}
				if n := len(eng.callsTo("decideWorkApproval")); n != 0 {
					t.Fatalf("the refused decision was recorded anyway (%d calls)", n)
				}
				return
			}
			if err != nil {
				t.Fatalf("decideApproval refused a decision on the failure path's own question: %v", err)
			}
			if n := len(eng.callsTo("decideWorkApproval")); n != 1 {
				t.Fatalf("decideWorkApproval called %d times, want 1", n)
			}
			update := eng.callTo(t, "updateWorkRun").Args(t)
			if update["status"] != tc.wantStatus {
				t.Fatalf("run status = %v, want %v", update["status"], tc.wantStatus)
			}
			if tc.wantStatus == runStatusFailed && update["errorCode"] != "approval_rejected" {
				t.Errorf("errorCode = %v, want approval_rejected", update["errorCode"])
			}
			if reply := decodeReply(t, nodes); reply["runResumed"] != true {
				t.Errorf("runResumed = %v", reply["runResumed"])
			}
		})
	}
}

// A FAILURE QUESTION RAISED BEFORE THE FIX IS STILL DECIDABLE (memql#5664).
// The failure path stored {symptom, stepKey, errorMessage} as the subject and
// hashed {runId, stepKey, error, symptom}: the same failure under other keys,
// plus the run's id as the executor spelled it. Such a row may still be pending
// in a running cluster, and no migration rewrites it, so the decide side also
// reads that shape for a question offering Retry and Abandon -- and still
// refuses one whose failure was edited since.
func TestDecideApprovalLandsOnALegacyFailureQuestion(t *testing.T) {
	legacy := func(t *testing.T, errorMessage string) map[string]any {
		t.Helper()
		req := work.FailureApproval(work.ApprovalKindFeedback, "v1:work:run:r1", "draft", work.SymptomHuman, "prompt is too long", "q", work.Evidence{}, testNow, time.Hour)
		raw, err := json.Marshal(map[string]any{
			// The pending list reads the run id back bare.
			"id": "v1:work:approval:a1", "runId": "r1", "ownerUserId": "u-alice", "stepKey": "draft", "kind": req.Kind,
			"subject": map[string]any{"symptom": string(work.SymptomHuman), "stepKey": "draft", "errorMessage": errorMessage},
			"artifactHash": work.ArtifactHash(map[string]any{
				"runId": "v1:work:run:r1", "stepKey": "draft", "error": "prompt is too long", "symptom": string(work.SymptomHuman),
			}),
			"question": req.Question, "options": req.Options,
		})
		if err != nil {
			t.Fatal(err)
		}
		var row map[string]any
		if err := json.Unmarshal(raw, &row); err != nil {
			t.Fatal(err)
		}
		return row
	}
	for _, tc := range []struct {
		name, errorMessage string
		wantChanged        bool
	}{
		{"as raised", "prompt is too long", false},
		{"its failure edited since", "a different failure", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i, eng := newTestIntegration(t)
			eng.reply("workApprovalsForOwner", legacy(t, tc.errorMessage))
			eng.reply("workRunForOwner", map[string]any{
				"id": "v1:work:run:r1", "ownerUserId": "u-alice", "status": runStatusWaiting,
				"waitingOn": map[string]any{"kind": "approval", "subject": "v1:work:approval:a1", "approvalKind": work.ApprovalKindFeedback},
			})
			_, err := i.handleDecideApproval(callerContext("u-alice"), map[string]any{"approvalId": "v1:work:approval:a1", "decision": "approved"}, 0)
			if tc.wantChanged {
				if err == nil || !strings.Contains(err.Error(), work.ErrArtifactChanged.Error()) {
					t.Fatalf("err = %v, want %q", err, work.ErrArtifactChanged)
				}
				return
			}
			if err != nil {
				t.Fatalf("a failure question raised before the fix was refused: %v", err)
			}
			if update := argsOf(t, eng, "updateWorkRun"); update["status"] != runStatusRunning {
				t.Fatalf("run update = %v, want it resumed", update)
			}
		})
	}
}

// A PERSON'S RETRY RESUMES THE RUN UNDER A REQUEST OF ITS OWN (memql#5664).
// Approving a failure question within four minutes of the run's dispatch put
// the run back to `running` with nothing but its id to claim it by -- and the
// execution that failed still held that claim -- so every agent lost it, no
// second event came, and the sweep closed the run as abandoned. The release is
// now a re-run request on the step the question was about, decided by the
// person who answered, and the agents claim it under that request.
func TestApprovingAFailureQuestionResumesUnderARequestOfItsOwn(t *testing.T) {
	i, eng, store := newActsIntegration(t)
	addVersion(store, actRunId, "fetch", 0, 1, "done", nil, nil)
	addVersion(store, actRunId, "draft", 1, 1, "failed", nil, nil)
	req := work.FailureApproval(work.ApprovalKindFeedback, actRunId, "draft", work.SymptomHuman, "prompt is too long", "q", work.Evidence{}, testNow, time.Hour)
	raw, err := json.Marshal(map[string]any{
		"id": "v1:work:approval:a1", "runId": actRunId, "ownerUserId": actOwner, "stepKey": "draft",
		"kind": req.Kind, "subject": req.Subject, "artifactHash": req.ArtifactHash, "options": req.Options,
	})
	if err != nil {
		t.Fatal(err)
	}
	var approval map[string]any
	if err := json.Unmarshal(raw, &approval); err != nil {
		t.Fatal(err)
	}
	eng.reply("workApprovalsForOwner", approval)
	run := actRunRow(runStatusWaiting, "fetch", "draft")
	run["waitingOn"] = map[string]any{"kind": "approval", "subject": "v1:work:approval:a1", "approvalKind": req.Kind}
	eng.reply("workRunForOwner", run)

	if _, err := i.handleDecideApproval(callerContext(actOwner), map[string]any{"approvalId": "v1:work:approval:a1", "decision": "approved"}, 0); err != nil {
		t.Fatalf("decideApproval: %v", err)
	}
	update := argsOf(t, eng, "updateWorkRun")
	rerun, _ := update["rerun"].(map[string]any)
	if update["status"] != runStatusRunning || rerun["reason"] != rerunReasonRerun || rerun["stepKey"] != "draft" || rerun["requestId"] == nil || rerun["requestId"] == "" {
		t.Fatalf("update = %v, want the run released under a re-run request on the step the question was about", update)
	}
	if rerun["requestedBy"] != actOwner {
		t.Errorf("requestedBy = %v, want the person who decided", rerun["requestedBy"])
	}
	if versions, _ := rerun["versions"].(map[string]any); versions["draft"] != float64(2) {
		t.Errorf("versions = %v, want the step's next version", rerun["versions"])
	}
	if _, written := update["cancelRequested"]; written {
		t.Errorf("the release wrote cancelRequested = %v", update["cancelRequested"])
	}

	released := map[string]any{}
	for k, v := range run {
		released[k] = v
	}
	for k, v := range update {
		if k != "runId" {
			released[k] = v
		}
	}
	claims := &pkClaims{}
	if !claims.ClaimWithTTL(context.Background(), runClaimName, actRunId, runClaimTTL) {
		t.Fatal("could not stand in for the failing execution's claim")
	}
	agent, _ := newTestIntegration(t)
	d := &signallingDispatcher{}
	agent.SetDispatcher(d)
	agent.SetRunClaimer(claims)
	agent.HandleRunEvent(remedyEvent(released))
	if got := d.settled(t, 1); len(got) != 1 || got[0].RerunRequestId == "" {
		t.Fatalf("the released run was dispatched %+v, want once under its request: on the bare run id it loses to the execution that failed", got)
	}
}

// A plan failure is an instruction to revise the remaining work, even when
// automatic remedies were exhausted. The decision travels through the graph
// to a planner that never handled the approval; it cannot replay the same
// failed model call or silently replenish the automatic retry allowance.
func TestApprovingAPlanFailureHandsOffOneReplanAcrossReplicas(t *testing.T) {
	i, eng, _ := newActsIntegration(t)
	req := work.FailureApproval(work.ApprovalKindFeedback, actRunId, "draft/nested", work.SymptomPlan,
		"model call exceeded its ceiling", "q", work.Evidence{Reason: "divide unfinished work"}, testNow, time.Hour)
	eng.reply("workApprovalsForOwner", map[string]any{
		"id": "v1:work:approval:a1", "runId": actRunId, "ownerUserId": actOwner, "stepKey": req.StepKey,
		"kind": req.Kind, "subject": req.Subject, "artifactHash": req.ArtifactHash, "options": req.Options,
		"evidence": map[string]any{"reason": req.Evidence.Reason},
	})
	run := actRunRow(runStatusWaiting, "fetch", "draft", "publish")
	run["spent"] = map[string]any{"retries": 2, "modelCalls": 40, "tokens": 10000}
	run["waitingOn"] = map[string]any{"kind": "approval", "subject": "v1:work:approval:a1", "since": rfc(testNow.Add(-time.Minute))}
	eng.reply("workRunForOwner", run)
	if _, err := i.handleDecideApproval(callerContext(actOwner), map[string]any{
		"approvalId": "v1:work:approval:a1", "decision": "answered", "answer": map[string]any{"value": work.FailureAnswerRetry},
	}, 0); err != nil {
		t.Fatal(err)
	}
	update := argsOf(t, eng, "updateWorkRun")
	wait := rowMap(update, "waitingOn")
	if update["status"] != runStatusWaiting || wait["kind"] != waitKindReplan || wait["subject"] != "draft" || wait["since"] == "" {
		t.Fatalf("plan failure was replayed instead of handed to the remedy: %v", update)
	}
	if wait["reason"] != "divide unfinished work\nmodel call exceeded its ceiling" {
		t.Fatalf("the remedy lost the classified cause: %v", wait)
	}
	for _, key := range []string{"spent", "rerun", "staleSteps", "head", "stepOrder"} {
		if _, changed := update[key]; changed {
			t.Errorf("approval changed %s before replanning: %v", key, update[key])
		}
	}
	if len(eng.callsTo("updateWorkGoal")) != 0 || len(eng.callsTo("updateWorkStep")) != 0 {
		t.Fatal("recovery changed the goal's budget or a completed step")
	}
	for k, v := range update {
		if k != "runId" {
			run[k] = v
		}
	}
	claims, remedy := &pkClaims{}, &signallingRemedy{}
	a, _ := plannerReplica(t, remedy, claims, run)
	b, _ := plannerReplica(t, remedy, claims, run)
	a.HandleRunEvent(remedyEvent(run))
	b.HandleRunEvent(remedyEvent(run))
	calls := remedy.settled(t, 1)
	if len(calls) != 1 || calls[0].kind != waitKindReplan || calls[0].stepKey != "draft" || calls[0].run.GoalId != actGoalId {
		t.Fatalf("approved recovery across replicas = %+v", calls)
	}
}

// Only a failure question's Retry is a re-run. A budget approval the model
// seam raised parks a step mid-flight on a person's word, and approving it
// resumes that step; writing a request would run it again as a new version.
func TestReleasingAnApprovalThatIsNotAFailureQuestionWritesNoRequest(t *testing.T) {
	i, eng, store := newActsIntegration(t)
	addVersion(store, actRunId, "fetch", 0, 1, "done", nil, nil)
	addVersion(store, actRunId, "draft", 1, 1, "running", nil, nil)
	req := work.BudgetApproval(actRunId, "draft", work.CeilingBreach{Ceiling: "maxTokens", Limit: "10", Actual: "12", Reason: "over"}, testNow, time.Hour)
	raw, err := json.Marshal(map[string]any{
		"id": "v1:work:approval:a1", "runId": actRunId, "ownerUserId": actOwner, "stepKey": "draft",
		"kind": req.Kind, "subject": req.Subject, "artifactHash": req.ArtifactHash,
	})
	if err != nil {
		t.Fatal(err)
	}
	var approval map[string]any
	if err := json.Unmarshal(raw, &approval); err != nil {
		t.Fatal(err)
	}
	eng.reply("workApprovalsForOwner", approval)
	run := actRunRow(runStatusWaiting, "fetch", "draft")
	run["waitingOn"] = map[string]any{"kind": "approval", "subject": "v1:work:approval:a1", "approvalKind": req.Kind}
	eng.reply("workRunForOwner", run)

	if _, err := i.handleDecideApproval(callerContext(actOwner), map[string]any{"approvalId": "v1:work:approval:a1", "decision": "approved"}, 0); err != nil {
		t.Fatalf("decideApproval: %v", err)
	}
	update := argsOf(t, eng, "updateWorkRun")
	if update["status"] != runStatusRunning {
		t.Fatalf("status = %v, want running", update["status"])
	}
	if rerun, written := update["rerun"]; written {
		t.Fatalf("a budget approval's release wrote a re-run request %v", rerun)
	}
}

// TestDecideApprovalRefusesAnApprovalTheCallerCannotSee. The caller's own
// pending list IS the ownership check.
func TestDecideApprovalRefusesAnApprovalTheCallerCannotSee(t *testing.T) {
	i, eng := newTestIntegration(t)
	// The pending list answers nothing, which is what the owned read does for
	// somebody else's approval.
	_, err := i.handleDecideApproval(callerContext("u-mallory"), map[string]any{
		"approvalId": "v1:work:approval:not-mine", "decision": "approved",
	}, 0)
	if err == nil {
		t.Fatal("decided an approval that did not come back from the owned read")
	}
	if n := len(eng.callsTo("decideWorkApproval")); n != 0 {
		t.Errorf("a decision was written anyway (%d calls)", n)
	}
}

// ---------------------------------------------------------------------------
// The safety sink
// ---------------------------------------------------------------------------

// TestSinkRaisesOneApprovalPerCorrelationKey.
//
// The correlation key IS the artifact hash, and that is what gives the sink
// its guarantee by construction: a retry of the SAME command finds the pending
// row, and a MODIFIED command hashes differently and raises a fresh one. A
// sink that raised a new row on every dispatch would turn one decision into an
// unbounded inbox.
func TestSinkRaisesOneApprovalPerCorrelationKey(t *testing.T) {
	desc := safety.NewExecAction(safety.SurfaceWorkbench, "rm -rf /tmp/scratch", safety.CallerContext{
		RunID: "v1:work:run:r1", StepID: "step-3", OwnerUserID: "u-alice",
	})
	cls := safety.Classification{Reason: "destructive shell command", RuleID: "shell.destructive"}
	key := safety.ApprovalCorrelationKey(desc)

	t.Run("first dispatch raises one", func(t *testing.T) {
		i, eng := newTestIntegration(t)
		v := i.NewSink(SinkOptions{Logger: testLogger()}).Check(callerContext("u-alice"), desc, cls)
		if v.State != safety.ApprovalStatePending {
			t.Fatalf("state = %v, want pending", v.State)
		}
		call := eng.callTo(t, "createWorkApproval")
		args := call.Args(t)
		if args["artifactHash"] != key {
			t.Errorf("artifactHash = %v, want the correlation key %v -- two identities for one row make 'is this what I approved' answerable two ways", args["artifactHash"], key)
		}
		if args["kind"] != work.ApprovalKindSideEffect {
			t.Errorf("kind = %v", args["kind"])
		}
		if args["runId"] != "v1:work:run:r1" || args["stepKey"] != "step-3" {
			t.Errorf("the approval was attached to {%v, %v}", args["runId"], args["stepKey"])
		}
		ev, _ := args["evidence"].(map[string]any)
		if ev == nil || ev["ruleId"] != "shell.destructive" {
			t.Errorf("evidence = %v; a person deciding a gate needs to know which rule fired", args["evidence"])
		}
		if !call.Origin.IsInternal() {
			t.Error("createWorkApproval reached the engine without internal origin; it is @serverOnly and the row would never exist")
		}
		if call.Actor != "u-alice" {
			t.Errorf("the approval was written under actor %q, want the owner's borrowed authority", call.Actor)
		}
	})

	t.Run("a retry of the same action reuses the pending row", func(t *testing.T) {
		i, eng := newTestIntegration(t)
		eng.reply("workApprovalsForOwner", map[string]any{
			"id": "v1:work:approval:existing", "artifactHash": key, "kind": work.ApprovalKindSideEffect,
		})
		v := i.NewSink(SinkOptions{Logger: testLogger()}).Check(callerContext("u-alice"), desc, cls)
		if v.State != safety.ApprovalStatePending || v.ApprovalRequestID != "v1:work:approval:existing" {
			t.Fatalf("verdict = %+v, want the existing pending row", v)
		}
		if n := len(eng.callsTo("createWorkApproval")); n != 0 {
			t.Errorf("a duplicate approval was raised (%d creates)", n)
		}
	})

	t.Run("a modified command raises a fresh one", func(t *testing.T) {
		i, eng := newTestIntegration(t)
		// The stored row is the approval for the ORIGINAL command.
		eng.reply("workApprovalsForOwner", map[string]any{
			"id": "v1:work:approval:existing", "artifactHash": key, "kind": work.ApprovalKindSideEffect,
		})
		modified := safety.NewExecAction(safety.SurfaceWorkbench, "rm -rf /", safety.CallerContext{
			RunID: "v1:work:run:r1", StepID: "step-3", OwnerUserID: "u-alice",
		})
		v := i.NewSink(SinkOptions{Logger: testLogger()}).Check(callerContext("u-alice"), modified, cls)
		if v.State != safety.ApprovalStatePending {
			t.Fatalf("state = %v", v.State)
		}
		create := eng.callTo(t, "createWorkApproval").Args(t)
		if create["artifactHash"] == key {
			t.Error("the modified command reused the original's hash -- approving one command would run another")
		}
	})
}

// TestSinkAnswersUnconfiguredRatherThanInventingARun.
//
// runId is required on v1:work:approval. A blank one would make the row be
// refused, and reporting "pending" for a row nobody can see would park a step
// on an approval that does not exist. Unconfigured keeps the Gate's own
// refusal, which is what a cluster with no sink has always had.
func TestSinkAnswersUnconfiguredRatherThanInventingARun(t *testing.T) {
	i, eng := newTestIntegration(t)
	desc := safety.NewExecAction(safety.SurfaceWorkbench, "ls", safety.CallerContext{OwnerUserID: "u-alice"})
	v := i.NewSink(SinkOptions{Logger: testLogger()}).Check(callerContext("u-alice"), desc, safety.Classification{})
	if v.State != safety.ApprovalStateUnconfigured {
		t.Fatalf("state = %v, want unconfigured", v.State)
	}
	if got := eng.summary(); got != "(none)" {
		t.Errorf("the sink reached the engine with no run to attach to: %s", got)
	}
}

// TestSinkRedactsTheSubject. The subject is shown to a person and stored in
// the graph, and a raw payload can carry a credential.
func TestSinkRedactsTheSubject(t *testing.T) {
	desc := safety.NewHTTPAction(safety.SurfaceWorkbench, "POST", "https://api.example.com/x?token=hunter2", "", safety.CallerContext{
		RunID: "v1:work:run:r1", OwnerUserID: "u-alice",
	})
	subject := subjectFrom(desc)
	redacted := safety.RedactedPayload(desc.Payload)
	if subject["url"] != redacted.URL {
		t.Errorf("the subject carries %v but RedactedPayload produced %v -- the subject must be built from the redacted payload, never desc.Payload", subject["url"], redacted.URL)
	}
}

// TestSinkFailureIsUnconfiguredNotAnError. The ApprovalSink contract: a dead
// approval sink must not crash live traffic.
func TestSinkFailureIsUnconfiguredNotAnError(t *testing.T) {
	i, eng := newTestIntegration(t)
	eng.refuse("createWorkApproval", errRefused)
	desc := safety.NewExecAction(safety.SurfaceWorkbench, "ls", safety.CallerContext{
		RunID: "v1:work:run:r1", OwnerUserID: "u-alice",
	})
	v := i.NewSink(SinkOptions{Logger: testLogger()}).Check(callerContext("u-alice"), desc, safety.Classification{})
	if v.State != safety.ApprovalStateUnconfigured {
		t.Fatalf("state = %v, want unconfigured -- a failed write must leave the Gate's own refusal in place, not crash the dispatch", v.State)
	}
}

var errRefused = &refusalError{}

type refusalError struct{}

func (*refusalError) Error() string { return "refused" }

func TestAJobCannotApproveItsOwnWork(t *testing.T) {
	for _, ctx := range []context.Context{
		auth.ContextWithUserActor(context.Background(), "u-alice"),
		auth.ContextWithAccess(context.Background(), auth.SystemActor("approval-test")),
		common.ContextWithRun(callerContext("u-alice"), common.RunContext{RunId: "running-job", OwnerUserId: "u-alice"}),
	} {
		i, engine := newTestIntegration(t)
		if _, err := i.handleDecideApproval(ctx, map[string]any{"approvalId": "approval", "decision": "approved"}, 0); err == nil {
			t.Fatal("non-interactive authority decided a human approval")
		}
		if len(engine.calls) != 0 {
			t.Fatal("refused approval reached storage")
		}
	}
}
