package parser

// expressions_migrate.go -- `memqlmigrate --rewrite=expressions` (epic
// memql#5363, task memql#5368): one .memql source moved onto the edition-2026
// expression forms. Which predicate a name means is a question about the whole
// tree, so the caller collects it first (CollectPredicates) and hands it in;
// cmd/memqlmigrate/expressions.go is that caller.
//
// Five positions change and nothing else does. Comments, `///` doc comments,
// string literals and every byte outside a rewritten span stay as written.
//
//	A. a struct-form query's `filter` clause  -> filter row => <v1>
//	B. a spec or trait body `{ return e }`    -> spec b n = row => <v1>
//	                                             (actor => ... over an @actor shape)
//	C. a trigger filter `@filter(e)`           -> @filter(row => <v1>)
//	D. inside logic, automation and mutation bodies:
//	     cond(p, a, b) -> p ? a : b      concat(a, b) -> a + b
//	     coalesce(a, b) -> a ?? b
//	     exists(x)     -> x != nil       null -> nil
//	     canonicalId(v, concept) -> canonicalId(v, "concept")
//	E. a query tool handler's `$args.x`       -> args.x
//
// A, B and C are PARSED, never spliced as text: the old clause is read by the
// legacy grammar (ParseExpression, which is what the engine loads it with
// today), converted node by node, and printed by ast.FormatExpr. A precedence
// the two grammars disagree about therefore cannot slip through as text -- the
// printer parenthesises whatever the new grammar would read differently. D and
// E are byte-level, in the null_coalesce_migrate.go style, because those
// positions are read today by the string evaluators this epic retires: there is
// no legacy tree for them to be converted from.
//
// A clause the rewrite cannot convert faithfully is REFUSED, never guessed: the
// file comes back byte-identical with an error naming the line and the clause.
//
// Unexported names here carry an `xm` prefix. The v1 parser is being built in
// this package on a sibling branch, and a helper named for what it does here
// (`boolForm`, `lambdaHeader`) is exactly the name that work would reach for.

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/dslclause"
)

// PredicateInfo is what the rewrite knows about a spec or trait by name.
type PredicateInfo struct {
	// Actor reports a spec bound to an @actor shape that is not also @row. Its
	// receiver is the actor envelope rather than a row, so a use is written
	// `name(actor)` and its own body's parameter is `actor`. Every other spec,
	// and every trait, is a row predicate. This mirrors the engine's own split
	// (shapeSpecKind in component/memql): a pure @actor shape evaluates in
	// process against the caller, a mixed shape compiles to SQL like a row.
	Actor bool
}

// xmWrapWidth is the width, measured from the `filter` keyword, past which a
// rewritten filter clause is broken at its top-level connectives. The lambda
// header and `row.` prefixes lengthen every clause; without a break the
// 65-query Shopify family alone would carry 150-column lines.
const xmWrapWidth = 110

var (
	xmIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

	xmSpecHeader  = regexp.MustCompile(`(?m)^[ \t]*spec[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)
	xmTraitHeader = regexp.MustCompile(`(?m)^[ \t]*trait[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)
	xmBodyHeader  = regexp.MustCompile(`(?m)^[ \t]*(?:logic|automation|mutate|mutation)[ \t]+[A-Za-z_][A-Za-z0-9_]*(?:[ \t]+[A-Za-z_][A-Za-z0-9_]*)?[ \t]*\{`)

	// The declarations CollectPredicates reads, in either edition: a legacy
	// body opens with `{`, a migrated one with `= <param> =>`.
	xmSpecDecl  = regexp.MustCompile(`(?m)^[ \t]*spec[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*(?:\{|=[^=>])`)
	xmTraitDecl = regexp.MustCompile(`(?m)^[ \t]*trait[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*(?:\{|=[^=>])`)
	xmShapeDecl = regexp.MustCompile(`(?m)^[ \t]*shape[ \t]+(?:[A-Za-z_][A-Za-z0-9_]*[ \t]+)?([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)

	xmAnnotation        = regexp.MustCompile(`@([A-Za-z_][A-Za-z0-9_]*)`)
	xmFilterAnnotation  = regexp.MustCompile(`@filter[ \t]*\(`)
	xmAutomationHeader  = regexp.MustCompile(`(?m)^[ \t]*automation[ \t]+[A-Za-z_][A-Za-z0-9_]*`)
	xmArgsField         = regexp.MustCompile(`(?m)^[ \t]*([A-Za-z_][A-Za-z0-9_]*)[ \t]+[A-Za-z\[]`)
	xmTriggerAnnotation = regexp.MustCompile(`@trigger[ \t]*\(`)
	xmHandlerAnnotation = regexp.MustCompile(`@handler[ \t]*\(`)

	// A query handler's placeholders, in the two spellings the old
	// substitution (component/memql/tool_execution.go) accepts: the whole
	// quoted slot `"$args.x"` -- written `\"$args.x\"` inside the handler's own
	// string -- and the bare `$args.x`. The key class is the substitution's
	// (argPlaceholderPattern), one identifier and no dots, so a trailing `.f`
	// stays text exactly as it did.
	xmQuotedArgsSlot = regexp.MustCompile(`\\"\$args\.([A-Za-z_][A-Za-z0-9_]*)\\"`)
	xmBareArgsSlot   = regexp.MustCompile(`\$args\.([A-Za-z_][A-Za-z0-9_]*)`)
)

// xmIntrinsics is the row-intrinsic set, keyed lower-case to its canonical
// spelling. It restates the engine's intrinsicFieldRegistry
// (component/memql/intrinsic_fields.go), which this package cannot import; the
// engine reads a bare filter field with one of these names as the INTRINSIC,
// not as a payload property (reservedFilterHead), and v1 reads `row.<name>` the
// same way, so both spellings migrate to `row.<canonical>`.
var xmIntrinsics = map[string]string{
	"id":         "id",
	"concept":    "concept",
	"type":       "type",
	"createdat":  "createdAt",
	"createdby":  "createdBy",
	"provenance": "provenance",
}

func xmIntrinsic(name string) (string, bool) {
	c, ok := xmIntrinsics[strings.ToLower(name)]
	return c, ok
}

// RewriteExpressions migrates one .memql source to the edition-2026 expression
// forms. preds holds every spec and trait declared anywhere in the tree -- the
// engine's predicate registry is flat, so a clause in one file names predicates
// declared in others -- and every spec this file declares must be in it.
//
// It is idempotent: a clause already in the v1 form is left alone, and a source
// with nothing to change comes back as the input slice itself. On any refusal
// the input comes back unchanged with an error naming each refused clause.
func RewriteExpressions(src []byte, preds map[string]PredicateInfo) ([]byte, error) {
	r := runExpressionRewrite(src, preds)
	if len(r.errs) > 0 {
		return src, errors.Join(r.errs...)
	}
	out, err := xmApply(r.f.src, r.edits)
	if err != nil {
		return src, err
	}
	if out == r.f.src {
		return src, nil
	}
	return []byte(out), nil
}

func runExpressionRewrite(src []byte, preds map[string]PredicateInfo) *xmRewrite {
	r := &xmRewrite{f: newXMFile(string(src)), preds: preds}
	r.filters()
	r.predicateBodies()
	r.triggerFilters()
	r.handlers()
	r.inProcessBodies()
	return r
}

// ExpressionEdit is one clause RewriteExpressions converts: the source bytes
// [Start, End) become Text.
type ExpressionEdit struct {
	Start, End int
	Text       string
}

// ExpressionRefusal is one clause RewriteExpressions will not convert: the
// source bytes [Start, End) the clause spans, and the error naming why, as
// RewriteExpressions reports it ("line N: ...").
type ExpressionRefusal struct {
	Start, End int
	Err        error
}

// ExpressionPlan is RewriteExpressions clause by clause, for an editor that
// applies the conversions it can and leaves a refused clause where it is.
// Applying every edit of a plan with no refusals is RewriteExpressions.
type ExpressionPlan struct {
	// Edits are the conversions, in source order; no two overlap.
	Edits []ExpressionEdit
	// Refused are the clauses the rewrite will not convert, in source order.
	Refused []ExpressionRefusal
}

// PlanExpressions is RewriteExpressions without the all-or-nothing: every
// clause it converts comes back as its own edit, every clause it refuses as a
// refusal with its span. Each edit is computed from src alone, so any subset
// of them applies cleanly -- but a clause's rewrite can name predicates it
// resolved through preds, exactly as RewriteExpressions' does. An error means
// the edits could not be kept apart, which is an internal fault and offers
// nothing.
func PlanExpressions(src []byte, preds map[string]PredicateInfo) (ExpressionPlan, error) {
	r := runExpressionRewrite(src, preds)
	var plan ExpressionPlan
	for _, e := range r.edits {
		plan.Edits = append(plan.Edits, ExpressionEdit{Start: e.start, End: e.end, Text: e.text})
	}
	sort.Slice(plan.Edits, func(i, j int) bool { return plan.Edits[i].Start < plan.Edits[j].Start })
	for i := 1; i < len(plan.Edits); i++ {
		if plan.Edits[i].Start < plan.Edits[i-1].End {
			return ExpressionPlan{}, fmt.Errorf("internal: two expression rewrites overlap at byte %d", plan.Edits[i].Start)
		}
	}
	plan.Refused = append(plan.Refused, r.refused...)
	sort.SliceStable(plan.Refused, func(i, j int) bool { return plan.Refused[i].Start < plan.Refused[j].Start })
	return plan, nil
}

// CollectPredicates reads every spec and trait declaration out of a set of
// sources, in either edition, together with the shapes the specs bind, and
// answers which of them is an actor predicate. A name declared twice with
// different answers is refused: the registry is flat, so no clause could say
// which of the two it means.
func CollectPredicates(files map[string][]byte) (map[string]PredicateInfo, error) {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	decls := make([]PredicateDeclarations, 0, len(paths))
	for _, p := range paths {
		decls = append(decls, ScanPredicateDeclarations(p, files[p]))
	}
	return ResolvePredicates(decls)
}

// PredicateDeclarations is what one source contributes to a predicate set: its
// top-level shape, spec and trait declarations. CollectPredicates is
// ScanPredicateDeclarations over every source followed by ResolvePredicates;
// the two are exported apart for a caller that keeps a tree's scans and
// rescans only the source that changed (the language server, as a file is
// typed in). Scanning is the expensive half.
type PredicateDeclarations struct {
	shapes []xmShapeDeclaration
	specs  []xmSpecDeclaration
	traits []xmTraitDeclaration
}

type xmShapeDeclaration struct {
	name string
	xmDecl
}

type xmSpecDeclaration struct{ name, bound, where string }

type xmTraitDeclaration struct{ name, where string }

// ScanPredicateDeclarations reads one source's declarations. path names the
// source in ResolvePredicates' errors.
func ScanPredicateDeclarations(path string, src []byte) PredicateDeclarations {
	var d PredicateDeclarations
	f := newXMFile(string(src))
	where := func(off int) string { return fmt.Sprintf("%s:%d", path, f.line(off)+1) }
	for _, m := range xmShapeDecl.FindAllStringSubmatchIndex(f.mask, -1) {
		if f.depthAt(m[0]) != 0 {
			continue
		}
		kinds := xmPreambleAnnotations(f, m[0])
		d.shapes = append(d.shapes, xmShapeDeclaration{name: f.src[m[2]:m[3]], xmDecl: xmDecl{where: where(m[0]), actor: kinds["actor"] && !kinds["row"]}})
	}
	for _, m := range xmSpecDecl.FindAllStringSubmatchIndex(f.mask, -1) {
		if f.depthAt(m[0]) != 0 {
			continue
		}
		d.specs = append(d.specs, xmSpecDeclaration{name: f.src[m[4]:m[5]], bound: f.src[m[2]:m[3]], where: where(m[0])})
	}
	for _, m := range xmTraitDecl.FindAllStringSubmatchIndex(f.mask, -1) {
		if f.depthAt(m[0]) != 0 {
			continue
		}
		d.traits = append(d.traits, xmTraitDeclaration{name: f.src[m[2]:m[3]], where: where(m[0])})
	}
	return d
}

// ResolvePredicates answers CollectPredicates' question over scanned sources.
// Pass them in the order CollectPredicates reads them -- sorted by path -- for
// the same answer: where one name is declared twice, the first declaration is
// the one the others are compared against.
func ResolvePredicates(decls []PredicateDeclarations) (map[string]PredicateInfo, error) {
	shapes := map[string][]xmDecl{}
	byName := map[string][]xmDecl{}
	var specs []xmSpecDeclaration
	for _, d := range decls {
		for _, s := range d.shapes {
			shapes[s.name] = append(shapes[s.name], s.xmDecl)
		}
		specs = append(specs, d.specs...)
		for _, t := range d.traits {
			byName[t.name] = append(byName[t.name], xmDecl{where: t.where})
		}
	}

	var errs []error
	for _, s := range specs {
		// A shape binding wins over a concept of the same name, as it does in
		// the engine (resolveOneSpecBinding looks the shape up first). A name
		// that is no shape binds a concept, and a concept is a row.
		actor := false
		if ds := shapes[s.bound]; len(ds) > 0 {
			actor = ds[0].actor
			for _, d := range ds[1:] {
				if d.actor != actor {
					errs = append(errs, fmt.Errorf("spec %s (%s) binds shape %s, which is declared as an @actor shape at one place and a row shape at another (%s, %s)",
						s.name, s.where, s.bound, ds[0].where, d.where))
					break
				}
			}
		}
		byName[s.name] = append(byName[s.name], xmDecl{where: s.where, actor: actor})
	}

	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make(map[string]PredicateInfo, len(byName))
	for _, n := range names {
		ds := byName[n]
		out[n] = PredicateInfo{Actor: ds[0].actor}
		for _, d := range ds[1:] {
			if d.actor != ds[0].actor {
				actorAt, rowAt := ds[0].where, d.where
				if d.actor {
					actorAt, rowAt = d.where, ds[0].where
				}
				errs = append(errs, fmt.Errorf("predicate %s is an actor predicate at %s and a row predicate at %s; the registry is flat, so no clause can say which one it means",
					n, actorAt, rowAt))
				break
			}
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return out, nil
}

// xmDecl is one declaration CollectPredicates found: where, and whether it is
// an actor predicate (for a shape: an @actor shape that is not also @row).
type xmDecl struct {
	where string
	actor bool
}

// xmPreambleAnnotations returns the annotation names above a declaration,
// read on the masked view so an `@actor` written in a doc comment or a string
// does not count.
func xmPreambleAnnotations(f *xmFile, headerStart int) map[string]bool {
	start := PreambleStartOf(f.src, headerStart)
	out := map[string]bool{}
	for _, m := range xmAnnotation.FindAllStringSubmatch(f.mask[start:headerStart], -1) {
		out[m[1]] = true
	}
	return out
}

// ----------------------------------------------------------------------------
// The file and its views
// ----------------------------------------------------------------------------

// xmFile is one source in the three views the scanners need. All three have
// the same length and the same newlines, so an offset found in one indexes the
// others.
type xmFile struct {
	src        string // as written
	code       string // comments blanked, strings intact: what a clause's text is read from
	mask       string // comments and string contents blanked: what structure is found on
	lineStarts []int
	lineDepth  []int // brace depth at the start of each line, counted on mask
}

func newXMFile(src string) *xmFile {
	f := &xmFile{src: src, code: BlankComments(src), mask: blankCommentsAndStrings(src)}
	f.lineStarts = []int{0}
	f.lineDepth = []int{0}
	depth := 0
	for i := 0; i < len(f.mask); i++ {
		switch f.mask[i] {
		case '{':
			depth++
		case '}':
			depth--
		case '\n':
			f.lineStarts = append(f.lineStarts, i+1)
			f.lineDepth = append(f.lineDepth, depth)
		}
	}
	return f
}

// line returns the 0-based line holding offset pos.
func (f *xmFile) line(pos int) int {
	return sort.Search(len(f.lineStarts), func(i int) bool { return f.lineStarts[i] > pos }) - 1
}

// depthAt returns the brace depth immediately before pos.
func (f *xmFile) depthAt(pos int) int {
	l := f.line(pos)
	d := f.lineDepth[l]
	for i := f.lineStarts[l]; i < pos; i++ {
		switch f.mask[i] {
		case '{':
			d++
		case '}':
			d--
		}
	}
	return d
}

// indentAt returns the leading whitespace of the line holding pos.
func (f *xmFile) indentAt(pos int) string {
	start := f.lineStarts[f.line(pos)]
	end := start
	for end < len(f.src) && (f.src[end] == ' ' || f.src[end] == '\t') {
		end++
	}
	return f.src[start:end]
}

// column returns pos's byte offset from the start of its line.
func (f *xmFile) column(pos int) int { return pos - f.lineStarts[f.line(pos)] }

// hasComment reports whether src[lo:hi] holds any comment text.
func (f *xmFile) hasComment(lo, hi int) bool { return f.src[lo:hi] != f.code[lo:hi] }

// ----------------------------------------------------------------------------
// The rewrite
// ----------------------------------------------------------------------------

type xmEdit struct {
	start, end int
	text       string
}

type xmRewrite struct {
	f       *xmFile
	preds   map[string]PredicateInfo
	edits   []xmEdit
	errs    []error
	refused []ExpressionRefusal
}

func (r *xmRewrite) edit(start, end int, text string) {
	r.edits = append(r.edits, xmEdit{start: start, end: end, text: text})
}

// fail refuses the clause spanning [lo, hi), naming the line of pos.
func (r *xmRewrite) fail(lo, hi, pos int, format string, args ...any) {
	err := fmt.Errorf("line %d: %s", r.f.line(pos)+1, fmt.Sprintf(format, args...))
	r.errs = append(r.errs, err)
	r.refused = append(r.refused, ExpressionRefusal{Start: lo, End: hi, Err: err})
}

// xmApply splices non-overlapping edits into src.
func xmApply(src string, edits []xmEdit) (string, error) {
	if len(edits) == 0 {
		return src, nil
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	var b strings.Builder
	prev := 0
	for _, e := range edits {
		if e.start < prev {
			return "", fmt.Errorf("internal: two expression rewrites overlap at byte %d", e.start)
		}
		b.WriteString(src[prev:e.start])
		b.WriteString(e.text)
		prev = e.end
	}
	b.WriteString(src[prev:])
	return b.String(), nil
}

// --- A. query filters -------------------------------------------------------

type xmLine struct {
	start int // absolute offset of the line's first byte
	text  string
}

func (r *xmRewrite) filters() {
	f := r.f
	for _, h := range queryStructHeader.FindAllStringSubmatchIndex(f.mask, -1) {
		if f.depthAt(h[0]) != 0 {
			continue
		}
		open := h[1] - 1
		end := MatchingCloseBrace(f.code, open)
		if end < 0 {
			continue // unbalanced: the load refuses the construct; nothing to migrate
		}
		bound := ""
		if h[2] >= 0 {
			bound = f.src[h[2]:h[3]]
		}
		r.queryFilters(f.src[h[4]:h[5]], bound, open+1, end)
	}
}

// queryFilters finds the filter clause of one query body [lo, hi) the way
// parseStructQueryBody does -- args block cut out, physical lines folded into
// clauses by joinStructQueryContinuations' three rules -- but keeping each
// line's offset so the clause can be spliced back where it was.
func (r *xmRewrite) queryFilters(query, bound string, lo, hi int) {
	f := r.f
	view := []byte(f.code[lo:hi])
	// The args block holds declarations, not clauses: blank it, so an argument
	// NAMED filter is never read as the clause.
	if loc := argsBlockHeader.FindStringIndex(f.mask[lo:hi]); loc != nil {
		brace := lo + loc[0] + strings.LastIndexByte(f.mask[lo+loc[0]:lo+loc[1]], '{')
		if end := MatchingCloseBrace(f.code, brace); end >= 0 {
			for i := loc[0]; i <= end-lo && i < len(view); i++ {
				if view[i] != '\n' {
					view[i] = ' '
				}
			}
		}
	}
	body := string(view)

	var clause []xmLine
	acc := ""
	flush := func() {
		if len(clause) > 0 && xmOpensFilter(strings.TrimSpace(clause[0].text)) {
			r.filterClause(query, bound, clause)
		}
		clause, acc = nil, ""
	}
	for start := 0; start <= len(body); {
		end := len(body)
		if nl := strings.IndexByte(body[start:], '\n'); nl >= 0 {
			end = start + nl
		}
		text := body[start:end]
		if t := strings.TrimSpace(text); t != "" {
			if acc != "" && dslclause.ContinuesClause(acc, t) {
				acc += " " + t
				clause = append(clause, xmLine{start: lo + start, text: text})
			} else {
				flush()
				acc = t
				clause = []xmLine{{start: lo + start, text: text}}
			}
		}
		if end == len(body) {
			break
		}
		start = end + 1
	}
	flush()
}

// xmOpensFilter reports whether a trimmed clause line is the filter clause.
// The engine accepts `filter(...)` as well as `filter <expr>`.
func xmOpensFilter(t string) bool {
	rest, ok := strings.CutPrefix(t, "filter")
	if !ok || rest == "" {
		return false
	}
	c, _ := utf8.DecodeRuneInString(rest)
	return unicode.IsSpace(c) || c == '('
}

func (r *xmRewrite) filterClause(query, bound string, lines []xmLine) {
	f := r.f
	first, last := lines[0], lines[len(lines)-1]
	kw := first.start + len(first.text) - len(strings.TrimLeftFunc(first.text, unicode.IsSpace))
	exprStart := kw + len("filter")
	for exprStart < len(f.code) && (f.code[exprStart] == ' ' || f.code[exprStart] == '\t') {
		exprStart++
	}
	gap := f.src[kw+len("filter") : exprStart]
	exprEnd := last.start + len(strings.TrimRightFunc(last.text, unicode.IsSpace))

	// The clause exactly as the engine reads it: the lines joined the way
	// joinStructQueryContinuations joins them, the keyword cut off.
	parts := make([]string, len(lines))
	for i, l := range lines {
		parts[i] = strings.TrimSpace(l.text)
	}
	clause := strings.TrimSpace(strings.TrimPrefix(strings.Join(parts, " "), "filter"))
	if clause == "" || dslclause.OpensLambda(clause) {
		return // already edition 2026: this is what makes a second run a no-op
	}
	if f.hasComment(exprStart, exprEnd) {
		r.fail(kw, exprEnd, kw, "query %s: filter %q: a comment inside the clause would be lost; move it above the clause and rerun", query, clause)
		return
	}
	expr, err := xmConvertChecked(clause, xmConverter{mode: xmFilter, param: "row", preds: r.preds, bound: bound})
	if err != nil {
		r.fail(kw, exprEnd, kw, "query %s: filter %q: %v", query, clause, err)
		return
	}
	head := "row => "
	if gap == "" {
		head = " " + head // `filter(...)`: give the lambda its own word
	}
	printed := ast.FormatExpr(expr)
	tooWide := len("filter")+len(gap)+len(head)+len(printed) > xmWrapWidth
	text := xmWrap(printed, expr, f.column(exprStart)+len(head), f.indentAt(kw), len(lines) > 1, tooWide)
	r.edit(exprStart, exprEnd, head+text)
}

// --- B. spec and trait bodies -----------------------------------------------

func (r *xmRewrite) predicateBodies() {
	f := r.f
	for _, h := range xmSpecHeader.FindAllStringSubmatchIndex(f.mask, -1) {
		if f.depthAt(h[0]) != 0 {
			continue
		}
		name := f.src[h[4]:h[5]]
		info, ok := r.preds[name]
		if !ok {
			hi := h[1]
			if end := MatchingCloseBrace(f.code, h[1]-1); end >= 0 {
				hi = end + 1
			}
			r.fail(h[0], hi, h[0], "spec %s is not in the predicate set; collect every spec and trait in the tree (CollectPredicates) before rewriting a file", name)
			continue
		}
		param := "row"
		if info.Actor {
			param = "actor"
		}
		r.predicateBody("spec", name, param, h[0], h[5], h[1]-1)
	}
	for _, h := range xmTraitHeader.FindAllStringSubmatchIndex(f.mask, -1) {
		if f.depthAt(h[0]) != 0 {
			continue
		}
		r.predicateBody("trait", f.src[h[2]:h[3]], "row", h[0], h[3], h[1]-1)
	}
}

// predicateBody converts the declaration whose header starts at hdr, whose
// name ends at nameEnd and whose body opens at open.
func (r *xmRewrite) predicateBody(kind, name, param string, hdr, nameEnd, open int) {
	f := r.f
	end := MatchingCloseBrace(f.code, open)
	if end < 0 {
		return
	}
	body := strings.TrimSpace(f.code[open+1 : end])
	rest, ok := strings.CutPrefix(body, "return")
	if !ok || rest == "" || !unicode.IsSpace(rune(rest[0])) {
		r.fail(hdr, end+1, open, "%s %s: the body is not `return <expression>`, the one body form the rewrite converts", kind, name)
		return
	}
	if f.hasComment(open+1, end) {
		r.fail(hdr, end+1, open, "%s %s: a comment inside the body would be lost; move it above the declaration and rerun", kind, name)
		return
	}
	exprText := strings.TrimSpace(rest)
	expr, err := xmConvertChecked(exprText, xmConverter{mode: xmSpecBody, param: param, preds: r.preds})
	if err != nil {
		r.fail(hdr, end+1, open, "%s %s: return %q: %v", kind, name, exprText, err)
		return
	}
	head := " = " + param + " => "
	printed := ast.FormatExpr(expr)
	text := xmWrap(printed, expr, f.column(nameEnd)+len(head), f.indentAt(nameEnd), strings.Contains(exprText, "\n"), false)
	r.edit(nameEnd, end+1, head+text)
}

// --- C. trigger filters -----------------------------------------------------

func (r *xmRewrite) triggerFilters() {
	f := r.f
	for _, m := range xmFilterAnnotation.FindAllStringIndex(f.mask, -1) {
		if f.depthAt(m[0]) != 0 {
			continue
		}
		open := m[1] - 1
		end := xmMatchParen(f.mask, open)
		if end < 0 {
			continue
		}
		r.triggerFilter(m[0], open, end)
	}
	// `@trigger(..., filter="...")` carries its filter as a string argument.
	// No tree carries one; moving it to its own annotation is a structural
	// edit this rewrite does not guess at, so it is refused by name.
	for _, m := range xmTriggerAnnotation.FindAllStringIndex(f.mask, -1) {
		if f.depthAt(m[0]) != 0 {
			continue
		}
		open := m[1] - 1
		end := xmMatchParen(f.mask, open)
		if end >= 0 && xmKeywordAt(f.mask[open+1:end], "filter") >= 0 {
			r.fail(m[0], end+1, m[0], "@trigger(..., filter=...) carries its filter as a string argument; move it to its own @filter(...) annotation and rerun")
		}
	}
}

// triggerFilter converts the @filter annotation starting at at, whose
// parentheses are [open, end].
func (r *xmRewrite) triggerFilter(at, open, end int) {
	f := r.f
	inner := strings.TrimSpace(f.code[open+1 : end])
	if inner == "" || dslclause.OpensLambda(inner) {
		return
	}
	if f.hasComment(open+1, end) {
		r.fail(at, end+1, open, "@filter(%s): a comment inside the filter would be lost; move it above the annotation and rerun", inner)
		return
	}
	exprText := inner
	if strings.HasPrefix(inner, `"`) {
		// The quoted form @filter("<expr>") holds the same expression.
		toks, err := NewLexer(inner).Tokenize()
		if err != nil || len(toks) != 2 || toks[0].Type != TokenString {
			r.fail(at, end+1, open, "@filter(%s): a quoted filter must be exactly one string literal", inner)
			return
		}
		exprText = toks[0].Literal
	}
	expr, err := xmConvertChecked(exprText, xmConverter{mode: xmTrigger, param: "row", preds: r.preds, argsFields: xmAutomationArgsAfter(f, end)})
	if err != nil {
		r.fail(at, end+1, open, "@filter(%s): %v", inner, err)
		return
	}
	head := "row => "
	printed := ast.FormatExpr(expr)
	text := xmWrap(printed, expr, f.column(open+1)+len(head), f.indentAt(open), strings.Contains(inner, "\n"), false)
	r.edit(open+1, end, head+text)
}

// --- E. query tool handlers -------------------------------------------------

func (r *xmRewrite) handlers() {
	f := r.f
	for _, m := range xmHandlerAnnotation.FindAllStringIndex(f.mask, -1) {
		if f.depthAt(m[0]) != 0 {
			continue
		}
		open := m[1] - 1
		end := xmMatchParen(f.mask, open)
		if end < 0 {
			continue
		}
		ts, te, ok := xmKeywordString(f, open+1, end, "type")
		if !ok || f.src[ts:te] != "query" {
			continue
		}
		qs, qe, ok := xmKeywordString(f, open+1, end, "query")
		if !ok {
			continue
		}
		content := f.src[qs:qe]
		next := xmQuotedArgsSlot.ReplaceAllString(content, "args.$1")
		// A bare placeholder INSIDE a quoted string of the query was
		// interpolated into that string by the old substitution. A v1 string
		// does not interpolate, so writing `args.x` there would silently
		// become literal text: refuse instead.
		if at := xmPlaceholderInsideString(next); at >= 0 {
			r.fail(m[0], end+1, m[0], "@handler query %q: the placeholder at %q sits inside a string literal, which v1 does not interpolate; rewrite it with + by hand", content, next[at:min(at+24, len(next))])
			continue
		}
		next = xmBareArgsSlot.ReplaceAllString(next, "args.$1")
		if next != content {
			r.edit(qs, qe, next)
		}
	}
}

// xmKeywordString finds `key = "..."` inside an annotation's argument list
// [lo, hi) and returns the literal's content span.
func xmKeywordString(f *xmFile, lo, hi int, key string) (int, int, bool) {
	at := xmKeywordAt(f.mask[lo:hi], key)
	if at < 0 {
		return 0, 0, false
	}
	i := lo + at + len(key)
	for i < hi && (f.mask[i] == ' ' || f.mask[i] == '\t') {
		i++
	}
	if i >= hi || f.mask[i] != '=' {
		return 0, 0, false
	}
	i++
	for i < hi && (f.mask[i] == ' ' || f.mask[i] == '\t') {
		i++
	}
	if i >= hi || f.mask[i] != '"' {
		return 0, 0, false
	}
	close := strings.IndexByte(f.mask[i+1:hi], '"')
	if close < 0 {
		return 0, 0, false
	}
	return i + 1, i + 1 + close, true
}

// xmKeywordAt returns the offset of the word key followed by `=` in s, or -1.
func xmKeywordAt(s, key string) int {
	for from := 0; ; {
		i := strings.Index(s[from:], key)
		if i < 0 {
			return -1
		}
		i += from
		from = i + 1
		if i > 0 && isIdentByte(s[i-1]) {
			continue
		}
		j := i + len(key)
		for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
			j++
		}
		if j < len(s) && s[j] == '=' && (j+1 >= len(s) || s[j+1] != '=') {
			return i
		}
	}
}

// xmPlaceholderInsideString returns the offset of the first bare `$args.`
// placeholder that sits inside a string of the handler query, or -1. The
// query's own strings are delimited by escaped quotes, because the query is
// itself the content of a string literal.
func xmPlaceholderInsideString(raw string) int {
	inner := false
	for i := 0; i < len(raw); i++ {
		switch {
		case raw[i] == '\\' && i+1 < len(raw) && raw[i+1] == '"':
			inner = !inner
			i++
		case raw[i] == '\\':
			i++
		case inner && strings.HasPrefix(raw[i:], "$args."):
			return i
		}
	}
	return -1
}

// --- D. in-process bodies ---------------------------------------------------

func (r *xmRewrite) inProcessBodies() {
	f := r.f
	for _, h := range xmBodyHeader.FindAllStringIndex(f.mask, -1) {
		if f.depthAt(h[0]) != 0 {
			continue
		}
		end := MatchingCloseBrace(f.code, h[1]-1)
		if end < 0 {
			continue
		}
		start := h[0] + len(f.src[h[0]:h[1]]) - len(strings.TrimLeft(f.src[h[0]:h[1]], " \t"))
		seg := f.src[start : end+1]
		out, err := xmRewriteInProcess(seg)
		if err != nil {
			var pe *xmPosError
			if errors.As(err, &pe) {
				r.fail(start, end+1, start+pe.pos, "%s", pe.msg)
			} else {
				r.fail(start, end+1, start, "%v", err)
			}
			continue
		}
		if out != seg {
			r.edit(start, end+1, out)
		}
	}
}

// ----------------------------------------------------------------------------
// Converting a legacy expression
// ----------------------------------------------------------------------------

type xmMode int

const (
	xmFilter   xmMode = iota // a query filter clause
	xmSpecBody               // a spec or trait body
	xmTrigger                // a trigger @filter
)

type xmConverter struct {
	mode  xmMode
	param string // the lambda parameter: row, or actor for a spec over an @actor shape
	preds map[string]PredicateInfo
	// bare holds, in trigger mode, the words the clause wrote WITHOUT quotes.
	// The legacy grammar hands a comparison a bare word and a quoted string as
	// the same Go string. The engine's SQL path reads both as text, but the
	// trigger evaluator resolves a bare dotted word as a path, so the two modes
	// need different answers for one value -- and only the tokens know which
	// spelling the author used.
	bare map[string]bool
	// argsFields holds, in trigger mode, the fields the automation's args
	// block declares: a bare one resolves to the bound argument (G2,
	// memql#2364) in both grammars, so it keeps its bare name.
	argsFields map[string]bool
	// bound is, in filter mode, the concept the query's signature binds
	// (`query <bound> <name>`): the legacy `<bound>.<field>` spelling is a
	// field of that row.
	bound string
}

// xmConvertChecked converts src and verifies the result: the conversion is run
// twice from two independent parses and must print identically (a map walked
// in iteration order is exactly the bug that would not), and every node it
// produced must be an edition-2026 node the printer can spell.
func xmConvertChecked(src string, c xmConverter) (ast.ExpressionNode, error) {
	first, err := c.convert(src)
	if err != nil {
		return nil, err
	}
	second, err := c.convert(src)
	if err != nil {
		return nil, err
	}
	if a, b := ast.FormatExpr(first), ast.FormatExpr(second); a != b {
		return nil, fmt.Errorf("internal: the conversion is not deterministic (%s, then %s)", a, b)
	}
	if err := xmCheckV1(first); err != nil {
		return nil, err
	}
	return first, nil
}

func (c xmConverter) convert(src string) (ast.ExpressionNode, error) {
	legacy, err := ParseExpression(src)
	if err != nil {
		return nil, fmt.Errorf("the legacy grammar does not read it: %w", err)
	}
	if legacy == nil {
		return nil, errors.New("the clause is empty")
	}
	if c.mode == xmTrigger {
		c.bare = xmBareWords(src)
	}
	form, err := c.boolean(legacy)
	if err != nil {
		return nil, err
	}
	// A guard that IS the whole clause needs no parentheses of its own.
	return ast.Unparen(form.t), nil
}

// xmForm is one legacy boolean operand in the spellings its position needs.
//
// The legacy `when(args.x) { e }` guard is not a boolean: when args.x is
// absent (or null) the engine DELETES the guard and its connective before the
// query runs (expandExpressionWithArgs), so `A && G` becomes `A`, `A || G`
// becomes `A`, and a clause that is nothing but dropped guards filters nothing.
// A v1 expression has no deletion, only values, so a guard becomes the value
// that its connective ignores:
//
//	under && (and at the top):  (args.x == nil || e)    -- true when dropped
//	under ||:                   (args.x != nil && e)    -- false when dropped
//
// t is the first spelling and f the second. The rule is local, and it is exact
// only while no connective can lose EVERY operand: `G1 || G2` with both
// dropped is deleted whole by the engine -- which at the top means "no
// constraint" -- yet the local spellings would give false || false. d and nd
// carry, for an operand that can be dropped, the args-only condition under
// which it is and that condition's negation, so a connective whose every
// operand can be dropped adds the one term the local rule is missing. No
// clause in dsl/ reaches that case; dsl/observability/queries.memql's guard
// under || sits beside an ordinary conjunct, which is the local rule's case.
type xmForm struct {
	t, f  ast.ExpressionNode
	d, nd ast.ExpressionNode // nil when the operand is never dropped
}

func xmLeaf(n ast.ExpressionNode) xmForm { return xmForm{t: n, f: n} }

func xmGuard(arg ast.ExpressionNode, inner xmForm) xmForm {
	absent := xmBin("==", arg, &ast.NilExpr{})
	present := xmBin("!=", arg, &ast.NilExpr{})
	g := xmForm{
		t:  &ast.ParenExpr{Inner: xmBin("||", absent, inner.t)},
		f:  &ast.ParenExpr{Inner: xmBin("&&", present, inner.f)},
		d:  absent,
		nd: present,
	}
	if inner.d != nil {
		// A guard nested in a guard: dropped when either argument is absent.
		g.d = xmBin("||", absent, inner.d)
		g.nd = xmBin("&&", present, inner.nd)
	}
	return g
}

func xmAnd(l, r xmForm) xmForm {
	out := xmForm{t: xmBin("&&", l.t, r.t)}
	out.f = out.t
	if l.d != nil && r.d != nil {
		// Both operands may vanish, and then the engine deletes the whole
		// conjunction; under || that must read false, not the true the two
		// dropped t-forms would give.
		out.d = xmBin("&&", l.d, r.d)
		out.nd = xmBin("||", l.nd, r.nd)
		out.f = xmBin("&&", out.t, out.nd)
	}
	return out
}

func xmOr(l, r xmForm) xmForm {
	out := xmForm{f: xmBin("||", l.f, r.f)}
	out.t = out.f
	if l.d != nil && r.d != nil {
		// Both operands may vanish, and then the engine deletes the whole
		// disjunction; under && or at the top that must read true.
		out.d = xmBin("&&", l.d, r.d)
		out.nd = xmBin("||", l.nd, r.nd)
		out.t = xmBin("||", xmBin("||", out.d, l.f), r.f)
	}
	return out
}

func (c xmConverter) boolean(n ast.ExpressionNode) (xmForm, error) {
	switch e := n.(type) {
	case *ast.LogicalExpr:
		l, err := c.boolean(e.Left)
		if err != nil {
			return xmForm{}, err
		}
		r, err := c.boolean(e.Right)
		if err != nil {
			return xmForm{}, err
		}
		switch e.Op {
		case ast.LogicalAnd:
			return xmAnd(l, r), nil
		case ast.LogicalOr:
			return xmOr(l, r), nil
		}
		return xmForm{}, fmt.Errorf("unknown connective %q", e.Op)
	case *ast.ConditionalFilterExpr:
		if c.mode != xmFilter {
			return xmForm{}, errors.New("a when(args.<field>) guard has no meaning outside a query filter")
		}
		if e.ArgPath == "" {
			return xmForm{}, errors.New("a ?. conditional filter whose value is not an args.<field> reference names no argument to guard on")
		}
		inner, err := c.boolean(e.Filter)
		if err != nil {
			return xmForm{}, err
		}
		arg, err := c.argRef(e.ArgPath)
		if err != nil {
			return xmForm{}, err
		}
		return xmGuard(arg, inner), nil
	}
	leaf, err := c.leaf(n)
	if err != nil {
		return xmForm{}, err
	}
	return xmLeaf(leaf), nil
}

func (c xmConverter) leaf(n ast.ExpressionNode) (ast.ExpressionNode, error) {
	switch e := n.(type) {
	case *ast.ComparisonExpr:
		return c.comparison(e)
	case *ast.SpecReferenceExpr:
		if c.mode == xmTrigger {
			// The trigger evaluator reads a bare word as a path to a value.
			return c.path(strings.Split(e.Name, "."))
		}
		return c.predicate(e.Name)
	case *ast.NotExpr:
		// The legacy filter and spec surfaces refuse `!` outright, so such a
		// clause never loaded; its intent is still unambiguous, and edition
		// 2026 gives `!` a meaning, so it is written as the negation. A
		// negated when-guard is the exception: the engine never defined what
		// negating a deleted operand means.
		inner, err := c.boolean(e.Target)
		if err != nil {
			return nil, err
		}
		if inner.d != nil {
			return nil, errors.New("`!` over a when(args.<field>) guard has no meaning to preserve: the engine deletes a guard whose argument is absent, and there is no negation of a deletion")
		}
		return &ast.UnaryExpr{Op: "!", Operand: inner.t}, nil
	case *ast.BinaryComparisonExpr:
		return nil, errors.New("an expression-led comparison (an arithmetic, literal or call on the left) is refused by the legacy filter surface, so this clause never loaded")
	case *ast.RelationshipExpr:
		return nil, fmt.Errorf("the %s(...) traversal takes a lambda in edition 2026; migrate it by hand", e.Function)
	}
	return nil, fmt.Errorf("%T has no edition-2026 form this rewrite writes", n)
}

// predicate converts a bare predicate conjunct into its application.
func (c xmConverter) predicate(name string) (ast.ExpressionNode, error) {
	info, ok := c.preds[name]
	if !ok {
		return nil, fmt.Errorf("%q is neither a spec nor a trait declared in the tree: the legacy grammar reads a bare name as a predicate, so there is nothing this could be (a boolean field is written row.%s == true)", name, name)
	}
	arg := "row"
	if info.Actor {
		arg = "actor"
	}
	if c.mode == xmSpecBody && arg != c.param {
		return nil, fmt.Errorf("%s is %s predicate and this body's parameter is %s; the engine refuses a spec that mixes the two", name, map[bool]string{true: "an actor", false: "a row"}[info.Actor], c.param)
	}
	return &ast.CallExpr{Name: name, Args: []ast.ExpressionNode{&ast.IdentExpr{Name: arg}}}, nil
}

func (c xmConverter) comparison(e *ast.ComparisonExpr) (ast.ExpressionNode, error) {
	if e.Field.Wildcard || e.CacheHintSeconds != nil || len(e.FieldSelections) > 0 {
		return nil, fmt.Errorf("the comparison on %s carries a wildcard, cache hint or field selection, which no clause position writes", e.Field.Raw)
	}
	left, err := c.field(e.Field.Parts)
	if err != nil {
		return nil, err
	}
	switch e.Operator {
	case ast.OpMissing:
		return xmBin("==", left, &ast.NilExpr{}), nil
	case ast.OpNotMissing:
		return xmBin("!=", left, &ast.NilExpr{}), nil
	}
	val, err := c.value(e.Value)
	if err != nil {
		return nil, err
	}
	switch e.Operator {
	case ast.OpEq, ast.OpNe, ast.OpLt, ast.OpLe, ast.OpGt, ast.OpGe:
		return xmBin(string(e.Operator), left, val), nil
	case ast.OpIn:
		return xmBin("in", left, val), nil
	case ast.OpHas:
		// The legacy parser folds `<scalar> in <collection>` into
		// `<collection> has <scalar>`; v1 writes it the way the author did.
		return xmBin("in", val, left), nil
	case ast.OpOut:
		return &ast.UnaryExpr{Op: "!", Operand: xmBin("in", left, val)}, nil
	case ast.OpStartsWith:
		return xmBin("startsWith", left, val), nil
	}
	return nil, fmt.Errorf("the operator %q has no edition-2026 form", e.Operator)
}

// field converts a comparison's left-hand path.
func (c xmConverter) field(parts []string) (ast.ExpressionNode, error) {
	for _, p := range parts {
		if !xmIdent.MatchString(p) {
			return nil, fmt.Errorf("the path %s has a segment %q that is not an identifier", strings.Join(parts, "."), p)
		}
	}
	head := parts[0]
	switch c.mode {
	case xmTrigger:
		return c.triggerPath(parts), nil
	case xmSpecBody:
		if len(parts) != 1 {
			return nil, fmt.Errorf("%s: a spec or trait body reads its bound fields by bare name, and the engine refuses a dotted path there", strings.Join(parts, "."))
		}
		if xmReservedHead(head) || head == "row" || head == "args" || head == "actor" || head == "payload" {
			return nil, fmt.Errorf("%s is a reserved name, not a bound field", head)
		}
		if canon, ok := xmIntrinsic(head); ok && c.param == "row" {
			return xmPath("row", canon), nil
		}
		return xmPath(c.param, head), nil
	}
	switch head {
	case "row":
		if len(parts) < 2 {
			return nil, errors.New("`row` alone names no field")
		}
		canon, ok := xmIntrinsic(parts[1])
		if !ok {
			return nil, fmt.Errorf("row.%s is not a row intrinsic, and the legacy engine refuses it", parts[1])
		}
		return xmPath("row", append([]string{canon}, parts[2:]...)...), nil
	case "actor", "args":
		// `actor.x` is the caller; `args.x` on the left is a caller flag
		// (memql#4814). Both keep their roots.
		if len(parts) < 2 {
			return nil, fmt.Errorf("`%s` alone names no field", head)
		}
		return xmPath(head, parts[1:]...), nil
	case "payload":
		if len(parts) < 2 {
			return nil, errors.New("`payload` alone names no field")
		}
		if _, ok := xmIntrinsic(parts[1]); ok {
			return nil, fmt.Errorf("payload.%s names the PAYLOAD field %s while row.%s names the row intrinsic; migrate it by hand", parts[1], parts[1], parts[1])
		}
		return xmPath("row", parts[1:]...), nil
	}
	if c.bound != "" && head == c.bound && len(parts) > 1 {
		// `<boundConcept>.<field>`: before bare payload access (epic #2292) a
		// filter wrote a payload field under the name of the concept its
		// query binds, and the legacy rewriter read the prefix as the
		// payload. It is that row's field. The tree carries none
		// (TestFilterSyntaxCanonical), but Go fixtures and bundles do.
		rest := parts[1:]
		if canon, ok := xmIntrinsic(rest[0]); ok {
			return xmPath("row", append([]string{canon}, rest[1:]...)...), nil
		}
		return xmPath("row", rest...), nil
	}
	if xmReservedHead(head) {
		return nil, fmt.Errorf("%s is a reserved engine name, not a field of the row", head)
	}
	if canon, ok := xmIntrinsic(head); ok {
		// A bare intrinsic in a filter IS the intrinsic (reservedFilterHead).
		return xmPath("row", append([]string{canon}, parts[1:]...)...), nil
	}
	return xmPath("row", parts...), nil
}

// xmReservedHead is the rest of the engine's reservedFilterHeadNames -- names a
// filter cannot use as a payload field and this rewrite has no place for --
// plus `ctx`, the retired argument shorthand: on a comparison's LEFT the engine
// never read it as an argument (it payload-prefixed it), so neither `args.` nor
// `row.` would be a faithful spelling.
func xmReservedHead(head string) bool {
	switch strings.ToLower(head) {
	case "now", "config", "trace", "meta", "schema", "partition", "ctx":
		return true
	}
	return false
}

// argRef converts a legacy argument reference. The legacy parser routes the
// caller's `actor.x` through ArgRefExpr too, carrying the prefix.
func (c xmConverter) argRef(path string) (ast.ExpressionNode, error) {
	if c.mode == xmSpecBody {
		return nil, fmt.Errorf("a spec or trait body has no arguments and no actor to read (%s)", path)
	}
	parts := strings.Split(path, ".")
	for _, p := range parts {
		if !xmIdent.MatchString(p) {
			return nil, fmt.Errorf("the reference %s has a segment %q that is not an identifier", path, p)
		}
	}
	if parts[0] == "actor" && len(parts) > 1 {
		return xmPath("actor", parts[1:]...), nil
	}
	return xmPath("args", parts...), nil
}

// path converts a bare dotted word the trigger evaluator reads as a value.
func (c xmConverter) path(parts []string) (ast.ExpressionNode, error) {
	for _, p := range parts {
		if !xmIdent.MatchString(p) {
			return nil, fmt.Errorf("the path %s has a segment %q that is not an identifier", strings.Join(parts, "."), p)
		}
	}
	if c.mode == xmTrigger {
		return c.triggerPath(parts), nil
	}
	if parts[0] == "payload" && len(parts) > 1 {
		return xmPath("row", parts[1:]...), nil
	}
	return xmPath(parts[0], parts[1:]...), nil
}

// xmTriggerScopeRoots are the names a trigger filter's scope binds itself --
// the automation run scope's fixed and seeded roots (component/automations
// RunScope, reservedAutomationRoots), which this package cannot import. A bare
// one keeps its name; every other bare name is a field of the triggering row.
var xmTriggerScopeRoots = map[string]bool{
	"now": true, "actor": true, "partition": true, "config": true, "trace": true,
	"event": true, "args": true, "steps": true, "timestamp": true, "ctx": true,
	"input": true, "item": true, "index": true, "var": true, "systemVar": true,
	"secret": true, "systemSecret": true, "automation": true, "error": true,
	"row": true,
}

// triggerPath converts a path in a trigger @filter (a dotted path's segments
// are already checked identifiers).
//
// `payload.<field>` is the triggering row's field, as it always was
// (`row.<field>`). A BARE name is too: edition 2026 reads a trigger filter as a
// lambda over the row, so `status == "archived"` means `row.status ==
// "archived"` -- unless the name is one the filter scope binds itself
// (xmTriggerScopeRoots: now, event, actor, args, ...) or a field of the
// automation's own args block, which the G2 tier resolves to the bound argument
// in both grammars. A row intrinsic takes its canonical spelling (`id` ->
// `row.id`). Every other rooted path keeps its root.
//
// The legacy string evaluator gave a bare name NO row meaning: with no dot it
// was not a path, so outside an args-block automation it fell through to its
// own text -- `status == "archived"` compared the word "status", constant-false
// -- and inside one it was the args field. The row reading is what the author
// meant and what edition 2026 can say; a bare args field keeps the meaning it
// had.
func (c xmConverter) triggerPath(parts []string) ast.ExpressionNode {
	head := parts[0]
	if head == "payload" && len(parts) > 1 {
		return xmPath("row", parts[1:]...)
	}
	if len(parts) == 1 && !xmTriggerScopeRoots[head] && !c.argsFields[head] {
		if canon, ok := xmIntrinsic(head); ok {
			return xmPath("row", canon)
		}
		return xmPath("row", head)
	}
	return xmPath(head, parts[1:]...)
}

// xmAutomationArgsAfter returns the fields declared by the args block of the
// automation an annotation ending at pos belongs to: the first top-level
// automation header after it. A terse automation, or one with no args block,
// declares none.
func xmAutomationArgsAfter(f *xmFile, pos int) map[string]bool {
	loc := xmAutomationHeader.FindStringIndex(f.mask[pos:])
	if loc == nil {
		return nil
	}
	// A terse automation (`automation x @trigger(...) => logic y`) has no
	// body, so no args block -- and the next braced header below it belongs
	// to a different automation.
	rest := strings.TrimLeft(f.mask[pos+loc[1]:], " \t")
	if !strings.HasPrefix(rest, "{") {
		return nil
	}
	open := pos + loc[1] + strings.IndexByte(f.mask[pos+loc[1]:], '{')
	end := MatchingCloseBrace(f.code, open)
	if end < 0 {
		return nil
	}
	body := f.mask[open+1 : end]
	args := argsBlockHeader.FindStringIndex(body)
	if args == nil {
		return nil
	}
	brace := open + 1 + args[0] + strings.LastIndexByte(body[args[0]:args[1]], '{')
	close := MatchingCloseBrace(f.code, brace)
	if close < 0 {
		return nil
	}
	out := map[string]bool{}
	for _, m := range xmArgsField.FindAllStringSubmatch(f.mask[brace+1:close], -1) {
		out[m[1]] = true
	}
	return out
}

// value converts a comparison's right-hand side, which the legacy parser hands
// over as a Go value or an expression node.
func (c xmConverter) value(v any) (ast.ExpressionNode, error) {
	switch x := v.(type) {
	case nil, *ast.NilExpr:
		return &ast.NilExpr{}, nil
	case string:
		return c.stringValue(x)
	case bool:
		return &ast.LiteralExpr{Value: x}, nil
	case int64:
		return &ast.LiteralExpr{Value: x}, nil
	case int:
		return &ast.LiteralExpr{Value: int64(x)}, nil
	case float64:
		return &ast.LiteralExpr{Value: x}, nil
	case []any:
		list := &ast.ListExpr{}
		for _, el := range x {
			n, err := c.value(el)
			if err != nil {
				return nil, err
			}
			list.Elems = append(list.Elems, n)
		}
		return list, nil
	case *ast.ArgRefExpr:
		return c.argRef(x.Path)
	case ast.ExpressionNode:
		return c.operand(x)
	}
	return nil, fmt.Errorf("a value of type %T has no edition-2026 form", v)
}

// stringValue converts a string the legacy parser produced for a value: a
// quoted literal, or a bare word it kept as text (an unquoted canonical id).
func (c xmConverter) stringValue(s string) (ast.ExpressionNode, error) {
	if c.mode == xmTrigger && c.bare[s] {
		// The trigger evaluator resolved a bare word as a path. A rooted
		// dotted word keeps that meaning; a colon-bearing one is an id,
		// which v1 writes as a string; a field of the automation's args
		// block is the bound argument in both grammars (G2); anything else
		// is ambiguous -- the legacy evaluator fell back to the word's own
		// text, so `payload.status == active` compared "active", and
		// neither reading can be picked for the author.
		if strings.Contains(s, ":") {
			return &ast.LiteralExpr{Value: s}, nil
		}
		if c.argsFields[s] {
			return &ast.IdentExpr{Name: s}, nil
		}
		if i := strings.IndexByte(s, '.'); i > 0 {
			switch s[:i] {
			case "payload", "event", "args", "actor":
				return c.path(strings.Split(s, "."))
			}
		}
		return nil, fmt.Errorf("the bare word %s is a path or a literal depending on the evaluator; quote it or root it (payload./event.) and rerun", s)
	}
	if strings.HasPrefix(s, "$") {
		return nil, fmt.Errorf("the $-variable %s has no edition-2026 form", s)
	}
	// A bare word the engine kept as text IS text: the spec binder maps only
	// a comparison's left side and the filter compiler binds a string value as
	// a literal. So `status == active` and `id == v1:crm:lead` are string
	// comparisons today and are written as the strings they are. Where the
	// author evidently meant a second field (`overage > reportedToStripe`),
	// the quotes make that long-standing mismatch visible instead of
	// silently changing what the clause does.
	return &ast.LiteralExpr{Value: s}, nil
}

// operand converts an expression node standing where a value goes.
func (c xmConverter) operand(n ast.ExpressionNode) (ast.ExpressionNode, error) {
	unary := func(name string, target ast.ExpressionNode) (ast.ExpressionNode, error) {
		arg, err := c.operand(target)
		if err != nil {
			return nil, err
		}
		return &ast.CallExpr{Name: name, Args: []ast.ExpressionNode{arg}}, nil
	}
	switch e := n.(type) {
	case *ast.LiteralExpr:
		return c.value(e.Value)
	case *ast.ArgRefExpr:
		return c.argRef(e.Path)
	case *ast.NilExpr:
		return &ast.NilExpr{}, nil
	case *ast.TimestampExprFunc:
		return &ast.IdentExpr{Name: "now"}, nil
	case *ast.CoalesceExpr:
		// `a ?? b ?? c` folds left, as the v1 parser folds it.
		var out ast.ExpressionNode
		for _, a := range e.Args {
			arm, err := c.operand(a)
			if err != nil {
				return nil, err
			}
			if out == nil {
				out = arm
				continue
			}
			out = xmBin("??", out, arm)
		}
		if out == nil {
			return nil, errors.New("an empty coalesce")
		}
		return out, nil
	case *ast.CanonicalIdExpr:
		v, err := c.operand(e.Value)
		if err != nil {
			return nil, err
		}
		return &ast.CallExpr{Name: "canonicalId", Args: []ast.ExpressionNode{v, &ast.LiteralExpr{Value: e.Concept}}}, nil
	case *ast.HashExpr:
		return unary("hash", e.Target)
	case *ast.ShortIdExpr:
		return unary("shortId", e.Target)
	case *ast.LowerExpr:
		return unary("lower", e.Target)
	case *ast.UpperExpr:
		return unary("upper", e.Target)
	case *ast.TrimExpr:
		return unary("trim", e.Target)
	case *ast.ToStringExpr:
		return unary("toString", e.Target)
	case *ast.FunctionCallExpr:
		return c.call(e)
	case *ast.SpecReferenceExpr:
		if c.mode == xmTrigger {
			return c.path(strings.Split(e.Name, "."))
		}
		return nil, fmt.Errorf("the bare word %s in a value position is neither a quoted string nor a reference", e.Name)
	}
	return nil, fmt.Errorf("%T has no edition-2026 form this rewrite writes", n)
}

// call converts a generic function call: positional arguments only, in index
// order (the legacy node keeps them in a map keyed "0", "1", ...).
func (c xmConverter) call(e *ast.FunctionCallExpr) (ast.ExpressionNode, error) {
	if e.Kind != "" {
		return nil, fmt.Errorf("the construct call %s %s(...) cannot stand in a clause", e.Kind, e.Name)
	}
	if !xmIdent.MatchString(e.Name) {
		return nil, fmt.Errorf("the call %s(...) has no identifier name", e.Name)
	}
	out := &ast.CallExpr{Name: e.Name}
	for i := 0; i < len(e.Args); i++ {
		v, ok := e.Args[fmt.Sprint(i)]
		if !ok {
			return nil, fmt.Errorf("the call %s(...) has named arguments, which only a construct call takes", e.Name)
		}
		arg, err := c.value(v)
		if err != nil {
			return nil, err
		}
		out.Args = append(out.Args, arg)
	}
	return out, nil
}

// xmBareWords returns the identifier spellings a clause used that never
// appear as a string literal in it.
func xmBareWords(src string) map[string]bool {
	toks, err := NewLexer(src).Tokenize()
	if err != nil {
		return nil
	}
	idents, strs := map[string]bool{}, map[string]bool{}
	for _, t := range toks {
		switch t.Type {
		case TokenIdentifier:
			idents[t.Literal] = true
		case TokenString:
			strs[t.Literal] = true
		}
	}
	for s := range strs {
		delete(idents, s)
	}
	return idents
}

func xmBin(op string, l, r ast.ExpressionNode) *ast.BinaryExpr {
	return &ast.BinaryExpr{Op: op, Left: l, Right: r}
}

func xmPath(root string, fields ...string) ast.ExpressionNode {
	var n ast.ExpressionNode = &ast.IdentExpr{Name: root}
	for _, f := range fields {
		n = &ast.MemberExpr{Object: n, Field: f}
	}
	return n
}

// xmCheckV1 verifies a converted tree is made only of edition-2026 nodes with
// printable literals. FormatExpr prints anything else as a `<?Type>` marker,
// which re-parses to nothing, so a leak here would ship as broken source.
func xmCheckV1(n ast.ExpressionNode) error {
	var bad error
	ast.WalkV1(n, func(e ast.ExpressionNode) bool {
		if bad != nil {
			return false
		}
		if ast.KindOf(e) == ast.KindUnknown {
			bad = fmt.Errorf("internal: the conversion produced a %T, which is not an edition-2026 node", e)
			return false
		}
		if l, ok := e.(*ast.LiteralExpr); ok {
			switch l.Value.(type) {
			case string, bool, int64, float64:
			default:
				bad = fmt.Errorf("internal: the conversion produced a literal of type %T", l.Value)
				return false
			}
		}
		return true
	})
	return bad
}

// xmWrap breaks a printed clause at its top-level `&&` (or `||`, when that is
// the top-level connective) when the original spanned lines or the result is
// too wide. The first line ends after an operand, and each continuation line
// opens with the operator, hanging so its operand lines up under the first
// operand after the lambda header -- the layout dsl/router/queries.memql's
// multi-line filter already used.
func xmWrap(printed string, root ast.ExpressionNode, operandCol int, indent string, multiLine, tooWide bool) string {
	if !multiLine && !tooWide {
		return printed
	}
	b, ok := root.(*ast.BinaryExpr)
	if !ok || (b.Op != "&&" && b.Op != "||") {
		return printed
	}
	parts := xmSplitChain(printed, b.Op)
	if len(parts) < 2 {
		return printed
	}
	pad := operandCol - len(indent) - len(b.Op) - 1
	if pad < 0 {
		pad = 0
	}
	return strings.Join(parts, "\n"+indent+strings.Repeat(" ", pad)+b.Op+" ")
}

// xmSplitChain splits canonical printer output at the connective op where it
// sits outside every bracket and string. FormatExpr parenthesises a looser
// operand and never a same-level left one, so for a root of op these are
// exactly the chain's operands.
func xmSplitChain(printed, op string) []string {
	sep := " " + op + " "
	var parts []string
	depth, last := 0, 0
	for i := 0; i < len(printed); i++ {
		switch c := printed[i]; c {
		case '"':
			for i++; i < len(printed) && printed[i] != '"'; i++ {
				if printed[i] == '\\' {
					i++
				}
			}
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ' ':
			if depth == 0 && strings.HasPrefix(printed[i:], sep) {
				parts = append(parts, printed[last:i])
				i += len(sep) - 1
				last = i + 1
			}
		}
	}
	return append(parts, printed[last:])
}

// ----------------------------------------------------------------------------
// D. In-process call forms, byte by byte
// ----------------------------------------------------------------------------

// xmPosError is a refusal inside an in-process body, at an offset relative to
// the body's first byte.
type xmPosError struct {
	pos int
	msg string
}

func (e *xmPosError) Error() string { return e.msg }

func xmRefuse(pos int, format string, args ...any) error {
	return &xmPosError{pos: pos, msg: fmt.Sprintf(format, args...)}
}

var xmInProcessCalls = []string{"cond", "concat", "coalesce", "exists"}

// xmRewriteInProcess rewrites the in-process call forms of one construct body.
// Calls are taken innermost first -- the call that starts furthest right can
// contain no other -- and the text is rescanned after each, so a nested call
// is rewritten before the call around it decides how to parenthesise it.
// Every rewrite so far lies to the right of the next call picked, and quoting a
// concept name adds no newline, so a refused call's offset still falls on its
// original line -- which is what a refusal reports.
func xmRewriteInProcess(s string) (string, error) {
	s = xmQuoteCanonicalIdConcepts(s)
	s, err := xmExpandKeylessMapEntries(s)
	if err != nil {
		return "", err
	}
	for guard := 0; ; guard++ {
		if guard > 4096 {
			return "", xmRefuse(0, "internal: the in-process rewrite did not converge")
		}
		mask := blankCommentsAndStrings(s)
		start, name := xmLastInProcessCall(mask)
		if start < 0 {
			break
		}
		next, err := xmRewriteCall(s, mask, start, name)
		if err != nil {
			return "", err
		}
		s = next
	}
	return xmNullToNil(s), nil
}

// xmLastInProcessCall returns the right-most call site of the three forms, or
// -1. A name glued to an identifier or a dot is a different symbol
// (`myconcat(`, `x.concat(`), and one after a construct-kind keyword is a call
// to a construct that happens to share the name (`logic concat(...)`).
func xmLastInProcessCall(mask string) (int, string) {
	best, bestName := -1, ""
	for _, name := range xmInProcessCalls {
		needle := name + "("
		for from := 0; ; {
			i := strings.Index(mask[from:], needle)
			if i < 0 {
				break
			}
			i += from
			from = i + 1
			if i > 0 && isIdentByte(mask[i-1]) {
				continue
			}
			if isInvocationKindKeyword(xmWordBefore(mask, i)) {
				continue
			}
			if i > best {
				best, bestName = i, name
			}
		}
	}
	return best, bestName
}

// xmWordBefore returns the identifier ending just before pos, skipping blanks.
func xmWordBefore(mask string, pos int) string {
	i := pos - 1
	for i >= 0 && (mask[i] == ' ' || mask[i] == '\t') {
		i--
	}
	end := i + 1
	for i >= 0 && isIdentByte(mask[i]) {
		i--
	}
	return mask[i+1 : end]
}

type xmSpan struct{ lo, hi int }

// xmMatchParen returns the index of the `)` closing the `(` at open, on a
// masked view (so a paren in a string or a comment does not count), or -1.
func xmMatchParen(mask string, open int) int {
	depth := 0
	for i := open; i < len(mask); i++ {
		switch mask[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// xmSplitArgs splits mask[lo:hi] at its top-level commas and returns each
// argument's span with the surrounding whitespace trimmed.
func xmSplitArgs(mask string, lo, hi int) []xmSpan {
	var out []xmSpan
	depth, from := 0, lo
	trimmed := func(a, b int) xmSpan {
		for a < b && xmIsSpace(mask[a]) {
			a++
		}
		for b > a && xmIsSpace(mask[b-1]) {
			b--
		}
		return xmSpan{a, b}
	}
	for i := lo; i < hi; i++ {
		switch mask[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, trimmed(from, i))
				from = i + 1
			}
		}
	}
	return append(out, trimmed(from, hi))
}

func xmIsSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// xmCallContext says what stands around a call, which decides whether its
// replacement needs parentheses of its own.
type xmCallContext struct {
	// value: the call is a whole value -- a statement right-hand side
	// (`x := ...`, `return ...`), an argument, a list element, or an object
	// or named-argument value. Anything else is an operand of an operator.
	value bool
	// sole: the call is the entire content of an enclosing ( ), the one
	// place a multi-line join can leave the brackets to the call around it.
	sole bool
}

func xmContextOf(mask string, start, end int) xmCallContext {
	i := start - 1
	for i >= 0 && xmIsSpace(mask[i]) {
		i--
	}
	opens := false
	switch {
	case i < 0:
		opens = true
	case strings.IndexByte("([{,:", mask[i]) >= 0:
		opens = true
	case mask[i] == '=':
		// `:=` assigns and a lone `=` names an argument; `==` `!=` `<=` `>=`
		// compare, which makes the call an operand.
		opens = i == 0 || strings.IndexByte("=!<>", mask[i-1]) < 0
	case isIdentByte(mask[i]):
		opens = xmWordBefore(mask, i+1) == "return"
	}
	j := end
	for j < len(mask) && (mask[j] == ' ' || mask[j] == '\t') {
		j++
	}
	closes := j >= len(mask) || strings.IndexByte(")]},\n\r", mask[j]) >= 0
	k := end
	for k < len(mask) && xmIsSpace(mask[k]) {
		k++
	}
	return xmCallContext{
		value: opens && closes,
		sole:  i >= 0 && mask[i] == '(' && k < len(mask) && mask[k] == ')',
	}
}

// xmLooseNeighbours reports whether a comparison written in place of the call
// at mask[start:end] needs no brackets: each neighbour is a boundary, a clause
// keyword, or an operator that binds LOOSER than a comparison (`&&`, `||`, the
// ternary's `?` and `:`). Anything tighter -- `!`, `+`, `??`, another
// comparison, a postfix `.` -- would capture one side of it.
// xmTightNeighbours reports whether an operator binding TIGHTER than `??`
// sits against the span: an arithmetic operator or a unary `!` before it, or
// an arithmetic operator or a member access after it. Such a neighbour would
// capture one arm of a bare `a ?? b`, so the fold is bracketed. A comparison
// or a connective is looser than `??` and needs nothing: `a ?? b == "x"`
// already reads `(a ?? b) == "x"` (D9).
func xmTightNeighbours(mask string, start, end int) bool {
	i := start - 1
	for i >= 0 && xmIsSpace(mask[i]) {
		i--
	}
	if i >= 0 && strings.IndexByte("+-*/%!", mask[i]) >= 0 {
		// A `!` that is the first half of `!=` is a comparison, not a unary.
		if !(mask[i] == '!' && i+1 < len(mask) && mask[i+1] == '=') {
			return true
		}
	}
	j := end
	for j < len(mask) && (mask[j] == ' ' || mask[j] == '\t') {
		j++
	}
	return j < len(mask) && strings.IndexByte(".+-*/%", mask[j]) >= 0
}

func xmLooseNeighbours(mask string, start, end int) bool {
	i := start - 1
	for i >= 0 && xmIsSpace(mask[i]) {
		i--
	}
	before := i < 0
	switch {
	case before:
	case strings.IndexByte("([{,:&|", mask[i]) >= 0:
		before = true
	case mask[i] == '?':
		before = i == 0 || mask[i-1] != '?'
	case mask[i] == '=':
		before = i == 0 || strings.IndexByte("=!<>", mask[i-1]) < 0
	case isIdentByte(mask[i]):
		switch xmWordBefore(mask, i+1) {
		case "return", "if", "where":
			before = true
		}
	}
	j := end
	for j < len(mask) && (mask[j] == ' ' || mask[j] == '\t') {
		j++
	}
	after := j >= len(mask)
	switch {
	case after:
	case strings.IndexByte(")]},{:\n\r", mask[j]) >= 0:
		after = true
	case mask[j] == '&' || mask[j] == '|':
		after = j+1 < len(mask) && mask[j+1] == mask[j]
	case mask[j] == '?':
		after = j+1 >= len(mask) || mask[j+1] != '?'
	}
	return before && after
}

// xmQuoteCanonicalIdConcepts writes canonicalId's concept argument as the
// string it names. The legacy grammar took a bare imported concept name there
// (`canonicalId(args.campaignId, campaign)`) and the loader resolved it; an
// edition-2026 expression resolves no bare name, so the name is quoted.
// Sites are taken right to left, so the text before the next one never moves;
// the mask is rebuilt after each change because an enclosing site's closing
// paren does.
func xmQuoteCanonicalIdConcepts(s string) string {
	mask := blankCommentsAndStrings(s)
	for from := len(mask); ; {
		i := strings.LastIndex(mask[:from], "canonicalId(")
		if i < 0 {
			return s
		}
		from = i
		if i > 0 && isIdentByte(mask[i-1]) {
			continue
		}
		open := i + len("canonicalId")
		end := xmMatchParen(mask, open)
		if end < 0 {
			continue
		}
		args := xmSplitArgs(mask, open+1, end)
		if len(args) != 2 {
			continue
		}
		name := s[args[1].lo:args[1].hi]
		if !xmConceptName.MatchString(name) {
			continue // already a string, or not a name at all
		}
		s = s[:args[1].lo] + ast.QuoteString(name) + s[args[1].hi:]
		mask = blankCommentsAndStrings(s)
	}
}

// xmConceptName is a bare concept name as the legacy grammar accepts it: an
// imported short name, or a canonical id that scanned as one identifier.
var xmConceptName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?::[A-Za-z_][A-Za-z0-9_]*)*$`)

// xmOps records which operators an argument carries outside every bracket.
type xmOps struct {
	ternary, lambda, logical, compare, coalesce, additive bool
}

// looserThanPlus reports an operator that binds looser than `+`: a bare `+`
// joined to such an argument would capture part of it.
func (o xmOps) looserThanPlus() bool {
	return o.ternary || o.lambda || o.logical || o.compare || o.coalesce
}

func xmTopOps(m string) xmOps {
	var o xmOps
	depth := 0
	for i := 0; i < len(m); i++ {
		c := m[i]
		switch c {
		case '(', '[', '{':
			depth++
			continue
		case ')', ']', '}':
			depth--
			continue
		}
		if depth != 0 {
			continue
		}
		var prev, next byte
		if i > 0 {
			prev = m[i-1]
		}
		if i+1 < len(m) {
			next = m[i+1]
		}
		switch c {
		case '?':
			switch {
			case next == '?':
				o.coalesce = true
				i++
			case prev != '.':
				o.ternary = true
			}
		case '&', '|':
			if next == c {
				o.logical = true
				i++
			}
		case '=':
			switch next {
			case '>':
				o.lambda = true
				i++
			case '=':
				o.compare = true
				i++
			}
		case '!':
			if next == '=' {
				o.compare = true
				i++
			}
		case '<', '>':
			o.compare = true
			if next == '=' {
				i++
			}
		case '+':
			o.additive = true
		case '-':
			// `-` is also an identifier character (`bff-local`), so only a
			// spaced one is certainly subtraction.
			if xmIsSpace(prev) && xmIsSpace(next) {
				o.additive = true
			}
		case 'i':
			if xmWordAt(m, i, "in") {
				o.compare = true
			}
		case 's':
			if xmWordAt(m, i, "startsWith") {
				o.compare = true
			}
		}
	}
	return o
}

func xmWordAt(s string, i int, word string) bool {
	if !strings.HasPrefix(s[i:], word) {
		return false
	}
	if i > 0 && isIdentByte(s[i-1]) {
		return false
	}
	end := i + len(word)
	return end >= len(s) || !isIdentByte(s[end])
}

// xmParen parenthesises text unless it already is one parenthesised group.
func xmParen(text, mask string) string {
	if strings.HasPrefix(mask, "(") && xmMatchParen(mask, 0) == len(mask)-1 {
		return text
	}
	return "(" + text + ")"
}

func xmRewriteCall(s, mask string, start int, name string) (string, error) {
	open := start + len(name)
	end := xmMatchParen(mask, open)
	if end < 0 {
		return "", xmRefuse(start, "%s( is never closed", name)
	}
	args := xmSplitArgs(mask, open+1, end)
	for _, a := range args {
		if a.lo == a.hi {
			return "", xmRefuse(start, "%s(...) has an empty argument", name)
		}
	}
	ctx := xmContextOf(mask, start, end+1)
	text := func(a xmSpan) string { return s[a.lo:a.hi] }
	ops := func(a xmSpan) xmOps { return xmTopOps(mask[a.lo:a.hi]) }
	// A comment BETWEEN the arguments would have nowhere to go once the
	// commas are gone; one inside an argument travels with it.
	gapComment := func() bool {
		prev := open + 1
		for _, a := range args {
			if s[prev:a.lo] != mask[prev:a.lo] {
				return true
			}
			prev = a.hi
		}
		return s[prev:end] != mask[prev:end]
	}

	var out string
	switch name {
	case "cond":
		if len(args) != 3 {
			return "", xmRefuse(start, "cond(...) takes a predicate, a then and an else; this one has %d arguments", len(args))
		}
		if gapComment() {
			return "", xmRefuse(start, "a comment between cond's arguments would be lost; move it and rerun")
		}
		p, a, b := args[0], args[1], args[2]
		pt, at, bt := text(p), text(a), text(b)
		// The condition must bind tighter than `? :`. The then-branch may
		// legally be a bare ternary, but the canonical printer brackets one for
		// the reader and so does this. The else-branch chains bare: the
		// operator is right-associative.
		if o := ops(p); o.ternary || o.lambda {
			pt = xmParen(pt, mask[p.lo:p.hi])
		}
		if o := ops(a); o.ternary || o.lambda {
			at = xmParen(at, mask[a.lo:a.hi])
		}
		if ops(b).lambda {
			bt = xmParen(bt, mask[b.lo:b.hi])
		}
		out = pt + " ? " + at + " : " + bt
		if !ctx.value {
			out = "(" + out + ")"
		}

	case "concat":
		if len(args) < 2 {
			return "", xmRefuse(start, "concat(...) with fewer than two arguments has no + spelling")
		}
		// `+` joins text only once a string is on its left: two numbers ADD.
		// concat joined the text of whatever it was given, so the join is
		// exact only when the first `+` already has a string in hand.
		if !xmStaticString(s, mask, args[0]) && !xmStaticString(s, mask, args[1]) {
			return "", xmRefuse(start, "concat(%s, %s, ...): neither of the first two arguments is certainly a string, and `+` adds two numbers where concat joined their text; wrap the first in toString(...) and rerun", text(args[0]), text(args[1]))
		}
		multi := strings.Contains(s[start:end+1], "\n")
		wrap := !ctx.value || (multi && !ctx.sole)
		var b strings.Builder
		if wrap {
			b.WriteByte('(')
		}
		b.WriteString(s[open+1 : args[0].lo])
		for k, a := range args {
			t := text(a)
			// `??`, `? :`, a comparison or a connective binds looser than `+`
			// and would capture its neighbour; a `+` or `-` after the first
			// argument would regroup the sum. Arguments keep their own lines.
			if o := ops(a); o.looserThanPlus() || (k > 0 && o.additive) {
				t = xmParen(t, mask[a.lo:a.hi])
			}
			b.WriteString(t)
			if k+1 == len(args) {
				break
			}
			gap := s[a.hi:args[k+1].lo]
			comma := strings.IndexByte(mask[a.hi:args[k+1].lo], ',')
			before, after := gap[:comma], gap[comma+1:]
			if before == "" {
				before = " "
			}
			if after == "" || !xmIsSpace(after[0]) {
				after = " " + after
			}
			b.WriteString(before + "+" + after)
		}
		b.WriteString(s[args[len(args)-1].hi:end])
		if wrap {
			b.WriteByte(')')
		}
		out = b.String()

	case "coalesce":
		// The longhand the null-coalesce rewrite (memql#3627) already retired
		// from dsl/ but that bundles and examples still carry. Folding it here
		// makes `expressions` the one rewrite a tree needs to reach the v1
		// grammar, which refuses coalesce( outright. `??` is the same
		// blank-coalescing fold (rule 30) and is associative, so a chain needs
		// no inner grouping; an argument holding an operator LOOSER than `??`
		// (a comparison, `in`, `startsWith`, a connective, a ternary, a lambda)
		// is bracketed so it cannot capture its neighbour.
		if len(args) < 2 {
			return "", xmRefuse(start, "coalesce(...) with fewer than two arguments has no ?? spelling")
		}
		if gapComment() {
			return "", xmRefuse(start, "a comment between coalesce's arguments would be lost; move it and rerun")
		}
		parts := make([]string, len(args))
		for k, a := range args {
			t := text(a)
			if o := ops(a); o.ternary || o.lambda || o.logical || o.compare {
				t = xmParen(t, mask[a.lo:a.hi])
			}
			parts[k] = t
		}
		out = strings.Join(parts, " ?? ")
		if xmTightNeighbours(mask, start, end+1) {
			out = "(" + out + ")"
		}

	case "exists":
		if len(args) != 1 {
			return "", xmRefuse(start, "exists(...) takes one argument; this one has %d", len(args))
		}
		if gapComment() {
			return "", xmRefuse(start, "a comment inside exists(...) would be lost; move it and rerun")
		}
		// exists() came from the trigger string evaluator (E3), which also
		// read a BLANK string as absent. Edition 2026 has one notion of unset
		// -- absent, JSON null, nil and "" are one value to == and != -- so a
		// plain `x != nil` already excludes the blank string, and that is the
		// whole of what the call meant at the sites that use it. (E3 also read
		// a whitespace-only string and an empty list or object as absent; the
		// sites in dsl/ test ids, timestamps and an object its writer only
		// ever sets non-empty, which that difference cannot reach.)
		x := text(args[0])
		if o := ops(args[0]); o.ternary || o.lambda || o.logical || o.compare {
			x = xmParen(x, mask[args[0].lo:args[0].hi])
		}
		out = x + " != nil"
		if !xmLooseNeighbours(mask, start, end+1) {
			out = "(" + out + ")"
		}
	}
	return s[:start] + out + s[end+1:], nil
}

// xmStringFuncs are the functions whose result is always a string.
var xmStringFuncs = map[string]bool{
	"hash": true, "shortId": true, "lower": true, "upper": true,
	"trim": true, "toString": true, "canonicalId": true,
}

// xmStaticString reports whether an argument is certainly a string: a string
// literal, a call to a string function, or a sum whose first operand is one
// (an inner concat already rewritten to `+`).
func xmStaticString(s, mask string, a xmSpan) bool {
	m := mask[a.lo:a.hi]
	for strings.HasPrefix(m, "(") && xmMatchParen(m, 0) == len(m)-1 {
		m = strings.TrimSpace(m[1 : len(m)-1])
	}
	if plus := xmTopLevelIndex(m, '+'); plus >= 0 {
		m = strings.TrimSpace(m[:plus]) // the first operand of a top-level sum
	}
	if strings.HasPrefix(m, `"`) {
		// The mask blanks a literal's content, so its closing quote is the
		// next quote; the operand is one literal when that quote ends it.
		return strings.IndexByte(m[1:], '"') == len(m)-2
	}
	if i := strings.IndexByte(m, '('); i > 0 && xmStringFuncs[m[:i]] {
		return xmMatchParen(m, i) == len(m)-1
	}
	return false
}

// xmTopLevelIndex returns the index of the first c in mask outside every
// bracket, or -1.
func xmTopLevelIndex(mask string, c byte) int {
	depth := 0
	for i := 0; i < len(mask); i++ {
		switch mask[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case c:
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// xmPathEntry is a bare dotted path, the legacy key-less map entry
// (parseObject's "Shorthand A"): its key is the terminal segment.
var xmPathEntry = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)+$`)

// xmAccessorEntry is the legacy key-less accessor entry (parseObject's
// "Shorthand B"): `node("a.b")` / `var("a.b")`, keyed by the terminal segment
// of its quoted argument.
var xmAccessorEntry = regexp.MustCompile(`^(?:node|var)\s*\(`)

// xmNoMapBlocks are the blocks whose braces hold no in-process map literal
// this rewrite may touch: an args block declares fields, and a mutation's
// write blocks are expanded by the struct-form rewriter, which owns their
// shorthands.
var xmNoMapBlocks = map[string]bool{"args": true, "insert": true, "update": true, "accept": true, "stamp": true}

// xmExpandKeylessMapEntries writes out the key of every key-less map-literal
// entry in one construct body (epic memql#5363).
//
// The legacy object literal accepted two entries with no `key:`, and the
// edition-2026 map literal refuses both ("a map key is one name" /
// "a map entry is written key: value"):
//
//	{ args.event.payload.identityId }  ->  { identityId: args.event.payload.identityId }
//	{ allAgents }                      ->  { allAgents: allAgents }
//
// -- the first keyed by the path's terminal segment (Shorthand A), the second
// a pun (G3, #2365). Each is expanded to the entry the legacy parse built, so
// the map means what it meant.
//
// A brace is a map literal where an expression is expected: after `:`, `=`,
// `(`, `,`, `[` or `return`. Any other brace opens a block, and a block's
// keyword decides whether maps inside it are this rewrite's (xmNoMapBlocks).
// Refused by name, rather than guessed at: the accessor shorthand
// `{ node("a.b") }`, which keys by its ARGUMENT's last segment -- no tree
// carries one -- and two entries that would land on one key, which the legacy
// map resolved last-wins in silence and a v1 map refuses.
func xmExpandKeylessMapEntries(s string) (string, error) {
	mask := blankCommentsAndStrings(s)
	type frame struct {
		open  byte
		isMap bool
		skip  bool // inside a block whose maps are not this rewrite's
	}
	type edit struct {
		start, end int
		text       string
	}
	var (
		stack []frame
		edits []edit
	)
	skipping := func() bool {
		for _, f := range stack {
			if f.skip {
				return true
			}
		}
		return false
	}
	for i := 0; i < len(mask); i++ {
		switch c := mask[i]; c {
		case '(', '[':
			stack = append(stack, frame{open: c})
		case ')', ']':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case '{':
			prevCh, prevWord := xmPrevSignificant(mask, i)
			isMap := strings.ContainsRune(":=(,[?", rune(prevCh)) || prevWord == "return" || xmAfterArrow(mask, i)
			fr := frame{open: '{', isMap: isMap, skip: !isMap && xmNoMapBlocks[prevWord]}
			if isMap && !skipping() {
				close := MatchingCloseBrace(mask, i)
				if close < 0 {
					return "", xmRefuse(i, "a map literal here does not close")
				}
				es, err := xmMapEntryEdits(s, mask, i, close)
				if err != nil {
					return "", err
				}
				for _, e := range es {
					edits = append(edits, edit{e.start, e.end, e.text})
				}
			}
			stack = append(stack, fr)
		case '}':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	if len(edits) == 0 {
		return s, nil
	}
	sort.Slice(edits, func(a, b int) bool { return edits[a].start < edits[b].start })
	var b strings.Builder
	prev := 0
	for _, e := range edits {
		b.WriteString(s[prev:e.start])
		b.WriteString(e.text)
		prev = e.end
	}
	b.WriteString(s[prev:])
	return b.String(), nil
}

// xmAfterArrow reports whether the brace at pos follows a lambda arrow, `g =>
// { ... }` -- a collection method's projection, whose body is a map.
func xmAfterArrow(mask string, pos int) bool {
	i := pos - 1
	for i >= 0 && (mask[i] == ' ' || mask[i] == '\t' || mask[i] == '\n' || mask[i] == '\r') {
		i--
	}
	return i >= 1 && mask[i] == '>' && mask[i-1] == '='
}

// xmPrevSignificant returns the last non-blank character before pos on the
// masked view, and the identifier it ends, if it ends one.
func xmPrevSignificant(mask string, pos int) (byte, string) {
	i := pos - 1
	for i >= 0 && (mask[i] == ' ' || mask[i] == '\t' || mask[i] == '\n' || mask[i] == '\r') {
		i--
	}
	if i < 0 {
		return 0, ""
	}
	end := i + 1
	for i >= 0 && isIdentByte(mask[i]) {
		i--
	}
	return mask[end-1], mask[i+1 : end]
}

// xmMapEntryEdits returns the edits that key the key-less entries of the map
// literal whose braces sit at open and close.
func xmMapEntryEdits(s, mask string, open, close int) ([]struct {
	start, end int
	text       string
}, error) {
	type ed = struct {
		start, end int
		text       string
	}
	var out []ed
	keys := map[string]int{}
	depth := 0
	entryStart := open + 1
	visit := func(lo, hi int) error {
		// [lo, hi) is one entry, surrounding blanks included.
		t := strings.TrimSpace(mask[lo:hi])
		if t == "" {
			return nil
		}
		at := lo + strings.Index(mask[lo:hi], t)
		raw := s[at : at+len(t)]
		key, expanded := "", ""
		switch {
		case xmEntryKeyed(t):
			sep := strings.IndexAny(t, ":=")
			key = strings.TrimSpace(t[:sep])
			if t[sep] == '=' {
				// The legacy `key = value` separator: the same entry, which a
				// v1 map spells `key: value`.
				value := strings.TrimLeft(raw[sep+1:], " \t")
				expanded = key + ": " + value
			}
		case xmPathEntry.MatchString(t):
			key = t[strings.LastIndexByte(t, '.')+1:]
			expanded = key + ": " + raw
		case xmIdent.MatchString(t):
			key = t
			expanded = key + ": " + raw
		case xmAccessorEntry.MatchString(t):
			return xmRefuse(at, "the map entry %s has no key: the legacy grammar keyed it by the last segment of its quoted argument, which edition 2026 does not; write the key yourself (key: %s)", raw, raw)
		default:
			return nil // not a shorthand: the v1 parser names what it is
		}
		if prevAt, dup := keys[key]; dup {
			return xmRefuse(at, "two entries of one map literal land on the key %q (the entry at byte %d and %s): the legacy map kept only the last one in silence, and an edition-2026 map refuses a duplicate key; write distinct keys", key, prevAt, raw)
		}
		keys[key] = at
		if expanded != "" {
			out = append(out, ed{start: at, end: at + len(t), text: expanded})
		}
		return nil
	}
	for i := open + 1; i < close; i++ {
		switch mask[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				if err := visit(entryStart, i); err != nil {
					return nil, err
				}
				entryStart = i + 1
			}
		}
	}
	if err := visit(entryStart, close); err != nil {
		return nil, err
	}
	return out, nil
}

// xmEntryKeyed reports whether a map entry (masked, trimmed) already names
// its key: `key: value`, or the legacy `key = value`.
func xmEntryKeyed(t string) bool {
	i := 0
	for i < len(t) && (isIdentByte(t[i]) || t[i] == '-') {
		i++
	}
	if i == 0 {
		return false
	}
	rest := strings.TrimLeft(t[i:], " \t\n\r")
	return strings.HasPrefix(rest, ":") || (strings.HasPrefix(rest, "=") && !strings.HasPrefix(rest, "=="))
}

// xmNullToNil replaces the bare word `null` with `nil` outside strings and
// comments. Edition 2026 has one spelling of absence.
func xmNullToNil(s string) string {
	mask := blankCommentsAndStrings(s)
	var b strings.Builder
	prev := 0
	for from := 0; ; {
		i := strings.Index(mask[from:], "null")
		if i < 0 {
			break
		}
		i += from
		from = i + 4
		if !xmWordAt(mask, i, "null") {
			continue
		}
		b.WriteString(s[prev:i])
		b.WriteString("nil")
		prev = i + 4
	}
	if prev == 0 {
		return s
	}
	b.WriteString(s[prev:])
	return b.String()
}
