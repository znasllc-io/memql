package memql

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecodeWorkContinuationRejectsMalformedEncodedReceipts(t *testing.T) {
	for _, saved := range []map[string]any{
		{"messagesEncoding": "unknown", "messages": "anything"},
		{"messagesEncoding": "json-base64-v1", "messages": []any{}},
		{"messagesEncoding": "json-base64-v1", "messages": "not base64!"},
		{"messagesEncoding": "json-base64-v1", "messages": base64.StdEncoding.EncodeToString([]byte("not JSON"))},
	} {
		_, err := decodeWorkContinuation(saved)
		require.Error(t, err)
	}
}
