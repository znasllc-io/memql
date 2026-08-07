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
// Forward returns ErrNoWorkbenchPeer immediately. The integration's
// dispatch path interprets that as "fall back to local in-process
// dispatch" rather than failing the tool call.
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
// workbench peer in the mesh. The caller should fall back to local
// dispatch (when wired) or surface a clean error to the LLM.
var ErrNoWorkbenchPeer = errors.New("workbench: no healthy peer available")

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
// and waits for the response. Caller fills in (planId, action, args,
// agentId, taskId, authority) on the request; this method stamps
// request_id, registers the inflight channel, sends, and awaits the
// matching response or ctx cancellation.
func (r *ForwardRouter) Forward(ctx context.Context, req *nodev1.WorkbenchForwardRequest) (*nodev1.WorkbenchForwardResponse, error) {
	if r == nil || r.peerMgr == nil {
		return nil, ErrNoWorkbenchPeer
	}
	peer := r.pickWorkbenchPeer()
	if peer == nil {
		return nil, ErrNoWorkbenchPeer
	}
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
		return nil, ErrNoWorkbenchPeer
	}
	msg := &nodev1.NodeClientMessage{
		MessageId: id.NewShortId(),
		Payload: &nodev1.NodeClientMessage_WorkbenchForwardRequest{
			WorkbenchForwardRequest: req,
		},
	}
	peer.Connection.Send(msg)
	select {
	case resp := <-respCh:
		return resp, nil
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
		return nil, ctx.Err()
	}
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

// pickWorkbenchPeer returns a healthy workbench peer with a live
// outbound connection, or nil if none are available. Selection is
// any-fit -- we don't currently track per-peer load.
func (r *ForwardRouter) pickWorkbenchPeer() *node.PeerEntry {
	if r.peerMgr == nil {
		return nil
	}
	peers := r.peerMgr.ByType(node.NodeTypeWorkbench)
	for _, p := range peers {
		if p == nil || p.Info == nil {
			continue
		}
		if p.Connection == nil {
			continue
		}
		if p.Info.Health != nodev1.NodeHealthStatus_NODE_HEALTH_HEALTHY {
			continue
		}
		return p
	}
	return nil
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
// Truthy values: "1", "true", "yes", "on" (case-insensitive).
// Anything else, including empty, is false.
func remoteEnabled(env string) bool {
	switch strings.ToLower(strings.TrimSpace(env)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// SilenceUnused references package-level identifiers that exist for
// completeness but aren't read in the current call sites. Cheaper
// than build-tagging the file and lets vet stay clean.
var _ = time.Second
