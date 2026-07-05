package memql

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// mcp_read_path_rootfix_db_test.go is the real-engine reproduction + fix guard
// for the read-path half of epic memql#1672 (run-3 MCP QA failures):
//
//   #1674  the `has` array-membership operator (the desugared form of
//          `<scalar> in payload.<arrayField>`) failed in the in-memory
//          comparison path -> notesByTag / notesSearch / skillNeedsRefresh
//          returned nothing when tagged/array data existed.
//   #1675  querySavedSpaces / queryArchivedSpaces gated on the active==true
//          trait, so saved/archived rows (active=false) never came back.
//   #1685  invocationsForPlan / expiredWorkerInvocations omitted the
//          not-deleted trait, so soft-deleted invocations leaked through.
//   #1682  searchUsers ignored its active/limit filter args (returned all users).
//
// Postgres-gated: skips when no DB is reachable, reusing readMergeTestEngine.

// queryIds runs a query and returns the ids of the returned rows.
func queryIds(t *testing.T, ctx context.Context, eng *MemQLEngine, q string) []string {
	t.Helper()
	res, err := eng.Execute(ctx, q)
	require.NoError(t, err, "query must not error: %s", q)
	require.NotNil(t, res)
	ids := []string{}
	if res.Bundle != nil {
		for _, n := range res.Bundle.Nodes {
			ids = append(ids, n.GetId())
		}
	}
	return ids
}

func contains(ids []string, want string) bool {
	for _, id := range ids {
		if id == want || strings.HasSuffix(id, want) {
			return true
		}
	}
	return false
}

// --- #1674: `has` array-membership end-to-end -------------------------------

func TestHasOperator_NotesByTag(t *testing.T) {
	eng, _, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-has-1674")
	sfx := uniqueSuffix("note")

	alpha := fmt.Sprintf("v1:notes:note:alpha-%s", sfx)
	beta := fmt.Sprintf("v1:notes:note:beta-%s", sfx)
	runMutation(t, ctx, eng, "createNote", map[string]any{
		"noteId": alpha, "body": "alpha body", "tags": []string{"work", "urgent"},
	})
	runMutation(t, ctx, eng, "createNote", map[string]any{
		"noteId": beta, "body": "beta body", "tags": []string{"personal"},
	})

	// The headline #1674 assertion: a tag-membership query returns the
	// matching row (before the fix this errored / returned nothing).
	work := queryIds(t, ctx, eng, `notesByTag(tag:"urgent")`)
	require.True(t, contains(work, alpha), "note tagged 'urgent' must be returned (#1674), got %v", work)
	require.False(t, contains(work, beta), "note NOT tagged 'urgent' must be excluded, got %v", work)

	personal := queryIds(t, ctx, eng, `notesByTag(tag:"personal")`)
	require.True(t, contains(personal, beta), "note tagged 'personal' must be returned, got %v", personal)
	require.False(t, contains(personal, alpha), "alpha must be excluded from 'personal', got %v", personal)
}

// NOTE (#2342): TestSpaceLists_SavedAndArchivedReturned lived here and covered
// #1675 (saved/archived space lists gate on status + not-deleted, NOT
// active==true). The entire space lifecycle -- the `space` concept, the
// save/archive mutations, and the saved/archived queries -- moved OUT of the
// engine core into the product pack (the carrier repo's DSL tree) in
// #2038, so this engine-repo test referenced constructs that no longer load
// here and could never pass. The behavioural coverage belongs in the pack
// repo; it is re-homed there (follow-up: memql#2344). Removed rather
// than rewritten because no engine-core concept carries the same
// saved/archived status-gated list shape to retarget it against.

// --- #1685: soft-deleted worker invocations excluded ------------------------

func TestWorkerInvocations_SoftDeletedExcluded(t *testing.T) {
	eng, _, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-wi-1685")
	sfx := uniqueSuffix("wi")

	planID := fmt.Sprintf("v1:planner:plan:p-%s", sfx)
	liveID := fmt.Sprintf("v1:worker:invocation:live-%s", sfx)
	deadID := fmt.Sprintf("v1:worker:invocation:dead-%s", sfx)

	base := func(id string) map[string]any {
		return map[string]any{
			"invocationId": id, "ownerUserId": "u-wi-1685", "workerId": "w-1",
			"agentId": "a-1", "planId": planID, "tool": "workerHost",
			"action": "exec", "startedAt": "2026-01-01T00:00:00Z", "outcome": "success",
		}
	}
	runMutation(t, ctx, eng, "createWorkerInvocation", base(liveID))
	runMutation(t, ctx, eng, "createWorkerInvocation", base(deadID))

	// Soft-delete one of them.
	runMutation(t, ctx, eng, "softDeleteWorkerInvocation", map[string]any{"invocationId": deadID})

	forPlan := queryIds(t, ctx, eng, fmt.Sprintf(`invocationsForPlan(planId:%q)`, planID))
	require.True(t, contains(forPlan, liveID), "live invocation must be returned, got %v", forPlan)
	require.False(t, contains(forPlan, deadID), "soft-deleted invocation must be excluded from invocationsForPlan (#1685), got %v", forPlan)

	expired := queryIds(t, ctx, eng, "expiredWorkerInvocations()")
	require.False(t, contains(expired, deadID), "soft-deleted invocation must be excluded from expiredWorkerInvocations (#1685), got %v", expired)
}

// --- #1682: searchUsers honors active + limit -------------------------------

func TestSearchUsers_ActiveAndLimitApplied(t *testing.T) {
	eng, _, _ := readMergeTestEngine(t)
	ctx := clusterOwnerCtx("u-su-1682")
	ctx = WithActingAgentRole(ctx, "owner")
	ctx = WithActingAgentId(ctx, "v1:agents:agent:su-1682")
	sfx := uniqueSuffix("su")

	// Two active + one deactivated user.
	activeA := fmt.Sprintf("v1:identity:user:su-active-a-%s", sfx)
	activeB := fmt.Sprintf("v1:identity:user:su-active-b-%s", sfx)
	inactive := fmt.Sprintf("v1:identity:user:su-inactive-%s", sfx)
	for i, id := range []string{activeA, activeB, inactive} {
		runMutation(t, ctx, eng, "createUser", map[string]any{
			"userId": id, "displayName": fmt.Sprintf("SU %d", i),
			"primaryEmail": fmt.Sprintf("su-%d-%s@example.com", i, sfx),
		})
	}
	// Deactivate the third via partial read-merge update.
	runMutation(t, ctx, eng, "updateUser", map[string]any{
		"userId": inactive, "payload": map[string]any{"active": false},
	})

	callTool := func(args map[string]any) string {
		out, err := eng.ExecuteToolByName(ctx, "searchUsers", args)
		require.NoError(t, err, "searchUsers tool must not error for args %v", args)
		require.NotContains(t, out, `\"isError\":true`)
		return out
	}

	// active=true must include the active users and exclude the deactivated one.
	activeOut := callTool(map[string]any{"active": true, "limit": float64(50)})
	require.Contains(t, activeOut, activeA, "active=true must include active user A (#1682)")
	require.Contains(t, activeOut, activeB, "active=true must include active user B (#1682)")
	require.NotContains(t, activeOut, inactive, "active=true must exclude the deactivated user (#1682)")

	// active=false must include the deactivated user and exclude the active ones.
	inactiveOut := callTool(map[string]any{"active": false, "limit": float64(50)})
	require.Contains(t, inactiveOut, inactive, "active=false must include the deactivated user (#1682)")
	require.NotContains(t, inactiveOut, activeA, "active=false must exclude active user A (#1682)")

	// limit=N caps the result count. Seed a few more active users so the cap bites.
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("v1:identity:user:su-bulk-%s-%d", sfx, i)
		runMutation(t, ctx, eng, "createUser", map[string]any{
			"userId": id, "displayName": fmt.Sprintf("Bulk %d", i),
			"primaryEmail": fmt.Sprintf("bulk-%d-%s@example.com", i, sfx),
		})
	}
	cappedOut := callTool(map[string]any{"limit": float64(2)})
	var capped ToolCallResult
	require.NoError(t, json.Unmarshal([]byte(cappedOut), &capped))
	require.False(t, capped.IsError)
	require.Len(t, capped.Content, 1)
	// The query result JSON carries one "id" per returned row.
	require.Equal(t, 2, strings.Count(capped.Content[0].Text, `"id"`),
		"limit=2 must cap the result count to 2 (#1682), body: %s", capped.Content[0].Text)

	// Robustness: an OMITTED limit must NOT crash -- the schema
	// @default("10") is materialized server-side (applyToolDefaults),
	// so the handler's paginate() sees a real integer literal instead
	// of a null. Both no-args and active-only calls must succeed and
	// cap at 10.
	for _, args := range []map[string]any{{}, {"active": true}} {
		out := callTool(args)
		var r ToolCallResult
		require.NoError(t, json.Unmarshal([]byte(out), &r))
		require.False(t, r.IsError, "omitted-limit call %v must not error (#1682)", args)
		require.Len(t, r.Content, 1)
		require.LessOrEqual(t, strings.Count(r.Content[0].Text, `"id"`), 10,
			"omitted limit must default-cap at 10 (#1682), args %v", args)
	}
}

// TestToolSchemaDefaults_Materialized is the focused unit guard for the
// schema-default materialization (#1682): a declared @default must be
// filled (coerced to its declared type) when the caller omits the field,
// must not override an explicit caller value, and must never touch an
// auto-injected field.
func TestToolSchemaDefaults_Materialized(t *testing.T) {
	tool := &Tool{
		Name:        "t",
		InputSchema: json.RawMessage(`{"properties":{"limit":{"type":"integer","default":"10"},"flag":{"type":"boolean","default":"false"},"name":{"type":"string"}}}`),
	}

	// Omitted -> filled with the coerced default.
	got := applyToolDefaults(context.Background(), tool, map[string]any{}, nil)
	require.Equal(t, int64(10), got["limit"], "integer default must coerce to int64")
	require.Equal(t, false, got["flag"], "boolean default must coerce to bool")
	_, hasName := got["name"]
	require.False(t, hasName, "field without a default must stay absent")

	// Explicit caller value wins over the schema default.
	got2 := applyToolDefaults(context.Background(), tool, map[string]any{"limit": float64(2)}, nil)
	require.Equal(t, float64(2), got2["limit"], "caller value must win over schema default")
}
