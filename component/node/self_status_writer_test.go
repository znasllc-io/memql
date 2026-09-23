package node

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
)

func TestSelfStatusWriterRefreshesEveryRoleWithCurrentLifecycle(t *testing.T) {
	for _, role := range []NodeType{NodeTypeBFF, NodeTypeAgent, NodeTypePlanner, NodeTypeIdentity, NodeTypeWorkbench, NodeTypeEdge, NodeTypeMCP} {
		t.Run(string(role), func(t *testing.T) {
			engine := &stubExecutor{}
			lifecycle := NewNodeLifecycle()
			identity := &Identity{ID: "self-pod", Type: role, Address: "self:50051"}
			writer := newSelfStatusWriter(identity, lifecycle, engine, testLogger())
			require.Equal(t, time.Minute, writer.interval)
			at := time.Date(2026, 9, 9, 6, 0, 0, 0, time.UTC)
			writer.now = func() time.Time { return at }
			require.NoError(t, writer.refresh(context.Background()))
			require.NoError(t, lifecycle.MarkReady())
			at = at.Add(time.Minute)
			require.NoError(t, writer.refresh(context.Background()))
			require.NoError(t, lifecycle.MarkDraining())
			at = at.Add(time.Minute)
			require.NoError(t, writer.refresh(context.Background()))
			require.NoError(t, lifecycle.MarkStopped())
			writer.SyncLifecycle()
			require.NoError(t, writer.refresh(context.Background()))
			writer.SyncLifecycle() // a delayed old observer must reread stopped.
			queries := engine.snapshot()
			require.Len(t, queries, 4)
			for i, health := range []string{"connecting", "healthy", "draining", "stopped"} {
				require.Contains(t, queries[i], `id: "self-pod"`)
				require.Contains(t, queries[i], `nodeType: "`+string(role)+`"`)
				require.Contains(t, queries[i], `health: "`+health+`"`)
			}
			require.Contains(t, queries[1], `lastSeen: "2026-09-09T06:01:00Z"`)
		})
	}
}

type blockedSelfExecutor struct {
	mu          sync.Mutex
	committed   []string
	entered     chan struct{}
	canceled    chan struct{}
	release     chan struct{}
	once        sync.Once
	releaseOnce sync.Once
}

func (e *blockedSelfExecutor) Execute(ctx context.Context, query string) error {
	block := false
	e.once.Do(func() { block = true })
	if block {
		close(e.entered)
		<-ctx.Done()
		close(e.canceled)
		<-e.release // cancellation is not completion; observer must join.
		return ctx.Err()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.committed = append(e.committed, query)
	return nil
}
func TestSelfStatusWriterStoppedBarrierCancelsAndJoinsRefresh(t *testing.T) {
	engine := &blockedSelfExecutor{entered: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(engine.releaseWrite)
	lifecycle := NewNodeLifecycle()
	require.NoError(t, lifecycle.MarkReady())
	writer := newSelfStatusWriter(&Identity{ID: "self", Type: NodeTypeEdge, Address: "self:50057"}, lifecycle, engine, testLogger())
	lifecycle.SetObserver(func(_, _ LifecycleState) { writer.SyncLifecycle() })
	refreshDone := make(chan struct{})
	go func() { defer close(refreshDone); _ = writer.refresh(context.Background()) }()
	awaitSelfSignal(t, engine.entered)
	stopDone := make(chan struct{})
	go func() { defer close(stopDone); _ = lifecycle.MarkStopped() }()
	select {
	case <-engine.canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("stopped transition did not cancel active write")
	}
	select {
	case <-stopDone:
		t.Fatal("stopped observer returned before canceled write joined")
	default:
	}
	engine.releaseWrite()
	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("stopped observer did not finish")
	}
	awaitSelfSignal(t, refreshDone)
	writer.SyncLifecycle()
	require.NoError(t, writer.refresh(context.Background()))
	engine.mu.Lock()
	defer engine.mu.Unlock()
	require.Len(t, engine.committed, 1)
	require.Contains(t, engine.committed[0], `health: "stopped"`)
}

func TestSelfStatusWriterRunCancellationJoinsWrite(t *testing.T) {
	engine := &blockedSelfExecutor{entered: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(engine.releaseWrite)
	lifecycle := NewNodeLifecycle()
	require.NoError(t, lifecycle.MarkReady())
	writer := newSelfStatusWriter(&Identity{ID: "self", Type: NodeTypeBFF, Address: "self:50058"}, lifecycle, engine, testLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ticks := make(chan time.Time)
	done := make(chan struct{})
	go func() { defer close(done); _ = writer.runWithTicks(ctx, func() {}, ticks) }()
	ticks <- time.Now()
	awaitSelfSignal(t, engine.entered)
	cancel()
	awaitSelfSignal(t, engine.canceled)
	select {
	case <-done:
		t.Fatal("run returned before active write finished")
	default:
	}
	engine.releaseWrite()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not join canceled write")
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	require.Empty(t, engine.committed)
}

func TestSelfStatusWriterSteadyHealthyTicksAdvanceBeyondPruneWindow(t *testing.T) {
	engine := &stubExecutor{}
	lifecycle := NewNodeLifecycle()
	require.NoError(t, lifecycle.MarkReady())
	writer := newSelfStatusWriter(&Identity{ID: "self", Type: NodeTypeMCP, Address: "self:50059"}, lifecycle, engine, testLogger())
	at := time.Now().UTC().Add(-35 * time.Minute)
	writer.now = func() time.Time { return at }
	for i := 0; i < 36; i++ {
		require.NoError(t, writer.refresh(context.Background()))
		at = at.Add(time.Minute)
	}
	queries := engine.snapshot()
	require.Len(t, queries, 36)
	require.NotEqual(t, queries[0], queries[35], "steady healthy status must still advance stored lastSeen")
	for _, q := range queries {
		require.True(t, strings.Contains(q, `health: "healthy"`))
	}
}

func TestSelfStatusWriterEveryStoppedObserverWaitsForTerminalWrite(t *testing.T) {
	engine := &blockedSelfExecutor{entered: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(engine.releaseWrite)
	lifecycle := NewNodeLifecycle()
	require.NoError(t, lifecycle.MarkReady())
	writer := newSelfStatusWriter(&Identity{ID: "self", Type: NodeTypeBFF, Address: "self:50058"}, lifecycle, engine, testLogger())
	refreshDone := make(chan struct{})
	go func() { defer close(refreshDone); _ = writer.refresh(context.Background()) }()
	awaitSelfSignal(t, engine.entered)
	require.NoError(t, lifecycle.MarkStopped())
	// A delayed earlier observer can see Stopped and win the terminal latch.
	olderObserverDone := make(chan struct{})
	go func() { defer close(olderObserverDone); writer.SyncLifecycle() }()
	awaitSelfSignal(t, engine.canceled)
	stoppedObserverDone := make(chan struct{})
	go func() { defer close(stoppedObserverDone); writer.SyncLifecycle() }()
	// This timeout tests a forbidden early return while the engine is
	// deterministically blocked, not timing-based work completion.
	select {
	case <-stoppedObserverDone:
		t.Fatal("MarkStopped observer returned while another observer still owns terminal persistence")
	case <-time.After(30 * time.Millisecond):
	}
	engine.releaseWrite()
	for _, done := range []chan struct{}{refreshDone, olderObserverDone, stoppedObserverDone} {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("terminal observers did not join")
		}
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	require.Len(t, engine.committed, 1)
	require.Contains(t, engine.committed[0], `health: "stopped"`)
}

func (e *blockedSelfExecutor) releaseWrite() { e.releaseOnce.Do(func() { close(e.release) }) }
func awaitSelfSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for self-writer lifecycle signal")
	}
}

// THE HEARTBEAT CARRIES WHAT THE NODE HEARS (memql#5338, D7). With the bridge's
// report attached, every refresh writes it onto the node's own row -- from the
// REAL bridge, over a real link, so the test cannot pass on a report shape the
// bridge never produces. Without a reporter the write carries no mesh at all.
func TestSelfStatusWriterCarriesTheMeshReport(t *testing.T) {
	engine := &stubExecutor{}
	identity := &Identity{ID: "edge-1", Type: NodeTypeEdge, Address: "edge-1:50062"}
	writer := newSelfStatusWriter(identity, NewNodeLifecycle(), engine, testLogger())
	writer.now = func() time.Time { return time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC) }

	require.NoError(t, writer.refresh(context.Background()))

	eb := newBridgeFor(t, "edge-1", NodeTypeEdge)
	attachDialedPeer(t, eb.peerManager, "bff-a", NodeTypeBFF)
	eb.ReceiveForward(&nodev1.EventForward{EventId: "e1", Topic: relayTopic, OriginNodeId: "agent-a", Hops: 2}, "bff-a")
	writer.SetMeshReporter(eb.MeshReport)
	require.NoError(t, writer.refresh(context.Background()))

	queries := engine.snapshot()
	require.Len(t, queries, 2)
	require.NotContains(t, queries[0], "mesh", "no reporter, no mesh argument: the stored report stands")
	for _, needle := range []string{`mesh: {`, `"heard":1`, `"receives":true`, `"node":"bff-a"`, `"via":"dialed"`} {
		require.Contains(t, queries[1], needle)
	}
}
