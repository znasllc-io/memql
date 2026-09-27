package procedure

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// serialize.go is the STORED form of a learned procedure's values: the
// template a replay executes and the process model it checks its live trace
// against. Both are written onto the construct (construct.procedure), hashed
// into the procedureHash a promotion approval pins, and read back by the
// replay runner and the OS.
//
// So the encoding is a contract with three readers, and three rules follow:
//
//   - It is EXPLICIT. A Node is {"kind": "lit"|"object"|"array"|"hole", ...}
//     with lowerCamelCase fields named after the Go ones (lit, litType, raw,
//     form, keys, kids, seps, holeId, holeType), omitted when empty, so a
//     TypeScript reader needs no knowledge of Go's enum values -- and a
//     payload written before a field existed encodes and decodes exactly as
//     it did. serialize_test.go pins the exact bytes.
//   - It is STABLE. The same value always encodes to the same bytes -- object
//     keys stay in the node's own (sorted) order -- because a hash over it is
//     a version pin.
//   - It FAILS CLOSED, both ways. A kind, form, literal type, hole type or
//     operator this code does not know is refused on decode rather than read
//     as something close to it: a replay that ignored an unknown Form would
//     send a list where a command line was recorded. Encoding refuses the
//     same things, so a malformed tree fails where it was built rather than
//     on the replica that later replays it.
//
// Node and ProcessTree implement json.Marshaler and json.Unmarshaler, and the
// other value types carry json tags, so a caller that embeds them in a larger
// payload gets exactly the encoding and the checks MarshalTemplate and
// MarshalTree give.

// Digest is the version digest a stored procedure is pinned by: "sha256:" and
// the lowercase hex SHA-256 of the bytes it is handed -- the spelling
// component/work.BindingDigest gives a binding, so every digest the ladder
// stores reads alike.
//
// It lives HERE, beside the encoding it is taken over, rather than in the
// wiring that computes procedureHash: integrations/ may not import
// crypto/sha256 (integrations/sha256_conformance_test.go), and a version pin
// computed in two places is two versions. The caller decides WHAT is hashed
// -- canonical bytes, keys sorted -- and this decides only how.
func Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// MarshalTemplate encodes a template: {"steps":[{"tool","args"}],"holes":[...]}.
func MarshalTemplate(t Template) ([]byte, error) {
	return json.Marshal(t)
}

// UnmarshalTemplate decodes MarshalTemplate's encoding, checking every node.
func UnmarshalTemplate(b []byte) (Template, error) {
	var t Template
	if err := json.Unmarshal(b, &t); err != nil {
		return Template{}, fmt.Errorf("procedure: decoding a template: %w", err)
	}
	return t, nil
}

// MarshalTree encodes a process tree: {"op","symbol","children"}; nil is null.
func MarshalTree(t *ProcessTree) ([]byte, error) {
	return json.Marshal(t)
}

// UnmarshalTree decodes MarshalTree's encoding, checking every node. null
// decodes to nil, which fitness.go reads as the empty procedure.
func UnmarshalTree(b []byte) (*ProcessTree, error) {
	var t *ProcessTree
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("procedure: decoding a process tree: %w", err)
	}
	return t, nil
}

// --- Node -------------------------------------------------------------------

var kindNames = map[NodeKind]string{
	KindLit:    "lit",
	KindObject: "object",
	KindArray:  "array",
	KindHole:   "hole",
}

var kindsByName = map[string]NodeKind{
	"lit":    KindLit,
	"object": KindObject,
	"array":  KindArray,
	"hole":   KindHole,
}

var knownForms = map[string]bool{"": true, FormArgv: true, FormPath: true, FormRootedPath: true, FormJSON: true}

var knownLitTypes = map[string]bool{"": true, "string": true, "number": true, "bool": true, "null": true}

var knownHoleTypes = map[string]bool{"": true, "string": true, "number": true, "bool": true, "object": true, "array": true, "mixed": true}

// wireNode is a Node as it is stored. Kids stay *Node, so each child is
// encoded and checked by the same methods.
type wireNode struct {
	Kind     string   `json:"kind"`
	Lit      string   `json:"lit,omitempty"`
	LitType  string   `json:"litType,omitempty"`
	Raw      string   `json:"raw,omitempty"`
	Form     string   `json:"form,omitempty"`
	Keys     []string `json:"keys,omitempty"`
	Kids     []*Node  `json:"kids,omitempty"`
	Seps     []string `json:"seps,omitempty"`
	HoleId   string   `json:"holeId,omitempty"`
	HoleType string   `json:"holeType,omitempty"`
}

// MarshalJSON encodes one node after checking it.
func (n Node) MarshalJSON() ([]byte, error) {
	if err := n.check(); err != nil {
		return nil, err
	}
	return json.Marshal(wireNode{
		Kind:     kindNames[n.Kind],
		Lit:      n.Lit,
		LitType:  n.LitType,
		Raw:      n.Raw,
		Form:     n.Form,
		Keys:     n.Keys,
		Kids:     n.Kids,
		Seps:     n.Seps,
		HoleId:   n.HoleId,
		HoleType: n.HoleType,
	})
}

// UnmarshalJSON decodes one node and checks it.
func (n *Node) UnmarshalJSON(b []byte) error {
	var w wireNode
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	kind, ok := kindsByName[w.Kind]
	if !ok {
		return fmt.Errorf("procedure: unknown node kind %q", w.Kind)
	}
	*n = Node{
		Kind:     kind,
		Lit:      w.Lit,
		LitType:  w.LitType,
		Raw:      w.Raw,
		Form:     w.Form,
		Keys:     w.Keys,
		Kids:     w.Kids,
		Seps:     w.Seps,
		HoleId:   w.HoleId,
		HoleType: w.HoleType,
	}
	return n.check()
}

// check is what a stored node must satisfy. Every field belongs to exactly
// the kinds that use it: a literal carrying children, or a hole with no id,
// is a tree Materialize and Bind would each read differently.
//
// The spelling of a command line is held to its tree here, in both
// directions. A spelling is what a replay SENDS; the tree is what a person
// approved and what the replay's input check reads. So an argument's spelling
// must read back as exactly its own value, and the separators must be only
// the whitespace between the arguments -- a spelling the tree does not
// describe is a command that would run unseen.
func (n Node) check() error {
	if _, ok := kindNames[n.Kind]; !ok {
		return fmt.Errorf("procedure: unknown node kind %d", n.Kind)
	}
	if !knownForms[n.Form] {
		return fmt.Errorf("procedure: unknown node form %q", n.Form)
	}
	if !knownLitTypes[n.LitType] {
		return fmt.Errorf("procedure: unknown literal type %q", n.LitType)
	}
	if !knownHoleTypes[n.HoleType] {
		return fmt.Errorf("procedure: unknown hole type %q", n.HoleType)
	}
	scalar := n.Kind == KindLit || n.Kind == KindHole
	switch {
	case scalar && (len(n.Keys) > 0 || len(n.Kids) > 0 || n.Form != ""):
		return fmt.Errorf("procedure: a %s node carries children or a form", kindNames[n.Kind])
	case n.Kind != KindLit && (n.Lit != "" || n.LitType != ""):
		return fmt.Errorf("procedure: a %s node carries a literal", kindNames[n.Kind])
	case n.Kind != KindHole && (n.HoleId != "" || n.HoleType != ""):
		return fmt.Errorf("procedure: a %s node carries a hole id or type", kindNames[n.Kind])
	case n.Kind == KindHole && n.HoleId == "":
		return fmt.Errorf("procedure: a hole node has no id")
	case n.Kind == KindArray && len(n.Keys) > 0:
		return fmt.Errorf("procedure: an array node carries keys")
	case n.Kind == KindObject && len(n.Keys) != len(n.Kids):
		return fmt.Errorf("procedure: an object node has %d keys and %d values", len(n.Keys), len(n.Kids))
	case n.Kind != KindLit && n.Raw != "":
		return fmt.Errorf("procedure: a %s node carries a spelling", kindNames[n.Kind])
	case n.Seps != nil && !(n.Kind == KindArray && n.Form == FormArgv):
		return fmt.Errorf("procedure: a %s node that is not a command line carries separators", kindNames[n.Kind])
	}
	if n.Seps != nil {
		if err := checkSeps(n.Seps, len(n.Kids)); err != nil {
			return err
		}
	}
	argv := n.Kind == KindArray && n.Form == FormArgv
	for i, k := range n.Kids {
		switch {
		case k == nil:
			return fmt.Errorf("procedure: child %d of a %s node is null", i, kindNames[n.Kind])
		case k.Raw != "" && !argv:
			return fmt.Errorf("procedure: child %d carries a spelling, and only a command line's arguments have one", i)
		case k.Raw != "" && !spelledAs(k.Raw, k.Lit):
			return fmt.Errorf("procedure: argument %d is spelled %q, which does not read back as its value %q", i, k.Raw, k.Lit)
		}
	}
	return nil
}

// --- ProcessTree --------------------------------------------------------------

var knownOps = map[TreeOp]bool{OpLeaf: true, OpSeq: true, OpXor: true, OpAnd: true, OpLoop: true}

// processTreeFields is ProcessTree without its methods, so encoding it does
// not recurse into MarshalJSON; its children are still *ProcessTree and are
// checked one by one.
type processTreeFields ProcessTree

// MarshalJSON encodes one tree node after checking it.
func (t ProcessTree) MarshalJSON() ([]byte, error) {
	if err := t.check(); err != nil {
		return nil, err
	}
	return json.Marshal(processTreeFields(t))
}

// UnmarshalJSON decodes one tree node and checks it.
func (t *ProcessTree) UnmarshalJSON(b []byte) error {
	var f processTreeFields
	if err := json.Unmarshal(b, &f); err != nil {
		return err
	}
	*t = ProcessTree(f)
	return t.check()
}

// check is what a stored tree node must satisfy: a known operator, a leaf with
// a symbol and no children, an operator with no symbol, no null child, and a
// loop with at least its body.
func (t ProcessTree) check() error {
	if !knownOps[t.Op] {
		return fmt.Errorf("procedure: unknown process-tree operator %q", t.Op)
	}
	switch {
	case t.Op == OpLeaf && (t.Symbol == "" || len(t.Children) > 0):
		return fmt.Errorf("procedure: a leaf needs a symbol and no children")
	case t.Op != OpLeaf && t.Symbol != "":
		return fmt.Errorf("procedure: a %s node carries a symbol", t.Op)
	case t.Op == OpLoop && len(t.Children) == 0:
		return fmt.Errorf("procedure: a loop has no body")
	}
	for i, c := range t.Children {
		if c == nil {
			return fmt.Errorf("procedure: child %d of a %s node is null", i, t.Op)
		}
	}
	return nil
}
