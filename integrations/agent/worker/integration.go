//go:build agent

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"mime"
	"path"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/id"
)

// genOutputIdEngine is the content-id engine used to derive deterministic
// generatedOutput ids. Safe for concurrent use.
var genOutputIdEngine = id.New()

// deriveGeneratedOutputId mints a deterministic v1:library:generatedOutput
// id from the producing (source, ownerUserId, stableKey). Deterministic by
// design: inserting with the same outputId re-versions the same logical
// row (the indexGeneratedOutputOnCreate automation is idempotent), so a
// re-run of the same producing action updates in place instead of minting
// a duplicate. NEVER derive from time -- that would defeat the idempotency
// the Library index relies on. Built on core/id (content-addressed SHA256)
// per the integrations no-raw-sha256 conformance rule.
func deriveGeneratedOutputId(source, ownerUserId, stableKey string) string {
	h := genOutputIdEngine.MustFromMap(map[string]any{
		"source":      source,
		"ownerUserId": ownerUserId,
		"stableKey":   stableKey,
	})
	return "genout-" + string(h)[:16]
}

// pathBasename returns the final path element, suitable for a Library
// title. Uses forward-slash semantics (path, not filepath) so remote
// POSIX paths resolve consistently regardless of the agent host OS.
func pathBasename(p string) string {
	p = strings.TrimRight(strings.TrimSpace(p), "/")
	if p == "" {
		return ""
	}
	return path.Base(p)
}

// formatForWorkerPath maps a computer-use file path to the
// v1:library:generatedOutput.format vocabulary by its extension, so a
// computer-use deliverable lands in the Library with the right viewer
// classification instead of always inheriting the markdown default
// (memql#789). Mirrors the workbench integration's formatForExtension.
// Note: generatedOutput.format has no "conversation" member, so the
// returned value is restricted to that concept's enum.
func formatForWorkerPath(p string) string {
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
		"go", "py", "js", "ts", "sh", "toml", "ini", "conf", "":
		return "text"
	default:
		return "other"
	}
}

// Integration registers the agent-side worker capabilities with the
// MemQL engine. Four capabilities are exposed:
//
//   - dispatchHost     -- workerHost.<action> dispatch entry
//   - dispatchComputer -- workerComputer.<action> dispatch entry
//   - listWorkers      -- list the caller's connected workers
//   - status           -- live three-state availability check
//     (connected / disconnected / unconfigured),
//     matches the agentReply prompt's seed
//
// The first two route through the same Dispatcher; the
// discriminator lives in the args. Calls land here from the agent
// tool loop via @executor("integration.agentworker.X") on the
// builtin functions.
type Integration struct {
	dispatcher *Dispatcher
	registry   *workerservice.Registry
	engine     *memql.MemQLEngine
	logger     *slog.Logger
	// uploader + bucket are the GCS attachment surface (memql#794). When
	// present (injected on the agent node where a GCS bucket is configured),
	// a successful computer-use fs_write uploads the bytes the agent forwarded
	// to the worker so the Library generatedOutput row carries a real
	// attachmentId and the file is downloadable. Without them (or when the
	// written content isn't recoverable agent-side), the row stays a
	// worker-local pointer naming the origin machine (memql#789).
	uploader attachmentUploader
	bucket   string
}

// attachmentUploader is the narrow blob storage upload surface (same shape as the
// workbench integration's). Implemented by the azureblob uploader; injected via
// SetAttachmentUploader from app/transport_agent.go.
type attachmentUploader interface {
	Upload(ctx context.Context, bucket, objectName string, data []byte, contentType string) (string, error)
}

// SetAttachmentUploader injects the blob storage uploader + bucket. Called once from
// app/transport_agent.go after the uploader is constructed, and only when a
// bucket is configured (mirrors the workbench wiring, memql#733/#742). nil-safe:
// without it, computer-use outputs stay worker-local pointer rows.
func (i *Integration) SetAttachmentUploader(u attachmentUploader, bucket string) {
	i.uploader = u
	i.bucket = bucket
}

// planPartitionId resolves a Plan's partitionId from its id (memql#794). The worker
// Request carries no space, but the attachment needs one for the GCS object
// path + the /api/spaces/{partitionId}/attachments download route. Mirrors the
// store's PlanScope lookup. Empty on any miss.
func (i *Integration) planPartitionId(ctx context.Context, planId string) string {
	if i.engine == nil || strings.TrimSpace(planId) == "" {
		return ""
	}
	res, err := i.engine.Execute(ctx, fmt.Sprintf(`query planById(planId:%q)`, planId))
	if err != nil || res == nil || res.Bundle == nil || len(res.Bundle.Nodes) == 0 {
		return ""
	}
	return strings.TrimSpace(stringField(res.Bundle.Nodes[0], "partitionId"))
}

// workerMimeType maps a filename to a MIME type for a computer-use upload.
// Extension-driven via the stdlib table, with a text/plain fallback (the
// common produceArtifact case is markdown/text).
func workerMimeType(fileName string) string {
	if ext := path.Ext(fileName); ext != "" {
		if mt := mime.TypeByExtension(ext); mt != "" {
			return mt
		}
		if strings.EqualFold(ext, ".md") || strings.EqualFold(ext, ".markdown") {
			return "text/markdown"
		}
	}
	return "text/plain; charset=utf-8"
}

// uploadWorkerAttachment uploads the bytes the agent forwarded for a
// computer-use fs_write to GCS and mints a v1:common:attachment row, returning
// its id (memql#794). The bytes are the SAME `content` the agent sent the
// worker, so no worker->agent round-trip and no binary-unsafe fs_read is
// needed (the memql#742 insight, applied to the computer-use path). Returns ""
// on any failure so the caller falls back to a worker-local pointer row.
func (i *Integration) uploadWorkerAttachment(ctx context.Context, planId, filePath, fileName, partitionId, ownerUserId string, data []byte) string {
	if i.uploader == nil || i.bucket == "" || partitionId == "" || len(data) == 0 {
		return ""
	}
	mimeType := workerMimeType(fileName)
	det := string(genOutputIdEngine.MustFromMap(map[string]any{"planId": planId, "path": filePath}))[:16]
	objectName := fmt.Sprintf("spaces/%s/attachments/%s/%s", partitionId, det, fileName)

	blobUrl, err := i.uploader.Upload(ctx, i.bucket, objectName, data, mimeType)
	if err != nil {
		if i.logger != nil {
			i.logger.Warn("worker integration: attachment blob upload failed -- using pointer row",
				"plan_id", planId, "path", filePath, "error", err)
		}
		return ""
	}

	attachmentId := partitionId + ":" + det
	call := fmt.Sprintf(
		`mutation mutationCreateAttachment(attachmentId:%q, partitionId:%q, fileName:%q, mimeType:%q, fileSize:%d, blobUrl:%q, status:%q, uploadedBy:%q)`,
		attachmentId, partitionId, fileName, mimeType, len(data), blobUrl, "ready", ownerUserId)
	if _, err := i.engine.Execute(ctx, call); err != nil {
		if i.logger != nil {
			i.logger.Warn("worker integration: attachment row create failed -- using pointer row",
				"plan_id", planId, "attachment_id", attachmentId, "error", err)
		}
		return ""
	}
	return attachmentId
}

// NewIntegration constructs the integration over an existing
// Dispatcher. Engine is used by the `status` capability to query
// v1:worker:registration rows when no online worker is in the
// registry (the configured-but-offline check). Pass nil if the
// engine isn't available -- status will return "" in that case.
// Logger is used for diagnostic logging in the status handler so
// "Sofia says unconfigured but the pill is green" reports have
// trace data on the agent side; pass nil to suppress.
func NewIntegration(dispatcher *Dispatcher, registry *workerservice.Registry, engine *memql.MemQLEngine, logger *slog.Logger) *Integration {
	return &Integration{dispatcher: dispatcher, registry: registry, engine: engine, logger: logger}
}

// IntegrationName implements memql.IntegrationProvider.
func (i *Integration) IntegrationName() string { return "agentworker" }

// Capabilities implements memql.IntegrationProvider.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "dispatchHost",
			Description: "Dispatch a workerHost.<action> call to the caller's worker. Args: action (string), <action> (object).",
			Handler:     i.handleDispatchHost,
			ArgsSchema: map[string]string{
				"action":        "string (required) -- one of exec / fs_read / fs_write / fs_list / fs_stat / http_fetch",
				"args":          "object (required) -- per-action args",
				"agentId":       "string (required) -- calling agent id",
				"ownerUserId":   "string (required) -- session-owner user id",
				"planId":        "string (optional) -- if invoked from a plan task",
				"taskId":        "string (optional) -- if invoked from a plan task",
				"correlationId": "string (optional) -- audit linkage",
			},
		},
		{
			Name:        "dispatchComputer",
			Description: "Dispatch a workerComputer.<action> call to the caller's computer-use-capable worker.",
			Handler:     i.handleDispatchComputer,
			ArgsSchema: map[string]string{
				"action":        "string (required) -- one of capabilities / wait / screenshot / cursor_position / mouse_move / mouse_click / mouse_down / mouse_up / mouse_drag / mouse_scroll / key_type / key_combo / key_hold / display_info / window_list / window_focus",
				"args":          "object (required)",
				"agentId":       "string (required)",
				"ownerUserId":   "string (required)",
				"planId":        "string (optional)",
				"taskId":        "string (optional)",
				"correlationId": "string (optional)",
			},
		},
		{
			Name:        "listWorkers",
			Description: "List workers connected for the caller. Args: ownerUserId (string).",
			Handler:     i.handleListWorkers,
			ArgsSchema: map[string]string{
				"ownerUserId": "string (required)",
			},
		},
		{
			Name:        "status",
			Description: "Resolve live computer-use availability (connected / disconnected / unconfigured) for the caller's owner. Mirrors the agentReply prompt's seed but pulled fresh at call time.",
			Handler:     i.handleStatus,
			ArgsSchema: map[string]string{
				"ownerUserId": "string (required) -- session-owner user id",
				"agentId":     "string (optional) -- calling agent id (audit only)",
			},
		},
		{
			Name:        "requestScope",
			Description: "Create a scope-elevation Plan + canvas card asking the user to approve a computer_use scope for the upcoming task. Used BEFORE attempting an out-of-scope worker call.",
			Handler:     i.handleRequestScope,
			ArgsSchema: map[string]string{
				"intent":         "string (required) -- short imperative summary",
				"requestedScope": "string (required) -- observe / full (legacy `interact` accepted for backward compat, treated as full)",
				"summary":        "string (required) -- one-paragraph card body",
				"agentId":        "string (required)",
				"ownerUserId":    "string (required)",
				"partitionId":    "string (optional) -- target space for the canvas card",
			},
		},
	}
}

func (i *Integration) handleDispatchHost(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	return i.handleDispatch(ctx, "workerHost", args)
}

func (i *Integration) handleDispatchComputer(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	return i.handleDispatch(ctx, "workerComputer", args)
}

func (i *Integration) handleDispatch(ctx context.Context, tool string, args map[string]any) ([]memorynodes.MemoryNode, error) {
	if i.dispatcher == nil {
		return nil, fmt.Errorf("worker integration: dispatcher not configured")
	}
	action := strings.TrimSpace(asString(args["action"]))
	if action == "" {
		return nil, fmt.Errorf("worker integration: action required")
	}
	innerArgs, _ := args["args"].(map[string]any)
	req := Request{
		Tool:          tool,
		Action:        action,
		Args:          innerArgs,
		AgentId:       strings.TrimSpace(asString(args["agentId"])),
		OwnerUserId:   strings.TrimSpace(asString(args["ownerUserId"])),
		PlanId:        strings.TrimSpace(asString(args["planId"])),
		TaskId:        strings.TrimSpace(asString(args["taskId"])),
		CorrelationId: strings.TrimSpace(asString(args["correlationId"])),
	}
	if req.OwnerUserId == "" || req.AgentId == "" {
		return nil, fmt.Errorf("worker integration: agentId and ownerUserId required")
	}
	res, err := i.dispatcher.Dispatch(ctx, req)
	if err != nil {
		return nil, err
	}

	// Library promotion (memql#722): a successful fs_write through a
	// worker is a standalone deliverable the user should be able to
	// find in their Library. Best-effort -- a promotion failure must
	// never break the producing tool call, so errors are logged and
	// swallowed. Only fs_write produces a durable artifact; exec /
	// fs_read / fs_list / fs_stat / http_fetch are read-only or have
	// no addressable deliverable, so they are skipped.
	if res.OK && action == "fs_write" {
		i.promoteWorkerOutput(ctx, req)
	}

	payload, _ := json.Marshal(map[string]any{
		"ok":            res.OK,
		"output":        rawOrString(res.OutputJSON),
		"errorCode":     res.ErrorCode,
		"errorMessage":  res.ErrorMessage,
		"bytesIn":       res.BytesIn,
		"bytesOut":      res.BytesOut,
		"outputPreview": res.OutputPreview,
	})
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("worker-call:%d", time.Now().UnixNano()),
		Concept:   "integration:worker:result",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}, nil
}

// promoteWorkerOutput records a v1:library:generatedOutput row for a
// file a worker just wrote to the remote host. The worker writes on a
// REMOTE machine, so there are no in-process bytes to attach -- the row
// is a pointer with a human note carrying the written path. Idempotent:
// the outputId is derived deterministically from (ownerUserId, planId,
// path) so a re-run of the same write re-versions the same logical row
// rather than minting duplicates. Best-effort: any error is logged and
// swallowed so a Library failure can't break the worker tool call.
func (i *Integration) promoteWorkerOutput(ctx context.Context, req Request) {
	if i.engine == nil {
		return
	}
	path := strings.TrimSpace(asString(req.Args["path"]))
	if path == "" {
		// No addressable artifact -- nothing meaningful to surface.
		return
	}
	ownerUserId := strings.TrimSpace(req.OwnerUserId)
	if ownerUserId == "" {
		return
	}

	outputId := deriveGeneratedOutputId("computer_use", ownerUserId, req.PlanId+":"+path)
	title := pathBasename(path)
	if title == "" {
		title = path
	}

	// Resolve WHICH of the user's registered machines produced this file
	// (memql#789). The dispatcher routes to a connected worker for the
	// owner; surface that machine's id + name as provenance so the Library
	// can say "Computer use - <machine>" and detect the current machine.
	// For a single connected worker (the common case) this is exact; with
	// several connected we take the first, matching the dispatcher's own
	// owner-scoped resolution.
	var workerId, workerName string
	if i.registry != nil {
		if workers := i.registry.WorkersForUser(ownerUserId); len(workers) > 0 {
			workerId = strings.TrimSpace(workers[0].RegistrationId)
			workerName = strings.TrimSpace(workers[0].Name)
		}
	}

	machineLabel := workerName
	if machineLabel == "" {
		machineLabel = "the connected computer"
	}
	format := formatForWorkerPath(path)

	mutationCtx := withUserActor(ctx, ownerUserId)

	// Make the file DOWNLOADABLE when we can (memql#794): the agent already
	// holds the `content` it forwarded to the worker for this fs_write, so
	// upload those bytes to GCS as a v1:common:attachment and carry the
	// attachmentId on the generatedOutput. No worker->agent round-trip, no
	// binary-unsafe fs_read. Falls back to a worker-local pointer row when the
	// content isn't recoverable agent-side (e.g. a binary write) or no GCS
	// uploader is configured -- in that case the Library still names the origin
	// machine (memql#789) and the frontend shows "lives on <machine>".
	content := asString(req.Args["content"])
	partitionId := i.planPartitionId(mutationCtx, req.PlanId)
	var attachmentId string
	if content != "" {
		attachmentId = i.uploadWorkerAttachment(mutationCtx, req.PlanId, path, title, partitionId, ownerUserId, []byte(content))
	}

	var body string
	if attachmentId != "" {
		body = fmt.Sprintf("File written on %s at `%s` and saved to your Library.", machineLabel, path)
	} else {
		body = fmt.Sprintf("File written to %s at `%s`.", machineLabel, path)
	}

	var b strings.Builder
	fmt.Fprintf(&b, `mutation createGeneratedOutput(outputId:%q, ownerUserId:%q, title:%q, body:%q, source:%q, format:%q`,
		outputId, ownerUserId, title, body, "computer_use", format)
	if req.AgentId != "" {
		fmt.Fprintf(&b, `, producedByAgentId:%q`, req.AgentId)
	}
	if req.PlanId != "" {
		fmt.Fprintf(&b, `, producedByPlanId:%q`, req.PlanId)
	}
	if workerId != "" {
		fmt.Fprintf(&b, `, producedByWorkerId:%q`, workerId)
	}
	if workerName != "" {
		fmt.Fprintf(&b, `, producedByWorkerName:%q`, workerName)
	}
	if partitionId != "" {
		fmt.Fprintf(&b, `, partitionId:%q`, partitionId)
	}
	if attachmentId != "" {
		fmt.Fprintf(&b, `, attachmentId:%q, mimeType:%q`, attachmentId, workerMimeType(title))
	}
	b.WriteString(")")

	if _, err := i.engine.Execute(mutationCtx, b.String()); err != nil {
		if i.logger != nil {
			i.logger.Warn("worker integration: generatedOutput promotion failed",
				"path", path, "owner_user_id", ownerUserId, "error", err)
		}
	}
}

func (i *Integration) handleListWorkers(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i.registry == nil {
		return nil, fmt.Errorf("worker integration: registry not configured")
	}
	owner := strings.TrimSpace(asString(args["ownerUserId"]))
	if owner == "" {
		return nil, fmt.Errorf("worker integration: ownerUserId required")
	}
	out := make([]map[string]any, 0)
	for _, w := range i.registry.WorkersForUser(owner) {
		entry := map[string]any{
			"registrationId": w.RegistrationId,
			"name":           w.Name,
			"capabilities":   w.Capabilities,
			"buildTag":       w.BuildTag,
			"version":        w.Version,
			"connectedAt":    w.ConnectedAt.UTC().Format(time.RFC3339Nano),
			"lastSeenAt":     w.LastSeenAt.UTC().Format(time.RFC3339Nano),
		}
		// Structured per-action capability self-description
		// (memql#1330). Present only when the worker sent one at
		// registration; older workers list without it.
		if w.CapabilityDescriptor != nil {
			entry["capabilityDescriptor"] = w.CapabilityDescriptor.AsMap()
		}
		out = append(out, entry)
	}
	payload, _ := json.Marshal(out)
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("worker-list:%d", time.Now().UnixNano()),
		Concept:   "integration:worker:list",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}, nil
}

// handleStatus mirrors app/computer_use_status_agent.go's resolution
// logic but lives in the integration so the agent can call it as a
// runtime tool. Three-state result:
//
//	"connected"    -- registry has a live worker for the owner
//	"disconnected" -- no online worker, but a non-revoked
//	                  v1:worker:registration row exists
//	"unconfigured" -- no rows at all for the owner
//
// The detail field carries the connected worker's name when
// status="connected" so the agent has a natural-language echo.
func (i *Integration) handleStatus(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i.registry == nil {
		return nil, fmt.Errorf("worker integration: registry not configured")
	}
	owner := strings.TrimSpace(asString(args["ownerUserId"]))
	if owner == "" {
		return nil, fmt.Errorf("worker integration: ownerUserId required")
	}

	status := "unconfigured"
	detail := ""

	// Diagnostic prelude: log the inputs so we can correlate with the
	// registry + DB state when investigating "Sofia says unconfigured
	// but the pill is green" reports. The agent log is the only
	// observability surface that can answer "did handleStatus see the
	// right owner / find the live worker / find the registration row?"
	registryWorkerCount := 0
	if i.registry != nil {
		registryWorkerCount = len(i.registry.WorkersForUser(owner))
	}
	if i.logger != nil {
		i.logger.Info("workerStatus: handleStatus enter",
			"owner_user_id", owner,
			"agent_id", strings.TrimSpace(asString(args["agentId"])),
			"registry_worker_count", registryWorkerCount,
		)
	}

	// Online check: in-memory registry knows currently-streaming
	// cockpits. Mirrors app/computer_use_status_agent.go:61-69.
	if workers := i.registry.WorkersForUser(owner); len(workers) > 0 {
		w := workers[0]
		name := strings.TrimSpace(w.Name)
		if name == "" {
			name = owner
		}
		status = "connected"
		detail = name
	} else if i.engine != nil {
		// Configured-but-offline check: look for non-revoked
		// v1:worker:registration rows. Mirrors
		// app/computer_use_status_agent.go:127-156.
		hasConfigured, err := workerHasConfigured(ctx, i.engine, owner)
		if i.logger != nil {
			i.logger.Info("workerStatus: hasConfigured fallback",
				"owner_user_id", owner,
				"has_configured", hasConfigured,
				"error", err,
			)
		}
		if err == nil && hasConfigured {
			status = "disconnected"
		}
		// On error, fall through to "unconfigured" -- safer
		// guidance than implying the user just needs to start
		// the cockpit.
	}

	if i.logger != nil {
		i.logger.Info("workerStatus: handleStatus result",
			"owner_user_id", owner,
			"status", status,
			"detail", detail,
		)
	}

	payload, _ := json.Marshal(map[string]any{
		"status":     status,
		"detail":     detail,
		"online":     status == "connected",
		"configured": status != "unconfigured",
	})
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("worker-status:%d", time.Now().UnixNano()),
		Concept:   "integration:worker:status",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}, nil
}

// handleRequestScope creates a Plan in awaitingFeedback /
// scope_elevation_required and lets the
// emitScopeElevationCanvasCard automation emit the
// plan.scopeElevationRequested canvas card. The agent gets back a
// small ack payload with the planId so it can reference it in its
// final respondToUser; user input on the card is the actual gate.
func (i *Integration) handleRequestScope(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if i.engine == nil {
		return nil, fmt.Errorf("worker integration: engine not configured")
	}
	intent := strings.TrimSpace(asString(args["intent"]))
	scope := strings.TrimSpace(asString(args["requestedScope"]))
	summary := strings.TrimSpace(asString(args["summary"]))
	agentId := strings.TrimSpace(asString(args["agentId"]))
	ownerUserId := strings.TrimSpace(asString(args["ownerUserId"]))
	partitionId := strings.TrimSpace(asString(args["partitionId"]))
	if intent == "" || scope == "" || summary == "" {
		return nil, fmt.Errorf("worker integration: intent, requestedScope, and summary are all required")
	}
	// `interact` is accepted for backward compat with older agents
	// + persisted Plans, but normalises to `full` so downstream
	// scope-allows comparisons see the simplified two-tier model.
	switch scope {
	case "observe", "full":
		// pass through
	case "interact":
		scope = "full"
	default:
		return nil, fmt.Errorf("worker integration: requestedScope must be observe or full (got %q)", scope)
	}
	if agentId == "" || ownerUserId == "" {
		return nil, fmt.Errorf("worker integration: agentId and ownerUserId required (auto-injection failed)")
	}

	// The mutation runs as the OWNING USER. The agent-tool dispatch
	// path doesn't carry the user's JWT into the per-tool context
	// (cognition forwards AgentGenerateTurnMsg over NodeService and
	// the per-call context is freshly built), so engine.Execute would
	// fail with "no actor found in context" on the insert path. Stamp
	// a synthetic TokenInfo with the user as the actor so the Plan's
	// createdBy lands as the user (correct for ownership / audit) and
	// downstream mutationActor calls succeed. Same pattern automations
	// use via contextWithSystemActor (automations/executor.go) but
	// scoped to a real user instead of a system actor.
	mutationCtx := withUserActor(ctx, ownerUserId)

	planId := fmt.Sprintf("scope-elevation-%d", time.Now().UnixNano())
	q := fmt.Sprintf(
		`mutation createScopeElevationPlan(planId:%q, agentId:%q, ownerUserId:%q, partitionId:%q, intent:%q, summary:%q, requestedScope:%q)`,
		planId, agentId, ownerUserId, partitionId, intent, summary, scope,
	)
	if _, err := i.engine.Execute(mutationCtx, q); err != nil {
		return nil, fmt.Errorf("worker integration: createScopeElevationPlan failed: %w", err)
	}

	payload, _ := json.Marshal(map[string]any{
		"status":         "awaiting_user",
		"planId":         planId,
		"requestedScope": scope,
		"message":        "Scope-elevation request emitted on the canvas. The user will approve or deny; reply with a short acknowledgement and end your turn.",
	})
	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("scope-elevation:%d", time.Now().UnixNano()),
		Concept:   "integration:worker:scopeElevation",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payload,
	}}, nil
}

// workerHasConfigured runs workersForUser and reports whether
// any non-revoked rows exist. Lives in the integration to avoid
// pulling in app-package code from the agent build.
//
// workersForUser uses shape(), which means rows land in
// res.OutputPayload() (Data axis), NOT res.Bundle.Nodes. The earlier
// implementation read Bundle.Nodes only and silently returned false
// for every user -- exactly the symptom Sofia hit when the
// kill-switch pill was green ("Connected: ...") but the agent reported
// "unconfigured and offline." Fix is the same shape() unwrap pattern
// used by app/computer_use_status_agent.go's userHasConfiguredWorker.
func workerHasConfigured(ctx context.Context, engine *memql.MemQLEngine, ownerUserId string) (bool, error) {
	if engine == nil || strings.TrimSpace(ownerUserId) == "" {
		return false, nil
	}
	q := fmt.Sprintf(`query workersForUser(ownerUserId:%q)`, ownerUserId)
	res, err := engine.Execute(ctx, q)
	if err != nil {
		return false, err
	}
	if res == nil {
		return false, nil
	}
	// Shape-query path: walk the projected rows directly.
	for _, row := range outputPayloadRows(res.OutputPayload()) {
		if row == nil {
			continue
		}
		if rev, ok := row["revokedAt"].(string); ok && strings.TrimSpace(rev) != "" {
			continue
		}
		return true, nil
	}
	// Bundle path (legacy / non-shape callers): scan Nodes too so we
	// don't silently miss a future variant that re-introduces the
	// raw bundle return.
	if res.Bundle != nil {
		for _, n := range res.Bundle.Nodes {
			if n == nil || n.Payload == nil {
				continue
			}
			fields := n.Payload.GetFields()
			if fields == nil {
				continue
			}
			if v, ok := fields["revokedAt"]; ok && v != nil {
				if rev := strings.TrimSpace(v.GetStringValue()); rev != "" {
					continue
				}
			}
			return true, nil
		}
	}
	return false, nil
}

// outputPayloadRows normalises a shape() query's OutputPayload into
// a []map[string]any slice. shape() can land as a slice of maps, a
// slice of `any` whose elements are maps, or a bare map (single-row
// projections). Returns nil when the payload doesn't carry rows.
// Mirrors app/computer_use_status_agent.go's helper of the same name
// -- duplicated here so the worker integration stays self-contained
// (no agent-build cross-import on the BFF side).
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

// withUserActor stamps a synthetic TokenInfo on ctx so engine
// mutations attribute their createdBy column to the named user.
// The agent-tool dispatch path doesn't carry the user's JWT
// through to per-tool context (cognition's forwarder builds a
// fresh context for the agent reply), and bare engine.Execute
// calls a mutation handler that requires an actor. Construct the
// minimum claims `mutationActor` reads (sub + email) keyed on the
// resolved owner so the mutation succeeds AND the createdBy
// stamp is meaningful for audit. Mirrors automations/executor.go's
// contextWithSystemActor pattern but scoped to a real user
// instead of a system identifier.
func withUserActor(ctx context.Context, ownerUserId string) context.Context {
	if strings.TrimSpace(ownerUserId) == "" {
		return ctx
	}
	claims := map[string]any{
		"sub":   ownerUserId,
		"email": ownerUserId,
		"role":  "user",
	}
	token := auth.BuildTokenInfo(claims)
	ctx = auth.ContextWithClaims(ctx, claims)
	return auth.ContextWithToken(ctx, token)
}

func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// rawOrString tries to parse s as JSON so the result lands as a
// nested object rather than a quoted string. Falls back to the
// raw string when the parse fails.
func rawOrString(s string) any {
	if s == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err == nil {
		return v
	}
	return s
}
