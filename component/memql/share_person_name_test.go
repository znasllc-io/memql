package memql

import "testing"

// An invited person's display name starts as their email; the share
// directory and every share record must show the part before the "@"
// instead, never the address (Jesús, Nov 18 2026).
func TestShareNamesNeverCarryAnEmail(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"share-viewer2@example.test", "share-viewer2"},
		{"  ana@example.org ", "ana"},
		{"Ana Ruiz", "Ana Ruiz"},
		{"Ana @ ZNAS", "Ana @ ZNAS"},
		{"@handle", "@handle"},
		{"a@b@c", "a@b@c"},
		{"", ""},
	} {
		if got := NameWithoutEmail(tc.in); got != tc.want {
			t.Errorf("NameWithoutEmail(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if got := sharePersonName(map[string]any{"displayName": "dana@example.test"}); got != "dana" {
		t.Errorf("sharePersonName with an email display name = %q, want %q", got, "dana")
	}
	if got := sharePersonName(map[string]any{"displayName": "", "firstName": "Lee", "lastName": "Park"}); got != "Lee Park" {
		t.Errorf("sharePersonName fallback = %q", got)
	}
}
