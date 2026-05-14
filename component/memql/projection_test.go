package memql

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	memqlv1 "github.com/visionarys-io/memql/component/grpc/gen"
)

func TestApplyPlanProjectionGlobalFields(t *testing.T) {
	ref := mustFieldRef(t, "payload.name")
	plan := &QueryPlan{
		Fields:        []FieldReference{ref},
		ConceptFields: make(map[string][]FieldReference),
	}
	configurePlanForSelect(plan)
	node := testMemoryNode(t, "v1:assistant", map[string]any{
		"name":  "alpha",
		"type":  "assistant",
		"extra": true,
	})

	schema, err := structpb.NewStruct(map[string]any{"title": "schema"})
	require.NoError(t, err)
	node.Schema = schema
	bundle := &memqlv1.GraphBundle{
		Nodes: []*memqlv1.MemoryNode{node},
	}

	applyPlanProjection(bundle, plan)

	require.Equal(t, map[string]any{"name": "alpha"}, payloadAsMap(t, bundle.Nodes[0].Payload))
	require.NotNil(t, bundle.Nodes[0].Schema)
	require.Len(t, bundle.Nodes[0].Schema.Fields, 0)
	require.Empty(t, bundle.Nodes[0].Concept)
}

func TestApplyPlanProjectionNestedFields(t *testing.T) {
	ref := mustFieldRef(t, "payload.profile.displayName")
	plan := &QueryPlan{
		Fields:        []FieldReference{ref},
		ConceptFields: make(map[string][]FieldReference),
	}
	configurePlanForSelect(plan)
	node := testMemoryNode(t, "v1:assistant", map[string]any{
		"profile": map[string]any{
			"displayName": "Helper",
			"avatarUrl":   "http://example",
		},
	})
	bundle := &memqlv1.GraphBundle{
		Nodes: []*memqlv1.MemoryNode{node},
	}

	applyPlanProjection(bundle, plan)

	require.Equal(t, map[string]any{
		"profile": map[string]any{
			"displayName": "Helper",
		},
	}, payloadAsMap(t, bundle.Nodes[0].Payload))
}

func TestApplyPlanProjectionWildcard(t *testing.T) {
	ref := mustFieldRef(t, "payload.profile.*")
	plan := &QueryPlan{
		Fields:        []FieldReference{ref},
		ConceptFields: make(map[string][]FieldReference),
	}
	configurePlanForSelect(plan)
	node := testMemoryNode(t, "v1:assistant", map[string]any{
		"profile": map[string]any{
			"displayName": "Helper",
			"avatarUrl":   "http://example",
		},
		"other": "keep?",
	})
	bundle := &memqlv1.GraphBundle{
		Nodes: []*memqlv1.MemoryNode{node},
	}

	applyPlanProjection(bundle, plan)

	require.Equal(t, map[string]any{
		"profile": map[string]any{
			"displayName": "Helper",
			"avatarUrl":   "http://example",
		},
	}, payloadAsMap(t, bundle.Nodes[0].Payload))
}

func TestApplyPlanProjectionConceptIntersection(t *testing.T) {
	global := mustFieldRef(t, "payload.name")
	conceptSpecific := mustFieldRef(t, "payload.profile.displayName")
	plan := &QueryPlan{
		Fields: []FieldReference{global},
		ConceptFields: map[string][]FieldReference{
			"v1:assistant": {conceptSpecific},
		},
	}
	configurePlanForSelect(plan)

	assistantNode := testMemoryNode(t, "v1:assistant", map[string]any{
		"profile": map[string]any{
			"displayName": "Helper",
		},
		"name": "assistant-name",
	})
	messageNode := testMemoryNode(t, "v1:message", map[string]any{
		"name": "message-name",
		"text": "hello",
	})
	bundle := &memqlv1.GraphBundle{
		Nodes: []*memqlv1.MemoryNode{
			assistantNode,
			messageNode,
		},
	}

	applyPlanProjection(bundle, plan)

	require.Equal(t, map[string]any{}, payloadAsMap(t, bundle.Nodes[0].Payload))
	require.Equal(t, map[string]any{"name": "message-name"}, payloadAsMap(t, bundle.Nodes[1].Payload))
}

func TestApplyPlanProjectionConceptOnly(t *testing.T) {
	conceptSpecific := mustFieldRef(t, "payload.profile.displayName")
	plan := &QueryPlan{
		Fields: nil,
		ConceptFields: map[string][]FieldReference{
			"v1:assistant": {conceptSpecific},
		},
	}
	configurePlanWithoutSelect(plan)

	assistantNode := testMemoryNode(t, "v1:assistant", map[string]any{
		"profile": map[string]any{
			"displayName": "Helper",
			"avatarUrl":   "http://example",
		},
	})
	bundle := &memqlv1.GraphBundle{
		Nodes: []*memqlv1.MemoryNode{assistantNode},
	}

	applyPlanProjection(bundle, plan)

	require.Equal(t, map[string]any{
		"profile": map[string]any{
			"displayName": "Helper",
		},
	}, payloadAsMap(t, bundle.Nodes[0].Payload))
}

func mustFieldRef(t *testing.T, raw string) FieldReference {
	t.Helper()
	ref, err := parseFieldReferenceLiteral(raw)
	require.NoError(t, err)
	return ref
}

func testMemoryNode(t *testing.T, concept string, payload map[string]any) *memqlv1.MemoryNode {
	payloadStruct, err := structpb.NewStruct(payload)
	require.NoError(t, err)
	schemaStruct, err := structpb.NewStruct(map[string]any{"$id": concept})
	require.NoError(t, err)
	return &memqlv1.MemoryNode{
		Id:        concept + ":node",
		Concept:   concept,
		Type:      "object",
		CreatedAt: timestamppb.New(time.Now().UTC()),
		CreatedBy: "tester",
		Payload:   payloadStruct,
		Schema:    schemaStruct,
	}
}

func payloadAsMap(t *testing.T, payload *structpb.Struct) map[string]any {
	t.Helper()
	if payload == nil {
		return map[string]any{}
	}
	return payload.AsMap()
}

func configurePlanForSelect(plan *QueryPlan) {
	plan.Metadata = metadataSelection{
		Fields: map[string]struct{}{
			"id": {},
		},
	}
	plan.PayloadSelect = true
}

func configurePlanWithoutSelect(plan *QueryPlan) {
	plan.Metadata = metadataSelection{
		IncludeAll: true,
		Fields:     make(map[string]struct{}),
	}
	plan.PayloadSelect = false
}
