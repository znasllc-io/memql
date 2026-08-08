package dslconformance

import (
	"fmt"
	"github.com/znasllc-io/memql/dsl"
	"io"
	"path"
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/dslfs"
)

// TestNoSameDomainUse keeps the tree free of use imports of a file's
// OWN domain (#2617): same-domain constructs are ambient (the loader
// seeds the namespace hint from the containing directory), so the
// import is pure ceremony -- and the longest-line ceremony in the
// corpus at that. The gate runs the EXACT codemod
// (parser.RewriteSameDomainUse with the directory-derived domain), so
// gate and rewrite cannot drift. Fix a failure with:
//
//	go run ./cmd/memqlmigrate --rewrite=same-domain-use -w dsl/
//
// _reference/ is structurally outside this gate (underscore walker
// skip + embed omission), matching the sibling gates.
func TestNoSameDomainUse(t *testing.T) {
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
		domain := path.Base(path.Dir(p))
		rewritten, rerr := languageParser.RewriteSameDomainUse(domain, raw)
		if rerr != nil {
			t.Fatalf("RewriteSameDomainUse %s: %v", p, rerr)
		}
		if string(rewritten) != string(raw) {
			stale = append(stale, p)
		}
	}
	if len(stale) > 0 {
		t.Errorf("%d file(s) import their own domain (#2617: same-domain constructs are ambient) -- run `go run ./cmd/memqlmigrate --rewrite=same-domain-use -w dsl/`:\n  %s",
			len(stale), strings.Join(stale, "\n  "))
	}
	_ = fmt.Sprintf
}
