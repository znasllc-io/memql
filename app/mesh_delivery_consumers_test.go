package app

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/edge"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/node"
)

// THE THREE CONSUMERS memql#5316 FOUND DEAF, OVER THE REAL TRANSPORT
// (memql#5342).
//
// memql#5316's analysis named three things broken on the nodes nobody dialed:
// readiness rows that never recomputed, a providers reload that never reached
// them, and the edge's site cache that only the TTL kept honest. Each already
// had a test -- over a HAND-BUILT bus bridge that forwarded every event to
// every node, which is exactly the assumption the transport did not honour.
// These run each real consumer on the replicas that could not hear, wired onto
// real NodeService streams over loopback, and hold that the consumer RUNS
// there because the event arrived -- not because a backstop fired:
//
//   - readiness: the production recompute loop, with its safety net pushed an
//     hour out, so a pass inside the test can only be the event's;
//   - the edge cache: the real SiteInvalidationSubscriber over the real
//     caching Resolver with an hour's TTL, so a re-query can only be the
//     invalidation's -- and the site write originates on the PRODUCT bff,
//     which reaches the edge only through a relay AND a push;
//   - providers reload: real engines, each running the production
//     subscriber, with the reload published on one bff.
//
// The topology is the cloud's, cut to the replicas that matter: two bffs that
// dial the agent and identity, the agent and the edge parented on the engine
// bff, and an mcp whose discovered parent is the edge.

// TestTheMeshCarriesReadinessAndSiteInvalidationToEveryNode is the half that
// needs no database.
func TestTheMeshCarriesReadinessAndSiteInvalidationToEveryNode(t *testing.T) {
	m := newConsumerMesh(t)

	// READINESS. The loop on every replica, baselined, then one registration
	// change written on the agent -- which holds machine streams, so it is
	// where registrations are written.
	passes := map[string]*atomic.Int32{}
	for _, r := range m.all {
		n := &atomic.Int32{}
		passes[r.id] = n
		memql.StartReadinessRecomputeProbe(m.ctx, r.bus, func(context.Context) (int, error) {
			n.Add(1)
			return 1, nil
		}, memql.ReadinessProbeTimings{
			Debounce: 20 * time.Millisecond, RetryBase: 50 * time.Millisecond,
			RetryMax: 100 * time.Millisecond, SafetyNet: time.Hour,
		})
	}
	time.Sleep(100 * time.Millisecond)
	baseline := map[string]int32{}
	for id, n := range passes {
		baseline[id] = n.Load()
	}
	m.byID["agent-a"].bus.Publish(events.NewEvent(events.TopicNodeUpdated(memql.WorkerRegistrationConcept),
		events.KindNodeUpdated, map[string]any{"id": memql.WorkerRegistrationConcept + ":machine-1"}))
	for _, r := range m.all {
		if r.identity.Type == node.NodeTypeIdentity {
			continue
		}
		n := passes[r.id]
		awaitTrue(t, 5*time.Second, func() bool { return n.Load() > baseline[r.id] },
			"%s never recomputed readiness from the registration change; with the safety net an hour out, only the event could have run it", r.id)
	}
	time.Sleep(200 * time.Millisecond)
	if got := passes["identity-a"].Load(); got != baseline["identity-a"] {
		t.Fatalf("identity recomputed %d time(s) from a broadcast it must never hear", got-baseline["identity-a"])
	}

	// THE EDGE CACHE. Prime it, then write the site on the product bff.
	lookups := &countingSiteLookups{}
	resolver := edge.NewResolver(lookups, time.Hour)
	sub := edge.NewSiteInvalidationSubscriber(quietLogger(), m.byID["edge-a"].bus, resolver)
	sub.Start(m.ctx)
	t.Cleanup(func() { sub.Stop(context.Background()) })
	const host = "shop.example.com"
	for i := 0; i < 2; i++ {
		if _, err := resolver.Resolve(m.ctx, host); err != nil {
			t.Fatal(err)
		}
	}
	if got := lookups.n.Load(); got != 1 {
		t.Fatalf("negative control: the second resolve should be a cache hit, lookups = %d", got)
	}
	m.byID["bff-product-a"].bus.Publish(events.NewEvent(events.TopicNodeUpdated("v1:platform:site"),
		events.KindNodeUpdated, map[string]any{"id": "v1:platform:site:s1", "payload": map[string]any{"hostname": host}}))
	awaitTrue(t, 5*time.Second, func() bool {
		_, _ = resolver.Resolve(m.ctx, host)
		return lookups.n.Load() >= 2
	}, "the edge kept serving its cached site after the product bff wrote it; with an hour's TTL only the invalidation could have evicted it")
}

// TestTheMeshCarriesAProvidersReloadToEveryNode is the half that needs real
// engines, and so a database: the provider registry is built by Init.
func TestTheMeshCarriesAProvidersReloadToEveryNode(t *testing.T) {
	dsn := dbtest.DSN()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		dbtest.Unreachable(t, "mesh providers reload", dsn, err)
		return
	}
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}

	m := newConsumerMesh(t)
	const requestID = "apply-mesh-e2e"
	receivers := map[string]*recordingHandler{}
	for _, id := range []string{"bff-product-a", "edge-a", "mcp-a"} {
		e, err := memql.New(db)
		if err != nil {
			t.Fatal(err)
		}
		logs := &recordingHandler{}
		e.Logger = slog.New(logs)
		if err := e.Init(memorynodes.DefaultRegistry()); err != nil {
			t.Fatal(err)
		}
		e.SetEventBus(m.byID[id].bus)
		e.StartProvidersReloadSubscriber(m.ctx)
		receivers[id] = logs
	}

	// THE APPLY lands on the engine bff; this is what the owner-gated
	// providersReload builtin publishes there.
	m.byID["bff-a"].bus.Publish(events.NewEvent(events.TopicProvidersReloadFor(requestID),
		events.KindProvidersReload, map[string]any{"requestId": requestID}))

	for id, logs := range receivers {
		awaitTrue(t, 10*time.Second, func() bool { return logs.reloaded(requestID) },
			"%s never ran the providers reload for %s: a federation change applied on one bff would not re-resolve there", id, requestID)
	}
}

// ---------------------------------------------------------------------------
// The loopback mesh, from the node package's exported surface
// ---------------------------------------------------------------------------

type consumerReplica struct {
	id       string
	identity *node.Identity
	bus      *events.Bus
	pm       *node.PeerManager
	bridge   *node.EventBridge
}

type consumerMesh struct {
	ctx  context.Context
	all  []*consumerReplica
	byID map[string]*consumerReplica
}

func newConsumerMesh(t *testing.T) *consumerMesh {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := &consumerMesh{ctx: ctx, byID: map[string]*consumerReplica{}}
	for _, spec := range []struct {
		id string
		nt node.NodeType
	}{
		{"bff-a", node.NodeTypeBFF}, {"bff-product-a", node.NodeTypeBFF},
		{"agent-a", node.NodeTypeAgent}, {"edge-a", node.NodeTypeEdge},
		{"mcp-a", node.NodeTypeMCP}, {"identity-a", node.NodeTypeIdentity},
	} {
		m.add(t, spec.id, spec.nt)
	}
	m.dial(t, "bff-a", "agent-a", "identity-a")
	m.dial(t, "bff-product-a", "agent-a", "identity-a")
	m.parent(t, "agent-a", "bff-a")
	m.parent(t, "edge-a", "bff-a")
	m.parent(t, "mcp-a", "edge-a")
	m.awaitLinks(t, [][2]string{
		{"bff-a", "agent-a"}, {"bff-a", "identity-a"},
		{"bff-product-a", "agent-a"}, {"bff-product-a", "identity-a"},
		{"agent-a", "bff-a"}, {"edge-a", "bff-a"}, {"mcp-a", "edge-a"},
	})
	return m
}

// add starts one replica serving NodeService on a loopback port it advertises
// as its own address, the way a pod advertises $(POD_IP):port.
func (m *consumerMesh) add(t *testing.T, id string, nt node.NodeType) {
	t.Helper()
	addr := loopbackAddress(t)
	t.Setenv("MEMQL_NODE_SERVICE_ADDRESS", addr)
	bus := events.NewBus(events.WithLogger(quietLogger()))
	t.Cleanup(bus.Close)
	ident := &node.Identity{ID: id, Type: nt, Address: addr}
	pm := node.NewPeerManager(ident, quietLogger())
	bridge := node.NewEventBridge(ident, bus, pm, quietLogger())
	server := node.NewNodeServer(ident, pm, quietLogger())
	pm.Start(m.ctx)
	bridge.Start(m.ctx)
	server.Start(m.ctx)
	t.Cleanup(func() {
		server.Stop(context.Background())
		bridge.Stop(context.Background())
		pm.Stop(context.Background())
	})
	select {
	case <-server.Ready():
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: node server never came up on %s", id, addr)
	}
	r := &consumerReplica{id: id, identity: ident, bus: bus, pm: pm, bridge: bridge}
	m.all = append(m.all, r)
	m.byID[id] = r
}

func (m *consumerMesh) dial(t *testing.T, from string, to ...string) {
	t.Helper()
	var seeds []node.WorkerTarget
	for _, id := range to {
		seeds = append(seeds, node.WorkerTarget{NodeType: m.byID[id].identity.Type, Address: m.byID[id].identity.Address})
	}
	src := m.byID[from]
	wd := node.NewWorkerDialer(src.identity, src.pm, nil, nil, seeds, quietLogger())
	wd.Start(m.ctx)
	t.Cleanup(func() { wd.Stop(context.Background()) })
}

func (m *consumerMesh) parent(t *testing.T, child, parent string) {
	t.Helper()
	c := m.byID[child]
	c.identity.ParentAddress = m.byID[parent].identity.Address
	pc := node.NewParentConnector(c.identity, c.pm, quietLogger())
	pc.Start(m.ctx)
	t.Cleanup(func() { pc.Stop(context.Background()) })
}

// awaitLinks waits until each dial is a link on BOTH ends, read from the
// replicas' own delivery reports: the dialer reports it dialed, the dialed
// node reports it accepted. That is the state in which both directions can
// carry an event.
func (m *consumerMesh) awaitLinks(t *testing.T, dials [][2]string) {
	t.Helper()
	linked := func(from, to string, via ...string) bool {
		for _, l := range m.byID[from].bridge.MeshReport().Links {
			if l.Node != to {
				continue
			}
			for _, v := range via {
				if l.Via == v {
					return true
				}
			}
		}
		return false
	}
	for _, d := range dials {
		awaitTrue(t, 10*time.Second, func() bool {
			return linked(d[0], d[1], "dialed", "both") && linked(d[1], d[0], "accepted", "both")
		}, "the %s -> %s stream never came up on both ends", d[0], d[1])
	}
}

func loopbackAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func awaitTrue(t *testing.T, within time.Duration, cond func() bool, format string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf(format, args...)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// countingSiteLookups is the edge's query executor: every resolution that is
// not a cache hit is a lookup, which is what the test counts.
type countingSiteLookups struct{ n atomic.Int32 }

func (c *countingSiteLookups) SiteByHostname(_ context.Context, hostname string) (*edge.Site, error) {
	c.n.Add(1)
	return &edge.Site{ID: "v1:platform:site:s1", Hostname: hostname, Kind: "static", Status: "live"}, nil
}
func (c *countingSiteLookups) SiteForCustomDomain(context.Context, string) (*edge.Site, error) {
	return nil, nil
}
func (c *countingSiteLookups) SiteForAccountFrontDoor(context.Context, string) (*edge.Site, error) {
	return nil, nil
}
func (c *countingSiteLookups) StoreByID(context.Context, string) (*edge.BoundStore, error) {
	return nil, nil
}

// recordingHandler keeps the engine's log records so the test can see the
// providers-reload subscriber run -- both of its outcomes log the requestId.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) reloaded(requestID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		if !strings.HasPrefix(r.Message, "provider reload propagation") {
			continue
		}
		found := false
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "requestId" && a.Value.String() == requestID {
				found = true
				return false
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}
