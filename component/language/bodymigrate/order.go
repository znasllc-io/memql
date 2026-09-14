package bodymigrate

// order.go -- which order the migrated statements run in, and the
// comment every move carries (epic memql#5370, task memql#5373; D6 and the
// "failure modes" of section 4 epic 3).
//
// THE RETIRED BODIES DID NOT RUN IN THE ORDER THEY WERE WRITTEN. The compiler
// sorted steps topologically over the references it could SEE, and it saw
// less than it looked like: a dotted reference whose first segment was a step
// id (`decide.result`, `result.First()`) and `first(x)` / `last(x)` -- never a
// `steps.`-rooted reference (its first segment is `steps`, not a step) and
// never an undotted bare name (`observedProbe`). Kahn's queue then popped
// every initially-free step first, so a step that depended on the first step
// ran after ALL of them, wherever it was written. And the consumer list was
// built by iterating a Go map, so steps one release freed together ran in an
// order that changed from load to load.
//
// A statement body runs in source order. So the rewrite decides the order to
// WRITE, from what ran:
//
//   - T is today's order: that sort, with co-released consumers in source order
//     (the one of today's possible orders nearest the author's).
//   - D is every reference, as the scope checker sees them.
//   - T respects D: write T. Behaviour is unchanged; every moved statement says so.
//   - T reads a name before it is bound (the engine read nothing there): write
//     the source order when it respects D, else the D-order nearest the source.
//     The statements that read nothing today say so -- this is a fix, and the
//     comment is how the reviewer finds it.
//   - Where today's order between two side-effecting statements was one of
//     several, the later one says that too.

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
			out = append(out, legacyNode{top: top, id: id, text: condText + " " + legacySeenText(st, true)})
		}
	}
	for i, st := range stmts {
		walk(i, st, "")
	}
	return out
}

// legacySeenText is the text the retired compiler scanned for a step's
// references (collectAllStepReferences): its condition, its call arguments, a
// loop's source and the calls in its body, a switch subject and its cases'
// calls. A NESTED step's condition is not scanned -- stepExpressionStrings
// reads a loop's, a case's and a branch's children by configuration only --
// and neither is a loop's filter; withCond is false below the top level.
func legacySeenText(st *lstmt, withCond bool) string {
	var parts []string
	callText := func(c *lcall) {
		if c == nil {
			return
		}
		for _, a := range c.args {
			parts = append(parts, a.value)
		}
	}
	switch st.form {
	case formCall:
		callText(st.call)
	case formIfCall:
		if withCond {
			parts = append(parts, st.cond)
		}
		callText(st.call)
	case formExpr, formReturn:
		parts = append(parts, st.expr)
	case formPublish:
		parts = append(parts, st.topic, st.payload)
	case formFor:
		parts = append(parts, st.source)
		for _, inner := range st.body {
			parts = append(parts, legacySeenText(inner, false))
		}
	case formSwitch:
		parts = append(parts, st.subject)
		for _, c := range st.cases {
			for _, inner := range c.body {
				parts = append(parts, legacySeenText(inner, false))
			}
		}
	case formParallel:
		for _, br := range st.par {
			for _, inner := range br.body {
				parts = append(parts, legacySeenText(inner, false))
			}
		}
	}
	return strings.Join(parts, " ")
}

// legacySeenRefs returns the step ids the retired compiler found in text: a
// dotted run whose first segment is an id, and first(x) / last(x).
func legacySeenRefs(text string, ids map[string]bool) map[string]bool {
	out := map[string]bool{}
	toks := tokenizeExpr(text)
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t.kind != 'i' {
			continue
		}
		prev := significant(toks, i-1, -1)
		if prev >= 0 && toks[prev].kind == 'o' && (toks[prev].text == "." || toks[prev].text == ".?") {
			continue
		}
		if (t.text == "first" || t.text == "last") && i+3 < len(toks) && toks[i+1].text == "(" &&
			toks[i+2].kind == 'i' && toks[i+3].text == ")" && ids[toks[i+2].text] {
			out[toks[i+2].text] = true
			continue
		}
		// Dotted: the identifier is directly followed by `.<ident>`.
		if i+2 < len(toks) && toks[i+1].kind == 'o' && toks[i+1].text == "." && toks[i+2].kind == 'i' && ids[t.text] {
			out[t.text] = true
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

// legacyFlatOrder is the retired compiler's order over its step list (Kahn,
// FIFO, co-released consumers in source order) and the groups of steps one
// release freed together -- the orders it chose between at random.
func legacyFlatOrder(nodes []legacyNode, deps []map[int]bool) (flat []int, tieNodes [][]int) {
	indeg := make([]int, len(nodes))
	consumers := make([][]int, len(nodes))
	for i := range nodes {
		indeg[i] = len(deps[i])
		for j := range deps[i] {
			consumers[j] = append(consumers[j], i)
		}
	}
	for j := range consumers {
		sort.Ints(consumers[j])
	}
	var queue []int
	for i := range nodes {
		if indeg[i] == 0 {
			queue = append(queue, i)
		}
	}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		flat = append(flat, n)
		var freed []int
		for _, c := range consumers[n] {
			indeg[c]--
			if indeg[c] == 0 {
				queue = append(queue, c)
				freed = append(freed, c)
			}
		}
		if len(freed) > 1 {
			tieNodes = append(tieNodes, freed)
		}
	}
	if len(flat) != len(nodes) {
		// A cycle: the retired compiler refused the body at load, so there is
		// no today's order; the source order stands.
		flat = nil
		for i := range nodes {
			flat = append(flat, i)
		}
		tieNodes = nil
	}
	return flat, tieNodes
}

// legacyOrder reproduces the retired compiler's order over top-level
// statements, and the tie groups among them.
func legacyOrder(stmts []*lstmt) (order []int, ties [][]int, interleaved map[int]bool) {
	nodes, deps := legacyGraph(stmts)
	flat, tieNodes := legacyFlatOrder(nodes, deps)
	for _, g := range tieNodes {
		var group []int
		for _, f := range g {
			group = append(group, nodes[f].top)
		}
		ties = append(ties, group)
	}
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
	return order, ties, interleaved
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
	today, ties, interleaved := legacyOrder(stmts)
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
				"// memqlmigrate: moved below %s -- the engine ran this after it, ordering steps by the references it could see rather than by source %s",
				label(stmts[plan.order[p-1]]), orderIssue))
		} else if p+1 < len(plan.order) {
			plan.comments[i] = append(plan.comments[i], fmt.Sprintf(
				"// memqlmigrate: moved above %s -- the engine ran this before it, ordering steps by the references it could see rather than by source %s",
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
	for _, group := range ties {
		var effect []int
		for _, i := range group {
			if sideEffecting(stmts[i], ix) {
				effect = append(effect, i)
			}
		}
		for k := 1; k < len(effect); k++ {
			plan.comments[effect[k]] = append(plan.comments[effect[k]], fmt.Sprintf(
				"// memqlmigrate: the engine ran this and %s in no fixed order; they now run in the order written %s",
				label(stmts[effect[0]]), orderIssue))
		}
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
