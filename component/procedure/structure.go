package procedure

import "sort"

// TreeOp is a process-tree operator.
type TreeOp string

const (
	// OpLeaf is one symbol.
	OpLeaf TreeOp = "leaf"
	// OpSeq is its children in order.
	OpSeq TreeOp = "seq"
	// OpXor is exactly one of its children.
	OpXor TreeOp = "xor"
	// OpAnd is all of its children, in any interleaving.
	OpAnd TreeOp = "and"
	// OpLoop is a body followed by any number of redo-then-body rounds.
	// Children are [body, redo].
	OpLoop TreeOp = "loop"
)

// ProcessTree is the structured shape of a corpus.
type ProcessTree struct {
	Op       TreeOp
	Symbol   string
	Children []*ProcessTree
}

// Structure runs the inductive miner over the corpus, per goal signature, so
// that a RETRY becomes a loop node.
//
// This stage exists because Mine cannot see repetition as repetition. A run
// that retried an action three times gives Mine a length-seven pattern, and
// lifting that produces an automation which hard-codes seven steps and three
// attempts -- correct about exactly one recording and wrong about the next.
//
// The cut order is exclusive choice, sequence, parallel, loop, and it is not
// negotiable: a different order finds a different tree for the same log, and
// two replicas structuring one corpus must agree. When no cut applies the
// answer is a FLOWER model -- a loop over a choice of every symbol -- and
// never nil, because nil would make an un-structurable corpus
// indistinguishable from an empty one.
func Structure(sequences [][]string, p Params) *ProcessTree {
	log := make([][]string, 0, len(sequences))
	for _, s := range sequences {
		trace := make([]string, 0, len(s))
		for _, sym := range s {
			if sym != "" {
				trace = append(trace, sym)
			}
		}
		log = append(log, trace)
	}
	if len(log) == 0 || countSymbols(log) == 0 {
		return nil
	}
	return induct(log, p, 0)
}

// maxInductionDepth stops a pathological log from recursing forever. A cut
// that fails to shrink its sub-logs would otherwise recurse on itself; the
// flower is the honest answer at that point.
const maxInductionDepth = 32

func induct(log [][]string, p Params, depth int) *ProcessTree {
	alphabet := distinctSymbols(log)
	switch {
	case len(alphabet) == 0:
		return nil
	case len(alphabet) == 1 && allTracesLength(log, 1):
		return &ProcessTree{Op: OpLeaf, Symbol: alphabet[0]}
	case depth >= maxInductionDepth:
		return flower(alphabet)
	}

	if t := exclusiveChoiceCut(log, p, depth); t != nil {
		return t
	}
	if t := sequenceCut(log, p, depth); t != nil {
		return t
	}
	if t := parallelCut(log, p, depth); t != nil {
		return t
	}
	if t := loopCut(log, p, depth); t != nil {
		return t
	}
	if len(alphabet) == 1 {
		// One symbol, traces of differing length: that is a loop over it.
		leaf := &ProcessTree{Op: OpLeaf, Symbol: alphabet[0]}
		return &ProcessTree{Op: OpLoop, Children: []*ProcessTree{leaf, {Op: OpSeq}}}
	}
	return flower(alphabet)
}

// exclusiveChoiceCut splits the alphabet into groups that never co-occur in a
// trace. It runs FIRST because a choice misread as a sequence puts both
// branches in every path.
func exclusiveChoiceCut(log [][]string, p Params, depth int) *ProcessTree {
	groups := connectedByCooccurrence(log)
	if len(groups) < 2 {
		return nil
	}
	children := make([]*ProcessTree, 0, len(groups))
	for _, g := range groups {
		sub := projectTracesContaining(log, g)
		if len(sub) == 0 {
			return nil
		}
		child := induct(sub, p, depth+1)
		if child == nil {
			return nil
		}
		children = append(children, child)
	}
	return &ProcessTree{Op: OpXor, Children: children}
}

// sequenceCut finds the leftmost position where the alphabet splits cleanly:
// every symbol before it precedes every symbol after it, in every trace.
func sequenceCut(log [][]string, p Params, depth int) *ProcessTree {
	alphabet := distinctSymbols(log)
	if len(alphabet) < 2 {
		return nil
	}
	precedes := precedenceMatrix(log)
	// Order the alphabet so that a valid prefix, if one exists, is a prefix of
	// this ordering.
	ordered := append([]string(nil), alphabet...)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		switch {
		case precedes[a][b] && !precedes[b][a]:
			return true
		case precedes[b][a] && !precedes[a][b]:
			return false
		default:
			return a < b
		}
	})
	for split := 1; split < len(ordered); split++ {
		left := map[string]bool{}
		for _, s := range ordered[:split] {
			left[s] = true
		}
		if !everyLeftPrecedesEveryRight(log, left) {
			continue
		}
		lsub, rsub := partitionTraces(log, left)
		lt, rt := induct(lsub, p, depth+1), induct(rsub, p, depth+1)
		if lt == nil || rt == nil {
			continue
		}
		return &ProcessTree{Op: OpSeq, Children: flattenSeq(lt, rt)}
	}
	return nil
}

// parallelCut splits the alphabet into groups that co-occur in every trace and
// interleave freely.
func parallelCut(log [][]string, p Params, depth int) *ProcessTree {
	alphabet := distinctSymbols(log)
	if len(alphabet) < 2 {
		return nil
	}
	groups := groupsByFreeInterleaving(log, alphabet)
	if len(groups) < 2 {
		return nil
	}
	children := make([]*ProcessTree, 0, len(groups))
	for _, g := range groups {
		set := map[string]bool{}
		for _, s := range g {
			set[s] = true
		}
		sub, _ := partitionTraces(log, set)
		child := induct(sub, p, depth+1)
		if child == nil {
			return nil
		}
		children = append(children, child)
	}
	return &ProcessTree{Op: OpAnd, Children: children}
}

// loopCut recognises a repeated body and the redo part between its rounds.
//
// It handles the SINGLE-SYMBOL body, and that narrowness is deliberate. The
// general cut needs the trace decomposed into rounds before it can recurse,
// and a version that recurses on a log still containing the repetition does
// not terminate -- the first draft of this function nested thirty loops until
// the depth cap stopped it, which is a wrong answer dressed as a deep one. A
// multi-symbol body falls through to the flower, which is the honest answer:
// "this recurred in a shape the cuts do not name".
//
// The claim that matters is the record's: a retry yields a LOOP node and not a
// length-seven pattern. That is the single-symbol case.
func loopCut(log [][]string, p Params, depth int) *ProcessTree {
	for _, body := range repeatingSymbols(log) {
		redo, ok := redoAround(log, body)
		if !ok {
			continue
		}
		var redoTree *ProcessTree
		if len(redo) == 0 {
			redoTree = &ProcessTree{Op: OpSeq}
		} else {
			redoTree = induct(redo, p, depth+1)
			if redoTree == nil {
				continue
			}
		}
		loop := &ProcessTree{
			Op:       OpLoop,
			Children: []*ProcessTree{{Op: OpLeaf, Symbol: body}, redoTree},
		}
		inLoop := map[string]bool{body: true}
		for _, gap := range redo {
			for _, sym := range gap {
				inLoop[sym] = true
			}
		}
		before, after := bracketsAround(log, map[string]bool{body: true}, inLoop)
		children := make([]*ProcessTree, 0, 3)
		if len(before) > 0 {
			children = append(children, leavesInOrder(before))
		}
		children = append(children, loop)
		if len(after) > 0 {
			children = append(children, leavesInOrder(after))
		}
		if len(children) == 1 {
			return loop
		}
		return &ProcessTree{Op: OpSeq, Children: flattenSeq(children...)}
	}
	return nil
}

// redoAround returns the contents of each gap between two rounds of body, and
// reports whether body is a legitimate loop body at all.
//
// It is NOT one when a symbol that appears inside a gap also appears outside
// every gap: that symbol belongs to the surrounding sequence as much as to the
// loop, and calling it the redo part would claim the loop swallows a step the
// recording put beside it.
func redoAround(log [][]string, body string) ([][]string, bool) {
	var gaps [][]string
	inGap := map[string]bool{}
	outGap := map[string]bool{}
	for _, trace := range log {
		last := -1
		for i, sym := range trace {
			if sym != body {
				continue
			}
			if last >= 0 {
				gap := append([]string(nil), trace[last+1:i]...)
				gaps = append(gaps, gap)
				for _, g := range gap {
					inGap[g] = true
				}
			}
			last = i
		}
		if last < 0 {
			continue
		}
		first := -1
		for i, sym := range trace {
			if sym == body {
				first = i
				break
			}
		}
		for _, sym := range trace[:first] {
			outGap[sym] = true
		}
		for _, sym := range trace[last+1:] {
			outGap[sym] = true
		}
	}
	for sym := range inGap {
		if outGap[sym] {
			return nil, false
		}
	}
	var nonEmpty [][]string
	for _, g := range gaps {
		if len(g) > 0 {
			nonEmpty = append(nonEmpty, g)
		}
	}
	return nonEmpty, true
}

func flower(alphabet []string) *ProcessTree {
	children := make([]*ProcessTree, len(alphabet))
	for i, s := range alphabet {
		children[i] = &ProcessTree{Op: OpLeaf, Symbol: s}
	}
	return &ProcessTree{
		Op:       OpLoop,
		Children: []*ProcessTree{{Op: OpXor, Children: children}, {Op: OpSeq}},
	}
}

// --- helpers over the log -------------------------------------------------

func countSymbols(log [][]string) int {
	n := 0
	for _, t := range log {
		n += len(t)
	}
	return n
}

func allTracesLength(log [][]string, n int) bool {
	for _, t := range log {
		if len(t) != n {
			return false
		}
	}
	return true
}

// connectedByCooccurrence groups symbols that ever appear in the same trace.
// Two groups that never co-occur are an exclusive choice.
func connectedByCooccurrence(log [][]string) [][]string {
	alphabet := distinctSymbols(log)
	parent := map[string]string{}
	var find func(string) string
	find = func(s string) string {
		if parent[s] == "" || parent[s] == s {
			parent[s] = s
			return s
		}
		parent[s] = find(parent[s])
		return parent[s]
	}
	for _, s := range alphabet {
		parent[s] = s
	}
	for _, trace := range log {
		for i := 1; i < len(trace); i++ {
			a, b := find(trace[0]), find(trace[i])
			if a != b {
				parent[a] = b
			}
		}
	}
	buckets := map[string][]string{}
	for _, s := range alphabet {
		r := find(s)
		buckets[r] = append(buckets[r], s)
	}
	roots := sortedKeys(toSet(buckets))
	out := make([][]string, 0, len(roots))
	for _, r := range roots {
		g := buckets[r]
		sort.Strings(g)
		out = append(out, g)
	}
	return out
}

func precedenceMatrix(log [][]string) map[string]map[string]bool {
	m := map[string]map[string]bool{}
	for _, s := range distinctSymbols(log) {
		m[s] = map[string]bool{}
	}
	for _, trace := range log {
		for i := 0; i < len(trace); i++ {
			for j := i + 1; j < len(trace); j++ {
				m[trace[i]][trace[j]] = true
			}
		}
	}
	return m
}

// everyLeftPrecedesEveryRight is the sequence-cut test, and it asks TWO
// things. The obvious one is ordering: nothing on the left may follow
// something on the right, in any trace. The second is that every trace must
// actually CONTAIN both sides -- a split some traces skip entirely is a
// CHOICE wearing a sequence's clothes, and accepting it renders
// `start (left|right) end` as the flat sequence `start left right end`, which
// claims every run did both.
func everyLeftPrecedesEveryRight(log [][]string, left map[string]bool) bool {
	for _, trace := range log {
		var sawLeft, sawRight bool
		for _, sym := range trace {
			if left[sym] {
				if sawRight {
					return false
				}
				sawLeft = true
			} else {
				sawRight = true
			}
		}
		if !sawLeft || !sawRight {
			return false
		}
	}
	return true
}

func partitionTraces(log [][]string, in map[string]bool) (inLog, outLog [][]string) {
	for _, trace := range log {
		var a, b []string
		for _, sym := range trace {
			if in[sym] {
				a = append(a, sym)
			} else {
				b = append(b, sym)
			}
		}
		if len(a) > 0 {
			inLog = append(inLog, a)
		}
		if len(b) > 0 {
			outLog = append(outLog, b)
		}
	}
	return inLog, outLog
}

func projectTracesContaining(log [][]string, group []string) [][]string {
	set := map[string]bool{}
	for _, s := range group {
		set[s] = true
	}
	var out [][]string
	for _, trace := range log {
		var keep []string
		for _, sym := range trace {
			if set[sym] {
				keep = append(keep, sym)
			}
		}
		if len(keep) > 0 {
			out = append(out, keep)
		}
	}
	return out
}

// groupsByFreeInterleaving returns groups whose symbols appear in every trace
// and in both orders relative to the other groups -- the signature of true
// concurrency rather than of a sequence.
func groupsByFreeInterleaving(log [][]string, alphabet []string) [][]string {
	precedes := precedenceMatrix(log)
	var groups [][]string
	assigned := map[string]bool{}
	for _, a := range alphabet {
		if assigned[a] {
			continue
		}
		group := []string{a}
		assigned[a] = true
		for _, b := range alphabet {
			if assigned[b] {
				continue
			}
			if precedes[a][b] && precedes[b][a] {
				continue // freely interleaved: a DIFFERENT group
			}
			group = append(group, b)
			assigned[b] = true
		}
		sort.Strings(group)
		groups = append(groups, group)
	}
	if len(groups) < 2 {
		return nil
	}
	for _, g := range groups {
		if !everyTraceContains(log, g) {
			return nil
		}
	}
	return groups
}

func everyTraceContains(log [][]string, group []string) bool {
	for _, trace := range log {
		found := false
		for _, sym := range trace {
			for _, g := range group {
				if sym == g {
					found = true
				}
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// repeatingSymbols are those that occur more than once in at least one trace.
func repeatingSymbols(log [][]string) []string {
	repeats := map[string]bool{}
	for _, trace := range log {
		seen := map[string]int{}
		for _, sym := range trace {
			seen[sym]++
			if seen[sym] > 1 {
				repeats[sym] = true
			}
		}
	}
	return sortedKeys(repeats)
}

// bracketsAround returns the symbols that always precede the loop and those
// that always follow it.
func bracketsAround(log [][]string, body, redo map[string]bool) (before, after []string) {
	b, a := map[string]bool{}, map[string]bool{}
	for _, trace := range log {
		first, last := -1, -1
		for i, sym := range trace {
			if body[sym] || redo[sym] {
				if first < 0 {
					first = i
				}
				last = i
			}
		}
		if first < 0 {
			continue
		}
		for _, sym := range trace[:first] {
			b[sym] = true
		}
		for _, sym := range trace[last+1:] {
			a[sym] = true
		}
	}
	return sortedKeys(b), sortedKeys(a)
}

func leavesInOrder(syms []string) *ProcessTree {
	if len(syms) == 1 {
		return &ProcessTree{Op: OpLeaf, Symbol: syms[0]}
	}
	kids := make([]*ProcessTree, len(syms))
	for i, s := range syms {
		kids[i] = &ProcessTree{Op: OpLeaf, Symbol: s}
	}
	return &ProcessTree{Op: OpSeq, Children: kids}
}

// flattenSeq keeps nested sequences flat, so seq(seq(a,b),c) prints and
// compares as seq(a,b,c). Two trees that mean the same thing must look the
// same, or the determinism test is measuring formatting.
func flattenSeq(nodes ...*ProcessTree) []*ProcessTree {
	var out []*ProcessTree
	for _, n := range nodes {
		if n.Op == OpSeq && len(n.Children) > 0 {
			out = append(out, n.Children...)
			continue
		}
		out = append(out, n)
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func toSet[T any](m map[string]T) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}
