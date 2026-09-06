package memql

import "testing"

// testWriter routes an slog stream into the test log, so a handler built for a
// test does not print to stderr.
//
// It lived in prompt_cognition_compaction_test.go, which went with the
// cognition prompts (epic memql#4988).
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}
