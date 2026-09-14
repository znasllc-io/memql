package main

// expressions.go -- the `expressions` tree rewrite (epic dsl-v1-expressions,
// task memql#5368): every .memql file of a tree moved onto the edition-2026
// expression forms. The per-file engine, and the list of what it changes, is
// langparser.RewriteExpressions.
//
// It is a TREE rewrite for one reason: a filter clause names predicates by
// bare name, the engine's predicate registry is flat, and so whether
// `requiresOwner` becomes `requiresOwner(row)` or `requiresOwner(actor)` is
// decided by a spec declared in some other file, over a shape declared in yet
// another. The whole tree is read for its predicates first; only then is any
// file rewritten.

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"sort"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// rewriteExpressions collects every spec and trait declared in the tree, then
// rewrites each .memql file against that set, returning only the files that
// change. A refusal anywhere refuses the run, and every refused clause in
// every file is named, so one run shows the whole list rather than the first.
func rewriteExpressions(_ string, files map[string][]byte) (map[string][]byte, error) {
	memql := map[string][]byte{}
	for p, b := range files {
		if path.Ext(p) == ".memql" {
			memql[p] = b
		}
	}
	preds, err := langparser.CollectPredicates(memql)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(memql))
	for p := range memql {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	out := map[string][]byte{}
	var errs []error
	for _, p := range paths {
		next, err := langparser.RewriteExpressions(memql[p], preds)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
			continue
		}
		if !bytes.Equal(next, memql[p]) {
			out[p] = next
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return out, nil
}
