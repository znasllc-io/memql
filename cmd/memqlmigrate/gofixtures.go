package main

// gofixtures.go -- `memqlmigrate --rewrite=bodies --go-fixtures`: the bodies
// rewrite over the MemQL that Go test files embed in string literals (epic
// memql#5370, task memql#5373; the flip, plan Task 13).
//
// The flip deletes the retired body grammar, so every fixture that still
// writes a `step` block, a logic's `body { }`, the terse header or a `mutate`
// declaration stops loading at the same moment. memqlmigrate's tree run cannot
// reach them: they are Go string literals, not .memql files. This mode is the
// same rewrite for them -- the expressions epic's
// scripts/migrations/expressions_go_fixtures, with this rewrite
// (component/language/bodymigrate), whose own reader of the retired forms
// outlives the engine's.
//
// Per *_test.go file:
//
//   - every string literal, raw or interpreted, whose text declares an
//     automation, a logic or a `mutate`, or writes a @trigger, is run through
//     the bodies rewrite as a tree of one file (bodymigrate.RewriteSource), its
//     calls resolved against the engine's embedded tree plus the file's own
//     literals -- a fixture that declares a logic in one literal and calls it
//     from another resolves it;
//   - the result goes back in the literal's own form: a raw literal stays raw,
//     an interpreted one is re-quoted;
//   - a literal the rewrite cannot carry is REPORTED with the rewrite's reason,
//     never guessed; so is a FRAGMENT, a retired body form with no construct
//     header in the literal to rewrite it from.
//
// It is dry by default and prints what it would change and what it refuses;
// -w writes. A second run is a no-op. A comment carrying `memqlmigrate:keep`
// on a literal's line or the line above keeps that literal, and
// `memqlmigrate:keep-file` anywhere keeps the file -- the markers the
// expressions tool reads. --exclude=PREFIX[,...] leaves paths alone; the
// defaults are this command's own tests, the bodies rewrite's and the
// migration scripts', whose inputs are the retired forms on purpose.

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/znasllc-io/memql/component/language/bodymigrate"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/repowalk"
)

// goFixtureDefaultExcludes are the tests whose literals are retired on purpose.
var goFixtureDefaultExcludes = []string{"cmd/memqlmigrate/", "component/language/bodymigrate/", "scripts/migrations/"}

const (
	goFixtureKeep     = "memqlmigrate:keep"
	goFixtureKeepFile = "memqlmigrate:keep-file"
)

var (
	// What makes a literal's text something the bodies rewrite can carry: a
	// construct header it rewrites, or a trigger it may strip.
	goFixtureHeader = regexp.MustCompile(`(?m)^[ \t]*(?:(?:automation|logic)[ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]*(?:\{|=>)|mutate[ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]*\{|@trigger[ \t]*\()`)
	// A retired body form, for telling a fragment from a literal with nothing
	// retired in it.
	goFixtureRetired = regexp.MustCompile(`(?m)^[ \t]*(?:step[ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]*\{|body[ \t]*\{)`)
)

type goFixtureOptions struct {
	write    bool
	verbose  bool
	excludes []string
}

type goFixtureOutcome int

const (
	goFixtureUnchanged goFixtureOutcome = iota
	goFixtureChanged
	goFixtureRefused
	goFixtureFragment
	goFixtureKept
)

type goFixtureLiteral struct {
	line       int
	start, end int // byte offsets of the literal, quotes included
	raw        bool
	text       string
	kept       bool
}

type goFixtureResult struct {
	lit     goFixtureLiteral
	outcome goFixtureOutcome
	newText string
	reason  string
}

type goFixtureFile struct {
	path    string
	results []goFixtureResult
	out     []byte
}

// runGoFixtures is the mode, reporting to w.
func runGoFixtures(w io.Writer, paths []string, o goFixtureOptions) error {
	files, err := goFixtureFiles(paths, o.excludes)
	if err != nil {
		return err
	}
	base, err := buildDeclIndex(nil)
	if err != nil {
		return err
	}
	var reports []goFixtureFile
	for _, p := range files {
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rep, err := goFixtureProcess(p, src, base)
		if errors.Is(err, errGoFixtureNotGo) {
			fmt.Fprintf(w, "SKIP     %s: %v\n", p, err)
			continue
		}
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		reports = append(reports, rep)
	}
	goFixtureReport(w, reports, o.verbose, o.write)
	if !o.write {
		return nil
	}
	for _, rep := range reports {
		if rep.out == nil {
			continue
		}
		info, err := os.Stat(rep.path)
		if err != nil {
			return err
		}
		if err := os.WriteFile(rep.path, rep.out, info.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

// goFixtureFiles lists the *_test.go files under paths, outside the excludes,
// the repository's skip list and testdata.
func goFixtureFiles(paths, excludes []string) ([]string, error) {
	var out []string
	add := func(p string) {
		slash := filepath.ToSlash(filepath.Clean(p))
		for _, ex := range excludes {
			if strings.HasPrefix(slash, ex) || strings.HasPrefix(slash, "./"+ex) {
				return
			}
		}
		if strings.HasSuffix(slash, "_test.go") {
			out = append(out, p)
		}
	}
	for _, root := range paths {
		info, err := os.Stat(root)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			add(root)
			continue
		}
		err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if repowalk.SkipDir(d.Name()) || d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			add(p)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(out)
	return out, nil
}

var errGoFixtureNotGo = errors.New("does not parse as Go")

// goFixtureProcess rewrites one test file's literals.
func goFixtureProcess(p string, src []byte, base *bodymigrate.Index) (goFixtureFile, error) {
	rep := goFixtureFile{path: p}
	lits, keepFile, err := goFixtureLiterals(p, src)
	if err != nil {
		return rep, err
	}
	var dsl []goFixtureLiteral
	for _, l := range lits {
		mask := langparser.BlankCommentsAndStrings(l.text)
		if goFixtureHeader.MatchString(mask) || goFixtureRetired.MatchString(mask) {
			dsl = append(dsl, l)
		}
	}
	if len(dsl) == 0 {
		return rep, nil
	}
	if keepFile {
		for _, l := range dsl {
			rep.results = append(rep.results, goFixtureResult{lit: l, outcome: goFixtureKept, reason: goFixtureKeepFile})
		}
		return rep, nil
	}
	// The file's own declarations resolve its calls, over the engine's.
	ix := base.Clone()
	for _, l := range dsl {
		ix.AddSource(l.text)
	}

	var edits []goFixtureEdit
	for _, l := range dsl {
		res := goFixtureRewrite(l, ix)
		rep.results = append(rep.results, res)
		if res.outcome == goFixtureChanged {
			edits = append(edits, goFixtureEdit{start: l.start, end: l.end, text: goFixtureEncode(l, res.newText)})
		}
	}
	if len(edits) == 0 {
		return rep, nil
	}
	out := goFixtureApply(src, edits)
	// A literal is replaced by a literal, so a file that no longer parses is an
	// encoding bug -- and is never written.
	if _, err := parser.ParseFile(token.NewFileSet(), p, out, parser.SkipObjectResolution); err != nil {
		return rep, fmt.Errorf("internal: the rewritten file does not parse as Go: %w", err)
	}
	rep.out = out
	return rep, nil
}

// goFixtureLiterals returns every string literal in a Go file, decoded, and
// whether the file carries the keep-file marker.
func goFixtureLiterals(p string, src []byte) ([]goFixtureLiteral, bool, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, p, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", errGoFixtureNotGo, err)
	}
	keepLines := map[int]bool{}
	keepFile := false
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			if strings.Contains(c.Text, goFixtureKeepFile) {
				keepFile = true
			} else if strings.Contains(c.Text, goFixtureKeep) {
				keepLines[fset.Position(c.End()).Line] = true
			}
		}
	}
	var out []goFixtureLiteral
	ast.Inspect(f, func(n ast.Node) bool {
		bl, ok := n.(*ast.BasicLit)
		if !ok || bl.Kind != token.STRING {
			return true
		}
		text, err := strconv.Unquote(bl.Value)
		if err != nil {
			return true
		}
		start := fset.Position(bl.Pos())
		out = append(out, goFixtureLiteral{
			line:  start.Line,
			start: start.Offset,
			end:   start.Offset + len(bl.Value),
			raw:   strings.HasPrefix(bl.Value, "`"),
			text:  text,
			kept:  keepLines[start.Line] || keepLines[start.Line-1],
		})
		return true
	})
	return out, keepFile, nil
}

// goFixtureRewrite runs the bodies rewrite over one literal.
func goFixtureRewrite(l goFixtureLiteral, ix *bodymigrate.Index) goFixtureResult {
	if l.kept {
		return goFixtureResult{lit: l, outcome: goFixtureKept, reason: goFixtureKeep}
	}
	next, err := bodymigrate.RewriteSource(l.text, ix)
	if err != nil {
		return goFixtureResult{lit: l, outcome: goFixtureRefused, reason: strings.Join(strings.Fields(err.Error()), " ")}
	}
	if next != l.text {
		return goFixtureResult{lit: l, outcome: goFixtureChanged, newText: next}
	}
	mask := langparser.BlankCommentsAndStrings(l.text)
	if goFixtureRetired.MatchString(mask) && !goFixtureHeader.MatchString(mask) {
		return goFixtureResult{lit: l, outcome: goFixtureFragment,
			reason: "a retired body form with no automation or logic header in this literal (the construct is split across literals)"}
	}
	return goFixtureResult{lit: l, outcome: goFixtureUnchanged}
}

// goFixtureEncode writes a rewritten text back in its literal's form: a raw
// literal stays raw unless the text now holds a backquote.
func goFixtureEncode(l goFixtureLiteral, text string) string {
	if l.raw && !strings.Contains(text, "`") {
		return "`" + text + "`"
	}
	return strconv.Quote(text)
}

type goFixtureEdit struct {
	start, end int
	text       string
}

func goFixtureApply(src []byte, edits []goFixtureEdit) []byte {
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	var b bytes.Buffer
	prev := 0
	for _, e := range edits {
		b.Write(src[prev:e.start])
		b.WriteString(e.text)
		prev = e.end
	}
	b.Write(src[prev:])
	return b.Bytes()
}

// goFixtureReport prints the refusals and fragments always, the changes with
// -v, and a summary line.
func goFixtureReport(w io.Writer, reports []goFixtureFile, verbose, wrote bool) {
	var nChanged, nFiles, nRefused, nFragment, nKept int
	for _, rep := range reports {
		fileChanged := false
		for _, r := range rep.results {
			at := fmt.Sprintf("%s:%d", rep.path, r.lit.line)
			switch r.outcome {
			case goFixtureChanged:
				nChanged++
				fileChanged = true
				if verbose {
					fmt.Fprintf(w, "CHANGE   %s\n", at)
				}
			case goFixtureRefused:
				nRefused++
				fmt.Fprintf(w, "REFUSED  %s: %s\n", at, r.reason)
			case goFixtureFragment:
				nFragment++
				fmt.Fprintf(w, "FRAGMENT %s: %s\n", at, r.reason)
			case goFixtureKept:
				nKept++
				if verbose {
					fmt.Fprintf(w, "KEPT     %s (%s)\n", at, r.reason)
				}
			}
		}
		if fileChanged {
			nFiles++
		}
	}
	verb := "would change"
	if wrote {
		verb = "changed"
	}
	fmt.Fprintf(w, "%d literal(s) in %d file(s) %s; %d refused; %d fragment(s) not rewritten; %d kept\n",
		nChanged, nFiles, verb, nRefused, nFragment, nKept)
}
