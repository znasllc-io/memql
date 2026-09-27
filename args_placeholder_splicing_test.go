// Static guard: no Go source builds MemQL text by filling `{args.X}`
// placeholders (memql#5432).
//
// # What this refuses
//
// A template such as `query skuLevels(sku: {args.sku})`, filled by replacing
// each `{args.X}` with a rendered value and handing the result to the engine as
// query TEXT. core/liveknowledge did exactly that for a `v1:knowledge:liveSource`
// row's queryTemplate, and it was the last Go code that did: memql#5363 (E7)
// had already removed the same pattern from tool handlers, where a handler is
// now a call whose arguments read `args.<name>` and are bound as values. The
// connector was dormant -- neither the liveSource nor the liveConnector concept
// was declared anywhere -- and memql#5432 retired it rather than reviving it.
//
// The pattern is worth a gate rather than a memory because its failure is
// quiet. The value becomes query SOURCE, so the renderer's quoting is the only
// thing standing between an argument and the grammar; core/liveknowledge had
// already been fixed once for exactly that (memql#3192, a control byte made the
// filled query unparseable), and its own dispatcher still built lookups with
// Go's %q, which disagrees with the lexer on four control characters.
//
// # How it looks
//
// It tokenizes every tracked non-test Go file and reads each STRING literal's
// value, so a comment describing the pattern is not a finding and a literal is
// found whatever its quoting. It matches the template spelling (`{args.sku}`)
// and the regexp that fills one (`\{args\.`), which is how core/liveknowledge
// spelled it. Test files are skipped: a test may carry the retired form as a
// fixture, as this one does.
package main

import (
	"go/scanner"
	"go/token"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// argsPlaceholder matches the `{args.` placeholder opener inside a literal's
// value, with or without the regexp escape before the dot.
var argsPlaceholder = regexp.MustCompile(`\{args\\?\.`)

// argsPlaceholderLiterals returns the line of every string literal in src whose
// value carries the placeholder, and how many string literals it read.
func argsPlaceholderLiterals(filename string, src []byte) (lines []int, literals int) {
	fset := token.NewFileSet()
	file := fset.AddFile(filename, fset.Base(), len(src))
	var s scanner.Scanner
	// Errors are ignored rather than fatal: the scanner resynchronises, and a
	// file the compiler accepts does not produce them.
	s.Init(file, src, func(token.Position, string) {}, 0)
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			return lines, literals
		}
		if tok != token.STRING {
			continue
		}
		literals++
		value, err := strconv.Unquote(lit)
		if err != nil {
			value = lit
		}
		if argsPlaceholder.MatchString(value) {
			lines = append(lines, fset.Position(pos).Line)
		}
	}
}

// TestArgsPlaceholderScanFindsTheRetiredForm is the negative control, run on
// every pass: the scanner must flag the literal core/liveknowledge shipped, in
// both quotings, and must not flag the same text in a comment. Without it a
// scanner that silently stopped matching would make the gate below pass on
// any tree.
func TestArgsPlaceholderScanFindsTheRetiredForm(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want int
	}{
		{
			name: "the regexp core/liveknowledge filled templates with (raw string)",
			src:  "package x\nvar argPlaceholder = regexp.MustCompile(`\\{args\\.([A-Za-z_][A-Za-z0-9_]*)\\}`)\n",
			want: 1,
		},
		{
			name: "the same regexp as an interpreted string",
			src:  "package x\nvar re = \"\\\\{args\\\\.([A-Za-z_]+)\\\\}\"\n",
			want: 1,
		},
		{
			name: "a template carrying the placeholder",
			src:  "package x\nconst tmpl = \"query skuLevels(sku: {args.sku})\"\n",
			want: 1,
		},
		{
			name: "a comment describing the pattern is not a finding",
			src:  "package x\n// a queryTemplate with {args.x} placeholders\nconst ok = \"query skuLevels(sku: args.sku)\"\n",
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, literals := argsPlaceholderLiterals("fixture.go", []byte(tc.src))
			if literals == 0 {
				t.Fatalf("read no string literals from the fixture -- the scanner is not reading it")
			}
			if len(got) != tc.want {
				t.Errorf("flagged %d literal(s) at lines %v, want %d", len(got), got, tc.want)
			}
		})
	}
}

// TestNoArgsPlaceholderSplicingIntoMemQLText is the gate.
func TestNoArgsPlaceholderSplicingIntoMemQLText(t *testing.T) {
	out, err := exec.Command("git", "ls-files", "-z", "*.go").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}

	var scanned, literals int
	var findings []string
	for _, rel := range strings.Split(string(out), "\x00") {
		if rel == "" || strings.HasSuffix(rel, "_test.go") || strings.Contains(rel, "/node_modules/") {
			continue
		}
		src, err := os.ReadFile(rel)
		if err != nil {
			continue
		}
		scanned++
		lines, n := argsPlaceholderLiterals(rel, src)
		literals += n
		for _, line := range lines {
			findings = append(findings, rel+":"+strconv.Itoa(line))
		}
	}

	// COVERAGE FLOOR. A walk that read nothing, or read files and found no
	// string literal in them, would pass on any tree.
	if scanned < 500 {
		t.Fatalf("scanned only %d Go files -- this gate is not looking at the repository", scanned)
	}
	if literals < 10000 {
		t.Fatalf("read only %d string literals across %d files -- the scanner is not tokenizing them", literals, scanned)
	}

	if len(findings) > 0 {
		t.Errorf("%d string literal(s) carry an `{args.X}` placeholder, the text-splicing form "+
			"memql#5363 removed from tool handlers and memql#5432 retired with core/liveknowledge:\n  %s\n"+
			"Filling a placeholder makes an argument's value part of the query SOURCE, so its quoting "+
			"is all that stands between the value and the grammar. Build the call from values instead: "+
			"langparser.RenderCall(name, args) renders one call with every value encoded as a literal "+
			"that cannot break out of itself, and a DSL tool handler names `args.<name>` and is bound, "+
			"never filled.",
			len(findings), strings.Join(findings, "\n  "))
	}
}
