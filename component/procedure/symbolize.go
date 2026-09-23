package procedure

import (
	"fmt"
	"sort"
)

// Symbol is one cluster of same-tool actions, and its template is the
// generalization of every member.
type Symbol struct {
	// Id is the symbol's name in every sequence downstream.
	Id string `json:"id"`
	// Tool is the identity every member shares.
	Tool string `json:"tool"`
	// Template is the anti-unification of all Members' argument trees.
	Template *Node `json:"template"`
	// Members are indices into the actions slice Symbolize was given, in
	// ascending order. They index a corpus, so a stored procedure has no use
	// for them and they are omitted when empty.
	Members []int `json:"members,omitempty"`
}

// holeNamer hands out hole ids. It is a closure rather than a counter field so
// that AntiUnify stays a function over values: two calls with a fresh namer
// produce the same tree, which is what makes the distance comparable.
type holeNamer func() string

// NewHoleNamer is the exported constructor, for callers outside this package
// that need to drive AntiUnify -- the reference parity harness is the only
// one. It is exported rather than the type, so a caller can make a namer and
// can do nothing else with it.
func NewHoleNamer() func() string { return (func() string)(newHoleNamer()) }

func newHoleNamer() holeNamer {
	n := 0
	return func() string {
		n++
		return fmt.Sprintf("h%d", n)
	}
}

// AntiUnify returns the least general generalization of two argument trees and
// the DISTANCE between them: how much had to be given up to reach it.
//
// Objects pair by KEY and arrays by LONGEST COMMON SUBSEQUENCE. Pairing arrays
// positionally is the obvious implementation and it is wrong: one inserted
// argument shifts every later position, so `[a b c]` against `[a x b c]`
// reports three differences instead of one, and no two recordings of a command
// with an added flag ever cluster.
//
// Distance is weighted by the SIZE of what a hole replaces, so generalizing
// away a whole object costs more than generalizing one literal. A flat count
// would rank "these differ in one nested payload" as cheaply as "these differ
// in one filename", and the budget would stop meaning anything.
func AntiUnify(a, b *Node, next holeNamer) (*Node, int) {
	switch {
	case a == nil && b == nil:
		return nil, 0
	case a == nil || b == nil:
		present := a
		if present == nil {
			present = b
		}
		return HoleNode(next(), typeOf(present)), present.Size()
	}
	if a.Kind != b.Kind {
		return HoleNode(next(), widenType(typeOf(a), typeOf(b))), maxInt(a.Size(), b.Size())
	}
	switch a.Kind {
	case KindLit:
		if a.Lit == b.Lit {
			// The LEFT operand's hints survive: a number both sides agree on
			// is still a number, or the template renders and materializes it
			// as a string.
			return &Node{Kind: KindLit, Lit: a.Lit, LitType: a.LitType, Form: a.Form}, 0
		}
		return HoleNode(next(), widenType(typeOf(a), typeOf(b))), 1
	case KindHole:
		if a.HoleId == b.HoleId {
			return HoleNode(a.HoleId, a.HoleType), 0
		}
		return HoleNode(next(), widenType(a.HoleType, b.HoleType)), 1
	case KindObject:
		return antiUnifyObject(a, b, next)
	default:
		return antiUnifyArray(a, b, next)
	}
}

func antiUnifyObject(a, b *Node, next holeNamer) (*Node, int) {
	keys := unionKeys(a.Keys, b.Keys)
	m := make(map[string]*Node, len(keys))
	dist := 0
	for _, k := range keys {
		av, aok := childByKey(a, k)
		bv, bok := childByKey(b, k)
		switch {
		case aok && bok:
			g, d := AntiUnify(av, bv, next)
			m[k], dist = g, dist+d
		case aok:
			m[k], dist = HoleNode(next(), typeOf(av)), dist+av.Size()
		default:
			m[k], dist = HoleNode(next(), typeOf(bv)), dist+bv.Size()
		}
	}
	g := Obj(m)
	// The generalization of two JSON documents is a JSON document. Form is a
	// hint Equal ignores, so the LEFT operand's is kept: the template reads
	// the way its first instance was read.
	g.Form = a.Form
	return g, dist
}

// antiUnifyArray aligns two arrays on their longest common subsequence and
// generalizes everything outside it.
func antiUnifyArray(a, b *Node, next holeNamer) (*Node, int) {
	pairs := lcsPairs(a.Kids, b.Kids)
	var (
		kids []*Node
		dist int
		ai   int
		bi   int
	)
	// emitGap generalizes the unmatched runs before the next matched pair.
	emitGap := func(untilA, untilB int) {
		for ai < untilA || bi < untilB {
			switch {
			case ai < untilA && bi < untilB:
				g, d := AntiUnify(a.Kids[ai], b.Kids[bi], next)
				kids = append(kids, g)
				dist += d
				ai++
				bi++
			case ai < untilA:
				kids = append(kids, HoleNode(next(), typeOf(a.Kids[ai])))
				dist += a.Kids[ai].Size()
				ai++
			default:
				kids = append(kids, HoleNode(next(), typeOf(b.Kids[bi])))
				dist += b.Kids[bi].Size()
				bi++
			}
		}
	}
	for _, p := range pairs {
		emitGap(p[0], p[1])
		kids = append(kids, a.Kids[ai].Clone())
		ai++
		bi++
	}
	emitGap(len(a.Kids), len(b.Kids))
	g := Arr(kids...)
	// A generalized command line is still a command line: without the Form a
	// template's argv would materialize as a list, and a shell cannot run one.
	g.Form = a.Form
	return g, dist
}

// lcsPairs returns the index pairs of a longest common subsequence, in order.
// Equality is structural, so a matched element is one both sides actually
// recorded rather than one that merely sits at the same offset.
func lcsPairs(a, b []*Node) [][2]int {
	n, m := len(a), len(b)
	table := make([][]int, n+1)
	for i := range table {
		table[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i].Equal(b[j]) {
				table[i][j] = table[i+1][j+1] + 1
			} else {
				table[i][j] = maxInt(table[i+1][j], table[i][j+1])
			}
		}
	}
	var pairs [][2]int
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i].Equal(b[j]):
			pairs = append(pairs, [2]int{i, j})
			i++
			j++
		case table[i+1][j] >= table[i][j+1]:
			i++
		default:
			j++
		}
	}
	return pairs
}

// Symbolize clusters actions by anti-unification distance under the budget.
//
// Actions are grouped by TOOL first, and that is not an optimization: it is
// what makes "two unrelated tools never share a symbol" true by construction.
// A budget alone could not promise it -- two tools whose arguments happen to
// match would cluster at any budget that admits anything.
func Symbolize(actions []Action, p Params) []Symbol {
	byTool := map[string][]int{}
	var toolOrder []string
	for i, a := range actions {
		if _, seen := byTool[a.Tool]; !seen {
			toolOrder = append(toolOrder, a.Tool)
		}
		byTool[a.Tool] = append(byTool[a.Tool], i)
	}
	sort.Strings(toolOrder)

	var out []Symbol
	for _, tool := range toolOrder {
		for _, idx := range byTool[tool] {
			joined := false
			for si := range out {
				if out[si].Tool != tool {
					continue
				}
				merged, dist := AntiUnify(out[si].Template, actions[idx].Args, newHoleNamer())
				if dist > p.SymbolBudget {
					continue
				}
				// Re-anti-unify on every join, so the template stays the
				// generalization of the WHOLE cluster rather than of its
				// first two members.
				out[si].Template = merged
				out[si].Members = append(out[si].Members, idx)
				joined = true
				break
			}
			if !joined {
				out = append(out, Symbol{
					Tool:     tool,
					Template: actions[idx].Args.Clone(),
					Members:  []int{idx},
				})
			}
		}
	}
	for i := range out {
		sort.Ints(out[i].Members)
		out[i].Id = fmt.Sprintf("s%d", i)
	}
	return out
}

// SymbolSequence writes one run's actions as the symbol ids they clustered
// into, in recorded order. It is what Mine and Structure read.
func SymbolSequence(actions []Action, symbols []Symbol) []string {
	idOf := make(map[int]string, len(actions))
	for _, s := range symbols {
		for _, m := range s.Members {
			idOf[m] = s.Id
		}
	}
	out := make([]string, 0, len(actions))
	for i := range actions {
		if id, ok := idOf[i]; ok {
			out = append(out, id)
		}
	}
	return out
}

func unionKeys(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, k := range append(append([]string(nil), a...), b...) {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func childByKey(n *Node, key string) (*Node, bool) {
	for i, k := range n.Keys {
		if k == key {
			return n.Kids[i], true
		}
	}
	return nil, false
}

func typeOf(n *Node) string {
	switch n.Kind {
	case KindObject:
		return "object"
	case KindArray:
		return "array"
	case KindHole:
		return n.HoleType
	default:
		// A literal is the type it was recorded as, when that is a type a
		// hole can carry. A null and an unrecorded type read as a string,
		// the safe reading for a value that has to be sent somewhere.
		if n.LitType == "number" || n.LitType == "bool" {
			return n.LitType
		}
		return "string"
	}
}

// scalarTypes are the hole types a literal can have. Canonicalization spells
// every scalar as a string, which is why two DIFFERENT scalar types widen to
// "string" rather than to "mixed": the one spelling they share is a string.
var scalarTypes = map[string]bool{"string": true, "number": true, "bool": true}

// widenType is the widest of two observed types, which is HoleType's own
// contract.
//
// It replaced a fixed answer -- every literal hole was "string" and every
// hole re-unified against a literal was "mixed" -- and the difference is not
// cosmetic. Generalize anti-unifies the growing template against each further
// instance, so from the THIRD instance on every hole met a literal and became
// "mixed", which the renderer writes as `any` and Materialize can only send as
// a string. A number hole meeting one more number has observed nothing wider.
// The distance and the hole numbering are unchanged; only the type is.
func widenType(a, b string) string {
	switch {
	case a == b:
		return a
	case scalarTypes[a] && scalarTypes[b]:
		return "string"
	default:
		return "mixed"
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
