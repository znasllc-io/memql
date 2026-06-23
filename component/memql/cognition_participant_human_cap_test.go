package memql

import "testing"

// TestHumanInsertIsActive covers the status-to-active mapping used by
// the human-cap guard: empty status defaults to active (mirrors
// joinSpaceAsHuman's coalesce(status, "active")); only "left"
// / "idle" and friends are inactive.
func TestHumanInsertIsActive(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{"", true},
		{"active", true},
		{"ACTIVE", true},
		{"Active", true},
		{"left", false},
		{"idle", false},
		{"pending", false},
	}
	for _, c := range cases {
		if got := humanInsertIsActive(c.status); got != c.want {
			t.Errorf("humanInsertIsActive(%q) = %v, want %v", c.status, got, c.want)
		}
	}
}

// TestHumanCapExceeded verifies the pure cap arithmetic: adding one more
// active human is allowed up to and including maxHumans, rejected beyond.
func TestHumanCapExceeded(t *testing.T) {
	cases := []struct {
		name        string
		activeCount int
		maxHumans   int
		want        bool
	}{
		{"empty space, cap 5", 0, 5, false},
		{"4 present, cap 5 -> 5th ok", 4, 5, false},
		{"5 present, cap 5 -> 6th rejected", 5, 5, true},
		{"6 present, cap 5 -> rejected", 6, 5, true},
		{"0 present, cap 1 -> ok", 0, 1, false},
		{"1 present, cap 1 -> rejected", 1, 1, true},
	}
	for _, c := range cases {
		if got := humanCapExceeded(c.activeCount, c.maxHumans); got != c.want {
			t.Errorf("%s: humanCapExceeded(%d, %d) = %v, want %v",
				c.name, c.activeCount, c.maxHumans, got, c.want)
		}
	}
}
