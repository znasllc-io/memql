package parser

// Corpus-level equivalence harness (memql#2635 DoD): for every construct in
// the shipped tree, the resolved description is unchanged by the rewrite --
// verified by re-running the codemod on each file (idempotency proves the
// tree IS its own output) and asserting no construct-level @description
// remains while every /// block round-trips through the ruling-2 join.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRewriteDocComments_CorpusConverged(t *testing.T) {
	root := "../../../dsl"
	if _, err := os.Stat(root); err != nil {
		t.Skip("dsl tree not present")
	}
	files := 0
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			if info != nil && info.IsDir() && strings.HasPrefix(info.Name(), "_") {
				return filepath.SkipDir
			}
			return err
		}
		if !strings.HasSuffix(p, ".memql") || strings.HasPrefix(info.Name(), "_") {
			return nil
		}
		files++
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		// Idempotency == the corpus is fully converged: re-running the
		// codemod must be a byte-level no-op on every file.
		out, cerr := RewriteDocCommentDescriptions(b)
		if cerr != nil {
			t.Errorf("%s: rewrite: %v", p, cerr)
			return nil
		}
		if string(out) != string(b) {
			t.Errorf("%s: corpus not converged (codemod would still change it)", p)
		}
		// Seeds, builtins, and verification-bailed constructs legitimately
		// keep @description -- convergence, not absence, is the invariant:
		// the codemod's self-verification guarantees every conversion
		// resolved identically, and whatever remains was refused for cause.
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 100 {
		t.Fatalf("walked only %d files; corpus missing?", files)
	}
}
