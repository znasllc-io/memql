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
}

// ExecutorResult is what an executor returns when its Task succeeds.
type ExecutorResult struct {
	Output      map[string]interface{} // per-kind output payload
	TokensSpent int                    // LLM tokens spent inside the executor (rolled up into Plan.tokenSpent)
	DurationMs  int64                  // wall-clock duration
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
	exec, ok := defaultExecutorRegistry.backends[name]
	if !ok {
		return nil, fmt.Errorf("planner: no container executor registered for backend %q", name)
	}
	return exec, nil
}

// RegisteredExecutors returns the list of registered backend names.
// Used by debug / status endpoints.
func RegisteredExecutors() []string {
	defaultExecutorRegistry.mu.RLock()
	defer defaultExecutorRegistry.mu.RUnlock()
	names := make([]string, 0, len(defaultExecutorRegistry.backends))
	for name := range defaultExecutorRegistry.backends {
		names = append(names, name)
	}
	return names
}
