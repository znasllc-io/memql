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

// TestSplitSpendKeepsSubscriptionOffTheDollarCeiling is memql#4362's
// accounting rule. The two caps want opposite answers about
// subscription tokens, so they are counted in different places.
func TestSplitSpendKeepsSubscriptionOffTheDollarCeiling(t *testing.T) {
	metered, subscription := SplitSpend(ExecutorResult{TokensSpent: 100, Billing: BillingSubscription})
	if metered != 0 || subscription != 100 {
		t.Fatalf("subscription spend = (%d metered, %d subscription), want (0, 100)", metered, subscription)
	}

	metered, subscription = SplitSpend(ExecutorResult{TokensSpent: 100, Billing: BillingMetered})
	if metered != 100 || subscription != 0 {
		t.Fatalf("metered spend = (%d, %d), want (100, 0)", metered, subscription)
	}

	// Unknown is NOT metered for the ceiling -- MemQL was not billed
	// -- but it is visible as unknown rather than folded into either
	// side, which is the whole reason the enum has three values.
	metered, subscription = SplitSpend(ExecutorResult{TokensSpent: 100, Billing: BillingUnknown})
	if metered != 0 || subscription != 100 {
		t.Fatalf("unknown spend = (%d, %d), want (0, 100)", metered, subscription)
	}

	// An executor that says NOTHING is metered: unattributed spend
	// counts against the ceiling rather than vanishing into the
	// covered bucket, where it would be invisible to the one control
	// that stops runaway cost.
	metered, subscription = SplitSpend(ExecutorResult{TokensSpent: 100})
	if metered != 100 || subscription != 0 {
		t.Fatalf("unreported billing = (%d, %d), want (100, 0)", metered, subscription)
	}
}

type stubLookup struct{ state TokenState }

func (s stubLookup) GetPlanTokenState(context.Context, string) (TokenState, error) {
	return s.state, nil
}

// TestDollarCeilingIgnoresSubscriptionSpend: a plan that leaned
// heavily on the user's own subscription must not be parked over
// money nobody charged. The more they use what they already pay for,
// the sooner their plans would stop -- exactly backwards.
func TestDollarCeilingIgnoresSubscriptionSpend(t *testing.T) {
	budget := &EngineTokenBudget{Lookup: stubLookup{state: TokenState{
		Budget:            1000,
		Spent:             100,
		SpentSubscription: 900_000,
	}}}
	if err := budget.CheckCall(context.Background(), "plan-1", 500); err != nil {
		t.Fatalf("subscription spend must not exhaust the dollar ceiling: %v", err)
	}

	// Metered spend still does.
	budget = &EngineTokenBudget{Lookup: stubLookup{state: TokenState{Budget: 1000, Spent: 900}}}
	if err := budget.CheckCall(context.Background(), "plan-1", 500); err == nil {
		t.Fatal("metered spend past the ceiling must still be refused")
	}
}
