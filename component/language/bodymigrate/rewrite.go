// Package bodymigrate is the bodies rewrite, `memqlmigrate --rewrite=bodies`
// (epic memql#5370, task memql#5373; D2, D6, D12-D15 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md).
// It is a library, as the expressions rewrite is (parser.RewriteExpressions),
// so the CLI, its Go-fixture mode and the logic corpus's run-time check all
// run the one rewrite.
//
// It carries a tree from the retired body forms to edition 2026's statements:
// `step` blocks and logic's `body { }` become statements, the terse header
// expands, a publishing logic moves into its automation, every call takes its
// kind, the step-result spellings and the argument pun retire, arguments are
// read `args.x`, rows read their fields directly, `mutate` declarations become
// `mutation`, and @trigger loses `partition=`. Statements are written in the
// order the retired engine ran them, and every move carries a comment.
//
// It refuses rather than guesses: a construct it cannot carry across exactly
// makes the whole run fail, naming every such construct, and writes nothing.
// A tree already in edition 2026 comes back unchanged.
package bodymigrate

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// RewriteTree rewrites the .memql files among files, resolving bare calls
// through ix, and returns the files it changed. ix must index the tree and
// every tree it calls into (see index.go).
func RewriteTree(files map[string][]byte, ix *Index) (map[string][]byte, error) {
	rw := &bodiesRewrite{ix: ix, cur: map[string]string{}}
	for p, b := range files {
		if path.Ext(p) == ".memql" && !skippedDir(p) {
			rw.cur[p] = string(b)
		}
	}
	if err := rw.run(); err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for p, s := range rw.cur {
		if s != string(files[p]) {
			out[p] = []byte(s)
		}
	}
	return out, nil
}

// RewriteSource is the rewrite over one source that stands alone: the tree
// rewrite, over a tree of one file, with the declaration index given. A
// construct it cannot carry fails the call, as it fails the tree.
func RewriteSource(src string, ix *Index) (string, error) {
	const name = "fixture/fixture.memql"
	rw := &bodiesRewrite{ix: ix, cur: map[string]string{name: src}}
	if err := rw.run(); err != nil {
		return "", err
	}
	return rw.cur[name], nil
}

// skippedDir reports a path under a directory the loaders skip: one whose
// name starts with `_` (dsl/_reference's don't-do-this skeletons keep the
// retired forms on purpose) or `.`.
func skippedDir(p string) bool {
	for _, seg := range strings.Split(path.Dir(p), "/") {
		if strings.HasPrefix(seg, "_") || (strings.HasPrefix(seg, ".") && seg != ".") {
			return true
		}
	}
	return false
}

type bodiesRewrite struct {
	ix       *Index
	cur      map[string]string
	problems []string
	// inline maps a terse automation's name to the publishing logic whose
	// statements replace its one call.
	inline map[string]*inlinePlan
	// deleted marks logic removed because they were inlined, by file.
	deleted map[string]map[string]bool
}

type inlinePlan struct {
	logic     *legacyBody
	logicFile string
	logicDoc  []string
	imports   map[string]string // name -> use path, from the logic's file
}

func (rw *bodiesRewrite) problem(format string, a ...any) {
	rw.problems = append(rw.problems, fmt.Sprintf(format, a...))
}

func (rw *bodiesRewrite) run() error {
	rw.planInlining()
	for _, p := range sortedKeys(rw.cur) {
		out, err := rw.rewriteFile(p, rw.cur[p])
		if err != nil {
			rw.problem("%v", err)
			continue
		}
		rw.cur[p] = out
	}
	for _, p := range sortedKeys(rw.cur) {
		rw.cur[p] = rewriteDeclarationsAndTriggers(rw.cur[p])
	}
	if len(rw.problems) > 0 {
		sort.Strings(rw.problems)
		return fmt.Errorf("the bodies rewrite cannot carry %d construct(s) across exactly; nothing was written:\n  %s",
			len(rw.problems), strings.Join(rw.problems, "\n  "))
	}
	return nil
}

// planInlining finds every legacy logic that publishes and decides whether it
// can move into its automation.
func (rw *bodiesRewrite) planInlining() {
	rw.inline = map[string]*inlinePlan{}
	rw.deleted = map[string]map[string]bool{}
	// Terse automations by the logic they call.
	terseBy := map[string][]string{}
	for _, p := range sortedKeys(rw.cur) {
		for _, c := range findConstructs(rw.cur[p]) {
			if c.terse {
				terseBy[c.terseLogic] = append(terseBy[c.terseLogic], c.name)
			}
		}
	}
	for _, p := range sortedKeys(rw.cur) {
		src := rw.cur[p]
		for _, c := range findConstructs(src) {
			if c.kind != "logic" || c.terse || !isLegacyLogic(src[c.open+1:c.close]) {
				continue
			}
			lb, err := readConstructBody(p, src, c)
			if err != nil || !publishes(lb.stmts) {
				continue // read errors are reported when the logic itself is rewritten
			}
			autos := terseBy[c.name]
			others := logicCallers(rw.cur, c.name, "")
			if len(autos) != 1 || len(others) != 1 {
				rw.problem("%s:%d: logic %s publishes, which a logic may not (D14), and it is reached from %d place(s): %s -- move the publish into the automation that calls it by hand",
					p, lineAt(src, c.start), c.name, len(others), strings.Join(others, "; "))
				continue
			}
			if _, err := inlineLogic(lb); err != nil {
				rw.problem("%s:%d: %v", p, lineAt(src, c.start), err)
				continue
			}
			rw.inline[autos[0]] = &inlinePlan{logic: lb, logicFile: p, logicDoc: docLines(src, c), imports: useImports(src)}
			if rw.deleted[p] == nil {
				rw.deleted[p] = map[string]bool{}
			}
			rw.deleted[p][c.name] = true
		}
	}
}

// rewriteFile rewrites every legacy automation and logic in one file.
func (rw *bodiesRewrite) rewriteFile(p, src string) (string, error) {
	cs := findConstructs(src)
	var imports map[string]string
	var removed []string // the text of each logic moved out of this file
	for k := len(cs) - 1; k >= 0; k-- {
		c := cs[k]
		switch {
		case c.terse:
			text := expandTerse(c, src)
			if plan := rw.inline[c.name]; plan != nil {
				t, need, err := rw.inlinedAutomation(p, src, c, plan)
				if err != nil {
					rw.problem("%v", err)
					continue
				}
				text = t
				if imports == nil {
					imports = map[string]string{}
				}
				for n, from := range need {
					imports[n] = from
				}
			}
			src = src[:c.start] + text + src[c.close:]
		case c.kind == "logic" && rw.deleted[p][c.name]:
			start, end := declarationRegion(src, c)
			removed = append(removed, src[start:end])
			src = src[:start] + src[end:]
		case c.kind == "automation" && isLegacyAutomation(src[c.open+1:c.close]),
			c.kind == "logic" && isLegacyLogic(src[c.open+1:c.close]):
			text, err := rw.rewriteConstruct(p, src, c)
			if err != nil {
				rw.problem("%v", err)
				continue
			}
			src = src[:c.start] + text + src[c.close+1:]
		}
	}
	src = pruneImports(src, strings.Join(removed, "\n"))
	return addImports(src, imports), nil
}

// rewriteConstruct returns the edition-2026 text of one legacy construct, from
// its keyword through its closing brace.
func (rw *bodiesRewrite) rewriteConstruct(p, src string, c construct) (string, error) {
	lb, err := readConstructBody(p, src, c)
	if err != nil {
		return "", err
	}
	rc := &refContext{construct: c.kind, steps: map[string]string{}, args: map[string]bool{},
		loopVars: map[string]bool{}, eventFields: map[string]bool{}}
	if c.kind == "automation" {
		for _, f := range lb.argFields {
			rc.args[f] = true
		}
	}
	rw.bindNames(lb.stmts, rc)
	e := &emitter{ix: rw.ix, inLogic: c.kind == "logic"}
	plan := planOrder(lb.stmts, rw.ix)
	if err := e.statements(lb.stmts, plan.order, plan.comments, 1, rc); err != nil {
		return "", &bodyError{file: p, construct: c.kind + " " + c.name, line: lineAt(src, c.start), msg: err.Error()}
	}
	e.comments(lb.trailing, indentStr(1))
	return rw.assemble(src, c, lb, rc, e.lines, nil), nil
}

// bindNames records every statement name a body binds and what it binds.
func (rw *bodiesRewrite) bindNames(stmts []*lstmt, rc *refContext) {
	for _, st := range stmts {
		switch st.form {
		case formCall, formIfCall:
			k := st.call.kind
			if k == "" {
				k, _ = rw.ix.kindOf(st.call.name)
			}
			if st.name != "" {
				rc.steps[st.name] = k
			}
		case formExpr:
			rc.steps[st.name] = "expr"
		case formPublish:
			if st.name != "" {
				rc.steps[st.name] = "publish"
			}
		}
		for _, br := range st.branches {
			rw.bindNames(br.body, rc)
		}
		for _, cs := range st.cases {
			rw.bindNames(cs.body, rc)
		}
	}
}

// assemble writes a construct: its header line, its args block (with the
// fields event.payload reads introduced), its preconditions, then the
// statement lines already written.
func (rw *bodiesRewrite) assemble(src string, c construct, lb *legacyBody, rc *refContext, stmtLines []string, lead []string) string {
	var b strings.Builder
	b.WriteString(src[c.start : c.open+1])
	b.WriteString("\n")
	ind := indentStr(1)
	var fields []string
	for f := range rc.eventFields {
		fields = append(fields, f)
	}
	sort.Strings(fields)
	argsText := ""
	var argsLeading []string
	if lb.args != nil {
		argsText, argsLeading = lb.args.text, lb.args.leading
	}
	argsText = argsBlockWithFields(argsText, lb.argFields, fields, 1)
	for _, l := range argsLeading {
		b.WriteString(commentLine(ind, l))
	}
	if argsText != "" {
		b.WriteString(ind + argsText + "\n")
	}
	for _, pc := range lb.precond {
		for _, l := range pc.leading {
			b.WriteString(commentLine(ind, l))
		}
		b.WriteString(ind + pc.text + "\n")
	}
	for _, l := range lead {
		b.WriteString(commentLine(ind, l))
	}
	for _, l := range stmtLines {
		b.WriteString(strings.TrimRight(l, " \t") + "\n")
	}
	b.WriteString("}")
	return b.String()
}

func commentLine(ind, l string) string {
	if l == "" {
		return "\n"
	}
	return ind + l + "\n"
}

// inlinedAutomation writes a terse automation whose logic publishes as the
// automation holding the logic's statements. It returns the text and the
// imports the automation's file needs for them.
func (rw *bodiesRewrite) inlinedAutomation(p, src string, c construct, plan *inlinePlan) (string, map[string]string, error) {
	stmts, err := inlineLogic(plan.logic)
	if err != nil {
		return "", nil, err
	}
	rc := &refContext{construct: "automation", steps: map[string]string{}, args: map[string]bool{},
		loopVars: map[string]bool{}, eventFields: map[string]bool{}, logicEvent: true}
	rw.bindNames(stmts, rc)
	e := &emitter{ix: rw.ix}
	order := planOrder(stmts, rw.ix)
	if err := e.statements(stmts, order.order, order.comments, 1, rc); err != nil {
		return "", nil, &bodyError{file: plan.logicFile, construct: "logic " + plan.logic.name + " (inlined into automation " + c.name + ")",
			line: 0, msg: err.Error()}
	}
	lineStartOff := lineStart(src, c.start)
	ind := src[lineStartOff:c.start]
	header := c.terseTrigger + "\n" + ind + "automation " + c.name + " {"
	fake := construct{start: 0, open: len(header) - 1}
	lead := append([]string{fmt.Sprintf("// memqlmigrate: the statements of logic %s, which published and so could not stay a logic (D14) %s", plan.logic.name, orderIssue)}, plan.logicDoc...)
	lb := &legacyBody{}
	text := rw.assemble(header, fake, lb, rc, e.lines, lead)
	// The imports the inlined statements lean on.
	need := map[string]string{}
	joined := strings.Join(e.lines, "\n")
	for name, from := range plan.imports {
		if containsWord(joined, name) {
			need[name] = from
		}
	}
	return text, need, nil
}

func containsWord(s, w string) bool {
	for i := strings.Index(s, w); i >= 0; {
		before := i == 0 || !isIdentByte(s[i-1])
		after := i+len(w) >= len(s) || !isIdentByte(s[i+len(w)])
		if before && after {
			return true
		}
		next := strings.Index(s[i+1:], w)
		if next < 0 {
			return false
		}
		i += 1 + next
	}
	return false
}
