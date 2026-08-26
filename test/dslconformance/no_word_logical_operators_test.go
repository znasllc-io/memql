package dslconformance

import (
	"github.com/znasllc-io/memql/dsl"
	"io"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/dslfs"
)

// blankQuotedAndComments replaces the contents of quoted string literals with
// spaces and drops a trailing `//` line comment, so prose inside @description /
// string literals and comments can't trip the word-operator scan.
func blankQuotedAndComments(line string) string {
	if idx := strings.Index(line, "//"); idx >= 0 {
		// Only treat `//` as a comment when it's outside a string. Good enough
		// for the scan: blank quotes first would be circular, so approximate by
		// cutting at the first `//` not preceded by a `:` (URLs are rare in
		// conditions and are quoted anyway).
		line = line[:idx]
	}
	var b strings.Builder
	inDouble := false
	inSingle := false
	// ESCAPES ARE HONOURED, and they have to be. Without this a `\"` inside a
	// description closed the string, so the REST of the line was scanned as
	// code -- and any ordinary prose after a quoted phrase ("... and ...") was
	// reported as an infix boolean operator. A false positive here is worse
	// than a miss: it teaches an author that quoting a phrase in a description
	// is forbidden, which is not a rule this repo has.
	escaped := false
	for _, r := range line {
		if escaped {
			escaped = false
			b.WriteRune(' ')
			continue
		}
		if r == '\\' && (inDouble || inSingle) {
			escaped = true
			b.WriteRune(' ')
			continue
		}
		switch r {
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
			b.WriteRune(' ')
			continue
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
			b.WriteRune(' ')
			continue
		}
		if inDouble || inSingle {
			b.WriteRune(' ')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// TestNoInfixWordAndOr is the #973 guardrail: the English `and` / `or` infix
// boolean operators are rejected tree-wide. They never evaluated correctly --
// the runtime condition normaliser only ever rewrote `&&` / `||`, so a word
// form fell through to a malformed single comparison. `&&` / `||` are the
// single canonical AND / OR everywhere.
func TestNoInfixWordAndOr(t *testing.T) {
	tree := dsl.Tree()
	paths, err := dslfs.WalkMemqlFiles(tree)
	if err != nil {
		t.Fatalf("WalkMemqlFiles: %v", err)
	}
	for _, p := range paths {
		file, openErr := tree.Open(p)
		if openErr != nil {
			t.Fatalf("open %s: %v", p, openErr)
		}
		raw, readErr := io.ReadAll(file)
		file.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", p, readErr)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			scan := blankQuotedAndComments(line)
			if strings.Contains(scan, " and ") || strings.Contains(scan, " or ") {
				t.Errorf("%s:%d uses an infix word boolean operator -- use `&&` / `||`:\n  %s", p, i+1, strings.TrimSpace(line))
			}
		}
	}
}
