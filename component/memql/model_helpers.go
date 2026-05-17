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

	// Expose the engine-stamped provenance intrinsic on the wire as a
	// first-class field (matches its first-class status at the DB
	// layer -- NOT NULL column with GIN index). Closed-enum kinds
	// (see component/provenance).
	var provPb *memqlv1.Provenance
	if len(node.Provenance) > 0 {
		var pv struct {
			Kind    string `json:"kind"`
			Name    string `json:"name"`
			Trigger string `json:"trigger,omitempty"`
			Via     string `json:"via,omitempty"`
		}
		if err := json.Unmarshal(node.Provenance, &pv); err == nil && pv.Kind != "" {
			provPb = &memqlv1.Provenance{
				Kind:    pv.Kind,
				Name:    pv.Name,
				Trigger: pv.Trigger,
				Via:     pv.Via,
			}
		}
	}

	return &memqlv1.MemoryNode{
		Id:         node.ID,
		Concept:    node.Concept,
		CreatedAt:  timestamppb.New(node.CreatedAt),
		CreatedBy:  node.CreatedBy,
		Type:       node.Type,
		Schema:     schema,
		Payload:    payloadStruct,
		Provenance: provPb,
	}, nil
}
