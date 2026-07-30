package conformance

// conf_1730_test.go -- regression dimension for memql#1730.
//
// userCount used to be a misnomer: it carried `shape userFull` and
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
//     length of the activeUsers row set.

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

// userCount pulls the numeric count out of the {count: N} envelope returned by
// userCount, asserting the shape is an object (not the legacy row array).
func userCount(t *testing.T, e *Env) int {
	t.Helper()
	res := e.runQuery(t, "userCount", map[string]any{})

	if _, isArray := res.([]any); isArray {
		t.Fatalf("#1730 REGRESSION: userCount returned an array of rows -- it must return a {count: N} aggregate object, not the legacy `shape userFull` row set: %#v", res)
	}
	obj, ok := res.(map[string]any)
	if !ok {
		t.Fatalf("#1730: userCount must return a {count: N} object, got %T: %#v", res, res)
	}
	raw, present := obj["count"]
	if !present {
		t.Fatalf("#1730: userCount envelope is missing the `count` field: %#v", obj)
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
		t.Skip("requires Postgres (set MEMQL_DATABASE_DSN); validated in the CI conformance job")
	}

	// Baseline: the count is an object {count: N}, and N is non-negative.
	before := userCount(t, e)
	if before < 0 {
		t.Fatalf("#1730: count must be non-negative, got %d", before)
	}

	// Cross-check against the active-user row set.
	//
	// activeUsers became @serverOnly in memql#2883 -- both its args are
	// optional, so a bare client call returned every active user in userFull
	// (every @pii field plus the cluster-wide auth role). It is read
	// server-side here because the property under test is the ROW COUNT, not
	// reachability; the refusal itself is asserted immediately below.
	activeRowCount := e.runQueryServerSide(t, "query activeUsers()")

	// The page ceiling is DERIVED FROM THE LOADED QUERY, not hardcoded.
	//
	// This guard used to compare against a literal 500 "so the check stays
	// correct on a large shared DB". activeUsers pages at 50
	// (dsl/identity/queries.memql), so on any database with more than 50
	// active users it returned exactly one full page -- comfortably under
	// 500, so the guard never fired and the assertion compared a population
	// against a page size (memql#2929).
	//
	// The failure only appeared on a developer's accumulated local DB, never
	// on a fresh CI database. That inverts the guard's stated intent: it was
	// added to survive a large shared DB and instead only worked on a small
	// one.
	//
	// Reading the limit off the loaded query means the two cannot drift.
	// Re-hardcoding it as 50 would reproduce the same trap with a different
	// number, and restating activeUsers' filter here to count the unpaged
	// population would duplicate the very thing that makes this check
	// meaningful -- that both sides share one filter.
	pageLimit, ok := paginateLimitOf(e, "activeUsers")
	if !ok {
		t.Fatal("#2929: could not read activeUsers' paginate limit from the loaded query. " +
			"This cross-check is only valid below that ceiling, so failing here rather than " +
			"guessing a constant -- if the paginate directive was removed, assert equality " +
			"unconditionally instead.")
	}

	// Always true, at any database size: the aggregate counts the whole
	// population, the row set is capped at one page, so the count can never
	// be smaller. This half catches an undercounting aggregate on a large DB,
	// where the equality below is unavailable.
	if before < activeRowCount {
		t.Fatalf("#1730: userCount=%d is LESS than len(activeUsers)=%d -- the aggregate counts "+
			"the full population and the row set is capped at one page of %d, so the count can "+
			"never be the smaller of the two", before, activeRowCount, pageLimit)
	}

	// Below the ceiling the row set is complete, so the two must agree
	// exactly. Above it, activeUsers has saturated and there is nothing to
	// compare -- the count/K-delta assertion further down is what pins the
	// value on a large database.
	if activeRowCount < pageLimit && before != activeRowCount {
		t.Fatalf("#1730: userCount=%d must match the active-user total len(activeUsers)=%d "+
			"(row set is below the page ceiling of %d, so it is complete)",
			before, activeRowCount, pageLimit)
	}
	if activeRowCount >= pageLimit {
		t.Logf("#2929: activeUsers saturated its page of %d (population=%d), so the equality "+
			"cross-check is skipped; the +K delta below still pins the aggregate.",
			pageLimit, before)
	}

	// memql#2883, the conformance half: run_query IS the MCP client surface,
	// so a @serverOnly construct reaching it would be exactly the leak the
	// gate exists to close.
	if out, isErr := e.toolCall(t, "run_query", map[string]any{"name": "activeUsers", "args": map[string]any{}}); !isErr {
		t.Fatalf("#2883: run_query activeUsers must be REFUSED on the MCP client surface, got: %v", out)
	} else if !strings.Contains(fmt.Sprint(out), "server-only") {
		t.Fatalf("#2883: run_query activeUsers was refused for the wrong reason: %v", out)
	}

	// Create K active users; the count must rise by exactly K.
	const k = 3
	sfx := uniqueSuffix("1730")
	for i := 0; i < k; i++ {
		uid := uniqueUserID(sfx, i)
		e.runMutation(t, "createUser", map[string]any{
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

	t.Logf("#1730: userCount returns {count: N} -- %d -> %d after creating %d active users", before, after, k)
}

func uniqueUserID(sfx string, i int) string {
	return "user-1730-" + sfx + "-" + string(rune('a'+i))
}

// paginateLimitOf reads a loaded query's `paginate N` ceiling off the
// registered expression tree.
//
// Derived rather than hardcoded (memql#2929). A literal in a test that must
// track a literal in the DSL is a trap with a delay fuse: this one sat wrong
// for long enough that the suite failed on every developer machine with an
// accumulated database while CI stayed green.
//
// The pipeline is shape(paginate(sort(<filter>), N), "..."), so the walk peels
// single-target wrappers until it finds the paginate node. Returns false when
// the query is unpaged or not registered -- the caller decides what that means
// rather than getting a silent zero.
func paginateLimitOf(e *Env, name string) (int, bool) {
	fn, err := e.Eng.Functions().Get(name)
	if err != nil || fn == nil {
		return 0, false
	}
	expr := fn.Expr
	for range 16 { // bounded: the wrapper chain is short and this must not spin
		switch n := expr.(type) {
		case *memql.PaginateExpression:
			if n.Limit == nil {
				return 0, false
			}
			return *n.Limit, true
		case *memql.ShapeExpression:
			expr = n.Target
		case *memql.SortExpression:
			expr = n.Target
		case *memql.SelectExpression:
			expr = n.Target
		case *memql.TimestampExpression:
			expr = n.Target
		case *memql.DepthExpression:
			expr = n.Target
		case *memql.CountExpression:
			expr = n.Target
		default:
			return 0, false
		}
	}
	return 0, false
}

// TestConf2929PaginateLimitIsDerivedNotHardcoded pins the mechanism that
// fixed memql#2929.
//
// The bug was a literal in a test that had to track a literal in the DSL:
// the guard said 500, activeUsers paged at 50, so on any database with more
// than 50 active users the guard never fired and the assertion compared a
// population against a page size. It failed only on a developer's
// accumulated local DB and never on fresh CI -- the exact inverse of the
// "stays correct on a large shared DB" intent it was written for.
//
// Re-hardcoding 50 would have reproduced the trap with a different number.
// This asserts the ceiling is read from the loaded query, so retuning
// `paginate` cannot silently re-break the cross-check.
func TestConf2929PaginateLimitIsDerivedNotHardcoded(t *testing.T) {
	e := newEnv(t)

	got, ok := paginateLimitOf(e, "activeUsers")
	if !ok {
		t.Fatal("could not read activeUsers' paginate limit from the loaded query -- the " +
			"cross-check in TestConf1730 depends on this, and it fails loudly there rather " +
			"than falling back to a constant")
	}
	if got <= 0 {
		t.Fatalf("paginate limit = %d, want a positive page size", got)
	}

	// The derivation must track the DECLARATION. If this ever disagrees with
	// `paginate N` in dsl/identity/queries.memql, the walk is reading the
	// wrong node and #2929's guard is silently wrong again.
	declared := declaredPaginate(t, "activeUsers")
	if got != declared {
		t.Fatalf("derived paginate limit %d != the declared `paginate %d` in "+
			"dsl/identity/queries.memql -- the expression walk is reading the wrong node",
			got, declared)
	}

	// An unpaged or unknown query must report false, not a silent zero: a
	// zero ceiling would make every row set look saturated and quietly
	// disable the equality assertion.
	if _, ok := paginateLimitOf(e, "definitelyNotAQueryName"); ok {
		t.Fatal("paginateLimitOf reported a limit for a query that does not exist")
	}
}

// declaredPaginate reads `paginate N` for a named query straight out of the
// DSL source, so the test compares the loaded value against the authored one
// rather than against another copy of the same derivation.
func declaredPaginate(t *testing.T, name string) int {
	t.Helper()
	data, err := os.ReadFile("../../dsl/identity/queries.memql")
	if err != nil {
		t.Fatalf("read queries.memql: %v", err)
	}
	src := string(data)
	head := regexp.MustCompile(`(?m)^query[ \t]+\w+[ \t]+` + regexp.QuoteMeta(name) + `[ \t]*\{`)
	loc := head.FindStringIndex(src)
	if loc == nil {
		t.Fatalf("no declaration for query %q", name)
	}
	body := src[loc[1]:]
	if end := strings.Index(body, "\n}"); end >= 0 {
		body = body[:end]
	}
	m := regexp.MustCompile(`(?m)^[ \t]*paginate[ \t]+(\d+)`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("query %q declares no paginate directive", name)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("paginate value %q: %v", m[1], err)
	}
	return n
}
