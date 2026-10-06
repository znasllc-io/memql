package memql

import "testing"

type unknownReplayExpression struct{}

func (*unknownReplayExpression) isExpressionNode() {}

func TestReplayReadOnlyFollowsRegisteredCallsAndSpecifications(t *testing.T) {
	fns, specs := newFunctionRegistry(), newSpecRegistry()
	for _, fn := range []*Function{
		{Name: "read", FunctionKind: "query", Expr: &ComparisonExpression{Value: "x"}},
		{Name: "write", FunctionKind: "mutation"},
		{Name: "knownRead", Type: FunctionTypeBuiltin, Executor: "integration.database.stats"},
		{Name: "knownWrite", Type: FunctionTypeBuiltin, Executor: "integration.storage.upload"},
		{Name: "unknown", Type: FunctionTypeBuiltin, Executor: "integration.new.publish"},
		{Name: "metadata", Type: FunctionTypeBuiltin, Executor: BuiltinExecutorConcepts},
		{Name: "logic", FunctionKind: "logic"},
		{Name: "callsRead", FunctionKind: "query", Expr: &NotExpression{Target: &FunctionCallExpression{Name: "read"}}},
		{Name: "callsWrite", FunctionKind: "query", Expr: &NotExpression{Target: &FunctionCallExpression{Name: "write"}}},
		{Name: "cycle", FunctionKind: "query", Expr: &FunctionCallExpression{Name: "cycle"}},
		{Name: "predicate", FunctionKind: "query", Expr: &SpecReferenceExpression{Name: "pure"}},
		{Name: "aiPredicate", FunctionKind: "query", Expr: &SpecReferenceExpression{Name: "model"}},
		{Name: "missingPredicate", FunctionKind: "query", Expr: &SpecReferenceExpression{Name: "missing"}},
		{Name: "nestedArg", FunctionKind: "query", Expr: &FunctionCallExpression{Name: "read", Args: map[string]any{"x": &BuiltinFunctionExpression{Executor: "integration.storage.upload"}}}},
		{Name: "futureIR", FunctionKind: "query", Expr: &unknownReplayExpression{}},
	} {
		if err := fns.Upsert(fn); err != nil {
			t.Fatal(err)
		}
	}
	if err := specs.add(&Spec{Name: "pure", Expr: &ComparisonExpression{Value: true}}); err != nil {
		t.Fatal(err)
	}
	if err := specs.add(&Spec{Name: "model", UsesAI: true, Expr: &AIExpression{}}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"read", "knownRead", "metadata", "callsRead", "predicate"} {
		if !ReplayReadOnly(fns, specs, name) {
			t.Errorf("read %s refused", name)
		}
	}
	for _, name := range []string{"write", "knownWrite", "unknown", "logic", "callsWrite", "cycle", "aiPredicate", "missingPredicate", "nestedArg", "futureIR", "missing"} {
		if ReplayReadOnly(fns, specs, name) {
			t.Errorf("unproven %s accepted", name)
		}
	}
}
