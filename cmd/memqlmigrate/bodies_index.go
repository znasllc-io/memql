package main

// bodies_index.go -- which construct a bare call names (epic memql#5370, task
// memql#5373).
//
// Edition 2026 writes every call with its kind (D13): `mutation
// expireAccessRequest(...)`, never `expireAccessRequest { ... }`. The retired
// forms left the kind off wherever the runtime could resolve the bare name, so
// the rewrite has to decide it -- and it decides the way the runtime did, by
// looking the name up among the declarations. The index covers the tree being
// rewritten AND the engine's embedded tree, because a bundle calls core
// mutations and builtins it does not declare.
//
// A name declared as two kinds is refused rather than guessed: the runtime's
// flat registry answered by load order, and a rewrite that picked one would be
// choosing a meaning nobody wrote.

import (
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"

	memqldsl "github.com/znasllc-io/memql/dsl"
)

// declIndex maps a construct name to the kinds it is declared as.
type declIndex struct {
	kinds map[string]map[string]bool
}

var (
	declTwoIdent = regexp.MustCompile(`(?m)^(query|mutate|mutation)[ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)
	declOneIdent = regexp.MustCompile(`(?m)^(logic|automation|builtin|action|capability)[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*[{@]`)
)

// declKindOf maps a declaration keyword to the call kind that invokes it.
var declKindOf = map[string]string{
	"query": "query", "mutate": "mutation", "mutation": "mutation", "logic": "logic",
	"automation": "automation", "builtin": "builtin", "action": "action", "capability": "capability",
}

// newDeclIndex returns an empty index.
func newDeclIndex() *declIndex { return &declIndex{kinds: map[string]map[string]bool{}} }

// addSource records every declaration in one .memql source.
func (ix *declIndex) addSource(src string) {
	view := codeView(src)
	for _, re := range []*regexp.Regexp{declTwoIdent, declOneIdent} {
		for _, m := range re.FindAllStringSubmatch(view, -1) {
			ix.add(m[2], declKindOf[m[1]])
		}
	}
}

func (ix *declIndex) add(name, kind string) {
	if ix.kinds[name] == nil {
		ix.kinds[name] = map[string]bool{}
	}
	ix.kinds[name][kind] = true
}

// buildDeclIndex indexes files plus the engine's embedded tree.
func buildDeclIndex(files map[string][]byte) (*declIndex, error) {
	ix := newDeclIndex()
	for p, b := range files {
		if path.Ext(p) == ".memql" {
			ix.addSource(string(b))
		}
	}
	err := fs.WalkDir(memqldsl.Tree(), ".", func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() || path.Ext(p) != ".memql" || strings.HasPrefix(path.Base(path.Dir(p)), "_") {
			return nil
		}
		b, rerr := fs.ReadFile(memqldsl.Tree(), p)
		if rerr != nil {
			return rerr
		}
		ix.addSource(string(b))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("index the embedded tree: %w", err)
	}
	return ix, nil
}

// bareCallKinds are the kinds a BARE call could reach: the runtime resolved a
// bare name through the function registry, which holds queries, mutations,
// logic and builtins. An automation or an action was only ever reached through
// its own kind word, so a bare name that is also an automation (a @template
// automation wrapping the builtin of the same name) still means the builtin.
var bareCallKinds = map[string]bool{"query": true, "mutation": true, "logic": true, "builtin": true}

// kindOf returns the one call kind a bare name resolved to.
func (ix *declIndex) kindOf(name string) (string, error) {
	ks := ix.kinds[name]
	var list, other []string
	for k := range ks {
		if bareCallKinds[k] {
			list = append(list, k)
		} else {
			other = append(other, k)
		}
	}
	sort.Strings(list)
	sort.Strings(other)
	switch {
	case len(list) == 1:
		return list[0], nil
	case len(list) > 1:
		return "", fmt.Errorf("%s is declared as %s; write the kind by hand", name, strings.Join(list, " and "))
	case len(other) > 0:
		return "", fmt.Errorf("%s is declared only as %s, which a bare call never reached; write `%s %s(...)` if that is what it meant", name, strings.Join(other, " and "), other[0], name)
	}
	return "", fmt.Errorf("%s names no query, mutation, logic or builtin declared in this tree or the engine's", name)
}

// has reports whether name is declared as kind.
func (ix *declIndex) has(name, kind string) bool { return ix.kinds[name][kind] }

// exprFunctions are the names a bare call means as an expression, not a
// construct: the expression catalog, retired spellings included, since the
// rewrite may run before or after epic 2's.
var exprFunctions = map[string]bool{
	"concat": true, "coalesce": true, "cond": true, "first": true, "last": true, "lower": true,
	"upper": true, "trim": true, "hash": true, "shortId": true, "canonicalId": true, "toString": true,
	"addDuration": true, "daysBetween": true, "contains": true, "error": true, "var": true,
	"exists": true, "len": true, "count": true, "timestamp": true, "includes": true, "field": true,
	"ai": true, "node": true, "children": true, "parent": true, "similar": true, "embed": true,
	"systemVar": true, "secret": true, "systemSecret": true, "case": true, "default": true,
	"append": true,
}
