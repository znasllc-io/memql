package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// The transport must end even when no more client messages arrive. Canceling
// a context derived from ServerStream.Context does not interrupt its Recv.
type terminationStore struct{ Store }

func (terminationStore) WorkerByIdentityId(context.Context, string, string) (*RegistrationRow, error) {
	return &RegistrationRow{ID: "reg-1", IdentityId: "identity", OwnerUserId: "owner"}, nil
}
func (terminationStore) RefreshRegistration(context.Context, RegistrationRow) error { return nil }
func (terminationStore) WorkersForUser(context.Context, string) ([]RegistrationRow, error) {
	return nil, nil
}
func (terminationStore) ClearConnectedNode(context.Context, string, string, string) error {
	return nil
}

type terminationAuthStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s terminationAuthStream) Context() context.Context { return s.ctx }

func terminationServer(t *testing.T, node string) (memqlv1.WorkerServiceClient, *Registry) {
	t.Helper()
	registry := NewRegistry(nil, time.Now)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := newServer(logger, terminationStore{}, registry, nil, time.Now, node)
	handler.pingFirst = time.Hour
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(grpc.StreamInterceptor(func(srv any, stream grpc.ServerStream, _ *grpc.StreamServerInfo, next grpc.StreamHandler) error {
		ctx := ContextWithWorkerIdentity(stream.Context(), &WorkerIdentity{IdentityId: "identity", OwnerUserId: "owner", Active: true})
		return next(srv, terminationAuthStream{ServerStream: stream, ctx: ctx})
	}))
	memqlv1.RegisterWorkerServiceServer(server, handler)
	go func() { _ = server.Serve(listener) }()
	conn, err := grpc.NewClient("passthrough:///worker-termination", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		server.Stop()
		_ = listener.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(); server.Stop(); _ = listener.Close() })
	return memqlv1.NewWorkerServiceClient(conn), registry
}

func registerTerminationClient(t *testing.T, client memqlv1.WorkerServiceClient) memqlv1.WorkerService_StreamClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	stream, err := client.Stream(ctx)
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
	return stream
}

func assertTerminatedStream(t *testing.T, stream memqlv1.WorkerService_StreamClient) {
	t.Helper()
	done := make(chan error, 1)
	go func() { _, err := stream.Recv(); done <- err }()
	select {
	case err := <-done:
		if status.Code(err) != codes.Canceled {
			t.Fatalf("terminated stream returned %v, want Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("terminated stream still waits for another client message")
	}
}

func TestReconnectEndsPredecessorAndPendingWorkOnTheSameReplica(t *testing.T) {
	client, registry := terminationServer(t, "agent-a")
	first := registerTerminationClient(t, client)
	old := registry.WorkerById("reg-1")
	callDone := make(chan error, 1)
	go func() {
		_, err := old.Dispatch(context.Background(), &memqlv1.ToolDispatch{CallId: "interrupted-call"})
		callDone <- err
	}()
	message, err := first.Recv()
	if err != nil || message.GetToolDispatch().GetCallId() != "interrupted-call" {
		t.Fatalf("pending dispatch: %v, %v", message, err)
	}
	second := registerTerminationClient(t, client)
	assertTerminatedStream(t, first)
	select {
	case err := <-callDone:
		if !errors.Is(err, ErrWorkerDisconnected) {
			t.Fatalf("interrupted work returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("interrupted work remains pending after reconnect")
	}
	live := registry.WorkerById("reg-1")
	if live == nil || live == old {
		t.Fatal("closing the predecessor removed its successor")
	}
	go func() {
		_, err := live.Dispatch(context.Background(), &memqlv1.ToolDispatch{CallId: "successor-call"})
		callDone <- err
	}()
	message, err = second.Recv()
	if err != nil || message.GetToolDispatch().GetCallId() != "successor-call" {
		t.Fatalf("successor dispatch: %v, %v", message, err)
	}
	if err := second.Send(&memqlv1.WorkerClientMessage{Payload: &memqlv1.WorkerClientMessage_ToolResult{
		ToolResult: &memqlv1.ToolResult{CallId: "successor-call"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := <-callDone; err != nil {
		t.Fatalf("successor call failed: %v", err)
	}
	if err := second.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("ordinary client half-close returned %v, want EOF", err)
	}
}

func TestRemoteRegistrationEventEndsAnIdleGRPCStream(t *testing.T) {
	clientA, registryA := terminationServer(t, "agent-a")
	first := registerTerminationClient(t, clientA)
	clientB, registryB := terminationServer(t, "agent-b")
	second := registerTerminationClient(t, clientB)
	watcher := quietWatcher(registryA, "agent-a")
	if got := watcher.Observe(context.Background(), RegistrationEvent{RegistrationId: "reg-1", ConnectedNodeId: "agent-b"}); got != DisconnectReasonSuperseded {
		t.Fatalf("remote registration verdict = %q", got)
	}
	assertTerminatedStream(t, first)
	if registryA.WorkerById("reg-1") != nil || registryB.WorkerById("reg-1") == nil {
		t.Fatal("the former holder must retire without removing the remote successor")
	}
	registryB.Terminate("reg-1", DisconnectReasonRevoked)
	assertTerminatedStream(t, second)
}
