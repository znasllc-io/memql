package work

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	state "github.com/znasllc-io/memql/component/work"
)

func TestOutcomeDecisionRecoveryRefusesChangedAuthorityOrReceipt(t *testing.T) {
	first, second := openActsDB(t), openActsDB(t)
	for _, change := range []string{"cancelled", "new-answer", "new-step", "changed-failure", "accepted", "closed", "foreign"} {
		t.Run(change, func(t *testing.T) {
			a, b := freshActsDBPair(t, first, second)
			runID := a.openRun(t, nil)
			message := state.OutcomeRecoveryError(fmt.Errorf("retained reporting stage")).Error()
			a.writeVersion(t, runID, runID, "finish", 0, 1, nil, "saved", map[string]any{"status": "failed", "errorMessage": message})
			approval := newRowId(approvalConcept)
			now := time.Now().UTC()
			question := state.FailureApproval(state.ApprovalKindFeedback, runID, "finish", state.SymptomHuman, message, "Retry reporting?", state.Evidence{}, now, time.Hour)
			a.write(t, "createWorkApproval", map[string]any{"approvalId": approval, "runId": runID, "stepKey": "finish", "kind": question.Kind, "subject": question.Subject, "artifactHash": question.ArtifactHash, "question": question.Question, "options": question.Options, "requestedAt": rfc(now), "expiresAt": rfc(now.Add(time.Hour))})
			a.write(t, "decideWorkApproval", map[string]any{"approvalId": approval, "decision": "answered", "decidedBy": a.owner, "decidedAt": rfc(time.Now()), "answer": map[string]any{"value": "retry"}})
			a.write(t, "updateWorkRun", map[string]any{"runId": runID, "status": "abandoned", "errorCode": "run_abandoned", "humanResumeId": approval, "stepOrder": []string{"finish"}, "head": state.Head{"finish": {Version: 1}}.Object()})
			before := a.runRow(t, runID)
			person := actorCtx(a.owner)
			switch change {
			case "cancelled":
				a.write(t, "updateWorkRun", map[string]any{"runId": runID, "cancelRequested": true})
			case "new-answer":
				a.write(t, "updateWorkRun", map[string]any{"runId": runID, "humanResumeId": "new-answer"})
			case "new-step":
				a.writeVersion(t, runID, runID, "finish", 0, 2, nil, "saved", map[string]any{"status": "failed", "errorMessage": message})
			case "changed-failure":
				a.writeVersion(t, runID, runID, "finish", 0, 2, nil, "saved", map[string]any{"status": "failed", "errorMessage": "different failure"})
			case "accepted":
				a.write(t, "updateWorkGoal", map[string]any{"goalId": rowString(before, "goalId"), "acceptance": map[string]any{"decision": "accepted"}})
			case "closed":
				a.write(t, "updateWorkGoal", map[string]any{"goalId": rowString(before, "goalId"), "status": "closed"})
			case "foreign":
				person = actorCtx("another-owner")
			}
			_, err := b.i.handleDecideApproval(person, map[string]any{"approvalId": approval, "decision": "answered", "answer": map[string]any{"value": "retry"}}, 0)
			require.Error(t, err)
			require.Equal(t, "abandoned", rowString(a.runRow(t, runID), "status"))
		})
	}
}
