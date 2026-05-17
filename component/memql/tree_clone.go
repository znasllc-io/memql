package memql

import (
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

func cloneExecuteResult(result *ExecuteResult) *ExecuteResult {
	if result == nil {
		return nil
	}

	clone := &ExecuteResult{
		output:        result.output,
		includeBundle: result.includeBundle,
		startTime:     result.startTime,
	}
	if result.Bundle != nil {
		clone.Bundle = cloneGraphBundle(result.Bundle)
	}
	if result.Meta != nil {
		clone.Meta = &ResultMeta{
			Count:    result.Meta.Count,
			HasMore:  result.Meta.HasMore,
			Cursor:   result.Meta.Cursor,
			TookMs:   result.Meta.TookMs,
			ClientId: result.Meta.ClientId,
			ServerId: result.Meta.ServerId,
			Version:  result.Meta.Version,
		}
	}
	return clone
}

func cloneGraphBundle(bundle *memqlv1.GraphBundle) *memqlv1.GraphBundle {
	if bundle == nil {
		return nil
	}

	nodes := make([]*memqlv1.MemoryNode, 0, len(bundle.Nodes))
	for i := range bundle.Nodes {
		if bundle.Nodes[i] == nil {
			continue
		}
		nodeCopy := cloneMemoryNode(bundle.Nodes[i])
		if nodeCopy != nil {
			nodes = append(nodes, nodeCopy)
		}
	}

	edges := make([]*memqlv1.GraphEdge, 0, len(bundle.Edges))
	for i := range bundle.Edges {
		if bundle.Edges[i] == nil {
			continue
		}
		edgeCopy, ok := proto.Clone(bundle.Edges[i]).(*memqlv1.GraphEdge)
		if ok && edgeCopy != nil {
			edges = append(edges, edgeCopy)
		}
	}

	return &memqlv1.GraphBundle{
		Nodes:   nodes,
		Edges:   edges,
		RootIds: append(make([]string, 0, len(bundle.RootIds)), bundle.RootIds...),
	}
}

func cloneMemoryNode(node *memqlv1.MemoryNode) *memqlv1.MemoryNode {
	if node == nil {
		return nil
	}
	cloned, ok := proto.Clone(node).(*memqlv1.MemoryNode)
	if !ok || cloned == nil {
		return nil
	}
	return cloned
}

func cloneJSONSchemaDocument(doc *structpb.Struct) *structpb.Struct {
	if doc == nil {
		return nil
	}
	cloned, ok := proto.Clone(doc).(*structpb.Struct)
	if !ok {
		return nil
	}
	return cloned
}

func cloneAnyMap(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}

	copy := make(map[string]any, len(src))
	for key, value := range src {
		copy[key] = cloneInterface(value)
	}
	return copy
}

func cloneInterface(value any) any {
	switch v := value.(type) {
	case *structpb.Struct:
		return cloneJSONSchemaDocument(v)
	case map[string]any:
		return cloneAnyMap(v)
	case []any:
		result := make([]any, len(v))
		for i := range v {
			result[i] = cloneInterface(v[i])
		}
		return result
	case []*structpb.Struct:
		result := make([]*structpb.Struct, len(v))
		for i := range v {
			result[i] = cloneJSONSchemaDocument(v[i])
		}
		return result
	case []map[string]any:
		result := make([]map[string]any, len(v))
		for i := range v {
			result[i] = cloneAnyMap(v[i])
		}
		return result
	default:
		return v
	}
}
