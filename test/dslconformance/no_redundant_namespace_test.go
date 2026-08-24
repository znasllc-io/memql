package dslconformance

import (
	"github.com/znasllc-io/memql/dsl"
	"io"
	"testing"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/dslfs"
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
	tree := dsl.Tree()
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
		// The derived namespace is the WHOLE directory path, not its first
		// segment (memql#3898): beta/sub/concepts.memql derives
		// v1:beta/sub:widget, so `@namespace("beta")` on a file one level
		// down is load-bearing rather than redundant -- it is the only
		// thing (with the directory's namespace.pin) holding the concept
		// in the parent namespace. Comparing against the first segment
		// called that annotation dead weight and told the author to
		// delete it, which would have silently re-keyed every concept id
		// in the file.
		//
		// dsl/shopify/generated is the first nested concept directory in
		// the tree (memql#4389); before it, every concept file sat at a
		// domain root and the two readings agreed, which is why the gate
		// could be wrong this long without anything noticing.
		rewritten, rerr := langparser.RewriteRedundantNamespace(dslfs.NamespaceFromFilePath(p), raw)
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
