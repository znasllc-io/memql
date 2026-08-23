//go:build agent

package worker

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/planner"
	workerservice "github.com/znasllc-io/memql/component/worker"
)

// TestCockpitAppRefusesAnUnattributedTask: worker registrations are
// per-user and the back-channel credential is minted with the owner
// as its subject. A blank owner would either match nobody or be read
// as "any", so it is a refusal rather than a default.
func TestCockpitAppRefusesAnUnattributedTask(t *testing.T) {
	exec := newTestCockpitAppExecutor(t)
	_, err := exec.Run(context.Background(), planner.ExecutorRequest{
		Input: map[string]any{"executorBackend": "cockpit-app:claude-code"},
	}, nil)
	if err == nil {
		t.Fatal("ran a task with no owner")
	}
	if !strings.Contains(err.Error(), "unattributed") {
		t.Fatalf("the refusal must say why, got %v", err)
	}
}

// TestCockpitAppRefusesAnAppItDoesNotDrive keeps the engine's closed
// runnable set closed at the executor boundary too, so a Task cannot
// reach past it by naming an app in its backend string.
func TestCockpitAppRefusesAnAppItDoesNotDrive(t *testing.T) {
	exec := newTestCockpitAppExecutor(t)
	_, err := exec.Run(context.Background(), planner.ExecutorRequest{
		OwnerUserId: "user-1",
		Input:       map[string]any{"executorBackend": "cockpit-app:some-future-app"},
	}, nil)
	if err == nil {
		t.Fatal("ran an app outside the engine's closed set")
	}
	if !strings.Contains(err.Error(), "known:") {
		t.Fatalf("the refusal must name what IS driveable, got %v", err)
	}
}

// TestCockpitAppRequiresAnAppInTheBackendName: "cockpit-app" alone
// does not say WHICH app, and picking one would be the engine
// choosing on the user's behalf.
func TestCockpitAppRequiresAnAppInTheBackendName(t *testing.T) {
	exec := newTestCockpitAppExecutor(t)
	_, err := exec.Run(context.Background(), planner.ExecutorRequest{
		OwnerUserId: "user-1",
		Input:       map[string]any{"executorBackend": "cockpit-app"},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "must name an app") {
		t.Fatalf("expected a name-the-app refusal, got %v", err)
	}
}

// TestCockpitAppRunsTheSharedConsentGates is D4: an app run gets
// EXACTLY the gates a shell command through the same machine gets.
// Here the per-task approval gate fires because the Task carries no
// PlanId -- the same denial `workerHost` would produce.
func TestCockpitAppRunsTheSharedConsentGates(t *testing.T) {
	exec := newTestCockpitAppExecutor(t)
	_, err := exec.Run(context.Background(), planner.ExecutorRequest{
		OwnerUserId: "user-1",
		Input:       map[string]any{"executorBackend": "cockpit-app:claude-code"},
	}, nil)
	if err == nil {
		t.Fatal("ran without per-task approval")
	}
	if !strings.Contains(err.Error(), "denied_no_per_task_approval") {
		t.Fatalf("expected the shared per-task approval gate, got %v", err)
	}
}

// TestDelegationPolicyAllowsKind pins two defaults that are easy to
// get backwards. Delegation off means NOTHING is eligible whatever
// the kind list says; and an EMPTY kind list allows nothing rather
// than everything, so opting into delegation does not silently opt
// every task kind in with it.
func TestDelegationPolicyAllowsKind(t *testing.T) {
	off := DelegationPolicy{EligibleKinds: []string{"runCommand"}}
	if off.AllowsKind("runCommand") {
		t.Fatal("delegation must be off until the master switch is on")
	}

	onButEmpty := DelegationPolicy{PreferSubscriptionApps: true}
	if onButEmpty.AllowsKind("runCommand") {
		t.Fatal("an empty eligible-kinds list must allow nothing, not everything")
	}

	on := DelegationPolicy{PreferSubscriptionApps: true, EligibleKinds: []string{"runCommand"}}
	if !on.AllowsKind("runCommand") {
		t.Fatal("an explicitly eligible kind must be allowed")
	}
	if on.AllowsKind("browseUrl") {
		t.Fatal("a kind outside the list must not be delegated")
	}
}

// TestMergeRequireLabelsKeepsTaskPins: the app requirement is added
// on top of the Task's own labels, never in place of them. It also
// drops any app: value so selection does not silently refuse a
// machine running a NEWER app -- a genuine version floor is something
// the Task states for itself.
func TestMergeRequireLabelsKeepsTaskPins(t *testing.T) {
	got := mergeRequireLabels(map[string]string{
		"has-gpu": "true",
		workerservice.AppLabelKey(workerservice.AppIdClaudeCode): "2.0",
	}, workerservice.AppIdClaudeCode)

	if got["has-gpu"] != "true" {
		t.Fatalf("the task's own pin was dropped: %v", got)
	}
	if _, pinned := got[workerservice.AppLabelKey(workerservice.AppIdClaudeCode)]; pinned {
		t.Fatalf("an app version pin must not be synthesised: %v", got)
	}
}

// TestCockpitAppIsRegisteredAtInit: the seam has been empty since
// memql#4120, so the thing worth pinning is that an agent binary now
// has an inhabitant and ValidateExecutorBackend accepts it.
func TestCockpitAppIsRegisteredAtInit(t *testing.T) {
	if err := planner.ValidateExecutorBackend("cockpit-app:claude-code"); err != nil {
		t.Fatalf("cockpit-app must be registered on an agent binary: %v", err)
	}
	found := false
	for _, name := range planner.RegisteredExecutors() {
		if name == BackendCockpitApp {
			found = true
		}
	}
	if !found {
		t.Fatalf("cockpit-app absent from %v", planner.RegisteredExecutors())
	}
}

// TestUnwiredBackendRefusesWithANamedReason: registration happens at
// init() but the implementation is installed later. A node that never
// installs one must refuse with a reason rather than nil-panic.
func TestUnwiredBackendRefusesWithANamedReason(t *testing.T) {
	cockpitAppMu.Lock()
	saved := cockpitAppExecutor
	cockpitAppExecutor = nil
	cockpitAppMu.Unlock()
	t.Cleanup(func() {
		cockpitAppMu.Lock()
		cockpitAppExecutor = saved
		cockpitAppMu.Unlock()
	})

	_, err := cockpitAppDelegate{}.Run(context.Background(), planner.ExecutorRequest{}, nil)
	if err == nil {
		t.Fatal("an unwired backend must refuse")
	}
	if !strings.Contains(err.Error(), "not wired on this node") {
		t.Fatalf("the refusal must say the node is not wired, got %v", err)
	}
}

func newTestCockpitAppExecutor(t *testing.T) *CockpitAppExecutor {
	t.Helper()
	logger := slog.Default()
	dispatcher, err := NewDispatcher(Options{
		Logger:   logger,
		Registry: workerservice.NewRegistry(logger, nil),
	})
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	exec, err := NewCockpitAppExecutor(logger, dispatcher, &workerservice.SessionRunner{
		Registry: workerservice.NewRegistry(logger, nil),
	}, nil)
	if err != nil {
		t.Fatalf("NewCockpitAppExecutor: %v", err)
	}
	return exec
}
