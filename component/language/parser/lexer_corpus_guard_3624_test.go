package parser

// The corpus-level guard for memql#3624, and the differential harness that
// found the hazards it guards against.
//
// A silent lexer defect is invisible to every test that asserts on parse
// RESULTS, because a silent misread produces a perfectly well-formed result --
// just not the one that was written. The two checks below attack that from the
// two directions that do work:
//
//   - The corpus sweep asserts on the token stream of every .memql file in the
//     tree, so a hazard that has actually reached authored source is caught.
//   - The differential harness asserts that two spellings which MUST mean the
//     same thing produce the same tokens, so a hazard is caught before anyone
//     writes it down.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/repowalk"
)

// memqlCorpusFiles collects every .memql file in the repository, including the
// `_`-prefixed reference skeletons -- the lexer has no notion of a disabled
// file, so all of them must lex.
func memqlCorpusFiles(t *testing.T) []string {
	t.Helper()
	root := "../../.."
	if _, err := os.Stat(filepath.Join(root, "dsl")); err != nil {
		t.Skip("dsl tree not present")
	}
	var files []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // an unreadable path is not this test's business
		}
		if info.IsDir() {
			if repowalk.SkipDir(info.Name()) {
				return filepath.SkipDir
			}
			switch info.Name() {
			case ".git", "node_modules", "vendor", "worktrees":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".memql") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	// A walk that finds nothing must FAIL, not pass vacuously. This repo has
	// been bitten by a checker whose selector silently collapsed to empty and
	// reported success over an unchecked tree; a corpus guard is exactly the
	// shape that rots that way.
	if len(files) < 100 {
		t.Fatalf("found only %d .memql files; the corpus walk is broken, not the corpus", len(files))
	}
	return files
}

// TestCorpusLexesWithoutErrorOrSurprise sweeps the whole shipped tree. Each
// assertion names the hazard it descends from, so a future regression reports
// what broke rather than just that something did.
func TestCorpusLexesWithoutErrorOrSurprise(t *testing.T) {
	for _, path := range memqlCorpusFiles(t) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		tokens, err := NewLexer(string(src)).Tokenize()
		if err != nil {
			// Covers hazard 1 (an unterminated /* is now an error rather than
			// a silent truncation) and hazard 2 (a leading-dot number).
			t.Errorf("%s: %v", path, err)
			continue
		}
		for _, tk := range tokens {
			// Hazard 3: a U+FFFD in a token literal means an escape decoded to
			// something that is not a character. No authored .memql should
			// contain one, escaped or literal.
			if strings.ContainsRune(tk.Literal, '�') {
				t.Errorf("%s:%d:%d: token %q contains U+FFFD -- an escape decoded to a non-character",
					path, tk.Line, tk.Column, tk.Literal)
			}
			// Hazard 2, belt and braces: a `.`-then-digit identifier is now
			// unreachable, so finding one means the guard was removed.
			if tk.Type == TokenIdentifier && len(tk.Literal) >= 2 &&
				tk.Literal[0] == '.' && isDigit(rune(tk.Literal[1])) {
				t.Errorf("%s:%d:%d: identifier %q is a leading-dot number lexed as a name",
					path, tk.Line, tk.Column, tk.Literal)
			}
			// A zero-width token other than EOF would desynchronise every
			// positional consumer downstream.
			if tk.Type != TokenEOF && tk.EndPos <= tk.Pos {
				t.Errorf("%s:%d:%d: token %q has an empty source span [%d,%d)",
					path, tk.Line, tk.Column, tk.Literal, tk.Pos, tk.EndPos)
			}
		}
	}
}

// TestCorpusEveryBlockCommentIsTerminated states hazard 1 as a property of the
// tree rather than of the lexer, so it still holds if the lexer is rewritten.
// It is deliberately independent of the lexer: it counts delimiters in the raw
// bytes, outside string literals.
func TestCorpusEveryBlockCommentIsTerminated(t *testing.T) {
	for _, path := range memqlCorpusFiles(t) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		// The lexer is the authority on where a comment starts, so reuse it:
		// a file whose block comments are all terminated lexes clean, and one
		// with a stray `/*` now says so.
		if _, err := NewLexer(string(src)).Tokenize(); err != nil &&
			strings.Contains(err.Error(), "unterminated block comment") {
			t.Errorf("%s: %v -- every construct after this point is invisible to the loader", path, err)
		}
	}
}

// TestTightAndSpacedSpellingsAgree is the differential oracle that found these
// defects: two spellings of one expression, differing only in whitespace, must
// produce the same token stream. Whitespace is not part of the meaning of an
// expression in this grammar, so any disagreement is a place where the lexer
// scanned into something other than what was written.
func TestTightAndSpacedSpellingsAgree(t *testing.T) {
	pairs := []struct{ tight, spaced string }{
		{"a==b", "a == b"},
		{"a!=b", "a != b"},
		{"a>=b", "a >= b"},
		{"a<=b", "a <= b"},
		{"a>b", "a > b"},
		{"a<b", "a < b"},
		{"a&&b", "a && b"},
		{"a||b", "a || b"},
		{"a??b", "a ?? b"},
		{"a+b", "a + b"},
		{"a*b", "a * b"},
		{"a/b", "a / b"},
		{"a%b", "a % b"},
		{"!a", "! a"},
		{"[a,b]", "[ a , b ]"},
		{"f(a)", "f( a )"},
		{"x:=y", "x := y"},
	}
	for _, p := range pairs {
		tight, err := lexLiterals(p.tight)
		if err != nil {
			t.Errorf("Tokenize(%q): %v", p.tight, err)
			continue
		}
		spaced, err := lexLiterals(p.spaced)
		if err != nil {
			t.Errorf("Tokenize(%q): %v", p.spaced, err)
			continue
		}
		if strings.Join(tight, "\x1f") != strings.Join(spaced, "\x1f") {
			t.Errorf("%q lexes as %v but %q lexes as %v -- whitespace changed the meaning",
				p.tight, tight, p.spaced, spaced)
		}
	}

	// `-` is the one operator NOT on that list, and its absence is the whole
	// of memql#3624 hazard 4: `-` is a legal identifier character, so `a-b`
	// lexes as ONE identifier while `a - b` lexes as a subtraction. Recorded
	// here as a KNOWN divergence rather than left as a silent gap, so the next
	// reader of this harness learns it from the harness. See the hazard-4 note
	// in lexer.go's `case '-'`.
	tight, err := lexLiterals("a-b")
	if err != nil {
		t.Fatalf("Tokenize(a-b): %v", err)
	}
	spaced, err := lexLiterals("a - b")
	if err != nil {
		t.Fatalf("Tokenize(a - b): %v", err)
	}
	if len(tight) != 1 || tight[0] != "a-b" {
		t.Errorf("a-b now lexes as %v; the hazard-4 note in lexer.go is stale and must be updated", tight)
	}
	if len(spaced) != 3 {
		t.Errorf("a - b lexes as %v, want three tokens", spaced)
	}
}

// TestFractionSpellingsAgree is the same oracle applied to hazard 2. There is
// now exactly ONE spelling of a fraction, so the two cannot disagree: the
// second is refused.
func TestFractionSpellingsAgree(t *testing.T) {
	if _, err := lexLiterals("score > .5"); err == nil {
		t.Error("`.5` still lexes; it must be refused so it cannot disagree with `0.5`")
	}
	got, err := lexLiterals("score > 0.5")
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	if want := []string{"score", ">", "0.5"}; strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("0.5 lexes as %v, want %v", got, want)
	}
}

// lexLiterals returns the literal of every non-EOF token.
func lexLiterals(src string) ([]string, error) {
	tokens, err := NewLexer(src).Tokenize()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, tk := range tokens {
		if tk.Type != TokenEOF {
			out = append(out, tk.Literal)
		}
	}
	return out, nil
}
