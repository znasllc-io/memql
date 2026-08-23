package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/node"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/component/safety"
	"github.com/znasllc-io/memql/core/id"
)

// Integration is the workbench IntegrationProvider. It owns the
// per-Plan workspace Manager and exposes one DSL capability
// (`dispatchHost`) that backs the workbenchHost tool's @executor.
//
// Modes:
//   - Single-node (default): the agent node runs this integration
//     and dispatches workbench actions locally against its own disk.
//     This is the MVP path.
//   - Cluster mode (MEMQL_WORKBENCH_REMOTE=1 AND a ForwardRouter is
//     installed): the agent node delegates dispatch to a remote
//     workbench node-type binary via NodeService.Stream. When no
//     workbench peer is reachable the call is REFUSED (memql#3506) --
//     the flag is an assertion that the work does not run on the agent,
//     and running it here anyway would invert that assertion in exactly
//     the case that matters. MEMQL_WORKBENCH_LOCAL_FALLBACK=1 restores
//     the old degrade-to-local behaviour for anyone who wants it, as an
//     explicit choice rather than as the consequence of missing config.
type Integration struct {
	manager *Manager
	logger  *slog.Logger
	router  *ForwardRouter
	remote  bool
	// localFallback is the MEMQL_WORKBENCH_LOCAL_FALLBACK opt-in
	// (memql#3506). Only consulted in remote mode, where it restores the
	// old degrade-to-local behaviour for an unreachable workbench. Off by
	// default, which is the entire safety property: a fallback reachable
	// by the ABSENCE of configuration is one that fires precisely when
	// nobody meant it to.
	localFallback bool
	// engine is used by the memql#722 Library-promotion path to record
	// a v1:library:generatedOutput row after a successful fs_write. Nil
	// on builds / wiring paths that don't inject it (promotion is then a
	// silent no-op). Injected via SetEngine during plug-in materialization.
	engine memql.IntegrationEngineAccess
	// store is the v1:workbench:workspace row writer (memql#4354), built over
	// the engine. Held as a field rather than derived per call so a test can
	// inject one: *memql.ExecuteResult cannot be constructed with a shape
	// payload outside component/memql, so a fake ENGINE cannot return rows.
	// Nil until an engine is injected; every store method is a no-op then.
	store *workspaceStore
	// uploader + bucket are the GCS attachment surface (memql#733). When
	// present (injected on the agent node where a GCS bucket is
	// configured), a successful LOCAL fs_write uploads the written bytes
	// to v1:common:attachment and the generatedOutput row carries the
	// resulting attachmentId. Nil on builds / nodes without GCS -- the
	// promotion then falls back to the inline pointer row.
	uploader attachmentUploader
	bucket   string
}

// attachmentUploader is the minimal slice of the blob storage FileUploader the
// workbench needs to push generated-output bytes to object storage
// (memql#733). Declared locally (structural typing) so the workbench
// package doesn't take a dependency on component/server; *azureblob.AzureBlobUploader
// (and server.FileUploader) satisfy it.
type attachmentUploader interface {
	Upload(ctx context.Context, bucket, objectName string, data []byte, contentType string) (string, error)
}

// NewIntegration constructs the workbench integration with a fresh
// Manager. Logger may be nil; the integration logs at info on
// provisioning and at warn on dispatch errors when present.
func NewIntegration(logger *slog.Logger) *Integration {
	return &Integration{
		manager:       NewManager(),
		logger:        logger,
		remote:        remoteEnabled(os.Getenv("MEMQL_WORKBENCH_REMOTE")),
		localFallback: localFallbackEnabled(os.Getenv("MEMQL_WORKBENCH_LOCAL_FALLBACK")),
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

// SetEngine injects the MemQL engine used by the memql#722
// Library-promotion path (a successful fs_write records a
// v1:library:generatedOutput row). Wired during plug-in
// materialization; nil-safe -- when the engine is absent the
// promotion path is a silent no-op so the workbench tool still
// works. Mirrors the SetForwardRouter injection pattern.
func (i *Integration) SetEngine(e memql.IntegrationEngineAccess) {
	i.engine = e
	i.store = newWorkspaceStore(e)
}

// SetAttachmentUploader injects the GCS uploader + bucket used by the
// memql#733 byte-upload path. Wired on the agent node
// (app/transport_agent.go) after the uploader is constructed, and only
// when a bucket is configured. nil-safe: without it, workbench
// generatedOutput rows stay inline pointers (no attachmentId), exactly
// as the #722 behaviour.
func (i *Integration) SetAttachmentUploader(u attachmentUploader, bucket string) {
	i.uploader = u
	i.bucket = bucket
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

	// The environment hint (memql#4353). A CALLER-CONTRACT check, which is why
	// it sits up here with the missing-arg checks rather than down among the
	// dispatch outcomes: nothing has run yet and nothing is going to, on either
	// the local or the forwarded path, so there is no dispatch for the safety
	// classifier below to classify or for a workspace to exist for.
	//
	// See environment.go for what the hint means and, at
	// evaluateEnvironmentHint, for why a mismatch is REFUSED here rather than
	// redirected to the user's own machine.
	hint, hintErr := parseEnvironmentHint(args["environment"])
	if hintErr != nil {
		return errorResultNode(planId, action, ErrCodeInvalidEnvironmentHint,
			"workbench: "+hintErr.Error(), started), nil
	}
	if mismatch := evaluateEnvironmentHint(hint); mismatch != nil {
		if i.logger != nil {
			i.logger.LogAttrs(ctx, slog.LevelInfo, "workbench: refusing dispatch -- environment mismatch",
				slog.String("planId", planId),
				slog.String("action", action),
				slog.String("unmetNeeds", strings.Join(mismatch.UnmetNeeds, ",")),
			)
		}
		return errorResultNodeWithPayload(planId, action, ErrCodeEnvironmentMismatch,
			describeMismatch(*mismatch), *mismatch, started), nil
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

	// Cluster mode. MEMQL_WORKBENCH_REMOTE is an operator asserting
	// THIS WORK DOES NOT RUN ON THE AGENT, so an unreachable workbench
	// is a refusal, not a degrade (memql#3506).
	//
	// This used to fall through to local dispatch on ErrNoWorkbenchPeer
	// or on a configured-but-no-router state, "so the tool still works
	// in degraded conditions". It does not honour the assertion, it
	// inverts it -- and most readily in the case that matters, which is
	// the workbench being unavailable. It is also what made memql#3450
	// invisible for its whole life: the shipped agent.yaml sets both the
	// remote flag and the peer seed, the seed was dropped at parse time,
	// so every workbench call ran on the agent pod with no error, no
	// warning and correct-looking results.
	//
	// A local fallback is still reachable, but only by explicitly asking
	// for it (MEMQL_WORKBENCH_LOCAL_FALLBACK), so "run this remotely"
	// and "run it here if you must" stop being spelled the same way.
	// Note the refusal happens BEFORE provisionForPlan: an error beside
	// a workspace the agent quietly created would be the same bug with
	// better logging.
	if i.remote {
		if i.router != nil {
			if res, ok := i.tryForward(ctx, planId, action, innerArgs, args, started); ok {
				return res, nil
			}
		}
		if !i.localFallback {
			return i.refuseNoWorkbenchPeer(ctx, planId, action, started), nil
		}
		if i.logger != nil {
			i.logger.Warn("workbench: no remote peer; running LOCALLY on the agent node because "+
				"MEMQL_WORKBENCH_LOCAL_FALLBACK is set. This is not the sandbox MEMQL_WORKBENCH_REMOTE asks for.",
				slog.String("planId", planId), slog.String("action", action))
		}
	}

	// The workspace row is written under the plan owner's actor (memql#4354),
	// so the owner is resolved BEFORE the directory exists -- an unattributable
	// workspace the refusal sits beside is the same bug with better logging,
	// which is the standard the memql#3506 refusal above is already held to.
	//
	// Resolved here rather than at the top of the function so an unreachable
	// workbench still reports no_workbench_peer. That is the recurring
	// deployment fault (memql#3450), and answering it with a bookkeeping error
	// instead would cost an operator the one message that names the missing
	// peer seed.
	planOwner, ownerErr := i.workspaceOwner(ctx, planId)
	if ownerErr != nil {
		return i.refuseWorkspaceOwner(ctx, planId, action, ownerErr, started), nil
	}

	ws, err := i.manager.provisionForPlan(planId)
	if err != nil {
		return nil, err
	}

	// THIS node made the directory, so THIS node records where it is.
	// Everything that writes a v1:workbench:workspace row does so from here, on
	// the node whose disk the row describes -- a second writer would be
	// describing a filesystem it cannot see.
	workspaceId, wsErr := i.recordWorkspace(ctx, planId, planOwner, ws.rootPath)
	if wsErr != nil {
		return i.refuseWorkspaceOwner(ctx, planId, action, wsErr, started), nil
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

	// Library promotion (memql#722): a successful fs_write into the
	// per-Plan workspace is a standalone deliverable, so mirror it into
	// the producing user's Library as a generatedOutput. Best-effort:
	// failures are logged inside the helper and never break the tool
	// call. Other actions (exec / fs_read / fs_list / fs_stat /
	// http_fetch) are read-only or have no addressable artifact.
	if res.OK && action == "fs_write" {
		// Local dispatch: the bytes are on this node's disk, so memql#733
		// can upload them (local=true).
		i.promoteWorkbenchOutput(ctx, planId, agentId, innerArgs, true)
	}

	// lastUsedAt is idle-detection telemetry, not a lifecycle gate, so a failed
	// bump is logged and swallowed: refusing a dispatch that already succeeded
	// because a timestamp did not land would trade a real result for a metric.
	// (The row's EXISTENCE is a different matter and is handled above, before
	// anything ran.)
	if res.OK {
		if err := i.workspaces().touch(ctx, planOwner, workspaceId); err != nil && i.logger != nil {
			i.logger.LogAttrs(ctx, slog.LevelWarn, "workbench: touchWorkspace failed",
				slog.String("planId", planId),
				slog.String("workspaceId", workspaceId),
				slog.String("error", err.Error()),
			)
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

// refuseNoWorkbenchPeer is the memql#3506 refusal: remote mode was
// requested and no workbench peer could be reached.
//
// The message names three things because an operator meeting this needs
// all three to act, and the failure it replaces gave them none. WHICH
// peer is missing (workbench), WHERE its address comes from
// (MEMQL_WORKER_PEERS -- the exact knob memql#3450 was silently
// dropping), and the way out if local execution really is what was
// wanted (MEMQL_WORKBENCH_LOCAL_FALLBACK). "workbench unavailable" on
// its own cannot distinguish a crashed pod from a seed the config never
// delivered, and #3450 was precisely the second.
//
// Logged at ERROR: this is a deployment fault, not a tool-call outcome.
func (i *Integration) refuseNoWorkbenchPeer(ctx context.Context, planId, action string, started time.Time) []memorynodes.MemoryNode {
	msg := "workbench: MEMQL_WORKBENCH_REMOTE is set, so this call must run on a workbench node, " +
		"but no healthy workbench peer is reachable. Check that a workbench node is running and that " +
		"MEMQL_WORKER_PEERS names it (e.g. MEMQL_WORKER_PEERS=workbench=workbench:50060). " +
		"Refusing rather than running on the agent node, because running here is not the isolation " +
		"MEMQL_WORKBENCH_REMOTE asks for. To allow local execution when no peer is reachable, set " +
		"MEMQL_WORKBENCH_LOCAL_FALLBACK=1 explicitly."
	if i.logger != nil {
		i.logger.LogAttrs(ctx, slog.LevelError, "workbench: refusing dispatch -- remote required, no peer reachable",
			slog.String("planId", planId),
			slog.String("action", action),
			slog.String("remedy", "MEMQL_WORKER_PEERS=workbench=<addr>"),
		)
	}
	return errorResultNode(planId, action, "no_workbench_peer", msg, started)
}

// tryForward attempts to dispatch via the remote forwarder. Returns
// (result, true) when the call was answered -- successfully or with a
// structured failure -- and (nil, false) on ErrNoWorkbenchPeer, which
// the caller turns into a refusal or (opt-in only) a local run. Any
// other error is returned to the agent's tool loop wrapped in an
// errored dispatchResult node so the LLM sees a structured failure
// rather than a tool-loop crash.
func (i *Integration) tryForward(ctx context.Context, planId, action string, innerArgs, allArgs map[string]any, started time.Time) ([]memorynodes.MemoryNode, bool) {
	argsJSON, err := EncodeArgs(innerArgs)
	if err != nil {
		return errorResultNode(planId, action, "encode_args", err.Error(), started), true
	}
	// The mandatory assertion (memql#3205 / memql#3219). This is a SECOND hop:
	// the agent node is already running inside a forward it accepted, so the
	// assertion is RE-ASSERTED from the ctx rather than rebuilt. Rebuilding it
	// from the AccessContext would be a downgrade -- an AccessContext carries
	// no credential class and no role ceiling, so a BADGE session would reach
	// the workbench as class="user" with no ceiling, and "no badge"
	// indistinguishable from "not stated" is exactly the property memql#3205
	// removed from the AI-forward path.
	//
	// FAIL CLOSED when there is none. In cluster mode every workbench dispatch
	// runs inside a forwarded agent turn, so an absent assertion means the call
	// graph changed rather than that this call is exempt. The alternative is
	// emitting an envelope the workbench node must then decide what to do with,
	// which is how a dead auth carrier gets born in the first place.
	//
	// It returns a structured dispatchResult so the tool loop surfaces it to
	// the LLM rather than crashing, and it does NOT fall back to local dispatch:
	// the per-Plan workspace lives on the workbench node, so running locally
	// would silently operate on a different filesystem.
	authority, ok := auth.ForwardedAuthorityFromContext(ctx)
	if !ok {
		return errorResultNode(planId, action, "no_forwarded_authority",
			"workbench forward requires the assertion this node accepted; none is bound to the call context",
			started), true
	}

	req := &nodev1.WorkbenchForwardRequest{
		PlanId:    planId,
		Action:    action,
		ArgsJson:  argsJSON,
		AgentId:   stringArg(allArgs["agentId"], ""),
		TaskId:    stringArg(allArgs["taskId"], ""),
		Authority: node.ForwardedAuthorityToProto(authority, i.router.SelfNodeId(), i.router.SelfNodeType()),
	}
	// AFFINITY (memql#4354). The plan's live workspace row names the replica
	// whose disk holds its directory; pass that to the picker so the call goes
	// back to the same filesystem it wrote to.
	//
	// A read failure here degrades to "" -- an unpinned pick -- rather than
	// refusing the call. That is the same outcome the pre-#4354 code always
	// had, so a transient read problem cannot be worse than the status quo,
	// and the receiving node still records the substitution.
	planOwner, ownerErr := i.workspaceOwner(ctx, planId)
	if ownerErr != nil {
		// Refused here rather than forwarded: the receiving node runs the same
		// check against the same row and would refuse identically, one hop
		// later and with a workbench slot spent on it.
		return i.refuseWorkspaceOwner(ctx, planId, action, ownerErr, started), true
	}
	pinned := i.pinnedWorkspaceNode(ctx, planId, planOwner)
	resp, servedBy, err := i.router.Forward(ctx, req, pinned)
	if pinned != "" && servedBy != "" && servedBy != pinned && i.logger != nil {
		// The bookkeeping (releasing the orphaned row, provisioning a
		// successor) happens on the node that receives this call, because that
		// is the node that owns the new directory. What belongs HERE is the
		// observation, from the only vantage point that can see both ids at
		// once: this plan's workspace was on a replica that is no longer
		// reachable, and the call has been sent somewhere else.
		//
		// The files are NOT migrated. They were on a node that left the mesh;
		// there is nothing to copy them from. The plan gets a fresh empty
		// directory and the reason is recorded on the released row
		// (releasedReason=node_lost) so "my file vanished" has an answer.
		i.logger.LogAttrs(ctx, slog.LevelWarn,
			"workbench: workspace replica is gone -- dispatching to a different node; the plan gets a FRESH empty workspace and its files are NOT migrated",
			slog.String("planId", planId),
			slog.String("lostNodeId", pinned),
			slog.String("servingNodeId", servedBy),
			slog.String("action", action),
		)
	}
	if errors.Is(err, ErrNoWorkbenchPeer) {
		// Not decided here (memql#3506). The caller owns the choice
		// between refusing and -- on the explicit opt-in only -- running
		// locally, because that is a question about what the OPERATOR
		// asked for rather than about this hop.
		return nil, false
	}
	if err != nil {
		return errorResultNode(planId, action, "forward_failed", err.Error(), started), true
	}
	if resp.ErrorCode != "" {
		return errorResultNode(planId, action, resp.ErrorCode, resp.ErrorMessage, started), true
	}
	// Library promotion for the cluster-mode path: a remote workbench
	// fs_write is just as much a deliverable as a local one. The remote
	// wrote the bytes, but the `content` we forwarded IS those bytes
	// (handleFSWrite persists them verbatim), so promoteWorkbenchOutput
	// uploads them here on the agent node -- no cross-node byte transfer
	// (memql#742). local=false selects that forwarded-content source.
	// Best-effort.
	if action == "fs_write" {
		i.promoteWorkbenchOutput(ctx, planId, stringArg(allArgs["agentId"], ""), innerArgs, false)
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
	return errorResultNodeWithPayload(planId, action, code, msg, nil, started)
}

// errorResultNodeWithPayload is the same node with a structured body attached
// on dispatchResult.Payload -- the field successful results already use, so a
// machine-readable failure needs no second envelope and no change at the seams
// that carry it (the forward response passes payload_json through verbatim).
//
// The environment_mismatch refusal is the caller: an error string a consumer
// has to regex for the unmet needs is a contract that breaks the first time
// somebody improves the wording. See EnvironmentMismatchFromPayload.
func errorResultNodeWithPayload(planId, action, code, msg string, body any, started time.Time) []memorynodes.MemoryNode {
	payload, _ := json.Marshal(dispatchResult{
		OK:        false,
		Action:    action,
		Payload:   body,
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

// ---------------------------------------------------------------------------
// Workspace row bookkeeping (memql#4354)
// ---------------------------------------------------------------------------

// ErrCodeWorkspaceOwnerUnresolved is the dispatchResult.errorCode when the
// parent plan's owner could not be resolved and the workspace row therefore
// cannot be written under an actor.
//
// It REFUSES the dispatch. The alternative -- write the row anyway -- is worse
// than it looks: auth.ContextWithUserActor is a no-op on a blank id, so the
// insert lands with ownerUserId stamped "", and @rowAuthz(owner="ownerUserId")
// then hides that row from everyone including the person whose files it
// describes. The next call reads no row, provisions a second workspace, and the
// split this issue exists to fix comes back wearing a bookkeeping layer.
const ErrCodeWorkspaceOwnerUnresolved = "workspace_owner_unresolved"

// workspaces returns the row store. Falls back to building one from the engine
// so an Integration assembled by struct literal (as several tests do) behaves
// the same as one built through SetEngine. Returns nil when there is no engine,
// which every store method treats as "no persistence layer" rather than as an
// error.
func (i *Integration) workspaces() *workspaceStore {
	if i.store != nil {
		return i.store
	}
	return newWorkspaceStore(i.engine)
}

// workspaceOwner resolves the user whose files the workspace holds: the parent
// plan's requestedBy, which is also the value provisionWorkspace stamps from
// actor.userId.
//
// Returns ("", nil) when no engine is injected. That is the pre-existing MVP
// posture, not a silent failure: with no engine there is no row layer to be
// wrong about, and every store method below is a no-op. It is a different
// situation from an engine that IS present and an owner that is not, which is
// the errNoPlanOwner refusal.
func (i *Integration) workspaceOwner(ctx context.Context, planId string) (string, error) {
	if !i.workspaces().available() {
		return "", nil
	}
	owner, _ := i.resolvePlanOwner(ctx, planId)
	if strings.TrimSpace(owner) == "" {
		return "", errNoPlanOwner
	}
	return owner, nil
}

// refuseWorkspaceOwner turns a workspace-bookkeeping failure into a structured
// tool result. Logged at ERROR because it is a wiring or data fault -- a plan
// with no resolvable owner, or an engine that cannot answer -- rather than
// something the agent did.
func (i *Integration) refuseWorkspaceOwner(ctx context.Context, planId, action string, cause error, started time.Time) []memorynodes.MemoryNode {
	msg := fmt.Sprintf("workbench: %s (planId %s). Check that planId names a v1:planner:plan row this "+
		"caller can read. The workspace row records which replica holds that plan's directory, and a row "+
		"written with no owner is readable by nobody -- including the operator answering \"where did my "+
		"file go\". A workspace keyed on a plan that does not exist also never reaches the "+
		"release-on-plan-terminal automation, so its directory is never reclaimed. Refusing rather than "+
		"provisioning an unattributable workspace.",
		cause.Error(), planId)
	if i.logger != nil {
		i.logger.LogAttrs(ctx, slog.LevelError, "workbench: refusing dispatch -- workspace owner unresolved",
			slog.String("planId", planId),
			slog.String("action", action),
			slog.String("error", cause.Error()),
		)
	}
	return errorResultNode(planId, action, ErrCodeWorkspaceOwnerUnresolved, msg, started)
}

// pinnedWorkspaceNode reports the node id on the plan's live workspace row, or
// "" when there is no row, no engine, or the read failed. Agent-side; feeds the
// forward router's affinity preference.
func (i *Integration) pinnedWorkspaceNode(ctx context.Context, planId, planOwner string) string {
	row, err := i.workspaces().forPlan(ctx, planId, planOwner)
	if err != nil {
		if i.logger != nil {
			i.logger.LogAttrs(ctx, slog.LevelWarn, "workbench: workspace affinity lookup failed; dispatching unpinned",
				slog.String("planId", planId),
				slog.String("error", err.Error()),
			)
		}
		return ""
	}
	if row == nil {
		return ""
	}
	return row.NodeId
}

// recordWorkspace makes the v1:workbench:workspace row agree with the directory
// this node just created, and returns the row id for the post-dispatch touch.
//
// Three cases, and the third is the node-loss transition:
//
//   - No live row: this plan's first call anywhere. Insert one naming this node.
//   - A live row naming this node (or naming nobody, which is what a row written
//     before nodeId existed looks like): adopt it. The common path, every call
//     after the first.
//   - A live row naming a DIFFERENT node: that replica held the directory and
//     the agent's picker only routed here because it could not reach it. Release
//     the orphan with reason=node_lost and insert a successor on this node.
//
// The files are not migrated and cannot be: they were on a disk that is no
// longer in the mesh. The design accepts a fresh empty directory and records
// WHY on the released row, so the question "where did my file go" has an answer
// instead of a plan that silently starts over.
//
// The third case can also be reached without an actual node loss -- if the
// agent's affinity read failed, it dispatches unpinned and may land anywhere.
// The outcome is then the same swap, which is exactly what happened on every
// call before this change; the difference is that it is now recorded rather
// than invisible.
func (i *Integration) recordWorkspace(ctx context.Context, planId, planOwner, storageRoot string) (string, error) {
	store := i.workspaces()
	if !store.available() {
		return "", nil
	}
	self := selfNodeId()
	existing, err := store.forPlan(ctx, planId, planOwner)
	if err != nil {
		return "", err
	}
	if existing != nil && (existing.NodeId == self || existing.NodeId == "") {
		return existing.Id, nil
	}
	if existing != nil {
		if relErr := store.release(ctx, planOwner, existing.Id, releaseReasonNodeLost); relErr != nil {
			return "", relErr
		}
		if i.logger != nil {
			i.logger.LogAttrs(ctx, slog.LevelWarn,
				"workbench: taking over a plan whose workspace replica is gone -- released the orphaned row as node_lost and provisioning a FRESH workspace; files are NOT migrated",
				slog.String("planId", planId),
				slog.String("lostNodeId", existing.NodeId),
				slog.String("servingNodeId", self),
				slog.String("releasedWorkspaceId", existing.Id),
			)
		}
	}
	row := workspaceRow{
		Id:          deriveWorkspaceId(planId, self),
		PlanId:      planId,
		StorageRoot: storageRoot,
		NodeId:      self,
	}
	if err := store.provision(ctx, planOwner, row); err != nil {
		return "", err
	}
	return row.Id, nil
}

// promoteWorkbenchOutput records a v1:library:generatedOutput row for a
// file just written into the per-Plan workspace (memql#722), uploading
// the written bytes to v1:common:attachment so the Library renders /
// downloads the real file (memql#733 local, memql#742 cluster).
//
// ownerUserId / partitionId are resolved from the producing Plan
// (planById -> createdBy / partitionId): the workbench dispatch args
// don't carry the user id, but the Plan's createdBy is server-stamped
// from actor.userId so it is the authoritative owner. Idempotent: the
// outputId is derived deterministically from (ownerUserId, planId,
// path). Best-effort: any failure (engine nil, owner unresolved, byte
// read/upload error, insert error) is logged and swallowed so it can't
// break fs_write; on a byte-upload miss the row falls back to an inline
// pointer (path in the body, no attachmentId).
//
// local selects the byte SOURCE, not whether to upload: for a local
// fs_write the bytes are read back off this node's disk (#733); for the
// forwarded cluster path the bytes are NOT on this node's disk, but the
// `content` we just forwarded to the remote IS exactly what it wrote
// (handleFSWrite persists []byte(content) verbatim), so we still hold
// them here and upload them on THIS node (which owns the GCS uploader +
// engine) -- no bytes cross the wire and the remote needs no changes
// (#742). This sidesteps the binary-unsafe forwarded-fs_read route
// (fs_read coerces bytes to a string that JSON-mangles non-UTF-8).
func (i *Integration) promoteWorkbenchOutput(ctx context.Context, planId, agentId string, innerArgs map[string]any, local bool) {
	if i.engine == nil {
		return
	}
	path := ""
	if innerArgs != nil {
		path, _ = innerArgs["path"].(string)
	}
	path = strings.TrimSpace(path)
	if path == "" || strings.TrimSpace(planId) == "" {
		return
	}

	ownerUserId, partitionId := i.resolvePlanOwner(ctx, planId)
	if strings.TrimSpace(ownerUserId) == "" {
		if i.logger != nil {
			i.logger.Warn("workbench: generatedOutput promotion skipped -- could not resolve plan owner",
				slog.String("planId", planId), slog.String("path", path))
		}
		return
	}

	outputId := deriveGeneratedOutputId("workbench_generated", ownerUserId, planId+":"+path)
	title := pathBasename(path)
	if title == "" {
		title = path
	}
	format := formatForExtension(path)
	mutationCtx := withUserActor(ctx, ownerUserId)

	// When a blob uploader + a target space are available, upload the
	// written bytes to v1:common:attachment and carry the attachmentId +
	// mimeType so the Library renders / downloads the real file. The byte
	// source depends on the dispatch path (see the doc comment): local =
	// read back off disk (#733); cluster = the `content` we forwarded to
	// the remote, which is exactly what it wrote (#742). Any miss falls
	// back to the inline pointer row below; it never breaks the fs_write.
	//
	// A text-shaped deliverable (markdown/text) is deliberately NOT uploaded:
	// it's stored inline on the generatedOutput body below, which the Library
	// renders directly without a download round-trip. That keeps text viewable
	// even where the download route isn't reachable, and avoids regressing the
	// inline path when a blob backend IS configured (e.g. local Azurite).
	// Binary deliverables (pdf/csv/images/...) still go to blob. (memql#888/#889)
	attachmentId, mimeType := "", ""
	if i.uploader != nil && i.bucket != "" && partitionId != "" && !isInlineTextFormat(format) {
		if data := i.workbenchOutputBytes(planId, path, innerArgs, local); len(data) > 0 {
			attachmentId, mimeType = i.uploadAttachmentBytes(mutationCtx, planId, path, title, partitionId, ownerUserId, data)
		}
	}

	// ownerUserId is NOT passed: createGeneratedOutput stamps it from
	// actor.userId (memql#2989), and mutationCtx carries exactly that
	// actor. The blank-owner early return above is load-bearing --
	// withUserActor returns ctx UNCHANGED for a blank owner, which would
	// attribute the row to the inbound caller instead.
	var b strings.Builder
	fmt.Fprintf(&b, `mutation createGeneratedOutput(outputId:%s, title:%s, source:%s, format:%s`,
		langparser.QuoteString(outputId), langparser.QuoteString(title), langparser.QuoteString("workbench_generated"), langparser.QuoteString(format))
	if attachmentId != "" {
		// File-backed: the bytes ARE the deliverable, so reference the
		// attachment instead of an inline pointer note (mirrors the
		// uploaded-document path).
		fmt.Fprintf(&b, `, attachmentId:%s`, langparser.QuoteString(attachmentId))
		if mimeType != "" {
			fmt.Fprintf(&b, `, mimeType:%s`, langparser.QuoteString(mimeType))
		}
	} else if inline := i.inlineTextBody(planId, path, format, innerArgs, local); inline != "" {
		// No blob uploader configured (the typical local-dev path): without
		// this the deliverable would be an un-viewable pointer note, so the
		// user "can't see the file in the Library". We already hold the bytes
		// the agent just wrote, so for a text-shaped file inline the content
		// as the generatedOutput body -- the Library DocumentCard renders it
		// directly (markdown / plain), no attachment round-trip needed. (memql#889)
		fmt.Fprintf(&b, `, body:%s`, langparser.QuoteString(inline))
		// Carry a real one-line summary derived from the content we already
		// hold, so indexGeneratedOutput stamps a meaningful
		// artifact.summary instead of nothing (memql#1392). Attachment-backed
		// and pointer rows have no text to summarize; they simply omit the
		// field and the index logic resolves it to null.
		if summary := deriveInlineSummary(inline); summary != "" {
			fmt.Fprintf(&b, `, summary:%s`, langparser.QuoteString(summary))
		}
	} else {
		// Pointer row: bytes unavailable or non-text, so describe where the file lives.
		fmt.Fprintf(&b, `, body:%s`, langparser.QuoteString(fmt.Sprintf("File written to the task workspace at `%s`.", path)))
	}
	if agentId = strings.TrimSpace(agentId); agentId != "" {
		fmt.Fprintf(&b, `, producedByAgentId:%s`, langparser.QuoteString(agentId))
	}
	fmt.Fprintf(&b, `, producedByPlanId:%s`, langparser.QuoteString(planId))
	if partitionId != "" {
		fmt.Fprintf(&b, `, partitionId:%s`, langparser.QuoteString(partitionId))
	}
	b.WriteString(")")

	if _, err := i.engine.Execute(mutationCtx, b.String()); err != nil {
		if i.logger != nil {
			i.logger.Warn("workbench: generatedOutput promotion failed",
				slog.String("planId", planId), slog.String("path", path),
				slog.String("ownerUserId", ownerUserId), slog.Any("error", err))
		}
	}
}

// isInlineTextFormat reports whether a deliverable of this format (as returned
// by formatForExtension) is stored inline on the generatedOutput body rather
// than uploaded to blob -- the Library renders these directly. (memql#888/#889)
// inlineSummaryMaxRunes bounds the derived artifact summary so the
// Library list stays a glanceable line, not a paragraph (matches the
// agent-streaming producer's summaryMaxLen, memql#1207).
const inlineSummaryMaxRunes = 200

// deriveInlineSummary produces a one-line summary for a workbench
// deliverable from its inline text body, mirroring the agent-streaming
// producer's deriveOutputSummary (memql#1207): the first non-empty line
// (leading markdown `#`s stripped), whitespace-collapsed and truncated
// to inlineSummaryMaxRunes runes with an ellipsis. Returns "" when
// nothing usable exists -- the row then carries no summary and
// indexGeneratedOutput resolves the missing field to null instead
// of leaking the unresolved reference literal (memql#1392).
func deriveInlineSummary(body string) string {
	line := ""
	for _, raw := range strings.Split(body, "\n") {
		l := strings.TrimSpace(raw)
		if l == "" {
			continue
		}
		line = strings.TrimSpace(strings.TrimLeft(l, "#"))
		break
	}
	line = strings.Join(strings.Fields(line), " ")
	if line == "" {
		return ""
	}
	runes := []rune(line)
	if len(runes) > inlineSummaryMaxRunes {
		return strings.TrimSpace(string(runes[:inlineSummaryMaxRunes])) + "…"
	}
	return line
}

func isInlineTextFormat(format string) bool {
	return format == "markdown" || format == "text"
}

// inlineTextBody returns the just-written file's content as a string when it
// is a text-shaped deliverable small enough to store inline, else "". Used on
// the no-blob-uploader path (local dev) so a produced markdown/text file is
// viewable in the Library without an attachment round-trip -- the bytes are
// the same ones workbenchOutputBytes sources for the upload path. (memql#889)
func (i *Integration) inlineTextBody(planId, relPath, format string, innerArgs map[string]any, local bool) string {
	if !isInlineTextFormat(format) {
		return ""
	}
	const maxInlineBytes = 256 << 10 // 256 KiB -- generatedOutput.body, not a blob
	data := i.workbenchOutputBytes(planId, relPath, innerArgs, local)
	if len(data) == 0 || len(data) > maxInlineBytes || !utf8.Valid(data) {
		return ""
	}
	return string(data)
}

// workbenchOutputBytes returns the bytes just written by an fs_write,
// from this node's disk for a local dispatch (#733) or from the `content`
// the agent forwarded to the remote node for the cluster path (#742) --
// handleFSWrite persists []byte(content) verbatim, so the forwarded
// content equals what the remote wrote and is available here without any
// cross-node transfer. Returns nil when the bytes can't be sourced (the
// caller then records an inline pointer row). Best-effort: a disk-read
// failure is logged, not fatal.
func (i *Integration) workbenchOutputBytes(planId, relPath string, innerArgs map[string]any, local bool) []byte {
	if local {
		data, err := i.readWorkspaceFile(planId, relPath)
		if err != nil {
			if i.logger != nil {
				i.logger.Warn("workbench: attachment byte read failed -- using pointer row",
					slog.String("planId", planId), slog.String("path", relPath), slog.Any("error", err))
			}
			return nil
		}
		return data
	}
	// Cluster path: reuse the content we forwarded to the remote node.
	if innerArgs != nil {
		if content, ok := innerArgs["content"].(string); ok && content != "" {
			return []byte(content)
		}
	}
	return nil
}

// uploadAttachmentBytes pushes the given bytes to GCS and creates a
// v1:common:attachment row, returning the attachmentId + detected
// mimeType. Returns ("", "") on any failure, so the caller falls back to
// the inline pointer row. Idempotent: the attachmentId and the GCS object
// name are derived deterministically from (planId, path) so a re-promoted
// fs_write re-versions / overwrites instead of leaking duplicates. Bytes
// are passed in (never JSON-round-tripped), so binary content stays
// intact on both the local and cluster paths.
func (i *Integration) uploadAttachmentBytes(ctx context.Context, planId, relPath, fileName, partitionId, ownerUserId string, data []byte) (attachmentId, mimeType string) {
	if len(data) == 0 {
		return "", ""
	}

	mimeType = detectMimeType(fileName, data)
	det := string(genOutputIdEngine.MustFromMap(map[string]any{"planId": planId, "path": relPath}))[:16]
	objectName := fmt.Sprintf("spaces/%s/attachments/%s/%s", partitionId, det, fileName)

	blobUrl, err := i.uploader.Upload(ctx, i.bucket, objectName, data, mimeType)
	if err != nil {
		if i.logger != nil {
			i.logger.Warn("workbench: attachment blob upload failed -- using pointer row",
				slog.String("planId", planId), slog.String("path", relPath), slog.Any("error", err))
		}
		return "", ""
	}

	attachmentId = partitionId + ":" + det
	var b strings.Builder
	fmt.Fprintf(&b, `mutation mutationCreateAttachment(attachmentId:%s, partitionId:%s, fileName:%s, mimeType:%s, fileSize:%d, blobUrl:%s, status:%s, uploadedBy:%s)`,
		langparser.QuoteString(attachmentId), langparser.QuoteString(partitionId), langparser.QuoteString(fileName), langparser.QuoteString(mimeType), len(data), langparser.QuoteString(blobUrl), langparser.QuoteString("ready"), langparser.QuoteString(ownerUserId))
	if _, err := i.engine.Execute(ctx, b.String()); err != nil {
		if i.logger != nil {
			i.logger.Warn("workbench: attachment row create failed -- using pointer row",
				slog.String("planId", planId), slog.String("attachmentId", attachmentId), slog.Any("error", err))
		}
		return "", ""
	}
	return attachmentId, mimeType
}

// readWorkspaceFile reads a file from the per-Plan workspace, reusing the
// same provisioning + safe-join guard as fs_read so it can't escape the
// workspace root. Capped at maxFSWriteBytes (the write ceiling), so it
// can never read more than fs_write could have produced.
func (i *Integration) readWorkspaceFile(planId, relPath string) ([]byte, error) {
	if i.manager == nil {
		return nil, fmt.Errorf("workbench: manager not configured")
	}
	ws, err := i.manager.provisionForPlan(planId)
	if err != nil {
		return nil, err
	}
	abs, err := ws.safeJoin(relPath)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, maxFSWriteBytes)
	n, err := io.ReadFull(f, buf)
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// detectMimeType picks a MIME type for an uploaded workbench file:
// extension first (mime.TypeByExtension), then content sniffing
// (http.DetectContentType) on the leading bytes, falling back to
// application/octet-stream. Extension wins because sniffing reports a
// generic text/plain for many structured text formats (csv, md, json).
func detectMimeType(fileName string, data []byte) string {
	if ext := path.Ext(fileName); ext != "" {
		if mt := mime.TypeByExtension(ext); mt != "" {
			return mt
		}
	}
	if len(data) > 0 {
		return http.DetectContentType(data)
	}
	return "application/octet-stream"
}

// resolvePlanOwner reads planById and returns the Plan's owner
// user id and partitionId. Returns empty strings on any failure -- the
// caller treats an empty owner as "skip promotion". The planFull shape
// flattens, so rows land in res.OutputPayload() with the fields as
// top-level keys.
//
// Owner = payload.requestedBy (the USER who asked for the deliverable),
// NOT the row-intrinsic createdBy. On the produceArtifact path the Plan
// row is INSERTED by the planner's system actor, so createdBy is
// "system:planner"; using it stamped the promoted v1:library:generatedOutput
// with ownerUserId="system:planner", which made it invisible to the user
// (Library reads gate on ownerUserId==actor.userId) AND made the planner's
// owner-scoped success check (memql#939) find zero rows -> Plan failed even
// though the file was written. requestedBy is faithfully forwarded from the
// originating user, so it's the correct owner. (memql#952)
func (i *Integration) resolvePlanOwner(ctx context.Context, planId string) (ownerUserId, partitionId string) {
	row, err := i.workspaces().planRow(ctx, planId)
	if err != nil || row == nil {
		return "", ""
	}
	owner := planOwnerFromRow(row)
	if owner == "" {
		return "", ""
	}
	return owner, strings.TrimSpace(stringFromRow(row, "partitionId"))
}

// planOwnerFromRow picks the deliverable owner from a planFull row.
// Prefers payload.requestedBy (the user who asked) over the row-intrinsic
// createdBy (the actor that inserted the Plan -- "system:planner" on the
// produceArtifact path). Falls back to createdBy only when requestedBy is
// absent, e.g. a user-initiated workbench call where the inserter IS the
// owner. (memql#952)
func planOwnerFromRow(row map[string]any) string {
	if owner := strings.TrimSpace(stringFromRow(row, "requestedBy")); owner != "" {
		return owner
	}
	return strings.TrimSpace(stringFromRow(row, "createdBy"))
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

// ----- Library-promotion helpers (memql#722) -------------------------------

// genOutputIdEngine is the content-id engine used to derive deterministic
// generatedOutput ids. Safe for concurrent use.
var genOutputIdEngine = id.New()

// deriveGeneratedOutputId mints a deterministic v1:library:generatedOutput
// id from (source, ownerUserId, stableKey). Deterministic by design:
// re-inserting with the same outputId re-versions the same logical row
// (the indexGeneratedOutputOnCreate automation is idempotent), so a
// re-write updates in place instead of minting a duplicate. NEVER derive
// from time. Built on core/id (content-addressed SHA256) per the
// integrations no-raw-sha256 conformance rule.
func deriveGeneratedOutputId(source, ownerUserId, stableKey string) string {
	h := genOutputIdEngine.MustFromMap(map[string]any{
		"source":      source,
		"ownerUserId": ownerUserId,
		"stableKey":   stableKey,
	})
	return "genout-" + string(h)[:16]
}

// pathBasename returns the final path element of a workspace-relative
// path, suitable for a Library title. POSIX forward-slash semantics
// (path, not filepath) so workspace paths resolve consistently.
func pathBasename(p string) string {
	p = strings.TrimRight(strings.TrimSpace(p), "/")
	if p == "" {
		return ""
	}
	return path.Base(p)
}

// formatForExtension maps a filename extension to a Library
// generatedOutput.format enum value (markdown / document / pdf /
// spreadsheet / image / text / other). Defaults to "text" for unknown
// or extension-less files -- a written workspace file is text far more
// often than not, and "text" is a safe, honest fallback.
func formatForExtension(p string) string {
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(pathBasename(p)), "."))
	switch ext {
	case "md", "markdown":
		return "markdown"
	case "pdf":
		return "pdf"
	case "doc", "docx", "odt", "rtf":
		return "document"
	case "csv", "tsv", "xls", "xlsx", "ods":
		return "spreadsheet"
	case "png", "jpg", "jpeg", "gif", "webp", "svg", "bmp", "tiff":
		return "image"
	case "txt", "log", "json", "yaml", "yml", "xml", "html", "htm",
		"go", "py", "js", "ts", "sh", "toml", "ini", "conf":
		return "text"
	case "":
		return "text"
	default:
		return "other"
	}
}

// stringFromRow reads a string field out of a flattened shape() row.
func stringFromRow(row map[string]any, key string) string {
	if row == nil {
		return ""
	}
	if s, ok := row[key].(string); ok {
		return s
	}
	return ""
}

// outputPayloadRows normalises a shape() query's OutputPayload into a
// []map[string]any slice. shape() can land as a slice of maps, a slice
// of `any` whose elements are maps, or a bare map (single-row
// projections). Mirrors the worker integration's helper of the same
// name -- duplicated to keep the workbench package self-contained.
func outputPayloadRows(payload any) []map[string]any {
	if payload == nil {
		return nil
	}
	switch v := payload.(type) {
	case []map[string]any:
		return v
	case []any:
		out := make([]map[string]any, 0, len(v))
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case map[string]any:
		return []map[string]any{v}
	}
	return nil
}

// withUserActor stamps a synthetic TokenInfo on ctx so the
// createGeneratedOutput insert attributes its createdBy column
// to the producing user. The workbench dispatch path doesn't carry the
// user's JWT into the per-call context, and the mutation handler
// requires an actor. Mirrors the worker integration's helper and
// automations/executor.go's contextWithSystemActor pattern, scoped to a
// real user.
func withUserActor(ctx context.Context, ownerUserId string) context.Context {
	return auth.ContextWithUserActor(ctx, ownerUserId)
}
