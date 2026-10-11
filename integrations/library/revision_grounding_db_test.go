package library

import (
	"errors"
	"reflect"
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

// A quote can exist while the objection about it is false. Reconsider the
// objection before changing correct prose, including after a real repair.
func testRevisionReconsidersContradictoryReview(t *testing.T, shared *revisionDB) {
	for _, afterRepair := range []bool{false, true} {
		t.Run(map[bool]string{false: "initial", true: "after_repair"}[afterRepair], func(t *testing.T) {
			f := shared.isolatedCase(t)
			artifact, doc := f.document("# Report\n\n**Profile**\n\nKeep this.\n")
			args, note := f.submit(artifact, doc, map[string]any{"kind": "document"}, "Promote Profile to a level-three heading; preserve the body.")
			request := asString(args["requestId"])
			f.ai.answer = revisionAnswer{Summary: "Promoted heading", Edits: []revisionReplacement{{Before: "**Profile**", After: "### Profile", Reason: "Requested heading", CommentIDs: []string{note}}}}
			candidate := "# Report\n\n### Profile\n\nKeep this.\n"
			falseFinding := map[string]any{"findings": []any{map[string]any{"kind": "present_text", "quote": "### Profile", "explanation": "The title is a bold paragraph rather than a level-three heading."}}}
			f.ai.completionAnswers = []map[string]any{falseFinding, {"findings": []any{}}}
			f.ai.completionDocuments = []string{candidate, candidate}
			expectedEdits, expectedChecks := int32(1), int32(2)
			if afterRepair {
				f.ai.firstAnswer = &revisionAnswer{Summary: "Unchanged", Edits: []revisionReplacement{}}
				f.ai.completionReply = "The requested heading is absent."
				missing := map[string]any{"findings": []any{map[string]any{"kind": "missing_requirement", "quote": "", "explanation": f.ai.completionReply}}}
				f.ai.completionAnswers = append([]map[string]any{missing, missing}, f.ai.completionAnswers...)
				original := "# Report\n\n**Profile**\n\nKeep this.\n"
				f.ai.completionDocuments = append([]string{original, original}, f.ai.completionDocuments...)
				expectedEdits, expectedChecks = 2, 4
			}
			err := f.execute(f.other, request, false)
			var wait *workstate.HumanWait
			if !errors.As(err, &wait) || f.ai.calls.Load() != expectedEdits || f.ai.completionCalls.Load() != expectedChecks || f.ai.repairCalls.Load() != expectedEdits-1 {
				t.Fatalf("false finding rewrote correct prose or escaped the bounded review: edit=%d assess=%d repair=%d err=%v", f.ai.calls.Load(), f.ai.completionCalls.Load(), f.ai.repairCalls.Load(), err)
			}
			if !reflect.DeepEqual(f.ai.completionFindings[expectedChecks-1], falseFinding["findings"]) {
				t.Fatal("reassessment lost the objection being challenged")
			}
			if !strings.Contains(f.ai.completionRecoveries[expectedChecks-1], "independently") {
				t.Fatal("reassessment did not challenge the previous review")
			}
			f.query(f.engine, f.ctx, "query", "decideApproval", map[string]any{"approvalId": wait.ApprovalID, "decision": "approved", "answer": f.accepted(request)})
			if err = f.execute(f.engine, request, true); err != nil {
				t.Fatal(err)
			}
			saved, err := f.second.reviewDocument(memql.ContextWithFreshRead(f.ctx), artifact)
			if err != nil || asString(saved.backing["body"]) != candidate {
				t.Fatalf("saved something other than the assessed candidate: %v", err)
			}
		})
	}
}
