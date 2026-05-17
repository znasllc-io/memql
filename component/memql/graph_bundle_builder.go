package memql

import (
	"strings"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

const (
	graphEdgeTypeChild = "child"
)

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

func (b *graphBundleBuilder) addEdge(fromId, toId, relType string, depth int) {
	if b == nil {
		return
	}
	source := strings.TrimSpace(fromId)
	target := strings.TrimSpace(toId)
	edgeType := strings.TrimSpace(relType)
	if source == "" || target == "" || edgeType == "" {
		return
	}
	edge := &memqlv1.GraphEdge{
		Type:   edgeType,
		FromId: source,
		ToId:   target,
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
