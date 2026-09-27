package procedure

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/work"
)

// ownerDislikes answers workDescriptionGuidance as the owned query does: the
// goal signature's dislikes, to their owner alone.
func ownerDislikes(t *testing.T, w *replayWorld, reason string) {
	t.Helper()
	w.eng.answerSome("workDescriptionGuidance", func(c recordedCall, _ string) ([]map[string]any, string, bool) {
		sig, _ := parseCallArgs(t, c.Query)["goalSignature"].(string)
		if sig != testSignature || !sameUser(replayOwner, c.Actor) {
			return nil, "", true
		}
		return []map[string]any{{
			"id": "v1:work:observation:dislike-1", "kind": "feedback", "ownerUserId": replayOwner,
			"data": map[string]any{"verdict": "dislike", "reason": reason, "goalSignature": testSignature,
				"axes": map[string]any{"product": true, "process": false, "performance": false}},
		}}, "", true
	})
}

// A goal handed back to the app carries the owner's earlier dislikes on the
// goal (epic memql#5414, D23). Text reaches only a model call, and the app
// taking the goal back is one; the replay that stopped read rows.
func TestAHandBackCarriesTheGoalsDescriptionGuidance(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	ownerDislikes(t, w, "the report left out the refunds")
	w.d.alter["step1"] = func(r *DispatchResult) {
		r.Observation.Contents[0].Digest = "sha256:" + strings.Repeat("0", 64)
	}
	out := w.serve(t, ReplayTrusted, map[string]any{"file": goalFile})
	if !out.Diverged || !out.FellBack {
		t.Fatalf("outcome = %+v, want a divergence handed back to the app", out)
	}
	g := w.f.recorded()[0].Guidance
	want := work.DescriptionGuidanceHeading + "\n- (product) the report left out the refunds"
	if g.DescriptionGuidance != want {
		t.Fatalf("guidance = %q, want the owner's dislike read under their actor", g.DescriptionGuidance)
	}
	if !strings.HasSuffix(g.Prompt, "\n\n"+want) || !strings.Contains(g.Prompt, "It stopped at step 2 because:") {
		t.Fatalf("the prompt does not close on the owner's dislikes:\n%s", g.Prompt)
	}
}

// "Never into a replay" (D23): a replay that serves the goal reads no
// description guidance at all, because nothing it does can act on text.
func TestAServedReplayNeverReadsDescriptionGuidance(t *testing.T) {
	w := newReplayWorld(t, "trusted")
	ownerDislikes(t, w, "the report left out the refunds")
	out := w.serve(t, ReplayTrusted, map[string]any{"file": goalFile})
	if !out.Served || out.FellBack {
		t.Fatalf("outcome = %+v, want the goal served by the replay", out)
	}
	for _, c := range w.eng.recorded() {
		if c.Name() == "workDescriptionGuidance" {
			t.Fatalf("a served replay read the goal's description guidance: %s", c.Query)
		}
	}
}

// A hand-back before any step ran says nothing was done, and still carries
// the owner's dislikes, as its own paragraph after the diagnosis.
func TestAHandBackThatRanNothingStillCarriesTheDislikes(t *testing.T) {
	guidance := work.DescriptionGuidanceHeading + "\n- (process) it asked me three times"
	got := renderGuidance("Summarise the invoices.", Guidance{Diagnosis: "the input file is missing.", DescriptionGuidance: guidance}, false, 0)
	want := "Summarise the invoices.\n\nA learned procedure for this goal did not start, so nothing has been done yet: the input file is missing.\n\n" + guidance
	if got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if got := renderGuidance("Summarise the invoices.", Guidance{Diagnosis: "the input file is missing."}, false, 0); strings.HasSuffix(got, "\n\n") {
		t.Fatalf("no dislikes must add no empty paragraph: %q", got)
	}
}
