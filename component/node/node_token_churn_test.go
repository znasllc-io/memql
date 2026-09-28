package node

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

// node_token_churn_test.go holds the node-token churn seen on the local k3d
// cluster on 2026-09-28: during one rollout identity issued ~2,900 node tokens
// in 27 seconds, up to 72 a second from a single bff. Terminating pods had
// released their database pool and answered every new NodeService stream
// with Unauthenticated "node token revocation check failed"; each connection
// re-minted on each answer and re-dialled at once, with nothing bounding the
// rate across attempts or across the connections one process holds.

// churnTokenExp is the fixed `exp` every token the counting bootstrap mints
// carries, so a test can name a token before it is minted.
var churnTokenExp = time.Now().Add(30 * 24 * time.Hour).Unix()

// churnJWT is a JWT-shaped token: an unverified header and signature around a
// payload carrying a real `exp`, so the Identity's expiry bookkeeping runs as
// it does on a token identity minted.
func churnJWT(serial int64, exp int64) string {
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := enc.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d,"jti":"mint-%d"}`, exp, serial)))
	return header + "." + payload + ".sig"
}

// countingBootstrap mirrors identity's POST /node/bootstrap and counts every
// token it mints. The env it sets is the env a cluster node runs with, so
// Identity.EnsureBearerToken, CanRemintBearerToken and the re-mint all take
// their production paths against it.
type countingBootstrap struct {
	mints int64
}

func newCountingBootstrap(t *testing.T) *countingBootstrap {
	t.Helper()
	b := &countingBootstrap{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/node/bootstrap" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		serial := atomic.AddInt64(&b.mints, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success":    true,
			"plainToken": churnJWT(serial, churnTokenExp),
			"expiresAt":  time.Unix(churnTokenExp, 0).UTC().Format(time.RFC3339),
		})
	}))
	t.Cleanup(srv.Close)
	t.Setenv(envAllowInsecureNodeBootstrap, "1")
	t.Setenv("MEMQL_NODE_TOKEN", "")
	t.Setenv("MEMQL_NODE_BOOTSTRAP_TOKEN", "secret")
	t.Setenv("MEMQL_IDENTITY_VERIFIER_BASE_URL", srv.URL)
	return b
}

func (b *countingBootstrap) count() int64 { return atomic.LoadInt64(&b.mints) }

// refusingNodeService refuses every stream before the handshake with a fixed
// status, the way a drained pod refused them.
type refusingNodeService struct {
	nodev1.UnimplementedNodeServiceServer
	code    codes.Code
	msg     string
	refused int64
}

func (s *refusingNodeService) Stream(nodev1.NodeService_StreamServer) error {
	atomic.AddInt64(&s.refused, 1)
	return status.Error(s.code, s.msg)
}

// serveNodeService runs svc on a loopback listener and returns its address.
func serveNodeService(t *testing.T, svc nodev1.NodeServiceServer) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	nodev1.RegisterNodeServiceServer(srv, svc)
	done := make(chan struct{})
	go func() {
		_ = srv.Serve(listener)
		close(done)
	}()
	t.Cleanup(func() {
		srv.Stop()
		<-done
	})
	return listener.Addr().String()
}

// dialLikeProduction builds a connection wired the way WorkerDialer and
// ParentConnector wire theirs.
func dialLikeProduction(identity *Identity, addr string) *peerConnection {
	pc := newPeerConnection(identity, "", addr, testLogger())
	pc.SetHeartbeatInterval(100 * time.Millisecond)
	if identity.CanRemintBearerToken() {
		pc.SetReauthFn(func(ctx context.Context, rejected string) (string, error) {
			return identity.RefreshRejectedBearerToken(ctx, testLogger(), rejected)
		})
	}
	return pc
}

// TestNodeTokenMintedOnceAcrossReconnects is the regression test for the
// churn. One process holds several connections, as a bff's WorkerDialer does,
// to peers that refuse every stream, and each connection makes several
// attempts. The process mints ONE token, at boot, however the peer refuses.
//
//   - "unauthenticated" is how a drained pod refused before this change. The
//     token was minted moments ago, so a new one cannot help and the process
//     must not mint it. Before the fix every refusal minted and re-dialled at
//     once: this subtest counted hundreds of mints.
//   - "unavailable" is how it refuses now (node_token_revocation.go), which
//     is not a verdict on the credential at all.
func TestNodeTokenMintedOnceAcrossReconnects(t *testing.T) {
	cases := []struct {
		name string
		code codes.Code
		msg  string
	}{
		{"unauthenticated", codes.Unauthenticated, "node token revocation check failed"},
		{"unavailable", codes.Unavailable, "node token revocation check unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bootstrap := newCountingBootstrap(t)
			identity := &Identity{ID: "bff-churn", Type: NodeTypeBFF, Address: "bff:50058"}
			require.NoError(t, identity.EnsureBearerToken(context.Background(), testLogger()))
			require.EqualValues(t, 1, bootstrap.count(), "the boot mint")
			require.True(t, identity.CanRemintBearerToken(), "the re-mint path must be live for this test to mean anything")

			const peers = 3
			const attemptsPerPeer = 3
			services := make([]*refusingNodeService, peers)
			ctx, cancel := context.WithCancel(context.Background())
			var wg sync.WaitGroup
			for i := range services {
				services[i] = &refusingNodeService{code: tc.code, msg: tc.msg}
				pc := dialLikeProduction(identity, serveNodeService(t, services[i]))
				wg.Add(1)
				go func() {
					defer wg.Done()
					_ = pc.Connect(ctx, func(*nodev1.NodeServerMessage) {})
				}()
			}
			t.Cleanup(func() {
				cancel()
				wg.Wait()
			})

			for i, svc := range services {
				waitForCount(t, &svc.refused, attemptsPerPeer, 15*time.Second,
					fmt.Sprintf("peer %d refusing %d attempts", i, attemptsPerPeer))
			}
			cancel()
			wg.Wait()

			var refused int64
			for _, svc := range services {
				refused += atomic.LoadInt64(&svc.refused)
			}
			assert.GreaterOrEqual(t, refused, int64(peers*attemptsPerPeer))
			assert.EqualValues(t, 1, bootstrap.count(),
				"%d refused attempts across %d connections must not mint beyond the boot token", refused, peers)
			assert.Equal(t, churnJWT(1, churnTokenExp), identity.BearerTokenValue())
		})
	}
}

// TestIsAuthRejection_RevocationCheckUnavailable pins the client half of the
// server fix: the answer a node gives when it could not finish checking a
// token is not a rejected credential.
func TestIsAuthRejection_RevocationCheckUnavailable(t *testing.T) {
	assert.False(t, isAuthRejection(status.Error(codes.Unavailable, "node token revocation check unavailable")))
	assert.True(t, isAuthRejection(status.Error(codes.Unauthenticated, "invalid or expired token: verifier: unknown kid")))
}

// TestRefreshRejectedBearerToken_OneMintPerRejectedToken covers the decision
// itself. A token minted long ago that a peer refuses is replaced once, however
// many connections report it; the replacement, refused in its turn, is not
// replaced again inside the floor.
func TestRefreshRejectedBearerToken_OneMintPerRejectedToken(t *testing.T) {
	bootstrap := newCountingBootstrap(t)
	identity := &Identity{ID: "agent-rotation", Type: NodeTypeAgent}
	require.NoError(t, identity.EnsureBearerToken(context.Background(), testLogger()))
	stranded := identity.BearerTokenValue()
	ageBootMint(identity)

	const reporters = 8
	got := make([]string, reporters)
	var wg sync.WaitGroup
	for i := 0; i < reporters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tok, err := identity.RefreshRejectedBearerToken(context.Background(), testLogger(), stranded)
			assert.NoError(t, err)
			got[i] = tok
		}(i)
	}
	wg.Wait()

	replacement := churnJWT(2, churnTokenExp)
	for i, tok := range got {
		assert.Equal(t, replacement, tok, "reporter %d", i)
	}
	assert.EqualValues(t, 2, bootstrap.count(), "boot mint plus ONE replacement for %d reporters", reporters)

	// The replacement is refused too: the refusal is about the peer.
	_, err := identity.RefreshRejectedBearerToken(context.Background(), testLogger(), replacement)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errRemintSuppressed), "got %v", err)
	assert.EqualValues(t, 2, bootstrap.count())

	// A connection still holding the stranded token is handed the current one.
	tok, err := identity.RefreshRejectedBearerToken(context.Background(), testLogger(), stranded)
	require.NoError(t, err)
	assert.Equal(t, replacement, tok)
	assert.EqualValues(t, 2, bootstrap.count())
}

// TestPeerConnection_KeyRotationRecoversWithOneMint runs memql#1521's recovery
// through the production hook: the peer stops honouring the boot token (its
// signing key rotated), the connection gets ONE new token and the next dial
// completes the handshake.
func TestPeerConnection_KeyRotationRecoversWithOneMint(t *testing.T) {
	bootstrap := newCountingBootstrap(t)
	identity := &Identity{ID: "agent-rotated", Type: NodeTypeAgent, Address: "agent:50055"}
	require.NoError(t, identity.EnsureBearerToken(context.Background(), testLogger()))
	ageBootMint(identity)

	svc := newKeyRotatingNodeService("bff-parent", churnJWT(2, churnTokenExp))
	pc := dialLikeProduction(identity, serveNodeService(t, svc))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = pc.Connect(ctx, func(*nodev1.NodeServerMessage) {})
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		pc.Close()
		<-done
	})

	waitForCount(t, &svc.acceptedHandshakes, 1, 5*time.Second, "handshake on the re-minted token")
	assert.EqualValues(t, 1, atomic.LoadInt64(&svc.rejectedHandshakes))
	assert.EqualValues(t, 2, bootstrap.count(), "boot mint plus one re-mint")
}

// TestBearerTokenForDial_RefreshesOnlyNearExpiry covers the one mint that is
// not an answer to a refusal: a token past nodeTokenRefreshFraction of its
// lifetime is replaced before a dial presents it, once.
func TestBearerTokenForDial_RefreshesOnlyNearExpiry(t *testing.T) {
	bootstrap := newCountingBootstrap(t)
	now := time.Now()

	fresh := &Identity{ID: "planner-fresh", Type: NodeTypePlanner}
	freshTok := churnJWT(100, now.Add(time.Hour).Unix())
	fresh.storeMintedToken(freshTok, now)
	assert.Equal(t, freshTok, fresh.BearerTokenForDial(context.Background(), testLogger()))
	assert.EqualValues(t, 0, bootstrap.count(), "a token early in its life is presented as it is")

	ageing := &Identity{ID: "planner-ageing", Type: NodeTypePlanner}
	ageing.storeMintedToken(churnJWT(101, now.Add(5*time.Second).Unix()), now.Add(-100*time.Second))
	refreshed := ageing.BearerTokenForDial(context.Background(), testLogger())
	assert.Equal(t, churnJWT(1, churnTokenExp), refreshed)
	assert.EqualValues(t, 1, bootstrap.count())
	assert.Equal(t, refreshed, ageing.BearerTokenForDial(context.Background(), testLogger()))
	assert.EqualValues(t, 1, bootstrap.count(), "the refreshed token has its whole life ahead of it")

	outOfBand := &Identity{ID: "planner-oob", Type: NodeTypePlanner, BearerToken: churnJWT(102, now.Add(-time.Hour).Unix())}
	assert.Equal(t, outOfBand.BearerToken, outOfBand.BearerTokenForDial(context.Background(), testLogger()))
	assert.EqualValues(t, 1, bootstrap.count(), "a token this process did not mint is never refreshed")
}

// TestParentConnector_ReResolvesAnUnreachableDiscoveredParent covers the
// other half of the 2026-09-28 finding: a bff whose DISCOVERED parent was an
// edge pod the rollout replaced redialled that pod's address every 30 seconds
// for as long as it ran. A discovered parent that stops answering is now
// replaced from the topology.
func TestParentConnector_ReResolvesAnUnreachableDiscoveredParent(t *testing.T) {
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	deadAddr := dead.Addr().String()
	require.NoError(t, dead.Close())

	live := newKeyRotatingNodeService("edge-live", "")
	liveAddr := serveNodeService(t, live)

	var mu sync.Mutex
	var excluded []string
	identity := &Identity{ID: "bff-reresolve", Type: NodeTypeBFF, Address: "bff:50058", ParentAddress: deadAddr}
	identity.parentResolver = func(_ context.Context, exclude string) (string, bool) {
		mu.Lock()
		excluded = append(excluded, exclude)
		mu.Unlock()
		return liveAddr, true
	}
	pm := NewPeerManager(identity, testLogger())
	pc := NewParentConnector(identity, pm, testLogger())
	require.NotNil(t, pc)

	pc.Start(context.Background())
	t.Cleanup(func() { pc.Stop(context.Background()) })

	waitForCount(t, &live.acceptedHandshakes, 1, 15*time.Second, "handshake with the re-resolved parent")
	assert.Equal(t, liveAddr, pc.currentParentAddress())
	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, excluded)
	assert.Equal(t, deadAddr, excluded[0], "re-resolution must skip the address that stopped answering")
}

// TestParentConnector_ConfiguredParentIsNeverAbandoned: an address from
// MEMQL_PARENT_ADDRESS is a Service that routes to a live pod, so it has no
// resolver and the connection never gives up on it.
func TestParentConnector_ConfiguredParentIsNeverAbandoned(t *testing.T) {
	identity := &Identity{ID: "agent-configured", Type: NodeTypeAgent, ParentAddress: "bff-active:50058"}
	pc := NewParentConnector(identity, NewPeerManager(identity, testLogger()), testLogger())
	require.NotNil(t, pc)
	assert.Nil(t, pc.resolveParent)
}

// TestDiscoverParentAddress_SkipsTheAddressThatStoppedAnswering covers the
// topology read both boot discovery and re-resolution make.
func TestDiscoverParentAddress_SkipsTheAddressThatStoppedAnswering(t *testing.T) {
	topology := fakeTopology{rows: []*memqlv1.MemoryNode{
		topologyRow(t, "v1:cluster:node:bff-self", "bff", "10.0.0.1:50058", "healthy"),
		topologyRow(t, "v1:cluster:node:identity-a", "identity", "10.0.0.2:50061", "healthy"),
		topologyRow(t, "v1:cluster:node:edge-old", "edge", "10.0.0.3:50062", "healthy"),
		topologyRow(t, "v1:cluster:node:edge-gone", "edge", "10.0.0.4:50062", "offline"),
		topologyRow(t, "v1:cluster:node:edge-new", "edge", "10.0.0.5:50062", "healthy"),
	}}
	identity := &Identity{ID: "bff-self", Type: NodeTypeBFF, Address: "10.0.0.1:50058"}

	addr, _, err := discoverParentAddress(context.Background(), topology, identity, "")
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.3:50062", addr)

	addr, peerId, err := discoverParentAddress(context.Background(), topology, identity, "10.0.0.3:50062")
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.5:50062", addr)
	assert.Equal(t, "v1:cluster:node:edge-new", peerId)

	identity.Address = "10.0.0.5:50062"
	addr, _, err = discoverParentAddress(context.Background(), topology, identity, "10.0.0.3:50062")
	require.NoError(t, err)
	assert.Empty(t, addr, "nothing left: self, identity, the excluded address and an offline pod")
}

type fakeTopology struct{ rows []*memqlv1.MemoryNode }

func (f fakeTopology) Execute(_ context.Context, query string) (*memqlengine.ExecuteResult, error) {
	if query != parentTopologyQuery {
		return nil, fmt.Errorf("unexpected topology query %q", query)
	}
	return &memqlengine.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: f.rows}}, nil
}

func topologyRow(t *testing.T, id, nodeType, address, health string) *memqlv1.MemoryNode {
	t.Helper()
	payload, err := structpb.NewStruct(map[string]any{
		"nodeType": nodeType,
		"address":  address,
		"health":   health,
	})
	require.NoError(t, err)
	return &memqlv1.MemoryNode{Id: id, Payload: payload}
}

// ageBootMint moves this process's last mint out of the re-mint floor, as if
// the boot token had been minted long before the refusal being tested.
func ageBootMint(identity *Identity) {
	identity.mintMu.Lock()
	identity.lastMintAttempt = time.Now().Add(-2 * nodeRemintMinInterval)
	identity.mintMu.Unlock()
}
