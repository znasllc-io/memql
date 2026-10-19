package worker

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestReconnectReleasesDispatchOnUnclosedPreviousStream(t *testing.T) {
	old, stream := newToolStreamTestSession()
	defer old.cancel()
	reg := old.server.registry
	reg.Add(old.worker)
	result := make(chan error, 1)
	go func() {
		_, err := old.worker.Dispatch(context.Background(), &memqlv1.ToolDispatch{CallId: "interrupted-build"})
		result <- err
	}()
	waitForSend(t, stream)

	// The replacement arrives before the old transport reports its loss.
	// A live registration must not leave the old call waiting for its deadline.
	next := &Worker{RegistrationId: RegistrationConcept + ":" + old.worker.RegistrationId, OwnerUserId: old.worker.OwnerUserId}
	reg.Add(next)
	select {
	case err := <-result:
		if !errors.Is(err, ErrWorkerDisconnected) {
			t.Fatalf("interrupted dispatch = %v, want disconnected", err)
		}
	case <-time.After(time.Second):
		t.Fatal("reconnected worker left the previous stream's dispatch waiting")
	}
	reg.RemoveSession(old.worker)
	if reg.WorkerById(next.RegistrationId) != next {
		t.Fatal("old session cleanup removed its live successor")
	}
}

func TestReconnectTerminatesOutsideRegistryLockAndPreservesSuccessor(t *testing.T) {
	reg := NewRegistry(nil, time.Now)
	old := &Worker{RegistrationId: "machine", OwnerUserId: "owner"}
	next := &Worker{RegistrationId: "machine", OwnerUserId: "owner"}
	called := make(chan string, 1)
	old.SetTerminateFunc(func(reason string) {
		reg.RemoveSession(old)
		if reg.WorkerById("machine") != next {
			t.Error("termination did not preserve the published successor")
		}
		called <- reason
	})
	reg.Add(old)
	go reg.Add(next)
	select {
	case reason := <-called:
		if reason != DisconnectReasonSuperseded {
			t.Fatalf("termination reason = %q", reason)
		}
	case <-time.After(time.Second):
		t.Fatal("termination blocked on the registry lock")
	}
}

func TestAddingSameWorkerPreservesItsLiveSession(t *testing.T) {
	reg := NewRegistry(nil, time.Now)
	w := &Worker{RegistrationId: "machine", OwnerUserId: "owner"}
	w.SetTerminateFunc(func(string) { t.Fatal("re-adding the same session terminated it") })
	reg.Add(w)
	reg.Add(w)
	if w.disconnected || len(reg.WorkersForUser("owner")) != 1 {
		t.Fatal("re-adding the same session disconnected or duplicated it")
	}
}

// Recv deliberately remains blocked until the transport closes. Cancelling
// a derived session context does not cancel gRPC's stream context: returning
// from the RPC handler is what closes the transport and releases Recv.
type unclosedWorkerStream struct {
	fakeWorkerStream
	registered bool
	ack        chan *memqlv1.RegisterAck
}

func (s *unclosedWorkerStream) Recv() (*memqlv1.WorkerClientMessage, error) {
	if !s.registered {
		s.registered = true
		return &memqlv1.WorkerClientMessage{Payload: &memqlv1.WorkerClientMessage_Register{
			Register: &memqlv1.Register{Capabilities: []string{CapabilityHeadless, ModelCapability}, Labels: map[string]string{ModelLabel("local"): "1"}},
		}}, nil
	}
	<-s.Context().Done()
	return nil, io.EOF
}

func (s *unclosedWorkerStream) Send(msg *memqlv1.WorkerServerMessage) error {
	if ack := msg.GetRegisterAck(); ack != nil {
		s.ack <- ack
	}
	return s.fakeWorkerStream.Send(msg)
}

func TestTerminatingIdleTransportEndsRPCAndOutstandingModelCall(t *testing.T) {
	for _, reason := range []string{DisconnectReasonSuperseded, DisconnectReasonRevoked} {
		t.Run(reason, func(t *testing.T) {
			srv := newUpsertTestServer(&fakeRegistrationStore{}, time.Now())
			transport, closeTransport := context.WithCancel(context.Background())
			stream := &unclosedWorkerStream{ack: make(chan *memqlv1.RegisterAck, 1)}
			stream.ctx = ContextWithWorkerIdentity(transport, &WorkerIdentity{
				IdentityId: "identity", OwnerUserId: "owner", Active: true,
			})
			done := make(chan error, 1)
			finished := make(chan struct{})
			go func() { defer close(finished); done <- srv.Stream(stream) }()
			defer func() { closeTransport(); <-finished }()
			var ack *memqlv1.RegisterAck
			select {
			case ack = <-stream.ack:
			case <-time.After(time.Second):
				t.Fatal("registration was not acknowledged")
			}
			old := srv.registry.WorkerById(ack.RegistrationId)
			handle, err := old.StartModelCall(context.Background(), ModelCallRequest{
				RequestId: "inference", Model: "local", Kind: "chat",
			})
			if err != nil {
				t.Fatal(err)
			}
			var next *Worker
			if reason == DisconnectReasonSuperseded {
				next = &Worker{RegistrationId: ack.RegistrationId, OwnerUserId: "owner"}
				srv.registry.Add(next)
			} else {
				srv.registry.Terminate(ack.RegistrationId, reason)
			}
			select {
			case err := <-done:
				if status.Code(err) != codes.Canceled {
					t.Fatalf("RPC error = %v", err)
				}
			case <-time.After(time.Second):
				closeTransport()
				<-done
				t.Fatal("terminated session left the RPC waiting on the old transport")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := handle.Wait(ctx); !errors.Is(err, ErrWorkerDisconnected) {
				t.Fatalf("model wait = %v, want disconnected", err)
			}
			if srv.registry.WorkerById(ack.RegistrationId) != next {
				t.Fatal("RPC cleanup lost its successor or retained the terminated worker")
			}
		})
	}
}
