package memql

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// last_owner_deletion_db_test.go -- memql#3967's acceptance criterion, against
// a real database and through the real mutation path.
//
// POSTGRES-GATED (reuses sharedReadMergeEngine) because the guard's whole job is
// a COUNT over versioned rows. A stubbed count would be asserting that the
// stub returns what the stub was told to return; the interesting cases -- a
// demoted owner, a previously-deactivated owner, an older version of a row
// that used to be an owner -- exist only in real row history.
//
// The four tests below are one property from four directions. Deleting the
// last owner is refused; deleting one of two is not; deleting a non-owner is
// not; and an owner whose ONLY other owner has already been deactivated is
// treated as the last one, which is the case a naive `COUNT(*) WHERE role =
// 'owner'` would get wrong.

// isolateSingleOwner deactivates every active owner EXCEPT keep, so the
// "exactly one owner" precondition holds whatever the database arrived
// carrying.
//
// THIS REPLACED A t.Skip, AND THE SKIP WAS THE BUG. The first version of these
// tests counted the owners already present and skipped when there were any --
// which, because the engine's seed materializer creates one at boot, meant the
// refusal case skipped on EVERY run, including CI. A test that never executes
// its assertion is indistinguishable from one that has none, and this is the
// assertion the whole guard exists for.
//
// Deactivating the others is legitimate rather than a fixture hack: `keep` is
// active throughout, so each of those deletions is a delete-one-of-several,
// which the guard is supposed to allow. The setup exercises the permissive
// branch on its way to setting up the refusing one.
func isolateSingleOwner(t *testing.T, ctx context.Context, eng *MemQLEngine, db *bun.DB, keep string) {
	t.Helper()
	var nodes []concept.MemoryNode
	err := db.NewSelect().Model(&nodes).
		Where("concept = ?", conceptIdentityUser).
		OrderExpr(`"createdAt" DESC`).
		Scan(ctx)
	require.NoError(t, err)

	// Stored ids are CANONICAL (`v1:identity:user:<shortId>`) while newOwner
	// hands back the bare short id. Comparing the two spellings directly made
	// this helper deactivate the very owner it was told to keep, and the
	// refusal it then produced looked like the guard misfiring on an unrelated
	// row (memql#3967).
	keepCanonical := keep
	if !strings.HasPrefix(keep, conceptIdentityUser+":") {
		keepCanonical = conceptIdentityUser + ":" + keep
	}

	seen := map[string]struct{}{}
	for i := range nodes {
		n := nodes[i]
		id := strings.TrimSpace(n.ID)
		if id == "" || id == keep || id == keepCanonical {
			continue
		}
		if _, done := seen[id]; done {
			continue // an older version already decided this id
		}
		seen[id] = struct{}{}
		if !latestRowIsActiveOwner(n) {
			continue
		}
		require.NoError(t, tryMutation(t, ctx, eng, "deleteUserHard", map[string]any{"userId": id}),
			"deactivating another owner while %s is still active must be allowed", keep)
	}

	others, err := eng.countOtherActiveOwners(ctx, keep)
	require.NoError(t, err)
	require.Zero(t, others, "isolateSingleOwner left %d other active owner(s); the refusal case "+
		"cannot be asserted until exactly one remains", others)
}

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

// TestDeletingTheLastOwnerIsRefused is the headline claim.
func TestDeletingTheLastOwnerIsRefused(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)

	owner := newOwner(t, eng, ctx, "lastowner")
	isolateSingleOwner(t, ctx, eng, db, owner)

	err := tryMutation(t, ctx, eng, "deleteUserHard", map[string]any{"userId": owner})
	require.Error(t, err, "deleting the cluster's only owner must be refused -- nothing could then "+
		"promote a replacement, and any recovery key bound to them becomes unredeemable")
	require.Contains(t, strings.ToLower(err.Error()), "last owner",
		"the refusal must say what it refused, so an operator can act on it: %v", err)
}

// TestDeletingOneOfTwoOwnersIsAllowed is the other half, and the one that
// stops the guard from being "owners are undeletable".
func TestDeletingOneOfTwoOwnersIsAllowed(t *testing.T) {
	eng, _, ctx := sharedReadMergeEngine(t)

	first := newOwner(t, eng, ctx, "twoowners-a")
	_ = newOwner(t, eng, ctx, "twoowners-b")

	err := tryMutation(t, ctx, eng, "deleteUserHard", map[string]any{"userId": first})
	require.NoError(t, err, "deleting one of two owners must be allowed; the guard counts owners, "+
		"it does not make the role undeletable")
}

// TestDeletingANonOwnerIsUntouched: the guard must not become a tax on every
// user deletion.
func TestDeletingANonOwnerIsUntouched(t *testing.T) {
	eng, _, ctx := sharedReadMergeEngine(t)

	reader := newReader(t, eng, ctx, "nonowner")
	err := tryMutation(t, ctx, eng, "deleteUserHard", map[string]any{"userId": reader})
	require.NoError(t, err, "deleting a non-owner is not this guard's business")
}

// TestAnAlreadyDeactivatedOwnerDoesNotCount is the case a naive count gets
// wrong, and the reason countOtherActiveOwners resolves each id to its LATEST
// version rather than trusting the id-level scan.
//
// Two owners exist; one is deleted (allowed). The survivor is then the last
// one, and deleting it must be refused -- even though a `SELECT ... WHERE
// payload->>'role' = 'owner'` still matches two ids, because the deactivated
// owner's OLD versions are still in the table.
func TestAnAlreadyDeactivatedOwnerDoesNotCount(t *testing.T) {
	eng, db, ctx := sharedReadMergeEngine(t)

	first := newOwner(t, eng, ctx, "stale-a")
	isolateSingleOwner(t, ctx, eng, db, first)
	second := newOwner(t, eng, ctx, "stale-b")

	err := tryMutation(t, ctx, eng, "deleteUserHard", map[string]any{"userId": first})
	require.NoError(t, err, "the first of two owners deletes fine")

	err = tryMutation(t, ctx, eng, "deleteUserHard", map[string]any{"userId": second})
	require.Error(t, err, "the survivor is now the LAST active owner and must be refused.\n"+
		"A count that trusted the id-level scan would still see two owner-shaped ids here -- the "+
		"deleted owner's older versions are all still in the table -- and would allow this write, "+
		"leaving the cluster with none.")
}
