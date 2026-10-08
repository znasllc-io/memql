package work

import (
	"strings"
	"testing"

	work "github.com/znasllc-io/memql/component/work"
)

// training_test.go -- memql#5063.
//
// The acceptance the issue names:
//
//	An approved specialist-training request starts training again, through
//	the work spine. Both gates above still hold, with a test for each.
//	Nothing re-introduces a v1:planner:plan.
//
// The two gates are the specialist needing a domain-bearing skill (else
// escalate) and the approval being the idempotency. Both are cost-control
// properties, not niceties: training with no domain is a bounded tool loop
// with nowhere to write, and a request that starts training twice pays twice.

// trainingApproval is a pending skillMint approval carrying a training
// subject, as the caller's own pending list answers it.
func trainingApproval(subject map[string]any) map[string]any {
	return map[string]any{
		"id": "v1:work:approval:a1", "runId": "v1:work:run:r1", "ownerUserId": "u-alice",
		"kind": work.ApprovalKindSkillMint, "subject": subject,
		"artifactHash": "a-correlation-key-passed-through",
	}
}

func trainingSubject() map[string]any {
	return map[string]any{
		"action":       TrainingSubjectAction,
		"specialistId": "v1:agents:agent:spec-1",
		"topic":        "French employment law",
		"mode":         "initial",
	}
}

// specialistWithADomain wires the two reads resolveSpecialistPrimaryDomain
// makes: the agent's skillIds, and the active skill catalog's domainIds.
func specialistWithADomain(eng *recordingEngine) {
	eng.reply("agentById", map[string]any{
		"id":           "v1:agents:agent:spec-1",
		"capabilities": map[string]any{"skillIds": []any{"v1:skills:skill:employment-law"}},
	})
	eng.reply("activeSkillsFull", map[string]any{
		"id":        "v1:skills:skill:employment-law",
		"domainIds": []any{"v1:knowledge:knowledgeDomain:fr-employment"},
	})
}

func parkedRun(eng *recordingEngine) {
	eng.reply("workRunForOwner", map[string]any{
		"id": "v1:work:run:r1", "ownerUserId": "u-alice", "status": runStatusWaiting,
		"waitingOn": map[string]any{"kind": "approval", "subject": "v1:work:approval:a1"},
	})
}

// THE RESTORED PATH. Approving the request opens training, through the same
// template the refresh cadence opens -- not a second trainer.
func TestApprovedTrainingOpensTheTrainingWork(t *testing.T) {
	i, eng := newTestIntegration(t)
	eng.reply("workApprovalsForOwner", trainingApproval(trainingSubject()))
	specialistWithADomain(eng)
	parkedRun(eng)

	nodes, err := i.handleDecideApproval(callerContext("u-alice"), map[string]any{
		"approvalId": "v1:work:approval:a1", "decision": "approved",
	}, 0)
	if err != nil {
		t.Fatalf("decideApproval: %v", err)
	}

	runs := eng.callsTo("createWorkRun")
	if len(runs) != 1 {
		t.Fatalf("opened %d runs, want exactly 1 -- approving training must start training", len(runs))
	}
	args := runs[0].Args(t)
	if args["automationName"] != trainingTemplate {
		t.Errorf("the run names template %v, want %q -- the approval path and the refresh cadence must not\n"+
			"become two trainers", args["automationName"], trainingTemplate)
	}
	input, _ := args["input"].(map[string]any)
	if input["domainId"] != "v1:knowledge:knowledgeDomain:fr-employment" {
		t.Errorf("input.domainId = %v; the template hard-requires the resolved domain", input["domainId"])
	}
	if input["specialistId"] != "v1:agents:agent:spec-1" || input["topic"] != "French employment law" || input["mode"] != "initial" {
		t.Errorf("the training input lost the request: %v", input)
	}
	if runs[0].Actor != "u-alice" {
		t.Errorf("the training run was opened under actor %q, want the approval owner's borrowed authority", runs[0].Actor)
	}
	// The reply has to SAY training started, or a surface cannot tell an
	// approval that caused work from one that did not.
	reply := decodeReply(t, nodes)
	if reply["trainingRunId"] == nil || reply["trainingGoalId"] == nil {
		t.Errorf("the reply does not name the training it started: %v", reply)
	}
}

// GATE 1. A specialist with no domain-bearing skill ESCALATES rather than
// starting work against no domain -- and starts nothing.
func TestApprovedTrainingEscalatesWhenNoDomainResolves(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*recordingEngine)
	}{
		{"the specialist has no skills at all", func(eng *recordingEngine) {
			eng.reply("agentById", map[string]any{"id": "v1:agents:agent:spec-1", "capabilities": map[string]any{}})
		}},
		{"its skills carry no domain", func(eng *recordingEngine) {
			eng.reply("agentById", map[string]any{
				"id":           "v1:agents:agent:spec-1",
				"capabilities": map[string]any{"skillIds": []any{"v1:skills:skill:tools-only"}},
			})
			eng.reply("activeSkillsFull", map[string]any{"id": "v1:skills:skill:tools-only", "domainIds": []any{}})
		}},
		{"the specialist does not resolve", func(_ *recordingEngine) {}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i, eng := newTestIntegration(t)
			eng.reply("workApprovalsForOwner", trainingApproval(trainingSubject()))
			tc.setup(eng)
			parkedRun(eng)

			nodes, err := i.handleDecideApproval(callerContext("u-alice"), map[string]any{
				"approvalId": "v1:work:approval:a1", "decision": "approved",
			}, 0)
			if err != nil {
				t.Fatalf("decideApproval: %v", err)
			}
			if got := len(eng.callsTo("createWorkRun")); got != 0 {
				t.Fatalf("opened %d training runs with no domain to train into; the Trainer's tool loop would\n"+
					"spend model calls and write nowhere", got)
			}
			// The escalation is an APPROVAL, not a log line: the person who
			// approved training is the person who has to attach a domain.
			raised := eng.callsTo("createWorkApproval")
			if len(raised) != 1 {
				t.Fatalf("raised %d approvals; the request must escalate for feedback", len(raised))
			}
			args := raised[0].Args(t)
			if args["kind"] != work.ApprovalKindFeedback {
				t.Errorf("escalated as kind %v, want feedback", args["kind"])
			}
			if q, _ := args["question"].(string); !strings.Contains(strings.ToLower(q), "domain") {
				t.Errorf("the escalation does not say what is missing: %q", q)
			}
			if reply := decodeReply(t, nodes); reply["trainingEscalated"] != true {
				t.Errorf("the reply does not report the escalation: %v", reply)
			}
		})
	}
}

// GATE 2. The approval is the idempotency, and it is INHERITED rather than
// re-implemented: handleDecideApproval resolves through the caller's PENDING
// list, so a second decide finds no row.
//
// The assertion is that no second training run is opened -- and that the
// refusal comes with nothing written, so a retry cannot half-apply.
func TestASecondDecideOfTheSameApprovalStartsNoSecondTraining(t *testing.T) {
	i, eng := newTestIntegration(t)
	eng.reply("workApprovalsForOwner", trainingApproval(trainingSubject()))
	specialistWithADomain(eng)
	parkedRun(eng)

	if _, err := i.handleDecideApproval(callerContext("u-alice"), map[string]any{
		"approvalId": "v1:work:approval:a1", "decision": "approved",
	}, 0); err != nil {
		t.Fatalf("first decide: %v", err)
	}
	if got := len(eng.callsTo("createWorkRun")); got != 1 {
		t.Fatalf("the first decide opened %d runs, want 1", got)
	}

	// The approval is no longer pending, which is exactly what the second
	// decide sees.
	eng.reply("workApprovalsForOwner")

	if _, err := i.handleDecideApproval(callerContext("u-alice"), map[string]any{
		"approvalId": "v1:work:approval:a1", "decision": "approved",
	}, 0); err == nil {
		t.Fatal("a decided approval was decided again; the pending-list resolution IS the idempotency")
	}
	if got := len(eng.callsTo("createWorkRun")); got != 1 {
		t.Fatalf("%d training runs after two decides of one approval; one approved decision starts exactly one", got)
	}
}

// A REJECTED training request starts nothing. Obvious, and the one direction a
// "did the approval land" hook gets wrong by reading the row rather than the
// decision.
func TestRejectedTrainingStartsNothing(t *testing.T) {
	i, eng := newTestIntegration(t)
	eng.reply("workApprovalsForOwner", trainingApproval(trainingSubject()))
	specialistWithADomain(eng)
	parkedRun(eng)

	if _, err := i.handleDecideApproval(callerContext("u-alice"), map[string]any{
		"approvalId": "v1:work:approval:a1", "decision": "rejected",
	}, 0); err != nil {
		t.Fatalf("decideApproval: %v", err)
	}
	if got := len(eng.callsTo("createWorkRun")); got != 0 {
		t.Fatalf("a rejection started %d training runs", got)
	}
}

// AND THE NARROWING. Every other approval kind, and a skillMint approval whose
// subject is about something else, must pass through untouched -- the hook
// requires BOTH the kind and the action, so neither alone can start training.
func TestOnlyATrainingSubjectStartsTraining(t *testing.T) {
	cases := []struct {
		name    string
		kind    string
		subject map[string]any
	}{
		{"a budget approval", work.ApprovalKindBudget, map[string]any{"tokenBudget": 1000}},
		{"a plan review", work.ApprovalKindPlanReview, map[string]any{"patches": []any{}}},
		{"skillMint for something that is not training", work.ApprovalKindSkillMint,
			map[string]any{"action": "mintSkill", "skillId": "v1:skills:skill:x"}},
		{"a training subject under the wrong kind", work.ApprovalKindSideEffect, trainingSubject()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i, eng := newTestIntegration(t)
			approval := trainingApproval(tc.subject)
			approval["kind"] = tc.kind
			// budget and planReview derive their hash from the subject, so the
			// fixture has to carry the real digest or the artifact-hash gate
			// refuses before this test's own assertion is reached.
			if tc.kind == work.ApprovalKindBudget || tc.kind == work.ApprovalKindPlanReview {
				approval["artifactHash"] = artifactHashOf(tc.subject)
			}
			eng.reply("workApprovalsForOwner", approval)
			specialistWithADomain(eng)
			parkedRun(eng)

			nodes, err := i.handleDecideApproval(callerContext("u-alice"), map[string]any{
				"approvalId": "v1:work:approval:a1", "decision": "approved",
			}, 0)
			if err != nil {
				t.Fatalf("decideApproval: %v", err)
			}
			if got := len(eng.callsTo("createWorkRun")); got != 0 {
				t.Fatalf("%q started %d training runs", tc.name, got)
			}
			// An absent key and a zero are different answers: a caller must
			// not read "no training" as "training failed".
			reply := decodeReply(t, nodes)
			if _, present := reply["trainingGoalId"]; present {
				t.Errorf("the reply carries a training key for an approval that was not about training: %v", reply)
			}
			if _, present := reply["trainingEscalated"]; present {
				t.Errorf("the reply reports an escalation that did not happen: %v", reply)
			}
		})
	}
}

// NOTHING RE-INTRODUCES A PLAN. The acceptance says so explicitly, and it is
// the one property a reader of this file cannot check by eye -- the deleted
// implementation minted a kind=trainSpecialist Plan.
func TestApprovedTrainingWritesNoPlanRow(t *testing.T) {
	i, eng := newTestIntegration(t)
	eng.reply("workApprovalsForOwner", trainingApproval(trainingSubject()))
	specialistWithADomain(eng)
	parkedRun(eng)

	if _, err := i.handleDecideApproval(callerContext("u-alice"), map[string]any{
		"approvalId": "v1:work:approval:a1", "decision": "approved",
	}, 0); err != nil {
		t.Fatalf("decideApproval: %v", err)
	}
	for _, c := range eng.recorded() {
		// Inspect the operation, not the frozen Spine source carried as data.
		if strings.Contains(strings.ToLower(c.Name()), "plan") {
			t.Errorf("a plan-shaped call reached the engine: %s", c.Query)
		}
	}
}

// ---------------------------------------------------------------------------
// The door
// ---------------------------------------------------------------------------

// A request raises the approval the gate acts on, hung on the caller's run.
func TestRequestSpecialistTrainingRaisesTheApproval(t *testing.T) {
	i, eng := newTestIntegration(t)
	eng.reply("workRunForOwner", map[string]any{
		"id": "v1:work:run:r1", "ownerUserId": "u-alice", "status": runStatusRunning,
	})

	nodes, err := i.handleRequestSpecialistTraining(callerContext("u-alice"), map[string]any{
		"runId": "v1:work:run:r1", "specialistId": "v1:agents:agent:spec-1",
		"topic": "French employment law", "mode": "refresh",
	}, 0)
	if err != nil {
		t.Fatalf("requestSpecialistTraining: %v", err)
	}

	raised := eng.callsTo("createWorkApproval")
	if len(raised) != 1 {
		t.Fatalf("raised %d approvals, want 1", len(raised))
	}
	args := raised[0].Args(t)
	if args["kind"] != work.ApprovalKindSkillMint {
		t.Errorf("kind = %v, want skillMint -- the gate requires BOTH the kind and the action", args["kind"])
	}
	subject, _ := args["subject"].(map[string]any)
	if subject["action"] != TrainingSubjectAction || subject["specialistId"] != "v1:agents:agent:spec-1" || subject["mode"] != "refresh" {
		t.Errorf("the subject does not describe the request: %v", subject)
	}
	// The hash must be DERIVED here, or the artifact-hash gate is inert on
	// this kind and a request edited between asking and deciding would train
	// on terms nobody agreed to.
	if args["artifactHash"] != artifactHashOf(subject) {
		t.Errorf("artifactHash = %v, want the digest of the subject", args["artifactHash"])
	}
	if raised[0].Actor != "u-alice" {
		t.Errorf("the approval was raised under actor %q, want the run owner's borrowed authority", raised[0].Actor)
	}
	if reply := decodeReply(t, nodes); reply["approvalId"] == nil {
		t.Errorf("the reply does not name the approval a person now has to decide: %v", reply)
	}
}

// AND THE ROUND TRIP, which is the only assertion that proves the door and the
// gate agree: raise a request, then decide the approval it produced.
func TestARequestedTrainingApprovalIsAcceptedByTheGate(t *testing.T) {
	i, eng := newTestIntegration(t)
	eng.reply("workRunForOwner", map[string]any{
		"id": "v1:work:run:r1", "ownerUserId": "u-alice", "status": runStatusRunning,
	})
	if _, err := i.handleRequestSpecialistTraining(callerContext("u-alice"), map[string]any{
		"runId": "v1:work:run:r1", "specialistId": "v1:agents:agent:spec-1", "topic": "French employment law",
	}, 0); err != nil {
		t.Fatalf("request: %v", err)
	}
	raised := eng.callsTo("createWorkApproval")[0].Args(t)

	// Replay the row exactly as the request wrote it -- the subject and hash
	// are the request's own, not this test's idea of them, so a change to
	// either side breaks this and nothing else has to be remembered.
	i2, eng2 := newTestIntegration(t)
	eng2.reply("workApprovalsForOwner", map[string]any{
		"id": raised["approvalId"], "runId": "v1:work:run:r1", "ownerUserId": "u-alice",
		"kind": raised["kind"], "subject": raised["subject"], "artifactHash": raised["artifactHash"],
	})
	specialistWithADomain(eng2)
	parkedRun(eng2)

	if _, err := i2.handleDecideApproval(callerContext("u-alice"), map[string]any{
		"approvalId": raised["approvalId"], "decision": "approved",
	}, 0); err != nil {
		t.Fatalf("decide: %v", err)
	}
	runs := eng2.callsTo("createWorkRun")
	if len(runs) != 1 || runs[0].Args(t)["automationName"] != trainingTemplate {
		t.Fatalf("approving the request the door raised did not start training: %d runs", len(runs))
	}
}

// A run the caller cannot read is a run they may not raise a decision on:
// requesting training against somebody else's run would put a question in
// their inbox they did not raise and cannot place.
func TestRequestSpecialistTrainingRefusesAnUnreadableRun(t *testing.T) {
	i, eng := newTestIntegration(t)
	// workRunForOwner is unlisted, so it answers no rows -- which is exactly
	// what the owned tier does for somebody else's run.
	if _, err := i.handleRequestSpecialistTraining(callerContext("u-mallory"), map[string]any{
		"runId": "v1:work:run:r1", "specialistId": "v1:agents:agent:spec-1",
	}, 0); err == nil {
		t.Fatal("training was requested against a run the caller cannot read")
	}
	if got := len(eng.callsTo("createWorkApproval")); got != 0 {
		t.Errorf("%d approvals were raised despite the refusal", got)
	}
}

// An unrecognised mode is refused rather than passed to the Trainer's prompt,
// where it would be interpreted instead of rejected.
func TestRequestSpecialistTrainingRefusesAnUnknownMode(t *testing.T) {
	i, eng := newTestIntegration(t)
	if _, err := i.handleRequestSpecialistTraining(callerContext("u-alice"), map[string]any{
		"runId": "v1:work:run:r1", "specialistId": "v1:agents:agent:spec-1", "mode": "aggressive",
	}, 0); err == nil {
		t.Fatal("an unknown training mode was accepted")
	}
	if got := len(eng.recorded()); got != 0 {
		t.Errorf("the refusal happened after %d engine calls", got)
	}
}
