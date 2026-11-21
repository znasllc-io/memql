package calls

import (
	"crypto/sha256"
	"encoding/hex"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestApplicationMessageValidationClosedVariantsAndBounds(t *testing.T) {
	text := strings.Repeat("a", 2000)
	q := MessageRequest{RoomID: "room_1", Identity: "seat-1", Sequence: 1, Kind: "chat", Text: &text}
	require.True(t, ValidMessage(q))
	text += "a"
	require.False(t, ValidMessage(q))
	text = strings.Repeat("🙂", 1000)
	require.True(t, ValidMessage(q))
	text += "🙂"
	require.False(t, ValidMessage(q))
	text = "hello\nthere\tfriend"
	require.True(t, ValidMessage(q))
	for _, bad := range []string{"  ", "a\x00b", "a\x7fb", string([]byte{255})} {
		text = bad
		require.False(t, ValidMessage(q))
	}
	text = "valid"
	q.Identity = "forged/name"
	require.False(t, ValidMessage(q))
	q.Identity = "seat-1"
	q.Sequence = 0
	require.False(t, ValidMessage(q))
	q.Sequence = 1
	raised := false
	q.Kind = "hand"
	q.Raised = &raised
	require.False(t, ValidMessage(q))
	q.Text = nil
	require.True(t, ValidMessage(q))
	q.Raised = nil
	require.False(t, ValidMessage(q))
	q.Kind = "reaction"
	reaction := "not-in-catalog"
	q.Reaction = &reaction
	require.False(t, ValidMessage(q))
	for id := range reactionIdentifiers {
		reaction = id
		break
	}
	require.True(t, ValidMessage(q))
	require.Len(t, reactionIdentifiers, 3944)
}

func TestApplicationReactionCatalogDigestMatchesDiscovery(t *testing.T) {
	data, e := reactionFiles.ReadFile("reaction_identifiers.json")
	require.NoError(t, e)
	digest := sha256.Sum256(data)
	require.Equal(t, ReactionCatalogDigest, hex.EncodeToString(digest[:]))
	meta := UTCPManual("https://example.invalid/_memql/calls/agent")["application_messages"].(map[string]any)
	require.Equal(t, MessageProtocol, meta["protocol"])
	require.Equal(t, ReactionCatalogDigest, meta["reactionCatalog"].(map[string]any)["sha256"])
}
