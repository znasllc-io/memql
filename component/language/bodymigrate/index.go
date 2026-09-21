package bodymigrate

// index.go -- which construct a bare call names (epic memql#5370, task
// memql#5373).
//
// Edition 2026 writes every call with its kind (D13): `mutation
// expireAccessRequest(...)`, never `expireAccessRequest { ... }`. The retired
// forms left the kind off wherever the runtime could resolve the bare name, so
// the rewrite has to decide it -- and it decides the way the runtime did, by
// looking the name up among the declarations. The index must cover the tree
// being rewritten AND the engine's embedded tree, because a bundle calls core
// mutations and builtins it does not declare: IndexFiles indexes the files it
// is given, and the caller adds the embedded tree (memqlmigrate does).
//
// A name declared as two kinds is refused rather than guessed: the runtime's
// flat registry answered by load order, and a rewrite that picked one would be
// choosing a meaning nobody wrote.

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

// Index maps a construct name to the kinds it is declared as.
type Index struct {
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

// NewIndex returns an empty index.
func NewIndex() *Index { return &Index{kinds: map[string]map[string]bool{}} }

// AddSource records every declaration in one .memql source.
func (ix *Index) AddSource(src string) {
	view := codeView(src)
	for _, re := range []*regexp.Regexp{declTwoIdent, declOneIdent} {
		for _, m := range re.FindAllStringSubmatch(view, -1) {
			ix.add(m[2], declKindOf[m[1]])
		}
	}
}

func (ix *Index) add(name, kind string) {
	if ix.kinds[name] == nil {
		ix.kinds[name] = map[string]bool{}
	}
	ix.kinds[name][kind] = true
}

// IndexFiles indexes the .memql files among files.
func IndexFiles(files map[string][]byte) *Index {
	ix := NewIndex()
	for p, b := range files {
		if path.Ext(p) == ".memql" {
			ix.AddSource(string(b))
		}
	}
	return ix
}

// Clone is a copy of the index that sources can be added to without
// changing it.
func (ix *Index) Clone() *Index {
	out := NewIndex()
	for name, kinds := range ix.kinds {
		for k := range kinds {
			out.add(name, k)
		}
	}
	return out
}

// bareCallKinds are the kinds a BARE call could reach: the runtime resolved a
// bare name through the function registry, which holds queries, mutations,
// logic and builtins. An automation or an action was only ever reached through
// its own kind word, so a bare name that is also an automation (a @template
// automation wrapping the builtin of the same name) still means the builtin.
var bareCallKinds = map[string]bool{"query": true, "mutation": true, "logic": true, "builtin": true}

// kindOf returns the one call kind a bare name resolved to.
func (ix *Index) kindOf(name string) (string, error) {
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
func (ix *Index) has(name, kind string) bool { return ix.kinds[name][kind] }

// exprFunctions are the names a bare call means as an expression, not a
// construct: the expression catalog, retired spellings included, since the
// rewrite may run before or after epic 2's.
//
// `ai` IS NOT AMONG THEM, and its absence is the migration half of restoring
// the call. `ai` is a BUILTIN now (dsl/agents/builtins.memql), so a bare
// `ai(...)` in a retired body is a construct call that has lost its kind word,
// exactly like a bare `createFolder(...)`: kindOf resolves it against the
// index -- which covers the engine's embedded tree -- and the rewrite writes
// the kind back, or refuses naming what it cannot carry. Measured on the
// legacy spelling `ai("docSummary", { ... })`:
//
//	listed here    `ai("docSummary", { ... })`   emitted back unchanged --
//	               the one spelling a body refuses (`body_call_unknown`)
//	not listed     refused, "builtin ai: the positional argument
//	               \"docSummary\" has no name" -- the builtin takes its
//	               arguments by name, and the author is told so
//
// The second is the honest answer: the legacy call was positional and the
// builtin is not, so there is a real edit to make and the rewrite says what it
// is instead of producing a file that loads nowhere.
var exprFunctions = map[string]bool{
	"concat": true, "coalesce": true, "cond": true, "first": true, "last": true, "lower": true,
	"upper": true, "trim": true, "hash": true, "shortId": true, "canonicalId": true, "toString": true,
	"addDuration": true, "daysBetween": true, "contains": true, "error": true, "var": true,
	"exists": true, "len": true, "count": true, "timestamp": true, "includes": true, "field": true,
	"node": true, "children": true, "parent": true, "similar": true, "embed": true,
	"systemVar": true, "secret": true, "systemSecret": true, "case": true, "default": true,
	"append": true,
}
