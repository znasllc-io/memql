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
//   avatarPersonas        -- hardcoded `vendor=="simli"` dropped every other
//                                 vendor's personas. Fixed to an optional vendor arg.
//   usersScheduledForDeletion / usersInDeletionCooldown -- must return
//                                 ONLY users with deletionScheduledAt set, not
//                                 every active user.
// Plus #1714:
//   expiredConsumedAuthCodes -- name says Consumed; filter now gates on
//                                 consumedAt!="" so unspent-but-expired codes are
//                                 excluded.
//
// Postgres-gated: skips when no DB is reachable, reusing readMergeTestEngine
// (component/memql/executor_mutation_readmerge_db_test.go).

// --- #1708: queryDocumentsForDomain joins on attachedDomains membership ------

// --- #1708: avatarPersonas no longer hardcodes vendor==simli ------------

func TestQueryAvatarPersonas_VendorOptionalNotHardcoded(t *testing.T) {
	eng, _, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-persona-1708")
	sfx := uniqueSuffix("persona")

	simliID := fmt.Sprintf("v1:agents:avatarPersona:simli-%s", sfx)
	anamID := fmt.Sprintf("v1:agents:avatarPersona:anam-%s", sfx)
	inactiveID := fmt.Sprintf("v1:agents:avatarPersona:inactive-%s", sfx)

	runMutation(t, ctx, eng, "createAvatarPersona", map[string]any{
		"avatarPersonaId": simliID, "vendor": "simli", "personaId": "face-simli-" + sfx,
		"name": "Simli One", "gender": "female", "active": true,
	})
	runMutation(t, ctx, eng, "createAvatarPersona", map[string]any{
		"avatarPersonaId": anamID, "vendor": "anam", "personaId": "face-anam-" + sfx,
		"name": "Anam One", "gender": "male", "active": true,
	})
	runMutation(t, ctx, eng, "createAvatarPersona", map[string]any{
		"avatarPersonaId": inactiveID, "vendor": "simli", "personaId": "face-inactive-" + sfx,
		"name": "Inactive One", "gender": "female", "active": false,
	})

	// Default (no vendor arg): every ACTIVE persona regardless of vendor.
	// The headline #1708 assertion -- the anam persona used to be silently
	// dropped by the hardcoded vendor==simli predicate.
	all := queryIds(t, ctx, eng, "avatarPersonas()")
	require.True(t, contains(all, simliID), "active simli persona MUST be listed, got %v", all)
	require.True(t, contains(all, anamID),
		"active anam persona MUST be listed -- the vendor filter is no longer hardcoded (#1708), got %v", all)
	require.False(t, contains(all, inactiveID),
		"inactive persona MUST be excluded (isActiveRecord), got %v", all)

	// Optional vendor scoping still works: vendor=simli excludes anam.
	simliOnly := queryIds(t, ctx, eng, `avatarPersonas(vendor:"simli")`)
	require.True(t, contains(simliOnly, simliID), "vendor=simli MUST include the simli persona, got %v", simliOnly)
	require.False(t, contains(simliOnly, anamID), "vendor=simli MUST exclude the anam persona, got %v", simliOnly)

	anamOnly := queryIds(t, ctx, eng, `avatarPersonas(vendor:"anam")`)
	require.True(t, contains(anamOnly, anamID), "vendor=anam MUST include the anam persona, got %v", anamOnly)
	require.False(t, contains(anamOnly, simliID), "vendor=anam MUST exclude the simli persona, got %v", anamOnly)
}

// --- #1708: user-deletion queries gate on deletionScheduledAt ---------------

func TestQueryUsersScheduledForDeletion_OnlyScheduled(t *testing.T) {
	eng, _, _ := readMergeTestEngine(t)
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
	eng, _, _ := readMergeTestEngine(t)
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
