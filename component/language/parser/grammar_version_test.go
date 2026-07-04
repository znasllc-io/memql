package parser

import "testing"

// The pinned fingerprint forces a CONSCIOUS GrammarVersion bump (and the
// accompanying memqlmigrate rewrite mode) whenever the author-facing keyword
// surface changes. If this test fails: you changed the grammar -- bump
// GrammarVersion in grammar_version.go, ship the migration mode, THEN update
// the pin (S6, memql#2361).
func TestGrammarFingerprintPinned(t *testing.T) {
	const pinned = "60badae8d3eab7d6"
	got := GrammarFingerprint()
	if pinned == "" {
		t.Logf("current fingerprint: %s", got)
		return
	}
	if got != pinned {
		t.Fatalf("grammar keyword surface changed (fingerprint %s != pinned %s) -- bump GrammarVersion + ship the memqlmigrate rewrite mode, then update the pin", got, pinned)
	}
}
