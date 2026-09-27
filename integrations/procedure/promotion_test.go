package procedure

import (
	"context"
	"testing"

	"github.com/znasllc-io/memql/component/work"
)

// promotion_test.go -- applying the one human decision (epic memql#5408, D3;
// plan Task 5 step 6). The decision is made in integrations/work; the
// onProcedurePromotionDecided automation, running as the cluster's
// maintenance principal, applies it here.

const promotionId = "v1:work:approval:p1"

// promotionWorld is a shadow procedure waiting on a decided approval.
func promotionWorld(t *testing.T, decision, artifactHash string) *replayWorld {
	t.Helper()
	w := newReplayWorld(t, "shadow")
	w.lc.set("promotionApprovalId", promotionId)
	w.lc.set("shadowMatches", float64(5))
	w.lc.set("distinctBindings", map[string]any{"s0.command.7": []any{work.BindingDigest("a"), work.BindingDigest("b")}})
	if artifactHash == "" {
		artifactHash = w.hash
	}
	w.eng.replyWhen("workApprovalById", `"`+promotionId+`"`, map[string]any{
		"id": promotionId, "kind": work.ApprovalKindProcedurePromotion, "decision": decision,
		"ownerUserId": replayOwner, "artifactHash": artifactHash, "runId": "v1:work:run:shadow1",
		"subject": map[string]any{"constructId": w.constructId, "procedureHash": artifactHash},
	})
	return w
}

func decideAsCluster(t *testing.T, w *replayWorld) work.Transition {
	t.Helper()
	tr, err := w.i.DecidePromotion(maintenanceCtx("onProcedurePromotionDecided"), promotionId)
	if err != nil {
		t.Fatalf("DecidePromotion: %v", err)
	}
	return tr
}

// TestAnApprovedPromotionMovesShadowToCanaryOnce (D3): a person's yes moves the
// procedure to canary -- the approval id read INTO the state Advance decides
// on, and cleared by it -- and a second delivery of the same decision finds
// nothing waiting on it and changes nothing.
func TestAnApprovedPromotionMovesShadowToCanaryOnce(t *testing.T) {
	w := promotionWorld(t, "approved", "")
	tr := decideAsCluster(t, w)
	if tr.From != work.RungShadow || tr.To != work.RungCanary {
		t.Fatalf("transition = %+v, want shadow to canary", tr)
	}
	if w.lc.get("ladder") != "canary" || w.lc.get("promotionApprovalId") != "" || w.lc.get("canaryMatches") != float64(0) {
		t.Fatalf("construct = ladder %v, approval %v, canaryMatches %v", w.lc.get("ladder"), w.lc.get("promotionApprovalId"), w.lc.get("canaryMatches"))
	}
	read := w.eng.callTo(t, "workApprovalById")
	if !read.Internal || !read.Synthetic {
		t.Fatalf("the approval was read internal=%v synthetic=%v; it is the principal's stamped read", read.Internal, read.Synthetic)
	}
	for _, c := range w.eng.writes() {
		if c.Actor != replayOwner || !c.Internal {
			t.Fatalf("%s written as %q (internal %v), want the construct's owner", c.Name(), c.Actor, c.Internal)
		}
	}

	again := decideAsCluster(t, w)
	if again.From != "" || len(w.eng.callsTo("recordConstructLadder")) != 1 {
		t.Fatalf("a second delivery moved the ladder again: %+v", again)
	}
}

// TestARejectedPromotionKeepsShadow: a no is final for the evidence it was
// shown -- the procedure stays in shadow, its streak spent and the approval
// cleared, so only NEW matches may propose again.
func TestARejectedPromotionKeepsShadow(t *testing.T) {
	w := promotionWorld(t, "rejected", "")
	tr := decideAsCluster(t, w)
	if tr.To != work.RungShadow || w.lc.get("ladder") != "shadow" {
		t.Fatalf("transition = %+v, want it kept in shadow", tr)
	}
	if w.lc.get("shadowMatches") != float64(0) || w.lc.get("promotionApprovalId") != "" {
		t.Fatalf("streak %v / approval %v, want both spent", w.lc.get("shadowMatches"), w.lc.get("promotionApprovalId"))
	}
	if b, _ := w.lc.get("distinctBindings").(map[string]any); len(b) != 0 {
		t.Fatalf("distinctBindings = %v, want cleared", b)
	}
}

// TestADecisionNothingIsWaitingOnChangesNothing: an approval the construct no
// longer points at (a re-lift superseded it), a construct no longer in
// shadow, a yes to a version the construct no longer is, and an undecided
// approval all leave the ladder exactly where it was.
func TestADecisionNothingIsWaitingOnChangesNothing(t *testing.T) {
	for name, set := range map[string]func(w *replayWorld){
		"superseded":          func(w *replayWorld) { w.lc.set("promotionApprovalId", "v1:work:approval:another") },
		"no longer in shadow": func(w *replayWorld) { w.lc.set("ladder", "trusted") },
		"undecided": func(w *replayWorld) {
			w.eng.conditional = nil
			w.eng.replyWhen("workApprovalById", promotionId, map[string]any{"id": promotionId, "kind": work.ApprovalKindProcedurePromotion, "decision": "", "ownerUserId": replayOwner, "subject": map[string]any{"constructId": w.constructId}})
		},
		"not a promotion": func(w *replayWorld) {
			w.eng.conditional = nil
			w.eng.replyWhen("workApprovalById", promotionId, map[string]any{"id": promotionId, "kind": "budget", "decision": "approved", "ownerUserId": replayOwner})
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := promotionWorld(t, "approved", "")
			set(w)
			decideAsCluster(t, w)
			if n := len(w.eng.callsTo("recordConstructLadder")); n != 0 {
				t.Fatalf("the ladder moved %d time(s)", n)
			}
		})
	}
	t.Run("a yes to a version the construct no longer is", func(t *testing.T) {
		w := promotionWorld(t, "approved", "sha256:the-version-a-person-saw")
		decideAsCluster(t, w)
		if w.lc.get("ladder") != "shadow" || len(w.eng.callsTo("recordConstructLadder")) != 0 {
			t.Fatalf("an approval of another version promoted this one to %v", w.lc.get("ladder"))
		}
	})
}

// TestAPersonCannotApplyAPromotion: applying a decision is the cluster's act,
// fired when the approval is decided; a person decides in Approvals.
func TestAPersonCannotApplyAPromotion(t *testing.T) {
	w := promotionWorld(t, "approved", "")
	if _, err := w.i.DecidePromotion(personCtx(replayOwner), promotionId); err == nil {
		t.Fatal("a person applied a promotion decision")
	}
	if _, err := w.i.DecidePromotion(context.Background(), promotionId); err == nil {
		t.Fatal("a caller with no actor applied a promotion decision")
	}
	if len(w.eng.recorded()) != 0 {
		t.Fatalf("a refused decision reached the engine: %s", w.eng.summary())
	}
}
