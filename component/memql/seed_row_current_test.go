package memql

import "testing"

func TestSeedArgsMatchPayload(t *testing.T) {
	payload := map[string]any{
		"name": "cap-owner-read-app-logs", "verb": "read", "resource": "app:logs/stream",
		"rank": float64(300), "aliases": []any{"writer"}, "nested": map[string]any{"a": float64(1)},
		"createdAt": "2026-09-13T16:20:00Z", "status": "active",
	}
	cases := []struct {
		name string
		args map[string]any
		want bool
	}{
		{"identical args, extra payload keys are fine", map[string]any{"name": "cap-owner-read-app-logs", "verb": "read", "resource": "app:logs/stream"}, true},
		{"a Go int compares as the JSON number it becomes", map[string]any{"rank": 300, "aliases": []string{"writer"}, "nested": map[string]any{"a": 1}}, true},
		{"the synthetic id arg is skipped", map[string]any{"capabilityId": "cap-owner-read-app-logs", "verb": "read"}, true},
		{"a changed value writes", map[string]any{"verb": "execute"}, false},
		{"a key the row lacks writes", map[string]any{"description": "new"}, false},
		{"a changed list writes", map[string]any{"aliases": []string{"writer", "reader"}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := seedArgsMatchPayload(c.args, payload, "capabilityId"); got != c.want {
				t.Fatalf("seedArgsMatchPayload = %v, want %v", got, c.want)
			}
		})
	}
}
