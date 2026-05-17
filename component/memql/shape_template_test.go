package memql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

func TestApplyShapeTemplateNodeSelection(t *testing.T) {
	payloadStruct, err := structpb.NewStruct(map[string]any{
		"title": "Weekly Sync",
	})
	require.NoError(t, err)
	bundle := &memqlv1.GraphBundle{
		Nodes: []*memqlv1.MemoryNode{
			{
				Id:      "v1:conversation:conv-1",
				Concept: "v1:conversation",
				Payload: payloadStruct,
			},
		},
		Edges:   []*memqlv1.GraphEdge{},
		RootIds: []string{"v1:conversation:conv-1"},
	}

	template := &shapeNodeFunc{
		Fields: []FieldReference{
			mustShapeFieldRef(t, "payload.title"),
			mustShapeFieldRef(t, "concept"),
			mustShapeFieldRef(t, "id"),
		},
	}

	value, err := applyShapeTemplate(context.Background(), bundle, template, nil, nil)
	require.NoError(t, err)

	result, ok := value.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "Weekly Sync", result["title"])
	require.Equal(t, "v1:conversation", result["concept"])
	require.Equal(t, "v1:conversation:conv-1", result["id"])
}

func TestApplyShapeTemplateChildren(t *testing.T) {
	payloadStruct, err := structpb.NewStruct(map[string]any{
		"body": "Hello there",
	})
	require.NoError(t, err)
	bundle := &memqlv1.GraphBundle{
		Nodes: []*memqlv1.MemoryNode{
			{
				Id:      "root",
				Concept: "v1:conversation",
			},
			{
				Id:      "child-1",
				Concept: "v1:message",
				Payload: payloadStruct,
			},
		},
		Edges: []*memqlv1.GraphEdge{
			{
				Type:   graphEdgeTypeChild,
				FromId: "root",
				ToId:   "child-1",
			},
		},
		RootIds: []string{"root"},
	}

	template := &shapeObject{
		Fields: map[string]shapeTemplate{
			"messages": &shapeRelationFunc{
				Relation: "children",
				Template: &shapeNodeFunc{
					Fields: []FieldReference{
						mustShapeFieldRef(t, "payload.body"),
					},
				},
			},
		},
	}

	value, err := applyShapeTemplate(context.Background(), bundle, template, nil, nil)
	require.NoError(t, err)

	result, ok := value.(map[string]any)
	require.True(t, ok)

	rawMessages, ok := result["messages"]
	require.True(t, ok)

	messages, ok := rawMessages.([]any)
	require.True(t, ok)
	require.Len(t, messages, 1)

	// Single-field node() projections return the value directly (not wrapped in a map)
	message, ok := messages[0].(string)
	require.True(t, ok)
	require.Equal(t, "Hello there", message)
}

func mustShapeFieldRef(t *testing.T, raw string) FieldReference {
	t.Helper()
	ref, err := parseFieldReferenceLiteral(raw)
	require.NoError(t, err)
	return ref
}

func TestRenderMatchSpecCondition(t *testing.T) {
	payloadStruct, err := structpb.NewStruct(map[string]any{
		"middleName": "Garcia",
		"region":     "LATAM",
	})
	require.NoError(t, err)

	bundle := &memqlv1.GraphBundle{
		Nodes: []*memqlv1.MemoryNode{
			{
				Id:      "v1:customer:cust-1",
				Concept: "v1:customer",
				Payload: payloadStruct,
			},
		},
		Edges:   []*memqlv1.GraphEdge{},
		RootIds: []string{"v1:customer:cust-1"},
	}

	// Create a spec that checks for Hispanic middle names
	hispanicSpec := &Spec{
		Name:       "hispanicMiddleName",
		ExprSource: `payload.middleName in ("Garcia","Rodriguez","Martinez")`,
		Expr: &ComparisonExpression{
			Field:    mustShapeFieldRef(t, "payload.middleName"),
			Operator: OpIn,
			Value:    []any{"Garcia", "Rodriguez", "Martinez"},
		},
	}

	specs := map[string]*Spec{
		"hispanicMiddleName": hispanicSpec,
	}

	template := &shapeObject{
		Fields: map[string]shapeTemplate{
			"id": &shapeNodeFunc{Fields: []FieldReference{mustShapeFieldRef(t, "id")}},
			"origin": &shapeMatchExpr{
				Cases: []*shapeCaseBranch{
					{
						Condition: &shapeSpecCondition{SpecName: "hispanicMiddleName"},
						Value:     &shapeLiteral{Value: "hisp"},
					},
				},
				Default: &shapeLiteral{Value: "unknown"},
			},
		},
	}

	value, err := applyShapeTemplate(context.Background(), bundle, template, nil, specs)
	require.NoError(t, err)

	result, ok := value.(map[string]any)
	require.True(t, ok)

	// Single-field node("id") returns the value directly (not wrapped in a map)
	idValue, ok := result["id"].(string)
	require.True(t, ok)
	require.Equal(t, "v1:customer:cust-1", idValue)

	require.Equal(t, "hisp", result["origin"])
}

func TestRenderMatchComparisonCondition(t *testing.T) {
	payloadStruct, err := structpb.NewStruct(map[string]any{
		"status": "active",
	})
	require.NoError(t, err)

	bundle := &memqlv1.GraphBundle{
		Nodes: []*memqlv1.MemoryNode{
			{
				Id:      "v1:customer:cust-1",
				Concept: "v1:customer",
				Payload: payloadStruct,
			},
		},
		Edges:   []*memqlv1.GraphEdge{},
		RootIds: []string{"v1:customer:cust-1"},
	}

	template := &shapeObject{
		Fields: map[string]shapeTemplate{
			"displayStatus": &shapeMatchExpr{
				Cases: []*shapeCaseBranch{
					{
						Condition: &shapeComparisonCondition{
							Left:     &shapeNodeFunc{Fields: []FieldReference{mustShapeFieldRef(t, "payload.status")}},
							Operator: OpEq,
							Right:    "active",
						},
						Value: &shapeLiteral{Value: "Active Account"},
					},
					{
						Condition: &shapeComparisonCondition{
							Left:     &shapeNodeFunc{Fields: []FieldReference{mustShapeFieldRef(t, "payload.status")}},
							Operator: OpEq,
							Right:    "pending",
						},
						Value: &shapeLiteral{Value: "Pending Activation"},
					},
				},
				Default: &shapeLiteral{Value: "Unknown Status"},
			},
		},
	}

	value, err := applyShapeTemplate(context.Background(), bundle, template, nil, nil)
	require.NoError(t, err)

	result, ok := value.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "Active Account", result["displayStatus"])
}

func TestRenderMatchDefaultFallback(t *testing.T) {
	payloadStruct, err := structpb.NewStruct(map[string]any{
		"middleName": "Smith",
	})
	require.NoError(t, err)

	bundle := &memqlv1.GraphBundle{
		Nodes: []*memqlv1.MemoryNode{
			{
				Id:      "v1:customer:cust-1",
				Concept: "v1:customer",
				Payload: payloadStruct,
			},
		},
		Edges:   []*memqlv1.GraphEdge{},
		RootIds: []string{"v1:customer:cust-1"},
	}

	// Spec that won't match "Smith"
	hispanicSpec := &Spec{
		Name: "hispanicMiddleName",
		Expr: &ComparisonExpression{
			Field:    mustShapeFieldRef(t, "payload.middleName"),
			Operator: OpIn,
			Value:    []any{"Garcia", "Rodriguez"},
		},
	}

	specs := map[string]*Spec{
		"hispanicMiddleName": hispanicSpec,
	}

	template := &shapeObject{
		Fields: map[string]shapeTemplate{
			"origin": &shapeMatchExpr{
				Cases: []*shapeCaseBranch{
					{
						Condition: &shapeSpecCondition{SpecName: "hispanicMiddleName"},
						Value:     &shapeLiteral{Value: "hisp"},
					},
				},
				Default: &shapeLiteral{Value: "unknown"},
			},
		},
	}

	value, err := applyShapeTemplate(context.Background(), bundle, template, nil, specs)
	require.NoError(t, err)

	result, ok := value.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "unknown", result["origin"])
}

func TestRenderMatchShortCircuit(t *testing.T) {
	payloadStruct, err := structpb.NewStruct(map[string]any{
		"score": float64(85),
	})
	require.NoError(t, err)

	bundle := &memqlv1.GraphBundle{
		Nodes: []*memqlv1.MemoryNode{
			{
				Id:      "v1:student:student-1",
				Concept: "v1:student",
				Payload: payloadStruct,
			},
		},
		Edges:   []*memqlv1.GraphEdge{},
		RootIds: []string{"v1:student:student-1"},
	}

	// Test short-circuit: score 85 should match second case (>=80 for "B")
	// but not first case (>=90 for "A")
	template := &shapeObject{
		Fields: map[string]shapeTemplate{
			"grade": &shapeMatchExpr{
				Cases: []*shapeCaseBranch{
					{
						Condition: &shapeComparisonCondition{
							Left:     &shapeNodeFunc{Fields: []FieldReference{mustShapeFieldRef(t, "payload.score")}},
							Operator: OpGe,
							Right:    float64(90),
						},
						Value: &shapeLiteral{Value: "A"},
					},
					{
						Condition: &shapeComparisonCondition{
							Left:     &shapeNodeFunc{Fields: []FieldReference{mustShapeFieldRef(t, "payload.score")}},
							Operator: OpGe,
							Right:    float64(80),
						},
						Value: &shapeLiteral{Value: "B"},
					},
					{
						Condition: &shapeComparisonCondition{
							Left:     &shapeNodeFunc{Fields: []FieldReference{mustShapeFieldRef(t, "payload.score")}},
							Operator: OpGe,
							Right:    float64(70),
						},
						Value: &shapeLiteral{Value: "C"},
					},
				},
				Default: &shapeLiteral{Value: "F"},
			},
		},
	}

	value, err := applyShapeTemplate(context.Background(), bundle, template, nil, nil)
	require.NoError(t, err)

	result, ok := value.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "B", result["grade"])
}

func TestRenderMatchWithInOperator(t *testing.T) {
	payloadStruct, err := structpb.NewStruct(map[string]any{
		"country": "CA",
	})
	require.NoError(t, err)

	bundle := &memqlv1.GraphBundle{
		Nodes: []*memqlv1.MemoryNode{
			{
				Id:      "v1:customer:cust-1",
				Concept: "v1:customer",
				Payload: payloadStruct,
			},
		},
		Edges:   []*memqlv1.GraphEdge{},
		RootIds: []string{"v1:customer:cust-1"},
	}

	template := &shapeObject{
		Fields: map[string]shapeTemplate{
			"region": &shapeMatchExpr{
				Cases: []*shapeCaseBranch{
					{
						Condition: &shapeComparisonCondition{
							Left:     &shapeNodeFunc{Fields: []FieldReference{mustShapeFieldRef(t, "payload.country")}},
							Operator: OpIn,
							Right:    []any{"US", "CA", "MX"},
						},
						Value: &shapeLiteral{Value: "North America"},
					},
					{
						Condition: &shapeComparisonCondition{
							Left:     &shapeNodeFunc{Fields: []FieldReference{mustShapeFieldRef(t, "payload.country")}},
							Operator: OpIn,
							Right:    []any{"UK", "FR", "DE"},
						},
						Value: &shapeLiteral{Value: "Europe"},
					},
				},
				Default: &shapeLiteral{Value: "Other"},
			},
		},
	}

	value, err := applyShapeTemplate(context.Background(), bundle, template, nil, nil)
	require.NoError(t, err)

	result, ok := value.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "North America", result["region"])
}

func TestRenderMatchWithMissingOperator(t *testing.T) {
	payloadStruct, err := structpb.NewStruct(map[string]any{
		"name": "Test Customer",
		// middleName is missing
	})
	require.NoError(t, err)

	bundle := &memqlv1.GraphBundle{
		Nodes: []*memqlv1.MemoryNode{
			{
				Id:      "v1:customer:cust-1",
				Concept: "v1:customer",
				Payload: payloadStruct,
			},
		},
		Edges:   []*memqlv1.GraphEdge{},
		RootIds: []string{"v1:customer:cust-1"},
	}

	template := &shapeObject{
		Fields: map[string]shapeTemplate{
			"hasMiddleName": &shapeMatchExpr{
				Cases: []*shapeCaseBranch{
					{
						Condition: &shapeComparisonCondition{
							Left:     &shapeNodeFunc{Fields: []FieldReference{mustShapeFieldRef(t, "payload.middleName")}},
							Operator: OpMissing,
							Right:    nil,
						},
						Value: &shapeLiteral{Value: false},
					},
				},
				Default: &shapeLiteral{Value: true},
			},
		},
	}

	value, err := applyShapeTemplate(context.Background(), bundle, template, nil, nil)
	require.NoError(t, err)

	result, ok := value.(map[string]any)
	require.True(t, ok)
	require.Equal(t, false, result["hasMiddleName"])
}

func TestRenderJSONWithNode(t *testing.T) {
	payloadStruct, err := structpb.NewStruct(map[string]any{
		"name":  "John Doe",
		"email": "john@example.com",
		"age":   30,
	})
	require.NoError(t, err)

	bundle := &memqlv1.GraphBundle{
		Nodes: []*memqlv1.MemoryNode{
			{
				Id:      "v1:user:user-1",
				Concept: "v1:user",
				Payload: payloadStruct,
			},
		},
		Edges:   []*memqlv1.GraphEdge{},
		RootIds: []string{"v1:user:user-1"},
	}

	template := &shapeObject{
		Fields: map[string]shapeTemplate{
			"id": &shapeNodeFunc{Fields: []FieldReference{mustShapeFieldRef(t, "id")}},
			"payloadJSON": &shapeJSONFunc{
				Inner: &shapeNodeFunc{Fields: []FieldReference{mustShapeFieldRef(t, "payload")}},
			},
		},
	}

	value, err := applyShapeTemplate(context.Background(), bundle, template, nil, nil)
	require.NoError(t, err)

	result, ok := value.(map[string]any)
	require.True(t, ok)

	payloadJSON, ok := result["payloadJSON"].(string)
	require.True(t, ok)
	require.Contains(t, payloadJSON, `"name":"John Doe"`)
	require.Contains(t, payloadJSON, `"email":"john@example.com"`)
	require.Contains(t, payloadJSON, `"age":30`)
}

func TestRenderJSONWithObject(t *testing.T) {
	payloadStruct, err := structpb.NewStruct(map[string]any{
		"displayName": "Jane Smith",
	})
	require.NoError(t, err)

	bundle := &memqlv1.GraphBundle{
		Nodes: []*memqlv1.MemoryNode{
			{
				Id:      "v1:user:user-2",
				Concept: "v1:user",
				Payload: payloadStruct,
			},
		},
		Edges:   []*memqlv1.GraphEdge{},
		RootIds: []string{"v1:user:user-2"},
	}

	template := &shapeObject{
		Fields: map[string]shapeTemplate{
			"profileJSON": &shapeJSONFunc{
				Inner: &shapeObject{
					Fields: map[string]shapeTemplate{
						"userId": &shapeNodeFunc{Fields: []FieldReference{mustShapeFieldRef(t, "id")}},
						"name":   &shapeNodeFunc{Fields: []FieldReference{mustShapeFieldRef(t, "payload.displayName")}},
					},
				},
			},
		},
	}

	value, err := applyShapeTemplate(context.Background(), bundle, template, nil, nil)
	require.NoError(t, err)

	result, ok := value.(map[string]any)
	require.True(t, ok)

	profileJSON, ok := result["profileJSON"].(string)
	require.True(t, ok)
	require.Contains(t, profileJSON, `"userId":`)
	require.Contains(t, profileJSON, `"v1:user:user-2"`)
	require.Contains(t, profileJSON, `"name":`)
}

func TestRenderJSONWithNilInner(t *testing.T) {
	bundle := &memqlv1.GraphBundle{
		Nodes: []*memqlv1.MemoryNode{
			{
				Id:      "v1:test:test-1",
				Concept: "v1:test",
			},
		},
		Edges:   []*memqlv1.GraphEdge{},
		RootIds: []string{"v1:test:test-1"},
	}

	template := &shapeObject{
		Fields: map[string]shapeTemplate{
			"data": &shapeJSONFunc{Inner: nil},
		},
	}

	value, err := applyShapeTemplate(context.Background(), bundle, template, nil, nil)
	require.NoError(t, err)

	result, ok := value.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "null", result["data"])
}

func TestRenderJSONWithLiteral(t *testing.T) {
	bundle := &memqlv1.GraphBundle{
		Nodes: []*memqlv1.MemoryNode{
			{
				Id:      "v1:test:test-1",
				Concept: "v1:test",
			},
		},
		Edges:   []*memqlv1.GraphEdge{},
		RootIds: []string{"v1:test:test-1"},
	}

	template := &shapeObject{
		Fields: map[string]shapeTemplate{
			"stringJSON": &shapeJSONFunc{Inner: &shapeLiteral{Value: "hello world"}},
			"numberJSON": &shapeJSONFunc{Inner: &shapeLiteral{Value: 42}},
			"boolJSON":   &shapeJSONFunc{Inner: &shapeLiteral{Value: true}},
		},
	}

	value, err := applyShapeTemplate(context.Background(), bundle, template, nil, nil)
	require.NoError(t, err)

	result, ok := value.(map[string]any)
	require.True(t, ok)
	require.Equal(t, `"hello world"`, result["stringJSON"])
	require.Equal(t, `42`, result["numberJSON"])
	require.Equal(t, `true`, result["boolJSON"])
}
