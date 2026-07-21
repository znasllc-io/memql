package dsl

import (
	"io"
	"strings"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/dslfs"
)

// TestNoRedundantNamespace keeps directory-restating @namespace out of the
// shipped tree (#2614): absent @namespace derives the containing domain
// directory at load, so the directory-equal form is dead weight. The gate
// RUNS the exact codemod (parser.RewriteRedundantNamespace) so gate and
// codemod cannot drift; colon-scoped sub-namespaces and pinned divergences
// (namespace.pin) are untouched by construction. Fix a failure with:
//
//	go run ./cmd/memqlmigrate --rewrite=namespace-default -w dsl/<domain>
func TestNoRedundantNamespace(t *testing.T) {
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
		domain := p
		if i := strings.IndexByte(domain, '/'); i > 0 {
			domain = domain[:i]
		}
		rewritten, rerr := langparser.RewriteRedundantNamespace(domain, raw)
		if rerr != nil {
			t.Fatalf("rewrite %s: %v", p, rerr)
		}
		if string(rewritten) != string(raw) {
			stale = append(stale, p)
		}
	}
	if len(stale) > 0 {
		t.Errorf("%d file(s) carry @namespace restating the domain directory; run `go run ./cmd/memqlmigrate --rewrite=namespace-default -w` on: %v", len(stale), stale)
	}
}
