package memql

import (
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestAppQuotaResetUsesReportedZoneAndSurvivesUsageWindow(t *testing.T) {
	at := time.Date(2026, 10, 21, 19, 40, 0, 0, time.UTC)
	for _, tt := range []struct{ message, want string }{
		{"You've hit your weekly limit · resets Oct 24 at 4pm (America/Phoenix)", "2026-10-24T23:00:00Z"},
		{"usage limit resets Oct 24 at 4:30pm (America/Phoenix)", "2026-10-24T23:30:00Z"},
		{"usage limit resets at 2026-10-24T16:00:00-07:00", "2026-10-24T23:00:00Z"},
		{"out of credits", ""},
		{"usage limit resets Oct 44 at 4pm (America/Phoenix)", ""},
		{"usage limit resets Oct 24 at 4pm (Unknown/Zone)", ""},
	} {
		t.Run(tt.message, func(t *testing.T) {
			got := AppQuotaReset(tt.message, at)
			if tt.want == "" {
				require.True(t, got.IsZero())
				return
			}
			require.Equal(t, tt.want, got.UTC().Format(time.RFC3339))
		})
	}
	row := map[string]any{"status": "failed", "errorMessage": "You've hit your weekly limit · resets Oct 24 at 4pm (America/Phoenix)", "endedAt": at.Format(time.RFC3339)}
	require.True(t, appRefusalUntil(row).After(at.Add(48*time.Hour)))
	row["errorMessage"] = "out of credits"
	require.Equal(t, at.Add(15*time.Minute), appRefusalUntil(row))
	row["status"] = "ended"
	require.True(t, appRefusalUntil(row).IsZero())
	require.Equal(t, "2027-01-01T23:00:00Z", AppQuotaReset("resets Jan 1 at 4pm (America/Phoenix)", time.Date(2026, 12, 31, 12, 0, 0, 0, time.UTC)).UTC().Format(time.RFC3339))
}

// Codex prints its reset in the MACHINE's zone and names no zone, so the engine
// reads the earliest instant that sentence could mean. Measured text, codex-cli
// 0.162.1, 2026-10-29: the machine read 2026-11-03T23:52:03-07:00.
const measuredCodexRefusal = "You\u2019ve hit your usage limit. Visit https://chatgpt.com/settings/usage to purchase more credits or try again at Nov 3rd, 2026 11:52 PM."

func TestAppQuotaResetReadsAnUnzonedDateAsItsEarliestInstant(t *testing.T) {
	at := time.Date(2026, 10, 30, 5, 0, 0, 0, time.UTC)
	got := AppQuotaReset(measuredCodexRefusal, at)
	require.Equal(t, "2026-11-03T09:52:00Z", got.UTC().Format(time.RFC3339))
	real := time.Date(2026, 11, 4, 6, 52, 3, 0, time.UTC)
	require.False(t, got.After(real), "a block must not outlast the reset it stands for")

	for _, message := range []string{
		"You've hit your usage limit. Try again at 11:52 PM.",
		"usage limit try again at Nov 3rd 11:52 PM",
		"usage limit try again at Foo 3rd, 2026 11:52 PM",
	} {
		require.True(t, AppQuotaReset(message, at).IsZero(), message)
	}
}

func TestAppRefusalUntilBlocksForTheDaysTheRefusalNames(t *testing.T) {
	at := time.Date(2026, 10, 30, 5, 0, 0, 0, time.UTC)
	row := map[string]any{"status": "failed", "errorMessage": measuredCodexRefusal, "endedAt": at.Format(time.RFC3339)}
	require.Equal(t, "2026-11-03T09:52:00Z", appRefusalUntil(row).UTC().Format(time.RFC3339))
}

func TestAppRefusalUntilRechecksAResetThatHasAlreadyPassed(t *testing.T) {
	// Between the earliest instant and the machine's real reset the app still
	// refuses, and the sentence still names the same date. A reset read as
	// already past would otherwise be no block at all, and every call would
	// meet the refusal again.
	at := time.Date(2026, 11, 3, 20, 0, 0, 0, time.UTC)
	row := map[string]any{"status": "failed", "errorMessage": measuredCodexRefusal, "endedAt": at.Format(time.RFC3339)}
	require.Equal(t, at.Add(15*time.Minute), appRefusalUntil(row))
}

func TestAppRefusalUntilBlocksASignedOutAppForTheBoundedRecheck(t *testing.T) {
	at := time.Date(2026, 10, 30, 5, 0, 0, 0, time.UTC)
	row := map[string]any{"status": "failed", "errorMessage": "harness: the claude turn exited 1: Failed to authenticate: OAuth session expired and could not be refreshed", "endedAt": at.Format(time.RFC3339)}
	require.Equal(t, at.Add(15*time.Minute), appRefusalUntil(row))
	row["status"] = "ended"
	require.True(t, appRefusalUntil(row).IsZero())
}
