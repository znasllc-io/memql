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
	"io"
	"os"
	"path/filepath"
	"strings"

	langparser "github.com/znasllc-io/memql/component/language/parser"
)

type rewriter func([]byte) ([]byte, error)

var rewriters = map[string]rewriter{
	"result-navigation": rewriteResultNavigation,
	"slice-syntax":      rewriteSliceSyntax,
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
	o, paths, err := parseFlags(args, stderr)
	if err != nil {
		return err
	}

	if len(o.rewrites) == 0 {
		fmt.Fprintln(stderr, "memqlmigrate: --rewrite=NAME is required")
		listRewriters(stderr)
		return errUsage
	}

	pipeline := make([]rewriter, 0, len(o.rewrites))
	for _, name := range o.rewrites {
		fn, ok := rewriters[name]
		if !ok {
			fmt.Fprintf(stderr, "memqlmigrate: unknown rewrite %q\n", name)
			listRewriters(stderr)
			return errUsage
		}
		pipeline = append(pipeline, fn)
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
		out, err := applyPipeline(orig, pipeline)
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
}

func applyPipeline(src []byte, pipeline []rewriter) ([]byte, error) {
	out := src
	for _, fn := range pipeline {
		next, err := fn(out)
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
//       → `stepName.Accessor()` (method form)
//
//  2. `stepName.accessor.path.to.x` (three or more segments where
//     segment[1] is a result-navigation accessor like `first`, `last`,
//     `empty`, `count`, `nodes`, `ran`)
//       → `stepName.Accessor().path.to.x` (method form followed by a
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
	type edit struct{ start, end int; replacement string }
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
		start := tokens[i].Pos
		end := tokens[i+3].Pos + 1 // closing paren is one byte
		edits = append(edits, edit{
			start:       start,
			end:         end,
			replacement: "[]" + tokens[i+2].Literal,
		})
	}
	if len(edits) == 0 {
		return src, nil
	}
	var out bytes.Buffer
	out.Grow(len(src))
	prev := 0
	for _, e := range edits {
		out.Write(src[prev:e.start])
		out.WriteString(e.replacement)
		prev = e.end
	}
	out.Write(src[prev:])
	return out.Bytes(), nil
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
	var out bytes.Buffer
	out.Grow(len(src))
	prev := 0
	for _, tok := range tokens {
		if tok.Type == langparser.TokenEOF {
			break
		}
		if replacement, ok := mapper(tok); ok {
			if tok.Pos >= prev {
				out.Write(src[prev:tok.Pos])
			}
			out.WriteString(replacement)
			prev = tok.Pos + len(tok.Literal)
		}
	}
	if prev < len(src) {
		out.Write(src[prev:])
	}
	return out.Bytes(), nil
}
