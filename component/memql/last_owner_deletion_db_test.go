package memql

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
)

// Owner deletion runs against an isolated graph: each account now has one
// owner. A former owner can be deleted after an atomic ownership transfer;
// historical owner versions must never count as another active owner.

// tryMutation runs a mutation and RETURNS its error instead of failing on it.
//
// Not runMutationRaw: that helper builds `name({...})`, the object-literal call
// form the parser now rejects outright, so every call through it fails on a
// parse error before the guard under test is even reached -- which reads as the
// guard refusing something it never saw.
func tryMutation(t *testing.T, ctx context.Context, eng *MemQLEngine, name string, args map[string]any) error {
	t.Helper()
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(args))
	for _, k := range keys {
		vb, err := json.Marshal(args[k])
		require.NoError(t, err)
		parts = append(parts, k+": "+string(vb))
	}
	writeCtx := ctx
	if _, ok := auth.AccessFromContext(writeCtx); !ok {
		writeCtx = auth.ContextWithUserActor(writeCtx, "system:lastowner-test")
	}
	_, err := eng.Execute(writeCtx, "mutation "+name+"("+strings.Join(parts, ", ")+")")
	return err
}

// newOwner creates an active owner and returns its user id.
func newOwner(t *testing.T, eng *MemQLEngine, ctx context.Context, suffix string) string {
	t.Helper()
	userId := "user-" + uniqueSuffix(suffix)
	runMutation(t, ctx, eng, "createUserOnFirstLogin", map[string]any{
		"userId":       userId,
		"displayName":  "Owner " + suffix,
		"primaryEmail": userId + "@example.com",
		"role":         "owner",
		"internal":     true,
	})
	return userId
}

func newReader(t *testing.T, eng *MemQLEngine, ctx context.Context, suffix string) string {
	t.Helper()
	userId := "user-" + uniqueSuffix(suffix)
	runMutation(t, ctx, eng, "createUserOnFirstLogin", map[string]any{
		"userId":       userId,
		"displayName":  "Reader " + suffix,
		"primaryEmail": userId + "@example.com",
		"role":         "reader",
		"internal":     true,
	})
	return userId
}

func TestDeletingTheLastOwnerIsRefused(t *testing.T) {
	eng := hierarchyTestEngine(t)
	ctx := auth.ContextWithInternalOrigin(systemActorContext(context.Background()))
	owner := newOwner(t, eng, ctx, "lastowner")
	err := tryMutation(t, ctx, eng, "deleteUserHard", map[string]any{"userId": owner})
	require.ErrorContains(t, err, "last owner")
}

func TestDeletingAFormerOwnerAfterTransferIsAllowed(t *testing.T) {
	eng := hierarchyTestEngine(t)
	ctx := auth.ContextWithInternalOrigin(systemActorContext(context.Background()))
	first := newOwner(t, eng, ctx, "former-owner")
	second := newReader(t, eng, ctx, "successor")
	seedAccountOwnedBy(t, eng, first, auth.RoleOwner, "self", "Primary")
	seedGroup(t, eng, "acct-self", "Primary", "account", "self")
	seedMembership(t, eng, "acct-self", second, "active")
	state, err := hierarchyCall(t, eng, first, auth.RoleOwner, "self", nil)
	require.NoError(t, err)
	_, err = hierarchyCall(t, eng, first, auth.RoleOwner, "self", map[string]any{"operation": "transfer", "revision": state["revision"], "userId": second})
	require.NoError(t, err)
	require.NoError(t, tryMutation(t, ctx, eng, "deleteUserHard", map[string]any{"userId": first}))
	require.Error(t, tryMutation(t, ctx, eng, "deleteUserHard", map[string]any{"userId": second}))
}

func TestDeletingANonOwnerIsUntouched(t *testing.T) {
	eng := hierarchyTestEngine(t)
	ctx := auth.ContextWithInternalOrigin(systemActorContext(context.Background()))
	reader := newReader(t, eng, ctx, "nonowner")
	require.NoError(t, tryMutation(t, ctx, eng, "deleteUserHard", map[string]any{"userId": reader}))
}

func TestAnAlreadyDeactivatedOwnerDoesNotCount(t *testing.T) {
	eng := hierarchyTestEngine(t)
	ctx := auth.ContextWithInternalOrigin(systemActorContext(context.Background()))
	// Pre-hierarchy history can contain a deactivated owner. It is evidence
	// for the latest-version count, not a second active owner to create today.
	seedAuthorityFixture(t, eng, conceptIdentityUser, "historical-owner", map[string]any{"role": "owner", "active": true})
	seedAuthorityFixture(t, eng, conceptIdentityUser, "historical-owner", map[string]any{"role": "owner", "active": false})
	current := newOwner(t, eng, ctx, "current-owner")
	require.Error(t, tryMutation(t, ctx, eng, "deleteUserHard", map[string]any{"userId": current}))
}
