//go:build agent

package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/planner"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/id"
)

// cockpitapp.go is the FIRST inhabitant of the container-executor
// seam (memql#4361). The seam has existed and been empty since
// memql#4120: RegisterContainerExecutor was there, Task carried
// executionSurface + executorBackend, and nothing in the tree ever
// called it -- so a containerExecutor Task had nowhere to land.
//
// It lives in this package rather than a sibling one so it can reuse
// preDispatchCheck unexported. That is not a shortcut: D4's whole
// point is that an app run needs EXACTLY the gates workerHost needs
// -- per-task approval, the kill switch, standing scope, the
// classifier -- and a copy of those gates in another package is a
// copy that drifts. Reusing the function means an app run cannot
// quietly end up with a weaker check than a shell command through the
// same machine.

// BackendCockpitApp is the registered backend name. Tasks name a
// specific app as "cockpit-app:<appId>"; the base name is what
// registers, so growing the app list is a value change rather than a
// release.
const BackendCockpitApp = "cockpit-app"

// defaultAppSessionMaxDuration bounds a run whose delegation policy
// sets none. Generous, because a headless coding agent legitimately
// runs for a long time -- but not unbounded, because a run nobody
// ends is a machine nobody gets back.
const defaultAppSessionMaxDuration = 4 * time.Hour

// CockpitAppExecutor runs a Task by opening an app session on one of
// the owner's cockpit machines.
type CockpitAppExecutor struct {
	logger     *slog.Logger
	dispatcher *Dispatcher
	runner     *workerservice.SessionRunner
	policies   DelegationPolicyReader
}

// DelegationPolicyReader resolves a user's delegation preference.
// Narrow on purpose so this file does not import the engine.
type DelegationPolicyReader interface {
	DelegationPolicy(ctx context.Context, ownerUserId string) (DelegationPolicy, error)
}

// DelegationPolicy is the subset of v1:worker:delegationPolicy the
// executor reads.
type DelegationPolicy struct {
	Found                  bool
	PreferSubscriptionApps bool
	EligibleKinds          []string
	AppOrder               []string
	MaxConcurrentSessions  int
	WorkspaceRoot          string
	CredentialLifetime     time.Duration
}

// AllowsKind reports whether this policy permits delegating a task of
// the given kind. An empty EligibleKinds list allows NOTHING rather
// than everything: opting into delegation should not silently opt
// every task kind in with it.
func (p DelegationPolicy) AllowsKind(kind string) bool {
	if !p.PreferSubscriptionApps {
		return false
	}
	for _, k := range p.EligibleKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// cockpitAppRegistry holds the process-wide executor so init() can
// register a placeholder that the app wiring later completes. The
// registry maps a NAME to an implementation, and the implementation
// needs a dispatcher and a runner that do not exist at init() time.
var (
	cockpitAppMu       sync.RWMutex
	cockpitAppExecutor *CockpitAppExecutor
)

func init() {
	// Registering at init() is what makes ValidateExecutorBackend
	// accept "cockpit-app:*" on an agent binary and refuse it
	// everywhere else, which is the honest answer: only an agent node
	// holds worker streams, so only an agent node can serve one.
	planner.RegisterContainerExecutor(BackendCockpitApp, cockpitAppDelegate{})
}

// cockpitAppDelegate is the registry entry. It forwards to whatever
// the app wiring installed, and refuses with a NAMED reason when
// nothing did -- rather than nil-panicking or silently succeeding.
type cockpitAppDelegate struct{}

func (cockpitAppDelegate) Backend() string { return BackendCockpitApp }

func (cockpitAppDelegate) Run(ctx context.Context, req planner.ExecutorRequest, progress planner.ProgressCallback) (planner.ExecutorResult, error) {
	cockpitAppMu.RLock()
	exec := cockpitAppExecutor
	cockpitAppMu.RUnlock()
	if exec == nil {
		return planner.ExecutorResult{}, errors.New(
			"cockpit-app: the backend is registered but not wired on this node (no worker service); " +
				"a Task can only reach it on an agent node running WorkerService")
	}
	return exec.Run(ctx, req, progress)
}

// InstallCockpitAppExecutor completes the registration made at
// init(). Called from the agent node's wiring once the worker service
// and the engine exist.
func InstallCockpitAppExecutor(exec *CockpitAppExecutor) {
	cockpitAppMu.Lock()
	cockpitAppExecutor = exec
	cockpitAppMu.Unlock()
}

// NewCockpitAppExecutor builds the executor.
func NewCockpitAppExecutor(logger *slog.Logger, dispatcher *Dispatcher, runner *workerservice.SessionRunner, policies DelegationPolicyReader) (*CockpitAppExecutor, error) {
	if logger == nil {
		return nil, errors.New("cockpit-app: logger required")
	}
	if dispatcher == nil {
		return nil, errors.New("cockpit-app: dispatcher required (it owns the consent gates)")
	}
	if runner == nil {
		return nil, errors.New("cockpit-app: session runner required")
	}
	return &CockpitAppExecutor{logger: logger, dispatcher: dispatcher, runner: runner, policies: policies}, nil
}

// Backend implements planner.ContainerExecutor.
func (e *CockpitAppExecutor) Backend() string { return BackendCockpitApp }

// Run opens an app session for the Task and maps its outcome onto an
// ExecutorResult.
//
// The gate order matters and is deliberate: identity, then app,
// then the SHARED consent gates, then the machine. Running the
// consent gates before selection means a refused run never reveals
// which machines the user has online; running them after would make
// "you have no machine for this" and "you may not do this" both
// present as the same failure.
func (e *CockpitAppExecutor) Run(ctx context.Context, req planner.ExecutorRequest, progress planner.ProgressCallback) (planner.ExecutorResult, error) {
	started := time.Now()

	// An empty owner is a REFUSAL, never a wildcard. Worker
	// registrations are per-user and the back-channel credential is
	// minted with this as its subject; a blank one would either match
	// nobody or, worse, be treated as "any".
	ownerUserId := strings.TrimSpace(req.OwnerUserId)
	if ownerUserId == "" {
		return planner.ExecutorResult{}, errors.New("cockpit-app: task has no owner; a machine-touching backend cannot run unattributed")
	}

	appId := planner.BackendArg(taskBackend(req))
	if appId == "" {
		return planner.ExecutorResult{}, errors.New(
			"cockpit-app: executorBackend must name an app, e.g. \"cockpit-app:claude-code\"")
	}
	if !workerservice.IsKnownAppId(appId) {
		return planner.ExecutorResult{}, fmt.Errorf(
			"cockpit-app: %q is not an app this engine drives (known: %s)",
			appId, strings.Join(workerservice.KnownAppIds(), ", "))
	}

	// D4: the SAME gates a shell command through this machine gets.
	// An app run edits files and runs commands on the user's own
	// computer; there is no weaker consent story that is honest about
	// what it does. `exec` is the action whose requirement is `full`,
	// which is the tier an autonomous coding agent actually needs.
	gate := e.dispatcher.preDispatchCheck(ctx, Request{
		Tool:        "workerHost",
		Action:      "exec",
		AgentId:     req.AgentId,
		OwnerUserId: ownerUserId,
		PlanId:      req.PlanId,
		TaskId:      req.TaskId,
		Args: map[string]any{
			"app":       appId,
			"kind":      req.Kind,
			"workspace": req.Workspace,
		},
	})
	if gate.deny {
		return planner.ExecutorResult{}, fmt.Errorf("cockpit-app: %s: %s", gate.errorCode, gate.errorMessage)
	}

	policy := DelegationPolicy{}
	if e.policies != nil {
		resolved, err := e.policies.DelegationPolicy(ctx, ownerUserId)
		if err != nil {
			e.logger.Warn("cockpit-app: delegation policy lookup failed; using defaults",
				"owner_user_id", ownerUserId, "error", err)
		} else {
			policy = resolved
		}
	}

	workspace := req.Workspace
	if workspace == "" && policy.WorkspaceRoot != "" {
		// The per-plan directory convention mirrors the workbench's:
		// one tree per unit of work, so a run cannot read the previous
		// one's leftovers by accident.
		workspace = strings.TrimRight(policy.WorkspaceRoot, "/") + "/" + req.PlanId
	}

	spec := workerservice.RunSpec{
		SessionId:          "v1:worker:appSession:" + id.NewShortId(),
		OwnerUserId:        ownerUserId,
		App:                appId,
		Kind:               workerservice.AppSessionKindRun,
		Prompt:             promptFromInput(req),
		Workspace:          workspace,
		Inputs:             req.Inputs,
		PlanId:             req.PlanId,
		TaskId:             req.TaskId,
		RequireLabels:      mergeRequireLabels(req.RequireLabels, appId),
		CredentialLifetime: policy.CredentialLifetime,
		MaxDuration:        defaultAppSessionMaxDuration,
	}

	result, runErr := e.runner.Run(ctx, spec, progressBridge(progress))

	out := planner.ExecutorResult{
		Output: map[string]interface{}{
			"sessionId":     result.SessionId,
			"workerId":      result.WorkerId,
			"app":           appId,
			"exitCode":      result.ExitCode,
			"appSessionRef": result.AppSessionRef,
			"transcript":    result.Transcript,
		},
		// TokensSpent is what the APP reported, never a MemQL
		// estimate. An app that reports nothing contributes zero
		// tokens and `unknown` billing, which is visible as silence
		// rather than as a free call.
		TokensSpent: int(result.Usage.InputTokens + result.Usage.OutputTokens),
		DurationMs:  time.Since(started).Milliseconds(),
		Billing:     billingOrUnknown(result.Billing),
		ArtifactIds: result.ProducedArtifactIds,
	}
	if runErr != nil {
		return out, fmt.Errorf("cockpit-app: %s run failed: %w", appId, runErr)
	}
	return out, nil
}

// mergeRequireLabels adds the app's own routing label to whatever the
// Task declared. Both must match: a Task that pinned a machine label
// keeps that pin, and the app requirement is added on top rather than
// replacing it.
func mergeRequireLabels(taskLabels map[string]string, appId string) map[string]string {
	out := make(map[string]string, len(taskLabels))
	for k, v := range taskLabels {
		out[k] = v
	}
	// The app: label's presence is what PickWorkerForApp checks; the
	// VALUE is a version, and pinning one here would refuse a machine
	// running a newer app for no stated reason. A Task that genuinely
	// needs a version floor states it in its own RequireLabels.
	delete(out, workerservice.AppLabelKey(appId))
	if len(out) == 0 {
		return nil
	}
	return out
}

// taskBackend recovers the full executorBackend name for this
// request. The planner passes it on Input under "executorBackend";
// falling back to the bare backend name keeps a Task that named no
// app reaching the explicit error above rather than a nil map read.
func taskBackend(req planner.ExecutorRequest) string {
	if raw, ok := req.Input["executorBackend"].(string); ok && raw != "" {
		return raw
	}
	return BackendCockpitApp
}

// promptFromInput pulls the run's prompt off the Task input.
func promptFromInput(req planner.ExecutorRequest) string {
	for _, key := range []string{"prompt", "goal", "instruction"} {
		if raw, ok := req.Input[key].(string); ok && strings.TrimSpace(raw) != "" {
			return raw
		}
	}
	return ""
}

// billingOrUnknown normalizes a runner billing value for the seam.
func billingOrUnknown(billing string) string {
	switch billing {
	case workerservice.BillingMetered:
		return planner.BillingMetered
	case workerservice.BillingSubscription:
		return planner.BillingSubscription
	}
	return planner.BillingUnknown
}

// progressBridge maps session chunks onto planner ProgressEvents so
// the Tasks page's live view renders a delegated run the same way it
// renders an in-process one.
//
// An "event" chunk carries the app's own structured JSON, which is
// where a `command` or `file` event comes from; plain stdout/stderr
// becomes narration. Guessing structure out of raw stdout would
// produce a live view that is confidently wrong about what the agent
// did.
func progressBridge(progress planner.ProgressCallback) workerservice.ProgressFunc {
	if progress == nil {
		return nil
	}
	return func(chunk workerservice.AppSessionChunk) {
		kind := "narration"
		if chunk.Stream == workerservice.AppSessionStreamEvent {
			kind = "command"
		}
		progress(planner.ProgressEvent{
			Kind: kind,
			Payload: map[string]interface{}{
				"stream": chunk.Stream,
				"seq":    chunk.Seq,
				"text":   string(chunk.Data),
			},
		})
	}
}
