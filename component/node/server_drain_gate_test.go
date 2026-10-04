package node_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/znasllc-io/memql/component/identity"
	"github.com/znasllc-io/memql/component/node"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// A draining node refuses NEW NodeService.Stream opens with Unavailable, and
// refuses them BEFORE the auth chain runs -- so before the node-token
// revocation lookup, which needs the database the drain has already released.
//
// The field record (k3d, 2026-09-28 and again 2026-10-04): a draining agent
// released its pool, then sat in the dependency Stop sweep for 30s with its
// NodeService listener still open. Every peer that re-dialled passed JWT
// verification and failed the revocation lookup, and the refusal it got back
// was an auth failure, which tells the caller to re-mint its token rather than
// to try another peer. The node was already de-routed (gossip DRAINING,
// readiness 503); it was still accepting mesh streams it could not serve.
//
// This runs the REAL interceptor chain the cluster wires
// (NodeClassStreamInterceptorWithRevocation, a minted class="node" token, a
// recording resolver) on a REAL listener, so a gate placed after the auth
// chain, or one that consults the wrong lifecycle, fails here.

// drainGateHarness is a started NodeServer with the production auth chain and
// a client-side token for one peer.
type drainGateHarness struct {
	srv      *node.NodeServer
	pm       *node.PeerManager
	resolver *switchableResolver
	token    string
	peerType string
	peerId   string
}

// switchableResolver answers "not revoked" until failWith is set, then fails
// every lookup the way the identity store does once the pool is closed.
type switchableResolver struct {
	calls    atomic.Int64
	failWith atomic.Pointer[error]
}

func (r *switchableResolver) IsNodeTokenRevoked(context.Context, string, string) (bool, error) {
	r.calls.Add(1)
	if err := r.failWith.Load(); err != nil {
		return false, *err
	}
	return false, nil
}

func newDrainGateHarness(t *testing.T) *drainGateHarness {
	t.Helper()
	t.Setenv("MEMQL_NODE_SERVICE_ADDRESS", "127.0.0.1:0")

	jwks := httptest.NewServer(http.NewServeMux())
	t.Cleanup(jwks.Close)
	mux := http.NewServeMux()
	jwks.Config.Handler = mux
	km, iss := newIssuer(t, jwks.URL)
	mux.Handle("/.well-known/jwks.json", jwksHandler(km))

	h := &drainGateHarness{resolver: &switchableResolver{}, peerType: "bff", peerId: "bff-drain-test"}
	tok, _, err := iss.IssueNodeAccessToken(identity.NodeIssueInput{
		IdentityId: "v1:identity:identity:node:" + h.peerType + ":" + h.peerId,
		NodeId:     h.peerId,
		NodeType:   h.peerType,
	}, time.Now().UTC())
	require.NoError(t, err)
	h.token = tok

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	self := node.NewIdentity("test")
	h.pm = node.NewPeerManager(self, logger)
	h.srv = node.NewNodeServer(self, h.pm, logger)
	h.srv.SetAuthInterceptor(node.NodeClassStreamInterceptorWithRevocation(
		newVerifier(t, jwks.URL),
		&node.NodeRevocationCheck{Resolver: h.resolver, CacheTTL: time.Nanosecond},
		logger,
	))

	h.srv.Start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		h.srv.Stop(ctx)
	})
	select {
	case <-h.srv.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("node server did not become ready")
	}
	require.NoError(t, h.pm.Lifecycle().Transition(node.LifecycleReady))
	return h
}

// open dials the server and opens a NodeService.Stream carrying the peer's
// token. The returned stream's first Recv reports the open's outcome. The
// deadline turns an ADMITTED stream (which waits for a NodeHello forever) into
// a DeadlineExceeded the assertions name, instead of a hung test.
func (h *drainGateHarness) open(t *testing.T) nodev1.NodeService_StreamClient {
	t.Helper()
	conn, err := grpc.NewClient(h.srv.BoundAddrForTest(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+h.token), 5*time.Second)
	t.Cleanup(cancel)
	stream, err := nodev1.NewNodeServiceClient(conn).Stream(ctx)
	require.NoError(t, err)
	return stream
}

func (h *drainGateHarness) hello() *nodev1.NodeClientMessage {
	return &nodev1.NodeClientMessage{
		MessageId: "hello-1",
		Payload: &nodev1.NodeClientMessage_NodeHello{
			NodeHello: &nodev1.NodeHello{NodeId: h.peerId, NodeType: h.peerType},
		},
	}
}

func TestNodeServer_DrainingRefusesNewStreamsBeforeTheRevocationLookup(t *testing.T) {
	for _, state := range []node.LifecycleState{node.LifecycleDraining, node.LifecycleStopped} {
		t.Run(state.String(), func(t *testing.T) {
			h := newDrainGateHarness(t)
			require.NoError(t, h.pm.Lifecycle().Transition(state))

			// The database is gone: were the open to reach the resolver, it
			// would fail as an auth refusal (Unauthenticated) and the
			// assertion on the code below would name the wrong answer.
			dbClosed := errors.New("sql: database is closed")
			h.resolver.failWith.Store(&dbClosed)

			_, err := h.open(t).Recv()
			require.Error(t, err)
			assert.Equal(t, codes.Unavailable, status.Code(err),
				"a %s node must answer a new stream Unavailable, so the peer backs off and dials another peer; got %v", state, err)
			assert.Zero(t, h.resolver.calls.Load(),
				"the refusal must come BEFORE the auth chain: a %s node has released its database, and the revocation lookup is a database read", state)
		})
	}
}

// The gate is for NEW streams only. A stream opened before the drain keeps
// being served through it (memql#1269's in-flight drain), and the control half
// proves the harness reaches the resolver at all, so the zero above is not
// vacuous.
func TestNodeServer_DrainingKeepsServingAStreamOpenedBeforeIt(t *testing.T) {
	h := newDrainGateHarness(t)

	inflight := h.open(t)
	require.Eventually(t, func() bool { return h.resolver.calls.Load() == 1 }, 5*time.Second, 10*time.Millisecond,
		"a stream opened while Ready must pass the gate and reach the revocation lookup")

	require.NoError(t, h.pm.Lifecycle().Transition(node.LifecycleDraining))

	require.NoError(t, inflight.Send(h.hello()))
	reply, err := inflight.Recv()
	require.NoError(t, err, "the in-flight stream must survive the drain")
	assert.NotNil(t, reply.GetNodeWelcome(), "the in-flight stream must still be served after the drain began; got %v", reply)

	_, err = h.open(t).Recv()
	assert.Equal(t, codes.Unavailable, status.Code(err), "a stream opened after the drain must be refused; got %v", err)
	assert.Equal(t, int64(1), h.resolver.calls.Load(), "only the pre-drain open may reach the revocation lookup")
}
