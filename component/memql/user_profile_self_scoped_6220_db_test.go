package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
)

// user_profile_self_scoped_6220_db_test.go -- memql#6220, against a real
// database. The sibling non-DB file pins the three mutations' SHAPE; this one
// pins what a round trip leaves on the row and what currentUser hands back,
// which is what the Profile and the dock avatar actually read.

func latestUserPayload(t *testing.T, ctx context.Context, db *bun.DB, userID string) map[string]any {
	t.Helper()
	canonical := userID
	if !strings.HasPrefix(userID, conceptIdentityUser+":") {
		canonical = conceptIdentityUser + ":" + userID
	}
	return latestPayload(t, ctx, db, conceptIdentityUser, canonical)
}

// TestProfileWrites_RoundTripOnTheCallersRow: two people, two rows, and a
// supplied id that must not retarget anything.
func TestProfileWrites_RoundTripOnTheCallersRow(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	userA := newReader(t, eng, ctx, "profileA")
	userB := newReader(t, eng, ctx, "profileB")
	ctxA, ctxB := rowAuthzCallerCtx(userA), rowAuthzCallerCtx(userB)

	require.NoError(t, tryMutation(t, ctxA, eng, "setMyAvatar", map[string]any{"avatar": webpAvatar}))
	require.NoError(t, tryMutation(t, ctxA, eng, "updateMyName", map[string]any{
		"firstName": "Ada", "lastName": "Lovelace", "displayName": "Ada Lovelace",
	}))

	a := latestUserPayload(t, ctx, db, userA)
	require.Equal(t, webpAvatar, a["avatar"])
	require.Equal(t, "Ada", a["firstName"])
	require.Equal(t, "Lovelace", a["lastName"])
	require.Equal(t, "Ada Lovelace", a["displayName"])
	require.Equal(t, "reader", a["role"], "a name or photo write must leave the role exactly as it was")

	// B names A's row; it must land on B's own.
	require.NoError(t, tryMutation(t, ctxB, eng, "setMyAvatar", map[string]any{
		"avatar": "data:image/png;base64,iVBORw0KGgo=",
		"userId": userA,
		"id":     conceptIdentityUser + ":" + userA,
	}))
	require.Equal(t, webpAvatar, latestUserPayload(t, ctx, db, userA)["avatar"],
		"user B's write reached user A's row: a supplied id must not retarget a self-only write")
	require.Equal(t, "data:image/png;base64,iVBORw0KGgo=", latestUserPayload(t, ctx, db, userB)["avatar"])

	// Clearing leaves no photo and touches nothing else.
	require.NoError(t, tryMutation(t, ctxA, eng, "clearMyAvatar", map[string]any{}))
	cleared := latestUserPayload(t, ctx, db, userA)
	require.Equal(t, "", cleared["avatar"])
	require.Equal(t, "Ada Lovelace", cleared["displayName"], "clearing the photo must keep the name")
}

// TestProfileWrites_RefusalsLeaveTheRowUntouched asserts the row as well as the
// error: a refusal that half-applied would be worse than one that never fired.
func TestProfileWrites_RefusalsLeaveTheRowUntouched(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)
	user := newReader(t, eng, ctx, "profileRefusals")
	caller := rowAuthzCallerCtx(user)
	require.NoError(t, tryMutation(t, caller, eng, "setMyAvatar", map[string]any{"avatar": webpAvatar}))
	before := latestUserPayload(t, ctx, db, user)

	for label, call := range map[string]struct {
		name string
		args map[string]any
	}{
		"a remote photo URL":   {"setMyAvatar", map[string]any{"avatar": "https://example.com/me.png"}},
		"an SVG photo":         {"setMyAvatar", map[string]any{"avatar": "data:image/svg+xml;base64,PHN2Zz48L3N2Zz4="}},
		"an oversized photo":   {"setMyAvatar", map[string]any{"avatar": "data:image/webp;base64," + strings.Repeat("A", 40001)}},
		"a blank display name": {"updateMyName", map[string]any{"firstName": "Ada", "displayName": "   "}},
	} {
		t.Run(label, func(t *testing.T) {
			require.Error(t, tryMutation(t, caller, eng, call.name, call.args), "%s must be refused", label)
			require.Equal(t, before, latestUserPayload(t, ctx, db, user), "a refused write must leave the row untouched")
		})
	}
}

// TestCurrentUserReturnsTheAvatar is the read the Profile and the dock make:
// the photo comes back on the caller's own row.
func TestCurrentUserReturnsTheAvatar(t *testing.T) {
	eng, _, ctx := sharedReadMergeEngine(t)
	user := newReader(t, eng, ctx, "profileRead")
	caller := rowAuthzCallerCtx(user)
	require.NoError(t, tryMutation(t, caller, eng, "setMyAvatar", map[string]any{"avatar": webpAvatar}))

	res, err := eng.Execute(ContextWithFreshRead(caller), "query currentUser()")
	require.NoError(t, err)
	require.Contains(t, resultBlob(t, res), webpAvatar, "currentUser must project the caller's photo (userFull carries avatar)")
}
