//go:build agent

package worker

// Cross-replica model calls (epic memql#4676, task memql#4677).
//
// The problem is the one memql#4352 solved for tool dispatch, arriving
// again for a different payload: a cockpit machine's WorkerService stream
// terminates on exactly ONE agent replica, and the turn that wants to run a
// model on it is served wherever the mesh routed the request. At the default
// two replicas that is a coin flip, and on the losing side the machine is
// simply not there -- which would present as "no local model available" for a
// laptop the user can see is on, i.e. as the very refusal this epic invented
// to mean something else entirely.
//
// It is deliberately the WorkerForward SHAPE and not a reuse of that message:
// stamp a request id, park on a channel, send to the replica named by
// connectedNodeId, route the answer back by id. What differs is what crosses
// -- a token stream rather than one result -- and one property that matters
// more here than there: this envelope carries the acting user's PROMPTS, so
// the receiver's ownership re-check is the ownership boundary (memql#4678)
// holding across the hop rather than only on the sender.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/node"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/id"
)

// ModelForwardOutcome is the terminal answer of a forwarded model call.
type ModelForwardOutcome struct {
	End                *memqlv1.ModelCallEnd
	ErrorCode          string
	ErrorMessage       string
	RefusedBeforeStart bool
}

// modelForwardCall is one parked model call: where the answer goes, and where
// the deltas go while it waits.
type modelForwardCall struct {
	resp    chan *nodev1.ModelForwardResponse
	onDelta func(*nodev1.ModelForwardDelta)
}

// ForwardModelCall sends a ModelCall to the replica holding the machine's
// stream and streams the answer back.
//
// Every failure before the envelope leaves this node is reported as REFUSED
// BEFORE START, because that is the truth: nothing ran, so the caller may try
// another machine. Every failure after it leaves is reported as having
// possibly run unless the RECEIVER says otherwise. The trade is gentler than
// for an exec -- re-running a generation spends tokens rather than repeating a
// side effect -- but "the same generation twice" is exactly what the loop caps
// and the identical-request breaker (memql#4680) exist to notice, so the rule
// is not relaxed.
func (r *ForwardRouter) ForwardModelCall(
	ctx context.Context,
	nodeId string,
	registrationId string,
	ownerUserId string,
	start *memqlv1.ModelCallStart,
	timeout time.Duration,
	onDelta func(seq uint64, content string),
) (ModelForwardOutcome, error) {
	if r == nil || r.sender == nil {
		return ModelForwardOutcome{RefusedBeforeStart: true}, ErrNoPeerForNode
	}
	if start == nil {
		return ModelForwardOutcome{RefusedBeforeStart: true}, errors.New("agent.worker: model forward requires a ModelCallStart")
	}

	// THE MANDATORY ASSERTION (memql#3205), re-asserted from the context
	// rather than rebuilt, for the reason ForwardDispatch states at length.
	// Absent, this REFUSES BEFORE START so the router can fall through to a
	// machine on this replica -- a working call where a hard failure would be
	// a broken one.
	authority, ok := auth.ForwardedAuthorityFromContext(ctx)
	if !ok {
		r.logger.Warn("model forward: no forwarded authority bound to the call context; refusing to forward",
			"registration_id", registrationId, "target_node_id", nodeId)
		return ModelForwardOutcome{
			ErrorCode:          "no_forwarded_authority",
			ErrorMessage:       "forwarding a model call requires the assertion this node accepted; none is bound to the call context",
			RefusedBeforeStart: true,
		}, nil
	}

	startJSON, err := protojson.Marshal(start)
	if err != nil {
		return ModelForwardOutcome{RefusedBeforeStart: true}, err
	}

	requestId := id.NewShortId()
	call := &modelForwardCall{resp: make(chan *nodev1.ModelForwardResponse, 1)}
	if onDelta != nil {
		call.onDelta = func(d *nodev1.ModelForwardDelta) {
			// A keepalive crossed the hop to prove the machine is alive; it
			// is not output, so it stops here exactly as it stops at the
			// stream session.
			if d.GetKeepalive() {
				return
			}
			onDelta(d.GetSeq(), d.GetContent())
		}
	}
	r.modelMu.Lock()
	if r.modelInflight == nil {
		r.modelInflight = make(map[string]*modelForwardCall)
	}
	r.modelInflight[requestId] = call
	r.modelMu.Unlock()
	defer func() {
		r.modelMu.Lock()
		delete(r.modelInflight, requestId)
		r.modelMu.Unlock()
	}()

	timeoutSec := int32(timeout / time.Second)
	if timeoutSec <= 0 {
		timeoutSec = 1
	}
	env := &nodev1.ModelForwardRequest{
		RequestId:          requestId,
		RegistrationId:     registrationId,
		OwnerUserId:        ownerUserId,
		ModelCallStartJson: startJSON,
		TimeoutSec:         timeoutSec,
		Authority:          node.ForwardedAuthorityToProto(authority, r.SelfNodeId(), r.SelfNodeType()),
	}
	if !r.sender.Send(nodeId, &nodev1.NodeClientMessage{
		MessageId: id.NewShortId(),
		Payload:   &nodev1.NodeClientMessage_ModelForwardRequest{ModelForwardRequest: env},
	}) {
		return ModelForwardOutcome{RefusedBeforeStart: true}, ErrNoPeerForNode
	}

	select {
	case resp := <-call.resp:
		out := ModelForwardOutcome{
			ErrorCode:          resp.GetErrorCode(),
			ErrorMessage:       resp.GetErrorMessage(),
			RefusedBeforeStart: resp.GetRefusedBeforeStart(),
		}
		if raw := resp.GetModelCallEndJson(); len(raw) > 0 {
			end := &memqlv1.ModelCallEnd{}
			if err := protojson.Unmarshal(raw, end); err == nil {
				out.End = end
			} else {
				r.logger.Warn("model forward: undecodable end envelope",
					"request_id", requestId, "error", err)
			}
		}
		return out, nil
	case <-ctx.Done():
		// Best-effort cancel so the machine stops generating.
		r.sender.Send(nodeId, &nodev1.NodeClientMessage{
			MessageId: id.NewShortId(),
			Payload: &nodev1.NodeClientMessage_ModelForwardCancel{
				ModelForwardCancel: &nodev1.ModelForwardCancel{RequestId: requestId, Reason: "caller_cancelled"},
			},
		})
		// NOT refused: the envelope was on the wire, so the generation may
		// well have started.
		return ModelForwardOutcome{}, ctx.Err()
	}
}

// DispatchModel implements node.WorkerForwardResponseSink for the terminal
// model answer.
func (r *ForwardRouter) DispatchModel(resp *nodev1.ModelForwardResponse) {
	if r == nil || resp == nil {
		return
	}
	r.modelMu.Lock()
	call, ok := r.modelInflight[resp.GetRequestId()]
	r.modelMu.Unlock()
	if !ok {
		// A response whose request is gone is the ordinary shape of a call
		// this node timed out or cancelled; it is not an error.
		r.logger.Debug("model forward response for an unknown request id",
			"request_id", resp.GetRequestId(), "error_code", resp.GetErrorCode())
		return
	}
	select {
	case call.resp <- resp:
	default:
		r.logger.Warn("model forward response dropped (channel full)", "request_id", resp.GetRequestId())
	}
}

// DispatchModelDelta implements node.WorkerForwardResponseSink for relayed
// tokens.
func (r *ForwardRouter) DispatchModelDelta(delta *nodev1.ModelForwardDelta) {
	if r == nil || delta == nil {
		return
	}
	r.modelMu.Lock()
	call, ok := r.modelInflight[delta.GetRequestId()]
	r.modelMu.Unlock()
	if !ok || call == nil || call.onDelta == nil {
		return
	}
	call.onDelta(delta)
}

// -----------------------------------------------------------------------------
// The receiving half
// -----------------------------------------------------------------------------

// HandleForwardedModelCall serves an inbound ModelForwardRequest.
//
// WHAT IT RE-CHECKS is exactly what HandleForwardedRequest re-checks and for
// the same reasons: the machine is owned by the VERIFIED principal (never by
// the envelope's owner hint), and it is not revoked. The consent and policy
// gates ran on the sender, and re-running them here would answer the same
// question twice from a worse vantage point.
func (h *ForwardHandler) HandleForwardedModelCall(
	ctx context.Context,
	req *nodev1.ModelForwardRequest,
	send func(*nodev1.NodeServerMessage) error,
) {
	requestId := req.GetRequestId()
	cctx, cancel := context.WithCancel(ctx)
	h.modelMu.Lock()
	if h.modelInflight == nil {
		h.modelInflight = make(map[string]context.CancelFunc)
	}
	h.modelInflight[requestId] = cancel
	h.modelMu.Unlock()
	defer func() {
		h.modelMu.Lock()
		delete(h.modelInflight, requestId)
		h.modelMu.Unlock()
		cancel()
	}()

	authority := node.ForwardedAuthorityFromProto(req.GetAuthority())
	access, err := auth.VerifyForwardedAuthority(authority, time.Now())
	if err != nil {
		h.logger.Warn("model forward: refused an envelope whose authority did not verify",
			"request_id", requestId, "registration_id", req.GetRegistrationId(), "error", err)
		h.sendModelRefusal(send, requestId, "forwarded_authority_refused", err.Error())
		return
	}
	cctx = auth.BindForwardedContext(cctx, authority.Principal().Claims, access, authority)

	owner := strings.TrimSpace(access.UserId)
	if owner == "" {
		h.sendModelRefusal(send, requestId, "forwarded_authority_refused",
			"the verified authority names no subject, so there is no owner to check the machine against")
		return
	}
	if hint := strings.TrimSpace(req.GetOwnerUserId()); hint != "" && !sameSubject(hint, owner) {
		h.logger.Warn("model forward: envelope owner does not match the verified authority",
			"request_id", requestId, "envelope_owner", hint, "authority_subject", owner)
		h.sendModelRefusal(send, requestId, "owner_mismatch",
			"the envelope's owner does not match the verified authority's subject")
		return
	}

	registrationId := req.GetRegistrationId()
	if err := h.verifyRegistration(cctx, owner, registrationId); err != nil {
		h.sendModelRefusal(send, requestId, "registration_refused", err.Error())
		return
	}

	w := h.registry.WorkerById(registrationId)
	if w == nil {
		h.sendModelRefusal(send, requestId, "worker_disconnected",
			"this replica no longer holds a stream for that machine")
		return
	}
	if w.OwnerUserId != "" && !sameSubject(w.OwnerUserId, owner) {
		h.sendModelRefusal(send, requestId, "owner_mismatch",
			"the connected machine is owned by a different user than the assertion names")
		return
	}

	start := &memqlv1.ModelCallStart{}
	if raw := req.GetModelCallStartJson(); len(raw) > 0 {
		if err := protojson.Unmarshal(raw, start); err != nil {
			h.sendModelRefusal(send, requestId, "decode_start", err.Error())
			return
		}
	}

	timeout := time.Duration(req.GetTimeoutSec()) * time.Second
	if timeout <= 0 {
		timeout = workerservice.ModelCallTimeoutDefault
	}
	callCtx, ccancel := context.WithTimeout(cctx, timeout)
	defer ccancel()

	if err := w.Acquire(callCtx, workerservice.ModelCapability); err != nil {
		h.sendModelRefusal(send, requestId, "worker_busy", err.Error())
		return
	}
	defer w.Release(workerservice.ModelCapability)

	// The local call gets its OWN id. The two id spaces never have to agree:
	// the answer is mapped back by this envelope's request id, and conflating
	// them would make a retry on one side collide with a live call on the
	// other.
	localId := id.NewShortId()
	handle, err := w.StartModelCall(callCtx, modelCallRequestFromProto(localId, start, timeout))
	if err != nil {
		// StartModelCall fails only before anything reached the machine --
		// unknown kind, a model this machine does not offer, a stream that
		// went away. All are pre-start.
		h.sendModelRefusal(send, requestId, "model_call_refused", err.Error())
		return
	}

	// FROM HERE ON, NOTHING IS RE-PICKABLE.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for d := range handle.Deltas() {
			h.relayModelDelta(send, requestId, d)
		}
	}()

	outcome, waitErr := handle.Wait(callCtx)
	wg.Wait()

	end := &memqlv1.ModelCallEnd{
		RequestId:    requestId,
		FinishReason: outcome.FinishReason,
		Content:      outcome.Content,
		Error:        outcome.Error,
		ErrorCode:    outcome.ErrorCode,
		Usage: &memqlv1.ModelCallUsage{
			InputTokens:  outcome.Usage.InputTokens,
			OutputTokens: outcome.Usage.OutputTokens,
			Known:        outcome.Usage.Known,
			Model:        outcome.Usage.Model,
		},
	}
	for _, v := range outcome.Embeddings {
		end.Embeddings = append(end.Embeddings, &memqlv1.ModelCallEmbedding{Values: v})
	}
	resp := &nodev1.ModelForwardResponse{
		RequestId: requestId,
		Ok:        waitErr == nil && outcome.Error == "",
	}
	if waitErr != nil {
		resp.ErrorCode = "model_call_failed"
		resp.ErrorMessage = waitErr.Error()
		if end.GetFinishReason() == "" {
			end.FinishReason = workerservice.ModelFinishError
		}
	} else if outcome.Error != "" {
		resp.ErrorCode = outcome.ErrorCode
		resp.ErrorMessage = outcome.Error
	}
	if raw, err := protojson.Marshal(end); err == nil {
		resp.ModelCallEndJson = raw
	} else {
		h.logger.Warn("model forward: could not encode the end envelope",
			"request_id", requestId, "error", err)
	}
	h.sendModel(send, resp)
}

// CancelForwardedModelCall stops an in-flight forwarded model call.
func (h *ForwardHandler) CancelForwardedModelCall(_ context.Context, requestId string) {
	h.modelMu.Lock()
	cancel, ok := h.modelInflight[requestId]
	if ok {
		delete(h.modelInflight, requestId)
	}
	h.modelMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// relayModelDelta forwards one delta across the hop. `seq` rides through
// UNTOUCHED: the de-duplication decision belongs to the originating replica,
// which is the side assembling the text.
func (h *ForwardHandler) relayModelDelta(send func(*nodev1.NodeServerMessage) error, requestId string, d workerservice.ModelCallDelta) {
	if err := send(&nodev1.NodeServerMessage{
		MessageId:   id.NewShortId(),
		CorrelateTo: requestId,
		Payload: &nodev1.NodeServerMessage_ModelForwardDelta{
			ModelForwardDelta: &nodev1.ModelForwardDelta{
				RequestId: requestId,
				Seq:       d.Seq,
				Content:   d.Content,
				Keepalive: d.Keepalive,
			},
		},
	}); err != nil {
		// A delta that cannot be sent is dropped rather than retried: the End
		// still carries the assembled content, and blocking the machine's
		// recv goroutine on a peer connection would be worse than losing a
		// token.
		h.logger.Debug("model forward: delta send failed", "request_id", requestId, "error", err)
	}
}

func (h *ForwardHandler) sendModelRefusal(send func(*nodev1.NodeServerMessage) error, requestId, code, msg string) {
	h.sendModel(send, &nodev1.ModelForwardResponse{
		RequestId:          requestId,
		ErrorCode:          code,
		ErrorMessage:       msg,
		RefusedBeforeStart: true,
	})
}

func (h *ForwardHandler) sendModel(send func(*nodev1.NodeServerMessage) error, resp *nodev1.ModelForwardResponse) {
	if err := send(&nodev1.NodeServerMessage{
		MessageId:   id.NewShortId(),
		CorrelateTo: resp.GetRequestId(),
		Payload:     &nodev1.NodeServerMessage_ModelForwardResponse{ModelForwardResponse: resp},
	}); err != nil {
		h.logger.Warn("model forward: response send failed",
			"request_id", resp.GetRequestId(), "error_code", resp.GetErrorCode(), "error", err)
	}
}

// modelCallRequestFromProto projects the wire envelope onto the worker
// package's request, re-stamping the id and the timeout the receiver enforces.
func modelCallRequestFromProto(localId string, start *memqlv1.ModelCallStart, timeout time.Duration) workerservice.ModelCallRequest {
	req := workerservice.ModelCallRequest{
		RequestId:            localId,
		Model:                start.GetModel(),
		Kind:                 start.GetKind(),
		ResponseFormatSchema: start.GetResponseFormatSchema(),
		EmbeddingInput:       start.GetEmbeddingInput(),
		PlanId:               start.GetPlanId(),
		TaskId:               start.GetTaskId(),
		Purpose:              start.GetPurpose(),
	}
	for _, m := range start.GetMessages() {
		req.Messages = append(req.Messages, workerservice.ModelCallMessage{Role: m.GetRole(), Content: m.GetContent()})
	}
	if p := start.GetParams(); p != nil {
		req.Params = workerservice.ModelCallParams{
			Temperature:     p.GetTemperature(),
			TemperatureSet:  p.GetTemperatureSet(),
			TopP:            p.GetTopP(),
			TopPSet:         p.GetTopPSet(),
			MaxOutputTokens: p.GetMaxOutputTokens(),
			Stop:            p.GetStop(),
			Seed:            p.GetSeed(),
			SeedSet:         p.GetSeedSet(),
		}
	}
	if l := start.GetLimits(); l != nil {
		req.Limits = workerservice.ModelCallLimits{
			Timeout:     time.Duration(l.GetTimeoutSeconds()) * time.Second,
			IdleTimeout: time.Duration(l.GetIdleTimeoutSeconds()) * time.Second,
			Keepalive:   time.Duration(l.GetKeepaliveSeconds()) * time.Second,
		}
	}
	// The receiver's own ceiling wins when it is tighter: a sender that asked
	// for an hour cannot hold this replica's slot for one.
	if req.Limits.Timeout <= 0 || req.Limits.Timeout > timeout {
		req.Limits.Timeout = timeout
	}
	return req
}
