package worker

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// A stream end counts as a drop only when it cannot have been Cockpit's own
// choice (network-aware fleet selection, section 3 of its design record):
// replaced before the cluster saw it end, or ended with work in flight. The
// table is the 2026-11-24 cases plus the cluster's own decisions, which never
// count whatever was in flight.
func TestClassifyStreamEnd(t *testing.T) {
	for _, c := range []struct {
		name     string
		reason   string
		inFlight WorkInFlight
		cause    string
		drop     bool
	}{
		{"replaced while idle", DisconnectReasonSuperseded, WorkInFlight{}, DropCauseReplaced, true},
		{"replaced with a call in flight", DisconnectReasonSuperseded, WorkInFlight{ToolCalls: 1}, DropCauseReplaced, true},
		{"lost with a tool call in flight", "", WorkInFlight{ToolCalls: 1}, DropCauseWorkInFlight, true},
		{"lost with an app session in flight", "", WorkInFlight{AppSessions: 1}, DropCauseWorkInFlight, true},
		{"lost with an app sign-in in flight", "", WorkInFlight{AppSignIns: 1}, DropCauseWorkInFlight, true},
		{"lost with a model call in flight", "", WorkInFlight{ModelCalls: 2}, DropCauseWorkInFlight, true},
		{"lost with a model pull in flight", "", WorkInFlight{ModelPulls: 1}, DropCauseWorkInFlight, true},
		{"ended idle with no decision behind it", "", WorkInFlight{}, "", false},
		{"drained by the cluster", DisconnectReasonServerDrain, WorkInFlight{ToolCalls: 1}, "", false},
		{"revoked", DisconnectReasonRevoked, WorkInFlight{ToolCalls: 1}, "", false},
		{"token expired", DisconnectReasonTokenExpired, WorkInFlight{ModelCalls: 1}, "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			cause, drop := classifyStreamEnd(c.reason, c.inFlight)
			if cause != c.cause || drop != c.drop {
				t.Fatalf("classifyStreamEnd(%q, %+v) = (%q, %v), want (%q, %v)", c.reason, c.inFlight, cause, drop, c.cause, c.drop)
			}
		})
	}
	if got := (WorkInFlight{ToolCalls: 1, AppSessions: 2, AppSignIns: 3, ModelCalls: 4, ModelPulls: 5}).Total(); got != 15 {
		t.Fatalf("Total() = %d, want 15", got)
	}
}

func TestAReplacedStreamRecordsOneDrop(t *testing.T) {
	f := newDropFixture(t, "agent-a")
	first, _ := f.register(t)
	f.register(t) // the same machine connects again before this replica saw the first end
	assertTerminatedStream(t, first)
	f.waitClosed(t, 1)
	drops := f.store.recorded()
	if len(drops) != 1 {
		t.Fatalf("recorded %d drops, want one for the replaced stream: %+v", len(drops), drops)
	}
	d := drops[0]
	if d.Cause != DropCauseReplaced || d.RegistrationId != "reg-1" || d.OwnerUserId != "owner" || d.NodeId != "agent-a" {
		t.Fatalf("drop = %+v, want reg-1 of owner replaced on agent-a", d)
	}
	if d.ConnectedAt.IsZero() || d.DroppedAt.IsZero() || d.DroppedAt.Before(d.ConnectedAt) {
		t.Fatalf("drop times = connected %s, dropped %s", d.ConnectedAt, d.DroppedAt)
	}
	if d.Reason != DisconnectReasonSuperseded {
		t.Fatalf("drop reason = %q, want the log line's %q", d.Reason, DisconnectReasonSuperseded)
	}
}

func TestAStreamReplacedOnAnotherReplicaRecordsOneDrop(t *testing.T) {
	a, b := newDropFixture(t, "agent-a"), newDropFixture(t, "agent-b")
	first, _ := a.register(t)
	b.register(t)
	watcher := quietWatcher(a.registry, "agent-a")
	if got := watcher.Observe(context.Background(), RegistrationEvent{RegistrationId: "reg-1", ConnectedNodeId: "agent-b"}); got != DisconnectReasonSuperseded {
		t.Fatalf("remote registration verdict = %q", got)
	}
	assertTerminatedStream(t, first)
	a.waitClosed(t, 1)
	if drops := a.store.recorded(); len(drops) != 1 || drops[0].Cause != DropCauseReplaced || drops[0].NodeId != "agent-a" {
		t.Fatalf("the replica that held the replaced stream recorded %+v, want one replaced drop from agent-a", drops)
	}
	if drops := b.store.recorded(); len(drops) != 0 {
		t.Fatalf("the replica holding the new stream recorded %+v, want none", drops)
	}
}

func TestAStreamLostWithWorkInFlightRecordsOneDrop(t *testing.T) {
	f := newDropFixture(t, "agent-a")
	stream, cancel := f.register(t)
	worker := f.registry.WorkerById("reg-1")
	callDone := make(chan error, 1)
	go func() {
		_, err := worker.Dispatch(context.Background(), &memqlv1.ToolDispatch{CallId: "running-step"})
		callDone <- err
	}()
	if msg, err := stream.Recv(); err != nil || msg.GetToolDispatch().GetCallId() != "running-step" {
		t.Fatalf("pending dispatch: %v, %v", msg, err)
	}
	cancel() // the machine's link goes away with the step running
	select {
	case <-callDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the call in flight was not ended with the stream")
	}
	f.waitClosed(t, 1)
	drops := f.store.recorded()
	if len(drops) != 1 || drops[0].Cause != DropCauseWorkInFlight || drops[0].WorkInFlight != 1 {
		t.Fatalf("recorded %+v, want one work-in-flight drop with one call", drops)
	}
}

func TestAnIdleCloseAndADrainRecordNoDrop(t *testing.T) {
	t.Run("an idle close is Cockpit's own choice", func(t *testing.T) {
		f := newDropFixture(t, "agent-a")
		stream, _ := f.register(t)
		if err := stream.CloseSend(); err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Recv(); err != io.EOF {
			t.Fatalf("half-close returned %v, want EOF", err)
		}
		f.waitClosed(t, 1)
		if drops := f.store.recorded(); len(drops) != 0 {
			t.Fatalf("an idle close recorded %+v", drops)
		}
	})
	t.Run("a revocation is the cluster's decision, whatever is in flight", func(t *testing.T) {
		f := newDropFixture(t, "agent-a")
		stream, _ := f.register(t)
		worker := f.registry.WorkerById("reg-1")
		go func() { _, _ = worker.Dispatch(context.Background(), &memqlv1.ToolDispatch{CallId: "running-step"}) }()
		if msg, err := stream.Recv(); err != nil || msg.GetToolDispatch().GetCallId() != "running-step" {
			t.Fatalf("pending dispatch: %v, %v", msg, err)
		}
		f.registry.Terminate("reg-1", DisconnectReasonRevoked)
		assertTerminatedStream(t, stream)
		f.waitClosed(t, 1)
		if drops := f.store.recorded(); len(drops) != 0 {
			t.Fatalf("a revocation recorded %+v", drops)
		}
	})
}

// dropStore is terminationStore plus the drop recorder.
type dropStore struct {
	terminationStore
	mu    sync.Mutex
	drops []ConnectionDrop
}

func (s *dropStore) RecordConnectionDrop(_ context.Context, d ConnectionDrop) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drops = append(s.drops, d)
	return nil
}

func (s *dropStore) recorded() []ConnectionDrop {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ConnectionDrop(nil), s.drops...)
}

// lockedBuffer is a log sink the server writes from its stream goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) count(s string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Count(b.buf.String(), s)
}

type dropFixture struct {
	client   memqlv1.WorkerServiceClient
	registry *Registry
	store    *dropStore
	log      *lockedBuffer
}

// newDropFixture is terminationServer's harness with a recording store and a
// log the test can wait on: the "worker disconnected" line is written after the
// drop decision, so once it appears the decision has been made.
func newDropFixture(t *testing.T, node string) *dropFixture {
	t.Helper()
	f := &dropFixture{registry: NewRegistry(nil, time.Now), store: &dropStore{}, log: &lockedBuffer{}}
	handler := newServer(slog.New(slog.NewTextHandler(f.log, nil)), f.store, f.registry, nil, time.Now, node)
	handler.pingFirst = time.Hour
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.StreamInterceptor(func(srv any, stream grpc.ServerStream, _ *grpc.StreamServerInfo, next grpc.StreamHandler) error {
		ctx := ContextWithWorkerIdentity(stream.Context(), &WorkerIdentity{IdentityId: "identity", OwnerUserId: "owner", Active: true})
		return next(srv, terminationAuthStream{ServerStream: stream, ctx: ctx})
	}))
	memqlv1.RegisterWorkerServiceServer(server, handler)
	go func() { _ = server.Serve(listener) }()
	conn, err := grpc.NewClient("passthrough:///worker-drops", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		server.Stop()
		_ = listener.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(); server.Stop(); _ = listener.Close() })
	f.client = memqlv1.NewWorkerServiceClient(conn)
	return f
}

// register opens and registers a stream whose context the test may cancel.
func (f *dropFixture) register(t *testing.T) (memqlv1.WorkerService_StreamClient, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	stream, err := f.client.Stream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&memqlv1.WorkerClientMessage{Payload: &memqlv1.WorkerClientMessage_Register{
		Register: &memqlv1.Register{Capabilities: []string{CapabilityHeadless}},
	}}); err != nil {
		t.Fatal(err)
	}
	message, err := stream.Recv()
	if err != nil || message.GetRegisterAck().GetRegistrationId() != "reg-1" {
		t.Fatalf("registration: %v, %v", message, err)
	}
	return stream, cancel
}

// waitClosed waits for n "worker disconnected" lines on this replica.
func (f *dropFixture) waitClosed(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for f.log.count(`msg="worker disconnected"`) < n {
		if time.Now().After(deadline) {
			t.Fatalf("this replica logged %d stream ends, want %d", f.log.count(`msg="worker disconnected"`), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
