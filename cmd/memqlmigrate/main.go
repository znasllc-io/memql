// memqlmigrate applies named syntax rewrites to MemQL source files.
//
// Usage:
//
//	memqlmigrate --rewrite=<name>[,<name>...] [-w] [-check] <path>...
//
// Companion to memqlfmt. Where memqlfmt normalises whitespace and
// indentation, memqlmigrate performs one-shot syntactic rewrites that
// retire a deprecated form. One named rewrite per legacy form so the
// tool runs idempotently per release, matching the shape of `go fix`.
//
// Supported rewrites:
//
//	result-navigation   .empty / .first / .last / .count / .nodes
//	                    (magical field access on step results) →
//	                    .Empty() / .First() / .Last() / .Len() / .Nodes()
//	                    (method-style, recognised by the evaluator).
//
//	slice-syntax        array(T) → []T (Go-aligned slice type spelling).
//
//	args-description    strip @description("...") from args{} fields --
//	                    the parser discards the annotation (no AST slot,
//	                    #2615); declaration-level and concept-field
//	                    @description are load-bearing and untouched.
//
//	accept-stamp        collapse arg-mirror runs in mutation write blocks
//	                    into the accept/stamp form (#2616; form shipped by
//	                    #2593). Only provably-safe blocks rewrite -- the
//	                    result is verified equivalent through the engine's
//	                    own mutation emitter.
//
//	same-domain-use     delete use imports of the file's own domain
//	                    (#2617: same-domain constructs are ambient); the
//	                    domain is the file's containing directory, so this
//	                    rewrite is path-aware.
//
// Flags:
//
//	--rewrite=NAME[,NAME...]   comma-separated list of rewrites to apply
//	-w                         rewrite files in place (otherwise print)
//	-check                     exit non-zero and list files that would change
//
// Without -w and without -check, the tool prints the rewritten content
// for each file to stdout. Rewrites apply at the lexical layer; the
// tool is conservative and leaves input unchanged when it cannot
// safely perform the transformation.
package main

import (
	"bytes"
	"fmt"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"io"
	"os"
	"path/filepath"
	"strings"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

type rewriter func([]byte) ([]byte, error)

// pathRewriter is a rewrite that needs the file's path (e.g. to derive
// the containing domain directory, #2617).
type pathRewriter func(path string, src []byte) ([]byte, error)

var rewriters = map[string]rewriter{
	"result-navigation": rewriteResultNavigation,
	"slice-syntax":      rewriteSliceSyntax,
	"args-description":  rewriteArgsDescription,
	"accept-stamp":      langparser.RewriteAcceptStamp,
	"required-sigil":    langparser.RewriteRequiredSigil,
	"enum-type":         langparser.RewriteEnumTypeArgs,
	"cache-positional":  langparser.RewriteCachePositional,
	"terse-automation":  langparser.RewriteLonghandSingleStepAutomation,
}

var pathRewriters = map[string]pathRewriter{
	"same-domain-use": rewriteSameDomainUse,
}

// rewriteSameDomainUse derives the file's domain (its containing
// directory) and delegates to the engine-package rewrite the dsl/
// conformance gate also runs.
func rewriteSameDomainUse(path string, src []byte) ([]byte, error) {
	return langparser.RewriteSameDomainUse(filepath.Base(filepath.Dir(path)), src)
}

type opts struct {
	rewrites []string
	check    bool
	write    bool
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	// --grammar-version prints the engine's grammar epoch + keyword
	// fingerprint (S6, memql#2361) -- the value stamped into authored rows
	// and release lockfiles, and the identity of the migration channel.
	if len(args) == 1 && args[0] == "--grammar-version" {
		fmt.Fprintf(stdout, "%s (fingerprint %s)\n", languageParser.GrammarVersion, languageParser.GrammarFingerprint())
		return nil
	}

	o, paths, err := parseFlags(args, stderr)
	if err != nil {
		return err
	}

	if len(o.rewrites) == 0 {
		fmt.Fprintln(stderr, "memqlmigrate: --rewrite=NAME is required")
		listRewriters(stderr)
		return errUsage
	}

	pipeline := make([]pathRewriter, 0, len(o.rewrites))
	for _, name := range o.rewrites {
		if fn, ok := rewriters[name]; ok {
			plain := fn
			pipeline = append(pipeline, func(_ string, b []byte) ([]byte, error) { return plain(b) })
			continue
		}
		if fn, ok := pathRewriters[name]; ok {
			pipeline = append(pipeline, fn)
			continue
		}
		fmt.Fprintf(stderr, "memqlmigrate: unknown rewrite %q\n", name)
		listRewriters(stderr)
		return errUsage
	}

	expanded, err := expandPaths(paths)
	if err != nil {
		return fmt.Errorf("memqlmigrate: %w", err)
	}

	changed := 0
	for _, p := range expanded {
		orig, err := os.ReadFile(p)
		if err != nil {
			return fmt.Errorf("memqlmigrate: read %s: %w", p, err)
		}
		out, err := applyPipeline(p, orig, pipeline)
		if err != nil {
			return fmt.Errorf("memqlmigrate: rewrite %s: %w", p, err)
		}
		if o.check {
			if !bytes.Equal(orig, out) {
				fmt.Fprintln(stdout, p)
				changed++
			}
			continue
		}
		if o.write {
			if !bytes.Equal(orig, out) {
				if err := os.WriteFile(p, out, 0o644); err != nil {
					return fmt.Errorf("memqlmigrate: write %s: %w", p, err)
				}
				changed++
			}
			continue
		}
		if _, err := stdout.Write(out); err != nil {
			return fmt.Errorf("memqlmigrate: stdout: %w", err)
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
	var o opts
	paths := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-w":
			o.write = true
		case a == "-check":
			o.check = true
		case a == "--rewrite":
			if i+1 >= len(args) {
				fmt.Fprintln(stderr, "memqlmigrate: --rewrite requires a value")
				return o, nil, errUsage
			}
			i++
			o.rewrites = append(o.rewrites, splitCSV(args[i])...)
		case strings.HasPrefix(a, "--rewrite="):
			o.rewrites = append(o.rewrites, splitCSV(strings.TrimPrefix(a, "--rewrite="))...)
		case a == "-h", a == "--help":
			printUsage(stderr)
			return o, nil, errUsage
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(stderr, "memqlmigrate: unknown flag %q\n", a)
			printUsage(stderr)
			return o, nil, errUsage
		default:
			paths = append(paths, a)
		}
	}
	if len(paths) == 0 {
		printUsage(stderr)
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

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: memqlmigrate --rewrite=NAME[,NAME...] [-w] [-check] <path>...")
	listRewriters(w)
}

func listRewriters(w io.Writer) {
	fmt.Fprintln(w, "available rewrites:")
	fmt.Fprintln(w, "  result-navigation   .empty/.first/.last/.count/.nodes → method forms")
	fmt.Fprintln(w, "  slice-syntax        array(T) → []T")
	fmt.Fprintln(w, "  args-description    strip parser-discarded @description from args{} fields")
	fmt.Fprintln(w, "  accept-stamp        collapse arg-mirror runs into accept{}/stamp{} (#2616)")
	fmt.Fprintln(w, "  same-domain-use     delete use imports of the file's own domain (#2617)")
	fmt.Fprintln(w, "  required-sigil      @required -> the `type!` sigil (#2618)")
	fmt.Fprintln(w, "  enum-type           args `string @enum(...)` -> the enum(...) type (#2618)")
	fmt.Fprintln(w, "  cache-positional    @cache(ttl=\"N\") -> @cache(N) (#2618)")
	fmt.Fprintln(w, "  terse-automation    longhand single-step automations -> the => form (#2619)")
}

func applyPipeline(path string, src []byte, pipeline []pathRewriter) ([]byte, error) {
	out := src
	for _, fn := range pipeline {
		next, err := fn(path, out)
		if err != nil {
			return nil, err
		}
		out = next
	}
	return out, nil
}

func expandPaths(paths []string) ([]string, error) {
	var out []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			out = append(out, p)
			continue
		}
		err = filepath.WalkDir(p, func(path string, d os.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			if d.IsDir() {
				return nil
			}
			if filepath.Ext(path) != ".memql" {
				return nil
			}
			out = append(out, path)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
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
// args{} fields (#2615). The args-block parser consumes the annotation
// and throws it away ("silently accepted (no AST slot)"), so the prose
// is dead weight. Only annotations lexically inside an `args { }` block
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
