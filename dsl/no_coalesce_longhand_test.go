package dsl

import (
	"io"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/dslfs"
)

// TestNoCoalesceLonghand keeps the longhand `coalesce(a, b)` call out of
// the shipped tree wherever the `??` shorthand #2611 resurrected can
// express it (#2766). The DSL optimizes for minimum tokens -- it is
// authored and read by LLMs -- so the call spelling is context-window
// waste once the operator exists.
//
// The gate RUNS the exact codemod (parser.RewriteNullCoalesce) rather
// than re-implementing a scan, so gate and codemod cannot drift: every
// position the codemod deliberately skips is, by construction, a
// position this gate does not flag. Those skips are the unsafe
// precedence adjacencies and the value slots that still reject the `??`
// token (memql#2772) -- see the codemod's doc comment. Fix a failure
// with:
//
//	go run ./cmd/memqlmigrate --rewrite=null-coalesce -w dsl
func TestNoCoalesceLonghand(t *testing.T) {
	tree := Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	var stale []string
	for _, p := range paths {
		f, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(f)
		f.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		rewritten, rerr := langparser.RewriteNullCoalesce(raw)
		if rerr != nil {
			t.Fatalf("rewrite %s: %v", p, rerr)
		}
		if string(rewritten) != string(raw) {
			stale = append(stale, p)
		}
	}
	if len(stale) > 0 {
		t.Errorf("%d file(s) spell a foldable coalesce() the long way; run `go run ./cmd/memqlmigrate --rewrite=null-coalesce -w dsl` on: %v", len(stale), stale)
	}
}
