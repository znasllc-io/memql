package memql

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
)

// query_predicate_audit_1708_db_test.go is the real-engine reproduction + fix
// guard for the query-predicate/join correctness audit (memql#1708) and the
// folded-in consumed-auth-code predicate (memql#1714).
//
// Every case seeds BOTH a row that must match the query's documented contract
// AND a row that must NOT, then asserts the inclusion half (matching row
// returned) AND the exclusion half (non-matching row omitted). A single-sided
// assertion would pass against a too-broad filter (returns everything) just as
// happily as against a correct one, which is exactly the bug class #1708
// catalogues -- so both halves are mandatory.
//
// The four #1708 queries:
//   queryDocumentsForDomain    -- joined `attachedDomains==domainId` (a []string
//                                 compared to a scalar -> never matched). Fixed
//                                 to `domainId in attachedDomains`.
//   avatarPersonas             -- hardcoded `vendor=="simli"` dropped every other
//                                 vendor's personas. Fixed to an optional vendor
//                                 arg: `isActiveRecord && when(args.vendor)
//                                 { vendor==args.vendor }`.
//   usersScheduledForDeletion / usersInDeletionCooldown -- must return
//                                 ONLY users with deletionScheduledAt set, not
//                                 every active user.
// Plus #1714:
//   expiredConsumedAuthCodes -- name says Consumed; filter now gates on
//                                 consumedAt!="" so unspent-but-expired codes are
//                                 excluded.
//
// avatarPersona went with the legacy cognition tree, so the shape that case
// pinned -- an unconditional conjunct ANDed with an optional equality guard --
// is now asserted over `todos`, whose filter is that shape exactly
// (`ownerUserId==actor.userId && when(args.done) { done==args.done }`). The
// property was never about avatars: it is that an omitted optional arg must
// not silently narrow the read, and that supplying it must still narrow.
//
// Postgres-gated: skips when no DB is reachable, reusing sharedReadMergeEngine
// (component/memql/executor_mutation_readmerge_db_test.go).

// --- #1708: queryDocumentsForDomain joins on attachedDomains membership ------

// --- #1708: an optional equality guard neither narrows when omitted --------
// --- nor stops narrowing when supplied ------------------------------------

// TestQueryTodos_OptionalDoneFilterNotHardcoded is the surviving statement of
// the avatarPersonas case. `todos` carries the same two-part filter the fixed
// avatarPersonas carried -- one unconditional conjunct ANDed with
// `when(args.done) { done==args.done }` -- so the same three questions have the
// same three answers here:
//
//   - omitted, the guard drops and BOTH values of the discriminator come back
//     (the half that failed: a hardcoded vendor==simli returned no anam row);
//   - supplied, it still narrows, in both directions;
//   - the unconditional conjunct keeps applying either way, so a row it
//     excludes is never admitted by the optional half being absent.
//
// The exclusion control is a to-do belonging to somebody ELSE, which is
// stronger than the inactive-persona row it replaces: it is refused by the
// filter's ownerUserId conjunct AND, independently, by the concept's
// @rowAuthz(owner="ownerUserId") tier. Both halves have to fail for it to
// appear, and either failing is a bug worth this test.
//
// A present `false` is deliberately one of the cases: the when-guard drops on
// ABSENCE and keeps a present false, so `todos(done: false)` must NARROW to the
// open item rather than widen to everything.
func TestQueryTodos_OptionalDoneFilterNotHardcoded(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	sfx := uniqueSuffix("todo-optional")
	ctx := clusterOwnerCtx("u-todo-1708-" + sfx)
	strangerCtx := clusterOwnerCtx("u-todo-1708-other-" + sfx)

	openID := runMutation(t, ctx, eng, "createTodo", map[string]any{
		"todoId": fmt.Sprintf("v1:todos:todo:open-%s", sfx), "title": "Still open",
	})
	doneID := runMutation(t, ctx, eng, "createTodo", map[string]any{
		"todoId": fmt.Sprintf("v1:todos:todo:done-%s", sfx), "title": "Already done",
	})
	// completeTodo is authored as a partial insert{}, so the engine read-merge
	// carries title/priority forward and only `done` changes.
	runMutation(t, ctx, eng, "completeTodo", map[string]any{
		"todoId": doneID, "payload": map[string]any{"done": true},
	})
	strangerID := runMutation(t, strangerCtx, eng, "createTodo", map[string]any{
		"todoId": fmt.Sprintf("v1:todos:todo:stranger-%s", sfx), "title": "Not yours",
	})

	// Default (no done arg): every to-do the caller owns, whatever its state.
	// The headline #1708 assertion -- the completed item used to be the one a
	// hardcoded predicate silently dropped.
	all := queryIds(t, ctx, eng, "todos()")
	require.True(t, contains(all, openID), "the open to-do MUST be listed, got %v", all)
	require.True(t, contains(all, doneID),
		"the completed to-do MUST be listed -- an omitted optional arg drops its guard and must "+
			"not narrow the read (#1708), got %v", all)
	require.False(t, contains(all, strangerID),
		"another user's to-do MUST be excluded -- the unconditional ownerUserId conjunct still "+
			"applies when the optional guard drops, got %v", all)

	// Supplied, the guard still narrows -- in both directions.
	doneOnly := queryIds(t, ctx, eng, "todos(done: true)")
	require.True(t, contains(doneOnly, doneID), "done=true MUST include the completed to-do, got %v", doneOnly)
	require.False(t, contains(doneOnly, openID), "done=true MUST exclude the open to-do, got %v", doneOnly)
	require.False(t, contains(doneOnly, strangerID), "done=true MUST exclude another user's to-do, got %v", doneOnly)

	openOnly := queryIds(t, ctx, eng, "todos(done: false)")
	require.True(t, contains(openOnly, openID), "done=false MUST include the open to-do, got %v", openOnly)
	require.False(t, contains(openOnly, doneID), "done=false MUST exclude the completed to-do, got %v", openOnly)
	require.False(t, contains(openOnly, strangerID), "done=false MUST exclude another user's to-do, got %v", openOnly)
}

// --- #1708: user-deletion queries gate on deletionScheduledAt ---------------

func TestQueryUsersScheduledForDeletion_OnlyScheduled(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	ctx := clusterOwnerCtx("u-deldel-1708")
	sfx := uniqueSuffix("deldel")

	scheduledID := fmt.Sprintf("v1:identity:user:sched-%s", sfx)
	plainID := fmt.Sprintf("v1:identity:user:plain-%s", sfx)

	for i, id := range []string{scheduledID, plainID} {
		runMutation(t, ctx, eng, "createUser", map[string]any{
			"userId": id, "displayName": fmt.Sprintf("Del %d", i),
			"primaryEmail": fmt.Sprintf("del-%d-%s@example.com", i, sfx),
		})
	}
	// Only the first gets a pending deletion.
	runMutation(t, ctx, eng, "scheduleAccountDeletion", map[string]any{"userId": scheduledID})

	// Both queries became @serverOnly in memql#2883 -- they take no required
	// args and project userFull, so a client-originated call returned every
	// cooling-off user's full row. The predicate behaviour this test is about
	// (#1708) is unchanged; only the origin gate is new, so the read is
	// stamped the way its real callers reach it: the deletion-sweep logics run
	// as automation steps, which executeStep stamps with OriginInternal.
	sweepCtx := auth.ContextWithInternalOrigin(ctx)

	for _, q := range []string{"usersScheduledForDeletion()", "usersInDeletionCooldown()"} {
		got := queryIds(t, sweepCtx, eng, q)
		require.True(t, contains(got, scheduledID),
			"%s MUST return the user with deletionScheduledAt set (#1708), got %v", q, got)
		require.False(t, contains(got, plainID),
			"%s MUST exclude an active user with NO deletionScheduledAt -- it must not return every active user (#1708), got %v", q, got)

		// The other half of #2883, asserted here because this is the only
		// place these two run against a real engine with real rows: a CLIENT
		// call must be refused outright rather than quietly returning the set
		// above. A cluster OWNER is used deliberately -- the gate is ORIGIN,
		// not role, so even the most privileged wire caller is refused.
		_, err := eng.Execute(ctx, q)
		require.Error(t, err, "%s must refuse a client-originated call (#2883)", q)
		require.Contains(t, err.Error(), "server-only",
			"%s refused a client call for the wrong reason: %v", q, err)
	}
}

// --- #1714: expiredConsumedAuthCodes gates on consumedAt ---------------

func TestQueryExpiredConsumedAuthCodes_OnlyConsumed(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	ctx := clusterOwnerCtx("u-authcode-1714")
	sfx := uniqueSuffix("authcode")

	consumedID := fmt.Sprintf("v1:identity:authCode:consumed-%s", sfx)
	unconsumedID := fmt.Sprintf("v1:identity:authCode:unconsumed-%s", sfx)

	seedCode := func(id string) {
		runMutation(t, ctx, eng, "createAuthCode", map[string]any{
			"codeId": id, "code": "plain-" + id, "codeHash": "hash-" + id,
			"clientId": "client-1", "redirectURI": "https://app.example.com/cb",
			"userId": "u-authcode-1714", "identityId": "v1:identity:identity:i-" + sfx,
			"magicLinkRequestId": "v1:identity:magicLinkRequest:ml-" + sfx,
			// Both codes are expired (well in the past) so expiry alone never
			// distinguishes them -- only consumedAt does.
			"expiresAt": "2020-01-01T00:00:00Z",
		})
	}
	seedCode(consumedID)
	seedCode(unconsumedID)

	// Spend exactly one of the two expired codes.
	runMutation(t, ctx, eng, "consumeAuthCode", map[string]any{"codeId": consumedID})

	got := queryIds(t, ctx, eng, `expiredConsumedAuthCodes(asOf:"2026-06-18T00:00:00Z")`)
	require.True(t, contains(got, consumedID),
		"an expired AND consumed auth code MUST be returned (#1714), got %v", got)
	require.False(t, contains(got, unconsumedID),
		"an expired-but-UNCONSUMED auth code MUST be excluded -- the name says Consumed (#1714), got %v", got)
}
