package main

// rewrites.go -- the one registry every rewrite is reached through (epic
// memql#5356, task memql#5358; D4 and D6 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
//
// A rewrite is registered under the EDITION it moves a tree onto and the EPIC
// that shipped it. `--edition` picks the edition (it defaults to the one this
// engine writes, parser.Edition) and `--rewrite` names rewrites registered for
// it; a name or an edition the registry does not hold is refused naming what
// it does hold, because the author's next move is to pick one of those.
//
// EVERY LATER EPIC REGISTERS ITS REWRITE HERE, as one entry in `registry`.
// That is the migration channel the language promises: when an epic narrows the
// language, the rewrite that carries a tree across the narrowing ships in the
// same PR and is reachable by the same two flags.
//
// Three kinds, exactly one per entry:
//
//   - plain rewrites one .memql file from its content alone;
//   - path rewrites one .memql file and needs its path (to derive its domain);
//   - tree rewrites a whole tree at once, for a rewrite that cannot decide one
//     file from that file: it sees every file under the root (keyed by
//     slash-separated path relative to it) and returns the files to write,
//     which may include files that did not exist.

import (
	"fmt"
	"sort"
	"strings"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/dslfs"
)

type rewrite struct {
	// name is what --rewrite=<name> selects. Unique within an edition.
	name string
	// edition is the edition this rewrite moves a tree onto.
	edition string
	// epic is the epic (or, for a rewrite older than the epic labels, the
	// issue) that registered it.
	epic string
	// doc is one line for the usage listing.
	doc string

	plain func(src []byte) ([]byte, error)
	path  func(path string, src []byte) ([]byte, error)
	tree  func(root string, files map[string][]byte) (map[string][]byte, error)
}

// registry is every rewrite this tool has, oldest first.
var registry = []rewrite{
	{name: "result-navigation", edition: "2026", epic: "pre-split",
		doc:   ".empty / .first / .last / .count / .nodes on step results -> the method forms .Empty() / .First() / .Last() / .Len() / .Nodes()",
		plain: rewriteResultNavigation},
	{name: "slice-syntax", edition: "2026", epic: "pre-split",
		doc:   "array(T) -> []T",
		plain: rewriteSliceSyntax},
	{name: "accept-stamp", edition: "2026", epic: "memql#2616",
		doc:   "collapse arg-mirror runs in mutation write blocks into accept / stamp, where provably equivalent",
		plain: langparser.RewriteAcceptStamp},
	{name: "same-domain-use", edition: "2026", epic: "memql#2617",
		doc:  "delete use imports of the file's own domain",
		path: rewriteSameDomainUse},
	{name: "namespace-default", edition: "2026", epic: "memql#2614",
		doc:  "delete @namespace annotations that restate the file's domain directory",
		path: rewriteNamespaceDefault},
	{name: "required-sigil", edition: "2026", epic: "memql#2618",
		doc:   "@required on an args field -> the ! type sigil",
		plain: langparser.RewriteRequiredSigil},
	{name: "enum-type", edition: "2026", epic: "memql#2618",
		doc:   "string @enum(...) on an args field -> the enum(...) type",
		plain: langparser.RewriteEnumTypeArgs},
	{name: "cache-positional", edition: "2026", epic: "memql#2618",
		doc:   "@cache(ttl=\"N\") -> @cache(N)",
		plain: langparser.RewriteCachePositional},
	{name: "terse-automation", edition: "2026", epic: "memql#2619",
		doc:   "a longhand single-step automation -> the terse header form",
		plain: langparser.RewriteLonghandSingleStepAutomation},
	{name: "actor-binding", edition: "2026", epic: "memql#2621",
		doc:   "declare @actor on a construct whose body reads actor.*",
		plain: langparser.RewriteActorBinding},
	{name: "doc-comment-descriptions", edition: "2026", epic: "memql#2635",
		doc:   "@description(\"...\") on a declaration -> a /// doc comment above it",
		plain: langparser.RewriteDocCommentDescriptions},
	{name: "null-coalesce", edition: "2026", epic: "memql#2766",
		doc:   "coalesce(a, b, c) -> a ?? b ?? c, where the precedence cannot re-associate it",
		plain: langparser.RewriteNullCoalesce},
	{name: "row-authz", edition: "2026", epic: "memql#2920",
		doc:  "seed @rowAuthz(...) on each concept from how its queries filter",
		path: rewriteRowAuthz},
	{name: "args-description", edition: "2026", epic: "memql#3336",
		doc:   "strip @description(\"...\") from args fields, which the parser refuses",
		plain: rewriteArgsDescription},
	{name: "language-line", edition: "2026", epic: "dsl-v1-foundations",
		doc:  "declare memql = \"" + langparser.LanguageVersion + "\" and edition = \"2026\" in every domain that has no " + dslfs.ManifestFile,
		tree: rewriteLanguageLine("2026")},
	{name: "expressions", edition: "2026", epic: "dsl-v1-expressions",
		doc:  "filters, spec and trait bodies and @filter -> lambdas (filter row => ...); cond / concat / exists / null -> ? : / + / != nil / nil; $args.x in query tool handlers -> args.x",
		tree: rewriteExpressions},
}

// rewritesFor returns the rewrites registered for an edition, sorted by name.
func rewritesFor(edition string) []rewrite {
	var out []rewrite
	for _, r := range registry {
		if r.edition == edition {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// resolveEdition refuses an edition the engine does not read.
func resolveEdition(edition string) error {
	for _, e := range langparser.Editions() {
		if e == edition {
			return nil
		}
	}
	return fmt.Errorf("memqlmigrate: edition %q is not one this engine reads (it reads: %s)",
		edition, strings.Join(langparser.Editions(), ", "))
}

// resolveRewrites maps names to registered rewrites for one edition, in the
// order given. The first name the edition does not hold is refused, naming
// every rewrite it does.
func resolveRewrites(edition string, names []string) ([]rewrite, error) {
	byName := map[string]rewrite{}
	for _, r := range rewritesFor(edition) {
		byName[r.name] = r
	}
	out := make([]rewrite, 0, len(names))
	for _, n := range names {
		r, ok := byName[n]
		if !ok {
			return nil, fmt.Errorf("memqlmigrate: no rewrite %q is registered for edition %s; registered for %s: %s",
				n, edition, edition, strings.Join(rewriteNames(edition), ", "))
		}
		out = append(out, r)
	}
	return out, nil
}

func rewriteNames(edition string) []string {
	rs := rewritesFor(edition)
	names := make([]string, len(rs))
	for i, r := range rs {
		names[i] = r.name
	}
	return names
}

// rewriteLanguageLine returns the tree rewrite that declares the engine's
// language line in every domain of a tree that has none (epic
// dsl-v1-foundations, memql#5357).
//
// A domain is exactly what the loader asks a line of: the first path segment
// of a .memql file some loader reads, however deep beneath it the file sits
// (parser.LanguageLineDomainOf, the rule the resolver itself keys on), so a
// domain holding only a sub-namespace (beta/sub/concepts.memql) is one, and
// `_`/`.` segments are skipped as the walkers and mounts skip them. When the
// root itself directly holds .memql files it is one domain (the caller passed
// dsl/<domain> rather than dsl/). A domain that already has a memql.toml is
// left alone whatever it declares, because moving a declared line is a
// decision about the tree, not a migration of it.
func rewriteLanguageLine(edition string) func(root string, files map[string][]byte) (map[string][]byte, error) {
	return func(root string, files map[string][]byte) (map[string][]byte, error) {
		domains := map[string]bool{} // domain dir ("" = the root) -> needs a line
		rootIsDomain := false
		for p := range files {
			if !strings.Contains(p, "/") && strings.HasSuffix(p, ".memql") && !strings.HasPrefix(p, "_") {
				rootIsDomain = true
			}
			if d := langparser.LanguageLineDomainOf(p); d != "" {
				domains[d] = true
			}
		}
		// A root that is itself a domain makes everything beneath it part of
		// that one domain, not domains of their own.
		if rootIsDomain {
			domains = map[string]bool{"": true}
		}
		line := dslfs.Manifest{Language: langparser.LanguageVersion, Edition: edition}
		out := map[string][]byte{}
		for dir := range domains {
			target := dslfs.ManifestFile
			if dir != "" {
				target = dir + "/" + dslfs.ManifestFile
			}
			if _, declared := files[target]; declared {
				continue
			}
			out[target] = []byte("# The language this directory's .memql files are written in.\n" +
				"# See docs/public/language/memql.md, \"The language line\".\n" + line.Render())
		}
		return out, nil
	}
}
