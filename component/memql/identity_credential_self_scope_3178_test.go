package memql

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// identity_credential_self_scope_3178_test.go is the memql#3178 guard.
//
// `patIdentitiesForUser` and `badgesForUser` filtered on `userId==args.userId`
// -- a CALLER-SUPPLIED id with no check that the caller IS that user -- so any
// authenticated client could enumerate a stranger's credential metadata just by
// passing their id. #3178 splits the PAT query into a self-scoped half
// (`userId==actor.userId`, no userId arg) backing /me/tokens and an
// admin-gated half (`requiresOwnerOrAdmin` as a top-level conjunct) backing the
// operator CLI, and moves badges onto the self-scoped shape.
//
// FIDELITY. These tests do not grep the .memql source and they do not
// hand-build an expression. They boot a REAL engine with the REAL embedded DSL,
// parse the EXACT query string the Go store sends
// (`query patIdentitiesForSelf()`, `query patIdentitiesForUser(userId:"...")`),
// and then run the engine's own post-filter pipeline over it:
//
//	parseWithFunctionsAmbient  -> the production plan, caller args bound
//	expandSpecReferences       -> identityIsApiKey / requiresOwnerOrAdmin inlined
//	resolveActorComparisons... -> actor.userId / actor.role folded to constants
//	nodeMatchesExpression      -> the SAME predicate the executor applies per row
//
// That last step is the one that makes this a row-level proof: a row that does
// not match here is a row the executor drops, so "does not match" IS "returns
// zero rows". No database is required, which is why this guard runs in CI
// rather than skipping.

// selfScopeEngine boots a real MemQLEngine with the full embedded DSL loaded
// (no DB). Parser, spec registry, shape registry and named-query resolution are
// all live; only the DB-backed row fetch is absent, and these tests supply
// their own rows.
func selfScopeEngine(t *testing.T) *MemQLEngine {
	t.Helper()
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts (dsl/ domain-first tree): %v", err)
	}
	registry := concept.DefaultRegistry()
	require.NotNil(t, registry)
	eng, err := New(nil)
	require.NoError(t, err)
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	require.NoError(t, eng.Init(registry))
	return eng
}

// identityRowNode builds a v1:identity:identity row owned by userId, carrying
// the fields every filter under test reads: userId (the ownership key) and
// identityType (what identityIsApiKey / identityIsBadge discriminate on).
func identityRowNode(t *testing.T, id, userId, identityType string) memorynodes.MemoryNode {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"userId":       userId,
		"identityType": identityType,
		"label":        "row for " + userId,
		"active":       true,
	})
	require.NoError(t, err)
	return memorynodes.MemoryNode{
		ID:      id,
		Concept: "v1:identity:identity",
		Payload: raw,
	}
}

// evaluableFilter parses a query string through the real engine and returns the
// filter in exactly the form executeCombinedFilterQuery post-filters with.
func evaluableFilter(t *testing.T, eng *MemQLEngine, ctx context.Context, queryString string) ExpressionNode {
	t.Helper()
	ambient := buildAmbientEnvelope(ctx, eng)
	plan, err := eng.parseWithFunctionsAmbient(queryString, eng.functions, nil, false,
		auth.OriginFromContext(ctx), ambient)
	require.NoErrorf(t, err, "parsing %q against the loaded DSL", queryString)
	require.NotNilf(t, plan.Root, "query %q produced no filter", queryString)

	expr := unwrapToFilter(plan.Root)
	expanded, err := eng.expandSpecReferences(expr)
	require.NoError(t, err)
	resolved, err := resolveActorComparisonsToConstants(ctx, expanded)
	require.NoError(t, err)
	resolved, err = resolveActorReferences(ctx, resolved)
	require.NoError(t, err)
	return resolved
}

func matchesFilter(t *testing.T, node memorynodes.MemoryNode, expr ExpressionNode) bool {
	t.Helper()
	ok, err := nodeMatchesExpression(node, expr, map[string]map[string]any{})
	require.NoError(t, err)
	return ok
}

// selfScopeUserCtx is an ordinary authenticated caller -- the shape /me/tokens
// runs under once #3178 stamps the signed-in user onto the request context.
func selfScopeUserCtx(userId string) context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: userId,
		Role:   auth.RoleWriter,
	})
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: userId})
}

// selfScopeOperatorCtx is the operator CLI's context: `memql pat list` runs
// unauthenticated by design and stamps the system actor, which carries
// role=owner (component/identity/middleware.go ContextWithSystemActor).
func selfScopeOperatorCtx(userId string) context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: userId,
		Role:   auth.RoleOwner,
	})
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: userId})
}

const (
	selfScopeAlice = "v1:identity:user:alice-3178"
	selfScopeBob   = "v1:identity:user:bob-3178"
)

// TestPatSelfQueryDeniesAStrangersTokens is the headline memql#3178 acceptance
// criterion: an authenticated caller who tries to read a stranger's tokens gets
// ZERO rows from the self-scoped query.
func TestPatSelfQueryDeniesAStrangersTokens(t *testing.T) {
	eng := selfScopeEngine(t)

	alicePAT := identityRowNode(t, "v1:identity:identity:pat-alice", selfScopeAlice, "api_key")
	bobPAT := identityRowNode(t, "v1:identity:identity:pat-bob", selfScopeBob, "api_key")

	// Alice is the authenticated caller. The query takes no userId at all, so
	// Bob's rows are unreachable no matter what she sends.
	filter := evaluableFilter(t, eng, selfScopeUserCtx(selfScopeAlice), `query patIdentitiesForSelf()`)

	assert.False(t, matchesFilter(t, bobPAT, filter),
		"an authenticated caller matched a STRANGER's PAT row through the self-scoped query -- "+
			"the filter is still keyed on a caller-supplied id (memql#3178)")
	assert.True(t, matchesFilter(t, alicePAT, filter),
		"the caller must still see her OWN PATs, or /me/tokens renders empty for everyone")
}

// The self-scoped query must not merely ignore a userId argument -- it must not
// declare one, so there is no caller-supplied id in the filter at all.
func TestPatSelfQueryDeclaresNoUserIdArg(t *testing.T) {
	eng := selfScopeEngine(t)
	fn, err := eng.functions.Get("patIdentitiesForSelf")
	require.NoError(t, err)

	if fn.ArgsSchema != nil {
		for _, f := range fn.ArgsSchema.Fields {
			assert.NotEqual(t, "userId", f.Name,
				"the self-scoped query declares a userId arg; the caller must not be able to "+
					"name whose tokens get listed (memql#3178)")
		}
	}
}

// TestPatAdminQueryStillServesTheOperatorCLI is the other half of the
// acceptance criterion: the CLI path still returns rows. `memql pat list
// --user-id <bob>` is an operator listing somebody else's tokens on purpose, so
// the admin-gated arm must keep admitting it.
func TestPatAdminQueryStillServesTheOperatorCLI(t *testing.T) {
	eng := selfScopeEngine(t)
	bobPAT := identityRowNode(t, "v1:identity:identity:pat-bob", selfScopeBob, "api_key")
	q := `query patIdentitiesForUser(userId:"` + selfScopeBob + `")`

	cliFilter := evaluableFilter(t, eng, selfScopeOperatorCtx("system:identity-svc"), q)
	assert.True(t, matchesFilter(t, bobPAT, cliFilter),
		"the operator CLI (`memql pat list --user-id ...`) got ZERO rows -- the admin arm of "+
			"the split must still serve it (memql#3178)")

	// The gate is real: an ordinary authenticated user gets nothing from it.
	strangerFilter := evaluableFilter(t, eng, selfScopeUserCtx(selfScopeAlice), q)
	assert.False(t, matchesFilter(t, bobPAT, strangerFilter),
		"a NON-admin caller read a stranger's PAT rows through the admin-gated query -- "+
			"requiresOwnerOrAdmin is not holding (memql#3178)")
}

// Badges follow the PAT self half. Evidence (recorded on memql#3178): the
// cockpit calls neither query, `badge.Store.ListForUser`'s only production
// caller passes the CALLER's own id, and no admin badge surface exists in
// either repo -- so nothing needs to read another user's badges.
func TestBadgeSelfQueryDeniesAStrangersBadges(t *testing.T) {
	eng := selfScopeEngine(t)

	aliceBadge := identityRowNode(t, "v1:identity:identity:badge-alice", selfScopeAlice, "badge")
	bobBadge := identityRowNode(t, "v1:identity:identity:badge-bob", selfScopeBob, "badge")

	filter := evaluableFilter(t, eng, selfScopeUserCtx(selfScopeAlice), `query badgesForSelf()`)

	assert.False(t, matchesFilter(t, bobBadge, filter),
		"an authenticated caller matched a STRANGER's badge row through the self-scoped "+
			"query (memql#3178)")
	assert.True(t, matchesFilter(t, aliceBadge, filter),
		"the caller must still see her OWN badges")
}

func TestBadgeSelfQueryDeclaresNoUserIdArg(t *testing.T) {
	eng := selfScopeEngine(t)
	fn, err := eng.functions.Get("badgesForSelf")
	require.NoError(t, err)

	if fn.ArgsSchema != nil {
		for _, f := range fn.ArgsSchema.Fields {
			assert.NotEqual(t, "userId", f.Name,
				"the self-scoped badge query declares a userId arg (memql#3178)")
		}
	}
}

// The caller-supplied-id badge query must be GONE, not merely unreferenced --
// an ungated copy left registered is the hole still open, reachable by any
// client that knows the name.
func TestRetiredCallerSuppliedBadgeQueryIsGone(t *testing.T) {
	eng := selfScopeEngine(t)
	_, err := eng.functions.Get("badgesForUser")
	assert.Error(t, err,
		"badgesForUser is still registered; #3178 replaces it with the self-scoped "+
			"badgesForSelf, and leaving the caller-supplied-id version in the tree leaves "+
			"the hole open")
}
