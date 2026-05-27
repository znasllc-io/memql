package cognition

import "testing"

// presenceRecordId must return a bare shortId so the engine's
// Concept.validateShortId accepts the insert. Regression guard for
// memql#392 / copresent#114 -- before this fix, the function returned
// the colon-bearing participant id verbatim, which failed
// validateShortId and made every upsert silently no-op.
func TestPresenceRecordId(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"canonical participant id", "v1:cognition:participant:sofia", "sofia"},
		{"uuid short", "v1:cognition:participant:abc-123-def", "abc-123-def"},
		{"already-bare", "sofia", "sofia"},
		{"whitespace trimmed", "  v1:cognition:participant:sofia  ", "sofia"},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := presenceRecordId(tc.input)
			if got != tc.want {
				t.Fatalf("presenceRecordId(%q) = %q; want %q", tc.input, got, tc.want)
			}
			if got != "" && containsColon(got) {
				t.Fatalf("presenceRecordId(%q) returned %q which still has a colon -- validateShortId would reject", tc.input, got)
			}
		})
	}
}

func containsColon(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			return true
		}
	}
	return false
}
