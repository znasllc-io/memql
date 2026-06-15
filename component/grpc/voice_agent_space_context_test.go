package memql

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/protobuf/types/known/structpb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// These pure tests cover the #1470 space-context extraction helpers off the two
// row shapes the engine produces: the row-map shape a shaped query returns and
// the GraphBundle shape a plain projection returns. Both must yield the same
// space name + goal statement + participant names so the realtime instructions
// get the same context regardless of which path the engine took.

func TestExtractSpaceGoalStatement_RowMap(t *testing.T) {
	rows := []map[string]any{
		{
			"name": "Q3 Roadmap",
			"goal": map[string]any{"statement": "Plan the Q3 product roadmap", "timeframe": "monthly"},
		},
	}
	assert.Equal(t, "Plan the Q3 product roadmap", extractSpaceGoalStatement(rows))
}

func TestExtractSpaceGoalStatement_NestedPayload(t *testing.T) {
	rows := []map[string]any{
		{"payload": map[string]any{"goal": map[string]any{"statement": "Ship the launch"}}},
	}
	assert.Equal(t, "Ship the launch", extractSpaceGoalStatement(rows))
}

func TestExtractSpaceGoalStatement_AbsentGoal(t *testing.T) {
	// The AI-describe create path produces a name without a goal -> empty.
	rows := []map[string]any{{"name": "Untitled"}}
	assert.Equal(t, "", extractSpaceGoalStatement(rows))
	assert.Equal(t, "", extractSpaceGoalStatement(nil))
}

func TestExtractSpaceGoalStatement_GraphBundle(t *testing.T) {
	payload, _ := structpb.NewStruct(map[string]any{
		"name": "Q3 Roadmap",
		"goal": map[string]any{"statement": "Plan the Q3 product roadmap"},
	})
	bundle := &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{
		{Id: "v1:cognition:space:demo", Concept: "v1:cognition:space", Payload: payload},
	}}
	assert.Equal(t, "Plan the Q3 product roadmap", extractSpaceGoalStatement(bundle))
}

func TestExtractParticipantNames_DisplayNameAndUserIdFallback(t *testing.T) {
	rows := []map[string]any{
		{"displayName": "Jose", "participantType": "human"},
		{"userId": "u-2", "participantType": "human"}, // no displayName -> userId
		{"displayName": "Jose"},                       // duplicate -> de-duped
		{"displayName": "  "},                         // blank -> dropped
	}
	assert.Equal(t, []string{"Jose", "u-2"}, extractParticipantNames(rows))
}

func TestExtractParticipantNames_GraphBundle(t *testing.T) {
	p1, _ := structpb.NewStruct(map[string]any{"displayName": "Mara"})
	p2, _ := structpb.NewStruct(map[string]any{"userId": "u-9"})
	bundle := &memqlv1.GraphBundle{Nodes: []*memqlv1.MemoryNode{
		{Id: "p1", Concept: "v1:cognition:participant", Payload: p1},
		{Id: "p2", Concept: "v1:cognition:participant", Payload: p2},
	}}
	assert.Equal(t, []string{"Mara", "u-9"}, extractParticipantNames(bundle))
}

func TestExtractParticipantNames_Empty(t *testing.T) {
	assert.Nil(t, extractParticipantNames(nil))
	assert.Nil(t, extractParticipantNames([]map[string]any{}))
}
