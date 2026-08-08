package dslconformance

import (
	"fmt"
	"github.com/znasllc-io/memql/dsl"
	"io"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql/sense"
	"github.com/znasllc-io/memql/core/dslfs"
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
	tree := dsl.Tree()
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

// argsFieldDescriptionLines returns the 1-indexed lines carrying a
// @description annotation inside an args{} block. It delegates to the
// EXACT scan the editor runs (sense.DiscardedArgsDescriptions), so the
// CI gate and the sense Hint cannot drift -- parity by construction
// (#2658 review round: the gate's own line-based scanner had both a
// false negative on the `args {` opener line and a false positive on a
// one-line `} shape {` transition).
func argsFieldDescriptionLines(src string) []int {
	var out []int
	for _, d := range sense.DiscardedArgsDescriptions(src) {
		out = append(out, d.Range.Start.Line)
	}
	return out
}
