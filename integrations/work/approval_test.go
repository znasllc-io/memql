package work

import (
	"encoding/json"
	"strings"
	"testing"

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
		PlanID: "v1:work:run:r1", TaskID: "step-3", OwnerUserID: "u-alice",
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
			PlanID: "v1:work:run:r1", TaskID: "step-3", OwnerUserID: "u-alice",
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
		PlanID: "v1:work:run:r1", OwnerUserID: "u-alice",
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
		PlanID: "v1:work:run:r1", OwnerUserID: "u-alice",
	})
	v := i.NewSink(SinkOptions{Logger: testLogger()}).Check(callerContext("u-alice"), desc, safety.Classification{})
	if v.State != safety.ApprovalStateUnconfigured {
		t.Fatalf("state = %v, want unconfigured -- a failed write must leave the Gate's own refusal in place, not crash the dispatch", v.State)
	}
}

var errRefused = &refusalError{}

type refusalError struct{}

func (*refusalError) Error() string { return "refused" }
