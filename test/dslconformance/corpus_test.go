package dslconformance

// corpus_test.go -- the two corpora the expression-reading gates run over
// until edition 2026 is the only one (epic memql#5363, task memql#5368).
//
// The codemod (memqlmigrate --rewrite=expressions) moves every filter, spec
// and trait body, @filter and in-process expression onto the v1 forms in one
// run. A gate that reads expression TEXT and does not understand the new
// spelling does not fail when that run lands: it stops recognising what it
// looks for, finds nothing, and reports a clean corpus. So each such gate runs
// here over BOTH the embedded tree and that tree as the codemod would leave
// it, migrated in memory by the codemod's own engine -- with the same
// assertions and the same reachable-positive floors on each, so a gate that
// goes blind on either reads as a failure rather than a pass.
//
// The migrated corpus is never written anywhere. It is what `memqlmigrate
// --rewrite=expressions` would produce from the embedded tree right now, which
// is exactly the tree these gates will face the day the codemod runs.

import (
	"io"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/language/dslclause"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/dslgate"
	"github.com/znasllc-io/memql/core/dslfs"
	"github.com/znasllc-io/memql/dsl"
)

// corpus is a set of .memql sources keyed by tree-relative path.
type corpus struct {
	name  string
	paths []string // sorted
	files map[string]string
}

// sources returns the corpus as the contract gates take it.
func (c corpus) sources() []dslgate.SourceFile {
	out := make([]dslgate.SourceFile, 0, len(c.paths))
	for _, p := range c.paths {
		out = append(out, dslgate.SourceFile{Path: p, Content: c.files[p]})
	}
	return out
}

func newCorpus(name string, files map[string]string) corpus {
	c := corpus{name: name, files: files}
	for p := range files {
		c.paths = append(c.paths, p)
	}
	sort.Strings(c.paths)
	return c
}

var (
	embeddedOnce sync.Once
	embedded     corpus
	embeddedErr  error

	migratedOnce sync.Once
	migrated     corpus
	migratedErr  error
)

// embeddedCorpus is every .memql file dsl.Tree() walks.
func embeddedCorpus(t *testing.T) corpus {
	t.Helper()
	embeddedOnce.Do(func() {
		tree := dsl.Tree()
		paths, err := dslfs.WalkMemqlFiles(tree)
		if err != nil {
			embeddedErr = err
			return
		}
		files := make(map[string]string, len(paths))
		for _, p := range paths {
			f, err := tree.Open(p)
			if err != nil {
				embeddedErr = err
				return
			}
			raw, err := io.ReadAll(f)
			f.Close()
			if err != nil {
				embeddedErr = err
				return
			}
			files[p] = string(raw)
		}
		embedded = newCorpus("embedded", files)
	})
	if embeddedErr != nil {
		t.Fatalf("reading the embedded tree: %v", embeddedErr)
	}
	return embedded
}

// migratedCorpus is the embedded corpus as the edition-2026 codemod leaves it,
// computed in memory with the codemod's own engine: every spec and trait in
// the corpus is collected first (whether `isX` becomes `isX(row)` or
// `isX(actor)` is decided by a declaration in another file), then each file is
// rewritten against that set.
func migratedCorpus(t *testing.T) corpus {
	t.Helper()
	src := embeddedCorpus(t)
	migratedOnce.Do(func() {
		in := make(map[string][]byte, len(src.files))
		for p, s := range src.files {
			in[p] = []byte(s)
		}
		preds, err := langparser.CollectPredicates(in)
		if err != nil {
			migratedErr = err
			return
		}
		files := make(map[string]string, len(in))
		for p, b := range in {
			next, err := langparser.RewriteExpressions(b, preds)
			if err != nil {
				migratedErr = err
				return
			}
			files[p] = string(next)
		}
		migrated = newCorpus("migrated", files)
	})
	if migratedErr != nil {
		t.Fatalf("migrating the embedded tree with the edition-2026 codemod: %v", migratedErr)
	}
	return migrated
}

// countLambdaFilters counts the filter clauses in src that open an
// edition-2026 lambda.
func countLambdaFilters(src string) int {
	n := 0
	for _, line := range strings.Split(blankComments(src), "\n") {
		trim := strings.TrimSpace(line)
		if dslclause.StartsWith(trim, "filter") && dslclause.OpensLambda(strings.TrimPrefix(trim, "filter")) {
			n++
		}
	}
	return n
}

// bothCorpora runs fn over the embedded tree and over its migrated form, as
// two subtests. Every floor fn asserts is asserted on each.
func bothCorpora(t *testing.T, fn func(t *testing.T, c corpus)) {
	t.Helper()
	t.Run("embedded", func(t *testing.T) { fn(t, embeddedCorpus(t)) })
	t.Run("migrated", func(t *testing.T) { fn(t, migratedCorpus(t)) })
}

// TestMigratedCorpusIsEditionTwentySix is the floor under every gate that runs
// over the migrated corpus: the codemod must actually have moved it. A
// migration that silently changed nothing would run every gate twice over the
// legacy tree and call the second run edition-2026 coverage.
func TestMigratedCorpusIsEditionTwentySix(t *testing.T) {
	src, mig := embeddedCorpus(t), migratedCorpus(t)
	if len(src.paths) != len(mig.paths) {
		t.Fatalf("the migrated corpus has %d files, the embedded tree %d", len(mig.paths), len(src.paths))
	}
	changed, lambdaFilters := 0, 0
	for _, p := range mig.paths {
		if mig.files[p] != src.files[p] {
			changed++
		}
		lambdaFilters += countLambdaFilters(mig.files[p])
	}
	// Measured when this landed: 126 of 308 files change and 522 filters open
	// a lambda. The floors sit under both so an ordinary corpus edit does not
	// trip them; a codemod that stopped rewriting would.
	if changed < 100 {
		t.Errorf("the codemod changed %d files; a migration that moves nothing makes every migrated-corpus gate a second legacy run", changed)
	}
	if lambdaFilters < 400 {
		t.Errorf("the migrated corpus has %d lambda filters; the codemod has stopped producing edition-2026 filters", lambdaFilters)
	}
	t.Logf("%d of %d files migrated; %d filters open a lambda", changed, len(mig.paths), lambdaFilters)
}
