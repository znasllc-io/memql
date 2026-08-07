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
// Per-request cancel state is tracked so CancelForwardedRequest
// can stop in-flight exec / http_fetch work when the originating
// agent abandons the call.
type ForwardHandler struct {
	integration *Integration
	logger      *slog.Logger

	mu       sync.Mutex
	inflight map[string]context.CancelFunc
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
		inflight:    make(map[string]context.CancelFunc),
	}
}

// HandleForwardedRequest implements node.WorkbenchForwardHandler.
// Verifies the request's ForwardedAuthority and binds the actor it asserts,
// then calls the local Integration's dispatch path against args reconstructed
// from args_json and sends exactly one WorkbenchForwardResponse via the
// provided callback.
func (h *ForwardHandler) HandleForwardedRequest(ctx context.Context, req *nodev1.WorkbenchForwardRequest, send func(*nodev1.NodeServerMessage) error) {
	requestId := req.GetRequestId()
	cctx, cancel := context.WithCancel(ctx)
	h.mu.Lock()
	h.inflight[requestId] = cancel
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.inflight, requestId)
		h.mu.Unlock()
		cancel()
	}()

	// THE GATE (memql#3205 / memql#3219). Refusal is a structured
	// WorkbenchForwardResponse rather than a dropped message, so the agent's
	// parked tool loop unblocks with a reason instead of waiting out its
	// timeout.
	cctx, err := h.bindAuthority(cctx, req)
	if err != nil {
		h.logger.Warn("workbench: refused a forwarded request",
			slog.String("requestId", requestId),
			slog.String("action", req.GetAction()),
			slog.String("planId", req.GetPlanId()),
			slog.String("error", err.Error()))
		h.sendError(send, requestId, "forwarded_authority_refused", err.Error())
		return
	}

	innerArgs, err := DecodeArgs(req.GetArgsJson())
	if err != nil {
		h.sendError(send, requestId, "decode_args", err.Error())
		return
	}

	// Reconstruct the args map handleDispatchHost expects (action +
	// planId + args + agentId + taskId). The local path is identical
	// to single-node operation -- same Manager, same workspace dir.
	dispatchArgs := map[string]any{
		"action":  req.GetAction(),
		"planId":  req.GetPlanId(),
		"args":    innerArgs,
		"agentId": req.GetAgentId(),
		"taskId":  req.GetTaskId(),
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
// so in-flight exec/http_fetch work stops promptly.
func (h *ForwardHandler) CancelForwardedRequest(_ context.Context, requestId string) {
	h.mu.Lock()
	cancel, ok := h.inflight[requestId]
	if ok {
		delete(h.inflight, requestId)
	}
	h.mu.Unlock()
	if cancel != nil {
		cancel()
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
