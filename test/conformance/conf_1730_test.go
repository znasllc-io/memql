package conformance

// conf_1730_test.go -- regression dimension for memql#1730.
//
// queryUserCount used to be a misnomer: it carried `shape userFull` and
// returned a JSON ARRAY of active-user rows, leaving every caller to take
// len() in Go. #1730 added real engine-level aggregate support (the `count`
// query directive) so the query now computes the cardinality server-side and
// returns a self-describing {count: N} OBJECT envelope.
//
// This dimension pins both halves of the fix:
//   - SHAPE: the result is an object {count: N}, NOT an array of rows (a future
//     regression back to `shape userFull` would make it an array again and fail
//     the type assertion here).
//   - VALUE: N is numeric and tracks the active-user total -- creating K active
//     users bumps the count by exactly K, and (when not paged) it equals the
//     length of the queryActiveUsers row set.

import (
	"testing"
)

// userCount pulls the numeric count out of the {count: N} envelope returned by
// queryUserCount, asserting the shape is an object (not the legacy row array).
func userCount(t *testing.T, e *Env) int {
	t.Helper()
	res := e.runQuery(t, "queryUserCount", map[string]any{})

	if _, isArray := res.([]any); isArray {
		t.Fatalf("#1730 REGRESSION: queryUserCount returned an array of rows -- it must return a {count: N} aggregate object, not the legacy `shape userFull` row set: %#v", res)
	}
	obj, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("#1730: queryUserCount must return a {count: N} object, got %T: %#v", res, res)
	}
	raw, present := obj["count"]
	if !present {
		t.Fatalf("#1730: queryUserCount envelope is missing the `count` field: %#v", obj)
	}
	n, ok := raw.(float64) // JSON numbers decode to float64
	if !ok {
		t.Fatalf("#1730: `count` must be numeric, got %T: %#v", raw, raw)
	}
	return int(n)
}

func TestConf1730UserCountReturnsNumericAggregate(t *testing.T) {
	e := newEnv(t)
	if !e.HasDB {
		t.Skip("requires Postgres (set MEMORY_NODES_DATABASE_DSN); validated in the CI conformance job")
	}

	// Baseline: the count is an object {count: N}, and N is non-negative.
	before := userCount(t, e)
	if before < 0 {
		t.Fatalf("#1730: count must be non-negative, got %d", before)
	}

	// Cross-check against the active-user row set when it is not paged: the
	// aggregate must equal len(queryActiveUsers). (queryActiveUsers pages at
	// MaxResults=500; only assert equality below that ceiling so the check
	// stays correct on a large shared DB.)
	activeRows := asArray(t, e.runQuery(t, "queryActiveUsers", map[string]any{}))
	if len(activeRows) < 500 && before != len(activeRows) {
		t.Fatalf("#1730: queryUserCount=%d must match the active-user total len(queryActiveUsers)=%d", before, len(activeRows))
	}

	// Create K active users; the count must rise by exactly K.
	const k = 3
	sfx := uniqueSuffix("1730")
	for i := 0; i < k; i++ {
		uid := uniqueUserID(sfx, i)
		e.runMutation(t, "mutationCreateUser", map[string]any{
			"userId":       uid,
			"displayName":  "Conf 1730 " + uid,
			"primaryEmail": uid + "@conf-1730.test",
			"role":         "reader",
		})
	}

	after := userCount(t, e)
	if after != before+k {
		t.Fatalf("#1730: creating %d active users must bump the count from %d to %d; got %d", k, before, before+k, after)
	}

	t.Logf("#1730: queryUserCount returns {count: N} -- %d -> %d after creating %d active users", before, after, k)
}

func uniqueUserID(sfx string, i int) string {
	return "user-1730-" + sfx + "-" + string(rune('a'+i))
}
