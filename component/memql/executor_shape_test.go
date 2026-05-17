package memql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

func TestShapeDirectiveExecutionNestedRelations(t *testing.T) {
	query := `shape(concept==v1:conversation;id=="conv-1",{"conversation":node("payload.title","id"),"messages":children({"id":node("id"),"body":node("payload.body"),"author":createdBy(node("payload.name"))})})`
	plan := mustParse(t, query)
	require.NotNil(t, plan.ShapeTemplate)

	bundle := buildShapeTestBundle()
	value, err := applyShapeTemplate(context.Background(), bundle, plan.ShapeTemplate, nil, nil)
	require.NoError(t, err)

	result, ok := value.(map[string]any)
	require.True(t, ok)

	conversation, ok := result["conversation"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "Weekly Sync", conversation["title"])
	require.Equal(t, "v1:conversation:conv-1", conversation["id"])

	rawMessages, ok := result["messages"].([]any)
	require.True(t, ok)
	require.Len(t, rawMessages, 2)

	firstMessage, ok := rawMessages[0].(map[string]any)
	require.True(t, ok)
	require.Contains(t, firstMessage, "id")
	require.Contains(t, firstMessage, "body")
	require.Contains(t, firstMessage, "author")

	// Single-field node() projections return the value directly (not wrapped in a map)
	body, ok := firstMessage["body"].(string)
	require.True(t, ok)
	require.Equal(t, "Agenda", body)

	id, ok := firstMessage["id"].(string)
	require.True(t, ok)
	require.Equal(t, "v1:message:msg-1", id)

	firstAuthorList, ok := firstMessage["author"].([]any)
	require.True(t, ok)
	require.Len(t, firstAuthorList, 1)
	// Single-field node("payload.name") returns the value directly (not wrapped in a map)
	firstAuthorName, ok := firstAuthorList[0].(string)
	require.True(t, ok)
	require.Equal(t, "Kai", firstAuthorName)
}

func TestShapeDirectiveExecutionArrayRoot(t *testing.T) {
	query := `shape(concept==v1:conversation;id=="conv-1",[node("id","payload.title"),children(node("id")),"done"])`
	plan := mustParse(t, query)
	require.NotNil(t, plan.ShapeTemplate)

	bundle := buildShapeTestBundle()
	value, err := applyShapeTemplate(context.Background(), bundle, plan.ShapeTemplate, nil, nil)
	require.NoError(t, err)

	arrayValue, ok := value.([]any)
	require.True(t, ok)
	require.Len(t, arrayValue, 3)

	rootProjection, ok := arrayValue[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "v1:conversation:conv-1", rootProjection["id"])
	require.Equal(t, "Weekly Sync", rootProjection["title"])

	childrenValue, ok := arrayValue[1].([]any)
	require.True(t, ok)
	require.Len(t, childrenValue, 2)
	// Single-field node("id") returns the value directly, not wrapped in map
	firstChildId, ok := childrenValue[0].(string)
	require.True(t, ok)
	require.Equal(t, "v1:message:msg-1", firstChildId)

	require.Equal(t, "done", arrayValue[2])
}

func buildShapeTestBundle() *memqlv1.GraphBundle {
	nodes := []memqlv1.MemoryNode{
		{
			Id:      "v1:conversation:conv-1",
			Concept: "v1:conversation",
			Payload: &structpb.Struct{
				Fields: map[string]*structpb.Value{
					"title": {
						Kind: &structpb.Value_StringValue{
							StringValue: "Weekly Sync",
						},
					},
				},
			},
		},
		{
			Id:      "v1:message:msg-1",
			Concept: "v1:message",
			Payload: &structpb.Struct{
				Fields: map[string]*structpb.Value{
					"body": {
						Kind: &structpb.Value_StringValue{
							StringValue: "Agenda",
						},
					},
				},
			},
		},
		{
			Id:      "v1:message:msg-2",
			Concept: "v1:message",
			Payload: &structpb.Struct{
				Fields: map[string]*structpb.Value{
					"body": {
						Kind: &structpb.Value_StringValue{
							StringValue: "Notes",
						},
					},
				},
			},
		},
		{
			Id:      "v1:memql:backend:user:user-1",
			Concept: "v1:memql:backend:user",
			Payload: &structpb.Struct{
				Fields: map[string]*structpb.Value{
					"name": {
						Kind: &structpb.Value_StringValue{
							StringValue: "Kai",
						},
					},
				},
			},
		},
		{
			Id:      "v1:memql:backend:user:user-2",
			Concept: "v1:memql:backend:user",
			Payload: &structpb.Struct{
				Fields: map[string]*structpb.Value{
					"name": {
						Kind: &structpb.Value_StringValue{
							StringValue: "Riley",
						},
					},
				},
			},
		},
	}

	edges := []memqlv1.GraphEdge{
		{
			Type:   graphEdgeTypeChild,
			FromId: "v1:conversation:conv-1",
			ToId:   "v1:message:msg-1",
		},
		{
			Type:   graphEdgeTypeChild,
			FromId: "v1:conversation:conv-1",
			ToId:   "v1:message:msg-2",
		},
		{
			Type:   relationshipTypeCreatedBy,
			FromId: "v1:message:msg-1",
			ToId:   "v1:memql:backend:user:user-1",
		},
		{
			Type:   relationshipTypeCreatedBy,
			FromId: "v1:message:msg-2",
			ToId:   "v1:memql:backend:user:user-2",
		},
	}

	return &memqlv1.GraphBundle{
		Nodes:   []*memqlv1.MemoryNode{&nodes[0], &nodes[1], &nodes[2], &nodes[3], &nodes[4]},
		Edges:   []*memqlv1.GraphEdge{&edges[0], &edges[1], &edges[2], &edges[3]},
		RootIds: []string{"v1:conversation:conv-1"},
	}
}
