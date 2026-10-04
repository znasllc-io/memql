//go:build agent || planner

package worker

// Cross-replica model pulls (epic memql#5103, design D3).
//
// The same problem memql#4352 solved for tool dispatch and memql#4676 for
// model calls, arriving a third time for a payload that differs from both: a
// cockpit machine's WorkerService stream terminates on exactly ONE agent
// replica, and the request to pull a model is served wherever the mesh routed
// it. At the default two replicas that is a coin flip.
//
// WHAT MAKES THIS ONE DIFFERENT IS THAT THERE IS NOWHERE TO FALL THROUGH TO.
// A model call that cannot reach a machine can be re-picked onto another one;
// a dispatch can. A pull cannot: a PERSON pressed Pull on ONE machine's page,
// and pulling a model onto a different machine than the one they were looking
// at would be a worse answer than refusing. So this file has no re-pick
// predicate and no `refused_before_start` field -- the outcome is the answer.
//
// The other difference is what is at stake on the receiving side. A model call
// is read-shaped from the machine's point of view: it spends tokens and leaves
// nothing behind. A pull WRITES -- gigabytes onto somebody's disk, and an edit
// to their policy.yaml -- so the receiver's ownership re-check is not merely
// holding a boundary across the hop, it is the last gate before a side effect
// on hardware the cluster does not own.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/node"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/id"
)

// ModelPullOutcome is the terminal answer of a forwarded pull.
type ModelPullOutcome struct {
	Model        string
	Ok           bool
	Readvertised bool
	ErrorCode    string
	ErrorMessage string
}

// modelPullForwardCall is one parked pull: where the answer goes, and where
// the observations go while it waits.
type modelPullForwardCall struct {
	resp       chan *nodev1.ModelPullForwardResponse
	onProgress func(workerservice.ModelPullProgress)
}

// ForwardModelPull sends a pull to the replica holding the machine's stream
// and streams the progress back.
func (r *ForwardRouter) ForwardModelPull(
	ctx context.Context,
	nodeId string,
	registrationId string,
	ownerUserId string,
	model string,
	timeout time.Duration,
	onProgress func(workerservice.ModelPullProgress),
) (ModelPullOutcome, error) {
	if r == nil || r.sender == nil {
		return ModelPullOutcome{Model: model}, ErrNoPeerForNode
	}
	if strings.TrimSpace(model) == "" {
		return ModelPullOutcome{}, errors.New("agent.worker: model pull requires a model")
	}

	// THE MANDATORY ASSERTION (memql#3205), re-asserted from the context
	// rather than rebuilt. Absent, this REFUSES: unlike a model call there is
	// no fall-through to a machine on this replica, so there is nothing to
	// preserve by being lenient and a great deal to lose.
	authority, ok := auth.ForwardedAuthorityFromContext(ctx)
	if !ok {
		r.logger.Warn("model pull forward: no forwarded authority bound to the call context; refusing to forward",
			"registration_id", registrationId, "target_node_id", nodeId)
		return ModelPullOutcome{
			Model:        model,
			ErrorCode:    "no_forwarded_authority",
			ErrorMessage: "forwarding a model pull requires the assertion this node accepted; none is bound to the call context",
		}, nil
	}

	requestId := id.NewShortId()
	call := &modelPullForwardCall{
		resp:       make(chan *nodev1.ModelPullForwardResponse, 1),
		onProgress: onProgress,
	}
	r.modelPullMu.Lock()
	if r.modelPullInflight == nil {
		r.modelPullInflight = make(map[string]*modelPullForwardCall)
	}
	r.modelPullInflight[requestId] = call
	r.modelPullMu.Unlock()
	defer func() {
		r.modelPullMu.Lock()
		delete(r.modelPullInflight, requestId)
		r.modelPullMu.Unlock()
	}()

	// A NON-POSITIVE TIMEOUT CROSSES AS ZERO, so the receiver applies its own
	// default. Clamping to 1 here would be the opposite of what a caller
	// passing 0 means: `ModelPullLimits.withDefaults` establishes 0 as "use the
	// default" everywhere else on this surface, and the receiver's own
	// `timeout <= 0 || timeout > ModelPullTimeoutDefault` accepts 1 verbatim --
	// so a caller following that convention would get a pull killed one second
	// in, with nothing anywhere saying why.
	timeoutSec := int32(timeout / time.Second)
	if timeoutSec < 0 {
		timeoutSec = 0
	}
	env := &nodev1.ModelPullForwardRequest{
		RequestId:      requestId,
		RegistrationId: registrationId,
		OwnerUserId:    ownerUserId,
		Model:          model,
		TimeoutSec:     timeoutSec,
		Authority:      node.ForwardedAuthorityToProto(authority, r.SelfNodeId(), r.SelfNodeType()),
	}
	if !r.sender.Send(nodeId, &nodev1.NodeClientMessage{
		MessageId: id.NewShortId(),
		Payload:   &nodev1.NodeClientMessage_ModelPullForwardRequest{ModelPullForwardRequest: env},
	}) {
		return ModelPullOutcome{Model: model}, ErrNoPeerForNode
	}

	select {
	case resp := <-call.resp:
		return ModelPullOutcome{
			Model:        resp.GetModel(),
			Ok:           resp.GetOk(),
			Readvertised: resp.GetReadvertised(),
			ErrorCode:    resp.GetErrorCode(),
			ErrorMessage: resp.GetErrorMessage(),
		}, nil
	case <-ctx.Done():
		// Best-effort cancel so the machine stops downloading. Whatever it
		// has already fetched stays on disk and is resumed by the next pull
		// of the same model -- a cancel stops the spend of bandwidth, not the
		// existence of bytes.
		r.sendCancel(nodeId, &nodev1.NodeClientMessage{
			MessageId: id.NewShortId(),
			Payload: &nodev1.NodeClientMessage_ModelPullForwardCancel{
				ModelPullForwardCancel: &nodev1.ModelPullForwardCancel{RequestId: requestId, Reason: "caller_cancelled"},
			},
		})
		return ModelPullOutcome{Model: model}, ctx.Err()
	}
}

// DispatchModelPull implements node.WorkerForwardResponseSink for the terminal
// pull answer.
func (r *ForwardRouter) DispatchModelPull(resp *nodev1.ModelPullForwardResponse) {
	if r == nil || resp == nil {
		return
	}
	r.modelPullMu.Lock()
	call, ok := r.modelPullInflight[resp.GetRequestId()]
	r.modelPullMu.Unlock()
	if !ok {
		r.logger.Debug("model pull forward response for an unknown request id",
			"request_id", resp.GetRequestId(), "error_code", resp.GetErrorCode())
		return
	}
	select {
	case call.resp <- resp:
	default:
		r.logger.Warn("model pull forward response dropped (channel full)", "request_id", resp.GetRequestId())
	}
}

// DispatchModelPullProgress implements node.WorkerForwardResponseSink for
// relayed observations.
func (r *ForwardRouter) DispatchModelPullProgress(p *nodev1.ModelPullForwardProgress) {
	if r == nil || p == nil {
		return
	}
	r.modelPullMu.Lock()
	call, ok := r.modelPullInflight[p.GetRequestId()]
	r.modelPullMu.Unlock()
	if !ok || call == nil || call.onProgress == nil {
		return
	}
	call.onProgress(workerservice.ModelPullProgress{
		CompletedBytes: p.GetCompletedBytes(),
		TotalBytes:     p.GetTotalBytes(),
		Status:         p.GetStatus(),
		Layer:          p.GetLayer(),
	})
}

// -----------------------------------------------------------------------------
// The receiving half
// -----------------------------------------------------------------------------

// HandleForwardedModelPull serves an inbound ModelPullForwardRequest.
//
// WHAT IT RE-CHECKS is what HandleForwardedModelCall re-checks -- the machine
// is owned by the VERIFIED principal, never by the envelope's owner hint, and
// it is not revoked -- and here that check is the last thing standing between
// a peer's envelope and gigabytes written to a person's disk.
func (h *ForwardHandler) HandleForwardedModelPull(
	ctx context.Context,
	req *nodev1.ModelPullForwardRequest,
	send func(*nodev1.NodeServerMessage) error,
) {
	requestId := req.GetRequestId()
	model := req.GetModel()
	cctx, cancel := context.WithCancel(ctx)
	h.modelPullMu.Lock()
	if h.modelPullInflight == nil {
		h.modelPullInflight = make(map[string]context.CancelFunc)
	}
	h.modelPullInflight[requestId] = cancel
	h.modelPullMu.Unlock()
	defer func() {
		h.modelPullMu.Lock()
		delete(h.modelPullInflight, requestId)
		h.modelPullMu.Unlock()
		cancel()
	}()

	authority := node.ForwardedAuthorityFromProto(req.GetAuthority())
	access, err := auth.VerifyForwardedAuthority(authority, time.Now())
	if err != nil {
		h.logger.Warn("model pull forward: refused an envelope whose authority did not verify",
			"request_id", requestId, "registration_id", req.GetRegistrationId(), "error", err)
		h.sendPullRefusal(send, requestId, model, "forwarded_authority_refused", err.Error())
		return
	}
	cctx = auth.BindForwardedContext(cctx, authority.Principal().Claims, access, authority)

	owner := strings.TrimSpace(access.UserId)
	if owner == "" {
		h.sendPullRefusal(send, requestId, model, "forwarded_authority_refused",
			"the verified authority names no subject, so there is no owner to check the machine against")
		return
	}
	if hint := strings.TrimSpace(req.GetOwnerUserId()); hint != "" && !sameSubject(hint, owner) {
		h.logger.Warn("model pull forward: envelope owner does not match the verified authority",
			"request_id", requestId, "envelope_owner", hint, "authority_subject", owner)
		h.sendPullRefusal(send, requestId, model, "owner_mismatch",
			"the envelope's owner does not match the verified authority's subject")
		return
	}

	registrationId := req.GetRegistrationId()
	if err := h.verifyRegistration(cctx, owner, registrationId); err != nil {
		h.sendPullRefusal(send, requestId, model, "registration_refused", err.Error())
		return
	}

	w := h.registry.WorkerById(registrationId)
	if w == nil {
		h.sendPullRefusal(send, requestId, model, "worker_disconnected",
			"this replica no longer holds a stream for that machine")
		return
	}
	if w.OwnerUserId != "" && !sameSubject(w.OwnerUserId, owner) {
		h.sendPullRefusal(send, requestId, model, "owner_mismatch",
			"the connected machine is owned by a different user than the assertion names")
		return
	}

	timeout := time.Duration(req.GetTimeoutSec()) * time.Second
	if timeout <= 0 || timeout > workerservice.ModelPullTimeoutDefault {
		timeout = workerservice.ModelPullTimeoutDefault
	}
	pullCtx, pcancel := context.WithTimeout(cctx, timeout)
	defer pcancel()

	// The local pull gets its OWN id, for the reason the model call's does:
	// the answer is mapped back by this envelope's request id, and conflating
	// the two id spaces would make a retry on one side collide with a live
	// pull on the other.
	localId := id.NewShortId()
	handle, err := w.StartModelPull(pullCtx, workerservice.ModelPullRequest{
		RequestId: localId,
		Model:     model,
		Limits:    workerservice.ModelPullLimits{Timeout: timeout},
	})
	if err != nil {
		// StartModelPull fails only before anything reached the machine --
		// a cockpit with no pull support, a blank model, a stream that went
		// away.
		h.sendPullRefusal(send, requestId, model, "model_pull_refused", err.Error())
		return
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for p := range handle.Progress() {
			h.relayPullProgress(send, requestId, model, p)
		}
	}()

	outcome, waitErr := handle.Wait(pullCtx)
	wg.Wait()

	resp := &nodev1.ModelPullForwardResponse{
		RequestId:    requestId,
		Model:        outcome.Model,
		Ok:           waitErr == nil && outcome.Ok,
		Readvertised: outcome.Readvertised,
	}
	if resp.GetModel() == "" {
		resp.Model = model
	}
	switch {
	case waitErr != nil:
		// The machine went away, or the pull hit a ceiling on this side.
		resp.ErrorCode = "model_pull_interrupted"
		resp.ErrorMessage = waitErr.Error()
	case !outcome.Ok:
		// The runtime's own failure, reported in the body of a stream that
		// itself succeeded. Distinct from the case above, because the fixes
		// are different and a person reading the message needs to know which
		// half spoke.
		resp.ErrorCode = "model_pull_failed"
		resp.ErrorMessage = outcome.Error
	}
	h.sendPull(send, resp)
}

// CancelForwardedModelPull stops an in-flight forwarded pull.
func (h *ForwardHandler) CancelForwardedModelPull(_ context.Context, requestId string) {
	h.modelPullMu.Lock()
	cancel, ok := h.modelPullInflight[requestId]
	if ok {
		delete(h.modelPullInflight, requestId)
	}
	h.modelPullMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// relayPullProgress forwards one observation across the hop, VERBATIM.
//
// No folding, no smoothing, no monotonising. The counters are per layer and
// restart at zero for each blob; a hop that tidied them would put a second,
// different progress model on the receiving side from the one every other
// reader sees.
func (h *ForwardHandler) relayPullProgress(
	send func(*nodev1.NodeServerMessage) error,
	requestId string,
	model string,
	p workerservice.ModelPullProgress,
) {
	if err := send(&nodev1.NodeServerMessage{
		MessageId:   id.NewShortId(),
		CorrelateTo: requestId,
		Payload: &nodev1.NodeServerMessage_ModelPullForwardProgress{
			ModelPullForwardProgress: &nodev1.ModelPullForwardProgress{
				RequestId:      requestId,
				Model:          model,
				CompletedBytes: p.CompletedBytes,
				TotalBytes:     p.TotalBytes,
				Status:         p.Status,
				Layer:          p.Layer,
			},
		},
	}); err != nil {
		// An observation that cannot be sent is dropped rather than retried:
		// the end still carries the verdict, and blocking the machine's recv
		// goroutine on a peer connection would be worse than losing a byte
		// count that is superseded a moment later.
		h.logger.Debug("model pull forward: progress send failed", "request_id", requestId, "error", err)
	}
}

func (h *ForwardHandler) sendPullRefusal(send func(*nodev1.NodeServerMessage) error, requestId, model, code, msg string) {
	h.sendPull(send, &nodev1.ModelPullForwardResponse{
		RequestId:    requestId,
		Model:        model,
		ErrorCode:    code,
		ErrorMessage: msg,
	})
}

func (h *ForwardHandler) sendPull(send func(*nodev1.NodeServerMessage) error, resp *nodev1.ModelPullForwardResponse) {
	if err := send(&nodev1.NodeServerMessage{
		MessageId:   id.NewShortId(),
		CorrelateTo: resp.GetRequestId(),
		Payload:     &nodev1.NodeServerMessage_ModelPullForwardResponse{ModelPullForwardResponse: resp},
	}); err != nil {
		h.logger.Warn("model pull forward: response send failed",
			"request_id", resp.GetRequestId(), "error_code", resp.GetErrorCode(), "error", err)
	}
}
