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

	// Expose row provenance via the existing Metadata struct so shape
	// templates can project `row.provenance` and the cockpit can render
	// it without needing a proto regen for a dedicated field. Stored
	// under metadata["provenance"] as {kind, name, trigger, via}.
	var metadataStruct *structpb.Struct
	if len(node.Provenance) > 0 {
		provObj := map[string]any{}
		if err := json.Unmarshal(node.Provenance, &provObj); err == nil && len(provObj) > 0 {
			metaMap := map[string]any{"provenance": provObj}
			if s, err := structpb.NewStruct(metaMap); err == nil {
				metadataStruct = s
			}
		}
	}

	return &memqlv1.MemoryNode{
		Id:        node.ID,
		Concept:   node.Concept,
		CreatedAt: timestamppb.New(node.CreatedAt),
		CreatedBy: node.CreatedBy,
		Type:      node.Type,
		Schema:    schema,
		Payload:   payloadStruct,
		Metadata:  metadataStruct,
	}, nil
}
