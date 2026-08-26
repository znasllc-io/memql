package planner

import (
	"context"
	"strings"
	"testing"
)

type stubExecutor struct{ name string }

func (s stubExecutor) Backend() string { return s.name }
func (s stubExecutor) Run(context.Context, ExecutorRequest, ProgressCallback) (ExecutorResult, error) {
	return ExecutorResult{}, nil
}

// withRegistered installs a backend for the duration of a test and
// removes it afterwards, so the package-level registry does not leak
// between cases.
func withRegistered(t *testing.T, name string) {
	t.Helper()
	RegisterContainerExecutor(name, stubExecutor{name: name})
	t.Cleanup(func() {
		defaultExecutorRegistry.mu.Lock()
		delete(defaultExecutorRegistry.backends, name)
		defaultExecutorRegistry.mu.Unlock()
	})
}

// TestValidateExecutorBackendRefusesUnregistered is memql#4361's
// creation-time gate. Before it, a Task could name any backend at
// all: the registry is queried only at DISPATCH, so a typo produced a
// Task that looked queued, sat there, and failed much later with an
// error naming a lookup rather than the typo.
func TestValidateExecutorBackendRefusesUnregistered(t *testing.T) {
	// An empty name is valid -- it means "the workspace default".
	if err := ValidateExecutorBackend(""); err != nil {
		t.Fatalf("empty backend must be allowed: %v", err)
	}

	err := ValidateExecutorBackend("cockpit-app:claude-code")
	if err == nil {
		t.Fatal("an unregistered backend must be refused at task creation")
	}
	// With nothing registered, the message must say so: this seam
	// spent its whole life empty (memql#4120), and "no container
	// executor is registered in this binary" is a far more useful
	// thing to read than a list of one.
	if !strings.Contains(err.Error(), "no container executor is registered") {
		t.Fatalf("an empty registry must say so, got %v", err)
	}

	withRegistered(t, "cockpit-app")
	if err := ValidateExecutorBackend("cockpit-app:claude-code"); err != nil {
		t.Fatalf("a registered backend with an app suffix must validate: %v", err)
	}
	if err := ValidateExecutorBackend("cockpit-app"); err != nil {
		t.Fatalf("a registered backend with no suffix must validate: %v", err)
	}

	err = ValidateExecutorBackend("nemoclaw")
	if err == nil {
		t.Fatal("a backend nobody registered must still be refused")
	}
	if !strings.Contains(err.Error(), "cockpit-app") {
		t.Fatalf("the refusal must name what IS registered, got %v", err)
	}
}

// TestLookupResolvesTheBaseName: a Task naming cockpit-app:codex must
// reach the one registered cockpit-app backend, which reads the app
// off the suffix. Registering per-app would mean a release every time
// the app list grew.
func TestLookupResolvesTheBaseName(t *testing.T) {
	withRegistered(t, "cockpit-app")
	exec, err := LookupContainerExecutor("cockpit-app:codex")
	if err != nil {
		t.Fatalf("LookupContainerExecutor: %v", err)
	}
	if exec.Backend() != "cockpit-app" {
		t.Fatalf("resolved to %q, want cockpit-app", exec.Backend())
	}
	if got := BackendArg("cockpit-app:codex"); got != "codex" {
		t.Fatalf("BackendArg = %q, want codex", got)
	}
	if got := BackendArg("cockpit-app"); got != "" {
		t.Fatalf("BackendArg with no suffix = %q, want empty", got)
	}
}

// TestSplitSpendKeepsUnbilledTokensOffTheDollarCeiling is memql#4362's
// accounting rule, extended to local models by memql#4681. The two caps
// want opposite answers about tokens MemQL was not billed for, so they
// are counted in different places.
func TestSplitSpendKeepsUnbilledTokensOffTheDollarCeiling(t *testing.T) {
	got := SplitSpend(ExecutorResult{TokensSpent: 100, Billing: BillingSubscription})
	if (got != Spend{Subscription: 100}) {
		t.Fatalf("subscription spend = %+v, want it entirely on the subscription counter", got)
	}

	got = SplitSpend(ExecutorResult{TokensSpent: 100, Billing: BillingMetered})
	if (got != Spend{Metered: 100}) {
		t.Fatalf("metered spend = %+v, want it entirely on the metered counter", got)
	}

	// A local model runs on hardware the user already owns. The tokens are
	// real; the bill is not.
	got = SplitSpend(ExecutorResult{TokensSpent: 100, Billing: BillingLocal})
	if (got != Spend{Local: 100}) {
		t.Fatalf("local spend = %+v, want it entirely on the local counter -- charging it to a "+
			"dollar budget would mean the more someone used their own machine, the sooner "+
			"their plans stopped", got)
	}

	// Unknown is not metered for the ceiling -- MemQL was not billed -- and
	// it must NOT land on the local counter either, which would claim the
	// work ran on the user's hardware.
	got = SplitSpend(ExecutorResult{TokensSpent: 100, Billing: BillingUnknown})
	if got.Local != 0 {
		t.Fatalf("unknown spend = %+v; recording it as local would claim it ran on the user's "+
			"machine, which is a fact nobody established", got)
	}
	if got.Metered != 0 || got.Subscription != 100 {
		t.Fatalf("unknown spend = %+v, want it off the dollar ceiling", got)
	}

	// An executor that says NOTHING is metered: unattributed spend counts
	// against the ceiling rather than vanishing into a covered bucket,
	// where it would be invisible to the one control that stops runaway
	// cost.
	got = SplitSpend(ExecutorResult{TokensSpent: 100})
	if (got != Spend{Metered: 100}) {
		t.Fatalf("unreported billing = %+v, want it counted against the ceiling", got)
	}
}

// The dollar ceiling reads Spent alone. Local and subscription tokens sit
// beside it and must not shrink the budget.
func TestTheDollarCeilingIgnoresLocalAndSubscriptionSpend(t *testing.T) {
	b := NewEngineTokenBudget(stubLookup{state: TokenState{
		Budget:            1000,
		Spent:             100,
		SpentSubscription: 5000,
		SpentLocal:        5000,
	}}, 0)
	if err := b.CheckCall(context.Background(), "p1", 500); err != nil {
		t.Fatalf("a plan with 900 metered tokens left must admit a 500-token call even after "+
			"10000 unbilled ones: %v", err)
	}
	if err := b.CheckCall(context.Background(), "p1", 901); err == nil {
		t.Fatal("the ceiling must still stop a call that does not fit the METERED budget")
	}
}

type stubLookup struct{ state TokenState }

func (s stubLookup) GetPlanTokenState(context.Context, string) (TokenState, error) {
	return s.state, nil
}

// TestDollarCeilingIgnoresSubscriptionSpend: a plan that leaned
// heavily on the user's own spend.Subscription must not be parked over
// money nobody charged. The more they use what they already pay for,
// the sooner their plans would stop -- exactly backwards.
func TestDollarCeilingIgnoresSubscriptionSpend(t *testing.T) {
	budget := &EngineTokenBudget{Lookup: stubLookup{state: TokenState{
		Budget:            1000,
		Spent:             100,
		SpentSubscription: 900_000,
	}}}
	if err := budget.CheckCall(context.Background(), "plan-1", 500); err != nil {
		t.Fatalf("spend.Subscription spend must not exhaust the dollar ceiling: %v", err)
	}

	// Metered spend still does.
	budget = &EngineTokenBudget{Lookup: stubLookup{state: TokenState{Budget: 1000, Spent: 900}}}
	if err := budget.CheckCall(context.Background(), "plan-1", 500); err == nil {
		t.Fatal("spend.Metered spend past the ceiling must still be refused")
	}
}
