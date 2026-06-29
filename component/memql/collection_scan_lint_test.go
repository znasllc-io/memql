package memql

import (
	"io"
	"log/slog"
	"testing"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// Story 6 (#2304 / ADR §2.2): the in-memory-vs-SQL guardrail lint warns when a
// collection chain's base receiver is an unfiltered full-concept query read,
// and stays quiet on `args.X` (caller list) and already-filtered queries.

// unfilteredUsersQuery is a query whose compiled body is a bare full-concept
// scan: `concept == v1:identity:user` and nothing else.
func unfilteredUsersQuery() *Function {
	return &Function{
		Name:         "allUsers",
		FunctionKind: "query",
		Expr: &ComparisonExpression{
			Field:    FieldReference{Raw: "concept", Parts: []string{"concept"}},
			Operator: OpEq,
			Value:    "v1:identity:user",
		},
	}
}

// filteredUsersQuery is a query already constrained by a non-concept predicate
// AND'd with the concept binding -- the shape ensureBoundConceptFilter produces.
func filteredUsersQuery() *Function {
	return &Function{
		Name:         "activeUsers",
		FunctionKind: "query",
		Expr: &LogicalExpression{
			Op: LogicalAnd,
			Left: &ComparisonExpression{
				Field:    FieldReference{Raw: "active", Parts: []string{"active"}},
				Operator: OpEq,
				Value:    true,
			},
			Right: &ComparisonExpression{
				Field:    FieldReference{Raw: "concept", Parts: []string{"concept"}},
				Operator: OpEq,
				Value:    "v1:identity:user",
			},
		},
	}
}

// chain builds `<receiver>.where(u => u.active).count()`.
func chain(receiver ExpressionNode) ExpressionNode {
	where := &CollectionMethodExpression{
		Receiver: receiver,
		Method:   "where",
		Args: []ExpressionNode{
			&LambdaExpression{
				Params: []string{"u"},
				Body: &ComparisonExpression{
					Field:    FieldReference{Raw: "u.active", Parts: []string{"u", "active"}},
					Operator: OpEq,
					Value:    true,
				},
			},
		},
	}
	return &CollectionMethodExpression{Receiver: where, Method: "count"}
}

// TestInMemoryScanWarnsOnUnfilteredQuery: a chain over an unfiltered
// full-concept query read produces exactly one finding naming the query +
// concept + chain-head method.
func TestInMemoryScanWarnsOnUnfilteredQuery(t *testing.T) {
	functions := map[string]*Function{
		"allUsers": unfilteredUsersQuery(),
		"logicCountActive": {
			Name:         "logicCountActive",
			FunctionKind: "logic",
			Expr:         chain(&FunctionCallExpression{Name: "allUsers", Args: map[string]any{}}),
		},
	}

	findings := InMemoryCollectionScanFindings(functions)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(findings), findings)
	}
	f := findings[0]
	if f.Logic != "logicCountActive" || f.Query != "allUsers" {
		t.Fatalf("unexpected finding target: %+v", f)
	}
	if f.Concept != "v1:identity:user" {
		t.Fatalf("expected concept v1:identity:user, got %q", f.Concept)
	}
	if f.Method != "count" {
		t.Fatalf("expected chain-head method count, got %q", f.Method)
	}
}

// TestInMemoryScanNoWarnOnArgs: a chain over `args.X` (a caller-supplied
// in-memory list) is legitimate -- no finding.
func TestInMemoryScanNoWarnOnArgs(t *testing.T) {
	functions := map[string]*Function{
		"logicCountMembers": {
			Name:         "logicCountMembers",
			FunctionKind: "logic",
			Expr:         chain(&ArgRefExpression{Path: "members"}),
		},
	}

	if findings := InMemoryCollectionScanFindings(functions); len(findings) != 0 {
		t.Fatalf("expected no findings for args.X receiver, got %d: %+v", len(findings), findings)
	}
}

// TestInMemoryScanNoWarnOnFilteredQuery: a chain over an already-filtered query
// (a non-concept predicate AND'd with the concept binding) is not a full-concept
// scan -- no finding.
func TestInMemoryScanNoWarnOnFilteredQuery(t *testing.T) {
	functions := map[string]*Function{
		"activeUsers": filteredUsersQuery(),
		"logicCountActive": {
			Name:         "logicCountActive",
			FunctionKind: "logic",
			Expr:         chain(&FunctionCallExpression{Name: "activeUsers", Args: map[string]any{}}),
		},
	}

	if findings := InMemoryCollectionScanFindings(functions); len(findings) != 0 {
		t.Fatalf("expected no findings for filtered query receiver, got %d: %+v", len(findings), findings)
	}
}

// TestInMemoryScanNoWarnOnShapedUnfilteredIsStillScan: a directive wrapper
// (shape) around a bare concept scan is STILL a full scan -- the wrapper shapes
// the projection but does not constrain rows. Confirms the peel logic warns.
func TestInMemoryScanWarnsThroughDirectiveWrapper(t *testing.T) {
	q := unfilteredUsersQuery()
	q.Name = "allUsersShaped"
	q.Expr = &ShapeExpression{Target: q.Expr, TemplateName: "userCard"}
	functions := map[string]*Function{
		"allUsersShaped": q,
		"logicAny": {
			Name:         "logicAny",
			FunctionKind: "logic",
			Expr:         chain(&FunctionCallExpression{Name: "allUsersShaped", Args: map[string]any{}}),
		},
	}
	if findings := InMemoryCollectionScanFindings(functions); len(findings) != 1 {
		t.Fatalf("expected 1 finding through a shape wrapper, got %d: %+v", len(findings), findings)
	}
}

// TestInMemoryScanOverFullDSL runs the lint over the full shipped DSL tree
// (same load path as TestEngineInitLoadsFullDSL) and reports any shipped logic
// that trips the warning. Informative: findings are logged, never failed --
// the lint is a warning and must not break load.
func TestInMemoryScanOverFullDSL(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := concept.DefaultRegistry()
	if registry == nil || len(registry.List()) == 0 {
		t.Fatal("concept registry is empty after LoadUnifiedConcepts")
	}
	eng, err := New(nil)
	if err != nil {
		t.Fatalf("construct engine: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(registry); err != nil {
		t.Fatalf("engine.Init over full DSL tree: %v", err)
	}
	findings := InMemoryCollectionScanFindings(eng.Functions().Snapshot())
	for _, f := range findings {
		t.Logf("WARNING: %s", f.Message())
	}
	t.Logf("collection-scan lint findings over shipped DSL: %d", len(findings))
}
