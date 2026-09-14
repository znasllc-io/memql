package dslconformance

// corpus_test.go -- the tree the expression-reading gates run over.
//
// The tree is edition 2026 (epic memql#5363): every filter, spec and trait
// body, @filter and in-process expression is written in the v1 forms. A gate
// that reads expression TEXT and does not understand a spelling does not fail
// when it meets one: it stops recognising what it looks for, finds nothing,
// and reports a clean corpus. So each such gate carries a reachable-positive
// floor -- a count of what it did recognise -- and a gate that goes blind
// reads as a failure rather than a pass.

import (
	"io"
	"sort"
	"sync"
	"testing"

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

// onTree runs fn over the embedded tree.
func onTree(t *testing.T, fn func(t *testing.T, c corpus)) {
	t.Helper()
	fn(t, embeddedCorpus(t))
}
