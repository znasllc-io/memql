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
	// LitType is the scalar's ORIGINAL type -- string, number, bool or null --
	// carried for one purpose: writing the value back, as MemQL (where a
	// number must not acquire quotes) or as the Go value a replay sends
	// (Materialize, where a null must not become "").
	//
	// Equal DELIBERATELY IGNORES IT. Canonicalization folds every scalar to
	// one string spelling so that a 1 from JSON and a 1 from argv compare
	// equal, which is what lets two recordings of the same call generalize;
	// making the type part of equality would undo that. So this field is a
	// rendering hint and never a semantic one, and an empty value means
	// "nobody said", which renders as a string.
	LitType string
	// Raw is an argv token's exact source spelling -- `"$HOME/My Docs"` for
	// the argument $HOME/My Docs -- set on the arguments of a FormArgv
	// command line and nowhere else. The value (Lit) is what the program
	// received after quote removal; the spelling is what the SHELL was handed,
	// expansions, kept backslashes and all, and it is the only thing that can
	// send that command again: a tree re-quoted from its values turns
	// `"$(date +%F)"` into the literal text and `"\d+"` into d+.
	//
	// Equal DELIBERATELY IGNORES IT, for the reason it ignores LitType: two
	// recordings of one argument spelled differently are the same argument,
	// and they must generalize. A rendering hint, never a semantic one; empty
	// means a payload from before it, which Materialize re-quotes as it
	// always did.
	Raw string
	// Keys are sorted for KindObject and align with Kids.
	Keys []string
	Kids []*Node
	// Seps is the exact text around a FormArgv command line's arguments, one
	// more entry than Kids: before the first, each gap, after the last. With
	// the spelling of every argument it IS the recorded command, byte for
	// byte -- the newlines of a heredoc, a line continuation, the spacing --
	// where single spaces would collapse a heredoc onto one line.
	//
	// Equal DELIBERATELY IGNORES IT, like Raw and Form; nil means a payload
	// from before it (or a generalization whose arguments are no longer the
	// first instance's one for one), which Materialize joins with single
	// spaces.
	Seps []string
	// HoleId names the hole for KindHole; Classify fills in the rest.
	HoleId string
	// HoleType is the widest type observed at this position: string, number,
	// bool, object, array, or mixed.
	HoleType string
	// Form is how canonicalization READ the string this subtree came from:
	// FormArgv (a command line), FormPath or FormRootedPath (a path, the
	// second one with its leading slash), FormJSON (a JSON document), or ""
	// (the subtree was never a string). It exists for one purpose: turning the
	// tree back into the value a replay has to send (Materialize).
	//
	// Equal DELIBERATELY IGNORES IT, for the reason it ignores LitType: two
	// recordings whose trees agree are the same action however their strings
	// were spelled, and making the reading part of identity would stop them
	// generalizing. A rendering hint, never a semantic one.
	Form string
}

// The readings canonicalization records on a parsed string (Node.Form).
const (
	// FormArgv is a command line split into arguments. It materializes as
	// ONE string: the recorded spelling of every argument (Raw) and the text
	// between them (Seps) where the recording kept them, so a command the
	// template holds whole is sent exactly as it was recorded.
	FormArgv = "argv"
	// FormPath is a relative path split on "/". It materializes joined.
	FormPath = "path"
	// FormRootedPath is a path that began with "/". The leading slash is not
	// a segment -- two recordings differing only in a root would otherwise
	// differ in a segment that is always empty -- so the Form is where it is
	// kept.
	FormRootedPath = "rootedPath"
	// FormJSON is a JSON object or array that arrived as a string. It
	// materializes as JSON text.
	FormJSON = "json"
)

// Lit builds a scalar node whose original type is unrecorded, which renders
// as a string.
func Lit(s string) *Node { return &Node{Kind: KindLit, Lit: s} }

// LitOf builds a scalar node carrying its original type: "string", "number",
// "bool" or "null".
func LitOf(s, litType string) *Node { return &Node{Kind: KindLit, Lit: s, LitType: litType} }

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
// LitType, Raw, Seps and Form describe spelling, not identity, and are not
// compared.
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
	c := &Node{Kind: n.Kind, Lit: n.Lit, LitType: n.LitType, Raw: n.Raw, HoleId: n.HoleId, HoleType: n.HoleType, Form: n.Form}
	if len(n.Keys) > 0 {
		c.Keys = append([]string(nil), n.Keys...)
	}
	if n.Seps != nil {
		c.Seps = append(make([]string, 0, len(n.Seps)), n.Seps...)
	}
	if len(n.Kids) > 0 {
		c.Kids = make([]*Node, len(n.Kids))
		for i, k := range n.Kids {
			c.Kids[i] = k.Clone()
		}
	}
	return c
}
