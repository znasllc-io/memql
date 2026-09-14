package memql

import (
	"context"
	"fmt"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

func TestRunScopedAuthoredFunctionsReachOrdinaryExecute(t *testing.T) {
	e, _, _ := sharedReadMergeEngine(t)
	// An edition-2026 logic body that returns an expression runs on the
	// LogicRunner (logic_body_v1.go), which component/automations provides and
	// this package cannot import. This one evaluates the body's return, so the
	// 42 asserted below is the authored body's own answer, reached through the
	// run-scoped registry -- not a value the stand-in makes up. Restored when
	// the test ends: the engine is the shared one.
	previous := e.LogicRunner()
	e.SetLogicRunner(returnEvaluatingLogicRunner{})
	t.Cleanup(func() { e.SetLogicRunner(previous) })

	var err error
	reg := NewAuthoredRuntimeRegistry()
	_, err = AuthorSessionBundle(reg, "alice", `logic executionAnswer { body { return 42 } }`, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := ContextWithAuthoredExecution(auth.ContextWithUserActor(context.Background(), "alice"), "alice", reg)
	got, err := e.Execute(ctx, "executionAnswer()")
	if err != nil {
		t.Fatalf("execution replica lost run-scoped function: %v", err)
	}
	if got == nil || fmt.Sprint(got.output) != "42" {
		t.Fatalf("missing computed answer: %+v", got)
	}
	if _, err = e.Execute(auth.ContextWithUserActor(ctx, "bob"), "executionAnswer()"); err == nil {
		t.Fatal("another owner resolved run-private code")
	}
	if _, err = e.Execute(auth.ContextWithUserActor(context.Background(), "alice"), "executionAnswer()"); err == nil {
		t.Fatal("run-private code escaped into shared engine")
	}
}

// returnEvaluatingLogicRunner stands in for the LogicRunner for a logic body
// that is one `return <expression>`: it evaluates the return with EvalExpr,
// over no bindings. A body with intermediate steps is refused -- this is not
// a second runtime.
type returnEvaluatingLogicRunner struct{}

func (returnEvaluatingLogicRunner) RunLogic(ctx context.Context, fnName string, body *languageParser.AutomationDef, _ map[string]any) (any, error) {
	if nonReturnStepCount(body.Steps) > 0 {
		return nil, fmt.Errorf("logic %q has steps before its return; the test runner evaluates a lone return only", fnName)
	}
	ret, err := extractLogicReturnExpression(body)
	if err != nil {
		return nil, err
	}
	return EvalExpr(ctx, ret, MapScope{}, EvalOptions{})
}
