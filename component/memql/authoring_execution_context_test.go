package memql

import (
	"context"
	"fmt"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

func TestRunScopedAuthoredFunctionsReachOrdinaryExecute(t *testing.T) {
	e, _, _ := sharedReadMergeEngine(t)
	var err error
	reg := NewAuthoredRuntimeRegistry()
	_, err = AuthorSessionBundle(reg, "alice", `logic executionAnswer { body { return 42 } }`, "authoring/concepts.memql")
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
