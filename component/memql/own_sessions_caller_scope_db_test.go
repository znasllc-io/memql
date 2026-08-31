package memql

// own_sessions_caller_scope_db_test.go -- the LIVE half of memql#4768.
//
// `authSessionsForSelfIncludingRevoked` replaced `authSessionsForSubject`,
// which filtered on a caller-supplied `subject` and nothing else -- no role
// gate, not @serverOnly -- so any signed-in caller could read anyone's
// sessions. The replacement takes NO argument and filters
// `subject==actor.userId`.
//
// component/grpc/own_sessions_scope_test.go pins the WIRING statically: no
// argument reaches the read, and it runs on the stream context rather than the
// elevated one. What that cannot see is whether the filter MATCHES.
//
// That is the dangerous half, and it fails silently in both directions:
//
//   - If `actor.userId` does not line up with the stored `subject`, the query
//     returns nothing, and "revoke all my sessions" reports revoking zero on a
//     healthy account. No error, no log line, and an empty session list is a
//     completely plausible answer.
//   - If the filter were dropped or mis-bound, it returns EVERYONE, which is
//     the bug the change exists to fix and which a "did I get my rows back"
//     assertion alone would pass.
//
// So both directions are asserted here, against a real engine and database,
// with two subjects in the table at once. A test with one subject in it cannot
// tell a working filter from an absent one.

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/id"
)

func TestAuthSessionsForSelf_IsScopedToTheCallerAndMatchesRows(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	if eng == nil {
		return // skipped: no database
	}

	// Two people, both with live sessions, both in the table at the same
	// time. The second one is the whole point: it is what makes a passing
	// result evidence of a filter rather than of an empty neighbourhood.
	mine := "v1:identity:user:" + id.NewShortId()
	theirs := "v1:identity:user:" + id.NewShortId()

	writeSession := func(subject string) string {
		t.Helper()
		sessionID := "v1:identity:authSession:" + id.NewShortId()
		// Written under the row's own subject as actor, the way the identity
		// service writes one.
		ctx := auth.ContextWithInternalOrigin(auth.ContextWithUserActor(nil, subject))
		call := fmt.Sprintf(
			`createAuthSession(sessionId: %q, subject: %q, tokenHash: %q, source: "bff_exchange", expiresAt: %q)`,
			sessionID, subject, id.NewShortId(), time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		)
		_, err := eng.Execute(ctx, call)
		require.NoError(t, err, "createAuthSession must persist a row for %s", subject)
		return sessionID
	}

	mineA := writeSession(mine)
	mineB := writeSession(mine)
	theirsA := writeSession(theirs)

	read := func(actor string) map[string]bool {
		t.Helper()
		res, err := eng.Execute(
			auth.ContextWithUserActor(nil, actor),
			`query authSessionsForSelfIncludingRevoked()`,
		)
		require.NoError(t, err)
		require.NotNil(t, res)
		require.NotNil(t, res.Bundle, "the read must return a bundle for Go callers to walk")
		got := map[string]bool{}
		for _, node := range res.Bundle.Nodes {
			if node != nil {
				got[node.Id] = true
			}
		}
		return got
	}

	// THE POSITIVE CONTROL. Without this, "I saw none of theirs" would be
	// satisfied by a query that returns nothing at all -- which is exactly the
	// silent failure mode this whole file exists for.
	got := read(mine)
	require.True(t, got[mineA], "the caller's own session %s must come back", mineA)
	require.True(t, got[mineB], "the caller's own session %s must come back", mineB)

	// THE SECURITY ASSERTION. Somebody else's row is in the table, live, and
	// must not be in this answer.
	require.False(t, got[theirsA],
		"session %s belongs to %s and came back for %s -- the caller scope is not holding (memql#4768)",
		theirsA, theirs, mine)

	// And symmetrically, so a filter accidentally pinned to one id is caught.
	theirGot := read(theirs)
	require.True(t, theirGot[theirsA], "%s must see their own session", theirs)
	require.False(t, theirGot[mineA], "%s must not see %s's session", theirs, mine)
}

// The read is scoped by the AccessContext, not by claims (memql#4768).
//
// This backs a claim the handlers' comments make, rather than leaving it as an
// assertion somebody has to trust. `contextWithSystemActor` in
// component/grpc replaces claims + TokenInfo and does NOT touch the
// AccessContext, and `actor.*` binds from the AccessContext
// (`resolveActorReference` -> `auth.AccessFromContext`).
//
// So a caller-scoped read resolves to the CALLER even under claims that say
// somebody else -- which is why the read/write split in those handlers is
// defence rather than a repair, and why the comment there says so instead of
// claiming the elevated context would have broken it.
//
// Pinning it matters in both directions. If it ever became false, the
// handlers' split would be load-bearing after all and the comment would be
// wrong the other way; and anyone tempted to "simplify" the split needs the
// coupling it rests on written down as something a test noticed.
func TestAuthSessionsForSelf_ScopeComesFromTheAccessContextNotClaims(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	if eng == nil {
		return // skipped: no database
	}

	subject := "v1:identity:user:" + id.NewShortId()
	sessionID := "v1:identity:authSession:" + id.NewShortId()
	writeCtx := auth.ContextWithInternalOrigin(auth.ContextWithUserActor(nil, subject))
	_, err := eng.Execute(writeCtx, fmt.Sprintf(
		`createAuthSession(sessionId: %q, subject: %q, tokenHash: %q, source: "bff_exchange", expiresAt: %q)`,
		sessionID, subject, id.NewShortId(), time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	))
	require.NoError(t, err)

	// The caller's AccessContext, then claims naming somebody else entirely --
	// the shape contextWithSystemActor produces.
	ctx := auth.ContextWithUserActor(nil, subject)
	ctx = auth.ContextWithClaims(ctx, map[string]any{
		"sub":  "polyphon-bridge-agent",
		"role": "system",
	})

	res, err := eng.Execute(ctx, `query authSessionsForSelfIncludingRevoked()`)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.NotNil(t, res.Bundle)

	found := false
	for _, node := range res.Bundle.Nodes {
		if node != nil && node.Id == sessionID {
			found = true
		}
	}
	require.True(t, found,
		"the caller's own session did not come back under claims naming another subject.\n"+
			"If this now fails, actor.* has started resolving from claims rather than from the "+
			"AccessContext -- which makes the read/write split in the revoke handlers "+
			"load-bearing, and their comments (which say it is defence, not a repair) wrong.")
}

// The projection a CLIENT receives carries no credential digests (memql#4768).
//
// # THE BUNDLE AND THE SHAPE ARE DIFFERENT VIEWS, AND ONLY ONE GOES ON THE WIRE
//
// This is the trap, and the first cut of this test fell into it. A shape does
// NOT narrow `ExecuteResult.Bundle`: `applyPlanProjection` projects the bundle
// from the query PLAN, while the shape template produces `output`, and
// `maybeClearBundle` then nils the bundle out of the API response entirely.
//
// So the two views differ on purpose:
//
//   - `result.Bundle` -- what in-process Go walks. Unprojected by the shape,
//     still carries every field. Fine: it never leaves the process.
//   - `result.OutputPayload()` -- what a client receives. Shaped, and the ONLY
//     thing on the wire.
//
// Asserting the bundle here would fail on a correctly-protected query and
// invite somebody to "fix" it by widening the shape. Asserting the shape
// source instead would prove nothing about what is served. So this executes
// the query and reads the output payload, which is exactly what a browser
// gets.
func TestAuthSessionsForSelf_ClientPayloadCarriesNoTokenDigests(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)
	if eng == nil {
		return // skipped: no database
	}

	subject := "v1:identity:user:" + id.NewShortId()
	sessionID := "v1:identity:authSession:" + id.NewShortId()
	writeCtx := auth.ContextWithInternalOrigin(auth.ContextWithUserActor(nil, subject))
	_, err := eng.Execute(writeCtx, fmt.Sprintf(
		`createAuthSession(sessionId: %q, subject: %q, tokenHash: %q, source: "bff_exchange", expiresAt: %q)`,
		sessionID, subject, id.NewShortId(), time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	))
	require.NoError(t, err)

	res, err := eng.Execute(
		auth.ContextWithUserActor(nil, subject),
		`query authSessionsForSelfIncludingRevoked()`,
	)
	require.NoError(t, err)
	require.NotNil(t, res)

	rows := shapedRows(t, res.OutputPayload())
	require.NotEmpty(t, rows,
		"the row just written must come back -- otherwise the assertions below hold vacuously")

	for _, row := range rows {
		for _, banned := range []string{"tokenHash", "refreshTokenHash", "previousRefreshTokenHash"} {
			_, present := row[banned]
			require.False(t, present,
				"the client payload projects %s -- a read reachable from a browser must not "+
					"carry the digests the auth hot path looks rows up by (memql#4768)", banned)
		}
	}

	// And the fields the revoke handlers filter on ARE there. A projection
	// that dropped these would stop the handlers skipping dead rows, which is
	// a correctness bug the "no digests" half would happily pass.
	for _, needed := range []string{"expiresAt", "revokedAt"} {
		_, present := rows[0][needed]
		require.True(t, present,
			"the projection dropped %s, which both revoke handlers filter on", needed)
	}
}

// shapedRows unwraps the shaped output payload into row maps.
func shapedRows(t *testing.T, payload any) []map[string]any {
	t.Helper()
	list, ok := payload.([]any)
	if !ok {
		t.Fatalf("shaped output is %T, want []any -- a shaped query returns row maps", payload)
	}
	out := make([]map[string]any, 0, len(list))
	for _, entry := range list {
		row, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("shaped row is %T, want map[string]any", entry)
		}
		out = append(out, row)
	}
	return out
}
