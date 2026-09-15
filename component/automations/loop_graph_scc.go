package automations

// loop_graph_scc.go -- the graph algorithms of the static loop graph
// (memql#5381): strongly connected components, their strata, and whether the
// @loop automations of a component cover every cycle in it (D-H).
//
// Every algorithm here is deterministic by construction: nodes are the
// automations' indices in their sorted order, successor lists are ascending,
// and every walk visits in that order. The graph is printed in a refusal and
// drawn in the OS, and an answer that depended on map order would differ
// between two boots of one tree.

import "sort"

// tarjan returns the strongly connected components of the graph on nodes
// 0..n-1 whose successors are succ, each component's members ascending, in
// the order Tarjan completes them -- a component after every component it
// reaches, which is reverse topological order. Iterative, so a long chain of
// automations cannot exhaust the stack.
func tarjan(n int, succ [][]int) [][]int {
	index := make([]int, n)
	low := make([]int, n)
	onStack := make([]bool, n)
	for i := range index {
		index[i] = -1
	}
	var (
		stack []int
		out   [][]int
		next  int
	)
	type frame struct{ v, i int }
	push := func(v int) {
		index[v], low[v] = next, next
		next++
		stack = append(stack, v)
		onStack[v] = true
	}
	for root := 0; root < n; root++ {
		if index[root] != -1 {
			continue
		}
		push(root)
		call := []frame{{v: root}}
		for len(call) > 0 {
			top := len(call) - 1
			v := call[top].v
			if call[top].i < len(succ[v]) {
				w := succ[v][call[top].i]
				call[top].i++
				switch {
				case index[w] == -1:
					push(w)
					call = append(call, frame{v: w})
				case onStack[w] && index[w] < low[v]:
					low[v] = index[w]
				}
				continue
			}
			call = call[:top]
			if top > 0 {
				if u := call[top-1].v; low[v] < low[u] {
					low[u] = low[v]
				}
			}
			if low[v] == index[v] {
				var comp []int
				for {
					w := stack[len(stack)-1]
					stack = stack[:len(stack)-1]
					onStack[w] = false
					comp = append(comp, w)
					if w == v {
						break
					}
				}
				sort.Ints(comp)
				out = append(out, comp)
			}
		}
	}
	return out
}

// hasSelfEdge reports whether v's successors include v.
func hasSelfEdge(succ [][]int, v int) bool {
	for _, w := range succ[v] {
		if w == v {
			return true
		}
	}
	return false
}

// strata layers the condensation of the graph Datalog-style: a component
// with no edge into it from another is stratum 0, and every other is one
// more than the deepest component with an edge into it -- the longest-path
// layering, so every member of a component shares its stratum, and a
// scheduled, template or raw-topic automation nothing publishes to is a
// source. comps must be in tarjan's order; comp maps a node to its
// component. It returns the stratum of every node.
func strata(comps [][]int, comp []int, succ [][]int) []int {
	level := make([]int, len(comps))
	// tarjan completes a component after everything it reaches, so walking
	// its output backwards visits every component before any it reaches.
	for c := len(comps) - 1; c >= 0; c-- {
		for _, v := range comps[c] {
			for _, w := range succ[v] {
				if d := comp[w]; d != c && level[d] < level[c]+1 {
					level[d] = level[c] + 1
				}
			}
		}
	}
	out := make([]int, len(comp))
	for v, c := range comp {
		out[v] = level[c]
	}
	return out
}

// covered reports whether every cycle of the component members lies through
// a @loop automation: the component minus its @loop members holds no cycle,
// which re-running tarjan on the induced subgraph answers.
func covered(members []int, succ [][]int, isLoop func(int) bool) bool {
	local := map[int]int{}
	var keep []int
	for _, m := range members {
		if !isLoop(m) {
			local[m] = len(keep)
			keep = append(keep, m)
		}
	}
	sub := make([][]int, len(keep))
	for i, v := range keep {
		for _, w := range succ[v] {
			if j, ok := local[w]; ok {
				sub[i] = append(sub[i], j)
			}
		}
	}
	for _, comp := range tarjan(len(keep), sub) {
		if len(comp) > 1 || hasSelfEdge(sub, comp[0]) {
			return false
		}
	}
	return true
}
