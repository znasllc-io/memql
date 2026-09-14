package bodymigrate

// order.go -- which order the migrated statements run in, and the
// comment every move carries (epic memql#5370, task memql#5373; D6 and the
// "failure modes" of section 4 epic 3).
//
// A RETIRED BODY RUNS IN THE ORDER ITS COMPILER SORTS IT. Since epic 2's flip
// (memql#5367) the compiler takes a step's dependencies to be every free name
// of its expressions that names a step -- dotted or not, `steps.<id>` included,
// conditions, loop sources and filters and nested steps all read -- and sorts
// stably: the next step to run is the first in source order whose
// dependencies have all run. So a step runs where it is written unless it
// reads a step written after it. (The compiler before that saw only dotted
// first segments and ran co-released steps in Go map order; a bundle last
// run on it moves to today's order when it upgrades, which is epic 2's
// change, not this rewrite's.)
//
// A statement body runs in source order. So the rewrite decides the order to
// WRITE, from what runs:
//
//   - T is today's order: that sort.
//   - D is every reference, as the scope checker sees them.
//   - T respects D: write T. Behaviour is unchanged; every moved statement says so.
//   - T reads a name before it is bound (the engine read nothing there): write
//     the source order when it respects D, else the D-order nearest the source.
//     The statements that read nothing today say so -- this is a fix, and the
//     comment is how the reviewer finds it. T and D read the same references,
//     so this arm is for a body T cannot sort (a cycle the compiler refuses).

import (
	"fmt"
	"sort"
	"strings"
)

// orderPlan is the order to write a body's top-level statements in, and the
// comment lines to put above some of them.
type orderPlan struct {
	order    []int
	comments map[int][]string
}

// legacyNode is one step as the retired compiler saw it: a top-level
// statement, or one statement of a logic if-block the parser flattened.
type legacyNode struct {
	top  int    // index of the top-level statement it belongs to
	id   string // its step id, "" when nothing can reference it
	text string // every expression the compiler serialised for it
}

// flattenLegacy returns the retired compiler's step list for a body, in
// source order, excluding the return (which ran last).
func flattenLegacy(stmts []*lstmt) []legacyNode {
	var out []legacyNode
	var walk func(top int, st *lstmt, condText string)
	walk = func(top int, st *lstmt, condText string) {
		switch st.form {
		case formIf:
			for _, br := range st.branches {
				c := condText + " " + br.cond
				for _, inner := range br.body {
					walk(top, inner, c)
				}
			}
		case formReturn:
			// Excluded: `_return` is peeled off and runs last.
		default:
			id := ""
			if st.form == formCall || st.form == formIfCall || st.form == formExpr || st.form == formPublish {
				id = st.name
			}
			out = append(out, legacyNode{top: top, id: id, text: condText + " " + legacySeenText(st)})
		}
	}
	for i, st := range stmts {
		walk(i, st, "")
	}
	return out
}

// legacySeenText is every expression text the compiler reads for a step's
// references (collectStepReferencesV1): its conditions and arguments at every
// depth, a loop's source and filter, a switch subject and its cases, a
// parallel's branches.
func legacySeenText(st *lstmt) string {
	return strings.Join(statementTexts(st), " ")
}

// freeNames are the names text reads as roots: an identifier not reached
// through `.` and not a map or argument key, `steps.<id>` read as <id>.
func freeNames(text string) []string {
	var out []string
	toks := tokenizeExpr(text)
	for k := 0; k < len(toks); k++ {
		t := toks[k]
		if t.kind != 'i' {
			continue
		}
		prev := significant(toks, k-1, -1)
		if prev >= 0 && toks[prev].kind == 'o' && (toks[prev].text == "." || toks[prev].text == ".?") {
			continue
		}
		next := significant(toks, k+1, 1)
		if next >= 0 && toks[next].text == ":" {
			continue // a key
		}
		name := t.text
		if name == "steps" && k+2 < len(toks) && toks[k+1].text == "." && toks[k+2].kind == 'i' {
			name = toks[k+2].text
		}
		out = append(out, name)
	}
	return out
}

// legacySeenRefs returns the step ids the compiler finds in text: every free
// name that is one.
func legacySeenRefs(text string, ids map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, name := range freeNames(text) {
		if ids[name] {
			out[name] = true
		}
	}
	return out
}

// legacyGraph is the retired compiler's step list for a body and the
// dependency edges it saw: deps[i] holds the steps step i waited for.
func legacyGraph(stmts []*lstmt) ([]legacyNode, []map[int]bool) {
	nodes := flattenLegacy(stmts)
	ids := map[string]bool{}
	for _, n := range nodes {
		if n.id != "" {
			ids[n.id] = true
		}
	}
	idNode := map[string]int{}
	for i, n := range nodes {
		if n.id != "" {
			idNode[n.id] = i
		}
	}
	deps := make([]map[int]bool, len(nodes))
	for i, n := range nodes {
		deps[i] = map[int]bool{}
		for ref := range legacySeenRefs(n.text, ids) {
			if j, ok := idNode[ref]; ok && j != i {
				deps[i][j] = true
			}
		}
	}
	return nodes, deps
}

// legacyFlatOrder is the compiler's order over its step list
// (topoSortSteps): the next step is the first in source order whose
// dependencies have all run. It chooses between no orders, so there is
// nothing to say about ties.
func legacyFlatOrder(nodes []legacyNode, deps []map[int]bool) []int {
	done := make([]bool, len(nodes))
	var flat []int
	for len(flat) < len(nodes) {
		next := -1
		for i := range nodes {
			if done[i] {
				continue
			}
			ready := true
			for d := range deps[i] {
				if !done[d] {
					ready = false
					break
				}
			}
			if ready {
				next = i
				break
			}
		}
		if next < 0 {
			// A cycle: the compiler refuses the body at load, so there is no
			// today's order; the source order stands.
			flat = flat[:0]
			for i := range nodes {
				flat = append(flat, i)
			}
			return flat
		}
		done[next] = true
		flat = append(flat, next)
	}
	return flat
}

// legacyOrder reproduces the compiler's order over top-level statements.
func legacyOrder(stmts []*lstmt) (order []int, interleaved map[int]bool) {
	nodes, deps := legacyGraph(stmts)
	flat := legacyFlatOrder(nodes, deps)
	// Map flattened steps back to top-level statements, first occurrence.
	seen := map[int]bool{}
	lastPos := map[int]int{}
	interleaved = map[int]bool{}
	for pos, n := range flat {
		top := nodes[n].top
		if seen[top] && lastPos[top] != pos-1 {
			interleaved[top] = true
		}
		lastPos[top] = pos
		if !seen[top] {
			seen[top] = true
			order = append(order, top)
		}
	}
	// Statements with no flattened step (a return) keep their place at the end.
	for i := range stmts {
		if !seen[i] {
			order = append(order, i)
		}
	}
	return order, interleaved
}

// trueRefs returns, for each top-level statement, the top-level statements it
// reads, by every spelling the scope checker resolves: a bare name dotted or
// not, a `steps.` reference, first(x) / last(x).
func trueRefs(stmts []*lstmt) []map[int]bool {
	owner := map[string]int{} // a name -> the top-level statement that binds it
	var bind func(top int, st *lstmt)
	bind = func(top int, st *lstmt) {
		if st.name != "" && (st.form == formCall || st.form == formIfCall || st.form == formExpr || st.form == formPublish) {
			owner[st.name] = top
		}
		if st.form == formIf {
			for _, br := range st.branches {
				for _, inner := range br.body {
					bind(top, inner)
				}
			}
		}
		if st.form == formSwitch {
			for _, c := range st.cases {
				for _, inner := range c.body {
					bind(top, inner)
				}
			}
		}
	}
	for i, st := range stmts {
		bind(i, st)
	}
	out := make([]map[int]bool, len(stmts))
	for i, st := range stmts {
		out[i] = map[int]bool{}
		for _, text := range statementTexts(st) {
			for _, name := range freeNames(text) {
				if j, ok := owner[name]; ok && j != i {
					out[i][j] = true
				}
			}
		}
	}
	return out
}

// statementTexts is every expression text of a statement, nested bodies included.
func statementTexts(st *lstmt) []string {
	var out []string
	add := func(s string) {
		if s != "" {
			out = append(out, s)
		}
	}
	addCall := func(c *lcall) {
		if c == nil {
			return
		}
		for _, a := range c.args {
			if a.name == "" {
				add(a.value)
			} else {
				add(a.value)
			}
		}
	}
	switch st.form {
	case formCall:
		addCall(st.call)
	case formIfCall:
		add(st.cond)
		addCall(st.call)
	case formExpr, formReturn:
		add(st.expr)
	case formPublish:
		add(st.payload)
	case formIf:
		for _, br := range st.branches {
			add(br.cond)
			for _, inner := range br.body {
				out = append(out, statementTexts(inner)...)
			}
		}
	case formFor:
		add(st.source)
		add(st.filter)
		for _, inner := range st.body {
			out = append(out, statementTexts(inner)...)
		}
	case formSwitch:
		add(st.subject)
		for _, c := range st.cases {
			for _, inner := range c.body {
				out = append(out, statementTexts(inner)...)
			}
		}
	case formParallel:
		for _, br := range st.par {
			for _, inner := range br.body {
				out = append(out, statementTexts(inner)...)
			}
		}
	}
	return out
}

// respects reports whether order puts every statement after the ones it reads.
func respects(order []int, deps []map[int]bool) bool {
	pos := make(map[int]int, len(order))
	for p, i := range order {
		pos[i] = p
	}
	for i, ds := range deps {
		for d := range ds {
			if pos[d] > pos[i] {
				return false
			}
		}
	}
	return true
}

// nearest returns the topological order of deps that stays nearest to prio:
// Kahn with the ready statement earliest in prio always taken first.
func nearest(n int, deps []map[int]bool, prio []int) []int {
	rank := make([]int, n)
	for p, i := range prio {
		rank[i] = p
	}
	indeg := make([]int, n)
	consumers := make([][]int, n)
	for i, ds := range deps {
		indeg[i] = len(ds)
		for d := range ds {
			consumers[d] = append(consumers[d], i)
		}
	}
	done := make([]bool, n)
	var out []int
	for len(out) < n {
		best := -1
		for i := 0; i < n; i++ {
			if !done[i] && indeg[i] == 0 && (best < 0 || rank[i] < rank[best]) {
				best = i
			}
		}
		if best < 0 {
			return prio // a cycle: the scope checker will refuse it; keep the given order
		}
		done[best] = true
		out = append(out, best)
		for _, c := range consumers[best] {
			indeg[c]--
		}
	}
	return out
}

// sideEffecting reports whether a statement writes, acts or publishes.
func sideEffecting(st *lstmt, ix *Index) bool {
	switch st.form {
	case formPublish:
		return true
	case formCall, formIfCall:
		k := st.call.kind
		if k == "" && ix != nil {
			k, _ = ix.kindOf(st.call.name)
		}
		return k != "query" && k != "logic" && !exprFunctions[st.call.name]
	case formIf, formFor, formSwitch, formParallel:
		var any bool
		var walk func(ss []*lstmt)
		walk = func(ss []*lstmt) {
			for _, s := range ss {
				if sideEffecting(s, ix) {
					any = true
				}
			}
		}
		for _, br := range st.branches {
			walk(br.body)
		}
		walk(st.body)
		for _, c := range st.cases {
			walk(c.body)
		}
		for _, br := range st.par {
			walk(br.body)
		}
		return any
	}
	return false
}

// label is how a comment names a statement: its name, else its call.
func label(st *lstmt) string {
	switch {
	case st.name != "":
		return st.name
	case st.call != nil:
		return st.call.name
	case st.form == formFor:
		return "the loop over " + st.loopVar
	case st.form == formSwitch:
		return "the switch on " + strings.TrimSpace(st.subject)
	case st.form == formPublish:
		return "the publish of " + st.topic
	}
	return "the " + st.form
}

const orderIssue = "(memql#5373)"

// joinAnd writes a list as prose: "a", "a and b", "a, b and c".
func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// planOrder decides the order to write a body's top-level statements in.
func planOrder(stmts []*lstmt, ix *Index) orderPlan {
	n := len(stmts)
	plan := orderPlan{comments: map[int][]string{}}
	source := make([]int, n)
	for i := range source {
		source[i] = i
	}
	if n < 2 {
		plan.order = source
		return plan
	}
	today, interleaved := legacyOrder(stmts)
	deps := trueRefs(stmts)
	todayOK := respects(today, deps)
	switch {
	case todayOK:
		plan.order = today
	case respects(source, deps):
		plan.order = source
	default:
		plan.order = nearest(n, deps, source)
	}
	// Moves: every statement outside the longest run the new order keeps in
	// source order.
	keep := longestIncreasing(plan.order)
	for p, i := range plan.order {
		if keep[i] {
			continue
		}
		// Name the move by the direction it went: a statement the engine ran
		// later than written says what it now follows; one it ran earlier
		// says what it now precedes.
		if p > i && p > 0 {
			plan.comments[i] = append(plan.comments[i], fmt.Sprintf(
				"// memqlmigrate: moved below %s -- the engine ran this after it, because this reads a step written after it %s",
				label(stmts[plan.order[p-1]]), orderIssue))
		} else if p+1 < len(plan.order) {
			plan.comments[i] = append(plan.comments[i], fmt.Sprintf(
				"// memqlmigrate: moved above %s -- the engine ran this before it, because that reads a step written after it %s",
				label(stmts[plan.order[p+1]]), orderIssue))
		}
	}
	if !todayOK {
		pos := make(map[int]int, n)
		for p, i := range today {
			pos[i] = p
		}
		for i := 0; i < n; i++ {
			var early []int
			for d := range deps[i] {
				if pos[d] > pos[i] {
					early = append(early, d)
				}
			}
			if len(early) == 0 {
				continue
			}
			sort.Ints(early)
			var names []string
			for _, d := range early {
				names = append(names, label(stmts[d]))
			}
			list := joinAnd(names)
			verb, pron := "reads", "it"
			if len(names) > 1 {
				verb, pron = "read", "them"
			}
			plan.comments[i] = append(plan.comments[i], fmt.Sprintf(
				"// memqlmigrate: the engine used to run this before %s, which it %s, so it read nothing; it now runs after %s, where it is written %s",
				list, verb, pron, orderIssue))
		}
		return plan
	}
	for i := range interleaved {
		plan.comments[i] = append(plan.comments[i], fmt.Sprintf(
			"// memqlmigrate: the engine interleaved this block's statements with the ones around it; the block now runs whole, here %s", orderIssue))
	}
	return plan
}

// longestIncreasing marks the statements of the longest subsequence of order
// that is increasing in source index -- the ones that did not move.
func longestIncreasing(order []int) map[int]bool {
	n := len(order)
	length := make([]int, n)
	prev := make([]int, n)
	best := -1
	for i := 0; i < n; i++ {
		length[i], prev[i] = 1, -1
		for j := 0; j < i; j++ {
			if order[j] < order[i] && length[j]+1 > length[i] {
				length[i], prev[i] = length[j]+1, j
			}
		}
		if best < 0 || length[i] > length[best] {
			best = i
		}
	}
	keep := map[int]bool{}
	for k := best; k >= 0; k = prev[k] {
		keep[order[k]] = true
	}
	return keep
}
