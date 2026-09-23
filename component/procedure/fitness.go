package procedure

import (
	"strconv"
	"strings"
)

// fitness.go is D16's third line of drift detection, the cheap one: a running
// token-based replay of the live trace against the procedure's process model.
// A replay checks it after every step, and only when it drops does align.go
// spend a search on naming the deviation.
//
// A ProcessTree is lowered to a WORKFLOW NET -- places, transitions, one
// source place and one sink place -- and a trace is replayed by firing the
// transition each event names (Rozinat and van der Aalst's token replay):
// tokens the model could not supply are counted MISSING, tokens left over
// when the trace ends are REMAINING, and the fitness is
//
//	0.5*(1 - missing/consumed) + 0.5*(1 - remaining/produced)
//
// which is 1 exactly when the trace is a run of the model.

// FitnessResult is one token replay.
type FitnessResult struct {
	// Fitness is 1 for a fitting trace and falls toward 0 as tokens go
	// missing or are left over.
	Fitness   float64 `json:"fitness"`
	Produced  int     `json:"produced"`
	Consumed  int     `json:"consumed"`
	Missing   int     `json:"missing"`
	Remaining int     `json:"remaining"`
	// Fits is missing == 0 && remaining == 0.
	Fits bool `json:"fits"`
	// FirstDeviation is the index into the trace of the first event that
	// needed a missing token -- or that the model does not name at all. -1
	// when no event did; a trace that merely STOPPED early deviates at no
	// event, and Fits is false for it all the same.
	FirstDeviation int `json:"firstDeviation"`
}

// TokenReplay replays a whole trace against the model, final marking
// included.
func TokenReplay(tree *ProcessTree, trace []string) FitnessResult {
	r := newReplayer(buildNet(tree))
	for i, ev := range trace {
		r.event(i, ev)
	}
	r.finish()
	res := FitnessResult{
		Produced:       r.produced,
		Consumed:       r.consumed,
		Missing:        r.missing,
		Remaining:      r.remaining,
		FirstDeviation: r.firstDeviation,
	}
	res.Fits = res.Missing == 0 && res.Remaining == 0
	res.Fitness = 0.5*(1-ratio(res.Missing, res.Consumed)) + 0.5*(1-ratio(res.Remaining, res.Produced))
	return res
}

// PrefixFits is the RUNNING check a replay makes after each step: the trace so
// far is replayed, and tokens still waiting for later steps are expected, not
// counted. It reports the index of the first event that needed a missing
// token, or -1 when every event so far is one the model could have produced
// next.
func PrefixFits(tree *ProcessTree, trace []string) (bool, int) {
	r := newReplayer(buildNet(tree))
	for i, ev := range trace {
		if !r.event(i, ev) {
			return false, i
		}
	}
	return true, -1
}

func ratio(part, whole int) float64 {
	if whole <= 0 {
		return 0
	}
	return float64(part) / float64(whole)
}

// --- the workflow net ------------------------------------------------------

const (
	sourcePlace = 0
	sinkPlace   = 1
)

// petriNet is the workflow net of one process tree. A transition with an
// empty label is SILENT: it moves tokens without an event.
type petriNet struct {
	places int
	trans  []netTransition
	// byLabel lists each label's transitions in construction order; a label
	// can have several when a symbol occurs twice in the tree.
	byLabel map[string][]int
	silent  []int
}

type netTransition struct {
	label   string
	in, out []int
}

// buildNet lowers a tree between the source and the sink. A nil tree is the
// EMPTY procedure -- one silent step from source to sink -- so only the empty
// trace fits it; "no model" never means "anything goes".
func buildNet(t *ProcessTree) *petriNet {
	n := &petriNet{places: 2, byLabel: map[string][]int{}}
	n.build(t, sourcePlace, sinkPlace)
	return n
}

func (n *petriNet) place() int {
	p := n.places
	n.places++
	return p
}

func (n *petriNet) add(label string, in, out []int) {
	idx := len(n.trans)
	n.trans = append(n.trans, netTransition{label: label, in: in, out: out})
	if label == "" {
		n.silent = append(n.silent, idx)
		return
	}
	n.byLabel[label] = append(n.byLabel[label], idx)
}

// build places the tree's behaviour between the places in and out.
//
//   - leaf: one transition labelled with the symbol.
//   - seq: the children chained through fresh places.
//   - xor: every child between the SAME in and out, so one token takes
//     exactly one branch.
//   - and: a silent split into one fresh place per child and a silent join
//     out of one fresh place per child, so every branch runs and they
//     interleave freely.
//   - loop(body, redo...): a silent ENTRY into a private start place, the
//     body from start to a private mid place, a silent exit from mid to out,
//     and each redo from mid back to start.
//
// The loop's private start is the one non-obvious choice, and a test guards
// it. The obvious construction runs the redo back to `in` -- and inside a
// choice `in` is shared with the other branches, so a redo there would let a
// loop that ended on its redo leave by another branch: xor(loop(a,b), c) would
// accept `a b c`. The entry transition costs one silent firing and keeps the
// redo inside the loop.
//
// An empty seq/xor/and, a nil child and an unknown operator are all silent: a
// node with no behaviour moves the token along and claims no event.
func (n *petriNet) build(t *ProcessTree, in, out int) {
	if t == nil {
		n.add("", []int{in}, []int{out})
		return
	}
	switch t.Op {
	case OpLeaf:
		n.add(t.Symbol, []int{in}, []int{out})
	case OpSeq:
		if len(t.Children) == 0 {
			n.add("", []int{in}, []int{out})
			return
		}
		cur := in
		for i, c := range t.Children {
			next := out
			if i < len(t.Children)-1 {
				next = n.place()
			}
			n.build(c, cur, next)
			cur = next
		}
	case OpXor:
		if len(t.Children) == 0 {
			n.add("", []int{in}, []int{out})
			return
		}
		for _, c := range t.Children {
			n.build(c, in, out)
		}
	case OpAnd:
		if len(t.Children) == 0 {
			n.add("", []int{in}, []int{out})
			return
		}
		starts := make([]int, len(t.Children))
		ends := make([]int, len(t.Children))
		for i := range t.Children {
			starts[i], ends[i] = n.place(), n.place()
		}
		n.add("", []int{in}, starts)
		for i, c := range t.Children {
			n.build(c, starts[i], ends[i])
		}
		n.add("", ends, []int{out})
	case OpLoop:
		start, mid := n.place(), n.place()
		n.add("", []int{in}, []int{start})
		var body *ProcessTree
		if len(t.Children) > 0 {
			body = t.Children[0]
		}
		n.build(body, start, mid)
		n.add("", []int{mid}, []int{out})
		if len(t.Children) < 2 {
			// No redo named: a silent one, so the body may simply repeat.
			n.add("", []int{mid}, []int{start})
		}
		for _, redo := range t.Children[1:] {
			n.build(redo, mid, start)
		}
	default:
		n.add("", []int{in}, []int{out})
	}
}

// marking is the token count on every place.
type marking []int

func (n *petriNet) initial() marking {
	m := make(marking, n.places)
	m[sourcePlace] = 1
	return m
}

func (m marking) clone() marking { return append(marking(nil), m...) }

// key is a marking as a map key. Counts, not bits: token replay inserts
// missing tokens, so a place can hold more than one.
func (m marking) key() string {
	var b strings.Builder
	for i, c := range m {
		if c == 0 {
			continue
		}
		b.WriteString(strconv.Itoa(i))
		b.WriteByte(':')
		b.WriteString(strconv.Itoa(c))
		b.WriteByte(',')
	}
	return b.String()
}

func (m marking) total() int {
	n := 0
	for _, c := range m {
		n += c
	}
	return n
}

func (m marking) isFinal() bool {
	return m[sinkPlace] == 1 && m.total() == 1
}

func (n *petriNet) enabled(m marking, t int) bool {
	for _, p := range n.trans[t].in {
		if m[p] < 1 {
			return false
		}
	}
	return true
}

// fire returns the marking after t, which must be enabled.
func (n *petriNet) fire(m marking, t int) marking {
	next := m.clone()
	for _, p := range n.trans[t].in {
		next[p]--
	}
	for _, p := range n.trans[t].out {
		next[p]++
	}
	return next
}

// silentSearchBound bounds one search over silent firings. The nets a
// procedure produces are small and safe, so the bound is never the answer in
// practice; it exists so a pathological tree cannot turn one replay step into
// an unbounded walk.
const silentSearchBound = 256

// silentPath searches, breadth first, for the fewest silent firings from m
// that reach a marking goal accepts. It returns the firings and the marking,
// or ok=false when none is found within the bound.
func (n *petriNet) silentPath(m marking, goal func(marking) bool) ([]int, marking, bool) {
	type node struct {
		m    marking
		path []int
	}
	queue := []node{{m: m}}
	seen := map[string]bool{m.key(): true}
	for len(queue) > 0 && len(seen) <= silentSearchBound {
		cur := queue[0]
		queue = queue[1:]
		if goal(cur.m) {
			return cur.path, cur.m, true
		}
		for _, t := range n.silent {
			if !n.enabled(cur.m, t) {
				continue
			}
			next := n.fire(cur.m, t)
			k := next.key()
			if seen[k] {
				continue
			}
			seen[k] = true
			queue = append(queue, node{m: next, path: append(append([]int(nil), cur.path...), t)})
		}
	}
	return nil, nil, false
}

// --- token replay -----------------------------------------------------------

type replayer struct {
	net            *petriNet
	m              marking
	produced       int
	consumed       int
	missing        int
	remaining      int
	firstDeviation int
}

func newReplayer(n *petriNet) *replayer {
	// The initial token counts as produced, and the final one, in finish, as
	// consumed: a fitting trace then produces and consumes the same number.
	return &replayer{net: n, m: n.initial(), produced: 1, firstDeviation: -1}
}

func (r *replayer) fireCounted(t int) {
	r.consumed += len(r.net.trans[t].in)
	r.produced += len(r.net.trans[t].out)
	r.m = r.net.fire(r.m, t)
}

// event replays one event and reports whether it needed no missing token.
//
// The transition fired is chosen in this order: one the event names that is
// enabled now; else one that the fewest silent firings enable; else the one
// needing the fewest missing tokens, which are then inserted. Ties go to
// construction order, so the replay is deterministic. The choice is GREEDY,
// which is the standard token replay and its known limit: when one symbol
// labels several transitions a greedy pick can be the wrong one and blame an
// event that an alignment would sync. Align is the exact answer, and it is
// what names a deviation once this check has raised one.
func (r *replayer) event(i int, label string) bool {
	cands := r.net.byLabel[label]
	if len(cands) == 0 {
		// An event the model never names. It is replayed as a transition
		// with a private input place (its token is missing) and a private
		// output place (its token remains), which is how the fitness counts
		// an unexplainable event without a place in the net to put it.
		r.missing++
		r.consumed++
		r.produced++
		r.remaining++
		r.deviate(i)
		return false
	}
	for _, t := range cands {
		if r.net.enabled(r.m, t) {
			r.fireCounted(t)
			return true
		}
	}
	if path, _, ok := r.net.silentPath(r.m, func(m marking) bool {
		for _, t := range cands {
			if r.net.enabled(m, t) {
				return true
			}
		}
		return false
	}); ok {
		for _, s := range path {
			r.fireCounted(s)
		}
		for _, t := range cands {
			if r.net.enabled(r.m, t) {
				r.fireCounted(t)
				return true
			}
		}
	}
	best, bestMissing := cands[0], -1
	for _, t := range cands {
		need := 0
		for _, p := range r.net.trans[t].in {
			if r.m[p] < 1 {
				need++
			}
		}
		if bestMissing < 0 || need < bestMissing {
			best, bestMissing = t, need
		}
	}
	for _, p := range r.net.trans[best].in {
		if r.m[p] < 1 {
			r.m[p]++
			r.missing++
		}
	}
	r.fireCounted(best)
	r.deviate(i)
	return false
}

func (r *replayer) deviate(i int) {
	if r.firstDeviation < 0 {
		r.firstDeviation = i
	}
}

// finish drives the marking toward the final one by silent firings, consumes
// the final token (counting it missing when the sink is empty) and counts
// every token left anywhere else as remaining.
func (r *replayer) finish() {
	if path, _, ok := r.net.silentPath(r.m, marking.isFinal); ok {
		for _, s := range path {
			r.fireCounted(s)
		}
	} else if path, _, ok := r.net.silentPath(r.m, func(m marking) bool { return m[sinkPlace] > 0 }); ok {
		// The final marking is out of reach; reaching the sink at least
		// keeps the final token from also being counted missing.
		for _, s := range path {
			r.fireCounted(s)
		}
	}
	r.consumed++
	if r.m[sinkPlace] > 0 {
		r.m[sinkPlace]--
	} else {
		r.missing++
	}
	r.remaining += r.m.total()
}
