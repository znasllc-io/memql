// Package planner houses planner-node-shared types and helpers that
// the v1 design references but doesn't yet wire into a full async
// integration. The planner integration (when it lands as
// integrations/planner/) will live in its own package and consume
// what's defined here.
//
// Specifically:
//   - ExecutorRegistry: pluggable container-executor backend
//     mapping (Q13). NemoClaw is the first registered backend;
//     future homegrown variants register themselves at init() time
//     via RegisterContainerExecutor.
//   - TokenBudget: pre-call budget check for the agent tool-call
//     wrapper (Q6 hard-stop enforcement).
package planner

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// ContainerExecutor is the interface a backend implements to run a
// containerExecutor-surface Task. The planner picks the backend
// from Task.executorBackend (or the workspace default) and calls
// Run with the Task's input + a callback to write progress updates.
type ContainerExecutor interface {
	// Backend returns the registered backend name (e.g. "nemoclaw").
	Backend() string
	// Run executes the Task synchronously inside the backend. The
	// implementation is responsible for streaming progress to the
	// optional progress callback (file activity, command output,
	// step narration) so the Tasks page "Watch live" tab can render.
	Run(ctx context.Context, req ExecutorRequest, progress ProgressCallback) (ExecutorResult, error)
}

// ExecutorRequest carries the Task input the planner hands the
// executor. Kept narrow on purpose; richer shapes (per-Task tool
// whitelists, per-agent capability flags) ride on input as a typed
// per-kind blob.
type ExecutorRequest struct {
	TaskId    string
	PlanId    string
	AgentId   string
	Kind      string                 // task kind (fileProcessor / browseUrl / runCommand / ...)
	Input     map[string]interface{} // per-kind input payload
	Workspace string                 // per-agent workspace path (NemoClaw uses /workspaces/{agentId}/)

	// OwnerUserId is the v1:identity:user the Task runs FOR
	// (memql#4361). A backend that reaches a machine needs it for two
	// separate things the other fields cannot supply: worker
	// registrations are per-user, so it is the routing key; and the
	// back-channel credential is minted with it as the subject, which
	// is what makes row authz apply to a delegated app. Empty is a
	// REFUSAL for any machine-touching backend, never a wildcard.
	OwnerUserId string

	// RequireLabels narrows which machine may serve this Task. The
	// backend merges its own requirement (an `app:<id>` label, say)
	// with whatever the Task declared; both must match.
	RequireLabels map[string]string

	// Inputs are artifact ids the executor should make available to
	// the run before it starts. The consent card names them, which is
	// what makes "what was this agent shown" answerable later.
	Inputs []string
}

// ExecutorResult is what an executor returns when its Task succeeds.
type ExecutorResult struct {
	Output      map[string]interface{} // per-kind output payload
	TokensSpent int                    // LLM tokens spent inside the executor
	DurationMs  int64                  // wall-clock duration

	// Billing says WHO PAID for TokensSpent (memql#4361):
	// BillingMetered (MemQL's own vendor spend), BillingSubscription
	// (an app the user already pays for) or BillingUnknown (the
	// executor could not tell).
	//
	// This is why TokensSpent no longer rolls into Plan.tokenSpent
	// unconditionally. Subscription tokens are real spend that MemQL
	// did not pay, so counting them against the plan's DOLLAR ceiling
	// would stop a plan for money nobody was charged; not counting
	// them in the LOOP caps would let a runaway loop hide behind a
	// subscription. They are split in component/planner/budget.go.
	// An empty value reads as BillingMetered, which is the
	// conservative direction: an executor that says nothing has its
	// spend counted against the ceiling.
	Billing string

	// ArtifactIds are Library artifact ids the run produced.
	ArtifactIds []string
}

// Billing values on an ExecutorResult. They mirror the enum on
// v1:router:call.billing and v1:worker:appSession.billing so one
// vocabulary spans the executor seam, the ledger and the row.
const (
	BillingMetered      = "metered"
	BillingSubscription = "subscription"
	BillingUnknown      = "unknown"
)

// EffectiveBilling normalizes a result's billing for accounting. An
// empty or unrecognised value reads as metered: an executor that does
// not say has its spend counted against the dollar ceiling, which is
// the direction that fails safe.
func (r ExecutorResult) EffectiveBilling() string {
	switch r.Billing {
	case BillingSubscription, BillingUnknown:
		return r.Billing
	}
	return BillingMetered
}

// CountsAgainstDollarCeiling reports whether this result's tokens
// should be charged to the Plan's dollar budget. Subscription and
// unknown spend does not: MemQL was not billed for it, so stopping a
// plan over it would stop work for money nobody was charged. Both
// still count in the loop caps -- see budget.go.
func (r ExecutorResult) CountsAgainstDollarCeiling() bool {
	return r.EffectiveBilling() == BillingMetered
}

// ProgressCallback is invoked by the executor to stream live
// activity. Optional -- planner passes nil if no live observer is
// attached.
type ProgressCallback func(event ProgressEvent)

// ProgressEvent is one streamable update from the executor.
type ProgressEvent struct {
	Kind    string                 // "command" | "file" | "narration" | "screenshot"
	Payload map[string]interface{} // kind-specific payload
}

// ExecutorRegistry stores registered container-executor backends.
// Self-registration via RegisterContainerExecutor at init() time;
// the planner queries by Task.executorBackend at dispatch time.
type ExecutorRegistry struct {
	mu       sync.RWMutex
	backends map[string]ContainerExecutor
}

var defaultExecutorRegistry = &ExecutorRegistry{
	backends: map[string]ContainerExecutor{},
}

// RegisterContainerExecutor adds a backend to the default registry.
// Call from init() in the backend's package -- e.g. NemoClaw's
// integration registers "nemoclaw" so the planner can route Tasks
// with executorBackend="nemoclaw" through it.
func RegisterContainerExecutor(name string, exec ContainerExecutor) {
	defaultExecutorRegistry.mu.Lock()
	defer defaultExecutorRegistry.mu.Unlock()
	defaultExecutorRegistry.backends[name] = exec
}

// LookupContainerExecutor returns the registered backend by name,
// or nil + error if not found.
func LookupContainerExecutor(name string) (ContainerExecutor, error) {
	defaultExecutorRegistry.mu.RLock()
	defer defaultExecutorRegistry.mu.RUnlock()
	// Lookup keys on the base name so a Task naming
	// "cockpit-app:claude-code" reaches the "cockpit-app" backend,
	// which reads the app id off the suffix.
	exec, ok := defaultExecutorRegistry.backends[BackendBase(name)]
	if !ok {
		return nil, fmt.Errorf("planner: no container executor registered for backend %q", name)
	}
	return exec, nil
}

// RegisteredExecutors returns the sorted list of registered backend
// names. Used by debug / status endpoints and by
// ValidateExecutorBackend.
func RegisteredExecutors() []string {
	defaultExecutorRegistry.mu.RLock()
	defer defaultExecutorRegistry.mu.RUnlock()
	names := make([]string, 0, len(defaultExecutorRegistry.backends))
	for name := range defaultExecutorRegistry.backends {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// BackendBase strips the per-instance suffix from an executorBackend
// name. The cockpit-app backend registers once as "cockpit-app" and
// Tasks name a specific app as "cockpit-app:claude-code", so lookup
// and validation both key on the part before the colon.
func BackendBase(backend string) string {
	if i := strings.IndexByte(backend, ':'); i >= 0 {
		return backend[:i]
	}
	return backend
}

// BackendArg returns the part of an executorBackend name AFTER the
// colon -- for "cockpit-app:claude-code", the app id. Empty when the
// name carries no suffix.
func BackendArg(backend string) string {
	if i := strings.IndexByte(backend, ':'); i >= 0 {
		return backend[i+1:]
	}
	return ""
}

// ValidateExecutorBackend refuses a backend name nobody registered
// (memql#4361).
//
// This is called at TASK CREATION, not at dispatch, and the
// difference is the whole point. Until now a Task could name any
// backend at all; the registry is queried only when the Task is
// dispatched, so a typo produced a Task that looked queued, sat
// there, and failed much later with an error naming a lookup rather
// than the typo. Validating at creation turns that into a refusal at
// the moment the mistake is made, where the name being wrong is
// still obvious.
//
// An empty name is valid: it means "the workspace default", which the
// dispatch path resolves.
func ValidateExecutorBackend(backend string) error {
	backend = strings.TrimSpace(backend)
	if backend == "" {
		return nil
	}
	base := BackendBase(backend)
	if base == "" {
		return fmt.Errorf("planner: executorBackend %q has no backend name before its colon", backend)
	}
	defaultExecutorRegistry.mu.RLock()
	_, ok := defaultExecutorRegistry.backends[base]
	defaultExecutorRegistry.mu.RUnlock()
	if ok {
		return nil
	}
	registered := RegisteredExecutors()
	if len(registered) == 0 {
		// Naming the empty registry explicitly matters: the seam has
		// spent its whole life with nothing in it (memql#4120), so
		// "no container executor is registered in this binary" is a
		// far more useful thing to read than a list of one.
		return fmt.Errorf("planner: executorBackend %q names a backend, but no container executor is registered in this binary", backend)
	}
	return fmt.Errorf("planner: executorBackend %q is not registered; this binary has %s",
		backend, strings.Join(registered, ", "))
}
