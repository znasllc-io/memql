package memql

import (
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// cache_ttl_wiring_test.go proves the struct-form `@cache(ttl=N)`
// annotation is wired end-to-end into the per-plan cache hint the engine
// consumes (epic 5, issue 5.5). The annotation is parsed into
// fn.CacheTTL; applyQueryCacheTTL stamps it onto the `concept==X`
// comparison as CacheHintSeconds, which collectCacheHints later reads
// into plan.CacheHints. Before 5.5 nothing populated CacheHintSeconds, so
// `@cache` was parsed-but-dropped on every struct query.

func conceptCmp(concept string) *ComparisonExpression {
	return &ComparisonExpression{
		Field:    FieldReference{Raw: "concept", Parts: []string{"concept"}},
		Operator: OpEq,
		Value:    concept,
	}
}

func TestApplyQueryCacheTTL_StampsConceptHint(t *testing.T) {
	cmp := conceptCmp("v1:safety:catalog")
	if err := applyQueryCacheTTL(cmp, "300"); err != nil {
		t.Fatalf("applyQueryCacheTTL: %v", err)
	}
	if cmp.CacheHintSeconds == nil {
		t.Fatal("CacheHintSeconds not stamped on the concept comparison")
	}
	if *cmp.CacheHintSeconds != 300 {
		t.Fatalf("CacheHintSeconds = %d, want 300", *cmp.CacheHintSeconds)
	}

	// collectCacheHints must surface it keyed by the concept id.
	hints := map[string]int64{}
	collectCacheHints(cmp, hints)
	if got := hints["v1:safety:catalog"]; got != 300 {
		t.Fatalf("collectCacheHints picked up %d, want 300", got)
	}
}

func TestApplyQueryCacheTTL_DescendsDirectives(t *testing.T) {
	// A realistic paginated/sorted/shaped query expression: the concept
	// comparison is buried under the directive wrappers, exactly as the
	// rewriter emits shape(paginate(sort(concept==X & filter))).
	inner := &LogicalExpression{
		Op:   LogicalAnd,
		Left: conceptCmp("v1:cognition:space"),
		Right: &ComparisonExpression{
			Field:    FieldReference{Raw: "payload.ownerUserId", Parts: []string{"payload", "ownerUserId"}},
			Operator: OpEq,
			Value:    "u-1",
		},
	}
	expr := &ShapeExpression{
		Target: &PaginateExpression{
			Target: &SortExpression{Target: inner},
		},
	}
	if err := applyQueryCacheTTL(expr, "30"); err != nil {
		t.Fatalf("applyQueryCacheTTL: %v", err)
	}
	hints := map[string]int64{}
	collectCacheHints(expr, hints)
	if got := hints["v1:cognition:space"]; got != 30 {
		t.Fatalf("hint under directive wrappers = %d, want 30", got)
	}
}

func TestApplyQueryCacheTTL_ZeroIsNeverCache(t *testing.T) {
	cmp := conceptCmp("v1:identity:identity")
	if err := applyQueryCacheTTL(cmp, "0"); err != nil {
		t.Fatalf("applyQueryCacheTTL(0): %v", err)
	}
	if cmp.CacheHintSeconds == nil || *cmp.CacheHintSeconds != 0 {
		t.Fatalf("@cache(ttl=0) must stamp 0 (explicit never-cache), got %v", cmp.CacheHintSeconds)
	}
	// cacheTTLForBundle reads a 0 hint as "don't cache".
	e := &MemQLEngine{}
	ttl := e.cacheTTLForBundle(&memqlv1.GraphBundle{}, map[string]int64{"v1:identity:identity": 0})
	if ttl != 0 {
		t.Fatalf("cacheTTLForBundle with a 0 hint = %v, want 0 (never cache)", ttl)
	}
}

func TestApplyQueryCacheTTL_Errors(t *testing.T) {
	// No concept comparison to attach to.
	noConcept := &ComparisonExpression{
		Field:    FieldReference{Raw: "payload.x", Parts: []string{"payload", "x"}},
		Operator: OpEq,
		Value:    "y",
	}
	if err := applyQueryCacheTTL(noConcept, "30"); err == nil {
		t.Fatal("expected error when no concept== filter is present")
	}
	// Non-numeric / negative TTLs are rejected.
	if err := applyQueryCacheTTL(conceptCmp("v1:x:y"), "soon"); err == nil {
		t.Fatal("expected error on non-numeric ttl")
	}
	if err := applyQueryCacheTTL(conceptCmp("v1:x:y"), "-5"); err == nil {
		t.Fatal("expected error on negative ttl")
	}
}
