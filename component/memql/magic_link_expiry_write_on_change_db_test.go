package memql

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMagicLinkExpirySelectsOnlyUnconsumedRows(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	suffix := uniqueSuffix("expiry6059")
	now := time.Now().UTC()
	ids := map[string]string{}
	for _, state := range []string{"expired", "consumed", "future"} {
		expiry := now.Add(-time.Hour)
		if state == "future" {
			expiry = now.Add(time.Hour)
		}
		ids[state] = runMutation(t, ctx, eng, "createMagicLinkRequest", map[string]any{
			"requestId": suffix + state, "email": "expiry@example.test", "tokenHash": suffix + state,
			"expiresAt": expiry.Format(time.RFC3339),
		})
	}
	runMutation(t, ctx, eng, "consumeMagicLinkRequest", map[string]any{"requestId": ids["consumed"], "consumedFromIP": "user"})
	consumedBefore := latestPayload(t, ctx, db, "v1:identity:magicLinkRequest", ids["consumed"])
	query := fmt.Sprintf("query expiredMagicLinkRequests(asOf: %q)", now.Format(time.RFC3339))
	read, err := eng.Execute(ctx, query)
	require.NoError(t, err)
	selected := map[string]bool{}
	for _, row := range MaterializeRows(read) {
		selected[BareShortId(fmt.Sprint(row["id"]))] = true
	}
	require.True(t, selected[BareShortId(ids["expired"])])
	require.False(t, selected[BareShortId(ids["consumed"])], "hourly sweep must not rewrite spent links")
	require.False(t, selected[BareShortId(ids["future"])])
	runMutation(t, ctx, eng, "consumeMagicLinkRequest", map[string]any{"requestId": ids["expired"], "consumedFromIP": "system:expiry"})
	again, err := eng.Execute(ctx, query)
	require.NoError(t, err)
	for _, row := range MaterializeRows(again) {
		require.NotEqual(t, BareShortId(ids["expired"]), BareShortId(fmt.Sprint(row["id"])), "next tick must not consume the same link again")
	}
	require.Equal(t, consumedBefore, latestPayload(t, ctx, db, "v1:identity:magicLinkRequest", ids["consumed"]))
}
