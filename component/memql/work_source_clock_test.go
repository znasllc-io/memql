package memql

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSourceClocksKeepOneInstantAcrossMidnightAndDST(t *testing.T) {
	for _, tc := range []struct{ utc, zone, want string }{
		{"2026-10-18T03:12:00Z", "America/Phoenix", "2026-10-17T20:12:00-07:00"},
		{"2026-10-18T03:12:00Z", "Asia/Kolkata", "2026-10-18T08:42:00+05:30"},
		{"2026-11-01T05:30:00Z", "America/New_York", "2026-11-01T01:30:00-04:00"},
		{"2026-11-01T06:30:00Z", "America/New_York", "2026-11-01T01:30:00-05:00"},
	} {
		t.Run(tc.utc+"/"+tc.zone, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, tc.utc)
			require.NoError(t, err)
			// Preserve nested JSON stdout from an arbitrary service tool.
			body, err := json.Marshal(map[string]any{"stdout": `{"timeZone":"` + tc.zone + `","observedAt":"old source timestamp"}`})
			require.NoError(t, err)
			sources := []any{map[string]any{"value": string(body)}}
			before, _ := json.Marshal(sources)
			clock := workSourceClock(sources, now)
			require.Equal(t, tc.utc, clock["currentTimeUTC"])
			require.Equal(t, map[string]string{tc.zone: tc.want}, clock["sourceZoneClocks"])
			converted, err := time.Parse(time.RFC3339, clock["sourceZoneClocks"].(map[string]string)[tc.zone])
			require.NoError(t, err)
			require.True(t, converted.Equal(now), "displayed clocks must be exactly the same instant")
			after, _ := json.Marshal(sources)
			require.Equal(t, before, after, "clock grounding must never rewrite an observation or make it fresh")
		})
	}
}

func TestSourceClocksRefuseGuessedAndUnboundedZones(t *testing.T) {
	now := time.Date(2026, 10, 18, 3, 12, 0, 0, time.UTC)
	sources := []any{
		map[string]any{"arguments": `{"timezone":"Asia/Tokyo"}`, "value": "The location is Catalina Foothills, Arizona; timezone America/Phoenix."},
		map[string]any{"value": `text before {"timezone":"Asia/Tokyo"}`},
		map[string]any{"value": `{"timezone":"Local"}`},
		map[string]any{"value": `{"timezone":"EST"}`},
		map[string]any{"value": `{"timezone":"../../etc/passwd"}`},
		map[string]any{"value": `{"timezone":"Not/AZone"}`},
		map[string]any{"value": `{"timezone":"Asia/Tokyo","padding":"` + strings.Repeat("x", 8000) + `"}`},
	}
	clock := workSourceClock(sources, now)
	require.Empty(t, clock["sourceZoneClocks"], "only complete bounded source JSON with explicit IANA zones qualifies")
	var nested any = map[string]any{"timezone": "Asia/Tokyo"}
	for range 12 {
		nested = []any{nested}
	}
	require.Empty(t, workSourceClock([]any{map[string]any{"value": nested}}, now)["sourceZoneClocks"])
	for _, zone := range []string{"UTC", "Europe/London", "Europe/Paris", "America/New_York", "America/Phoenix", "Asia/Tokyo", "Asia/Kolkata", "Australia/Sydney", "Pacific/Auckland"} {
		sources = append(sources, map[string]any{"value": map[string]any{"timezone": zone}})
	}
	require.Len(t, workSourceClock(sources, now)["sourceZoneClocks"], 8)
}
