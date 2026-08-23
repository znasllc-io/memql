package memql

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
)

// Epic memql#4358, end to end against a real database. Every DB-free test
// around this feature drives a FAKE stream and parses no SQL; the fleet
// call-site gate parses the queries but executes none. What only a database
// can show is that the row survives the executor write path and comes back
// through the DSL reads the portal and the planner actually call.
//
// Green-by-skip warning: this file skips wherever no Postgres is reachable,
// so a plain `go test ./...` proves nothing here. To verify for real:
//
//	MEMQL_DATABASE_DSN=... MEMQL_REQUIRE_DB=1 go test -count=1 \
//	  -run TestAppSession ./component/memql/

// appSessionOwnerCtx builds a caller context for a plain (non-owner) user.
// The three appSession mutations are @serverOnly, so the write also needs
// INTERNAL origin -- without it the engine refuses and the only evidence is
// a WARN, which is the exact failure component/worker's own test guards.
func appSessionOwnerCtx(userId string) context.Context {
	return auth.ContextWithInternalOrigin(auth.ContextWithUserActor(context.Background(), userId))
}

func TestAppSessionRowPersistsAndReadsBack(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)

	owner := fmt.Sprintf("u-appsession-%s", uniqueSuffix("4360"))
	ctx := appSessionOwnerCtx(owner)
	sessionID := fmt.Sprintf("s-%s", uniqueSuffix("appsession4360"))

	const conceptName = "v1:worker:appSession"

	// createAppSession deliberately passes NO ownerUserId: the concept marks
	// it @serverSet, so the mutation stamps it from the actor. Passing it
	// would be refused at load -- that absence is part of the contract.
	storedID := runMutation(t, ctx, eng, "createAppSession", map[string]any{
		"sessionId": sessionID,
		"workerId":  "reg-4360",
		"app":       "claude-code",
		"kind":      "run",
		"planId":    "plan-4360",
		"taskId":    "task-4360",
		"workspace": "/w/4360",
		"prompt":    "do the thing",
		"startedAt": "2026-08-22T09:00:00Z",
	})

	p := latestPayload(t, ctx, db, conceptName, storedID)
	// CANONICAL form, not the bare id: the engine canonicalises actor.userId
	// on the way in (docs/public/concepts/identifiers.md). Asserting the bare
	// value would fail on correct behaviour; asserting only "non-empty" would
	// pass on a caller-supplied one. The canonical id is what proves the
	// stamp ran.
	require.Equal(t, canonicalUserId(owner), p["ownerUserId"],
		"ownerUserId must be STAMPED from the actor -- @rowAuthz(owner=...) is worthless if it is not")
	require.Equal(t, "claude-code", p["app"])
	require.Equal(t, "starting", p["status"], "a fresh session starts in 'starting'")
	require.Equal(t, "unknown", p["billing"], "billing is unknown until the app reports usage")

	// The transcript flush, which the runner calls on a 2s cadence.
	runMutation(t, ctx, eng, "appendAppSessionTranscript", map[string]any{
		"sessionId": sessionID, "transcript": "line one\n", "transcriptBytes": 9, "status": "running",
	})
	p = latestPayload(t, ctx, db, conceptName, storedID)
	require.Equal(t, "line one\n", p["transcript"])
	require.Equal(t, "running", p["status"])

	// The terminal write, carrying the app's REPORTED usage verbatim.
	runMutation(t, ctx, eng, "endAppSession", map[string]any{
		"sessionId": sessionID,
		"status":    "ended",
		"exitCode":  0,
		"usage": map[string]any{
			"inputTokens": 900, "outputTokens": 350, "costUSD": 0.22, "known": true,
		},
		"billing":             "subscription",
		"transcript":          "line one\nline two\n",
		"transcriptBytes":     19,
		"producedArtifactIds": []string{"artifact-a"},
		"appSessionRef":       "cc-1",
		"endedAt":             "2026-08-22T10:00:00Z",
	})
	p = latestPayload(t, ctx, db, conceptName, storedID)
	require.Equal(t, "ended", p["status"])
	require.Equal(t, "subscription", p["billing"])
	usage, ok := p["usage"].(map[string]any)
	require.True(t, ok, "usage must round-trip as an object, got %T", p["usage"])
	require.Equal(t, float64(900), usage["inputTokens"])
	require.Equal(t, true, usage["known"])

	// And the reads the portal and the planner call actually find it.
	got := queryIds(t, ctx, eng, fmt.Sprintf("appSessionById(sessionId:%q)", sessionID))
	require.True(t, contains(got, storedID),
		"appSessionById(%q) must return the row just written; got %v", sessionID, got)

	got = queryIds(t, ctx, eng, fmt.Sprintf("appSessionsForTask(taskId:%q)", "task-4360"))
	require.True(t, contains(got, storedID), "appSessionsForTask must find the session; got %v", got)

	// A TERMINAL session must NOT appear in the concurrency-cap read --
	// counting ended rows would make the cap tighten permanently as history
	// accumulated, which is the one way this limit could silently break.
	got = queryIds(t, ctx, eng, "liveAppSessionsForUser()")
	require.False(t, contains(got, storedID),
		"liveAppSessionsForUser must exclude an ENDED session; got %v", got)
}

// TestAppSessionReadsAreCallerScoped: the queries filter on actor.userId, so
// another user's session must be invisible even by exact id. A leak here
// would expose a transcript of somebody's machine.
func TestAppSessionReadsAreCallerScoped(t *testing.T) {
	eng, _, _ := sharedReadMergeEngine(t)

	ownerA := fmt.Sprintf("u-a-%s", uniqueSuffix("4360scope"))
	ownerB := fmt.Sprintf("u-b-%s", uniqueSuffix("4360scope"))
	sessionID := fmt.Sprintf("s-%s", uniqueSuffix("4360scope"))

	storedID := runMutation(t, appSessionOwnerCtx(ownerA), eng, "createAppSession", map[string]any{
		"sessionId": sessionID, "workerId": "reg-x", "app": "codex", "kind": "run",
		"taskId": "task-scope", "startedAt": "2026-08-22T09:00:00Z",
	})

	// A's own read finds it.
	got := queryIds(t, appSessionOwnerCtx(ownerA), eng, fmt.Sprintf("appSessionById(sessionId:%q)", sessionID))
	require.True(t, contains(got, storedID), "the owner must be able to read their own session")

	// B's does not, even naming the id exactly.
	got = queryIds(t, appSessionOwnerCtx(ownerB), eng, fmt.Sprintf("appSessionById(sessionId:%q)", sessionID))
	require.False(t, contains(got, storedID),
		"another user read %q -- app-session rows carry the transcript of somebody's machine", sessionID)
}

// TestDelegationPolicyStampsItsOwner: setDelegationPolicy is NOT @serverOnly
// (the portal calls it), so the owner stamp is the only thing stopping a
// caller authoring a policy attributed to somebody else.
func TestDelegationPolicyStampsItsOwner(t *testing.T) {
	eng, db, _ := sharedReadMergeEngine(t)

	owner := fmt.Sprintf("u-policy-%s", uniqueSuffix("4362"))
	ctx := auth.ContextWithUserActor(context.Background(), owner)
	policyID := fmt.Sprintf("p-%s", uniqueSuffix("4362"))

	storedID := runMutation(t, ctx, eng, "setDelegationPolicy", map[string]any{
		"policyId":               policyID,
		"preferSubscriptionApps": true,
		"eligibleKinds":          []string{"runCommand"},
		"appOrder":               []string{"claude-code"},
		"updatedAt":              "2026-08-22T09:00:00Z",
	})

	p := latestPayload(t, ctx, db, "v1:worker:delegationPolicy", storedID)
	require.Equal(t, canonicalUserId(owner), p["ownerUserId"],
		"ownerUserId must be stamped from the actor, never accepted from args")
	require.Equal(t, true, p["preferSubscriptionApps"])

	got := queryIds(t, ctx, eng, "delegationPolicyForUser()")
	require.True(t, contains(got, storedID), "the planner's triage read must find the policy; got %v", got)
}

// canonicalUserId is the form the engine stamps actor.userId as. Written out
// rather than read from a helper so the test states the contract it is
// asserting: a bare id in this column would mean the stamp did not run.
func canonicalUserId(bare string) string {
	return "v1:identity:user:" + bare
}
