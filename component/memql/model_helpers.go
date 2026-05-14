package memql

import (
	"encoding/json"
	"strings"

	memorynodes "github.com/visionarys-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/visionarys-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func optionalString(value string) *string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	v := trimmed
	return &v
}

func optionalInt(value int) *int {
	v := value
	return &v
}

func toAPIMemoryNode(node *memorynodes.MemoryNode) (*memqlv1.MemoryNode, error) {
	if node == nil {
		return nil, nil
	}

	schema := &structpb.Struct{}
	if len(node.Schema) > 0 {
		if err := json.Unmarshal(node.Schema, &schema); err != nil {
			return nil, err
		}
	}

	payload := map[string]any{}

	if len(node.Payload) > 0 {
		if err := json.Unmarshal(node.Payload, &payload); err != nil {
			return nil, err
		}
	}

	payloadStruct, err := structpb.NewStruct(payload)

	if err != nil {
		return nil, err
	}

	return &memqlv1.MemoryNode{
		Id:        node.ID,
		Concept:   node.Concept,
		CreatedAt: timestamppb.New(node.CreatedAt),
		CreatedBy: node.CreatedBy,
		Type:      node.Type,
		Schema:    schema,
		Payload:   payloadStruct,
	}, nil
}
