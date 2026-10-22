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
		{"usage limit try again at Oct 26th, 2026 11:32 PM", ""},
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
	require.True(t, appQuotaUntil(row).After(at.Add(48*time.Hour)))
	row["errorMessage"] = "out of credits"
	require.Equal(t, at.Add(15*time.Minute), appQuotaUntil(row))
	row["status"] = "ended"
	require.True(t, appQuotaUntil(row).IsZero())
	require.Equal(t, "2027-01-01T23:00:00Z", AppQuotaReset("resets Jan 1 at 4pm (America/Phoenix)", time.Date(2026, 12, 31, 12, 0, 0, 0, time.UTC)).UTC().Format(time.RFC3339))
}
