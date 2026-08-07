package dslconformance

import (
	"fmt"
	"github.com/znasllc-io/memql/dsl"
	"io"
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/dslfs"
)

// TestNoBareMirrorRuns keeps mutation write blocks collapsed into the
// accept/stamp form where the collapse is provably safe (#2616): the
// gate runs the EXACT codemod (parser.RewriteAcceptStamp) and fails on
// any file the rewrite would change -- gate and codemod cannot drift,
// and every eligibility rule (two-mirror minimum, declared-arg check,
// comment/nested-object skips, engine-emitter equivalence proof) is
// shared by construction. Fix a failure with:
//
//	go run ./cmd/memqlmigrate --rewrite=accept-stamp -w dsl/
//
// _reference/ is structurally outside this gate (underscore walker
// skip + embed omission), matching the sibling gates.
func TestNoBareMirrorRuns(t *testing.T) {
	tree := dsl.Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}

	var stale []string
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
		rewritten, rerr := languageParser.RewriteAcceptStamp(raw)
		if rerr != nil {
			t.Fatalf("RewriteAcceptStamp %s: %v", p, rerr)
		}
		if string(rewritten) != string(raw) {
			stale = append(stale, fmt.Sprintf("%s (%d byte delta)", p, len(raw)-len(rewritten)))
		}
	}
	if len(stale) > 0 {
		t.Errorf("%d file(s) carry arg-mirror runs collapsible into accept{}/stamp{} (#2616) -- run `go run ./cmd/memqlmigrate --rewrite=accept-stamp -w dsl/`:\n  %s",
			len(stale), strings.Join(stale, "\n  "))
	}
}
