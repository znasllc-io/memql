package memql

import (
	"context"
	"fmt"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/common"
)

// Result/journal fixtures supply the serving engine's admission seam explicitly.
// They exercise live dispatch versus replay, not the durable guard's storage or
// lease arithmetic. Keep the shared engine factory unwired so a missing guard
// remains a refusal in every fixture that does not opt in.
type resultProviderAdmission struct {
	fakeGuard
	expected common.RunContext
	calls    int
}

func wireResultProviderAdmission(e *MemQLEngine, expected common.RunContext) *resultProviderAdmission {
	guard := &resultProviderAdmission{expected: expected}
	e.SetRunCeilingGuard(guard)
	return guard
}

func (g *resultProviderAdmission) AdmitProvider(ctx context.Context, run common.RunContext, _ ProviderAdmission) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	access, _ := auth.AccessFromContext(ctx)
	if access == nil || access.UserId != g.expected.OwnerUserId || run.OwnerUserId != g.expected.OwnerUserId ||
		run.RunId != g.expected.RunId || run.GoalId != g.expected.GoalId || run.StepKey != g.expected.StepKey || run.Mode != common.RunModeLive {
		return fmt.Errorf("result fixture provider admission lost its exact live owner/run/goal/step")
	}
	g.calls++
	return nil
}

func (g *resultProviderAdmission) assertCalls(t *testing.T, want int) {
	t.Helper()
	if g.calls != want {
		t.Fatalf("provider admission count=%d, want %d; replay must not dispatch or reserve another provider call", g.calls, want)
	}
}
