package memql

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/core/common"
)

func TestDelegatedFeedbackReferenceKeepsCallsAndSmallResultsExact(t *testing.T) {
	pair := []common.ChatMessage{
		{Role: "assistant", ToolCalls: []common.ToolCall{{ID: "effect", Name: "write", Arguments: strings.Repeat("large input", 10000)}}},
		{Role: "tool", ToolCallId: "effect", Content: strings.Repeat("x", 64*1024)},
	}
	out, err := delegatedFeedbackReference("run", "receipt", pair)
	require.NoError(t, err)
	require.Equal(t, pair, out, "the threshold must not archive small results or conceal large arguments")
	pair[1].Content += "x"
	before := mustMarshalMessages(t, pair)
	out, err = delegatedFeedbackReference("run", "receipt", pair)
	require.NoError(t, err)
	require.Equal(t, pair[0], out[0])
	require.Equal(t, before, mustMarshalMessages(t, pair), "durable source projection must not mutate its input")
	require.Contains(t, out[1].Content, `"checkpoint":"receipt"`)
	require.Contains(t, out[1].Content, `"sourceRunId":"run"`)
	_, err = delegatedFeedbackReference("run", "", pair)
	require.ErrorIs(t, err, ErrWorkCheckpointRequired)
}
