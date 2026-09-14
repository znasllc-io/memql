package bodymigrate

// emit.go -- writing a read body back as edition-2026 statements (epic
// memql#5370, task memql#5373).
//
// Every call gets its kind, every expression passes through the reference
// rewrite, and every comment the reader attached is written back where it was:
// above its statement, above its argument, or at the end of its line. Values
// and conditions keep the author's own text; only their continuation lines are
// re-indented, and never inside a string literal.

import (
	"fmt"
	"strconv"
	"strings"
)

// emitter writes statements into lines.
type emitter struct {
	ix    *Index
	lines []string
	// inLogic makes a publish an error: a logic that publishes is inlined
	// into its automation before it is written, or refused.
	inLogic bool
}

func (e *emitter) add(s string) { e.lines = append(e.lines, s) }

// comments writes comment lines at indent; "" is a blank line.
func (e *emitter) comments(lines []string, indent string) {
	for _, l := range lines {
		if l == "" {
			e.add("")
			continue
		}
		e.add(indent + l)
	}
}

// multi writes text whose first line follows prefix, shifting its
// continuation lines from origCol to newCol.
func (e *emitter) multi(prefix, text string, origCol, newCol int, suffix string) {
	text = reindent(text, origCol, newCol)
	parts := strings.Split(text, "\n")
	parts[0] = prefix + parts[0]
	parts[len(parts)-1] += suffix
	e.lines = append(e.lines, parts...)
}

func indentStr(depth int) string { return strings.Repeat("  ", depth) }

// statements writes stmts at depth, in order, with the plan's comments.
func (e *emitter) statements(stmts []*lstmt, order []int, extra map[int][]string, depth int, rc *refContext) error {
	if order == nil {
		order = make([]int, len(stmts))
		for i := range order {
			order[i] = i
		}
	}
	for _, i := range order {
		if err := e.statement(stmts[i], depth, rc, extra[i]); err != nil {
			return err
		}
	}
	return nil
}

// withTrailing appends a `//` comment to a line.
func withTrailing(line, comment string) string {
	if comment == "" {
		return line
	}
	return line + " " + comment
}

// statement writes one statement.
func (e *emitter) statement(st *lstmt, depth int, rc *refContext, extra []string) error {
	ind := indentStr(depth)
	e.comments(st.leading, ind)
	e.comments(extra, ind)
	switch st.form {
	case formCall:
		if st.call.kind == "" && exprFunctions[st.call.name] {
			// A bare call to an expression function is a value, not a call.
			if st.name == "" {
				return fmt.Errorf("%s(...) as a statement does nothing: bind it or drop it", st.call.name)
			}
			return e.expr(&lstmt{name: st.name, expr: st.call.raw, col: st.col, trailing: st.trailing}, depth, rc)
		}
		head := ind
		if st.name != "" {
			head += st.name + " := "
		}
		mods := ""
		if st.retry > 0 {
			mods = " retry(" + strconv.Itoa(st.retry) + ")"
		}
		if err := e.call(head, st.call, depth, rc, mods, st.trailing, st.col); err != nil {
			return err
		}
	case formIfCall:
		cond, err := rewriteRefs(st.cond, rc)
		if err != nil {
			return err
		}
		e.multi(ind+"if ", cond, st.col, len(ind), " {")
		head := indentStr(depth + 1)
		if st.name != "" {
			head += st.name + " := "
		}
		if err := e.call(head, st.call, depth+1, rc, "", "", st.col+2); err != nil {
			return err
		}
		e.add(withTrailing(ind+"}", st.trailing))
	case formIf:
		for k, br := range st.branches {
			switch {
			case k == 0:
				cond, err := rewriteRefs(br.cond, rc)
				if err != nil {
					return err
				}
				e.multi(ind+"if ", cond, st.col, len(ind), " {")
			case br.cond != "":
				cond, err := rewriteRefs(br.cond, rc)
				if err != nil {
					return err
				}
				e.multi(ind+"} else if ", cond, st.col, len(ind), " {")
			default:
				e.add(ind + "} else {")
			}
			if err := e.statements(br.body, nil, nil, depth+1, rc); err != nil {
				return err
			}
			e.comments(br.trailing, indentStr(depth+1))
		}
		e.add(withTrailing(ind+"}", st.trailing))
	case formFor:
		source, err := rewriteRefs(st.source, rc)
		if err != nil {
			return err
		}
		inner := rc.withLoopVar(st.loopVar)
		head := "for " + st.loopVar + " in " + source
		if st.filter != "" {
			filter, err := rewriteRefs(st.filter, inner)
			if err != nil {
				return err
			}
			head += " if " + filter
		}
		e.multi(ind, head, st.col, len(ind), " {")
		if err := e.statements(st.body, nil, nil, depth+1, inner); err != nil {
			return err
		}
		e.comments(st.bodyTrailing, indentStr(depth+1))
		e.add(withTrailing(ind+"}", st.trailing))
	case formSwitch:
		subject, err := rewriteRefs(st.subject, rc)
		if err != nil {
			return err
		}
		e.multi(ind+"switch ", subject, st.col, len(ind), " {")
		cind := indentStr(depth + 1)
		for _, c := range st.cases {
			e.comments(c.leading, cind)
			head := "default"
			if !c.isDefault {
				head = "case " + strings.Join(c.labels, ", ")
			}
			if len(c.body) == 0 && len(c.trailing) == 0 {
				e.add(cind + head + " { }")
				continue
			}
			e.add(cind + head + " {")
			if err := e.statements(c.body, nil, nil, depth+2, rc); err != nil {
				return err
			}
			e.comments(c.trailing, indentStr(depth+2))
			e.add(cind + "}")
		}
		e.comments(st.bodyTrailing, cind)
		e.add(withTrailing(ind+"}", st.trailing))
	case formParallel:
		if !st.failFast {
			e.add(ind + "// memqlmigrate: the parallel's failFast: false has no statement spelling; a failed branch now stops the others " + orderIssue)
		}
		e.add(ind + "parallel {")
		bind := indentStr(depth + 1)
		for _, br := range st.par {
			e.comments(br.leading, bind)
			e.add(bind + "branch " + br.name + " {")
			if err := e.statements(br.body, nil, nil, depth+2, rc); err != nil {
				return err
			}
			e.add(bind + "}")
		}
		tail := "}"
		if st.wait == "any" {
			tail += " wait any"
		}
		e.add(withTrailing(ind+tail, st.trailing))
	case formExpr:
		if err := e.expr(st, depth, rc); err != nil {
			return err
		}
	case formReturn:
		if st.expr == "" {
			e.add(withTrailing(ind+"return", st.trailing))
			break
		}
		if call := asConstructCall(st.expr); call != nil {
			if err := e.call(ind+"return ", call, depth, rc, "", st.trailing, st.col); err != nil {
				return err
			}
			break
		}
		v, err := rewriteRefs(st.expr, rc)
		if err != nil {
			return err
		}
		e.multi(ind+"return ", v, st.col, len(ind), "")
		e.lines[len(e.lines)-1] = withTrailing(e.lines[len(e.lines)-1], st.trailing)
	case formPublish:
		if e.inLogic {
			return fmt.Errorf("publish %s in a logic: a logic may not publish (D14) and nothing inlined it", st.topic)
		}
		payload, err := rewriteRefs(st.payload, rc)
		if err != nil {
			return err
		}
		from := st.payloadCol
		if from < 0 {
			from = st.col
		}
		e.multi(ind+"publish "+st.topic+" ", payload, from, len(ind), "")
		e.lines[len(e.lines)-1] = withTrailing(e.lines[len(e.lines)-1], st.trailing)
	case "branch":
		return fmt.Errorf("a parallel branch outside a parallel")
	default:
		return fmt.Errorf("statement form %q has no writer", st.form)
	}
	e.comments(st.after, ind)
	return nil
}

// expr writes `name := <expression>`.
func (e *emitter) expr(st *lstmt, depth int, rc *refContext) error {
	ind := indentStr(depth)
	v, err := rewriteRefs(st.expr, rc)
	if err != nil {
		return err
	}
	e.multi(ind+st.name+" := ", v, st.col, len(ind), "")
	e.lines[len(e.lines)-1] = withTrailing(e.lines[len(e.lines)-1], st.trailing)
	return nil
}

// asConstructCall returns the call a whole expression is, or nil: a return's
// value that is a construct call takes its kind like any other call.
func asConstructCall(expr string) *lcall {
	view := codeView(expr)
	if !isCallShaped(view) {
		return nil
	}
	call, end, err := readCall(expr, view, 0, len(expr), func(off int, f string, a ...any) error {
		return fmt.Errorf(f, a...)
	})
	if err != nil || strings.TrimSpace(view[end:]) != "" {
		return nil
	}
	if call.kind == "" && exprFunctions[call.name] {
		return nil
	}
	return call
}

// resolveKind returns the kind a call is written with.
func (e *emitter) resolveKind(c *lcall) (string, error) {
	if c.kind != "" {
		return c.kind, nil
	}
	return e.ix.kindOf(c.name)
}

// call writes `<head><kind> <name>(<args>)<surface><mods><trailing>`.
func (e *emitter) call(head string, c *lcall, depth int, rc *refContext, mods, trailing string, col int) error {
	kind, err := e.resolveKind(c)
	if err != nil {
		return err
	}
	args := make([]larg, len(c.args))
	for i, a := range c.args {
		v, err := rewriteRefs(a.value, rc)
		if err != nil {
			return fmt.Errorf("%s %s: argument %s: %w", kind, c.name, argLabel(a), err)
		}
		if a.name == "" {
			pun := strings.TrimSpace(a.value)
			if !isBareIdent(pun) {
				return fmt.Errorf("%s %s: the positional argument %s has no name, and only a bare name can say which parameter it fills", kind, c.name, pun)
			}
			a.name, a.sep, a.punned = pun, ": ", true
		}
		a.value = v
		args[i] = a
	}
	surface := ""
	if c.surface != "" {
		if kind != "action" {
			return fmt.Errorf("%s %s: on surface(...) belongs to an action", kind, c.name)
		}
		surface = " on surface(" + c.surface + ")"
	}
	open := head + kind + " " + c.name + "("
	if !c.multiline || len(args) == 0 {
		var parts []string
		for _, a := range args {
			parts = append(parts, a.name+normalSep(a.sep)+a.value)
		}
		line := open + strings.Join(parts, ", ") + ")" + surface + mods
		// A single-line call whose values carry comments cannot stay one line.
		for _, a := range args {
			if len(a.leading) > 0 || a.trailing != "" || strings.Contains(a.value, "\n") {
				return e.callMulti(open, args, depth, surface+mods, trailing, col)
			}
		}
		e.add(withTrailing(line, trailing))
		return nil
	}
	return e.callMulti(open, args, depth, surface+mods, trailing, col)
}

// callMulti writes a call one argument per line. When the author aligned
// the values in a column, an argument the rewrite named (a pun) is padded to
// the same column rather than breaking it.
func (e *emitter) callMulti(open string, args []larg, depth int, tail, trailing string, col int) error {
	e.add(open)
	aind := indentStr(depth + 1)
	if valueCol, aligned := alignedColumn(args); aligned {
		for i := range args {
			if pad := valueCol - len(args[i].name) - 1; args[i].punned && pad >= 1 {
				args[i].sep = ":" + strings.Repeat(" ", pad)
			}
		}
	}
	for i, a := range args {
		e.comments(a.leading, aind)
		sep := ","
		if i == len(args)-1 {
			sep = ""
		}
		orig := a.col
		if orig < 0 {
			orig = col + 2
		}
		e.multi(aind+a.name+a.sep, a.value, orig, len(aind), sep)
		e.lines[len(e.lines)-1] = withTrailing(e.lines[len(e.lines)-1], a.trailing)
	}
	e.add(withTrailing(indentStr(depth)+")"+tail, trailing))
	return nil
}

// alignedColumn reports whether the arguments the author named put their
// values in one column, padded wider than a single space, and which column.
func alignedColumn(args []larg) (int, bool) {
	col, padded, n := -1, false, 0
	for _, a := range args {
		if a.punned {
			continue
		}
		n++
		c := len(a.name) + len(a.sep)
		if strings.Contains(a.sep, "  ") {
			padded = true
		}
		if col < 0 {
			col = c
		} else if c != col {
			return 0, false
		}
	}
	if !padded || n < 2 {
		return 0, false
	}
	return col, true
}

// normalSep is the separator a single-line call writes: the author's padding
// is for column alignment, which one line does not have.
func normalSep(sep string) string {
	if strings.TrimSpace(sep) == ":" {
		return ": "
	}
	return sep
}

func argLabel(a larg) string {
	if a.name != "" {
		return a.name
	}
	return strings.TrimSpace(a.value)
}

// argsBlockWithFields returns an args block with fields appended for every
// name in add the block does not declare. block is the block as written
// (`args {` ... `}`), or "" when the construct had none.
func argsBlockWithFields(block string, declared []string, add []string, depth int) string {
	have := map[string]bool{}
	for _, d := range declared {
		have[d] = true
	}
	var missing []string
	for _, a := range add {
		if !have[a] {
			missing = append(missing, a)
		}
	}
	if len(missing) == 0 {
		return block
	}
	fieldInd := indentStr(depth + 1)
	if block == "" {
		var b strings.Builder
		b.WriteString("args {\n")
		for _, m := range missing {
			b.WriteString(fieldInd + m + " any\n")
		}
		b.WriteString(indentStr(depth) + "}")
		return b.String()
	}
	close := strings.LastIndexByte(block, '}')
	body := block[:close]
	// Match the indentation of the block's own fields.
	for _, l := range strings.Split(body, "\n")[1:] {
		if strings.TrimSpace(l) != "" && !isCommentLine(l) {
			fieldInd = indentOf(l)
			break
		}
	}
	var b strings.Builder
	b.WriteString(strings.TrimRight(body, " \t"))
	if !strings.HasSuffix(b.String(), "\n") {
		b.WriteString("\n")
	}
	for _, m := range missing {
		b.WriteString(fieldInd + m + " any\n")
	}
	b.WriteString(indentStr(depth) + "}")
	return b.String()
}
