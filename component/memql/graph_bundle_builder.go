package memql

import (
	"strings"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// Graph edge labels -- the closed vocabulary a traversal writes to
// GraphEdge.type on the wire, and the single source of truth for it
// (memql#3657). Every addEdge call site emits from these constants,
// docs/public/language/memql.md documents exactly this set, and
// TestGraphEdgeLabelsMatchPublicDocs pins the two together so neither can
// move without the other.
//
// This is NOT the @relationship `type` vocabulary (canonicalRelationshipType
// in relations.go). The two overlap but differ at both ends:
//
//   - `parent` is declarable and never emitted. It surfaces as `child`,
//     because an edge is written in the direction it was traversed.
//   - `dependsOn` / `formedFrom` are declarable types whose graph-expansion
//     traversal is deliberately unwired, so they emit nothing at all.
//
// Where a label does mirror a relationship type it is DEFINED from that
// constant rather than restating the string, so renaming the type moves the
// wire label with it and the docs pin then forces the documentation to follow.
// Two copies of one vocabulary is exactly how this one went wrong.
const (
	GraphEdgeLabelChild         = "child"
	GraphEdgeLabelAlias         = relationshipTypeAlias
	GraphEdgeLabelEquals        = relationshipTypeEquals
	GraphEdgeLabelInteractsWith = relationshipTypeInteracts
	GraphEdgeLabelCreatedBy     = relationshipTypeCreatedBy
	GraphEdgeLabelContains      = relationshipTypeContains
	GraphEdgeLabelOwns          = relationshipTypeOwns
)

// GraphEdgeLabels returns every label the runtime emits on GraphEdge.type,
// in the order expandGraph walks the edge kinds.
func GraphEdgeLabels() []string {
	return []string{
		GraphEdgeLabelChild,
		GraphEdgeLabelAlias,
		GraphEdgeLabelEquals,
		GraphEdgeLabelInteractsWith,
		GraphEdgeLabelCreatedBy,
		GraphEdgeLabelContains,
		GraphEdgeLabelOwns,
	}
}

type graphBundleBuilder struct {
	nodes    map[string]*memqlv1.MemoryNode
	order    []string
	edges    []*memqlv1.GraphEdge
	rootList []string
	rootSet  map[string]struct{}
}

func newGraphBundleBuilder() *graphBundleBuilder {
	return &graphBundleBuilder{
		nodes:    make(map[string]*memqlv1.MemoryNode),
		order:    make([]string, 0),
		edges:    make([]*memqlv1.GraphEdge, 0),
		rootList: make([]string, 0),
		rootSet:  make(map[string]struct{}),
	}
}

func (b *graphBundleBuilder) addNode(node *memqlv1.MemoryNode) {
	if b == nil || node == nil {
		return
	}
	id := strings.TrimSpace(node.Id)
	if id == "" {
		return
	}
	if _, exists := b.nodes[id]; exists {
		return
	}
	b.nodes[id] = cloneMemoryNode(node)
	b.order = append(b.order, id)
}

// addEdge appends one traversed edge.
//
// label is the STRUCTURAL edge label and must be a GraphEdgeLabel* constant --
// TestGraphEdgeLabelsAreEmittedFromTheConstantSet fails on a bare string
// literal reaching this call. as is the DOMAIN label the traversed
// relationship declared (memql#3652 / #3656), empty when it declared none.
//
// The two are independent axes and are deliberately not interchangeable: an
// edge is `child` because of what the engine did with it, and `assignedTo`
// because of what its author said it means.
func (b *graphBundleBuilder) addEdge(fromId, toId, label, as string, depth int) {
	if b == nil {
		return
	}
	source := strings.TrimSpace(fromId)
	target := strings.TrimSpace(toId)
	edgeLabel := strings.TrimSpace(label)
	if source == "" || target == "" || edgeLabel == "" {
		return
	}
	edge := &memqlv1.GraphEdge{
		Type:   edgeLabel,
		FromId: source,
		ToId:   target,
		As:     strings.TrimSpace(as),
	}
	if depth >= 0 {
		edge.Depth = int32(depth)
	}
	b.edges = append(b.edges, edge)
}

func (b *graphBundleBuilder) toBundle() *memqlv1.GraphBundle {
	if b == nil {
		return &memqlv1.GraphBundle{}
	}

	var nodes []*memqlv1.MemoryNode
	for _, id := range b.order {
		if node, ok := b.nodes[id]; ok {
			nodes = append(nodes, node)
		}
	}

	var edges []*memqlv1.GraphEdge
	if len(b.edges) > 0 {
		edges = append(edges, b.edges...)
	}

	var rootIds []string
	if len(b.rootList) > 0 {
		rootIds = append(rootIds, b.rootList...)
	}

	return &memqlv1.GraphBundle{
		Nodes:   nodes,
		Edges:   edges,
		RootIds: rootIds,
	}
}

func (b *graphBundleBuilder) addRoot(id string) {
	if b == nil {
		return
	}
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return
	}
	if _, exists := b.rootSet[trimmed]; exists {
		return
	}
	b.rootSet[trimmed] = struct{}{}
	b.rootList = append(b.rootList, trimmed)
}
