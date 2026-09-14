package main

// bodies_read.go -- reading the retired body forms into statements (epic
// memql#5370, task memql#5373).
//
// The reader turns each retired body into lstmt values already shaped like
// the edition-2026 statements they become, keeping every comment attached to
// the statement or argument it sat above. It decides NOTHING about names,
// kinds or order: references are rewritten by bodies_refs.go, kinds come from
// the declaration index, and order from bodies_order.go.

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

const (
	formCall     = "call"     // `[n :=] <kind> <name>(...)`
	formIfCall   = "ifCall"   // `if <cond> { [n :=] <call> }` from a gated step or `n := if c { call }`
	formIf       = "if"       // a logic `if` block with statements, and its else arms
	formFor      = "for"      // forEach / for-range
	formSwitch   = "switch"   // switch with case blocks
	formParallel = "parallel" // parallel with branches
	formExpr     = "expr"     // `n := <expression>`
	formReturn   = "return"   // `return [<expression>]`
	formPublish  = "publish"  // publishEvent(topic: "t", payload: {...})
)

// lstmt is one statement as the reader found it in a retired form.
type lstmt struct {
	leading  []string // comment lines above it, trimmed; "" is a blank line
	after    []string // comment lines that followed it inside a step's braces
	line     int      // 1-based line of its first token
	col      int      // 0-based column of its first token: multi-line parts shift from it
	trailing string   // a `//` comment ending its last line

	name string // the bound name; "" when none
	form string

	call  *lcall // formCall, formIfCall
	retry int    // retry(n) on a call
	cond  string // formIfCall

	branches []lbranch // formIf

	loopVar      string // formFor
	source       string
	filter       string
	body         []*lstmt
	bodyTrailing []string

	subject string // formSwitch
	cases   []lcase

	wait     string   // formParallel: "all" | "any"
	failFast bool     // formParallel, as written
	par      []*lstmt // formParallel branches: name = branch label

	expr string // formExpr, formReturn

	topic      string // formPublish: the topic literal, quoted
	payload    string // formPublish: the payload map text
	payloadCol int    // formPublish: the column its continuation lines are relative to; -1 when it began on the call's line
}

// lbranch is one arm of a logic if statement.
type lbranch struct {
	cond     string // "" for the final else
	body     []*lstmt
	trailing []string
}

// lcase is one case (or the default) of a switch.
type lcase struct {
	leading   []string
	labels    []string // literal text as written
	isDefault bool
	body      []*lstmt
	trailing  []string
}

// lcall is one construct call.
type lcall struct {
	kind      string // "" when the source wrote none
	name      string
	args      []larg
	multiline bool   // the arguments were written one per line
	surface   string // action: the argument of `on surface(...)`, as written
	raw       string // the call as written, for a bare call that turns out to be an expression function
}

// larg is one call argument.
type larg struct {
	leading  []string // comment lines above it
	name     string   // "" for a positional (punned) argument
	sep      string   // the text between the name and the value, e.g. ":     "
	value    string   // the value's text, possibly multi-line
	trailing string   // a `//` comment ending its line
	col      int      // its column when it began its own line; -1 when it shared the bracket's line
	punned   bool     // the rewrite named it: the source passed it positionally
}

// bodyError is a construct the reader cannot carry across exactly.
type bodyError struct {
	file, construct string
	line            int
	msg             string
}

func (e *bodyError) Error() string {
	return fmt.Sprintf("%s:%d: %s: %s", e.file, e.line, e.construct, e.msg)
}

// ---- construct location ---------------------------------------------------

// construct is one automation or logic declaration in a file.
type construct struct {
	kind  string // "automation" | "logic"
	name  string
	start int // offset of the keyword
	open  int // offset of `{`; -1 for a terse automation
	close int // offset of the matching `}`; for a terse automation, the end of its line
	terse bool
	// For a terse automation: the `@trigger(...)` text and the target logic.
	terseTrigger string
	terseLogic   string
}

var (
	constructHeader = regexp.MustCompile(`(?m)^[ \t]*(automation|logic)[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)
	terseHeader     = regexp.MustCompile(`(?m)^[ \t]*automation[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]+(@trigger\([^)]*\))[ \t]*=>[ \t]*logic[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*$`)
)

// findConstructs returns every automation and logic declaration in src, in
// source order, located on the code view so a commented-out header is not one.
func findConstructs(src string) []construct {
	view := codeView(src)
	var out []construct
	for _, m := range terseHeader.FindAllStringSubmatchIndex(view, -1) {
		out = append(out, construct{
			kind: "automation", name: view[m[2]:m[3]], start: m[0] + strings.Index(view[m[0]:m[1]], "automation"),
			open: -1, close: m[1], terse: true,
			terseTrigger: src[m[4]:m[5]], terseLogic: view[m[6]:m[7]],
		})
	}
	for _, m := range constructHeader.FindAllStringSubmatchIndex(view, -1) {
		open := m[1] - 1
		close := matchClose(view, open)
		if close < 0 {
			continue // unbalanced: the parser reports it; nothing to rewrite
		}
		out = append(out, construct{
			kind: view[m[2]:m[3]], name: view[m[4]:m[5]], start: m[2], open: open, close: close,
		})
	}
	sortConstructs(out)
	return out
}

func sortConstructs(cs []construct) {
	for i := 1; i < len(cs); i++ {
		for j := i; j > 0 && cs[j].start < cs[j-1].start; j-- {
			cs[j], cs[j-1] = cs[j-1], cs[j]
		}
	}
}

var (
	stepBlockHeader  = regexp.MustCompile(`^step[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*\{`)
	precondHeader    = regexp.MustCompile(`^precondition[ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]*\{`)
	argsBlockHeader  = regexp.MustCompile(`^args[ \t]*\{`)
	bodyBlockHeader  = regexp.MustCompile(`^body[ \t]*\{`)
	forRangeHeader   = regexp.MustCompile(`^for[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]*:=[ \t]*range[ \t]+`)
	forEachHeader    = regexp.MustCompile(`^forEach[ \t]+([A-Za-z_][A-Za-z0-9_]*)[ \t]+in[ \t]+`)
	assignHeader     = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)[ \t]*:=[ \t]*`)
	retryWrapper     = regexp.MustCompile(`^retry[ \t]*\([ \t]*([0-9]+)[ \t]*\)[ \t]*`)
	namedArgHead     = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)([ \t]*:[ \t]*)`)
	onSurfaceTrailer = regexp.MustCompile(`^on[ \t]+surface[ \t]*\(`)
)

// isLegacyAutomation reports whether an automation body (the text between
// its braces) is in a retired form: it holds a `step` block.
func isLegacyAutomation(body string) bool {
	view := codeView(body)
	for _, line := range strings.Split(view, "\n") {
		if stepBlockHeader.MatchString(strings.TrimSpace(line)) {
			return true
		}
	}
	return false
}

// isLegacyLogic reports whether a logic body holds the retired `body { }`.
func isLegacyLogic(body string) bool {
	view := codeView(body)
	for _, line := range strings.Split(view, "\n") {
		if bodyBlockHeader.MatchString(strings.TrimSpace(line)) {
			return true
		}
	}
	return false
}

// ---- items at the top of a construct body -----------------------------------

// rawItem is a block the rewrite carries over verbatim: an args block or a
// precondition.
type rawItem struct {
	leading []string
	text    string // the block, from its keyword to its closing brace
}

// legacyBody is one construct's body as read.
type legacyBody struct {
	file      string
	kind      string
	name      string
	headerIdx int // offset of the construct keyword in the file
	args      *rawItem
	argFields []string // declared args field names, in order
	precond   []rawItem
	stmts     []*lstmt
	trailing  []string
}

// readConstructBody reads the body of a legacy automation or logic. src is
// the whole file; c locates the construct.
func readConstructBody(file, src string, c construct) (*legacyBody, error) {
	inner := src[c.open+1 : c.close]
	base := c.open + 1
	lb := &legacyBody{file: file, kind: c.kind, name: c.name, headerIdx: c.start}
	errAt := func(off int, format string, a ...any) error {
		return &bodyError{file: file, construct: c.kind + " " + c.name, line: lineAt(src, base+off), msg: fmt.Sprintf(format, a...)}
	}
	view := codeView(inner)
	pos := 0
	var leading []string
	for {
		next, lead := skipTrivia(inner, view, pos)
		leading = append(leading, lead...)
		pos = next
		if pos >= len(inner) {
			break
		}
		rest := view[pos:]
		switch {
		case argsBlockHeader.MatchString(rest):
			open := pos + strings.IndexByte(rest, '{')
			end := matchClose(view, open)
			if end < 0 {
				return nil, errAt(pos, "args block has no closing brace")
			}
			lb.args = &rawItem{leading: leading, text: inner[pos : end+1]}
			lb.argFields = argsFieldNames(inner[open+1 : end])
			leading = nil
			pos = end + 1
		case precondHeader.MatchString(rest):
			open := pos + strings.IndexByte(rest, '{')
			end := matchClose(view, open)
			if end < 0 {
				return nil, errAt(pos, "precondition block has no closing brace")
			}
			lb.precond = append(lb.precond, rawItem{leading: leading, text: inner[pos : end+1]})
			leading = nil
			pos = end + 1
		case c.kind == "automation" && stepBlockHeader.MatchString(rest):
			m := stepBlockHeader.FindStringSubmatch(rest)
			open := pos + len(m[0]) - 1
			end := matchClose(view, open)
			if end < 0 {
				return nil, errAt(pos, "step %q has no closing brace", m[1])
			}
			st, err := readStepBody(m[1], inner, view, open+1, end, func(off int, f string, a ...any) error { return errAt(off, f, a...) })
			if err != nil {
				return nil, err
			}
			st.leading = append(leading, st.leading...)
			st.line = lineAt(src, base+pos)
			lb.stmts = append(lb.stmts, st)
			leading = nil
			pos = end + 1
		case c.kind == "logic" && bodyBlockHeader.MatchString(rest):
			open := pos + strings.IndexByte(rest, '{')
			end := matchClose(view, open)
			if end < 0 {
				return nil, errAt(pos, "body block has no closing brace")
			}
			stmts, trailing, err := readStatements(inner, view, open+1, end, func(off int, f string, a ...any) error { return errAt(off, f, a...) }, src, base)
			if err != nil {
				return nil, err
			}
			if len(stmts) > 0 {
				stmts[0].leading = append(leading, stmts[0].leading...)
			} else {
				trailing = append(leading, trailing...)
			}
			leading = nil
			lb.stmts = append(lb.stmts, stmts...)
			lb.trailing = append(lb.trailing, trailing...)
			pos = end + 1
		default:
			return nil, errAt(pos, "unexpected %q in a retired %s body; the rewrite reads args, precondition and %s blocks", firstLine(inner[pos:]), c.kind, map[string]string{"automation": "step", "logic": "body"}[c.kind])
		}
	}
	lb.trailing = append(lb.trailing, leading...)
	return lb, nil
}

// argsFieldNames returns the field names an args block declares.
func argsFieldNames(block string) []string {
	var out []string
	view := codeView(block)
	lines := strings.Split(view, "\n")
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if id, _ := leadingIdent(t); id != "" {
			out = append(out, id)
		}
	}
	return out
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 60 {
		s = s[:60] + "..."
	}
	return s
}

// skipTrivia advances over whitespace and whole-line comments from pos,
// returning the new position and what it passed: each comment line trimmed,
// and "" for each run of blank lines -- including a run between the previous
// item and the first comment or the next item, so the emitter can keep the
// author's spacing. The remainder of the line pos starts on is not a blank
// line: it is the end of whatever came before.
func skipTrivia(raw, view string, pos int) (int, []string) {
	var lines []string
	pendingBlank := false
	first := true
	for pos < len(raw) {
		eol := strings.IndexByte(raw[pos:], '\n')
		lineEnd := len(raw)
		if eol >= 0 {
			lineEnd = pos + eol
		}
		vseg := view[pos:lineEnd]
		if strings.TrimSpace(vseg) != "" {
			if pendingBlank {
				lines = append(lines, "")
			}
			return pos + (len(vseg) - len(strings.TrimLeft(vseg, " \t"))), lines
		}
		if t := strings.TrimSpace(raw[pos:lineEnd]); t != "" {
			if pendingBlank {
				lines = append(lines, "")
			}
			pendingBlank = false
			lines = append(lines, t)
		} else if !first && eol >= 0 {
			// A whole empty line. The partial line a block ends on (the
			// indentation before its closing brace) is not one.
			pendingBlank = true
		}
		first = false
		if eol < 0 {
			break
		}
		pos = lineEnd + 1
	}
	if pendingBlank {
		lines = append(lines, "")
	}
	return len(raw), lines
}

// ---- steps ---------------------------------------------------------------------

type errFn func(off int, format string, a ...any) error

// readStepBody reads one `step <name> { ... }` block's contents (raw[from:to],
// offsets into raw) into the statement it becomes.
func readStepBody(name, raw, view string, from, to int, errAt errFn) (*lstmt, error) {
	pos, leading := skipTrivia(raw[:to], view[:to], from)
	if pos >= to {
		return nil, errAt(from, "step %q is empty", name)
	}
	// Trailing comments inside the step after its one item are kept as the
	// statement's trailing lines by the caller's emitter; read the item.
	st, end, err := readStepItem(raw, view, pos, to, errAt)
	if err != nil {
		return nil, err
	}
	end = pastLineComment(view, end, to)
	if rest, more := skipTrivia(raw[:to], view[:to], end); rest < to {
		return nil, errAt(rest, "step %q holds more than one item", name)
	} else if len(more) > 0 {
		// Comments after the item, inside the step's braces, follow the
		// statement the step becomes.
		st.after = append(st.after, more...)
	}
	st.leading = leading
	st.col = pos - lineStart(raw, pos)
	switch st.form {
	case formCall:
		st.name = name
	case formIfCall:
		st.name = name
	case formFor, formSwitch, formParallel:
		// A for, a switch and a parallel bind nothing: the step name labelled
		// a phase and has no statement to name.
	}
	return st, nil
}

// readStepItem reads one item at raw[pos:] (bounded by limit): a call, an
// `if <cond> { <call> }`, a forEach / for-range loop, a switch or a parallel.
// It returns the statement and the offset just past it.
func readStepItem(raw, view string, pos, limit int, errAt errFn) (*lstmt, int, error) {
	head, _ := leadingIdent(view[pos:limit])
	switch head {
	case "if":
		return readIfCall(raw, view, pos, limit, errAt)
	case "forEach", "for":
		return readLoop(raw, view, pos, limit, errAt, readStepItem)
	case "switch":
		return readSwitch(raw, view, pos, limit, errAt, readStepItem)
	case "parallel":
		return readParallel(raw, view, pos, limit, errAt)
	case "":
		return nil, 0, errAt(pos, "expected a call, if, forEach, switch or parallel, got %q", firstLine(raw[pos:limit]))
	}
	call, end, err := readCall(raw, view, pos, limit, errAt)
	if err != nil {
		return nil, 0, err
	}
	st := &lstmt{form: formCall, call: call}
	if call.name == "publishEvent" && call.kind == "" {
		pub, perr := publishFromCall(call)
		if perr != nil {
			return nil, 0, errAt(pos, "%v", perr)
		}
		st = pub
	}
	st.trailing = lineTrailingComment(raw, end, limit)
	return st, end, nil
}

// pastLineComment moves end over the rest of its line when that rest is only
// a comment (already captured as the item's trailing comment) and spaces, so
// the next trivia scan does not read the same comment again.
func pastLineComment(view string, end, limit int) int {
	eol := strings.IndexByte(view[end:limit], '\n')
	if eol < 0 {
		if strings.TrimSpace(view[end:limit]) == "" {
			return limit
		}
		return end
	}
	if strings.TrimSpace(view[end:end+eol]) == "" {
		return end + eol
	}
	return end
}

// lineTrailingComment returns a `//` comment that follows end on its line.
func lineTrailingComment(raw string, end, limit int) string {
	eol := strings.IndexByte(raw[end:limit], '\n')
	seg := raw[end:limit]
	if eol >= 0 {
		seg = seg[:eol]
	}
	t := strings.TrimSpace(seg)
	if strings.HasPrefix(t, "//") {
		return t
	}
	return ""
}

// readIfCall reads `if <cond> { <call> }` (the gated step) at pos.
func readIfCall(raw, view string, pos, limit int, errAt errFn) (*lstmt, int, error) {
	after := pos + len("if")
	brace := after + firstTopLevel(view[after:limit], '{')
	if brace < after {
		return nil, 0, errAt(pos, "if: expected `{` after the condition")
	}
	end := matchClose(view, brace)
	if end < 0 || end >= limit {
		return nil, 0, errAt(pos, "if: the body has no closing brace")
	}
	cond := strings.TrimSpace(raw[after:brace])
	ipos, _ := skipTrivia(raw[:end], view[:end], brace+1)
	inner, iend, err := readStepItem(raw, view, ipos, end, errAt)
	if err != nil {
		return nil, 0, err
	}
	if rest, _ := skipTrivia(raw[:end], view[:end], iend); rest < end {
		return nil, 0, errAt(rest, "if: a gated step holds one call")
	}
	switch inner.form {
	case formCall:
		return &lstmt{form: formIfCall, cond: cond, call: inner.call, trailing: lineTrailingComment(raw, end+1, limit)}, end + 1, nil
	case formPublish:
		// `if c { publishEvent(...) }`: a publish binds nothing, so the gate
		// becomes an if block around the publish statement.
		return &lstmt{form: formIf, branches: []lbranch{{cond: cond, body: []*lstmt{inner}}},
			trailing: lineTrailingComment(raw, end+1, limit)}, end + 1, nil
	case formParallel:
		return nil, 0, errAt(pos, "if: a gated parallel has no statement form the rewrite writes; wrap the parallel in an if by hand")
	}
	return nil, 0, errAt(pos, "if: a gated step holds one call, got %s", inner.form)
}

// itemReader reads one item of a block's body.
type itemReader func(raw, view string, pos, limit int, errAt errFn) (*lstmt, int, error)

// readLoop reads `forEach <v> in <src> [where <f>] { ... }` or
// `for <v> := range <src> [if <f>] { ... }`.
func readLoop(raw, view string, pos, limit int, errAt errFn, item itemReader) (*lstmt, int, error) {
	rest := view[pos:limit]
	var m []int
	filterWord := "if"
	if m = forEachHeader.FindStringSubmatchIndex(rest); m != nil {
		filterWord = "where"
	} else if m = forRangeHeader.FindStringSubmatchIndex(rest); m == nil {
		return nil, 0, errAt(pos, "expected `forEach <v> in <source>` or `for <v> := range <source>`")
	}
	loopVar := rest[m[2]:m[3]]
	hstart := pos + m[1]
	brace := hstart + firstTopLevel(view[hstart:limit], '{')
	if brace < hstart {
		return nil, 0, errAt(pos, "loop: expected `{` to open the body")
	}
	end := matchClose(view, brace)
	if end < 0 || end >= limit {
		return nil, 0, errAt(pos, "loop: the body has no closing brace")
	}
	header := raw[hstart:brace]
	hview := view[hstart:brace]
	source, filter := strings.TrimSpace(header), ""
	if i := indexWordTopLevel(hview, filterWord); i >= 0 {
		source = strings.TrimSpace(header[:i])
		filter = strings.TrimSpace(header[i+len(filterWord):])
	}
	body, trailing, err := readBlock(raw, view, brace+1, end, errAt, item)
	if err != nil {
		return nil, 0, err
	}
	return &lstmt{form: formFor, loopVar: loopVar, source: source, filter: filter, body: body, bodyTrailing: trailing}, end + 1, nil
}

// indexWordTopLevel returns the offset of the first whole word w at depth zero
// in view, or -1.
func indexWordTopLevel(view, w string) int {
	depth := 0
	for i := 0; i < len(view); i++ {
		switch view[i] {
		case '(', '[', '{':
			depth++
			continue
		case ')', ']', '}':
			depth--
			continue
		}
		if depth != 0 || !strings.HasPrefix(view[i:], w) {
			continue
		}
		if i > 0 && isIdentByte(view[i-1]) {
			continue
		}
		if j := i + len(w); j < len(view) && isIdentByte(view[j]) {
			continue
		}
		return i
	}
	return -1
}

// readBlock reads the items of a `{ ... }` body (raw[from:to]).
func readBlock(raw, view string, from, to int, errAt errFn, item itemReader) ([]*lstmt, []string, error) {
	var out []*lstmt
	pos := from
	var leading []string
	for {
		next, lead := skipTrivia(raw[:to], view[:to], pos)
		leading = append(leading, lead...)
		pos = next
		if pos >= to {
			break
		}
		st, end, err := item(raw, view, pos, to, errAt)
		if err != nil {
			return nil, nil, err
		}
		st.leading = append(leading, st.leading...)
		st.col = pos - lineStart(raw, pos)
		leading = nil
		out = append(out, st)
		pos = pastLineComment(view, end, to)
		// A comma between items (the parallel branches list) is trivia.
		for pos < to && (raw[pos] == ',' || raw[pos] == ' ' || raw[pos] == '\t') {
			pos++
		}
	}
	return out, leading, nil
}

// readSwitch reads `switch <subject> { case <labels> { ... } default { ... } }`.
func readSwitch(raw, view string, pos, limit int, errAt errFn, item itemReader) (*lstmt, int, error) {
	after := pos + len("switch")
	brace := after + firstTopLevel(view[after:limit], '{')
	if brace < after {
		return nil, 0, errAt(pos, "switch: expected `{` after the subject")
	}
	end := matchClose(view, brace)
	if end < 0 || end >= limit {
		return nil, 0, errAt(pos, "switch: no closing brace")
	}
	st := &lstmt{form: formSwitch, subject: strings.TrimSpace(raw[after:brace])}
	p := brace + 1
	var leading []string
	for {
		next, lead := skipTrivia(raw[:end], view[:end], p)
		leading = append(leading, lead...)
		p = next
		if p >= end {
			break
		}
		kw, _ := leadingIdent(view[p:end])
		var c lcase
		switch kw {
		case "case":
			ob := p + firstTopLevel(view[p:end], '{')
			if ob < p {
				return nil, 0, errAt(p, "switch: expected `{` after the case label")
			}
			labels := splitTopLevel(raw[p+len("case"):ob], view[p+len("case"):ob], ',')
			for _, l := range labels {
				c.labels = append(c.labels, strings.TrimSpace(l))
			}
			p = ob
		case "default":
			ob := p + len("default") + strings.IndexByte(view[p+len("default"):end], '{')
			c.isDefault = true
			p = ob
		default:
			return nil, 0, errAt(p, "switch: expected `case` or `default`, got %q", firstLine(raw[p:end]))
		}
		ce := matchClose(view, p)
		if ce < 0 || ce > end {
			return nil, 0, errAt(p, "switch: a case has no closing brace")
		}
		body, trailing, err := readBlock(raw, view, p+1, ce, errAt, item)
		if err != nil {
			return nil, 0, err
		}
		c.leading, c.body, c.trailing = leading, body, trailing
		leading = nil
		st.cases = append(st.cases, c)
		p = ce + 1
	}
	st.bodyTrailing = leading
	if len(st.cases) == 0 {
		return nil, 0, errAt(pos, "switch: no case")
	}
	return st, end + 1, nil
}

// readParallel reads the procedural `parallel { wait: "...", failFast: ...,
// branches: [ step <name> { ... }, ... ] }` config.
func readParallel(raw, view string, pos, limit int, errAt errFn) (*lstmt, int, error) {
	brace := pos + strings.IndexByte(view[pos:limit], '{')
	if brace < pos {
		return nil, 0, errAt(pos, "parallel: expected `{`")
	}
	end := matchClose(view, brace)
	if end < 0 || end >= limit {
		return nil, 0, errAt(pos, "parallel: no closing brace")
	}
	st := &lstmt{form: formParallel, wait: "all"}
	p := brace + 1
	for {
		p, _ = skipTrivia(raw[:end], view[:end], p)
		for p < end && (raw[p] == ',' || raw[p] == ' ' || raw[p] == '\t' || raw[p] == '\n') {
			p++
		}
		if p >= end {
			break
		}
		key, rest := leadingIdent(view[p:end])
		colon := strings.IndexByte(rest, ':')
		if key == "" || colon < 0 || strings.TrimSpace(rest[:colon]) != "" {
			return nil, 0, errAt(p, "parallel: expected `wait:`, `failFast:` or `branches:`")
		}
		v := p + len(key) + colon + 1
		for v < end && (raw[v] == ' ' || raw[v] == '\t') {
			v++
		}
		switch key {
		case "wait":
			q, err := strconv.Unquote(strings.TrimSpace(raw[v : v+strings.IndexAny(raw[v+1:end], `"`)+2]))
			if err != nil {
				return nil, 0, errAt(v, "parallel: wait takes a quoted value")
			}
			if q != "all" && q != "any" {
				return nil, 0, errAt(v, "parallel: wait %q has no statement form (write all or any by hand)", q)
			}
			st.wait = q
			p = v + len(strconv.Quote(q))
		case "failFast":
			w, _ := leadingIdent(view[v:end])
			st.failFast = w == "true"
			p = v + len(w)
		case "branches":
			if v >= end || raw[v] != '[' {
				return nil, 0, errAt(v, "parallel: branches takes a list")
			}
			le := matchClose(view, v)
			if le < 0 || le > end {
				return nil, 0, errAt(v, "parallel: the branches list has no closing bracket")
			}
			branches, _, err := readBlock(raw, view, v+1, le, errAt, readBranchStep)
			if err != nil {
				return nil, 0, err
			}
			st.par = branches
			p = le + 1
		default:
			return nil, 0, errAt(p, "parallel: unknown key %q", key)
		}
	}
	if len(st.par) == 0 {
		return nil, 0, errAt(pos, "parallel: no branches")
	}
	return st, end + 1, nil
}

// readBranchStep reads one `step <name> { ... }` inside a parallel's branches.
func readBranchStep(raw, view string, pos, limit int, errAt errFn) (*lstmt, int, error) {
	m := stepBlockHeader.FindStringSubmatch(view[pos:limit])
	if m == nil {
		return nil, 0, errAt(pos, "parallel: each branch is `step <name> { ... }`")
	}
	open := pos + len(m[0]) - 1
	end := matchClose(view, open)
	if end < 0 || end >= limit {
		return nil, 0, errAt(pos, "parallel: branch %q has no closing brace", m[1])
	}
	st, err := readStepBody(m[1], raw, view, open+1, end, errAt)
	if err != nil {
		return nil, 0, err
	}
	// The branch label carries the step's name; the statement inside the
	// branch binds nothing a later statement could read (a branch's names
	// stay inside it).
	st.name = ""
	branch := &lstmt{form: "branch", name: m[1], body: []*lstmt{st}}
	return branch, end + 1, nil
}

// ---- calls -------------------------------------------------------------------

var kindWords = map[string]string{
	"logic": "logic", "mutate": "mutation", "mutation": "mutation", "query": "query",
	"builtin": "builtin", "automation": "automation", "action": "action",
}

// readCall reads one construct call at pos: `[kind] <name>(...)`,
// `[kind] <name> { ... }`, or any spelling of an action, with an optional
// `on surface(...)`. It returns the call and the offset just past it.
func readCall(raw, view string, pos, limit int, errAt errFn) (*lcall, int, error) {
	call := &lcall{}
	p := pos
	w, _ := leadingIdent(view[p:limit])
	if k, ok := kindWords[w]; ok {
		call.kind = k
		p += len(w)
		for p < limit && (raw[p] == ' ' || raw[p] == '\t') {
			p++
		}
	}
	if call.kind == "action" {
		return readActionCall(raw, view, pos, p, limit, call, errAt)
	}
	name, _ := leadingIdent(view[p:limit])
	if name == "" {
		return nil, 0, errAt(p, "expected a construct name, got %q", firstLine(raw[p:limit]))
	}
	call.name = name
	p += len(name)
	for p < limit && (raw[p] == ' ' || raw[p] == '\t') {
		p++
	}
	if p >= limit || (raw[p] != '(' && raw[p] != '{') {
		return nil, 0, errAt(p, "call %s: expected `(` or `{` after the name", name)
	}
	end := matchClose(view, p)
	if end < 0 || end >= limit {
		return nil, 0, errAt(p, "call %s: no closing bracket", name)
	}
	args, multi, err := readArgs(raw[p+1:end], view[p+1:end], raw[p] == '{')
	if err != nil {
		return nil, 0, errAt(p, "call %s: %v", name, err)
	}
	call.args, call.multiline = args, multi
	q := end + 1
	if s, e, ok := readOnSurface(raw, view, q, limit); ok {
		call.surface, q = s, e
	}
	call.raw = raw[pos:q]
	return call, q, nil
}

// readOnSurface reads an optional ` on surface("<name>")` at q.
func readOnSurface(raw, view string, q, limit int) (string, int, bool) {
	p := q
	for p < limit && (raw[p] == ' ' || raw[p] == '\t') {
		p++
	}
	if !onSurfaceTrailer.MatchString(view[p:limit]) {
		return "", q, false
	}
	open := p + strings.IndexByte(view[p:limit], '(')
	end := matchClose(view, open)
	if end < 0 || end >= limit {
		return "", q, false
	}
	return strings.TrimSpace(raw[open+1 : end]), end + 1, true
}

// readActionCall reads the three action spellings: `action <name>(...)`,
// `action("<ref>") { args { ... } }`, and `action { ref: ..., args: ..., surface: ... }`.
func readActionCall(raw, view string, start, p, limit int, call *lcall, errAt errFn) (*lcall, int, error) {
	switch {
	case p < limit && isIdentStartByte(raw[p]):
		name, _ := leadingIdent(view[p:limit])
		call.name = name
		p += len(name)
		for p < limit && (raw[p] == ' ' || raw[p] == '\t') {
			p++
		}
		if p >= limit || raw[p] != '(' {
			return nil, 0, errAt(p, "action %s: expected `(`", name)
		}
		end := matchClose(view, p)
		if end < 0 || end >= limit {
			return nil, 0, errAt(p, "action %s: no closing paren", name)
		}
		args, multi, err := readArgs(raw[p+1:end], view[p+1:end], false)
		if err != nil {
			return nil, 0, errAt(p, "action %s: %v", name, err)
		}
		call.args, call.multiline = args, multi
		q := end + 1
		if s, e, ok := readOnSurface(raw, view, q, limit); ok {
			call.surface, q = s, e
		}
		return call, q, nil
	case p < limit && raw[p] == '(':
		end := matchClose(view, p)
		if end < 0 || end >= limit {
			return nil, 0, errAt(p, "action: no closing paren after the ref")
		}
		ref, err := strconv.Unquote(strings.TrimSpace(raw[p+1 : end]))
		if err != nil {
			return nil, 0, errAt(p, "action: the ref is not a quoted string")
		}
		if err := setActionRef(call, ref); err != nil {
			return nil, 0, errAt(p, "%v", err)
		}
		q := end + 1
		for q < limit && (raw[q] == ' ' || raw[q] == '\t' || raw[q] == '\n') {
			q++
		}
		if q < limit && raw[q] == '{' {
			be := matchClose(view, q)
			if be < 0 || be >= limit {
				return nil, 0, errAt(q, "action: the body has no closing brace")
			}
			ip, _ := skipTrivia(raw[:be], view[:be], q+1)
			if ip < be {
				if !argsBlockHeader.MatchString(view[ip:be]) {
					return nil, 0, errAt(ip, "action: expected `args { ... }` in the body")
				}
				ao := ip + strings.IndexByte(view[ip:be], '{')
				ae := matchClose(view, ao)
				args, multi, err := readArgs(raw[ao+1:ae], view[ao+1:ae], true)
				if err != nil {
					return nil, 0, errAt(ao, "action: %v", err)
				}
				call.args, call.multiline = args, multi
			}
			q = be + 1
		}
		if s, e, ok := readOnSurface(raw, view, q, limit); ok {
			call.surface, q = s, e
		}
		return call, q, nil
	case p < limit && raw[p] == '{':
		end := matchClose(view, p)
		if end < 0 || end >= limit {
			return nil, 0, errAt(p, "action: the config has no closing brace")
		}
		entries, _, err := readArgs(raw[p+1:end], view[p+1:end], true)
		if err != nil {
			return nil, 0, errAt(p, "action: %v", err)
		}
		for _, e := range entries {
			switch e.name {
			case "ref":
				ref, uerr := strconv.Unquote(strings.TrimSpace(e.value))
				if uerr != nil {
					return nil, 0, errAt(p, "action: ref is not a quoted string")
				}
				if err := setActionRef(call, ref); err != nil {
					return nil, 0, errAt(p, "%v", err)
				}
			case "args":
				v := strings.TrimSpace(e.value)
				if !strings.HasPrefix(v, "{") || !strings.HasSuffix(v, "}") {
					return nil, 0, errAt(p, "action: args is not an object")
				}
				inner := v[1 : len(v)-1]
				args, multi, aerr := readArgs(inner, codeView(inner), true)
				if aerr != nil {
					return nil, 0, errAt(p, "action: %v", aerr)
				}
				call.args, call.multiline = args, multi
			case "surface":
				call.surface = strings.TrimSpace(e.value)
			default:
				return nil, 0, errAt(p, "action: unknown key %q", e.name)
			}
		}
		if call.name == "" {
			return nil, 0, errAt(p, "action: no ref")
		}
		return call, end + 1, nil
	}
	return nil, 0, errAt(start, "action: expected `<name>(...)`, `(\"<ref>\")` or `{ ref: ... }`")
}

// setActionRef takes an action ref. A pinned `id@version` has no statement
// spelling -- an action statement names the action, which floats to its
// latest version -- so it is refused rather than silently unpinned.
func setActionRef(call *lcall, ref string) error {
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		return fmt.Errorf("action %q pins a version the statement form cannot spell; write `action %s(...)` by hand if the latest version is meant, or keep the pin in the action library", ref, ref[:i])
	}
	if !isBareIdent(ref) {
		return fmt.Errorf("action ref %q is not a name", ref)
	}
	call.name = ref
	return nil
}

// readArgs reads a call's argument list (the text inside its brackets).
// braced is true for the `name { ... }` form, whose entries may also be
// separated by newlines alone.
func readArgs(raw, view string, braced bool) ([]larg, bool, error) {
	if strings.TrimSpace(view) == "" {
		// Only comments, or nothing.
		return nil, false, nil
	}
	multi := strings.Contains(strings.TrimSpace(raw), "\n")
	pieces := splitTopLevel(raw, view, ',')
	var out []larg
	var carry string // a same-line comment that followed a comma: it belongs to the previous arg
	for _, piece := range pieces {
		if carry != "" && len(out) > 0 {
			out[len(out)-1].trailing = carry
			carry = ""
		}
		// A comment on the same line right after the comma.
		if nl := strings.IndexByte(piece, '\n'); nl >= 0 {
			if first := strings.TrimSpace(piece[:nl]); strings.HasPrefix(first, "//") && len(out) > 0 {
				out[len(out)-1].trailing = first
				piece = piece[nl+1:]
			}
		}
		for _, sub := range splitBracedEntries(piece, braced) {
			a, ok, err := readOneArg(sub)
			if err != nil {
				return nil, false, err
			}
			if !ok {
				// A piece with nothing but comments: they trail the list.
				if len(out) > 0 {
					for _, l := range commentLines(sub) {
						if out[len(out)-1].trailing == "" {
							out[len(out)-1].trailing = l
						} else {
							out[len(out)-1].trailing += " " + l
						}
					}
				}
				continue
			}
			out = append(out, a)
		}
	}
	return out, multi, nil
}

// splitBracedEntries splits a braced call's piece further at line starts that
// begin a new `name:` entry, for the brace form's newline-separated entries.
func splitBracedEntries(piece string, braced bool) []string {
	if !braced {
		return []string{piece}
	}
	view := codeView(piece)
	lines := strings.SplitAfter(piece, "\n")
	vlines := strings.SplitAfter(view, "\n")
	var out []string
	var cur strings.Builder
	depth := 0
	seenEntry := false
	for i, l := range lines {
		vl := vlines[i]
		t := strings.TrimSpace(vl)
		if depth == 0 && namedArgHead.MatchString(t) && seenEntry {
			out = append(out, cur.String())
			cur.Reset()
		}
		if depth == 0 && namedArgHead.MatchString(t) {
			seenEntry = true
		}
		cur.WriteString(l)
		for j := 0; j < len(vl); j++ {
			switch vl[j] {
			case '(', '[', '{':
				depth++
			case ')', ']', '}':
				depth--
			}
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// commentLines returns the comment lines of s, trimmed.
func commentLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); strings.HasPrefix(t, "//") {
			out = append(out, t)
		}
	}
	return out
}

// readOneArg reads one argument piece: leading comment lines, then `name: value`
// or a positional value, then an optional same-line comment.
func readOneArg(piece string) (larg, bool, error) {
	var a larg
	lines := strings.Split(piece, "\n")
	i := 0
	for ; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if t == "" {
			continue
		}
		if strings.HasPrefix(t, "//") {
			a.leading = append(a.leading, t)
			continue
		}
		break
	}
	if i >= len(lines) {
		return a, false, nil
	}
	a.col = -1
	if i > 0 {
		a.col = len(strings.ReplaceAll(indentOf(lines[i]), "\t", "    "))
	}
	text := strings.Join(lines[i:], "\n")
	text = strings.TrimRight(text, " \t\n")
	// A trailing `//` on the last line.
	lastNL := strings.LastIndexByte(text, '\n')
	last := text[lastNL+1:]
	if code, c := trailingComment(last); c != "" {
		a.trailing = c
		text = text[:lastNL+1] + code
	}
	text = strings.TrimLeft(text, " \t")
	if m := namedArgHead.FindStringSubmatch(text); m != nil && !strings.HasPrefix(text[len(m[1]):], ":=") {
		a.name, a.sep = m[1], m[2]
		a.value = strings.TrimRight(text[len(m[0]):], " \t\n")
	} else {
		a.value = strings.TrimRight(text, " \t\n")
	}
	return a, true, nil
}

// publishFromCall turns `publishEvent(topic: "t", payload: {...})` into the
// publish statement. The topic must be a literal: a publish statement's topic
// is known at load.
func publishFromCall(call *lcall) (*lstmt, error) {
	st := &lstmt{form: formPublish}
	for _, a := range call.args {
		switch a.name {
		case "topic":
			v := strings.TrimSpace(a.value)
			if _, err := strconv.Unquote(v); err != nil || !strings.HasPrefix(v, `"`) {
				return nil, fmt.Errorf("publishEvent: the topic %s is not a string literal; a publish statement's topic is known at load", v)
			}
			st.topic = v
		case "payload":
			v := strings.TrimSpace(a.value)
			if !strings.HasPrefix(v, "{") {
				return nil, fmt.Errorf("publishEvent: the payload %s is not a map literal; a publish statement's payload is written { k: v }", v)
			}
			st.payload = v
			st.payloadCol = a.col
		case "kind":
			return nil, fmt.Errorf("publishEvent: `kind:` has no publish-statement spelling; drop it by hand if the default kind is meant")
		default:
			return nil, fmt.Errorf("publishEvent: unexpected argument %q", a.name)
		}
	}
	if st.topic == "" {
		return nil, fmt.Errorf("publishEvent: no topic")
	}
	if st.payload == "" {
		st.payload = "{}"
	}
	return st, nil
}

// ---- logic statements ------------------------------------------------------

// readStatements reads a logic `body { ... }`'s statements (raw[from:to]).
// src and base let errors and lines point into the file.
func readStatements(raw, view string, from, to int, errAt errFn, src string, base int) ([]*lstmt, []string, error) {
	var out []*lstmt
	pos := from
	var leading []string
	for {
		next, lead := skipTrivia(raw[:to], view[:to], pos)
		leading = append(leading, lead...)
		pos = next
		if pos >= to {
			break
		}
		end := statementEnd(view, pos, to)
		st, err := readLogicStatement(raw, view, pos, end, errAt, src, base)
		if err != nil {
			return nil, nil, err
		}
		st.leading = append(leading, st.leading...)
		st.line = lineAt(src, base+pos)
		st.col = pos - lineStart(raw, pos)
		leading = nil
		out = append(out, st)
		pos = end
	}
	return out, leading, nil
}

// statementEnd returns the offset just past the statement starting at pos:
// the end of the line on which the bracket depth returns to zero, unless the
// statement continues (a trailing operator, or a next line that begins with
// one, `}` of a block, or `else`).
func statementEnd(view string, pos, to int) int {
	depth := 0
	i := pos
	for i < to {
		c := view[i]
		switch c {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case '\n':
			if depth > 0 {
				break
			}
			line := view[lineStart(view, i):i]
			if continuesAtLineEnd(line) {
				break
			}
			// Peek at the next non-blank line.
			j := i + 1
			for j < to {
				eol := strings.IndexByte(view[j:to], '\n')
				nl := view[j:to]
				if eol >= 0 {
					nl = view[j : j+eol]
				}
				if strings.TrimSpace(nl) != "" {
					if continuesAtLineStart(nl) && !strings.HasPrefix(strings.TrimSpace(nl), "}") {
						goto next
					}
					break
				}
				if eol < 0 {
					break
				}
				j += eol + 1
			}
			return i
		}
	next:
		i++
	}
	return to
}

func lineStart(s string, i int) int {
	if j := strings.LastIndexByte(s[:i], '\n'); j >= 0 {
		return j + 1
	}
	return 0
}

// readLogicStatement reads one logic statement raw[pos:end].
func readLogicStatement(raw, view string, pos, end int, errAt errFn, src string, base int) (*lstmt, error) {
	text := raw[pos:end]
	vtext := view[pos:end]
	head, _ := leadingIdent(vtext)
	switch head {
	case "return":
		st := &lstmt{form: formReturn}
		body := strings.TrimSpace(text[len("return"):])
		if code, c := trailingCommentIfLast(body); c != "" {
			body, st.trailing = code, c
		}
		st.expr = body
		return st, nil
	case "for":
		st, _, err := readLoop(raw, view, pos, end, errAt, func(r, v string, p, l int, e errFn) (*lstmt, int, error) {
			se := statementEnd(v, p, l)
			s, err := readLogicStatement(r, v, p, se, e, src, base)
			if s != nil {
				s.line = lineAt(src, base+p)
			}
			return s, se, err
		})
		return st, err
	case "if":
		return readLogicIf(raw, view, pos, end, errAt, src, base)
	case "switch":
		st, _, err := readSwitch(raw, view, pos, end, errAt, func(r, v string, p, l int, e errFn) (*lstmt, int, error) {
			se := statementEnd(v, p, l)
			s, err := readLogicStatement(r, v, p, se, e, src, base)
			if s != nil {
				s.line = lineAt(src, base+p)
			}
			return s, se, err
		})
		return st, err
	}
	if m := assignHeader.FindStringSubmatchIndex(vtext); m != nil {
		name := vtext[m[2]:m[3]]
		rhsPos := pos + m[1]
		rview := view[rhsPos:end]
		if w, _ := leadingIdent(rview); w == "if" {
			st, _, err := readIfCall(raw, view, rhsPos, end, errAt)
			if err != nil {
				return nil, err
			}
			if st.form == formIfCall {
				st.name = name
			} else {
				// A publish binds nothing; the name it had is recorded so a
				// read of it can be refused.
				st.branches[0].body[0].name = name
			}
			return st, nil
		}
		retry := 0
		if rm := retryWrapper.FindStringSubmatchIndex(rview); rm != nil {
			retry, _ = strconv.Atoi(rview[rm[2]:rm[3]])
			rhsPos += rm[1]
			rview = view[rhsPos:end]
		}
		if isCallShaped(rview) {
			call, cend, err := readCall(raw, view, rhsPos, end, errAt)
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(view[cend:end]) == "" {
				if call.name == "publishEvent" && call.kind == "" {
					pub, perr := publishFromCall(call)
					if perr != nil {
						return nil, errAt(pos, "%v", perr)
					}
					pub.name = name
					pub.trailing = lineTrailingComment(raw, cend, end)
					return pub, nil
				}
				st := &lstmt{form: formCall, name: name, call: call, retry: retry, trailing: lineTrailingComment(raw, cend, end)}
				return st, nil
			}
		}
		if retry > 0 {
			return nil, errAt(pos, "retry(n) wraps a construct call")
		}
		body := strings.TrimSpace(raw[pos+m[1] : end])
		st := &lstmt{form: formExpr, name: name}
		if code, c := trailingCommentIfLast(body); c != "" {
			body, st.trailing = code, c
		}
		st.expr = body
		return st, nil
	}
	if isCallShaped(vtext) {
		call, cend, err := readCall(raw, view, pos, end, errAt)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(view[cend:end]) == "" {
			if call.name == "publishEvent" && call.kind == "" {
				pub, perr := publishFromCall(call)
				if perr != nil {
					return nil, errAt(pos, "%v", perr)
				}
				pub.trailing = lineTrailingComment(raw, cend, end)
				return pub, nil
			}
			return &lstmt{form: formCall, call: call, trailing: lineTrailingComment(raw, cend, end)}, nil
		}
	}
	return nil, errAt(pos, "unrecognised logic statement %q", firstLine(text))
}

// trailingCommentIfLast splits a trailing `//` comment off the last line of s.
func trailingCommentIfLast(s string) (string, string) {
	nl := strings.LastIndexByte(s, '\n')
	last := s[nl+1:]
	code, c := trailingComment(last)
	if c == "" {
		return s, ""
	}
	return strings.TrimRight(s[:nl+1]+code, " \t"), c
}

// isCallShaped reports whether view starts with `[kind] <name>(` or
// `[kind] <name> {` -- the shape of a construct call.
func isCallShaped(view string) bool {
	v := strings.TrimLeft(view, " \t")
	w, rest := leadingIdent(v)
	if w == "" {
		return false
	}
	if _, ok := kindWords[w]; ok {
		rest = strings.TrimLeft(rest, " \t")
		if w == "action" && rest != "" && (rest[0] == '(' || rest[0] == '{') {
			return true
		}
		n, r2 := leadingIdent(rest)
		if n == "" {
			return false
		}
		r2 = strings.TrimLeft(r2, " \t")
		return r2 != "" && (r2[0] == '(' || r2[0] == '{')
	}
	rest = strings.TrimLeft(rest, " \t")
	return rest != "" && (rest[0] == '(' || rest[0] == '{')
}

// readLogicIf reads a logic `if <cond> { stmts } [else if ... | else { ... }]`.
func readLogicIf(raw, view string, pos, end int, errAt errFn, src string, base int) (*lstmt, error) {
	st := &lstmt{form: formIf}
	p := pos
	for {
		w, _ := leadingIdent(view[p:end])
		if w != "if" {
			return nil, errAt(p, "expected `if`")
		}
		after := p + len("if")
		brace := after + firstTopLevel(view[after:end], '{')
		if brace < after {
			return nil, errAt(p, "if: expected `{`")
		}
		be := matchClose(view, brace)
		if be < 0 || be >= end {
			return nil, errAt(p, "if: no closing brace")
		}
		body, trailing, err := readStatements(raw, view, brace+1, be, errAt, src, base)
		if err != nil {
			return nil, err
		}
		st.branches = append(st.branches, lbranch{cond: strings.TrimSpace(raw[after:brace]), body: body, trailing: trailing})
		q := be + 1
		for q < end && (raw[q] == ' ' || raw[q] == '\t' || raw[q] == '\n') {
			q++
		}
		if w, _ := leadingIdent(view[q:end]); w != "else" {
			return st, nil
		}
		q += len("else")
		for q < end && (raw[q] == ' ' || raw[q] == '\t' || raw[q] == '\n') {
			q++
		}
		if w, _ := leadingIdent(view[q:end]); w == "if" {
			p = q
			continue
		}
		if q >= end || raw[q] != '{' {
			return nil, errAt(q, "else: expected `{` or `if`")
		}
		ee := matchClose(view, q)
		if ee < 0 || ee > end {
			return nil, errAt(q, "else: no closing brace")
		}
		body, trailing, err = readStatements(raw, view, q+1, ee, errAt, src, base)
		if err != nil {
			return nil, err
		}
		st.branches = append(st.branches, lbranch{body: body, trailing: trailing})
		return st, nil
	}
}
