package dsl

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql/dslfs"
)

// TestNoArgsFieldDescription keeps args{} blocks free of @description
// (#2615): the parser consumes the annotation and throws it away
// (args_block_parser.go: "silently accepted (no AST slot)"), so the prose
// is dead weight -- authors pay the tokens, the runtime gets nothing.
// Declaration-level and concept-field @description are load-bearing and
// untouched; only annotations INSIDE an args block are gated. Per-field
// documentation returns properly with the /// doc-comment epic (#2601).
//
// _reference/ is structurally outside this gate (underscore walker skip +
// embed omission). Block comments are blanked before scanning so prose
// inside /* */ never fails the gate.
func TestNoArgsFieldDescription(t *testing.T) {
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}

	var hits []string
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
		for _, hit := range argsFieldDescriptionLines(string(raw)) {
			hits = append(hits, fmt.Sprintf("%s:%d", p, hit))
		}
	}
	if len(hits) > 0 {
		t.Errorf("%d args-field @description annotation(s); the parser discards them (#2615) -- delete the annotation(s), per-field docs arrive with /// doc comments (#2601):\n  %s",
			len(hits), strings.Join(hits, "\n  "))
	}
}

var argsOpenRE = regexp.MustCompile(`(^|[^A-Za-z0-9_])args[ \t]*\{`)

// argsFieldDescriptionLines returns the 1-indexed lines carrying a
// @description annotation inside an args{} block. Line comments and
// string contents are blanked before brace tracking so embedded braces
// never skew the depth; /* */ spans are blanked newline-preserving so
// reported line numbers stay stable.
func argsFieldDescriptionLines(src string) []int {
	src = blankBlockComments(src)
	var out []int
	depth := 0
	inArgs := false
	argsDepth := 0
	for i, line := range strings.Split(src, "\n") {
		code := stripStringsForScan(line)
		if inArgs && depth < argsDepth {
			inArgs = false
		}
		if !inArgs && argsOpenRE.MatchString(code) {
			inArgs = true
			argsDepth = depth + 1
			depth += strings.Count(code, "{") - strings.Count(code, "}")
			continue
		}
		if inArgs && strings.Contains(code, "@description") {
			out = append(out, i+1)
		}
		depth += strings.Count(code, "{") - strings.Count(code, "}")
	}
	return out
}
