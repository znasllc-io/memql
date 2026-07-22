package dsl

import (
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql/dslfs"
)

// standaloneRetiredRe matches a line consisting solely of a retired
// construct-level annotation: @internal (#2620 ruling / #2708) or @role
// (#2631 BURY ruling / #2709). Whether such a line is the retired construct
// form or something legal depends on context: the concept parser attaches a
// standalone annotation line to the PRECEDING field, so a bare @internal
// between two concept fields is live @secret/@pii-family redaction
// vocabulary, not the retired construct form. The walk below therefore looks
// AHEAD: only a standalone retired annotation that (skipping comments and
// further leading annotations) heads a construct declaration is flagged.
var standaloneRetiredRe = regexp.MustCompile(`^\s*@(internal|role(\([^)]*\))?)\s*$`)

// constructKeywordAfterInternal matches the construct-declaration keywords a
// retired construct-level annotation could annotate.
var constructKeywordAfterInternal = regexp.MustCompile(
	`^\s*(query|mutation|mutate|logic|automation|builtin|tool|shape|spec|trait|prompt|provider|policy|capability|action|concept|seed)\b`)

// TestNoRetiredConstructAnnotations is the #2708 corpus lock-in, the
// TestNoClockCallForms shape: the load gate already rejects construct-level
// @internal with a migration hint, but this gives a clear tree-wide
// file:line report and documents the contract at the source.
func TestNoRetiredConstructAnnotations(t *testing.T) {
	tree := Tree()
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
		lines := strings.Split(string(raw), "\n")
		for i, line := range lines {
			if idx := strings.Index(line, "//"); idx >= 0 {
				line = line[:idx]
			}
			if !standaloneRetiredRe.MatchString(line) {
				continue
			}
			if headsConstructDecl(lines, i+1) {
				t.Errorf("%s:%d: retired construct-level annotation (@internal #2708 / @role #2709) -- delete the annotation", p, i+1)
			}
		}
	}
}

// headsConstructDecl reports whether the lines following index `from`
// (skipping blanks, comments, and further leading annotations) begin a
// construct declaration -- distinguishing the retired construct-level
// @internal from a legal field-level one inside a concept body.
func headsConstructDecl(lines []string, from int) bool {
	for _, l := range lines[from:] {
		trimmed := strings.TrimSpace(l)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "@") {
			continue
		}
		return constructKeywordAfterInternal.MatchString(l)
	}
	return false
}
