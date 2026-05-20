package memqlws

import (
	"testing"
)

// TestEnvOriginPatterns_CSVParsing exercises the F3 origin allow-list
// loading. The env variable MEMQL_WS_ORIGIN_PATTERNS is a
// comma-separated glob list; this test confirms that Apply respects the
// CSV semantics and drops empty entries.
func TestEnvOriginPatterns_CSVParsing(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{"single", "https://app.example.com", []string{"https://app.example.com"}},
		{"two", "https://app.example.com,https://admin.example.com", []string{"https://app.example.com", "https://admin.example.com"}},
		{"glob", "https://*.example.com,https://localhost:*", []string{"https://*.example.com", "https://localhost:*"}},
		{"empties", "https://a, , ,https://b", []string{"https://a", "https://b"}},
		{"whitespace", "  https://a  ,  https://b  ", []string{"https://a", "https://b"}},
		{"empty string", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := tt.raw
			opts := EnvOptions{OriginPatterns: &raw}
			target := Options{}
			if err := opts.Apply(&target); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			got := target.OriginPatterns
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d (got=%v)", len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestEnvOriginPatterns_UnsetLeavesUnchanged confirms that the legacy
// behaviour is preserved when the env var is unset entirely (Apply
// must not zero out an existing OriginPatterns on Options).
func TestEnvOriginPatterns_UnsetLeavesUnchanged(t *testing.T) {
	target := Options{OriginPatterns: []string{"https://caller-set.example.com"}}
	opts := EnvOptions{} // OriginPatterns left nil
	if err := opts.Apply(&target); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(target.OriginPatterns) != 1 || target.OriginPatterns[0] != "https://caller-set.example.com" {
		t.Fatalf("unset env should not zero out caller-provided OriginPatterns: got %v", target.OriginPatterns)
	}
}

// TestSanitizeOriginPatterns_StripsEmptyAndTrims confirms the New()
// path normalises operator-supplied patterns the same way the env
// loader does.
func TestSanitizeOriginPatterns_StripsEmptyAndTrims(t *testing.T) {
	got := sanitizeOriginPatterns([]string{"https://a", "", "  https://b  ", "  "})
	want := []string{"https://a", "https://b"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (got=%v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
