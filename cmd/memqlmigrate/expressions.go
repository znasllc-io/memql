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
//
// The tree is read OVER the engine's core tree -- the dsl/ this binary embeds
// -- because that is the tree it loads over: a bundle's spec over the core
// @actor shape actorEnvelope is an actor predicate though the bundle declares
// no such shape, and its filter naming the core trait isActiveRecord
// resolves. The tree's own declarations win wherever both declare a name, so
// run over dsl/ itself the core changes no answer.

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"sort"
	"sync"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// corePredicates is the embedded core tree's predicate declarations, scanned
// once.
var corePredicates = sync.OnceValues(func() ([]langparser.PredicateDeclarations, error) {
	return langparser.ScanPredicateTree(memqldsl.Tree())
})

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
	paths := make([]string, 0, len(memql))
	for p := range memql {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	base, err := corePredicates()
	if err != nil {
		return nil, fmt.Errorf("read the core tree's predicates: %w", err)
	}
	local := make([]langparser.PredicateDeclarations, 0, len(paths))
	for _, p := range paths {
		local = append(local, langparser.ScanPredicateDeclarations(p, memql[p]))
	}
	preds, err := langparser.ResolvePredicatesOver(base, local)
	if err != nil {
		return nil, err
	}

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
