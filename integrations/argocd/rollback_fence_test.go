package argocd

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func unappliedRollbackFixture(t *testing.T) (*apiFixture, *Client, Intent, Intent) {
	t.Helper()
	f := newFixture(t)
	f.change(t, func(obj map[string]any) {
		object(object(obj, "spec"), "source")["targetRevision"] = strings.Repeat("b", 40)
	})
	c := f.client(t)
	forward := fixtureIntent(t, c)
	before, err := c.Read(t.Context(), forward.Target)
	require.NoError(t, err)
	fence, err := PlanUnappliedRollback(before, forward, "baseline-fence")
	require.NoError(t, err)
	require.NoError(t, ValidateUnappliedRollback(forward, fence))
	require.Equal(t, 0, f.writeCount())
	return f, c, forward, fence
}

func TestUnappliedRollbackFencesDelayedForwardAndRecoversLostResponse(t *testing.T) {
	f, c, forward, fence := unappliedRollbackFixture(t)
	f.configure(true, false)
	_, err := c.Apply(t.Context(), fence)
	require.Error(t, err, "fixture loses the response after applying the baseline fence")
	fresh := f.client(t)
	facts, err := fresh.Apply(t.Context(), fence)
	require.NoError(t, err)
	require.True(t, facts.IntentObserved)
	_, err = fresh.Apply(t.Context(), forward)
	require.ErrorIs(t, err, ErrChanged)
	require.Equal(t, 1, f.writeCount(), "late forward must not replace the fence")
}

func TestDelayedForwardWinsBeforeBaselineFenceRequiresExactTerminalReversal(t *testing.T) {
	f, c, forward, fence := unappliedRollbackFixture(t)
	_, err := c.Apply(t.Context(), forward)
	require.NoError(t, err)
	_, err = c.Apply(t.Context(), fence)
	require.ErrorIs(t, err, ErrChanged)
	_, err = c.PlanRollback(t.Context(), forward, "reconciled-reversal")
	require.Error(t, err, "an in-flight operation cannot be replaced")
	f.change(t, func(obj map[string]any) { finishOperation(obj, "Succeeded") })
	reversal, err := c.PlanRollback(t.Context(), forward, "reconciled-reversal")
	require.NoError(t, err)
	require.NoError(t, ValidateRollback(forward, reversal))
	require.Error(t, ValidateUnappliedRollback(forward, reversal))
	_, err = c.Apply(t.Context(), reversal)
	require.NoError(t, err)
	require.Equal(t, 2, f.writeCount())
	_, err = c.Apply(t.Context(), fence)
	require.ErrorIs(t, err, ErrChanged, "the original uncertain fence never rebinds")
}

func TestUnappliedRollbackRefusesChangedBaselineOrSubstitution(t *testing.T) {
	f, c, forward, fence := unappliedRollbackFixture(t)
	for _, fault := range []string{"target", "marker", "generation", "revision", "source", "request", "prune"} {
		t.Run(fault, func(t *testing.T) {
			changed := fence
			switch fault {
			case "target":
				changed.Target.UID = "replaced"
			case "marker":
				changed.BeforeMarker = "foreign"
			case "generation":
				changed.BeforeGeneration++
			case "revision":
				changed.Revision = strings.Repeat("f", 40)
			case "source":
				before, err := decodeObject(changed.BeforeSpec)
				require.NoError(t, err)
				object(before, "source")["path"] = "other"
				changed.BeforeSpec, err = canonical(before)
				require.NoError(t, err)
			case "request":
				changed.RequestID = forward.RequestID
			case "prune":
				changed.Prune = !changed.Prune
			}
			require.Error(t, ValidateUnappliedRollback(forward, changed))
		})
	}
	f.change(t, func(obj map[string]any) { object(object(obj, "spec"), "source")["path"] = "changed" })
	snapshot, err := c.Read(context.Background(), forward.Target)
	require.NoError(t, err)
	_, err = PlanUnappliedRollback(snapshot, forward, "baseline-fence")
	require.ErrorIs(t, err, ErrChanged)
	require.Equal(t, 0, f.writeCount())
}
