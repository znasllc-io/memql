package memql

import (
	"strings"
	"testing"
	"testing/fstest"
)

// Helpers retained for in-package deprecated-use tests. Public offline lint
// integration tests run independently in the offline test package.
// diagsContain reports whether any diagnostic's message contains sub.
func diagsContain(diags []LintDiagnostic, sub string) bool {
	for _, d := range diags {
		if strings.Contains(d.Message, sub) {
			return true
		}
	}
	return false
}

// lint runs the parity pass over root with every fixture domain declaring the
// engine's own language line (memql#5357), so each test sees only the problem
// its fixture is about.
func lint(t *testing.T, root fstest.MapFS) []LintDiagnostic {
	t.Helper()
	diags, _, err := LintUnifiedTree(nil, withLanguageLines(root))
	if err != nil {
		t.Fatalf("LintUnifiedTree: %v", err)
	}
	return diags
}
