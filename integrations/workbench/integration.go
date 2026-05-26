package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/component/safety"
)

// Integration is the workbench IntegrationProvider. It owns the
// per-Plan workspace Manager and exposes one DSL capability
// (`dispatchHost`) that backs the workbenchHost tool's @executor.
//
// Modes:
//   - Single-node (default): the agent node runs this integration
//     and dispatches workbench actions locally against its own disk.
//     This is the MVP path and remains the fallback in cluster mode
//     when no workbench peer is reachable.
//   - Cluster mode (MEMQL_WORKBENCH_REMOTE=1 AND a ForwardRouter is
//     installed): the agent node delegates dispatch to a remote
//     workbench node-type binary via NodeService.Stream. The local
//     Manager is kept for fallback so a transient peer outage
//     doesn't break the tool.
type Integration struct {
	manager *Manager
	logger  *slog.Logger
	router  *ForwardRouter
	remote  bool
}

// NewIntegration constructs the workbench integration with a fresh
// Manager. Logger may be nil; the integration logs at info on
// provisioning and at warn on dispatch errors when present.
func NewIntegration(logger *slog.Logger) *Integration {
	return &Integration{
		manager: NewManager(),
		logger:  logger,
		remote:  remoteEnabled(os.Getenv("MEMQL_WORKBENCH_REMOTE")),
	}
}

// SetForwardRouter installs the cluster-mode forwarder. When set
// AND MEMQL_WORKBENCH_REMOTE is truthy, handleDispatchHost prefers
// the router over local dispatch. Wired during agent-node bootstrap
// (the agent has access to the PeerManager); other node types leave
// this nil.
func (i *Integration) SetForwardRouter(r *ForwardRouter) {
	i.router = r
}

// IntegrationName implements memql.IntegrationProvider.
func (i *Integration) IntegrationName() string { return "workbench" }

// Capabilities implements memql.IntegrationProvider. Two capabilities:
// dispatchHost (the workbenchHost tool surface) and teardownDirectory
// (called by the releaseWorkspaceOnPlanTerminal automation). The
// shared canvasPublish capability is wired by its own integration
// and surfaces in the agent's tool list via the workbench_use slug
// expansion.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "dispatchHost",
			Description: "Dispatch a workbenchHost.<action> call to the per-Plan workbench workspace. Lazily provisions the workspace on first call; subsequent calls in the same Plan see persisted files.",
			Handler:     i.handleDispatchHost,
			ArgsSchema: map[string]string{
				"action":  "string (required) -- exec / fs_read / fs_write / fs_list / fs_stat / http_fetch",
				"args":    "object (required) -- per-action args",
				"planId":  "string (required) -- v1:planner:plan.id; keys the workspace",
				"agentId": "string (optional) -- calling agent id (audit only)",
				"taskId":  "string (optional) -- v1:planner:task.id when invoked from a plan task",
			},
		},
		{
			Name:        "teardownDirectory",
			Description: "Remove the on-disk workbench workspace directory for a Plan. Idempotent: a Plan that never provisioned a workspace is a no-op.",
			Handler:     i.handleTeardownDirectory,
			ArgsSchema: map[string]string{
				"planId": "string (required) -- v1:planner:plan.id whose workspace should be removed",
			},
		},
	}
}

// handleDispatchHost is the single entry point for workbenchHost
// calls. The agent tool loop unpacks the LLM args, fills in planId
// from the dispatch context, and invokes this handler. The shape
// mirrors the worker integration's dispatchHost for prompt-symmetry.
func (i *Integration) handleDispatchHost(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	started := time.Now()
	action, _ := args["action"].(string)
	if strings.TrimSpace(action) == "" {
		return nil, fmt.Errorf("workbench: missing required arg `action`")
	}
	planId, _ := args["planId"].(string)
	if strings.TrimSpace(planId) == "" {
		return nil, fmt.Errorf("workbench: missing required arg `planId`")
	}
	innerArgs, _ := args["args"].(map[string]any)
	if innerArgs == nil {
		innerArgs = map[string]any{}
	}

	// Safety classifier (memql#229). Workbench is sandboxed per-Plan
	// so blast radius is bounded -- fail-OPEN on classifier error.
	// In shadow mode (the default) this is observation-only; the
	// legacy EnforceExecAllowlist in handleExec stays the active
	// block on exec until #235 flips enforce. Runs BEFORE the
	// remote-forward branch so cluster + local paths agree on the
	// audit shape. agentId / taskId arrive in the outer args (the
	// cluster-forward path uses them too), so we surface them in
	// the CallerContext for audit fidelity.
	agentId, _ := args["agentId"].(string)
	taskId, _ := args["taskId"].(string)
	safetyDesc := buildSafetyDescriptor(action, planId, innerArgs)
	safetyDesc.Caller.AgentID = agentId
	safetyDesc.Caller.TaskID = taskId
	workbenchGate := safety.DefaultGate()
	decision, cls, classErr := workbenchGate.Evaluate(ctx, safetyDesc)
	// #235: per-surface fail-closed posture via env override.
	// Workbench is a per-Plan sandbox so default is fail-OPEN; env
	// flip lets ops escalate to fail-closed for a hardening pass.
	failClosed := safety.FailClosedForSurface(safetyDesc.Surface)
	if proceed, reason := workbenchGate.EnforceDecision(safetyDesc.Surface, decision, cls, classErr, failClosed); !proceed {
		return nil, fmt.Errorf("workbench: %s", reason)
	}

	// Cluster mode: try the remote forwarder first. On
	// ErrNoWorkbenchPeer (no healthy workbench peer in the mesh) OR
	// a configured-but-no-router state, fall through to local
	// dispatch so the tool still works in degraded conditions.
	if i.remote && i.router != nil {
		if res, ok := i.tryForward(ctx, planId, action, innerArgs, args, started); ok {
			return res, nil
		}
	}

	ws, err := i.manager.provisionForPlan(planId)
	if err != nil {
		return nil, err
	}

	var res dispatchResult
	switch action {
	case "exec":
		res = i.handleExec(ctx, ws, innerArgs)
	case "fs_read":
		res = i.handleFSRead(ctx, ws, innerArgs)
	case "fs_write":
		res = i.handleFSWrite(ctx, ws, innerArgs)
	case "fs_list":
		res = i.handleFSList(ctx, ws, innerArgs)
	case "fs_stat":
		res = i.handleFSStat(ctx, ws, innerArgs)
	case "http_fetch":
		res = i.handleHTTPFetch(ctx, ws, innerArgs)
	default:
		res = errResult(action, "unknown_action", fmt.Sprintf("workbench: unknown action %q", action))
	}

	// Surface duration in the exec payload for observability; other
	// actions ignore the field. Don't bother for error cases; the
	// tool-loop will see ok=false and surface the error.
	if res.OK && res.Action == "exec" {
		if m, ok := res.Payload.(map[string]any); ok {
			m["durationMs"] = time.Since(started).Milliseconds()
		}
	}

	// memql#233: output-screening pass on http_fetch response body
	// before it lands in the model's context. Fetched HTML/JSON from
	// the open web is the highest-risk ingress vector for prompt
	// injection -- a poisoned page can convert agentic AI into a
	// confused-deputy attack. Shadow mode (the default) just records
	// what would have fired; enforce mode replaces Blocked content
	// with a sanitised stub so the model still sees that SOMETHING
	// happened but can't follow embedded instructions. Per-surface
	// wiring lands incrementally -- this is the demonstration
	// integration; tool_output / file_read / knowledge_seed follow.
	//
	// CRITICAL: gated on res.Action only, NOT res.OK. handleHTTPFetch
	// flips res.OK on the HTTP status code (< 400), but a 4xx/5xx
	// response body still flows to the model when the agent
	// processes the error. An attacker who controls a URL can serve
	// a 403 with a poisoned body and bypass the screener entirely
	// if we gated on res.OK. Screening runs for ALL http_fetch
	// outcomes; only the body-replacement on Blocked depends on
	// having a body to replace.
	if res.Action == "http_fetch" {
		if pl, ok := res.Payload.(map[string]any); ok {
			if body, ok := pl["body"].(string); ok && body != "" {
				outGate := safety.DefaultOutputGate()
				in := safety.ScreeningInput{
					ContentType: safety.ContentTypeHTTPFetch,
					Content:     body,
					Caller: safety.CallerContext{
						AgentID: agentId,
						PlanID:  planId,
						TaskID:  taskId,
					},
				}
				verdict, sr, screenErr := outGate.Screen(ctx, in)
				// #235: apply SuspiciousAsBlocked policy per
				// content type. When the operator sets
				// MEMQL_SAFETY_OUTPUT_HTTP_FETCH_SUSPICIOUS_AS_BLOCKED=true,
				// Suspicious-tier verdicts are escalated to Blocked
				// here so the body gets sanitised. Default is
				// pass-through (opt-in).
				//
				// Fail-open posture on screener error: the gate
				// already returns Clean on error (audit trail
				// captures it via the recorder). For surfaces opted
				// into SuspiciousAsBlocked, we ALSO sanitise on
				// screener-error so a classifier outage can't
				// silently let a poisoned body through on
				// hardened surfaces. Default-surfaces (no opt-in)
				// keep the body intact -- losing the screener
				// shouldn't break workflows that weren't asking
				// for the strict posture.
				if screenErr != nil && safety.SuspiciousAsBlockedForContentType(in.ContentType) {
					pl["body"] = safety.SanitisedReplacement(in,
						safety.ScreeningResult{
							RuleID: "screener.error",
							Reason: "screener errored; sanitising on hardened surface",
						})
					pl["screenedBy"] = "screener.error"
					pl["screenReason"] = screenErr.Error()
				} else if safety.EffectiveVerdict(in.ContentType, verdict) == safety.ScreeningVerdictBlocked {
					pl["body"] = safety.SanitisedReplacement(in, sr)
					pl["screenedBy"] = sr.RuleID
					pl["screenReason"] = sr.Reason
				}
			}
		}
	}

	if i.logger != nil {
		level := slog.LevelInfo
		if !res.OK {
			level = slog.LevelWarn
		}
		i.logger.LogAttrs(ctx, level, "workbench dispatch",
			slog.String("planId", planId),
			slog.String("action", action),
			slog.Bool("ok", res.OK),
			slog.String("errorCode", res.ErrorCode),
			slog.Int64("durationMs", time.Since(started).Milliseconds()),
		)
	}

	payloadBytes, _ := json.Marshal(res)
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("workbench:dispatch:%s:%d", planId, started.UnixNano()),
		Concept:   "integration:workbench:dispatch",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payloadBytes,
	}}, nil
}

// tryForward attempts to dispatch via the remote forwarder. Returns
// (result, true) on success and (nil, false) on ErrNoWorkbenchPeer
// so the caller can fall back to local dispatch transparently. Any
// other error is returned to the agent's tool loop wrapped in an
// errored dispatchResult node so the LLM sees a structured failure
// rather than a tool-loop crash.
func (i *Integration) tryForward(ctx context.Context, planId, action string, innerArgs, allArgs map[string]any, started time.Time) ([]memorynodes.MemoryNode, bool) {
	argsJSON, err := EncodeArgs(innerArgs)
	if err != nil {
		return errorResultNode(planId, action, "encode_args", err.Error(), started), true
	}
	req := &nodev1.WorkbenchForwardRequest{
		PlanId:   planId,
		Action:   action,
		ArgsJson: argsJSON,
		AgentId:  stringArg(allArgs["agentId"], ""),
		TaskId:   stringArg(allArgs["taskId"], ""),
	}
	resp, err := i.router.Forward(ctx, req)
	if errors.Is(err, ErrNoWorkbenchPeer) {
		if i.logger != nil {
			i.logger.Info("workbench: no remote peer available, falling back to local dispatch",
				slog.String("planId", planId), slog.String("action", action))
		}
		return nil, false
	}
	if err != nil {
		return errorResultNode(planId, action, "forward_failed", err.Error(), started), true
	}
	if resp.ErrorCode != "" {
		return errorResultNode(planId, action, resp.ErrorCode, resp.ErrorMessage, started), true
	}
	// Pass the workbench node's payload through verbatim. The shape
	// mirrors dispatchResult so the agent tool loop's downstream
	// formatting works without translation.
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("workbench:dispatch:%s:%d", planId, started.UnixNano()),
		Concept:   "integration:workbench:dispatch",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   resp.PayloadJson,
	}}, true
}

// errorResultNode builds a dispatchResult node carrying an error
// payload. Used for both forward-path errors and the local error
// surface. Keeps the wire shape identical so the agent's tool loop
// formats both kinds the same way.
func errorResultNode(planId, action, code, msg string, started time.Time) []memorynodes.MemoryNode {
	payload, _ := json.Marshal(dispatchResult{
		OK:        false,
		Action:    action,
		ErrorCode: code,
		ErrorMsg:  msg,
	})
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("workbench:dispatch:%s:%d", planId, started.UnixNano()),
		Concept:   "integration:workbench:dispatch",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}
}

// handleTeardownDirectory removes the per-Plan workspace directory.
// Called by the releaseWorkspaceOnPlanTerminal automation; also
// safe to call manually (idempotent). Removes the in-memory cache
// entry and rm -rf's the on-disk directory.
func (i *Integration) handleTeardownDirectory(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	planId, _ := args["planId"].(string)
	if strings.TrimSpace(planId) == "" {
		return nil, fmt.Errorf("workbench: missing required arg `planId`")
	}
	removedBytes, err := i.manager.tearDownForPlan(planId)
	outcome := "removed"
	errMsg := ""
	if err != nil {
		outcome = "failed"
		errMsg = err.Error()
	} else if removedBytes == 0 {
		outcome = "noop"
	}
	if i.logger != nil {
		level := slog.LevelInfo
		if err != nil {
			level = slog.LevelWarn
		}
		i.logger.LogAttrs(ctx, level, "workbench teardown",
			slog.String("planId", planId),
			slog.String("outcome", outcome),
			slog.Int64("removedBytes", removedBytes),
			slog.String("error", errMsg),
		)
	}
	payloadBytes, _ := json.Marshal(map[string]any{
		"planId":       planId,
		"outcome":      outcome,
		"removedBytes": removedBytes,
		"error":        errMsg,
	})
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("workbench:teardown:%s:%d", planId, time.Now().UnixNano()),
		Concept:   "integration:workbench:teardown",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payloadBytes,
	}}, nil
}
