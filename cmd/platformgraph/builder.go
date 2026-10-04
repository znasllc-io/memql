package main

import (
	"fmt"
	"reflect"

	"github.com/znasllc-io/memql/component/architecture/model"
)

// builder accumulates the graph across passes. A node added twice must be the
// same node -- two passes disagreeing about one id is a defect worth failing
// on, not a merge to resolve silently -- and an edge may be added to a node
// only once the node exists, which keeps a pass from inventing endpoints.
type builder struct {
	nodes map[model.ID]model.PlatformNode
	edges []model.PlatformEdge
	errs  []error
}

func newBuilder() *builder {
	return &builder{nodes: map[model.ID]model.PlatformNode{}}
}

func (b *builder) node(id model.ID, kind model.PlatformNodeKind, label string, attrs map[string]string) {
	n := model.PlatformNode{ID: id, Kind: kind, Label: label, Attrs: prune(attrs)}
	if prev, ok := b.nodes[id]; ok {
		if !reflect.DeepEqual(prev, n) {
			b.errs = append(b.errs, fmt.Errorf("node %s added twice with different content: %+v vs %+v", id, prev, n))
		}
		return
	}
	b.nodes[id] = n
}

func (b *builder) has(id model.ID) bool {
	_, ok := b.nodes[id]
	return ok
}

func (b *builder) edge(from, to model.ID, kind model.PlatformEdgeKind, attrs map[string]string) {
	if !b.has(from) || !b.has(to) {
		b.errs = append(b.errs, fmt.Errorf("edge %s -%s-> %s names a node no pass added", from, kind, to))
		return
	}
	b.edges = append(b.edges, model.PlatformEdge{From: from, To: to, Kind: kind, Attrs: prune(attrs)})
}

// graph returns the accumulated graph, validated, or the first defect.
func (b *builder) graph() (*model.PlatformGraph, error) {
	if len(b.errs) > 0 {
		return nil, b.errs[0]
	}
	g := &model.PlatformGraph{SchemaVersion: model.PlatformSchemaVersion}
	for _, n := range b.nodes {
		g.Nodes = append(g.Nodes, n)
	}
	g.Edges = append(g.Edges, b.edges...)
	if err := model.ValidatePlatformGraph(g); err != nil {
		return nil, err
	}
	return g, nil
}

// prune drops empty values and returns nil for an empty map, so "absent" has
// one spelling in the file.
func prune(attrs map[string]string) map[string]string {
	if len(attrs) == 0 {
		return nil
	}
	out := make(map[string]string, len(attrs))
	for k, v := range attrs {
		if v != "" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
