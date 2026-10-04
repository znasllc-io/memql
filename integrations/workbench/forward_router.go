package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/node"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	"github.com/znasllc-io/memql/core/id"
)

// ForwardRouter is the agent-node-side abstraction for dispatching
// a workbenchHost call to a remote workbench peer.
//
// Cluster mode (the distributed workbench node-type binary running
// somewhere in the mesh): the agent's Integration handleDispatchHost
// hands the call to this router, which finds a healthy workbench
// peer, marshals the args to a WorkbenchForwardRequest, sends it on
// the peer's connection, and parks the calling goroutine on a
// per-request response channel until the workbench node returns a
// WorkbenchForwardResponse (or ctx is cancelled).
//
// Single-node mode (the MVP -- no workbench peer in the mesh):
// Forward returns ErrNoWorkbenchPeer immediately.
//
// What the integration does with that answer depends on what the
// OPERATOR asked for (memql#3506). With MEMQL_WORKBENCH_REMOTE set,
// ErrNoWorkbenchPeer is a FAILURE: the operator asserted the work does
// not run on the agent, and quietly running it there inverts the
// assertion in exactly the case that matters. Without it, there was no
// assertion to honour and local dispatch is the MVP path. The old
// degrade-to-local behaviour survives as its own opt-in,
// MEMQL_WORKBENCH_LOCAL_FALLBACK.
//
// The router implements node.WorkbenchForwardResponseSink: the node
// stream wires it as the sink for inbound WorkbenchForwardResponse
// envelopes so responses get routed back to the parked request.
type ForwardRouter struct {
	peerMgr *node.PeerManager
	logger  *slog.Logger

	mu       sync.Mutex
	inflight map[string]chan *nodev1.WorkbenchForwardResponse
}

// ErrNoWorkbenchPeer signals the router couldn't find a healthy
// workbench peer in the mesh. In remote mode this is a refusal
// (memql#3506); only with the explicit MEMQL_WORKBENCH_LOCAL_FALLBACK
// opt-in does the caller degrade to local dispatch.
var ErrNoWorkbenchPeer = errors.New("workbench: no healthy peer available")

// ErrWorkbenchPeerLost ends a ForwardWatched wait whose replica stopped being
// one this node would send to -- unhealthy, disconnected, or gone from the
// peer table -- before it answered. NO CANCEL WAS SENT: whatever the request
// started is left running on purpose, for the caller to find again (a
// pipeline step's Job is adopted by the next replica it is forwarded to). The
// node id returned beside it names the replica that was lost.
var ErrWorkbenchPeerLost = errors.New("workbench peer lost while waiting")

// defaultPeerWatchInterval is ForwardWatched's re-check cadence when the caller
// names none: the mesh's default heartbeat interval (component/node), since
// the peer table changes no faster than heartbeats arrive.
const defaultPeerWatchInterval = 5 * time.Second

// NewForwardRouter constructs an idle router. peerMgr must be the
// agent node's PeerManager; logger may be nil (slog.Default used).
func NewForwardRouter(peerMgr *node.PeerManager, logger *slog.Logger) *ForwardRouter {
	if logger == nil {
		logger = slog.Default()
	}
	return &ForwardRouter{
		peerMgr:  peerMgr,
		logger:   logger,
		inflight: make(map[string]chan *nodev1.WorkbenchForwardResponse),
	}
}

// SelfNodeId / SelfNodeType report the forwarding node's own identity, for the
// audit-only origin stamp on the assertion (memql#3219). Both are empty when
// the router has no PeerManager -- which is also exactly when Forward returns
// ErrNoWorkbenchPeer, so an unstamped origin never reaches the wire.
func (r *ForwardRouter) SelfNodeId() string {
	if r == nil || r.peerMgr == nil {
		return ""
	}
	return r.peerMgr.SelfNodeId()
}

func (r *ForwardRouter) SelfNodeType() string {
	if r == nil || r.peerMgr == nil {
		return ""
	}
	return r.peerMgr.SelfNodeType()
}

// Forward dispatches a workbench call to a remote workbench peer
// and waits for the response. Caller fills in (runId, action, args,
// agentId, stepId, authority) on the request; this method stamps
// request_id, registers the inflight channel, sends, and awaits the
// matching response or ctx cancellation.
//
// pinnedNodeId is the node id recorded on the run's live workspace row, or ""
// when the run has no workspace yet (or the row could not be read). It is a
// PREFERENCE, not a requirement: while that replica is healthy and connected
// the call goes there, and when it is gone the call goes anywhere healthy and
// the caller learns about the substitution from the returned node id.
//
// The second return value is the node the call was actually served by. The
// caller needs it because "the workspace moved" is only knowable by comparing
// it with the pin, and that comparison is the difference between a recorded
// re-provision and the silent split this change exists to remove.
func (r *ForwardRouter) Forward(ctx context.Context, req *nodev1.WorkbenchForwardRequest, pinnedNodeId string) (*nodev1.WorkbenchForwardResponse, string, error) {
	return r.forward(ctx, req, pinnedNodeId, 0)
}

// ForwardWatched is Forward plus a liveness watch, for a request that runs for
// minutes -- a pipeline step lasts as long as its Job. Every interval it
// re-checks the replica it sent to, by the predicate the picker used to choose
// it: still in the peer table, healthy, connected. A replica that fails it ends
// the wait with ErrWorkbenchPeerLost, and the node id returned names it.
//
// Without the watch a replica that goes away mid-request -- a deploy drains
// it, a node dies -- leaves the caller waiting out its whole deadline for a
// reply nobody is left to send.
//
// THE LOSS SENDS NO CANCEL, unlike ctx ending, which sends one exactly as
// Forward does. Losing sight of a replica is not abandoning the work: a step's
// Job runs in the cluster rather than in the replica, and the caller
// re-forwards so another replica adopts it. A cancel can still be delivered in
// that moment -- a draining replica keeps serving the streams it has -- and
// the runner deletes the Job on a cancel.
//
// A non-positive interval means defaultPeerWatchInterval.
func (r *ForwardRouter) ForwardWatched(ctx context.Context, req *nodev1.WorkbenchForwardRequest, pinnedNodeId string, interval time.Duration) (*nodev1.WorkbenchForwardResponse, string, error) {
	if interval <= 0 {
		interval = defaultPeerWatchInterval
	}
	return r.forward(ctx, req, pinnedNodeId, interval)
}

// forward is Forward and ForwardWatched: watch == 0 waits for the reply or ctx
// alone, and a positive watch also re-checks the serving replica on that
// cadence.
func (r *ForwardRouter) forward(ctx context.Context, req *nodev1.WorkbenchForwardRequest, pinnedNodeId string, watch time.Duration) (*nodev1.WorkbenchForwardResponse, string, error) {
	if r == nil || r.peerMgr == nil {
		return nil, "", ErrNoWorkbenchPeer
	}
	peer := r.pickWorkbenchPeer(pinnedNodeId)
	if peer == nil {
		return nil, "", ErrNoWorkbenchPeer
	}
	servedBy := peer.Info.GetNodeId()
	if req.RequestId == "" {
		req.RequestId = id.NewShortId()
	}
	respCh := make(chan *nodev1.WorkbenchForwardResponse, 1)
	r.mu.Lock()
	r.inflight[req.RequestId] = respCh
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.inflight, req.RequestId)
		r.mu.Unlock()
	}()

	if peer.Connection == nil {
		return nil, "", ErrNoWorkbenchPeer
	}
	msg := &nodev1.NodeClientMessage{
		MessageId: id.NewShortId(),
		Payload: &nodev1.NodeClientMessage_WorkbenchForwardRequest{
			WorkbenchForwardRequest: req,
		},
	}
	peer.Connection.Send(msg)

	var recheck <-chan time.Time
	if watch > 0 {
		ticker := time.NewTicker(watch)
		defer ticker.Stop()
		recheck = ticker.C
	}
	for {
		select {
		case resp := <-respCh:
			return resp, servedBy, nil
		case <-ctx.Done():
			// Best-effort cancel notification; the worker side handler
			// is wired to stop in-flight work on receipt.
			if peer.Connection != nil {
				peer.Connection.Send(&nodev1.NodeClientMessage{
					MessageId: id.NewShortId(),
					Payload: &nodev1.NodeClientMessage_WorkbenchForwardCancel{
						WorkbenchForwardCancel: &nodev1.WorkbenchForwardCancel{
							RequestId: req.RequestId,
						},
					},
				})
			}
			return nil, servedBy, ctx.Err()
		case <-recheck:
			if r.stillServing(servedBy) {
				continue
			}
			// A reply that landed in the same instant is still the answer.
			select {
			case resp := <-respCh:
				return resp, servedBy, nil
			default:
			}
			r.logger.Warn("workbench: the replica serving a watched forward is no longer one this node would send to; "+
				"ending the wait without a cancel so its work can be adopted",
				"request_id", req.RequestId,
				"action", req.GetAction(),
				"node_id", servedBy)
			return nil, servedBy, ErrWorkbenchPeerLost
		}
	}
}

// stillServing reports whether nodeId is still a replica this node would send
// to: in the peer table, healthy and connected -- healthyWorkbenchPeer, the
// predicate pickWorkbenchPeer chose it by, so the watch and the picker cannot
// disagree about what reachable means.
//
// Read from a snapshot, because this runs while heartbeats rewrite the very
// fields it reads: the snapshot copies them under the peer table's lock.
func (r *ForwardRouter) stillServing(nodeId string) bool {
	for _, p := range r.peerMgr.SnapshotByType(node.NodeTypeWorkbench) {
		if p.Info.GetNodeId() == nodeId {
			return healthyWorkbenchPeer(p)
		}
	}
	return false
}

// Dispatch implements node.WorkbenchForwardResponseSink. Called by
// the agent node's stream handler when a WorkbenchForwardResponse
// lands on a peer connection. Routes the response to the matching
// inflight request_id; unknown request_ids are logged and dropped.
func (r *ForwardRouter) Dispatch(resp *nodev1.WorkbenchForwardResponse) {
	if r == nil || resp == nil {
		return
	}
	r.mu.Lock()
	ch, ok := r.inflight[resp.RequestId]
	r.mu.Unlock()
	if !ok {
		r.logger.Warn("workbench forward response for unknown request_id (timed out or never sent?)",
			"request_id", resp.RequestId,
			"error_code", resp.ErrorCode,
		)
		return
	}
	select {
	case ch <- resp:
	default:
		r.logger.Warn("workbench forward response dropped (channel full)",
			"request_id", resp.RequestId,
		)
	}
}

// pickWorkbenchPeer returns a healthy workbench peer with a live outbound
// connection, or nil if none are available.
//
// AFFINITY FIRST (memql#4354). existingNodeId is the replica whose disk already
// holds this run's workspace directory; when it is healthy and connected, it
// is the only correct answer, because a workspace is a filesystem and a
// filesystem does not follow the request.
//
// Selection used to be plain any-fit, which is the bug rather than a
// simplification. The base manifest runs two workbench replicas, so a run's
// first call made a directory on one and its second call landed on the other
// with even odds -- an fs_write followed by an fs_read of the same path,
// answering "not found" with both calls reporting success. Nothing in either
// result named a node, so the failure read as the agent having imagined the
// write.
//
// The fallback is still any-fit and still untuned for load: with the pinned
// replica gone there is no better information here, and the node-loss bookkeeping
// (releasing the orphaned row, provisioning a successor) belongs to the caller,
// which is the layer that can see both node ids.
func (r *ForwardRouter) pickWorkbenchPeer(existingNodeId string) *node.PeerEntry {
	if r.peerMgr == nil {
		return nil
	}
	return selectWorkbenchPeer(r.peerMgr.ByType(node.NodeTypeWorkbench), existingNodeId, healthyWorkbenchPeer)
}

// selectWorkbenchPeer is the selection itself, separated from where the
// candidates come from and from what "reachable" means.
//
// Both separations earn their keep. The candidate list arrives from a map
// iteration, so its ORDER varies between calls -- which is what turned any-fit
// into a coin flip per call rather than a stable wrong answer, and is why the
// split was so hard to see from the outside. And reachability depends on a live
// *peerConnection, a type no test outside component/node can construct, so
// without the predicate as a parameter the affinity rule would be the one part
// of this file with no coverage.
func selectWorkbenchPeer(peers []*node.PeerEntry, existingNodeId string, reachable func(*node.PeerEntry) bool) *node.PeerEntry {
	pinned := strings.TrimSpace(existingNodeId)
	var anyHealthy *node.PeerEntry
	for _, p := range peers {
		if !reachable(p) {
			continue
		}
		if pinned != "" && p.Info.GetNodeId() == pinned {
			return p
		}
		if anyHealthy == nil {
			anyHealthy = p
		}
	}
	return anyHealthy
}

// healthyWorkbenchPeer is the reachability predicate: known, healthy, and with
// a live outbound connection to send on. Split out of pickWorkbenchPeer so the
// affinity branch and the any-fit branch cannot drift apart -- a pinned peer
// admitted on weaker terms than a substitute would send work to a replica that
// cannot answer.
func healthyWorkbenchPeer(p *node.PeerEntry) bool {
	if p == nil || p.Info == nil || p.Connection == nil {
		return false
	}
	return p.Info.Health == nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY
}

// EncodeArgs marshals the inner args object to JSON for the wire.
// Returns nil for an empty or nil map (the workbench node treats
// missing args_json as "no args" per dispatchResult contract).
func EncodeArgs(args map[string]any) ([]byte, error) {
	if len(args) == 0 {
		return nil, nil
	}
	return json.Marshal(args)
}

// DecodeArgs unmarshals the wire JSON into a Go map. Empty / nil
// bytes return an empty map (never nil) so handler args lookups
// don't blow up.
func DecodeArgs(b []byte) (map[string]any, error) {
	out := map[string]any{}
	if len(b) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("workbench: decode args: %w", err)
	}
	return out, nil
}

// remoteEnabled reads the MEMQL_WORKBENCH_REMOTE env var. When set
// to a truthy value, the agent integration delegates dispatch to
// the ForwardRouter instead of executing locally. Default is local
// (single-node mode) for backwards-compat with the MVP.
//
// Setting it is an ASSERTION -- "this work does not run on the agent"
// -- not a preference, so an unreachable workbench refuses the call
// rather than degrading to local execution (memql#3506).
//
// Truthy values: "1", "true", "yes", "on" (case-insensitive).
// Anything else, including empty, is false.
func remoteEnabled(env string) bool {
	return truthy(env)
}

// localFallbackEnabled reads MEMQL_WORKBENCH_LOCAL_FALLBACK: the
// explicit opt-in that restores the pre-memql#3506 behaviour of
// running a workbench call on the agent node when no workbench peer is
// reachable.
//
// It exists because "run it here if you must" is a legitimate thing to
// want during development -- but it is a DIFFERENT intention from "run
// it remotely", and the bug was that the two were spelled the same
// way. Degrading on the ABSENCE of configuration means the failure
// mode nobody configured is the one that silently fires; requiring a
// variable means somebody typed it.
//
// Off unless explicitly truthy. Same parse as remoteEnabled.
func localFallbackEnabled(env string) bool {
	return truthy(env)
}

// truthy is the shared parse for both workbench mode flags.
func truthy(env string) bool {
	switch strings.ToLower(strings.TrimSpace(env)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
