package memql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
)

// The account stamp's decisions that need no row read: a root concept (no `via`,
// no list) decides from the payload, the stored row and the call alone, so these
// run on an engine with no database. The database-gated half, through Execute
// and read back off the table, is account_stamp_db_test.go.

func stampRootConcept(decl langparser.BelongsToDecl) *memorynodes.Concept {
	return &memorynodes.Concept{Name: "v1:stamptest:root", BelongsTo: &decl}
}

func stampEngine(t *testing.T) *MemQLEngine {
	t.Helper()
	e := newQuietEngine(t)
	e.SetAccountStampingEnabled(true)
	return e
}

func trustedIn(account string) context.Context {
	ctx := auth.ContextWithInternalOrigin(rankActorCtx("stamp-unit-operator", auth.RoleOwner))
	return auth.ContextWithCallAccount(ctx, account)
}

func rawPersonIn(account string) context.Context {
	return auth.ContextWithCallAccount(rankActorCtx("stamp-unit-person", auth.RoleWriter), account)
}

// TestAccountStampCarriesTheStoredAccountForward: a write that replaces the
// whole row, so the payload names no account, keeps the one the stored row
// records, in the spelling it was stored in.
func TestAccountStampCarriesTheStoredAccountForward(t *testing.T) {
	e := stampEngine(t)
	c := stampRootConcept(langparser.BelongsToDecl{Scope: langparser.BelongsToAccount})

	for name, ctx := range map[string]context.Context{"trusted Go": trustedIn("beta"), "the raw surface": rawPersonIn("beta")} {
		for _, spelling := range []string{"acme", conceptAccountsAccount + ":acme"} {
			prior := map[string]any{"body": "old", "accountId": spelling}
			payload := map[string]any{"body": "whole new row"}
			require.NoError(t, e.stampAccount(ctx, c, payload, prior, captureAccountDelta(payload), false), name)
			require.Equal(t, spelling, payload["accountId"], "%s: the stored account was not carried forward as stored", name)
		}
	}

	// A template too.
	payload := map[string]any{"body": "re-inserted"}
	require.NoError(t, e.stampAccount(trustedIn("beta"), c, payload, map[string]any{"accountId": "acme"}, captureAccountDelta(payload), true))
	require.Equal(t, "acme", payload["accountId"])

	// A stored row with no account is not given the call's.
	payload = map[string]any{"body": "re-inserted"}
	require.NoError(t, e.stampAccount(trustedIn("beta"), c, payload, map[string]any{"body": "old"}, captureAccountDelta(payload), false))
	_, has := payload["accountId"]
	require.False(t, has)
}

// TestAccountStampEmptyDeltaIsNoKey: the empty string in the delta, after the
// read-merge has put it over the stored value, is undone to what a write naming
// nothing would store.
func TestAccountStampEmptyDeltaIsNoKey(t *testing.T) {
	e := stampEngine(t)
	c := stampRootConcept(langparser.BelongsToDecl{Scope: langparser.BelongsToAccount})

	delta := map[string]any{"accountId": ""}
	merged := map[string]any{"body": "x", "accountId": ""}
	require.NoError(t, e.stampAccount(rawPersonIn("beta"), c, merged, map[string]any{"accountId": "acme"}, captureAccountDelta(delta), false),
		"the empty string was read as a move")
	require.Equal(t, "acme", merged["accountId"])

	// On a create it is the call's account, as no key would be.
	created := map[string]any{"body": "x", "accountId": ""}
	require.NoError(t, e.stampAccount(trustedIn("beta"), c, created, nil, captureAccountDelta(created), true))
	require.Equal(t, "beta", created["accountId"])

	// And on a shared concept, the same: a cluster-provided row is one nothing
	// decides, not one whose writer typed the empty string.
	shared := stampRootConcept(langparser.BelongsToDecl{Scope: langparser.BelongsToAccount, Shared: true})
	created = map[string]any{"body": "x", "accountId": ""}
	require.NoError(t, e.stampAccount(trustedIn("beta"), shared, created, nil, captureAccountDelta(created), true))
	require.Equal(t, "beta", created["accountId"])

	// With nothing to decide, the key is gone rather than stored empty.
	created = map[string]any{"body": "x", "accountId": ""}
	require.NoError(t, e.stampAccount(context.Background(), c, created, nil, captureAccountDelta(created), true))
	_, has := created["accountId"]
	require.False(t, has, "an empty account was stored where no key would have been")
}

// TestAccountStampOffWritesTheFieldForNobody: before the switch the field is
// written for no one. An account named in a write, by a person or by server
// code, through a template or not, is dropped: the row keeps what it had
// stored, or no key. A value planted while the checks were not running would
// otherwise be believed once they were.
func TestAccountStampOffWritesTheFieldForNobody(t *testing.T) {
	e := newQuietEngine(t)
	c := stampRootConcept(langparser.BelongsToDecl{Scope: langparser.BelongsToAccount})

	for name, tc := range map[string]struct {
		ctx          context.Context
		fromTemplate bool
	}{
		"a person's raw write":     {rawPersonIn("acme"), false},
		"a person's template":      {rawPersonIn("acme"), true},
		"server code":              {trustedIn("acme"), false},
		"server code's template":   {trustedIn("acme"), true},
		"nobody, with no call set": {context.Background(), false},
	} {
		// A create that names an account stores none.
		created := map[string]any{"body": "x", "accountId": "beta"}
		require.NoError(t, e.stampAccount(tc.ctx, c, created, nil, captureAccountDelta(created), tc.fromTemplate), name)
		require.Equal(t, map[string]any{"body": "x"}, created, "%s: an account was written before the switch", name)

		// An update that names one keeps the stored value, here a real stamp
		// another replica wrote after the switch turned on for the cluster.
		delta := map[string]any{"accountId": "beta"}
		merged := map[string]any{"body": "x", "accountId": "beta"}
		require.NoError(t, e.stampAccount(tc.ctx, c, merged, map[string]any{"body": "x", "accountId": "acme"}, captureAccountDelta(delta), tc.fromTemplate), name)
		require.Equal(t, "acme", merged["accountId"], "%s: a held node changed a stored account", name)

		// A write that names none is left exactly as it came.
		plain := map[string]any{"body": "x"}
		require.NoError(t, e.stampAccount(tc.ctx, c, plain, nil, captureAccountDelta(plain), tc.fromTemplate), name)
		require.Equal(t, map[string]any{"body": "x"}, plain, name)
	}
}

// TestAccountStampLeavesAStatedConceptToItsWriters: the `stated` form returns
// before the payload is touched, empty string included, switch on or off.
func TestAccountStampLeavesAStatedConceptToItsWriters(t *testing.T) {
	e := newQuietEngine(t)
	stated := stampRootConcept(langparser.BelongsToDecl{Scope: langparser.BelongsToAccount, Stated: true})
	for _, on := range []bool{false, true} {
		e.SetAccountStampingEnabled(on)
		for _, named := range []string{"", "beta"} {
			payload := map[string]any{"accountId": named}
			require.NoError(t, e.stampAccount(trustedIn("beta"), stated, payload, map[string]any{"accountId": "acme"}, captureAccountDelta(payload), false))
			require.Equal(t, map[string]any{"accountId": named}, payload, "a stated concept is left to its writers (switch on: %v)", on)
		}
	}
}

func TestPickListedAccountAndSameList(t *testing.T) {
	require.Equal(t, "a", pickListedAccount([]string{"b", "a"}, "a"))
	require.Equal(t, "b", pickListedAccount([]string{"b", "a"}, "c"))
	require.Equal(t, "b", pickListedAccount([]string{"b", "a"}, ""))
	require.Equal(t, []string{"b", "a"}, accountList([]any{"v1:accounts:account:b", " ", "a", 3}))
	require.True(t, sameAccountList([]string{"a", "b"}, []string{"a", "b"}))
	require.False(t, sameAccountList([]string{"a", "b"}, []string{"b", "a"}))
	require.False(t, sameAccountList([]string{"a"}, nil))
}
