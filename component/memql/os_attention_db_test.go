package memql

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestAttentionReceiptsArePerUserAndPerRevision(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("attention")
	alice := rowAuthzCallerCtx("attention-alice-" + suffix)
	bob := rowAuthzCallerCtx("attention-bob-" + suffix)
	args := map[string]any{"changeId": "deployables:version", "revision": "sha-one"}
	first := runMutation(t, alice, eng, "acknowledgeAttention", args)
	require.Equal(t, first, runMutation(t, alice, eng, "acknowledgeAttention", args))
	other := runMutation(t, bob, eng, "acknowledgeAttention", args)
	require.NotEqual(t, first, other)
	second := runMutation(t, alice, eng, "acknowledgeAttention", map[string]any{"changeId": "deployables:version", "revision": "sha-two"})
	require.NotEqual(t, first, second)
	p := latestPayload(t, alice, db, "v1:os:attentionReceipt", first)
	require.Equal(t, canonicalUserId("attention-alice-"+suffix), p["ownerUserId"])
	res, err := eng.Execute(alice, "query myAttentionReceipts()")
	require.NoError(t, err)
	blob := resultBlob(t, res)
	require.Contains(t, blob, first)
	require.Contains(t, blob, second)
	require.NotContains(t, blob, other)
}
