package memql

import (
	"strings"
	"testing"
)

// TestValidateCQSAcrossRegistry_QueryCallsMutation locks the
// cross-file CQS rule: a query in file A calling a mutation in
// file B is a violation, reported at engine startup with a
// message naming both parties.
func TestValidateCQSAcrossRegistry_QueryCallsMutation(t *testing.T) {
	reg := newFunctionRegistry()
	must(t, reg.Upsert(&Function{
		Name:         "queryBad",
		FunctionKind: "query",
		ExprSource:   `mutationArchive(args)`,
	}))
	must(t, reg.Upsert(&Function{
		Name:         "mutationArchive",
		FunctionKind: "mutation",
		ExprSource:   `insert { id: args.id }`,
	}))

	err := ValidateCQSAcrossRegistry(reg)
	if err == nil {
		t.Fatal("expected CQS violation, got nil")
	}
	for _, want := range []string{"queryBad", "mutationArchive", "CQS violation"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got %v", want, err)
		}
	}
}

// TestValidateCQSAcrossRegistry_MutationCallsMutation locks the
// "one observable write per body" rule at the call-graph level.
func TestValidateCQSAcrossRegistry_MutationCallsMutation(t *testing.T) {
	reg := newFunctionRegistry()
	must(t, reg.Upsert(&Function{
		Name:         "mutationCaller",
		FunctionKind: "mutation",
		ExprSource:   `mutationCallee(args)`,
	}))
	must(t, reg.Upsert(&Function{
		Name:         "mutationCallee",
		FunctionKind: "mutation",
		ExprSource:   `insert { id: args.id }`,
	}))

	err := ValidateCQSAcrossRegistry(reg)
	if err == nil {
		t.Fatal("expected CQS violation, got nil")
	}
	if !strings.Contains(err.Error(), "Rule #1") {
		t.Errorf("error should reference Rule #1 (mutation single-write) refactor path, got %v", err)
	}
}

// TestValidateCQSAcrossRegistry_QueryCallsQuery is the allow case:
// queries can compose other queries.
func TestValidateCQSAcrossRegistry_QueryCallsQuery(t *testing.T) {
	reg := newFunctionRegistry()
	must(t, reg.Upsert(&Function{
		Name:         "queryOuter",
		FunctionKind: "query",
		ExprSource:   `queryInner(args)`,
	}))
	must(t, reg.Upsert(&Function{
		Name:         "queryInner",
		FunctionKind: "query",
		ExprSource:   `filter id == args.id`,
	}))

	if err := ValidateCQSAcrossRegistry(reg); err != nil {
		t.Errorf("query->query should be allowed, got %v", err)
	}
}

// TestValidateCQSAcrossRegistry_IgnoresDottedCalls covers the
// extractor's edge: `args.foo(` is method-call syntax that should
// NOT register as a bare callee.
func TestValidateCQSAcrossRegistry_IgnoresDottedCalls(t *testing.T) {
	reg := newFunctionRegistry()
	must(t, reg.Upsert(&Function{
		Name:         "queryWithDots",
		FunctionKind: "query",
		ExprSource:   `args.foo() && obj.mutationLike()`,
	}))
	must(t, reg.Upsert(&Function{
		Name:         "mutationLike",
		FunctionKind: "mutation",
		ExprSource:   `insert { }`,
	}))

	if err := ValidateCQSAcrossRegistry(reg); err != nil {
		t.Errorf("dotted method-call syntax should not trigger CQS, got %v", err)
	}
}

// TestValidateCQSAcrossRegistry_SelfReferenceFromRewriter pins that
// a mutation whose stored ExprSource carries the struct-form
// rewriter's procedural header (`func (Mutation) NAME(ctx any) error
// { ... }`) is NOT flagged as calling itself. The extractor sees the
// header's `NAME(` and would otherwise loop back as a self-call --
// flagging every mutation in the registry, refusing to boot.
func TestValidateCQSAcrossRegistry_SelfReferenceFromRewriter(t *testing.T) {
	reg := newFunctionRegistry()
	// ExprSource shape mirrors what extractExpressionFromContent
	// returns for a rewritten mutation -- args block, the synthetic
	// `func (Mutation) NAME(ctx any) error { ... }` wrapper, and the
	// `return insert(...)` body. The function header carries the
	// name with a `(` immediately after, which extractBareCallNames
	// would otherwise mark as a callee.
	must(t, reg.Upsert(&Function{
		Name:         "mutationAddAgentToSpace",
		FunctionKind: "mutation",
		ExprSource: `partitionId string @required
}
func (Mutation) mutationAddAgentToSpace(ctx any) error {
  return insert(participant, id=concat("si-", hash("seed")))
}`,
	}))

	if err := ValidateCQSAcrossRegistry(reg); err != nil {
		t.Errorf("self-reference from rewriter header should be ignored, got %v", err)
	}
}

// TestValidateCQSAcrossRegistry_EmptyRegistry covers the no-op
// guard.
func TestValidateCQSAcrossRegistry_EmptyRegistry(t *testing.T) {
	reg := newFunctionRegistry()
	if err := ValidateCQSAcrossRegistry(reg); err != nil {
		t.Errorf("empty registry should pass, got %v", err)
	}
	if err := ValidateCQSAcrossRegistry(nil); err != nil {
		t.Errorf("nil registry should pass, got %v", err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}
}
