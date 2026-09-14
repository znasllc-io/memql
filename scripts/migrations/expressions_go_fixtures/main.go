// Command expressions_go_fixtures carries the MemQL source that Go test files
// embed in string literals onto the edition-2026 expression forms (epic
// memql#5363, memql#5368), with the rewrite `memqlmigrate
// --rewrite=expressions` runs over .memql files (langparser.RewriteExpressions).
//
// Edition 2026 refuses every legacy predicate form at parse, so a fixture that
// still writes one -- a struct query's `filter status == "x"`, a
// `spec ... { return ... }` body, an `@filter(payload.x == 1)`, a `cond(...)`
// in a logic body -- does not load. memqlmigrate cannot reach those: they are
// Go string literals, not .memql files. This tool is memqlmigrate for them.
//
// What it does, per *_test.go file:
//
//   - finds every string literal, raw (`...`) or interpreted ("..."), whose
//     text carries struct-form MemQL: a query / mutate / mutation / logic /
//     automation / spec / trait header, a `filter` clause, or an `@filter(`;
//   - runs the expressions rewrite over the literal's DECODED text, with the
//     tree's predicate set (every spec and trait under -dsl, collected once)
//     plus whatever the file's own literals declare -- a fixture that defines
//     a trait in one literal and filters on it in another resolves it. The
//     file's declarations are resolved OVER the tree, so a spec a fixture
//     declares over the core @actor shape actorEnvelope, without declaring
//     the shape, is an actor predicate; a binding nothing declares is
//     refused by name;
//   - writes the result back in the literal's own form: a raw literal stays
//     raw, byte for byte outside the rewritten spans, so its indentation is the
//     indentation it had; an interpreted literal is re-quoted;
//   - REPORTS every literal it cannot rewrite rather than guessing: a clause
//     the rewrite refuses (with its reason), and a FRAGMENT -- a legacy clause
//     whose construct is split across literals, so no single literal holds the
//     header the rewrite needs.
//
// It is DRY by default: it prints what it would change and what it refuses,
// and writes nothing. -w writes. A second run is a no-op.
//
// Some tests pin the legacy forms on purpose -- the codemod's own inputs, a
// parser test of the legacy refusal messages. -exclude names path prefixes to
// leave alone (the defaults are printed by -h), and a comment carrying
// `memqlmigrate:keep` on a literal's line or the line above keeps that
// literal; `memqlmigrate:keep-file` anywhere keeps the whole file.
//
// -survey reports the literals of NON-test Go files that carry MemQL -- the
// code that renders .memql source -- and never writes.
//
// Usage:
//
//	go run ./scripts/migrations/expressions_go_fixtures [-w] [-v] [-dsl dsl] [-exclude p,...] [path...]
//	go run ./scripts/migrations/expressions_go_fixtures -survey [path...]
//
// Paths default to the current directory, which should be the repository
// root. Exit status: 0 done (a refusal is reported, not fatal), 1 an error
// reading, parsing or writing.
package main

import (
	"bytes"
	"errors"
	"flag"
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

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/core/repowalk"
)

// defaultExcludes are the test files whose literals are legacy ON PURPOSE:
// the expressions codemod's own inputs, and this tool's.
var defaultExcludes = []string{
	"cmd/memqlmigrate/",
	"component/language/parser/expressions_migrate_test.go",
	"scripts/migrations/expressions_go_fixtures/",
}

// Markers a test author writes to keep a literal, or a file, as it is.
const (
	keepMarker     = "memqlmigrate:keep"
	keepFileMarker = "memqlmigrate:keep-file"
)

var (
	// What makes a literal's text MemQL, read on a view whose comments and
	// string contents are blanked.
	dslHeader = regexp.MustCompile(`(?m)^[ \t]*(?:query|mutate|mutation|logic|automation|spec|trait)[ \t]+[A-Za-z_][A-Za-z0-9_]*(?:[ \t]+[A-Za-z_][A-Za-z0-9_]*)?[ \t]*(?:\{|=[^=>])`)
	// A filter CLAUSE, not a sentence that starts with the word: the line
	// carries an operator or a lambda arrow.
	filterLine = regexp.MustCompile(`(?m)^[ \t]*filter(?:[ \t]+|[ \t]*\()[^\n]*?(?:==|!=|<=|>=|=>|[<>]|\bin\b|\bstartsWith\b|&&|\|\|)`)
	atFilter   = regexp.MustCompile(`@filter[ \t]*\(`)

	// filterKeyword finds where a filter clause's expression starts.
	filterKeyword = regexp.MustCompile(`(?m)^[ \t]*filter(?:[ \t]+|[ \t]*\()`)

	// dslKeywordAny is isMemQL's cheap pre-test: a construct keyword at all.
	dslKeywordAny = regexp.MustCompile(`\b(?:query|mutate|mutation|logic|automation|spec|trait)\b`)

	// What is LEFT legacy after a rewrite that changed nothing.
	lambdaHead      = regexp.MustCompile(`^[ \t(]*(?:[A-Za-z_][A-Za-z0-9_]*|\([ \t]*[A-Za-z_][A-Za-z0-9_]*(?:[ \t]*,[ \t]*[A-Za-z_][A-Za-z0-9_]*)*[ \t]*\))[ \t]*=>`)
	predicateBody   = regexp.MustCompile(`(?m)^[ \t]*(?:spec[ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]+[A-Za-z_][A-Za-z0-9_]*|trait[ \t]+[A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)
	inProcessLegacy = regexp.MustCompile(`\b(?:cond|concat|coalesce|exists)[ \t]*\(`)
)

// literal is one Go string literal and its decoded text.
type literal struct {
	line       int
	start, end int // byte offsets of the literal in its file, quotes included
	raw        bool
	text       string
	kept       bool
}

// outcome is what the tool did with one MemQL literal.
type outcome int

const (
	unchanged outcome = iota
	changed
	refused
	fragment
	kept
)

type result struct {
	lit     literal
	outcome outcome
	newText string // the rewritten decoded text, for changed
	reason  string // for refused and fragment
}

// fileReport is one file's results.
type fileReport struct {
	path    string
	results []result
	out     []byte // the rewritten file, when any literal changed
}

func main() {
	write := flag.Bool("w", false, "rewrite files in place (default: report only)")
	flag.BoolVar(write, "write", false, "alias of -w")
	verbose := flag.Bool("v", false, "list every literal that would change, not only the refusals")
	dsl := flag.String("dsl", "dsl", "comma-separated .memql trees whose specs and traits form the predicate set")
	exclude := flag.String("exclude", strings.Join(defaultExcludes, ","), "comma-separated path prefixes to leave alone")
	survey := flag.Bool("survey", false, "report the MemQL-bearing literals of NON-test Go files; never writes")
	flag.Parse()

	paths := flag.Args()
	if len(paths) == 0 {
		paths = []string{"."}
	}
	if err := run(os.Stdout, paths, options{
		write:    *write && !*survey,
		verbose:  *verbose,
		dslRoots: splitList(*dsl),
		excludes: splitList(*exclude),
		survey:   *survey,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "expressions_go_fixtures:", err)
		os.Exit(1)
	}
}

type options struct {
	write    bool
	verbose  bool
	dslRoots []string
	excludes []string
	survey   bool
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, filepath.ToSlash(p))
		}
	}
	return out
}

// run is the whole tool, writing its report to w.
func run(w io.Writer, paths []string, o options) error {
	files, err := goFiles(paths, o)
	if err != nil {
		return err
	}
	if o.survey {
		return surveyFiles(w, files)
	}
	base, err := loadPredicates(o.dslRoots)
	if err != nil {
		return err
	}
	var reports []fileReport
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rep, err := processFile(path, src, base)
		if errors.Is(err, errNotGo) {
			// A file the Go parser cannot read holds no literal this tool can
			// place; say so and go on.
			fmt.Fprintf(w, "SKIP     %s: %v\n", path, err)
			continue
		}
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		reports = append(reports, rep)
	}
	printReport(w, reports, o.verbose, o.write)
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

// goFiles lists the Go files to scan: *_test.go, or -- for a survey -- every
// other .go file. Directories that are not source are skipped, and so is a
// path under an exclude prefix.
func goFiles(paths []string, o options) ([]string, error) {
	var out []string
	add := func(path string) {
		slash := filepath.ToSlash(filepath.Clean(path))
		for _, ex := range o.excludes {
			if strings.HasPrefix(slash, ex) || strings.HasPrefix(slash, "./"+ex) {
				return
			}
		}
		isTest := strings.HasSuffix(slash, "_test.go")
		if strings.HasSuffix(slash, ".go") && isTest != o.survey {
			out = append(out, path)
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
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				// The repository's one skip list (a worktree under .claude/
				// would otherwise be walked as a second copy of the tree), plus
				// testdata, whose Go files are fixtures of other tools.
				if repowalk.SkipDir(d.Name()) || d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			add(path)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(out)
	return out, nil
}

// predicateBase is the tree a fixture's MemQL loads over: its declarations,
// scanned once, and the predicate set they answer on their own.
type predicateBase struct {
	decls []langparser.PredicateDeclarations
	preds map[string]langparser.PredicateInfo
}

// loadPredicates scans every spec, trait, shape and concept the named trees
// declare.
func loadPredicates(roots []string) (predicateBase, error) {
	files := map[string][]byte{}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && repowalk.SkipDir(d.Name()) {
				return filepath.SkipDir
			}
			if d.IsDir() || filepath.Ext(path) != ".memql" {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files[path] = b
			return nil
		})
		if err != nil {
			return predicateBase{}, fmt.Errorf("read the predicate tree %s: %w", root, err)
		}
	}
	return newPredicateBase(files)
}

// newPredicateBase scans files, in path order, and resolves them.
func newPredicateBase(files map[string][]byte) (predicateBase, error) {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var b predicateBase
	for _, p := range paths {
		b.decls = append(b.decls, langparser.ScanPredicateDeclarations(p, files[p]))
	}
	preds, err := langparser.ResolvePredicates(b.decls)
	if err != nil {
		return predicateBase{}, err
	}
	b.preds = preds
	return b, nil
}

// processFile rewrites one test file's MemQL literals.
func processFile(path string, src []byte, base predicateBase) (fileReport, error) {
	rep := fileReport{path: path}
	lits, keepFile, err := stringLiterals(path, src)
	if err != nil {
		return rep, err
	}
	var dsl []literal
	for _, l := range lits {
		if isMemQL(l.text) {
			dsl = append(dsl, l)
		}
	}
	if len(dsl) == 0 {
		return rep, nil
	}
	if keepFile {
		for _, l := range dsl {
			rep.results = append(rep.results, result{lit: l, outcome: kept, reason: keepFileMarker})
		}
		return rep, nil
	}
	preds, conflict := filePredicates(path, base, lits)

	var edits []edit
	for _, l := range dsl {
		lp := preds
		if conflict != nil {
			// The file holds several fixtures that declare one name
			// differently, so no set answers for the whole file: each literal
			// answers for itself, over the tree.
			lp = literalPredicates(base, l)
		}
		res := rewriteLiteral(l, lp)
		if res.outcome == refused && conflict != nil {
			// Say why the file's other declarations were not used: the
			// refusal alone would point at the tree.
			res.reason += "; this file's own declarations conflict, so each literal was resolved on its own over the tree: " + firstOf(conflict)
		}
		rep.results = append(rep.results, res)
		if res.outcome == changed {
			edits = append(edits, edit{start: l.start, end: l.end, text: encode(l, res.newText)})
		}
	}
	if len(edits) == 0 {
		return rep, nil
	}
	out := applyEdits(src, edits)
	// The file must still be Go. A literal is replaced by a literal, so this
	// can only fail on an encoding bug -- and a file that no longer parses is
	// never written.
	if _, err := parser.ParseFile(token.NewFileSet(), path, out, parser.SkipObjectResolution); err != nil {
		return rep, fmt.Errorf("internal: the rewritten file does not parse as Go: %w", err)
	}
	rep.out = out
	return rep, nil
}

// errNotGo marks a file the Go parser cannot read.
var errNotGo = errors.New("does not parse as Go")

// stringLiterals returns every string literal in a Go file, decoded, and
// whether the file carries the keep-file marker.
func stringLiterals(path string, src []byte) ([]literal, bool, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", errNotGo, err)
	}
	keepLines := map[int]bool{}
	keepFile := false
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			if strings.Contains(c.Text, keepFileMarker) {
				keepFile = true
			} else if strings.Contains(c.Text, keepMarker) {
				keepLines[fset.Position(c.End()).Line] = true
			}
		}
	}
	var out []literal
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
		out = append(out, literal{
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

// isMemQL reports whether a literal's text carries struct-form MemQL.
func isMemQL(text string) bool {
	if !strings.Contains(text, "filter") && !dslKeywordAny.MatchString(text) {
		return false // the cheap test: most literals name no construct at all
	}
	mask := langparser.BlankCommentsAndStrings(text)
	return dslHeader.MatchString(mask) || filterLine.MatchString(mask) || atFilter.MatchString(mask)
}

// filePredicates is the corpus predicate set with the file's own literal
// declarations laid over it: a fixture that declares a trait and filters on it
// resolves its own. Every literal of the file is read for declarations, not
// only the ones the tool rewrites -- a fixture declares its @actor shape or its
// concept in a literal that carries nothing to rewrite, often split over a
// `+` whose first half holds the header. The file's declarations resolve
// through the corpus's shapes and concepts, so a spec over a binding the
// corpus declares -- the core @actor shape actorEnvelope -- takes its kind
// from there, and one over a binding nothing declares is refused by the
// rewrite, by name. A file whose declarations cannot be collected (two
// literals declaring one name differently -- two fixtures of one file, each
// with its own shape of one name) falls back to the corpus, and the rewrite
// refuses whatever that leaves unresolved; the conflict comes back for the
// report.
func filePredicates(path string, base predicateBase, lits []literal) (map[string]langparser.PredicateInfo, error) {
	local := make([]langparser.PredicateDeclarations, 0, len(lits))
	for _, l := range lits {
		// Named by the literal's Go line, so a conflict points at the file.
		local = append(local, langparser.ScanPredicateDeclarations(fmt.Sprintf("%s:%d", filepath.Base(path), l.line), []byte(l.text)))
	}
	preds, err := langparser.ResolvePredicatesOver(base.decls, local)
	if err != nil {
		return base.preds, err
	}
	return preds, nil
}

// literalPredicates is one literal's own declarations laid over the corpus:
// the answer for a literal of a file whose declarations conflict as a whole --
// independent snippets, one declaring `spec actorEnvelope foo` and another
// `trait foo`, each of which is consistent on its own.
func literalPredicates(base predicateBase, l literal) map[string]langparser.PredicateInfo {
	preds, err := langparser.ResolvePredicatesOver(base.decls, []langparser.PredicateDeclarations{langparser.ScanPredicateDeclarations("literal", []byte(l.text))})
	if err != nil {
		return base.preds
	}
	return preds
}

// rewriteLiteral runs the expressions rewrite over one literal's text.
func rewriteLiteral(l literal, preds map[string]langparser.PredicateInfo) result {
	if l.kept {
		return result{lit: l, outcome: kept, reason: keepMarker}
	}
	next, err := langparser.RewriteExpressions([]byte(l.text), preds)
	if err != nil {
		return result{lit: l, outcome: refused, reason: oneLine(err.Error())}
	}
	if string(next) != l.text {
		return result{lit: l, outcome: changed, newText: string(next)}
	}
	if why := legacyLeft(l.text); why != "" {
		return result{lit: l, outcome: fragment, reason: why}
	}
	return result{lit: l, outcome: unchanged}
}

// legacyLeft explains a legacy form a rewrite that changed nothing left in
// place, or returns "": the construct around it is not in this literal (a
// fixture assembled from several literals, or with fmt), so the rewrite could
// not see it.
func legacyLeft(text string) string {
	mask := langparser.BlankCommentsAndStrings(text)
	for _, loc := range filterKeyword.FindAllStringIndex(mask, -1) {
		if !filterLine.MatchString(mask[loc[0]:]) {
			continue
		}
		rest := strings.TrimLeft(text[loc[1]:], " \t(")
		if !lambdaHead.MatchString(rest) && !dslHeader.MatchString(mask) {
			return "a legacy filter clause with no query header in this literal (the construct is split across literals)"
		}
	}
	for _, loc := range atFilter.FindAllStringIndex(mask, -1) {
		if !lambdaHead.MatchString(text[loc[1]:]) {
			return "a legacy @filter the rewrite could not reach"
		}
	}
	if predicateBody.MatchString(mask) {
		return "a spec or trait body the rewrite could not reach"
	}
	if inProcessLegacy.MatchString(mask) && !dslHeader.MatchString(mask) {
		return "a cond / concat / coalesce / exists call with no logic, automation or mutation header in this literal"
	}
	return ""
}

// firstOf is the first of a joined error's lines, and how many follow.
func firstOf(err error) string {
	lines := strings.Split(err.Error(), "\n")
	if len(lines) == 1 {
		return oneLine(lines[0])
	}
	return fmt.Sprintf("%s (and %d more)", oneLine(lines[0]), len(lines)-1)
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// encode writes a rewritten text back in its literal's form. A raw literal
// stays raw unless the text now holds a backquote, which a raw literal cannot
// carry; an interpreted literal is re-quoted.
func encode(l literal, text string) string {
	if l.raw && !strings.Contains(text, "`") {
		return "`" + text + "`"
	}
	return strconv.Quote(text)
}

type edit struct {
	start, end int
	text       string
}

func applyEdits(src []byte, edits []edit) []byte {
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

// printReport prints the refusals and fragments always, the changes with -v,
// and a summary line.
func printReport(w io.Writer, reports []fileReport, verbose, wrote bool) {
	var nChanged, nFiles, nRefused, nFragment, nKept int
	for _, rep := range reports {
		fileChanged := false
		for _, r := range rep.results {
			at := fmt.Sprintf("%s:%d", rep.path, r.lit.line)
			switch r.outcome {
			case changed:
				nChanged++
				fileChanged = true
				if verbose {
					fmt.Fprintf(w, "CHANGE   %s\n", at)
				}
			case refused:
				nRefused++
				fmt.Fprintf(w, "REFUSED  %s: %s\n", at, r.reason)
			case fragment:
				nFragment++
				fmt.Fprintf(w, "FRAGMENT %s: %s\n", at, r.reason)
			case kept:
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

// surveyFiles lists the MemQL-bearing literals of non-test files -- the code
// that renders .memql source.
func surveyFiles(w io.Writer, files []string) error {
	n := 0
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lits, _, err := stringLiterals(path, src)
		if err != nil {
			fmt.Fprintf(w, "SKIP     %s: %v\n", path, err)
			continue
		}
		for _, l := range lits {
			if !isMemQL(l.text) {
				continue
			}
			n++
			first := strings.TrimSpace(strings.SplitN(strings.TrimSpace(l.text), "\n", 2)[0])
			if len(first) > 90 {
				first = first[:90] + "..."
			}
			fmt.Fprintf(w, "RENDERS  %s:%d: %s\n", path, l.line, first)
		}
	}
	fmt.Fprintf(w, "%d MemQL-bearing literal(s) in non-test Go files\n", n)
	return nil
}
