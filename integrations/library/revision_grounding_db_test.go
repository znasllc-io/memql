package library

import (
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
)

func testRevisionRejectsUngroundedReview(t *testing.T, shared *revisionDB) {
	for _, persistent := range []bool{false, true} {
		t.Run(map[bool]string{false: "reassess_without_editing", true: "bounded_refusal"}[persistent], func(t *testing.T) {
			f := shared.isolatedCase(t)
			artifact, doc := f.document("# Report\n\nKeep this. Unsupported old claim.\n")
			args, note := f.submit(artifact, doc, map[string]any{"kind": "document"}, "Remove the unsupported old claim, preserving the rest.")
			request := asString(args["requestId"])
			f.ai.answer = revisionAnswer{Summary: "Removed", Edits: []revisionReplacement{{Before: "Keep this. Unsupported old claim.", After: "Keep this.", Reason: "Remove unsupported claim", CommentIDs: []string{note}}}}
			bad := map[string]any{"findings": []any{map[string]any{"kind": "present_text", "quote": "Unsupported old claim.", "explanation": "The old claim still appears."}}}
			f.ai.completionAnswers = []map[string]any{bad, {"findings": []any{}}}
			if persistent {
				f.ai.completionAnswers[1] = bad
			}
			f.ai.completionDocuments = []string{"# Report\n\nKeep this.\n", "# Report\n\nKeep this.\n"}
			err := f.execute(f.other, request, false)
			if f.ai.calls.Load() != 1 || f.ai.repairCalls.Load() != 0 || f.ai.completionCalls.Load() != 2 {
				t.Fatalf("stale review drove document repair or bypassed reassessment: edit=%d repair=%d assess=%d err=%v", f.ai.calls.Load(), f.ai.repairCalls.Load(), f.ai.completionCalls.Load(), err)
			}
			if !strings.Contains(f.ai.completionRecoveries[1], "Unsupported old claim.") {
				t.Fatal("reassessment lost the measured absent quote")
			}
			if persistent {
				_, _, _, approval, _ := f.first.revisionRequest(memql.ContextWithFreshRead(f.ctx), request)
				if err == nil || !strings.Contains(err.Error(), "review findings could not be grounded") || approval != nil {
					t.Fatalf("invalid assessment reached approval: %v", err)
				}
				return
			}
			var wait *workstate.HumanWait
			if !errors.As(err, &wait) {
				t.Fatal(err)
			}
			f.query(f.engine, f.ctx, "query", "decideApproval", map[string]any{"approvalId": wait.ApprovalID, "decision": "approved", "answer": f.accepted(request)})
			if err = f.execute(f.engine, request, true); err != nil {
				t.Fatal(err)
			}
			saved, err := f.second.reviewDocument(memql.ContextWithFreshRead(f.ctx), artifact)
			if err != nil || asString(saved.backing["body"]) != f.ai.completionDocuments[0] {
				t.Fatalf("did not save the assessed candidate: %v", err)
			}
		})
	}
}
