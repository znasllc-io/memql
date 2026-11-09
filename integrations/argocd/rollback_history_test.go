package argocd

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// plannedReversal applies a forward intent, finishes it, and plans its
// history-backed reversal the way the installation journal does.
func plannedReversal(t *testing.T) (*continuityFixture, *Client, Intent, Intent) {
	t.Helper()
	app, _, prior := rollbackFixture(t)
	app.change(t, func(obj map[string]any) { finishOperation(obj, "Succeeded") })
	f, history := historyClient(t, app)
	reversal, err := history.PlanRollback(context.Background(), prior, "explicit-rollback")
	require.NoError(t, err)
	require.Equal(t, 2, reversal.FormatVersion)
	require.Equal(t, "42", reversal.BeforeResourceVersion)
	require.Equal(t, "100", reversal.WatchResourceVersion)
	return f, history, prior, reversal
}

func statusWrite(f *continuityFixture, version string, generation int64) {
	f.modify(version, generation, func(o map[string]any) {
		object(o, "status")["reconciledAt"] = "2026-11-09T12:00:0" + version[len(version)-1:] + "Z"
	})
}

// Fix for Restore's writes refusing forever: an Application CRD without a
// status subresource advances metadata.generation on Argo's status writes.
// A reversal bound to the exact generation it was planned at refused its
// patch after one such write, and every retry refused the same way. A
// history-backed reversal accepts those status-only advances.
func TestReversalPatchSurvivesStatusOnlyGenerationAdvances(t *testing.T) {
	f, c, prior, reversal := plannedReversal(t)
	statusWrite(f, "43", 9)
	statusWrite(f, "44", 10)
	_, err := c.Apply(context.Background(), reversal)
	require.ErrorContains(t, err, "fresh complete Application history", "no history proof, no patch")
	require.Equal(t, 1, f.application.writeCount())

	facts, err := applyReversal(c, reversal)
	require.NoError(t, err)
	require.True(t, facts.IntentObserved)
	require.Equal(t, 2, f.application.writeCount())
	require.Equal(t, strings.Repeat("b", 40), stringAt(object(object(f.application.snapshot(), "spec"), "source"), "targetRevision"))
	// The reversal's marker now fences the superseded forward intent.
	_, err = c.Apply(context.Background(), prior)
	require.ErrorIs(t, err, ErrChanged)
	require.Equal(t, 2, f.application.writeCount())
}

// The strict reversal this replaces, for contrast: planned from a plain read,
// it refuses the same status-only advance on every attempt.
func TestStrictReversalRefusesAStatusOnlyGenerationAdvance(t *testing.T) {
	app, _, prior := rollbackFixture(t)
	app.change(t, func(obj map[string]any) { finishOperation(obj, "Succeeded") })
	plain := app.client(t)
	snapshot, err := plain.Read(context.Background(), prior.Target)
	require.NoError(t, err)
	strict, err := PlanRevision(snapshot, "explicit-rollback", strings.Repeat("b", 40), prior.Prune)
	require.NoError(t, err)
	require.Equal(t, 1, strict.FormatVersion)
	f, history := historyClient(t, app)
	statusWrite(f, "43", 9)
	for range 2 {
		_, err = history.Apply(context.Background(), strict)
		require.ErrorIs(t, err, ErrChanged)
	}
	require.Equal(t, 1, app.writeCount())
}

// A real edit by someone else between planning and the patch still refuses,
// including one restored before the patch, and no proof means no write.
func TestReversalPatchStillRefusesAnotherWritersSpecChange(t *testing.T) {
	for _, fault := range []string{"spec", "away and back", "marker", "operation", "history unavailable"} {
		t.Run(fault, func(t *testing.T) {
			f, c, _, reversal := plannedReversal(t)
			original := f.application.snapshot()
			switch fault {
			case "spec", "away and back":
				f.modify("43", 9, func(o map[string]any) { object(object(o, "spec"), "source")["path"] = "deploy/elsewhere" })
				if fault == "away and back" {
					f.modify("44", 10, func(o map[string]any) { o["spec"] = original["spec"] })
				}
			case "marker":
				f.modify("43", 8, func(o map[string]any) {
					object(object(o, "metadata"), "annotations")[intentAnnotation] = "another-intent"
				})
			case "operation":
				f.modify("43", 8, func(o map[string]any) {
					o["operation"] = map[string]any{"initiatedBy": map[string]any{"username": "admin"}, "sync": map[string]any{}}
				})
			case "history unavailable":
				statusWrite(f, "43", 9)
				f.watchError = true
			}
			_, err := applyReversal(c, reversal)
			require.Error(t, err)
			if fault == "history unavailable" {
				require.ErrorIs(t, err, ErrHistoryUnavailable)
			} else {
				require.ErrorIs(t, err, ErrChanged)
			}
			require.Equal(t, 1, f.application.writeCount(), "no reversal write")
		})
	}
}

// With its history gone, a reversal is still proved while the generation has
// not moved since planning: only metadata can have changed, as the strict fence
// it replaced accepted. A generation that moved, or a spec that differs, still
// refuses, and nothing is written.
func TestReversalWithoutHistoryNeedsTheGenerationItWasPlannedAt(t *testing.T) {
	for _, change := range []string{"label written", "status written", "spec differs"} {
		t.Run(change, func(t *testing.T) {
			f, c, _, reversal := plannedReversal(t)
			switch change {
			case "label written":
				f.modify("43", 8, func(o map[string]any) {
					meta := object(o, "metadata")
					meta["labels"] = map[string]any{"example.com/owner": "platform"}
				})
			case "status written":
				statusWrite(f, "43", 9)
			case "spec differs":
				f.modify("43", 8, func(o map[string]any) { object(object(o, "spec"), "source")["path"] = "deploy/elsewhere" })
			}
			f.watchError = true
			facts, err := applyReversal(c, reversal)
			switch change {
			case "label written":
				require.NoError(t, err)
				require.True(t, facts.IntentObserved)
				require.Equal(t, 2, f.application.writeCount())
			case "status written":
				require.ErrorIs(t, err, ErrHistoryUnavailable)
				require.Equal(t, 1, f.application.writeCount())
			default:
				require.ErrorIs(t, err, ErrChanged)
				require.Equal(t, 1, f.application.writeCount())
			}
		})
	}
}

// Recovery of an applied reversal needs no history: its own marker matches.
func TestAppliedReversalRecoversWithoutHistory(t *testing.T) {
	f, c, _, reversal := plannedReversal(t)
	_, err := applyReversal(c, reversal)
	require.NoError(t, err)
	statusWrite(f, "44", 9)
	f.watchError = true
	facts, err := c.Apply(context.Background(), reversal)
	require.NoError(t, err)
	require.True(t, facts.IntentObserved)
	require.Equal(t, 2, f.application.writeCount())
}
