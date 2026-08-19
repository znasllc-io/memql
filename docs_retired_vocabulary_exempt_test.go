package main

import "testing"

// The escape hatch added in memql#4125 has no users in the tree yet, so
// TestRetiredVocabulary's own sweep cannot demonstrate it works -- a
// zero-hit exemption path is indistinguishable from a broken one. This
// pins the behaviour directly.
func TestLineIsVocabExempt(t *testing.T) {
	banned := "run `make release VERSION=0.19.1` to inspect the image locally"

	cases := []struct {
		name string
		line string
		want bool
	}{
		{"no marker", banned, false},
		{"marker with reason", banned + " <!-- retired-vocabulary-ok: local inspection, memql#4116 -->", true},
		{"marker with no reason", banned + " <!-- retired-vocabulary-ok: -->", false},
		{"marker unterminated", banned + " <!-- retired-vocabulary-ok: local inspection", false},
		{"marker name alone is not enough", banned + " retired-vocabulary-ok", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lineIsVocabExempt(tc.line); got != tc.want {
				t.Errorf("lineIsVocabExempt(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}
