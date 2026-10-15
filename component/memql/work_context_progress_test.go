package memql

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWorkCheckpointPreservesNamedGapAndEvidenceWithinBudget(t *testing.T) {
	c := workCheckpoint{NextArtifact: "Behavioral regression draft", UnresolvedQuestion: "Which fixture establishes the old failure?", EvidenceReferences: []string{"tool-call:read-fixture"}, Unfinished: []string{"Draft is absent"}, Facts: []string{strings.Repeat("background", 1000)}}
	raw := boundedWorkCheckpoint(c, 700)
	require.LessOrEqual(t, len(raw), 700)
	var restored workCheckpoint
	require.NoError(t, json.Unmarshal([]byte(raw), &restored))
	require.Equal(t, c.NextArtifact, restored.NextArtifact)
	require.Equal(t, c.UnresolvedQuestion, restored.UnresolvedQuestion)
	require.Equal(t, c.EvidenceReferences, restored.EvidenceReferences)
	require.Contains(t, restored.Unfinished, "Draft is absent")
}
