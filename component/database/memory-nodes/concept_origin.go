package memoryNodes

import "github.com/znasllc-io/memql/component/language/parser"

// The data-origins accessors (epic memql#4378). The Concept struct
// stores the two DECLARED values; every consumer that needs to know
// what they MEAN comes through here, so the three states are derived in
// exactly one place (parser.OriginDecl.State) no matter who asks.
//
// A second derivation is the specific failure this indirection exists
// to prevent: the write guard, the row-admission rule, the registry
// broadcast, the SDKs and the portal badge all answer "is this a
// mirror?", and a copy that disagreed would show a reader one badge
// while the engine enforced another.

// OriginDecl returns the concept's data-origins declaration. The zero
// value is meaningful: it is a NATIVE concept, which is what an
// undeclared concept is.
func (c *Concept) OriginDecl() parser.OriginDecl {
	if c == nil {
		return parser.OriginDecl{}
	}
	return parser.OriginDecl{Origin: c.Origin, MirroredTo: c.MirroredTo}
}

// DataState returns the derived relationship between MemQL and this
// concept's data: mirror, origin, or native.
func (c *Concept) DataState() parser.DataState {
	return c.OriginDecl().State()
}

// EffectiveOrigin returns the origin name a client should be shown:
// the declared connector, or "memql" when MemQL originates the data.
// Unlike the stored Origin field this is never empty, which is what
// lets a wire consumer render a badge without re-implementing the
// default.
func (c *Concept) EffectiveOrigin() string {
	d := c.OriginDecl()
	if d.IsMemQLOrigin() {
		return parser.OriginMemQL
	}
	return d.Origin
}

// IsMirror reports whether this concept is a faithful copy of data
// owned elsewhere -- the state that makes it read-only by construction.
func (c *Concept) IsMirror() bool {
	return c.DataState() == parser.DataStateMirror
}

// DeclaredConnectors returns every connector name the concept names --
// its external origin, plus every mirror target -- in a stable order.
// This is the set the boot check resolves against the connector
// registry.
func (c *Concept) DeclaredConnectors() []string {
	return c.OriginDecl().Connectors()
}
