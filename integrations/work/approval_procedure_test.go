package work

import (
	"strings"
	"testing"

	work "github.com/znasllc-io/memql/component/work"
)

// approval_procedure_test.go -- deciding a procedurePromotion (epic
// memql#5408, D15). The approval pins ONE version of a learned procedure,
// and that version is a ROW that keeps changing after the approval is
// raised: a re-lift rewrites the construct's source, template and
// preconditions. These tests are the two halves of what deciding it means --
// the approval never carries to a changed construct, and deciding it never
// touches the shadow run whose evidence it names.

const (
	promotionApprovalId = "v1:work:approval:p1"
	promotionShadowRun  = "v1:work:run:shadow1"
	promotionConstruct  = "v1:authoring:construct:c1"
)

func promotionApprovalRow(storedHash string) map[string]any {
	return map[string]any{
		"id": promotionApprovalId, "runId": promotionShadowRun, "ownerUserId": "u-alice",
		"kind": work.ApprovalKindProcedurePromotion, "artifactHash": storedHash,
		"subject": map[string]any{
			"constructId": promotionConstruct, "constructName": "learnedProcedure_abc_l1",
			"procedureHash": storedHash, "shadowMatches": 5.0,
		},
	}
}

// TestApprovingAPromotionAfterTheConstructChangedIsRefused: the subject on
// the approval row still says what it said when the ladder proposed it, so
// recomputing over the subject would pass every time. What the person
// approved is the construct AS IT WAS; a re-lift since then is a new
// candidate (D15), and approving the old hash must not promote the new
// source.
func TestApprovingAPromotionAfterTheConstructChangedIsRefused(t *testing.T) {
	cases := []struct {
		name        string
		current     map[string]any // the construct row as it reads now; nil = gone
		decision    string
		wantRefused bool
	}{
		{"the construct is unchanged -- approve", map[string]any{"id": promotionConstruct, "procedureHash": "sha256:aaa"}, "approved", false},
		{"the construct was re-lifted -- REFUSED", map[string]any{"id": promotionConstruct, "procedureHash": "sha256:bbb"}, "approved", true},
		{"the construct is gone -- REFUSED", nil, "approved", true},
		{"a rejection of a changed construct is still recorded", map[string]any{"id": promotionConstruct, "procedureHash": "sha256:bbb"}, "rejected", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i, eng := newTestIntegration(t)
			eng.reply("workApprovalsForOwner", promotionApprovalRow("sha256:aaa"))
			if tc.current != nil {
				eng.reply("authoringConstructById", tc.current)
			}
			_, err := i.handleDecideApproval(callerContext("u-alice"), map[string]any{
				"approvalId": promotionApprovalId, "decision": tc.decision,
			}, 0)
			if tc.wantRefused {
				if err == nil {
					t.Fatal("a changed or missing construct was approved -- the approval carried to something the person never saw")
				}
				if !strings.Contains(err.Error(), work.ErrArtifactChanged.Error()) {
					t.Errorf("the refusal %q does not say the artifact changed", err)
				}
				if n := len(eng.callsTo("decideWorkApproval")); n != 0 {
					t.Errorf("the decision was recorded despite the refusal (%d calls)", n)
				}
				return
			}
			if err != nil {
				t.Fatalf("decideApproval: %v", err)
			}
			if n := len(eng.callsTo("decideWorkApproval")); n != 1 {
				t.Errorf("decideWorkApproval called %d times, want exactly 1", n)
			}
		})
	}
}

// TestDecidingAPromotionNeverTouchesTheShadowRun: the promotion's runId is
// the finished shadow run whose comparison met the threshold. Nothing parks
// on it, and a rejection must never rewrite it -- resumeParkedRun's
// rejection branch FAILS the run it names, which here would turn the
// evidence a person was shown into a failure they never saw happen.
func TestDecidingAPromotionNeverTouchesTheShadowRun(t *testing.T) {
	for _, decision := range []string{"approved", "rejected"} {
		t.Run(decision, func(t *testing.T) {
			i, eng := newTestIntegration(t)
			eng.reply("workApprovalsForOwner", promotionApprovalRow("sha256:aaa"))
			eng.reply("authoringConstructById", map[string]any{"id": promotionConstruct, "procedureHash": "sha256:aaa"})
			// Even a run that looks parked on this approval stays untouched.
			eng.reply("workRunForOwner", map[string]any{
				"id": promotionShadowRun, "ownerUserId": "u-alice", "status": runStatusWaiting,
				"waitingOn": map[string]any{"kind": "approval", "subject": promotionApprovalId},
			})
			out, err := i.handleDecideApproval(callerContext("u-alice"), map[string]any{
				"approvalId": promotionApprovalId, "decision": decision,
			}, 0)
			if err != nil {
				t.Fatalf("decideApproval: %v", err)
			}
			if n := len(eng.callsTo("updateWorkRun")); n != 0 {
				t.Errorf("deciding a promotion wrote the shadow run %d time(s)", n)
			}
			if got := decodeReply(t, out)["runResumed"]; got != false {
				t.Errorf("runResumed = %v, want false: nothing is parked on a promotion", got)
			}
		})
	}
}
