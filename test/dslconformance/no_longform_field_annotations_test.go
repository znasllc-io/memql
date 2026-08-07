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

// The #2618 long-form gates: each runs its EXACT codemod and fails on
// any file the rewrite would change -- gate and codemod cannot drift.
// Fix a failure with the named memqlmigrate mode. _reference/ is
// structurally outside these gates (underscore walker skip), matching
// the sibling gates.

func runRewriteGate(t *testing.T, label, mode string, fn func([]byte) ([]byte, error)) {
	t.Helper()
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
		rewritten, rerr := fn(raw)
		if rerr != nil {
			t.Fatalf("%s %s: %v", label, p, rerr)
		}
		if string(rewritten) != string(raw) {
			stale = append(stale, p)
		}
	}
	if len(stale) > 0 {
		t.Errorf("%d file(s) carry the %s long form (#2618) -- run `go run ./cmd/memqlmigrate --rewrite=%s -w dsl/`:\n  %s",
			len(stale), label, mode, strings.Join(stale, "\n  "))
	}
	_ = fmt.Sprintf
}

func TestNoRequiredAnnotation(t *testing.T) {
	runRewriteGate(t, "@required", "required-sigil", languageParser.RewriteRequiredSigil)
}

func TestNoStringEnumAnnotation(t *testing.T) {
	runRewriteGate(t, "string @enum", "enum-type", languageParser.RewriteEnumTypeArgs)
}

func TestNoKeywordCacheTTL(t *testing.T) {
	runRewriteGate(t, "@cache(ttl=...)", "cache-positional", languageParser.RewriteCachePositional)
}
