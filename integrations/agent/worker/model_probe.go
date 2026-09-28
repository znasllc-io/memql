//go:build agent || planner

package worker

// Cross-replica model probes (epic memql#5146, design D3).
//
// The pull's hop, and the same authority rule: the receiver re-checks ownership
// against the VERIFIED assertion and never against the envelope's owner hint.
//
// WHAT THE CHECK IS PROTECTING SITS BETWEEN THE OTHER TWO. A model call is
// read-shaped from the machine's point of view -- it spends tokens and leaves
// nothing behind. A pull WRITES gigabytes onto somebody's disk. A probe writes
// nothing, but it occupies somebody's GPU for minutes, which is a real cost on
// hardware the cluster does not own; and unlike a call it cannot be re-picked,
// because a measurement OF A DIFFERENT MACHINE answers a question nobody asked.
//
// So, like the pull, there is no `refused_before_start` field here and no
// re-pick predicate: the outcome is the answer.

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
	"github.com/znasllc-io/memql/component/worker/probe"
	"github.com/znasllc-io/memql/core/id"
)

// ModelProbeOutcome is the terminal answer of a forwarded probe.
type ModelProbeOutcome struct {
	Model        string
	Ok           bool
	SuiteVersion string
	ErrorCode    string
	ErrorMessage string
	Figures      probe.Figures
}

// modelProbeForwardCall is one parked probe: where the answer goes, and where
// the observations go while it waits.
type modelProbeForwardCall struct {
	resp       chan *nodev1.ModelProbeForwardResponse
	onProgress func(workerservice.ModelProbeProgress)
}

// ForwardModelProbe sends a probe to the replica holding the machine's stream
// and streams the per-case progress back.
func (r *ForwardRouter) ForwardModelProbe(
	ctx context.Context,
	nodeId string,
	registrationId string,
	ownerUserId string,
	model string,
	suiteVersion string,
	timeout time.Duration,
	onProgress func(workerservice.ModelProbeProgress),
) (ModelProbeOutcome, error) {
	if r == nil || r.sender == nil {
		return ModelProbeOutcome{Model: model}, ErrNoPeerForNode
	}
	if strings.TrimSpace(model) == "" {
		return ModelProbeOutcome{}, errors.New("agent.worker: model probe requires a model")
	}

	// THE MANDATORY ASSERTION, re-asserted from the context rather than
	// rebuilt. Absent, this REFUSES: like a pull there is no fall-through to a
	// machine on this replica, so there is nothing to preserve by being lenient.
	authority, ok := auth.ForwardedAuthorityFromContext(ctx)
	if !ok {
		r.logger.Warn("model probe forward: no forwarded authority bound to the call context; refusing to forward",
			"registration_id", registrationId, "target_node_id", nodeId)
		return ModelProbeOutcome{
			Model:        model,
			ErrorCode:    "no_forwarded_authority",
			ErrorMessage: "forwarding a model probe requires the assertion this node accepted; none is bound to the call context",
		}, nil
	}

	requestId := id.NewShortId()
	call := &modelProbeForwardCall{
		resp:       make(chan *nodev1.ModelProbeForwardResponse, 1),
		onProgress: onProgress,
	}
	r.modelProbeMu.Lock()
	if r.modelProbeInflight == nil {
		r.modelProbeInflight = make(map[string]*modelProbeForwardCall)
	}
	r.modelProbeInflight[requestId] = call
	r.modelProbeMu.Unlock()
	defer func() {
		r.modelProbeMu.Lock()
		delete(r.modelProbeInflight, requestId)
		r.modelProbeMu.Unlock()
	}()

	// A NON-POSITIVE TIMEOUT CROSSES AS ZERO so the receiver applies its own
	// default -- the pull's convention, and clamping to 1 here would kill a
	// probe one second in with nothing anywhere saying why.
	timeoutSec := int32(timeout / time.Second)
	if timeoutSec < 0 {
		timeoutSec = 0
	}
	env := &nodev1.ModelProbeForwardRequest{
		RequestId:      requestId,
		RegistrationId: registrationId,
		OwnerUserId:    ownerUserId,
		Model:          model,
		SuiteVersion:   suiteVersion,
		TimeoutSec:     timeoutSec,
		Authority:      node.ForwardedAuthorityToProto(authority, r.SelfNodeId(), r.SelfNodeType()),
	}
	if !r.sender.Send(nodeId, &nodev1.NodeClientMessage{
		MessageId: id.NewShortId(),
		Payload:   &nodev1.NodeClientMessage_ModelProbeForwardRequest{ModelProbeForwardRequest: env},
	}) {
		return ModelProbeOutcome{Model: model}, ErrNoPeerForNode
	}

	select {
	case resp := <-call.resp:
		return ModelProbeOutcome{
			Model:        resp.GetModel(),
			Ok:           resp.GetOk(),
			SuiteVersion: resp.GetSuiteVersion(),
			ErrorCode:    resp.GetErrorCode(),
			ErrorMessage: resp.GetErrorMessage(),
			Figures:      probe.FiguresFromJSON(resp.GetFiguresJson()),
		}, nil
	case <-ctx.Done():
		// Best-effort cancel so the machine stops running the suite. It is
		// somebody's GPU, and unlike a pull there is nothing left behind that a
		// later run resumes -- a cancelled probe simply measured less.
		r.sendCancel(nodeId, &nodev1.NodeClientMessage{
			MessageId: id.NewShortId(),
			Payload: &nodev1.NodeClientMessage_ModelProbeForwardCancel{
				ModelProbeForwardCancel: &nodev1.ModelProbeForwardCancel{RequestId: requestId, Reason: "caller_cancelled"},
			},
		})
		return ModelProbeOutcome{Model: model}, ctx.Err()
	}
}

// DispatchModelProbe delivers the terminal probe answer.
func (r *ForwardRouter) DispatchModelProbe(resp *nodev1.ModelProbeForwardResponse) {
	if r == nil || resp == nil {
		return
	}
	r.modelProbeMu.Lock()
	call, ok := r.modelProbeInflight[resp.GetRequestId()]
	r.modelProbeMu.Unlock()
	if !ok {
		r.logger.Debug("model probe forward response for an unknown request id",
			"request_id", resp.GetRequestId(), "error_code", resp.GetErrorCode())
		return
	}
	select {
	case call.resp <- resp:
	default:
		r.logger.Warn("model probe forward response dropped (channel full)", "request_id", resp.GetRequestId())
	}
}

// DispatchModelProbeProgress delivers one relayed observation.
func (r *ForwardRouter) DispatchModelProbeProgress(p *nodev1.ModelProbeForwardProgress) {
	if r == nil || p == nil {
		return
	}
	r.modelProbeMu.Lock()
	call, ok := r.modelProbeInflight[p.GetRequestId()]
	r.modelProbeMu.Unlock()
	if !ok || call == nil || call.onProgress == nil {
		return
	}
	call.onProgress(workerservice.ModelProbeProgress{
		CaseId:    p.GetCaseId(),
		Completed: p.GetCompletedCases(),
		Total:     p.GetTotalCases(),
		Ok:        p.GetCaseOk(),
		Error:     p.GetCaseError(),
	})
}

// -----------------------------------------------------------------------------
// The receiving half
// -----------------------------------------------------------------------------

// HandleForwardedModelProbe serves an inbound ModelProbeForwardRequest.
//
// It re-checks what HandleForwardedModelPull re-checks -- the machine is owned
// by the VERIFIED principal, never by the envelope's owner hint, and it is not
// revoked -- and the gate is the last thing standing between a peer's envelope
// and minutes of somebody's GPU.
func (h *ForwardHandler) HandleForwardedModelProbe(
	ctx context.Context,
	req *nodev1.ModelProbeForwardRequest,
	send func(*nodev1.NodeServerMessage) error,
) {
	requestId := req.GetRequestId()
	model := req.GetModel()
	cctx, cancel := context.WithCancel(ctx)
	h.modelProbeMu.Lock()
	if h.modelProbeInflight == nil {
		h.modelProbeInflight = make(map[string]context.CancelFunc)
	}
	h.modelProbeInflight[requestId] = cancel
	h.modelProbeMu.Unlock()
	defer func() {
		h.modelProbeMu.Lock()
		delete(h.modelProbeInflight, requestId)
		h.modelProbeMu.Unlock()
		cancel()
	}()

	authority := node.ForwardedAuthorityFromProto(req.GetAuthority())
	access, err := auth.VerifyForwardedAuthority(authority, time.Now())
	if err != nil {
		h.logger.Warn("model probe forward: refused an envelope whose authority did not verify",
			"request_id", requestId, "registration_id", req.GetRegistrationId(), "error", err)
		h.sendProbeRefusal(send, requestId, model, "forwarded_authority_refused", err.Error())
		return
	}
	cctx = auth.BindForwardedContext(cctx, authority.Principal().Claims, access, authority)

	owner := strings.TrimSpace(access.UserId)
	if owner == "" {
		h.sendProbeRefusal(send, requestId, model, "forwarded_authority_refused",
			"the verified authority names no subject, so there is no owner to check the machine against")
		return
	}
	if hint := strings.TrimSpace(req.GetOwnerUserId()); hint != "" && !sameSubject(hint, owner) {
		h.logger.Warn("model probe forward: envelope owner does not match the verified authority",
			"request_id", requestId, "envelope_owner", hint, "authority_subject", owner)
		h.sendProbeRefusal(send, requestId, model, "owner_mismatch",
			"the envelope's owner does not match the verified authority's subject")
		return
	}

	registrationId := req.GetRegistrationId()
	if err := h.verifyRegistration(cctx, owner, registrationId); err != nil {
		h.sendProbeRefusal(send, requestId, model, "registration_refused", err.Error())
		return
	}

	w := h.registry.WorkerById(registrationId)
	if w == nil {
		h.sendProbeRefusal(send, requestId, model, "worker_disconnected",
			"this replica no longer holds a stream for that machine")
		return
	}
	if w.OwnerUserId != "" && !sameSubject(w.OwnerUserId, owner) {
		h.sendProbeRefusal(send, requestId, model, "owner_mismatch",
			"the connected machine is owned by a different user than the assertion names")
		return
	}

	timeout := time.Duration(req.GetTimeoutSec()) * time.Second
	if timeout <= 0 || timeout > workerservice.DefaultModelProbeTimeout {
		timeout = workerservice.DefaultModelProbeTimeout
	}
	probeCtx, pcancel := context.WithTimeout(cctx, timeout)
	defer pcancel()

	// The local probe gets its OWN id, for the pull's reason: the answer is
	// mapped back by this envelope's request id, and conflating the two id
	// spaces would make a retry on one side collide with a live probe on the
	// other.
	localId := id.NewShortId()
	handle, err := w.StartModelProbe(probeCtx, workerservice.ModelProbeRequest{
		RequestId:      localId,
		RegistrationId: registrationId,
		Model:          model,
		SuiteVersion:   req.GetSuiteVersion(),
		Limits:         workerservice.ModelProbeLimits{Timeout: timeout},
	})
	if err != nil {
		// StartModelProbe fails only before anything reached the machine -- a
		// cockpit with no probe support, a model the machine does not
		// advertise, an unknown suite version, a stream that went away.
		h.sendProbeRefusal(send, requestId, model, "model_probe_refused", err.Error())
		return
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for p := range handle.Progress() {
			h.relayProbeProgress(send, requestId, model, p)
		}
	}()

	outcome, waitErr := handle.Wait(probeCtx)
	wg.Wait()

	resp := &nodev1.ModelProbeForwardResponse{
		RequestId:    requestId,
		Model:        outcome.Model,
		Ok:           waitErr == nil && outcome.Ok,
		SuiteVersion: outcome.SuiteVersion,
	}
	if resp.GetModel() == "" {
		resp.Model = model
	}
	switch {
	case waitErr != nil:
		resp.ErrorCode = "model_probe_interrupted"
		resp.ErrorMessage = waitErr.Error()
	case !outcome.Ok:
		resp.ErrorCode = "model_probe_failed"
		resp.ErrorMessage = outcome.Error
	}
	// THE FIGURES RIDE EVEN ON A FAILURE, because some cases can have run
	// before the suite stopped and each figure says for itself whether it was
	// measured. Dropping them here would turn a partial measurement into no
	// measurement, which is a strictly worse answer and an unrecoverable one.
	resp.FiguresJson = probe.FiguresToJSON(outcome.Figures)
	_ = send(&nodev1.NodeServerMessage{
		MessageId: id.NewShortId(),
		Payload:   &nodev1.NodeServerMessage_ModelProbeForwardResponse{ModelProbeForwardResponse: resp},
	})
}

// CancelForwardedModelProbe stops a forwarded probe in flight.
//
// The entry is DELETED under the lock before the cancel runs, the pull's shape:
// the handler's own defer would delete it anyway, but doing it here means a
// second cancel for the same id is a no-op rather than a second call into a
// cancel func whose context has already gone.
func (h *ForwardHandler) CancelForwardedModelProbe(_ context.Context, requestId string) {
	h.modelProbeMu.Lock()
	cancel, ok := h.modelProbeInflight[requestId]
	if ok {
		delete(h.modelProbeInflight, requestId)
	}
	h.modelProbeMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (h *ForwardHandler) sendProbeRefusal(
	send func(*nodev1.NodeServerMessage) error,
	requestId, model, code, message string,
) {
	_ = send(&nodev1.NodeServerMessage{
		MessageId: id.NewShortId(),
		Payload: &nodev1.NodeServerMessage_ModelProbeForwardResponse{
			ModelProbeForwardResponse: &nodev1.ModelProbeForwardResponse{
				RequestId:    requestId,
				Model:        model,
				Ok:           false,
				ErrorCode:    code,
				ErrorMessage: message,
				// A refusal carries the ABSENT figures explicitly rather than an
				// empty string, so the originating replica reads four named
				// absences instead of having to decide what a blank means.
				FiguresJson: probe.FiguresToJSON(probe.Figures{
					StructuredValidity:  probe.Absent(probe.AbsentUnmeasured, message),
					ToolCallCorrectness: probe.Absent(probe.AbsentUnmeasured, message),
					ThroughputTps:       probe.Absent(probe.AbsentUnmeasured, message),
					TimeToFirstTokenMs:  probe.Absent(probe.AbsentUnmeasured, message),
				}),
			},
		},
	})
}

func (h *ForwardHandler) relayProbeProgress(
	send func(*nodev1.NodeServerMessage) error,
	requestId, model string,
	p workerservice.ModelProbeProgress,
) {
	_ = send(&nodev1.NodeServerMessage{
		MessageId: id.NewShortId(),
		Payload: &nodev1.NodeServerMessage_ModelProbeForwardProgress{
			ModelProbeForwardProgress: &nodev1.ModelProbeForwardProgress{
				RequestId:      requestId,
				Model:          model,
				CaseId:         p.CaseId,
				CompletedCases: p.Completed,
				TotalCases:     p.Total,
				CaseOk:         p.Ok,
				CaseError:      p.Error,
			},
		},
	})
}
