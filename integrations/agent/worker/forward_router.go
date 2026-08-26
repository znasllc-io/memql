//go:build agent

package worker

// The SENDING half of cross-node worker dispatch (memql#4352).
//
// WHY THIS EXISTS. A cockpit machine's WorkerService stream terminates on
// exactly ONE agent replica -- whichever one it happened to connect to. The
// turn that wants to use it is served wherever the mesh routed the request.
// With the default topology, two agent replicas, that is a coin flip, and on
// the losing side the machine was simply not there: the in-memory registry is
// per-node and is never rehydrated from the rows, so the turn reported
// no_worker_available for a laptop the user could see was online. A fleet page
// listing machines the engine cannot reach would have made that visible
// without making it work, which is why this lands in the same epic.
//
// It is the WorkbenchForward* shape, deliberately: stamp a request id, park on
// a channel, send to a named peer, route the answer back by id. The two
// differences are both forced by what a machine is.
//
//   - The peer is named by connectedNodeId, not chosen. A workbench call can
//     go to any healthy workbench; a dispatch to somebody's laptop can only go
//     to the replica holding that laptop's stream.
//   - The response carries refused_before_start, because the sender may need
//     to try another machine and must never do so after a call has begun.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/node"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/core/id"
)

// ErrNoPeerForNode signals that the replica named by a machine's
// connectedNodeId is not a reachable peer. It is a REFUSAL BEFORE START: the
// dispatch never left this node, so the caller may try another machine.
var ErrNoPeerForNode = errors.New("agent.worker: no reachable peer for the machine's replica")

// PeerSender is "hand this envelope to the replica with this node id", and it
// is a seam rather than a direct PeerManager call for one reason: a
// *peerConnection is unexported, so without it the hop this file exists to
// perform could only be tested against a live two-replica cluster. That is
// exactly the shape of test that skips everywhere and therefore guards
// nothing -- the argument test/clustere2e/automation_run_routing_test.go makes
// at length about its own subject.
//
// It reports whether the envelope was handed off. False means no reachable
// peer, which the caller turns into a refusal BEFORE START.
type PeerSender interface {
	Send(nodeId string, msg *nodev1.NodeClientMessage) bool
}

// ForwardRouter dispatches to a machine held by another agent replica.
type ForwardRouter struct {
	sender PeerSender
	self   func() (string, string)
	logger *slog.Logger

	mu       sync.Mutex
	inflight map[string]*forwardCall

	// Model calls park in their own table under their own lock. Separate
	// because they are a different payload with a different lifetime, and
	// sharing one map would make an id collision between the two families a
	// silent cross-delivery rather than a miss.
	modelMu       sync.Mutex
	modelInflight map[string]*modelForwardCall
}

// peerManagerSender is the production PeerSender: look the replica up by node
// id and send on its connection.
type peerManagerSender struct{ peerMgr *node.PeerManager }

func (p peerManagerSender) Send(nodeId string, msg *nodev1.NodeClientMessage) bool {
	if p.peerMgr == nil {
		return false
	}
	peer := p.peerMgr.Get(nodeId)
	if peer == nil || peer.Connection == nil {
		return false
	}
	peer.Connection.Send(msg)
	return true
}

// forwardCall is one parked dispatch: where the answer goes, and where the
// chunks go while it waits.
type forwardCall struct {
	resp    chan *nodev1.WorkerForwardResponse
	onChunk func(*nodev1.WorkerForwardStream)
}

// NewForwardRouter constructs an idle router bound to the agent node's
// PeerManager.
func NewForwardRouter(peerMgr *node.PeerManager, logger *slog.Logger) *ForwardRouter {
	self := func() (string, string) {
		if peerMgr == nil {
			return "", ""
		}
		return peerMgr.SelfNodeId(), peerMgr.SelfNodeType()
	}
	return newForwardRouter(peerManagerSender{peerMgr: peerMgr}, self, logger)
}

// newForwardRouter is the constructor the hop test uses with a fake sender.
func newForwardRouter(sender PeerSender, self func() (string, string), logger *slog.Logger) *ForwardRouter {
	if logger == nil {
		logger = slog.Default()
	}
	if self == nil {
		self = func() (string, string) { return "", "" }
	}
	return &ForwardRouter{
		sender:        sender,
		self:          self,
		logger:        logger,
		inflight:      make(map[string]*forwardCall),
		modelInflight: make(map[string]*modelForwardCall),
	}
}

// SelfNodeId reports this replica's own id, for the audit-only origin stamp on
// the forwarded authority.
func (r *ForwardRouter) SelfNodeId() string {
	if r == nil {
		return ""
	}
	id, _ := r.self()
	return id
}

// SelfNodeType reports this replica's node type, same purpose.
func (r *ForwardRouter) SelfNodeType() string {
	if r == nil {
		return ""
	}
	_, t := r.self()
	return t
}

// ForwardDispatch implements RemoteDispatcher.
//
// Every failure before the envelope leaves this node is reported as
// ForwardRefusedBeforeStart, because that is the truth: nothing ran. Every
// failure after it leaves is reported as ForwardCompleted unless the RECEIVER
// says otherwise, because from here the two are indistinguishable and the safe
// reading of "I do not know whether it ran" is that it did.
func (r *ForwardRouter) ForwardDispatch(
	ctx context.Context,
	nodeId string,
	req Request,
	registrationId string,
	capability string,
	timeout time.Duration,
) (Result, ForwardOutcome, error) {
	if r == nil || r.sender == nil {
		return Result{}, ForwardRefusedBeforeStart, ErrNoPeerForNode
	}

	// THE MANDATORY ASSERTION (memql#3205). Like the workbench's second hop,
	// this is RE-ASSERTED from the context rather than rebuilt: an
	// AccessContext carries no credential class and no role ceiling, so a
	// badge session rebuilt from one would reach the receiving replica as an
	// ordinary user with no ceiling -- "no badge" indistinguishable from "not
	// stated", which is the property memql#3205 removed from the AI-forward
	// path.
	//
	// Absent, this REFUSES BEFORE START rather than failing the turn. The
	// difference matters: a refusal lets the router fall through to a machine
	// on this replica, which is a working call, where a hard failure would be
	// a broken one. The log line is what keeps that from being silent.
	authority, ok := auth.ForwardedAuthorityFromContext(ctx)
	if !ok {
		r.logger.Warn("worker forward: no forwarded authority bound to the call context; refusing to forward",
			"registration_id", registrationId, "target_node_id", nodeId)
		return Result{
				OK:           false,
				ErrorCode:    "no_forwarded_authority",
				ErrorMessage: "forwarding a machine dispatch requires the assertion this node accepted; none is bound to the call context",
			}, ForwardRefusedBeforeStart,
			nil
	}

	// The INNER args only. The receiver rebuilds the ToolDispatch envelope
	// itself from the fields beside this one, so the wire carries the action's
	// arguments and not a half-built envelope that both sides would have to
	// agree on the shape of.
	argsJSON, err := json.Marshal(req.Args)
	if err != nil {
		return Result{}, ForwardRefusedBeforeStart, err
	}

	requestId := id.NewShortId()
	call := &forwardCall{resp: make(chan *nodev1.WorkerForwardResponse, 1), onChunk: req.OnStreamChunk}
	r.mu.Lock()
	r.inflight[requestId] = call
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.inflight, requestId)
		r.mu.Unlock()
	}()

	timeoutSec := int32(timeout / time.Second)
	if timeoutSec <= 0 {
		timeoutSec = 1
	}
	env := &nodev1.WorkerForwardRequest{
		RequestId:      requestId,
		RegistrationId: registrationId,
		OwnerUserId:    req.OwnerUserId,
		Capability:     capability,
		Tool:           req.Tool,
		Action:         req.Action,
		ArgsJson:       argsJSON,
		AgentId:        req.AgentId,
		PlanId:         req.PlanId,
		TaskId:         req.TaskId,
		CorrelationId:  req.CorrelationId,
		TimeoutSec:     timeoutSec,
		Authority:      node.ForwardedAuthorityToProto(authority, r.SelfNodeId(), r.SelfNodeType()),
	}
	if !r.sender.Send(nodeId, &nodev1.NodeClientMessage{
		MessageId: id.NewShortId(),
		Payload: &nodev1.NodeClientMessage_WorkerForwardRequest{
			WorkerForwardRequest: env,
		},
	}) {
		return Result{}, ForwardRefusedBeforeStart, ErrNoPeerForNode
	}

	select {
	case resp := <-call.resp:
		return resultFromForward(resp), forwardOutcome(resp), nil
	case <-ctx.Done():
		// Best-effort cancel. The receiving replica stops in-flight work and
		// frees the machine's concurrency slot.
		r.sender.Send(nodeId, &nodev1.NodeClientMessage{
			MessageId: id.NewShortId(),
			Payload: &nodev1.NodeClientMessage_WorkerForwardCancel{
				WorkerForwardCancel: &nodev1.WorkerForwardCancel{RequestId: requestId},
			},
		})
		// ForwardCompleted, not refused: the envelope was on the wire, so the
		// call may well have started on the machine. Re-picking here would run
		// a side effect twice on a cancellation that had nothing to do with
		// the machine.
		return Result{}, ForwardCompleted, ctx.Err()
	}
}

// Dispatch implements node.WorkerForwardResponseSink for the single response.
func (r *ForwardRouter) Dispatch(resp *nodev1.WorkerForwardResponse) {
	if r == nil || resp == nil {
		return
	}
	r.mu.Lock()
	call, ok := r.inflight[resp.GetRequestId()]
	r.mu.Unlock()
	if !ok {
		// A response whose request is gone is the ordinary shape of a call
		// this node timed out or cancelled; it is not an error.
		r.logger.Debug("worker forward response for an unknown request id",
			"request_id", resp.GetRequestId(), "error_code", resp.GetErrorCode())
		return
	}
	select {
	case call.resp <- resp:
	default:
		r.logger.Warn("worker forward response dropped (channel full)",
			"request_id", resp.GetRequestId())
	}
}

// DispatchStream implements node.WorkerForwardResponseSink for relayed chunks.
func (r *ForwardRouter) DispatchStream(chunk *nodev1.WorkerForwardStream) {
	if r == nil || chunk == nil {
		return
	}
	r.mu.Lock()
	call, ok := r.inflight[chunk.GetRequestId()]
	r.mu.Unlock()
	if !ok || call == nil || call.onChunk == nil {
		return
	}
	call.onChunk(chunk)
}

// resultFromForward projects the wire response onto the tool-loop Result.
func resultFromForward(resp *nodev1.WorkerForwardResponse) Result {
	if resp == nil {
		return Result{OK: false, ErrorCode: "worker_disconnected", ErrorMessage: "empty forward response"}
	}
	return Result{
		OK:            resp.GetOk(),
		OutputJSON:    string(resp.GetOutputJson()),
		ErrorCode:     resp.GetErrorCode(),
		ErrorMessage:  resp.GetErrorMessage(),
		BytesIn:       int(resp.GetBytesIn()),
		BytesOut:      int(resp.GetBytesOut()),
		OutputPreview: resp.GetOutputPreview(),
	}
}

func forwardOutcome(resp *nodev1.WorkerForwardResponse) ForwardOutcome {
	if resp != nil && resp.GetRefusedBeforeStart() {
		return ForwardRefusedBeforeStart
	}
	return ForwardCompleted
}
