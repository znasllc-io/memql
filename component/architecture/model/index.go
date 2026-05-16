package model

// Index is a derived lookup layer over a Model. It's built on demand
// (renderers, the drill-down navigator) rather than persisted, so the
// on-disk format stays compact and adding new lookup shapes doesn't
// require a schema bump.
type Index struct {
	model    *Model
	byID     map[ID]*Node
	children map[ID][]*Node              // parent ID -> child nodes (via EdgeContains)
	outgoing map[ID]map[EdgeKind][]*Edge // from -> kind -> edges
	incoming map[ID]map[EdgeKind][]*Edge // to   -> kind -> edges
}

// NewIndex builds an Index over m. m must not be mutated for the
// lifetime of the Index; if you need a fresh view after editing,
// rebuild it.
func NewIndex(m *Model) *Index {
	ix := &Index{
		model:    m,
		byID:     make(map[ID]*Node, len(m.Nodes)),
		children: make(map[ID][]*Node),
		outgoing: make(map[ID]map[EdgeKind][]*Edge),
		incoming: make(map[ID]map[EdgeKind][]*Edge),
	}
	for i := range m.Nodes {
		n := &m.Nodes[i]
		ix.byID[n.ID] = n
	}
	for i := range m.Edges {
		e := &m.Edges[i]
		if e.Kind == EdgeContains {
			if child, ok := ix.byID[e.To]; ok {
				ix.children[e.From] = append(ix.children[e.From], child)
			}
		}
		if ix.outgoing[e.From] == nil {
			ix.outgoing[e.From] = make(map[EdgeKind][]*Edge)
		}
		ix.outgoing[e.From][e.Kind] = append(ix.outgoing[e.From][e.Kind], e)
		if ix.incoming[e.To] == nil {
			ix.incoming[e.To] = make(map[EdgeKind][]*Edge)
		}
		ix.incoming[e.To][e.Kind] = append(ix.incoming[e.To][e.Kind], e)
	}
	return ix
}

// Node returns the node with the given ID, or nil if not present.
func (ix *Index) Node(id ID) *Node { return ix.byID[id] }

// Children returns nodes that this node "contains" (via EdgeContains).
// Returned slice is owned by the Index; callers must not mutate it.
func (ix *Index) Children(id ID) []*Node { return ix.children[id] }

// Roots returns nodes that have no incoming EdgeContains -- typically
// the cluster(s) at the top of the C4 hierarchy.
func (ix *Index) Roots() []*Node {
	roots := []*Node{}
	for i := range ix.model.Nodes {
		n := &ix.model.Nodes[i]
		if n.Parent == "" {
			roots = append(roots, n)
		}
	}
	return roots
}

// Outgoing returns edges of the given kind leaving the node.
// Pass kind == "" to get edges of all kinds.
func (ix *Index) Outgoing(id ID, kind EdgeKind) []*Edge {
	m := ix.outgoing[id]
	if m == nil {
		return nil
	}
	if kind == "" {
		var out []*Edge
		for _, es := range m {
			out = append(out, es...)
		}
		return out
	}
	return m[kind]
}

// Incoming is the inverse of Outgoing.
func (ix *Index) Incoming(id ID, kind EdgeKind) []*Edge {
	m := ix.incoming[id]
	if m == nil {
		return nil
	}
	if kind == "" {
		var out []*Edge
		for _, es := range m {
			out = append(out, es...)
		}
		return out
	}
	return m[kind]
}

// NodesByKind returns all nodes whose Kind matches. Useful for
// renderers that draw one kind per pane (e.g. all services at L2,
// all packages within a service at L3).
func (ix *Index) NodesByKind(kind Kind) []*Node {
	var out []*Node
	for i := range ix.model.Nodes {
		n := &ix.model.Nodes[i]
		if n.Kind == kind {
			out = append(out, n)
		}
	}
	return out
}
