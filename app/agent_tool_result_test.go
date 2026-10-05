package app

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/memql"
)

func TestAgentToolResultKeepsEvidenceWithoutProtocolEscaping(t *testing.T) {
	text := `{"data":[{"note":"An actual quoted phrase: \"hello\"","source":"source-1"}]}`
	for _, failed := range []bool{false, true} {
		raw, err := json.Marshal(memql.ToolCallResult{IsError: failed, Content: []memql.ToolResultContent{{Type: "text", Text: text}}})
		require.NoError(t, err)
		got, err := agentToolResultText(string(raw))
		if failed {
			require.ErrorContains(t, err, text)
		} else {
			require.NoError(t, err)
			require.Equal(t, text, got)
		}
	}
	raw, err := json.Marshal(memql.ToolCallResult{Content: []memql.ToolResultContent{{Type: "image", Data: "image-data", MimeType: "image/png"}, {Type: "text", Text: "caption"}}})
	require.NoError(t, err)
	got, err := agentToolResultText(string(raw))
	require.NoError(t, err)
	require.JSONEq(t, string(raw), got)
	_, err = agentToolResultText("malformed")
	require.Error(t, err)
}
