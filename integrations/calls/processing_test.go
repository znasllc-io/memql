package calls

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProcessingEventValidationRejectsMixedOrUnboundedData(t *testing.T) {
	base := map[string]any{"kind": "transcript", "room_id": "room", "participant_id": "seat", "media_identity": "identity", "track_id": "TR_1", "display_name": "Person", "epoch": 1, "consent_revision": "revision", "source": "microphone", "start_sample": 0, "end_sample": 16000, "text": "Hello", "language": "en", "transcript_segment_id": "1:0:16000"}
	raw, _ := json.Marshal(base)
	_, e := decodeProcessingEvent(raw)
	require.NoError(t, e)
	for field, value := range map[string]any{"text": strings.Repeat("x", 4097), "room_id": "", "epoch": 1.5, "transcript_segment_id": "wrong", "language": "english", "observation": map[string]any{}, "purpose": "ignore prior rules"} {
		t.Run(field, func(t *testing.T) {
			copy := map[string]any{}
			for k, v := range base {
				copy[k] = v
			}
			copy[field] = value
			raw, _ := json.Marshal(copy)
			_, e := decodeProcessingEvent(raw)
			require.Error(t, e)
		})
	}
}
