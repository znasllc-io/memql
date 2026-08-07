package dslconformance

import (
	"github.com/znasllc-io/memql/dsl"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/dslfs"
)

// retiredBuiltinCallRe matches call-forms of the eight expression builtins
// hard-retired under the 2026.08 epoch (#2620 ruling / #2707): the calendar
// extractors/predicates, memqlVersion, and subtractTimestamps (whose
// replacement is addDuration with a negative duration). The `\b` keeps
// identifiers that merely end in one of the names (e.g. `halfYear(`) from
// matching.
var retiredBuiltinCallRe = regexp.MustCompile(
	`\b(year|quarter|month|dayOfMonth|isAnniversary|isFirstDayOfQuarter|memqlVersion|subtractTimestamps)\(`)

// TestNoRetiredBuiltinCallForms is the #2707 corpus lock-in, the same
// belt-and-suspenders shape as TestNoClockCallForms: the parser already
// rejects the retired names with a migration hint, but this gives a clear
// tree-wide file:line report instead of a single parse error during load,
// and documents the contract at the source. The registry-executor builtin
// `builtin serviceVersion` (@alias memqlVersion) in common/builtins.memql is
// a different construct -- the meta-command surface -- and does not use the
// call-form this regex matches.
func TestNoRetiredBuiltinCallForms(t *testing.T) {
	tree := dsl.Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	for _, p := range paths {
		file, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(file)
		file.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			// Strip line comments -- prose may legitimately mention the
			// retired names; only authored code counts.
			if idx := strings.Index(line, "//"); idx >= 0 {
				line = line[:idx]
			}
			if m := retiredBuiltinCallRe.FindString(line); m != "" {
				t.Errorf("%s:%d: retired builtin call-form %q -- retired under the 2026.08 epoch (#2620 ruling / #2707)", p, i+1, m)
			}
		}
	}
}
