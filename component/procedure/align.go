package procedure

import (
	"container/heap"
	"strconv"
)

// align.go names a deviation. The token replay in fitness.go is the cheap
// running check; when it drops, an ALIGNMENT says what happened, in the
// vocabulary the diagnosis handed to the app uses: a SYNC move (the run and
// the procedure did the same step), a LOG move (the run did something the
// procedure does not), a MODEL move (the procedure has a step the run did not
// take). The optimal alignment is the cheapest pairing of the trace with a run
// of the model -- the standard definition (van der Aalst, Adriansyah, van
// Dongen) with unit costs.

// MoveKind is one of the three kinds of alignment move.
type MoveKind string

const (
	MoveSync  MoveKind = "sync"
	MoveLog   MoveKind = "log"
	MoveModel MoveKind = "model"
)

// Move is one step of an alignment. TraceIndex is the event's index for a sync
// or log move; for a model move it is the index of the event the skipped step
// would have come before (len(trace) at the end).
type Move struct {
	Kind       MoveKind `json:"kind"`
	Label      string   `json:"label"`
	TraceIndex int      `json:"traceIndex"`
}

// Alignment is the moves in order and their total cost. Cost is -1 when the
// search hit its state cap and the moves are a best-effort alignment rather
// than an optimal one.
type Alignment struct {
	Moves []Move `json:"moves"`
	Cost  int    `json:"cost"`
}

// FirstDeviation is the earliest move that is not a sync, or false when every
// move is one.
func (a Alignment) FirstDeviation() (Move, bool) {
	for _, m := range a.Moves {
		if m.Kind != MoveSync {
			return m, true
		}
	}
	return Move{}, false
}

// alignStateCap bounds one alignment search, counted in distinct (marking,
// trace position) states. A model with wide parallel blocks has a state space
// that grows as a product of its branches, and without a cap one diagnosis
// could cost more than the replay it diagnoses.
const alignStateCap = 100000

// alignMarkingBudget bounds the same search in BYTES of markings. Every state
// stores a dense marking -- one int per place of the net -- so a count of
// states alone is not a bound on memory: a procedure of a thousand steps has
// a thousand places, and a hundred thousand of its states were most of a
// gigabyte for one diagnosis of a run that went its own way halfway through.
const alignMarkingBudget = 64 << 20

// alignStatesFor is the state cap for a net of this many places: the smaller
// of alignStateCap and what alignMarkingBudget holds of its markings. A small
// net keeps the full cap; a large one gets fewer states, never fewer than one.
func alignStatesFor(places int) int {
	perMarking := places * (strconv.IntSize / 8)
	if perMarking <= 0 {
		return alignStateCap
	}
	n := alignMarkingBudget / perMarking
	switch {
	case n < 1:
		return 1
	case n > alignStateCap:
		return alignStateCap
	}
	return n
}

// Align returns an optimal alignment of the trace with the model: Dijkstra
// over (marking, trace position), a sync or silent move costing 0 and a log or
// model move costing 1. Over the state cap -- alignStatesFor the net's size --
// it returns the best partial with Cost -1 (see alignCapped).
func Align(tree *ProcessTree, trace []string) Alignment {
	net := buildNet(tree)
	return alignNet(net, trace, alignStatesFor(net.places))
}

// alignState is one discovered search state.
type alignState struct {
	m    marking
	i    int
	cost int
	prev int   // index of the predecessor state; -1 for the start
	move *Move // the move that led here; nil for a silent firing and the start
}

// alignCapped is Align with its cap as a parameter, so the cap's behaviour can
// be tested without building a model a hundred thousand states wide.
func alignCapped(tree *ProcessTree, trace []string, maxStates int) Alignment {
	return alignNet(buildNet(tree), trace, maxStates)
}

// alignNet is the search over a net already built.
//
// Over the cap the search stops and returns the FURTHEST state it settled --
// the most trace consumed, then the cheapest -- completed into an alignment of
// the whole trace: every event it never reached becomes a log move, and the
// model is driven to its final marking by the fewest model moves. Cost is -1,
// so no caller mistakes it for optimal. Completing it matters: a partial that
// simply ended where the search stopped would read as a trace that fit.
func alignNet(net *petriNet, trace []string, maxStates int) Alignment {
	states := []alignState{{m: net.initial(), prev: -1}}
	best := map[string]int{stateKey(states[0].m, 0): 0}
	settled := map[string]bool{}
	pq := &alignQueue{states: &states}
	heap.Push(pq, 0)

	furthest := -1
	relax := func(from int, m marking, i, cost int, move *Move) {
		k := stateKey(m, i)
		if c, seen := best[k]; seen && c <= cost {
			return
		}
		best[k] = cost
		states = append(states, alignState{m: m, i: i, cost: cost, prev: from, move: move})
		heap.Push(pq, len(states)-1)
	}

	for pq.Len() > 0 {
		idx := heap.Pop(pq).(int)
		s := states[idx]
		k := stateKey(s.m, s.i)
		if settled[k] || s.cost > best[k] {
			continue
		}
		settled[k] = true
		if furthest < 0 || s.i > states[furthest].i || (s.i == states[furthest].i && s.cost < states[furthest].cost) {
			furthest = idx
		}
		if s.i == len(trace) && s.m.isFinal() {
			return Alignment{Moves: movesTo(states, idx), Cost: s.cost}
		}
		if len(best) > maxStates {
			break
		}
		for t, tr := range net.trans {
			if !net.enabled(s.m, t) {
				continue
			}
			next := net.fire(s.m, t)
			if tr.label == "" {
				relax(idx, next, s.i, s.cost, nil)
				continue
			}
			if s.i < len(trace) && tr.label == trace[s.i] {
				relax(idx, next, s.i+1, s.cost, &Move{Kind: MoveSync, Label: tr.label, TraceIndex: s.i})
			}
			relax(idx, next, s.i, s.cost+1, &Move{Kind: MoveModel, Label: tr.label, TraceIndex: s.i})
		}
		if s.i < len(trace) {
			relax(idx, s.m, s.i+1, s.cost+1, &Move{Kind: MoveLog, Label: trace[s.i], TraceIndex: s.i})
		}
	}

	// The cap (or, defensively, an exhausted search: a net built from a tree
	// always reaches its final marking, so exhaustion should not happen).
	f := states[furthest]
	moves := movesTo(states, furthest)
	for i := f.i; i < len(trace); i++ {
		moves = append(moves, Move{Kind: MoveLog, Label: trace[i], TraceIndex: i})
	}
	moves = append(moves, net.completeToFinal(f.m, len(trace))...)
	return Alignment{Moves: moves, Cost: -1}
}

// movesTo walks the predecessor chain back to the start.
func movesTo(states []alignState, idx int) []Move {
	var rev []Move
	for i := idx; i >= 0; i = states[i].prev {
		if states[i].move != nil {
			rev = append(rev, *states[i].move)
		}
	}
	moves := make([]Move, len(rev))
	for i := range rev {
		moves[i] = rev[len(rev)-1-i]
	}
	return moves
}

// completionBound bounds the walk that finishes a capped alignment, in
// markings visited. The walk only has to find SOME route to the end, and in a
// net built from a tree it finds one without backtracking in practice; the
// bound is for the tree nobody anticipated. It is held to the marking budget
// too (alignStatesFor), because every marking it visits is one more copy.
const completionBound = 10000

// completeToFinal walks from m to the final marking, silent firings first and
// otherwise in construction order, never revisiting a marking, and returns the
// labelled firings as model moves placed at trace position at. Nothing when no
// route is found within the bound.
//
// It is a depth-first walk and not a search for the FEWEST model moves on
// purpose. It runs only after the optimal search hit its cap, when the answer
// is already best-effort (Cost -1), and a breadth-first search over a wide
// parallel block would explore the product of its branches -- the very space
// the cap refused. What it must deliver is validity: an alignment that ends in
// the final marking, so the model steps the run never reached are named.
func (n *petriNet) completeToFinal(m marking, at int) []Move {
	order := append(append([]int(nil), n.silent...), n.labelled()...)
	type frame struct {
		m    marking
		next int
		move *Move
	}
	stack := []frame{{m: m}}
	seen := map[string]bool{m.key(): true}
	bound := completionBound
	if byBytes := alignStatesFor(n.places); byBytes < bound {
		bound = byBytes
	}
	for len(stack) > 0 && len(seen) <= bound {
		top := len(stack) - 1
		if stack[top].m.isFinal() {
			var moves []Move
			for _, f := range stack {
				if f.move != nil {
					moves = append(moves, *f.move)
				}
			}
			return moves
		}
		pushed := false
		for stack[top].next < len(order) {
			t := order[stack[top].next]
			stack[top].next++
			if !n.enabled(stack[top].m, t) {
				continue
			}
			next := n.fire(stack[top].m, t)
			k := next.key()
			if seen[k] {
				continue
			}
			seen[k] = true
			var mv *Move
			if label := n.trans[t].label; label != "" {
				mv = &Move{Kind: MoveModel, Label: label, TraceIndex: at}
			}
			stack = append(stack, frame{m: next, move: mv})
			pushed = true
			break
		}
		if !pushed {
			stack = stack[:top]
		}
	}
	return nil
}

// labelled is every labelled transition in construction order.
func (n *petriNet) labelled() []int {
	out := make([]int, 0, len(n.trans)-len(n.silent))
	for t, tr := range n.trans {
		if tr.label != "" {
			out = append(out, t)
		}
	}
	return out
}

func stateKey(m marking, i int) string {
	return m.key() + "|" + strconv.Itoa(i)
}

// alignQueue orders states by cost, then by MORE trace consumed, then by
// discovery. The first is Dijkstra; the second only breaks ties, toward the
// goal, so a fitting trace is aligned without exploring its alternatives; the
// third makes the answer independent of anything but the inputs.
type alignQueue struct {
	states *[]alignState
	items  []int
}

func (q *alignQueue) Len() int { return len(q.items) }
func (q *alignQueue) Less(a, b int) bool {
	sa, sb := (*q.states)[q.items[a]], (*q.states)[q.items[b]]
	switch {
	case sa.cost != sb.cost:
		return sa.cost < sb.cost
	case sa.i != sb.i:
		return sa.i > sb.i
	default:
		return q.items[a] < q.items[b]
	}
}
func (q *alignQueue) Swap(a, b int) { q.items[a], q.items[b] = q.items[b], q.items[a] }
func (q *alignQueue) Push(x any)    { q.items = append(q.items, x.(int)) }
func (q *alignQueue) Pop() any {
	last := q.items[len(q.items)-1]
	q.items = q.items[:len(q.items)-1]
	return last
}

// AssignSymbol names the symbol an action belongs to, under Symbolize's own
// distance budget: the symbol of the same tool whose template anti-unifies
// with the action at the smallest distance within the budget, earliest on a
// tie. It reports false when no template of the action's tool is that close.
//
// A shadow comparison uses it to write a NEW recording in the alphabet of a
// stored procedure; an action it cannot place is outside that alphabet, which
// the comparison reads as a mismatch rather than as a guess. The closest
// template rather than the first that fits, because the stored templates are
// the clusters' final generalizations -- more general than when their members
// joined -- and the closest one is the answer that does not depend on the
// order the clusters were written in.
func AssignSymbol(symbols []Symbol, a Action, budget int) (string, bool) {
	bestId, bestDist := "", -1
	for _, s := range symbols {
		if s.Tool != a.Tool {
			continue
		}
		_, d := AntiUnify(s.Template, a.Args, newHoleNamer())
		if d > budget {
			continue
		}
		if bestDist < 0 || d < bestDist {
			bestId, bestDist = s.Id, d
		}
	}
	return bestId, bestDist >= 0
}

// SequenceTree is a procedure's OWN model: its steps' symbols in order,
// seq(leaf...). A replay checks its live trace against this rather than
// against the corpus's structured model, because the procedure is one path
// through that corpus -- a mined pattern may skip symbols the corpus has
// between its steps, and the corpus model would count every replay of it as
// a deviation at the first gap.
func SequenceTree(symbols []string) *ProcessTree {
	kids := make([]*ProcessTree, 0, len(symbols))
	for _, s := range symbols {
		if s == "" {
			continue
		}
		kids = append(kids, &ProcessTree{Op: OpLeaf, Symbol: s})
	}
	return &ProcessTree{Op: OpSeq, Children: kids}
}
