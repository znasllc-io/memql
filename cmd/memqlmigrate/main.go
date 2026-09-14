// memqlmigrate applies named syntax rewrites to MemQL source files.
//
// Usage:
//
//	memqlmigrate [--edition=<edition>] --rewrite=<name>[,<name>...] [-w] [-check] <path>...
//	memqlmigrate --rewrite=bodies --go-fixtures [-w] [-v] [--exclude=PREFIX,...] <path>...
//	memqlmigrate --grammar-version
//
// Companion to memqlfmt. Where memqlfmt normalises whitespace and
// indentation, memqlmigrate performs one-shot syntactic rewrites that
// carry a tree across a change to the language. One named rewrite per
// change, so the tool runs idempotently per release, matching the shape of
// `go fix` and `cargo fix --edition`.
//
// Every rewrite is registered in rewrites.go under the EDITION it moves a
// tree onto and the EPIC that shipped it (epic memql#5356). --edition picks
// the edition and defaults to the one this engine writes; --rewrite names
// rewrites registered for it. `memqlmigrate --help` lists them. Each
// rewrite's own doc comment says exactly what it changes and what it leaves
// alone.
//
// A TREE rewrite cannot decide a file from that file alone, so it takes a
// directory and refuses a file argument: `language-line` (a domain's
// manifest) and `expressions` (edition 2026's lambda forms, whose predicate
// uses are resolved against every spec and trait in the tree) are the two
// today.
//
// Flags:
//
//	--edition=EDITION          the edition whose rewrites to run (default: the engine's)
//	--rewrite=NAME[,NAME...]   comma-separated rewrites, applied in the order given
//	-w                         rewrite files in place (otherwise print)
//	-check                     exit non-zero and list files that would change
//
// Without -w and without -check, the tool prints the rewritten content of
// each file to stdout; a file a tree rewrite creates is printed under a
// "==> path <==" header. Rewrites apply at the lexical layer; the tool is
// conservative and leaves input unchanged when it cannot safely perform the
// transformation.
//
// --go-fixtures runs the bodies rewrite over the MemQL that Go test files
// embed in string literals instead of over .memql files (gofixtures.go): dry
// unless -w, reporting every literal it refuses; -v lists the literals it
// changes too; --exclude names path prefixes to leave alone.
//
// Exit codes: 0 done, 1 a rewrite failed or -check found files that would
// change, 2 a usage error (an unknown flag, edition or rewrite).
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// rewriteNamespaceDefault strips @namespace annotations that restate the
// file's containing DOMAIN directory (#2614) -- absent @namespace derives
// the domain at load, so the domain-equal form is redundant. The domain is
// the first path segment under the dsl root -- NOT the immediate parent:
// nested files (dsl/agents/tools/x.memql) belong to the top-level domain,
// matching the unified loader's firstPathSegment and the
// no_redundant_namespace gate (review #2614: an immediate-parent derivation
// left nested files gate-flagged but codemod-unfixable, and worse, could
// strip an erroneous nested @namespace the loader would have rejected).
func rewriteNamespaceDefault(path string, src []byte) ([]byte, error) {
	return langparser.RewriteRedundantNamespace(domainForDSLPath(path), src)
}

// domainForDSLPath returns the first path segment after the LAST "dsl"
// element (the domain), falling back to the immediate parent directory for
// paths outside a dsl root (e.g. a product tree mounted elsewhere).
func domainForDSLPath(path string) string {
	segs := strings.Split(filepath.ToSlash(path), "/")
	for i := len(segs) - 2; i >= 0; i-- {
		if segs[i] == "dsl" && i+1 < len(segs)-1+1 {
			if i+1 <= len(segs)-2 {
				return segs[i+1]
			}
		}
	}
	return filepath.Base(filepath.Dir(path))
}

// rewriteSameDomainUse derives the file's domain (its containing
// directory) and delegates to the engine-package rewrite the dsl/
// conformance gate also runs.
func rewriteSameDomainUse(path string, src []byte) ([]byte, error) {
	return langparser.RewriteSameDomainUse(filepath.Base(filepath.Dir(path)), src)
}

// rowAuthzInferenceCache memoises the whole-tree inference per dsl
// root. Unlike every other rewrite here, `row-authz` cannot decide a
// file from the file: a concept's tier is inferred from the QUERIES
// over it, which live in sibling files. The walk therefore happens
// once per root and every concepts.memql reads its slice out.
var rowAuthzInferenceCache = map[string]*rowAuthzInference{}

// rewriteRowAuthz seeds `@rowAuthz(...)` on the concepts declared in
// this file, from the tiers inferred across the whole tree (#2920).
//
// Only `<domain>/concepts.memql` is a target -- concepts are declared
// nowhere else, and running the header scan over queries.memql would
// find nothing while costing a tree walk per file.
func rewriteRowAuthz(path string, src []byte) ([]byte, error) {
	if filepath.Base(path) != "concepts.memql" {
		return src, nil
	}
	domain := filepath.Base(filepath.Dir(path))
	root := filepath.Dir(filepath.Dir(path))

	inference, ok := rowAuthzInferenceCache[root]
	if !ok {
		var err error
		inference, err = inferRowAuthz(root)
		if err != nil {
			return nil, err
		}
		rowAuthzInferenceCache[root] = inference
		// The run states what it inferred AND what it left alone, on
		// stderr so it never lands in a -w diff or a piped file.
		fmt.Fprint(os.Stderr, inference.Report())
	}
	return langparser.RewriteRowAuthz(src, inference.Tiers[domain])
}

type opts struct {
	edition  string
	rewrites []string
	check    bool
	write    bool
	// goFixtures, verbose and excludes are the --go-fixtures mode's.
	goFixtures bool
	verbose    bool
	excludes   []string
}

func main() {
	err := run(os.Args[1:], os.Stdout, os.Stderr)
	switch {
	case err == nil:
	case errors.Is(err, errUsage):
		// The usage problem was already written to stderr where it was found.
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	// --grammar-version prints the engine's grammar epoch + keyword
	// fingerprint (S6, memql#2361) -- the value stamped into authored rows
	// and release lockfiles, and the identity of the migration channel.
	if len(args) == 1 && args[0] == "--grammar-version" {
		fmt.Fprintf(stdout, "%s (fingerprint %s)\n", langparser.GrammarVersion, langparser.GrammarFingerprint())
		return nil
	}

	o, paths, err := parseFlags(args, stderr)
	if err != nil {
		return err
	}
	if err := resolveEdition(o.edition); err != nil {
		fmt.Fprintln(stderr, err)
		return errUsage
	}
	if len(o.rewrites) == 0 {
		fmt.Fprintln(stderr, "memqlmigrate: --rewrite=NAME is required")
		listRewriters(stderr, o.edition)
		return errUsage
	}
	pipeline, err := resolveRewrites(o.edition, o.rewrites)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return errUsage
	}
	if o.goFixtures {
		if len(o.rewrites) != 1 || o.rewrites[0] != "bodies" || o.check {
			fmt.Fprintln(stderr, "memqlmigrate: --go-fixtures runs the bodies rewrite alone: --rewrite=bodies --go-fixtures [-w] [-v]")
			return errUsage
		}
		excludes := o.excludes
		if excludes == nil {
			excludes = goFixtureDefaultExcludes
		}
		if err := runGoFixtures(stdout, paths, goFixtureOptions{write: o.write, verbose: o.verbose, excludes: excludes}); err != nil {
			return fmt.Errorf("memqlmigrate: %w", err)
		}
		return nil
	}

	sets, err := loadWorkingSets(paths, pipeline)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return errUsage
	}

	changed := 0
	for _, ws := range sets {
		if err := applyPipeline(ws, pipeline); err != nil {
			return fmt.Errorf("memqlmigrate: %w", err)
		}
		for _, rel := range ws.outputOrder() {
			full := filepath.Join(ws.root, filepath.FromSlash(rel))
			orig, existed := ws.orig[rel]
			out := ws.files[rel]
			differs := !existed || !bytes.Equal(orig, out)
			switch {
			case o.check:
				if differs {
					fmt.Fprintln(stdout, full)
					changed++
				}
			case o.write:
				if differs {
					if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
						return fmt.Errorf("memqlmigrate: write %s: %w", full, err)
					}
					if err := os.WriteFile(full, out, 0o644); err != nil {
						return fmt.Errorf("memqlmigrate: write %s: %w", full, err)
					}
					changed++
				}
			default:
				if !existed {
					fmt.Fprintf(stdout, "==> %s <==\n", full)
				}
				if _, err := stdout.Write(out); err != nil {
					return fmt.Errorf("memqlmigrate: stdout: %w", err)
				}
			}
		}
	}

	if o.check && changed > 0 {
		return errChanged
	}
	return nil
}

var (
	errUsage   = fmt.Errorf("usage error")
	errChanged = fmt.Errorf("files would change")
)

func parseFlags(args []string, stderr io.Writer) (opts, []string, error) {
	o := opts{edition: langparser.Edition}
	paths := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-w":
			o.write = true
		case a == "-check":
			o.check = true
		case a == "--go-fixtures":
			o.goFixtures = true
		case a == "-v":
			o.verbose = true
		case strings.HasPrefix(a, "--exclude="):
			o.excludes = append(o.excludes, splitCSV(strings.TrimPrefix(a, "--exclude="))...)
		case a == "--rewrite", a == "--edition":
			if i+1 >= len(args) {
				fmt.Fprintf(stderr, "memqlmigrate: %s requires a value\n", a)
				return o, nil, errUsage
			}
			i++
			if a == "--rewrite" {
				o.rewrites = append(o.rewrites, splitCSV(args[i])...)
			} else {
				o.edition = strings.TrimSpace(args[i])
			}
		case strings.HasPrefix(a, "--rewrite="):
			o.rewrites = append(o.rewrites, splitCSV(strings.TrimPrefix(a, "--rewrite="))...)
		case strings.HasPrefix(a, "--edition="):
			o.edition = strings.TrimSpace(strings.TrimPrefix(a, "--edition="))
		case a == "-h", a == "--help":
			printUsage(stderr, o.edition)
			return o, nil, errUsage
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(stderr, "memqlmigrate: unknown flag %q\n", a)
			printUsage(stderr, o.edition)
			return o, nil, errUsage
		default:
			paths = append(paths, a)
		}
	}
	if len(paths) == 0 {
		printUsage(stderr, o.edition)
		return o, nil, errUsage
	}
	return o, paths, nil
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func printUsage(w io.Writer, edition string) {
	fmt.Fprintln(w, "usage: memqlmigrate [--edition=EDITION] --rewrite=NAME[,NAME...] [-w] [-check] <path>...")
	listRewriters(w, edition)
}

// listRewriters is DERIVED from the registry, never hand-listed. The
// hand-written version once silently omitted `null-coalesce` and
// `row-authz` -- a usage message that denies a rewrite exists is worse than
// none, because the docs tell operators to run it.
func listRewriters(w io.Writer, edition string) {
	rs := rewritesFor(edition)
	if len(rs) == 0 {
		fmt.Fprintf(w, "no rewrites are registered for edition %s (this engine reads: %s)\n",
			edition, strings.Join(langparser.Editions(), ", "))
		return
	}
	width := 0
	for _, r := range rs {
		if len(r.name) > width {
			width = len(r.name)
		}
	}
	fmt.Fprintf(w, "rewrites for edition %s:\n", edition)
	for _, r := range rs {
		fmt.Fprintf(w, "  %-*s  %s (%s)\n", width, r.name, r.doc, r.epic)
	}
}

// workingSet is one path argument's files, keyed by slash-separated path
// relative to root.
type workingSet struct {
	root string
	// files is the current content of every file the pipeline can see.
	files map[string][]byte
	// orig is the content as read. A key present in files and absent here is
	// a file a tree rewrite created.
	orig map[string][]byte
	// visit is the .memql files a file-level rewrite runs over, in the order
	// they were found.
	visit []string
}

// outputOrder is every file the run reports: the visited files in the order
// they were found, then any file a tree rewrite created or changed that a
// file-level rewrite did not visit, sorted.
func (ws *workingSet) outputOrder() []string {
	seen := make(map[string]bool, len(ws.visit))
	out := append([]string(nil), ws.visit...)
	for _, v := range ws.visit {
		seen[v] = true
	}
	var extra []string
	for k, v := range ws.files {
		if seen[k] {
			continue
		}
		if o, existed := ws.orig[k]; !existed || !bytes.Equal(o, v) {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	return append(out, extra...)
}

// loadWorkingSets reads each path argument. A directory is a tree: its .memql
// files are visited, and when the pipeline holds a tree rewrite every file
// under it is loaded so the rewrite can see the whole tree. A file argument is
// a one-file set, which a tree rewrite cannot run on.
func loadWorkingSets(paths []string, pipeline []rewrite) ([]*workingSet, error) {
	var treeRewrite string
	for _, r := range pipeline {
		if r.tree != nil {
			treeRewrite = r.name
			break
		}
	}
	sets := make([]*workingSet, 0, len(paths))
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, fmt.Errorf("memqlmigrate: %w", err)
		}
		ws := &workingSet{files: map[string][]byte{}, orig: map[string][]byte{}}
		if !info.IsDir() {
			if treeRewrite != "" {
				return nil, fmt.Errorf("memqlmigrate: rewrite %q works on a whole tree; pass a directory, not the file %s", treeRewrite, p)
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return nil, fmt.Errorf("memqlmigrate: read %s: %w", p, err)
			}
			ws.root = filepath.Dir(p)
			rel := filepath.ToSlash(filepath.Base(p))
			ws.files[rel], ws.orig[rel] = data, data
			ws.visit = []string{rel}
			sets = append(sets, ws)
			continue
		}
		ws.root = p
		err = fs.WalkDir(os.DirFS(p), ".", func(rel string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			if d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			isMemql := path.Ext(rel) == ".memql"
			if !isMemql && treeRewrite == "" {
				return nil
			}
			data, err := fs.ReadFile(os.DirFS(p), rel)
			if err != nil {
				return err
			}
			ws.files[rel], ws.orig[rel] = data, data
			if isMemql {
				ws.visit = append(ws.visit, rel)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("memqlmigrate: %w", err)
		}
		sets = append(sets, ws)
	}
	return sets, nil
}

// applyPipeline runs the rewrites in order over one working set. A file-level
// rewrite runs over every visited .memql file; a tree rewrite sees the whole
// set and its result is merged back, new files included.
func applyPipeline(ws *workingSet, pipeline []rewrite) error {
	for _, r := range pipeline {
		switch {
		case r.plain != nil || r.path != nil:
			for _, rel := range ws.visit {
				full := filepath.Join(ws.root, filepath.FromSlash(rel))
				var next []byte
				var err error
				if r.plain != nil {
					next, err = r.plain(ws.files[rel])
				} else {
					next, err = r.path(full, ws.files[rel])
				}
				if err != nil {
					return fmt.Errorf("rewrite %s: %s: %w", r.name, full, err)
				}
				ws.files[rel] = next
			}
		case r.tree != nil:
			view := make(map[string][]byte, len(ws.files))
			for k, v := range ws.files {
				view[k] = v
			}
			out, err := r.tree(ws.root, view)
			if err != nil {
				return fmt.Errorf("rewrite %s: %s: %w", r.name, ws.root, err)
			}
			for k, v := range out {
				if _, known := ws.files[k]; !known && path.Ext(k) == ".memql" {
					ws.visit = append(ws.visit, k)
				}
				ws.files[k] = v
			}
		}
	}
	return nil
}

// resultNavigationMap is the canonical result-navigation rewrite table.
// The LHS suffix on a dotted identifier is replaced by the method-style
// RHS. `.count` maps to `.Len()` to match Go's slice-length idiom; the
// others preserve their noun/verb shape.
var resultNavigationMap = map[string]string{
	".empty": ".Empty()",
	".first": ".First()",
	".last":  ".Last()",
	".count": ".Len()",
	".nodes": ".Nodes()",
	".ran":   ".Ran()",
}

// resultNavigationSegments is the segment-level rewrite table used by
// rewriteResultNavigation. Keys are the legacy bare suffix name; values
// are the method-form replacement (with trailing `()`).
var resultNavigationSegments = map[string]string{
	"empty": "Empty()",
	"first": "First()",
	"last":  "Last()",
	"count": "Len()",
	"nodes": "Nodes()",
	"ran":   "Ran()",
}

// rewriteResultNavigation walks the token stream, recognising dotted
// identifiers of the shapes:
//
//  1. `stepName.accessor` (exactly two segments)
//     → `stepName.Accessor()` (method form)
//
//  2. `stepName.accessor.path.to.x` (three or more segments where
//     segment[1] is a result-navigation accessor like `first`, `last`,
//     `empty`, `count`, `nodes`, `ran`)
//     → `stepName.Accessor().path.to.x` (method form followed by a
//     post-call dotted chain, now that the parser accepts chained
//     access after a call expression — see
//     component/language/parser/parser.go parseValue().)
//
// Deeper paths where segment[1] is NOT a result-navigation accessor
// (e.g. `getAgent.result.Bundle.nodes` — `result` is a raw record
// field, not a navigation method) are left alone. Strings, comments,
// and annotation names pass through unchanged — they aren't tokenised
// as bare identifiers.
func rewriteResultNavigation(src []byte) ([]byte, error) {
	return lexicalTokenRewrite(src, func(tok langparser.Token) (string, bool) {
		if tok.Type != langparser.TokenIdentifier {
			return "", false
		}
		literal := tok.Literal
		segments := strings.Split(literal, ".")
		if len(segments) < 2 {
			return "", false
		}
		head, terminal := segments[0], segments[1]
		if head == "" {
			return "", false
		}
		replacement, ok := resultNavigationSegments[terminal]
		if !ok {
			return "", false
		}
		// Two-segment case: `stepName.first` → `stepName.First()`
		if len(segments) == 2 {
			return head + "." + replacement, true
		}
		// Chained case: `stepName.first.payload.x` →
		// `stepName.First().payload.x`. The `.payload.x` tail rejoins
		// the method call via the parser's post-call chaining.
		tail := strings.Join(segments[2:], ".")
		return head + "." + replacement + "." + tail, true
	})
}

// rewriteSliceSyntax rewrites `array(T)` → `[]T` at the token level.
// Recognised forms:
//
//	array(string)         → []string
//	array(v1:foo:bar)     → []v1:foo:bar
//	array( TYPE )         → []TYPE    (whitespace tolerated)
//
// T may be any identifier token. Nested array-of-array isn't in the
// existing grammar, so the rewriter doesn't need to recurse.
func rewriteSliceSyntax(src []byte) ([]byte, error) {
	tokens, err := langparser.NewLexer(string(src)).Tokenize()
	if err != nil {
		return src, nil
	}
	// Drop trailing EOF for indexing sanity.
	if n := len(tokens); n > 0 && tokens[n-1].Type == langparser.TokenEOF {
		tokens = tokens[:n-1]
	}
	// Find all `array ( IDENT )` triples that warrant rewriting. The
	// input is reassembled from the original bytes so we preserve
	// surrounding whitespace and comments; we only substitute the
	// matched run's byte range.
	// Rune space throughout: token positions are rune offsets (the lexer
	// scans []rune), so byte slicing drifts after any multibyte char.
	runes := []rune(string(src))
	type edit struct {
		start, end  int
		replacement string
	}
	var edits []edit
	for i := 0; i+3 < len(tokens); i++ {
		if tokens[i].Type != langparser.TokenIdentifier || tokens[i].Literal != "array" {
			continue
		}
		if tokens[i+1].Type != langparser.TokenParenOpen {
			continue
		}
		if tokens[i+2].Type != langparser.TokenIdentifier {
			continue
		}
		if tokens[i+3].Type != langparser.TokenParenClose {
			continue
		}
		edits = append(edits, edit{
			start:       tokens[i].Pos,
			end:         tokens[i+3].EndPos,
			replacement: "[]" + tokens[i+2].Literal,
		})
	}
	if len(edits) == 0 {
		return src, nil
	}
	out := make([]rune, 0, len(runes))
	prev := 0
	for _, e := range edits {
		out = append(out, runes[prev:e.start]...)
		out = append(out, []rune(e.replacement)...)
		prev = e.end
	}
	out = append(out, runes[prev:]...)
	return []byte(string(out)), nil
}

// lexicalTokenRewrite walks every lexer token in src and gives the
// callback a chance to substitute its text. When the callback returns
// (replacement, true), the token's original byte range is replaced
// with the replacement; otherwise the bytes pass through unchanged.
// Whitespace, comments, and any non-token bytes are preserved.
func lexicalTokenRewrite(src []byte, mapper func(langparser.Token) (string, bool)) ([]byte, error) {
	tokens, err := langparser.NewLexer(string(src)).Tokenize()
	if err != nil {
		return src, nil
	}
	// Rune space throughout: token Pos/EndPos are rune offsets. EndPos is
	// the lexer-stamped half-open end -- Pos+len(Literal) undercounts for
	// string tokens (quotes stripped, escapes decoded).
	runes := []rune(string(src))
	out := make([]rune, 0, len(runes))
	prev := 0
	for _, tok := range tokens {
		if tok.Type == langparser.TokenEOF {
			break
		}
		if replacement, ok := mapper(tok); ok {
			if tok.Pos >= prev {
				out = append(out, runes[prev:tok.Pos]...)
			}
			out = append(out, []rune(replacement)...)
			prev = tok.EndPos
		}
	}
	if prev < len(runes) {
		out = append(out, runes[prev:]...)
	}
	return []byte(string(out)), nil
}

// rewriteArgsDescription strips @description("...") annotations from
// args{} fields. The args-block parser REJECTS the annotation outright
// (memql#3336) -- there is no AST slot for it and an arg description is
// carried by the /// doc comment above the field -- so this rewrite is
// how a file carrying one is made loadable again. Only annotations
// lexically inside an `args { }` block
// are touched -- declaration-level and concept-field @description are
// load-bearing. Tracking happens on the token stream, so `args {` in a
// comment or string never opens a block. A deletion that leaves its
// line blank removes the whole line.
func rewriteArgsDescription(src []byte) ([]byte, error) {
	tokens, err := langparser.NewLexer(string(src)).Tokenize()
	if err != nil {
		return src, nil
	}
	if n := len(tokens); n > 0 && tokens[n-1].Type == langparser.TokenEOF {
		tokens = tokens[:n-1]
	}
	// Token positions are RUNE offsets (the lexer scans []rune), so all
	// range math and slicing happen in rune space -- byte slicing drifts
	// after the first multibyte character in the file.
	runes := []rune(string(src))
	type edit struct{ start, end int }
	var edits []edit
	depth := 0
	argsDepth := -1 // interior depth of the innermost args block; -1 = outside
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		switch tok.Type {
		case langparser.TokenBraceOpen:
			depth++
			if argsDepth < 0 && i > 0 &&
				tokens[i-1].Type == langparser.TokenIdentifier &&
				tokens[i-1].Literal == "args" {
				argsDepth = depth
			}
		case langparser.TokenBraceClose:
			depth--
			if argsDepth >= 0 && depth < argsDepth {
				argsDepth = -1
			}
		case langparser.TokenAt:
			if argsDepth < 0 || i+4 >= len(tokens) {
				continue
			}
			if tokens[i+1].Type != langparser.TokenIdentifier ||
				tokens[i+1].Literal != "description" ||
				tokens[i+1].Pos != tok.Pos+1 ||
				tokens[i+2].Type != langparser.TokenParenOpen ||
				tokens[i+3].Type != langparser.TokenString ||
				tokens[i+4].Type != langparser.TokenParenClose {
				continue
			}
			start := tok.Pos
			for start > 0 && (runes[start-1] == ' ' || runes[start-1] == '\t') {
				start--
			}
			end := tokens[i+4].EndPos // half-open, past the closing paren
			// Standalone-line annotation: remove the line entirely.
			lineStart := start
			for lineStart > 0 && runes[lineStart-1] != '\n' {
				lineStart--
			}
			leadingWS := true
			for j := lineStart; j < start; j++ {
				if runes[j] != ' ' && runes[j] != '\t' {
					leadingWS = false
					break
				}
			}
			if leadingWS {
				switch {
				case end < len(runes) && runes[end] == '\n':
					start, end = lineStart, end+1
				case end+1 < len(runes) && runes[end] == '\r' && runes[end+1] == '\n':
					start, end = lineStart, end+2
				}
			}
			edits = append(edits, edit{start, end})
			i += 4
		}
	}
	if len(edits) == 0 {
		return src, nil
	}
	var out []rune
	out = make([]rune, 0, len(runes))
	prev := 0
	for _, e := range edits {
		out = append(out, runes[prev:e.start]...)
		prev = e.end
	}
	out = append(out, runes[prev:]...)
	return []byte(string(out)), nil
}
