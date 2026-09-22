//go:build agent || planner

package worker

// The RECEIVING half of cross-node worker dispatch (memql#4352).
//
// This replica holds the machine's WorkerService stream. Another replica is
// serving a turn that needs it and has forwarded the dispatch here.
//
// WHAT THIS RE-CHECKS, AND WHAT IT DELIBERATELY DOES NOT.
//
// The consent gates -- per-task approval, the kill switch, standing scope, the
// safety classifier -- ran on the SENDER, before its router picked anything.
// Re-running them here would need the sender's plan and agent context, would
// answer the same question twice, and would create a second place for the
// answer to drift. They are not re-run.
//
// What IS re-checked is the pair of facts only this node can establish, and
// they are exactly the two ways a forward could otherwise be abused:
//
//   - THE MACHINE IS OWNED BY THE ASSERTED PRINCIPAL. Without this, a
//     compromised or buggy replica could name any registration id and reach a
//     machine belonging to somebody else. The owner comes from the verified
//     ForwardedAuthority, never from the envelope's owner_user_id field, which
//     is a hint used only to read the fleet.
//   - THE MACHINE IS NOT REVOKED. Revocation is a row, and rows are what the
//     sender read one heartbeat ago. This node reads it again at the moment of
//     dispatch, because "revoked while a turn was in flight" is precisely the
//     window that matters.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/node"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/id"
)

// ForwardHandler serves inbound WorkerForwardRequest envelopes.
type ForwardHandler struct {
	registry *workerservice.Registry
	store    FleetStore
	logger   *slog.Logger

	mu       sync.Mutex
	inflight map[string]context.CancelFunc

	// In-flight model calls, in their own table under their own lock, for
	// the reason ForwardRouter's twin states.
	modelMu       sync.Mutex
	modelInflight map[string]context.CancelFunc

	// In-flight pulls, in their own table again (epic memql#5103).
	modelPullMu       sync.Mutex
	modelPullInflight map[string]context.CancelFunc
	// Separate from the pull's for the same reason it is separate from the
	// call's: one map would let a probe's cancel reach a pull.
	modelProbeMu       sync.Mutex
	modelProbeInflight map[string]context.CancelFunc
}

// NewForwardHandler wraps this replica's registry and fleet store.
func NewForwardHandler(registry *workerservice.Registry, store FleetStore, logger *slog.Logger) *ForwardHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &ForwardHandler{
		registry:           registry,
		store:              store,
		logger:             logger,
		inflight:           make(map[string]context.CancelFunc),
		modelInflight:      make(map[string]context.CancelFunc),
		modelPullInflight:  make(map[string]context.CancelFunc),
		modelProbeInflight: make(map[string]context.CancelFunc),
	}
}

// HandleForwardedRequest implements node.WorkerForwardHandler.
func (h *ForwardHandler) HandleForwardedRequest(
	ctx context.Context,
	req *nodev1.WorkerForwardRequest,
	send func(*nodev1.NodeServerMessage) error,
) {
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

	authority := node.ForwardedAuthorityFromProto(req.GetAuthority())
	access, err := auth.VerifyForwardedAuthority(authority, time.Now())
	if err != nil {
		h.logger.Warn("worker forward: refused an envelope whose authority did not verify",
			"request_id", requestId, "registration_id", req.GetRegistrationId(), "error", err)
		// REFUSED BEFORE START: nothing was dispatched. The sender may try
		// another machine, which is the right outcome -- a bad assertion is a
		// fact about this hop, not about the user's fleet.
		h.sendRefusal(send, requestId, "forwarded_authority_refused", err.Error())
		return
	}
	cctx = auth.BindForwardedContext(cctx, authority.Principal().Claims, access, authority)

	owner := strings.TrimSpace(access.UserId)
	if owner == "" {
		h.sendRefusal(send, requestId, "forwarded_authority_refused",
			"the verified authority names no subject, so there is no owner to check the machine against")
		return
	}
	// The envelope's owner_user_id is a HINT and is checked against the
	// assertion rather than trusted. They disagree only if the sender is
	// confused or lying, and either way this dispatch does not run.
	if hint := strings.TrimSpace(req.GetOwnerUserId()); hint != "" && !sameSubject(hint, owner) {
		h.logger.Warn("worker forward: envelope owner does not match the verified authority",
			"request_id", requestId, "envelope_owner", hint, "authority_subject", owner)
		h.sendRefusal(send, requestId, "owner_mismatch",
			"the envelope's owner does not match the verified authority's subject")
		return
	}

	registrationId := req.GetRegistrationId()
	if err := h.verifyRegistration(cctx, owner, registrationId); err != nil {
		h.sendRefusal(send, requestId, "registration_refused", err.Error())
		return
	}

	w := h.registry.WorkerById(registrationId)
	if w == nil {
		// The sender's row said this replica holds the stream; it does not any
		// more. Ordinary, and re-pickable.
		h.sendRefusal(send, requestId, "worker_disconnected",
			"this replica no longer holds a stream for that machine")
		return
	}
	if w.OwnerUserId != "" && !sameSubject(w.OwnerUserId, owner) {
		// Belt and braces against a registry entry disagreeing with the row.
		h.sendRefusal(send, requestId, "owner_mismatch",
			"the connected machine is owned by a different user than the assertion names")
		return
	}

	capability := req.GetCapability()
	timeout := time.Duration(req.GetTimeoutSec()) * time.Second
	if timeout <= 0 {
		timeout = workerservice.DispatchTimeoutDefault
	}
	dispatchCtx, dcancel := context.WithTimeout(cctx, timeout)
	defer dcancel()

	if err := w.Acquire(dispatchCtx, capability); err != nil {
		h.sendRefusal(send, requestId, "worker_busy", err.Error())
		return
	}
	defer w.Release(capability)

	innerArgs := map[string]any{}
	if raw := req.GetArgsJson(); len(raw) > 0 {
		if err := json.Unmarshal(raw, &innerArgs); err != nil {
			// The slot is already held, but nothing has been sent to the
			// machine, so this is still a pre-start refusal.
			h.sendRefusal(send, requestId, "decode_args", err.Error())
			return
		}
	}

	envelope := buildToolDispatch(Request{
		Tool:          req.GetTool(),
		Action:        req.GetAction(),
		Args:          innerArgs,
		AgentId:       req.GetAgentId(),
		OwnerUserId:   owner,
		RunId:         req.GetRunId(),
		StepId:        req.GetStepId(),
		CorrelationId: req.GetCorrelationId(),
	}, timeout)

	// FROM HERE ON, NOTHING IS RE-PICKABLE. The envelope is on its way to the
	// machine, so every remaining answer is reported with
	// refused_before_start false, including a mid-call disconnect: an exec
	// whose stream died may have run.
	res, err := w.DispatchWithStream(dispatchCtx, envelope, func(chunk *memqlv1.ToolStream) {
		h.relayChunk(send, requestId, chunk)
	})
	out := translateResult(envelope.GetCallId(), res, err)
	h.send(send, &nodev1.WorkerForwardResponse{
		RequestId:     requestId,
		Ok:            out.OK,
		OutputJson:    []byte(out.OutputJSON),
		ErrorCode:     out.ErrorCode,
		ErrorMessage:  out.ErrorMessage,
		BytesIn:       uint64(max(out.BytesIn, 0)),
		BytesOut:      uint64(max(out.BytesOut, 0)),
		OutputPreview: out.OutputPreview,
	})
}

// verifyRegistration re-reads the row and refuses a machine that is not this
// owner's or that has been revoked.
//
// TOOL DISPATCH ONLY. It is deliberately NOT the model path's check: sharing a
// machine lends its GPU, not its shell, so `WorkerForward` stays owner-only
// and verifySharedRegistration beside it is what the model forward asks.
func (h *ForwardHandler) verifyRegistration(ctx context.Context, ownerUserId, registrationId string) error {
	if h.store == nil {
		// No fleet store on this replica. The registry check below still
		// establishes ownership from the live stream, but revocation would go
		// unchecked -- so refuse rather than dispatch on a weaker check than
		// the one this function exists to perform.
		return errNoFleetStore
	}
	machines, err := h.store.WorkersForOwner(ctx, ownerUserId)
	if err != nil {
		return err
	}
	for _, m := range machines {
		if !sameSubject(m.RegistrationId, registrationId) {
			continue
		}
		if !m.RevokedAt.IsZero() {
			return errRegistrationRevoked
		}
		return nil
	}
	return errRegistrationNotOwned
}

// verifySharedRegistration is verifyRegistration for a MODEL call, and the
// difference between the two is the whole of finding H-2 (epic memql#5327,
// design D10).
//
// The router picks a cluster-shared machine belonging to owner O for acting
// user U. The envelope then crosses to the replica holding O's stream, and
// this is the check it lands on. Before D10 the only check was
// `WorkersForOwner(U)`, which by construction cannot return O's machine -- so
// the receiver refused with errRegistrationNotOwned and the call died as
// `registration_refused`. Neither this file nor model_forward.go was touched
// when sharing landed, so the feature worked exactly when the stream happened
// to be local, which on the default two-replica topology is a coin toss.
//
// So: admit when the machine is the caller's OWN, or when it is unrevoked and
// cluster-shared. The second arm resolves through SharedFleetStore -- the SAME
// cross-owner read the router used to pick it, deliberately on a second
// interface so user-scoped paths cannot reach it -- and it is a narrow
// widening rather than a relaxed check: revocation, both halves of the
// consent, and the owner's identity are all still decided here.
//
// A replica with no SharedFleetStore answers exactly as it did before: only
// owned machines. That is the honest degradation, because a node that cannot
// make the cross-owner read cannot establish that anything was shared.
func (h *ForwardHandler) verifySharedRegistration(ctx context.Context, actingUserId, registrationId string) error {
	ownErr := h.verifyRegistration(ctx, actingUserId, registrationId)
	switch {
	case ownErr == nil:
		return nil
	case errors.Is(ownErr, errRegistrationRevoked):
		// Their own machine, revoked. The shared arm cannot rescue that, and
		// saying "not shared with you" about somebody's own revoked machine
		// would send them to the wrong repair.
		return ownErr
	case errors.Is(ownErr, errNoFleetStore):
		return ownErr
	}

	shared, ok := h.store.(SharedFleetStore)
	if !ok {
		return ownErr
	}
	machines, err := shared.SharedInferenceWorkers(ctx)
	if err != nil {
		return err
	}
	for _, m := range machines {
		if !sameSubject(m.RegistrationId, registrationId) {
			continue
		}
		if !m.RevokedAt.IsZero() {
			return errRegistrationRevoked
		}
		if !m.ServesCluster() {
			return errRegistrationNotShared
		}
		return nil
	}
	return ownErr
}

// ownerOfSharedMachine confirms that the LIVE registry entry for a machine
// names the same owner the shared row does (epic memql#5327, design D10).
//
// It exists because the registry's plain `w.OwnerUserId != owner` refusal is
// right for an owned call and wrong for a shared one -- on a shared call the
// connected machine belongs to somebody else by design. Dropping the check
// entirely would be the easy move and the wrong one: it is the only thing that
// notices the registry entry and the row disagreeing about whose machine this
// is, which is a crossed or stale entry rather than a sharing arrangement.
//
// So the question it asks is narrower than the one it replaces: not "is this
// the caller's machine" but "is the machine this replica is holding the same
// one the shared catalogue just described".
func (h *ForwardHandler) ownerOfSharedMachine(ctx context.Context, registrationId, registryOwner string) error {
	shared, ok := h.store.(SharedFleetStore)
	if !ok {
		return errRegistrationNotOwned
	}
	machines, err := shared.SharedInferenceWorkers(ctx)
	if err != nil {
		return err
	}
	for _, m := range machines {
		if !sameSubject(m.RegistrationId, registrationId) {
			continue
		}
		if sameSubject(m.OwnerUserId, registryOwner) {
			return nil
		}
		return forwardRefusal("this replica's live registration for that machine names a different owner than the fleet row does")
	}
	return errRegistrationNotOwned
}

// relayChunk forwards one ToolStream chunk across the hop.
func (h *ForwardHandler) relayChunk(send func(*nodev1.NodeServerMessage) error, requestId string, chunk *memqlv1.ToolStream) {
	if chunk == nil {
		return
	}
	out := &nodev1.WorkerForwardStream{RequestId: requestId}
	switch payload := chunk.GetPayload().(type) {
	case *memqlv1.ToolStream_StdoutChunk:
		out.Payload = &nodev1.WorkerForwardStream_StdoutChunk{StdoutChunk: payload.StdoutChunk}
	case *memqlv1.ToolStream_StderrChunk:
		out.Payload = &nodev1.WorkerForwardStream_StderrChunk{StderrChunk: payload.StderrChunk}
	case *memqlv1.ToolStream_DataChunk:
		out.Payload = &nodev1.WorkerForwardStream_DataChunk{DataChunk: payload.DataChunk}
	default:
		return
	}
	if err := send(&nodev1.NodeServerMessage{
		MessageId:   id.NewShortId(),
		CorrelateTo: requestId,
		Payload:     &nodev1.NodeServerMessage_WorkerForwardStream{WorkerForwardStream: out},
	}); err != nil {
		// A chunk that cannot be sent is dropped, not retried: the result
		// still carries the full output, and blocking the machine's recv
		// goroutine on a peer connection would be worse than losing a
		// progress update.
		h.logger.Debug("worker forward: stream chunk send failed",
			"request_id", requestId, "error", err)
	}
}

// CancelForwardedRequest implements node.WorkerForwardHandler.
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

// sendRefusal answers with refused_before_start TRUE. Every caller of this
// function is a path where the dispatch demonstrably did not reach the
// machine; a path where it might have must use send() directly.
func (h *ForwardHandler) sendRefusal(send func(*nodev1.NodeServerMessage) error, requestId, code, msg string) {
	h.send(send, &nodev1.WorkerForwardResponse{
		RequestId:          requestId,
		ErrorCode:          code,
		ErrorMessage:       msg,
		RefusedBeforeStart: true,
	})
}

func (h *ForwardHandler) send(send func(*nodev1.NodeServerMessage) error, resp *nodev1.WorkerForwardResponse) {
	if err := send(&nodev1.NodeServerMessage{
		MessageId:   id.NewShortId(),
		CorrelateTo: resp.GetRequestId(),
		Payload:     &nodev1.NodeServerMessage_WorkerForwardResponse{WorkerForwardResponse: resp},
	}); err != nil {
		h.logger.Warn("worker forward: response send failed",
			"request_id", resp.GetRequestId(), "error_code", resp.GetErrorCode(), "error", err)
	}
}

// sameSubject compares two ids that may be spelled canonically
// (`v1:identity:user:abc`) on one side and bare (`abc`) on the other. The
// engine bare-ifies on egress and canonicalizes on write, so both spellings
// are in play on any given comparison -- see docs/public/concepts/identifiers.md.
func sameSubject(a, b string) bool {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == b {
		return a != ""
	}
	return a != "" && b != "" && lastSegment(a) == lastSegment(b)
}

func lastSegment(s string) string {
	if i := strings.LastIndex(s, ":"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// The refusals verifyRegistration can produce. Each is a REFUSAL BEFORE START.
var (
	errNoFleetStore         = forwardRefusal("no fleet store on this replica, so the machine's revocation cannot be checked")
	errRegistrationRevoked  = forwardRefusal("that machine has been revoked")
	errRegistrationNotOwned = forwardRefusal("that machine is not registered to the asserted owner")
	// errRegistrationNotShared is the MODEL path's own refusal (epic
	// memql#5327, design D10). It is a separate sentence from "not registered
	// to the asserted owner" because it leads somewhere else: that one says
	// the machine is not yours and this one says it is somebody else's and
	// they have not offered it, which is a thing its owner can change.
	errRegistrationNotShared = forwardRefusal("that machine belongs to somebody else and is not shared with the cluster")
)

type forwardRefusal string

func (e forwardRefusal) Error() string { return string(e) }
