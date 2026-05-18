package timeutil

import (
	"context"
	"encoding/json"
	"testing"
)

// TestDateKeyInTimezone pins the IANA-tz date conversion across a few
// timezone + reference-time pairs that span midnight in the target zone
// while sitting safely inside the prior/next UTC day. Locks the
// "today is what your wall clock says it is" semantic the daily-space
// automations depend on.
func TestDateKeyInTimezone(t *testing.T) {
	in := NewIntegration()
	ctx := context.Background()

	cases := []struct {
		name     string
		tz       string
		now      string
		wantKey  string
		wantTzUz string
	}{
		{
			name:     "UTC noon is the UTC day",
			tz:       "",
			now:      "2026-05-18T12:00:00Z",
			wantKey:  "2026-05-18",
			wantTzUz: "UTC",
		},
		{
			// 06:00 UTC == 23:00 previous day in Los Angeles.
			name:     "LA late-evening lands on the prior day",
			tz:       "America/Los_Angeles",
			now:      "2026-05-19T06:00:00Z",
			wantKey:  "2026-05-18",
			wantTzUz: "America/Los_Angeles",
		},
		{
			// 14:00 UTC == 23:00 same day in Tokyo (UTC+9). Boundary
			// in JST is well past midnight, so the JST date matches.
			name:     "Tokyo evening matches the JST day",
			tz:       "Asia/Tokyo",
			now:      "2026-05-18T14:00:00Z",
			wantKey:  "2026-05-18",
			wantTzUz: "Asia/Tokyo",
		},
		{
			name:     "garbage timezone falls back to UTC",
			tz:       "Not/A/Real/Zone",
			now:      "2026-05-18T12:00:00Z",
			wantKey:  "2026-05-18",
			wantTzUz: "UTC",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := in.handleDateKeyInTimezone(ctx, map[string]any{
				"timezone": tc.tz,
				"now":      tc.now,
			}, 0)
			if err != nil {
				t.Fatalf("handleDateKeyInTimezone returned err: %v", err)
			}
			if len(res) != 1 {
				t.Fatalf("expected 1 node, got %d", len(res))
			}
			var payload map[string]any
			if err := json.Unmarshal(res[0].Payload, &payload); err != nil {
				t.Fatalf("payload not JSON: %v", err)
			}
			if got := payload["dateKey"]; got != tc.wantKey {
				t.Errorf("dateKey = %v, want %s", got, tc.wantKey)
			}
			if got := payload["tzUsed"]; got != tc.wantTzUz {
				t.Errorf("tzUsed = %v, want %s", got, tc.wantTzUz)
			}
		})
	}
}

// TestDateKeyInTimezone_DefaultsToNow asserts that omitting `now`
// returns *something* shaped like YYYY-MM-DD without erroring -- the
// production automations will call it without a fixed `now`.
func TestDateKeyInTimezone_DefaultsToNow(t *testing.T) {
	in := NewIntegration()
	res, err := in.handleDateKeyInTimezone(context.Background(), map[string]any{
		"timezone": "America/New_York",
	}, 0)
	if err != nil {
		t.Fatalf("returned err: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(res[0].Payload, &payload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	key, _ := payload["dateKey"].(string)
	if len(key) != 10 || key[4] != '-' || key[7] != '-' {
		t.Errorf("dateKey %q not YYYY-MM-DD shaped", key)
	}
}
