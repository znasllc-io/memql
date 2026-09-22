package procedure

import "sort"

// Step is one recorded step, the pure value the wiring maps a v1:work:step row
// onto. It is defined HERE rather than taken from the row because this module
// may not read a row: the mapping is integrations/procedure's job, and the
// boundary is what lets every algorithm below be tested on a literal.
type Step struct {
	RunId string
	Key   string
	Seq   int
	// StepType is what the step IS. For a recorded app session it is the
	// action -- exec, fs_write, fs_read, fetch, mcp, app_answer. For a
	// compiled statement it is the statement type, and `automation` is the
	// one D24's second corpus level cares about.
	StepType string
	Call     Call
	Input    map[string]any
	// ResultDigest is the row's resultFingerprint: a digest rather than the
	// result, because the spine stays about actions.
	ResultDigest string
	// EffectDigest is a digest of the observed footprint. Two actions with the
	// same tool and arguments but different effects are not the same action.
	EffectDigest string
	// ResultValue is the trimmed result, when the row carried one. Data-flow
	// classification needs the VALUE to see that a later literal came from it;
	// where only a digest survives, the digest is what gets compared.
	ResultValue any
	// Consumed is true when some later step's arguments referenced this step's
	// result. Only the wiring can know it -- it needs the whole run -- so it
	// is carried in rather than derived here.
	Consumed bool
	// GoalSignature is the run's key, component/work.GoalSignature. Mining
	// groups by it, so two unrelated goals never blend into one procedure.
	GoalSignature string
}

// Call is what a step invoked, by name only.
type Call struct {
	Construct string
	Name      string
}

// Action is a canonicalized step: an identity plus an argument TREE.
type Action struct {
	// Tool is the canonical identity. An app action is its StepType; a subrun
	// step is "automation:" + the construct name, so D24's two levels share
	// one vocabulary and the pipeline never has to ask which writer wrote a
	// row.
	Tool         string
	Args         *Node
	ResultDigest string
	EffectDigest string
	Key          string
	Seq          int
	// ResultValue is the step's trimmed result, carried through from the row
	// so that Classify can see a later literal EQUAL an earlier result. The
	// digests answer "is this the same action"; only the value answers "where
	// did this argument come from".
	ResultValue any
}

// NodeKind is what a node in an argument tree is.
type NodeKind uint8

const (
	// KindLit is a scalar, held as its string spelling. One representation,
	// because 1 arriving from JSON and 1 arriving from argv must compare equal
	// or two recordings of the same command never generalize.
	KindLit NodeKind = iota
	KindObject
	KindArray
	// KindHole is a position where instances disagreed.
	KindHole
)

// Node is one node of an argument tree.
type Node struct {
	Kind NodeKind
	Lit  string
	// Keys are sorted for KindObject and align with Kids.
	Keys []string
	Kids []*Node
	// HoleId names the hole for KindHole; Classify fills in the rest.
	HoleId string
	// HoleType is the widest type observed at this position: string, number,
	// bool, object, array, or mixed.
	HoleType string
}

// Lit builds a scalar node.
func Lit(s string) *Node { return &Node{Kind: KindLit, Lit: s} }

// Arr builds an array node.
func Arr(kids ...*Node) *Node { return &Node{Kind: KindArray, Kids: kids} }

// Obj builds an object node with its keys sorted, which is what makes Equal
// order-independent and the whole tree comparable.
func Obj(m map[string]*Node) *Node {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	kids := make([]*Node, len(keys))
	for i, k := range keys {
		kids[i] = m[k]
	}
	return &Node{Kind: KindObject, Keys: keys, Kids: kids}
}

// HoleNode builds a hole node. It is HoleNode rather than Hole because Hole
// is the CLASSIFIED hole in generalize.go -- a position plus what explains it
// -- and the two are different enough that sharing a name would be a bug
// waiting for a reader in a hurry.
func HoleNode(id, typ string) *Node { return &Node{Kind: KindHole, HoleId: id, HoleType: typ} }

// Equal reports structural equality. A hole equals only a hole with the same
// id: two templates open at different positions are different templates.
func (n *Node) Equal(o *Node) bool {
	switch {
	case n == nil || o == nil:
		return n == o
	case n.Kind != o.Kind:
		return false
	}
	switch n.Kind {
	case KindLit:
		return n.Lit == o.Lit
	case KindHole:
		return n.HoleId == o.HoleId
	case KindObject:
		if len(n.Keys) != len(o.Keys) {
			return false
		}
		for i := range n.Keys {
			if n.Keys[i] != o.Keys[i] || !n.Kids[i].Equal(o.Kids[i]) {
				return false
			}
		}
		return true
	default: // KindArray
		if len(n.Kids) != len(o.Kids) {
			return false
		}
		for i := range n.Kids {
			if !n.Kids[i].Equal(o.Kids[i]) {
				return false
			}
		}
		return true
	}
}

// Size counts every node in the tree. It is the unit the compression score is
// denominated in, which is why a hole costs the same as a literal here and is
// priced differently in score.go: Size measures the SHAPE, and what a hole
// costs the caller is a separate question with a separate answer.
func (n *Node) Size() int {
	if n == nil {
		return 0
	}
	total := 1
	for _, k := range n.Kids {
		total += k.Size()
	}
	return total
}

// At walks a path of object keys and array indices, reporting a MISS rather
// than an empty node -- absent and empty are different answers, and a
// data-flow reference that silently resolved to empty would explain a hole
// that nothing explains.
func (n *Node) At(path []string) (*Node, bool) {
	cur := n
	for _, seg := range path {
		if cur == nil {
			return nil, false
		}
		found := false
		switch cur.Kind {
		case KindObject:
			for i, k := range cur.Keys {
				if k == seg {
					cur, found = cur.Kids[i], true
					break
				}
			}
		case KindArray:
			idx, ok := atoiIndex(seg)
			if ok && idx < len(cur.Kids) {
				cur, found = cur.Kids[idx], true
			}
		}
		if !found {
			return nil, false
		}
	}
	return cur, cur != nil
}

// atoiIndex parses a non-negative decimal array index. It is deliberately
// stricter than strconv.Atoi: a leading sign or a stray space is a path
// segment that was never an index, and accepting it would resolve a reference
// the recording never made.
func atoiIndex(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}

// Clone returns a deep copy. Anti-unification and corpus rewriting both build
// new trees from old ones, and sharing a subtree between a template and the
// corpus it was mined from makes a later rewrite mutate its own evidence.
func (n *Node) Clone() *Node {
	if n == nil {
		return nil
	}
	c := &Node{Kind: n.Kind, Lit: n.Lit, HoleId: n.HoleId, HoleType: n.HoleType}
	if len(n.Keys) > 0 {
		c.Keys = append([]string(nil), n.Keys...)
	}
	if len(n.Kids) > 0 {
		c.Kids = make([]*Node, len(n.Kids))
		for i, k := range n.Kids {
			c.Kids[i] = k.Clone()
		}
	}
	return c
}
