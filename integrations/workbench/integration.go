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
	// engine is used by the memql#722 Library-promotion path to record
	// a v1:library:generatedOutput row after a successful fs_write. Nil
	// on builds / wiring paths that don't inject it (promotion is then a
	// silent no-op). Injected via SetEngine during plug-in materialization.
	engine memql.IntegrationEngineAccess
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

// SetEngine injects the MemQL engine used by the memql#722
// Library-promotion path (a successful fs_write records a
// v1:library:generatedOutput row). Wired during plug-in
// materialization; nil-safe -- when the engine is absent the
// promotion path is a silent no-op so the workbench tool still
// works. Mirrors the SetForwardRouter injection pattern.
func (i *Integration) SetEngine(e memql.IntegrationEngineAccess) {
	i.engine = e
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
	fmt.Fprintf(&b, `mutation createGeneratedOutput(outputId:%q, title:%q, source:%q, format:%q`,
		outputId, title, "workbench_generated", format)
	if attachmentId != "" {
		// File-backed: the bytes ARE the deliverable, so reference the
		// attachment instead of an inline pointer note (mirrors the
		// uploaded-document path).
		fmt.Fprintf(&b, `, attachmentId:%q`, attachmentId)
		if mimeType != "" {
			fmt.Fprintf(&b, `, mimeType:%q`, mimeType)
		}
	} else if inline := i.inlineTextBody(planId, path, format, innerArgs, local); inline != "" {
		// No blob uploader configured (the typical local-dev path): without
		// this the deliverable would be an un-viewable pointer note, so the
		// user "can't see the file in the Library". We already hold the bytes
		// the agent just wrote, so for a text-shaped file inline the content
		// as the generatedOutput body -- the Library DocumentCard renders it
		// directly (markdown / plain), no attachment round-trip needed. (memql#889)
		fmt.Fprintf(&b, `, body:%q`, inline)
		// Carry a real one-line summary derived from the content we already
		// hold, so indexGeneratedOutput stamps a meaningful
		// artifact.summary instead of nothing (memql#1392). Attachment-backed
		// and pointer rows have no text to summarize; they simply omit the
		// field and the index logic resolves it to null.
		if summary := deriveInlineSummary(inline); summary != "" {
			fmt.Fprintf(&b, `, summary:%q`, summary)
		}
	} else {
		// Pointer row: bytes unavailable or non-text, so describe where the file lives.
		fmt.Fprintf(&b, `, body:%q`, fmt.Sprintf("File written to the task workspace at `%s`.", path))
	}
	if agentId = strings.TrimSpace(agentId); agentId != "" {
		fmt.Fprintf(&b, `, producedByAgentId:%q`, agentId)
	}
	fmt.Fprintf(&b, `, producedByPlanId:%q`, planId)
	if partitionId != "" {
		fmt.Fprintf(&b, `, partitionId:%q`, partitionId)
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
	fmt.Fprintf(&b, `mutation mutationCreateAttachment(attachmentId:%q, partitionId:%q, fileName:%q, mimeType:%q, fileSize:%d, blobUrl:%q, status:%q, uploadedBy:%q)`,
		attachmentId, partitionId, fileName, mimeType, len(data), blobUrl, "ready", ownerUserId)
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
	if i.engine == nil {
		return "", ""
	}
	res, err := i.engine.Execute(ctx, fmt.Sprintf(`query planById(planId:%q)`, planId))
	if err != nil || res == nil {
		return "", ""
	}
	for _, row := range outputPayloadRows(res.OutputPayload()) {
		if row == nil {
			continue
		}
		owner := planOwnerFromRow(row)
		space := strings.TrimSpace(stringFromRow(row, "partitionId"))
		if owner != "" {
			return owner, space
		}
	}
	return "", ""
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
