package workbench

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/node"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/core/id"
)

// ForwardHandler is the workbench-node-side bridge that turns an
// inbound WorkbenchForwardRequest into a local Integration dispatch
// and sends the result back as a WorkbenchForwardResponse.
//
// One instance lives on the workbench node (wired in
// app/transport_workbench.go to the NodeServer via
// SetWorkbenchForwardHandler). The Integration it wraps is the
// same one the single-node MVP uses -- the only difference is the
// envelope.
//
// Per-request cancel state is tracked so CancelForwardedRequest (a
// WorkbenchForwardCancel from the originating agent) can stop the work: a
// pipeline step, which runs on its own goroutine for the whole life of its
// Job, and in-flight exec / http_fetch work.
type ForwardHandler struct {
	integration *Integration
	logger      *slog.Logger

	mu       sync.Mutex
	inflight map[string]*inflightCall
	// pipelines answers the pipeline actions. Nil until the app wires a
	// runner, and on a node that cannot run steps; every pipeline action then
	// answers ErrCodePipelinesNotConfigured having run nothing.
	pipelines PipelineRunner
}

// inflightCall is one tracked forward's cancel, held by pointer so a release
// can tell its own entry from a later one under the same request id.
type inflightCall struct {
	cancel context.CancelFunc
}

// The pipeline actions (epic memql#5478, #5493): how the agent's pipeline
// executor drives a CI step through the workbench replica that creates and
// watches its Kubernetes Job. The names are this package's because the
// transport owns its vocabulary; the executor and the runner both use these
// constants rather than a second copy of the strings.
const (
	// PipelineStepAction runs, or adopts, the step's Job to completion and
	// answers with its outcome. LONG: it lasts as long as the Job, so it runs
	// on its own goroutine and never on the stream's receive loop.
	// The version is part of the action: an older replica must reject the
	// request, not silently discard execution/recovery fields it cannot enforce.
	PipelineStepAction = "pipelineStepV2"
	// PipelineStatusAction asks after a step's Job: running, finished (with
	// the outcome), absent or stale. Quick -- the runner bounds it at ten
	// seconds -- and still answered off the receive loop, as are the next two.
	PipelineStatusAction = "pipelineStatus"
	// PipelineAckAction tells the runner its outcome was durably journaled, so the Job
	// and its Secret can be deleted. Quick.
	PipelineAckAction = "pipelineReceiptAck"
	// PipelineCancelAction deletes every Job of a run. Quick.
	PipelineCancelAction = "pipelineCancel"
	// PipelineReadinessAction reads this replica's runner and last isolation
	// verdict. It starts no work and creates no Kubernetes objects.
	PipelineReadinessAction = "pipelineReadiness"
)

// ErrCodePipelinesNotConfigured is the error_code every pipeline action
// answers with on a workbench node that has no PipelineRunner. Nothing ran.
const ErrCodePipelinesNotConfigured = "pipelines_not_configured"

// PipelineRunner runs pipeline steps on this node. JSON in, JSON out: the
// payloads are the runner's own wire (integrations/pipelinesteps), and this
// package carries them verbatim so it never imports the runner -- which
// imports this package for the action names. An adapter in app/ implements it
// over the real runner.
//
// The runner reads a done ctx as a CANCEL (it deletes the step's Job), which is
// why RunStep's ctx is ended only by WorkbenchForwardCancel and never by the
// stream the request arrived on.
type PipelineRunner interface {
	RunStep(ctx context.Context, argsJSON []byte) (outcomeJSON []byte)
	Status(ctx context.Context, argsJSON []byte) (replyJSON []byte, errorCode string)
	Ack(ctx context.Context, argsJSON []byte) (errorCode string)
	CancelRun(ctx context.Context, argsJSON []byte) (replyJSON []byte, errorCode string)
	Readiness(ctx context.Context) (replyJSON []byte, errorCode string)
}

// SetPipelineRunner installs the runner behind the pipeline actions. Left
// nil, every one of them answers ErrCodePipelinesNotConfigured. Safe to call
// while the stream is serving, since the wiring can land after it.
func (h *ForwardHandler) SetPipelineRunner(p PipelineRunner) {
	h.mu.Lock()
	h.pipelines = p
	h.mu.Unlock()
}

func (h *ForwardHandler) pipelineRunner() PipelineRunner {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pipelines
}

// NewForwardHandler wraps an existing Integration for use as the
// workbench node's NodeService.Stream handler. Pass the integration
// instance materialized via memql.RegisterPlugin so the same
// per-process Manager backs both local and forwarded dispatches.
func NewForwardHandler(integ *Integration, logger *slog.Logger) *ForwardHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &ForwardHandler{
		integration: integ,
		logger:      logger,
		inflight:    make(map[string]*inflightCall),
	}
}

// HandleForwardedRequest implements node.WorkbenchForwardHandler.
// Verifies the request's ForwardedAuthority and binds the actor it asserts,
// then calls the local Integration's dispatch path against args reconstructed
// from args_json and sends exactly one WorkbenchForwardResponse via the
// provided callback.
//
// It is called on the peer stream's RECEIVE loop and returns before long work:
// every pipeline action is answered on a goroutine of its own, which sends its
// one response when it ends.
func (h *ForwardHandler) HandleForwardedRequest(ctx context.Context, req *nodev1.WorkbenchForwardRequest, send func(*nodev1.NodeServerMessage) error) {
	requestId := req.GetRequestId()

	// THE GATE (memql#3205 / memql#3219). Refusal is a structured
	// WorkbenchForwardResponse rather than a dropped message, so the agent's
	// parked tool loop unblocks with a reason instead of waiting out its
	// timeout.
	actx, err := h.bindAuthority(ctx, req)
	if err != nil {
		h.logger.Warn("workbench: refused a forwarded request",
			slog.String("requestId", requestId),
			slog.String("action", req.GetAction()),
			slog.String("runId", req.GetRunId()),
			slog.String("error", err.Error()))
		h.sendError(send, requestId, "forwarded_authority_refused", err.Error())
		return
	}

	// THE PIPELINE ENTRIES (epic memql#5478) fork here, beside the build entry
	// and for its reason, and BEFORE the request is tracked below: each one
	// outlives this call, answered on a goroutine of its own, so it owns its
	// tracking and its release, and a deferred release here would forget it
	// the moment the receive loop moved on -- leaving a running step no cancel
	// could reach.
	if isPipelineAction(req.GetAction()) {
		h.handleForwardedPipeline(actx, req, send)
		return
	}

	// Everything below answers before it returns, so it is tracked for exactly
	// that long.
	cctx, release := h.track(actx, requestId)
	defer release()

	// THE BUILD ENTRY (epic memql#4900, task #4901) is a different caller with
	// a different contract, and it forks here rather than inside
	// handleDispatchHost so the two never share a code path: a build's command
	// is the manifest's rather than the allowlist's, its key is a deployment
	// rather than a run, and it writes no workspace row. What makes it
	// checkable is the CLASS inside the assertion this handler just verified:
	// only the engine can mint a SYSTEM-class authority (a node acting for
	// itself, which ForwardedAuthorityForSystem refuses to give a user
	// subject), and every tool-loop forward re-asserts the caller's own class
	// instead. So a `build` arriving under any other class is refused having
	// run nothing.
	if req.GetAction() == BuildAction {
		h.handleForwardedBuild(cctx, req, send)
		return
	}

	innerArgs, err := DecodeArgs(req.GetArgsJson())
	if err != nil {
		h.sendError(send, requestId, "decode_args", err.Error())
		return
	}

	// Reconstruct the args map handleDispatchHost expects (action +
	// runId + args + agentId + stepId). The local path is identical
	// to single-node operation -- same Manager, same workspace dir.
	dispatchArgs := map[string]any{
		"action":  req.GetAction(),
		"runId":   req.GetRunId(),
		"args":    innerArgs,
		"agentId": req.GetAgentId(),
		"stepId":  req.GetStepId(),
	}
	nodes, err := h.integration.handleDispatchHost(cctx, dispatchArgs, 0)
	if err != nil {
		h.sendError(send, requestId, "dispatch_failed", err.Error())
		return
	}
	// handleDispatchHost returns exactly one node with a JSON
	// payload matching dispatchResult shape. Pass that payload
	// through verbatim.
	var payload []byte
	if len(nodes) > 0 {
		payload = nodes[0].Payload
	}
	resp := &nodev1.WorkbenchForwardResponse{
		RequestId:   requestId,
		PayloadJson: payload,
	}
	// Pull error_code / error_message out of the payload so the
	// agent-side router doesn't have to re-parse to know success
	// vs failure. Best-effort: parse failures leave the fields
	// empty and the agent treats the response as success.
	type peek struct {
		OK   bool   `json:"ok"`
		Code string `json:"errorCode"`
		Msg  string `json:"errorMessage"`
	}
	var p peek
	if err := json.Unmarshal(payload, &p); err == nil && !p.OK {
		resp.ErrorCode = p.Code
		resp.ErrorMessage = p.Msg
	}
	if err := send(&nodev1.NodeServerMessage{
		MessageId:   id.NewShortId(),
		CorrelateTo: requestId,
		Payload: &nodev1.NodeServerMessage_WorkbenchForwardResponse{
			WorkbenchForwardResponse: resp,
		},
	}); err != nil {
		h.logger.Warn("workbench forward response send failed",
			"request_id", requestId, "error", err)
	}
}

// handleForwardedBuild answers a `build` forward: verify the class, run the
// build on this node's disk, and send the typed result back.
//
// The class check is the whole gate and it is deliberately NOT a check on the
// subject or the role. A SYSTEM principal is already constrained by
// VerifyForwardedAuthority to RoleReader and to a subject that is not a person,
// so what is left to say here is which ENTRY that principal may reach -- and
// the answer is this one only.
func (h *ForwardHandler) handleForwardedBuild(ctx context.Context, req *nodev1.WorkbenchForwardRequest, send func(*nodev1.NodeServerMessage) error) {
	requestId := req.GetRequestId()
	class := node.ForwardedAuthorityFromProto(req.GetAuthority()).CredentialClass
	if class != auth.ForwardedClassSystem {
		h.logger.Warn("workbench: refused a build forward -- the assertion is not the engine's own",
			slog.String("requestId", requestId),
			slog.String("credentialClass", class))
		h.sendError(send, requestId, BuildCodeEntryRefused,
			"workbench: the build entry answers only to this cluster's own engine, and this request carries a "+
				class+" assertion. Nothing was built.")
		return
	}
	var buildReq BuildRequest
	if err := json.Unmarshal(req.GetArgsJson(), &buildReq); err != nil {
		h.sendError(send, requestId, BuildCodeInvalid, "workbench: the build request could not be read: "+err.Error())
		return
	}
	// runBuildLocal rather than RunBuild: this IS the workbench node, and
	// asking RunBuild would consult the remote flag again and forward the
	// build back out to a peer -- which on a two-replica workbench is a loop
	// with a hop in it.
	res := h.integration.runBuildLocal(ctx, buildReq)
	payload, err := json.Marshal(res)
	if err != nil {
		h.sendError(send, requestId, BuildCodeForwardFailed, "workbench: the build result could not be encoded: "+err.Error())
		return
	}
	resp := &nodev1.WorkbenchForwardResponse{RequestId: requestId, PayloadJson: payload}
	if !res.OK {
		resp.ErrorCode = res.ErrorCode
		resp.ErrorMessage = res.ErrorMessage
	}
	if serr := send(&nodev1.NodeServerMessage{
		MessageId:   id.NewShortId(),
		CorrelateTo: requestId,
		Payload: &nodev1.NodeServerMessage_WorkbenchForwardResponse{
			WorkbenchForwardResponse: resp,
		},
	}); serr != nil {
		h.logger.Warn("workbench build forward response send failed",
			"request_id", requestId, "error", serr)
	}
}

func isPipelineAction(action string) bool {
	switch action {
	case PipelineStepAction, PipelineStatusAction, PipelineAckAction, PipelineCancelAction, PipelineReadinessAction:
		return true
	}
	return false
}

// handleForwardedPipeline answers the pipeline actions.
//
// THE CLASS GATE is the build entry's, for the build entry's reason: the only
// caller of these entries is the engine's own pipeline executor, which forwards
// under a SYSTEM-class assertion, and only the engine can mint one. A person's
// session -- an operator stream or the dev shim included -- reaches none of
// them, because a step's command is a repository's, run with that repository's
// secrets, and an ack or a cancel deletes Jobs. Checked before the runner, so
// what a refused caller learns is the refusal, not this node's configuration.
//
// Each starts on a goroutine of its own and this returns: pipelineStep for
// as long as its Job lasts, the other actions for at most the runner's bound on
// them.
func (h *ForwardHandler) handleForwardedPipeline(ctx context.Context, req *nodev1.WorkbenchForwardRequest, send func(*nodev1.NodeServerMessage) error) {
	requestId := req.GetRequestId()
	action := req.GetAction()
	if class := node.ForwardedAuthorityFromProto(req.GetAuthority()).CredentialClass; class != auth.ForwardedClassSystem {
		h.logger.Warn("workbench: refused a pipeline forward -- the assertion is not the engine's own",
			slog.String("requestId", requestId),
			slog.String("action", action),
			slog.String("runId", req.GetRunId()),
			slog.String("credentialClass", class))
		h.sendError(send, requestId, "forwarded_authority_refused",
			"workbench: the pipeline entries answer only to this cluster's own engine, and this request carries a "+
				class+" assertion. Nothing was run.")
		return
	}
	runner := h.pipelineRunner()
	if runner == nil {
		h.sendError(send, requestId, ErrCodePipelinesNotConfigured,
			"workbench: this node has no pipeline runner (its startup log says why), so it can neither run nor "+
				"inspect a pipeline step. Nothing was run.")
		return
	}
	if action == PipelineStepAction {
		h.startPipelineStep(ctx, runner, req, send)
		return
	}
	h.startPipelineQuick(ctx, runner, req, send)
}

// startPipelineQuick answers a status, an ack or a cancel on a goroutine of
// its own and returns at once (final review, M2).
//
// OFF THE RECEIVE LOOP for the step's reason, at a smaller scale: each reads or
// writes the API server, which the runner bounds at ten seconds
// (quickCallTimeout), and an API server that hangs holds every one of them
// that long. The agent asks after every step it waits on every thirty seconds,
// so answered inline they would keep this loop blocked -- heartbeats, event
// forwards, a running step's cancel and every other forward waiting behind
// them. The reply goes out through the same send, which the node server
// serializes with every other send on the stream (serializeStream).
//
// TRACKED BEFORE IT STARTS, here on the receive loop, as a step is, so a cancel
// read next finds it. It keeps the stream's cancellation, unlike a step: a
// stream that ends leaves nobody to answer, and these are reads or
// idempotent deletes the agent asks for again.
func (h *ForwardHandler) startPipelineQuick(ctx context.Context, runner PipelineRunner, req *nodev1.WorkbenchForwardRequest, send func(*nodev1.NodeServerMessage) error) {
	requestId, action, args := req.GetRequestId(), req.GetAction(), req.GetArgsJson()
	cctx, release := h.track(ctx, requestId)
	go func() {
		var reply []byte
		var code string
		switch action {
		case PipelineReadinessAction:
			reply, code = runner.Readiness(cctx)
		case PipelineStatusAction:
			reply, code = runner.Status(cctx, args)
		case PipelineAckAction:
			code = runner.Ack(cctx, args)
		case PipelineCancelAction:
			reply, code = runner.CancelRun(cctx, args)
		}
		// Released before the reply goes out, as a step's is.
		release()
		h.sendPipelineReply(send, requestId, action, reply, code)
	}()
}

// startPipelineStep starts a step on its own goroutine and returns at once.
//
// ON ITS OWN GOROUTINE, because RunStep does not return until the step's Job
// does -- up to the step's timeout, twenty minutes by default -- and this runs
// on the peer stream's RECEIVE loop. Inline, one step would stop this node
// reading heartbeats, event forwards and every other forward from that peer for
// its whole life, until the liveness checker marked a healthy peer offline. It
// would also make the cancel unreachable: WorkbenchForwardCancel arrives on the
// SAME stream, so the one message that could end the block is the one the block
// keeps us from reading.
//
// TRACKED BEFORE IT STARTS, here on the receive loop rather than inside the
// goroutine. The next message this loop reads can be the cancel for this very
// request, and a registration made inside the goroutine could lose that race
// and leave the cancel nothing to reach.
//
// DETACHED FROM THE STREAM. The step keeps the request's values (the verified
// assertion travels with it) and drops its cancellation, because the runner
// reads a done context as a cancel and deletes the Job. A stream that ends --
// a mesh flap, the agent replica restarting, this replica draining for a
// deploy -- is not a cancel: the Job is meant to survive it and be adopted by
// whichever replica the agent forwards to next. Only WorkbenchForwardCancel
// ends a step.
//
// The reply goes out through the same send when the step ends. If the stream
// it arrived on is gone by then, the send fails and is logged; the runner
// records the outcome on the Job before it returns, which is where the agent's
// next forward or pipelineStatus finds it.
func (h *ForwardHandler) startPipelineStep(ctx context.Context, runner PipelineRunner, req *nodev1.WorkbenchForwardRequest, send func(*nodev1.NodeServerMessage) error) {
	requestId := req.GetRequestId()
	stepCtx, release := h.track(context.WithoutCancel(ctx), requestId)
	args := req.GetArgsJson()
	go func() {
		outcome := runner.RunStep(stepCtx, args)
		// Released before the reply goes out: once the step has answered there
		// is nothing left for a cancel to reach.
		release()
		h.sendPipelineReply(send, requestId, PipelineStepAction, outcome, "")
	}()
}

// sendPipelineReply answers a pipeline action with the runner's JSON verbatim,
// and its refusal code, when it gave one, on error_code -- so the agent reads
// success or failure off the envelope without parsing a payload whose shape
// depends on the action.
func (h *ForwardHandler) sendPipelineReply(send func(*nodev1.NodeServerMessage) error, requestId, action string, payload []byte, code string) {
	resp := &nodev1.WorkbenchForwardResponse{RequestId: requestId, PayloadJson: payload, ErrorCode: code}
	if code != "" {
		resp.ErrorMessage = "workbench: the pipeline runner refused " + action + ": " + code
	}
	if err := send(&nodev1.NodeServerMessage{
		MessageId:   id.NewShortId(),
		CorrelateTo: requestId,
		Payload: &nodev1.NodeServerMessage_WorkbenchForwardResponse{
			WorkbenchForwardResponse: resp,
		},
	}); err != nil {
		h.logger.Warn("workbench pipeline forward reply send failed",
			"request_id", requestId, "action", action, "error", err)
	}
}

// track registers a forward's cancel under its request id, so
// CancelForwardedRequest reaches the work, and returns the context the work
// runs under with the release that forgets it. A path that answers before it
// returns defers the release; a pipeline step's goroutine calls it when the
// step ends.
//
// The release removes only its OWN entry. Two forwards can carry one request
// id -- a caller that re-sends the same envelope after losing a replica -- and
// a release that deleted by id alone would strip the later one's cancel,
// leaving a running step that nothing can stop.
func (h *ForwardHandler) track(parent context.Context, requestId string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	call := &inflightCall{cancel: cancel}
	h.mu.Lock()
	h.inflight[requestId] = call
	h.mu.Unlock()
	return ctx, func() {
		h.mu.Lock()
		if h.inflight[requestId] == call {
			delete(h.inflight, requestId)
		}
		h.mu.Unlock()
		cancel()
	}
}

// bindAuthority verifies the request's assertion and returns the context every
// downstream dispatch runs under.
//
// Nothing is re-resolved: no claims lookup, no userByIdSystem keyed by a
// wire-supplied subject, no fallback. An ABSENT assertion takes the same path
// as a malformed one -- the contract's premise is that absence is never read as
// safe, and the previous shape of this message (a claims map nothing populated
// and nothing read) is exactly what made absence look like the normal case.
//
// The attribution claims are DERIVED from the assertion rather than carried
// beside it. That is what makes "claims without an authority" inexpressible on
// this hop: there is no map on the wire for one to arrive in.
//
// A named method rather than inline steps because the ACCEPT half needs to be
// reachable from a test. A receiver that verifies and then forgets to bind
// leaves worker-side DSL with no actor -- every owned read returning zero rows,
// every write stamping createdBy: "" -- and that failure looks like success
// (memql#2876). Refusal tests cannot reach it; they return first.
func (h *ForwardHandler) bindAuthority(ctx context.Context, req *nodev1.WorkbenchForwardRequest) (context.Context, error) {
	authority := node.ForwardedAuthorityFromProto(req.GetAuthority())
	access, err := auth.VerifyForwardedAuthority(authority, time.Now())
	if err != nil {
		return ctx, err
	}
	return auth.BindForwardedContext(ctx, authority.Principal().Claims, access, authority), nil
}

// CancelForwardedRequest implements node.WorkbenchForwardHandler.
// Cancels the per-request context registered in HandleForwardedRequest
// so in-flight work stops promptly -- for a pipeline step, the runner's
// cue to delete the step's Job.
func (h *ForwardHandler) CancelForwardedRequest(_ context.Context, requestId string) {
	h.mu.Lock()
	call, ok := h.inflight[requestId]
	if ok {
		delete(h.inflight, requestId)
	}
	h.mu.Unlock()
	if call != nil {
		call.cancel()
	}
}

func (h *ForwardHandler) sendError(send func(*nodev1.NodeServerMessage) error, requestId, code, msg string) {
	if err := send(&nodev1.NodeServerMessage{
		MessageId:   id.NewShortId(),
		CorrelateTo: requestId,
		Payload: &nodev1.NodeServerMessage_WorkbenchForwardResponse{
			WorkbenchForwardResponse: &nodev1.WorkbenchForwardResponse{
				RequestId:    requestId,
				ErrorCode:    code,
				ErrorMessage: msg,
			},
		},
	}); err != nil {
		h.logger.Warn("workbench forward error response send failed",
			"request_id", requestId, "error_code", code, "error", err)
	}
}

// Keep package-level references to the few imports that aren't
// reached on every path so go vet stays clean.
var (
	_ = memorynodes.NodeTypeObject
	_ = time.Now
	_ = fmt.Sprintf
)
