package worker

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

func TestRegistryRegistrationIdentitySurvivesReadbackAndReconnect(t *testing.T) {
	const bare = "new-machine"
	const canonical = RegistrationConcept + ":" + bare
	for _, firstID := range []string{bare, canonical} {
		t.Run(firstID, func(t *testing.T) {
			r := NewRegistry(slog.Default(), time.Now)
			first := &Worker{RegistrationId: firstID, OwnerUserId: "owner"}
			r.Add(first)
			for _, id := range []string{bare, canonical} {
				if r.WorkerById(id) != first {
					t.Fatalf("live stream registered as %q is missing under %q", firstID, id)
				}
			}
			if r.WorkerById("v1:identity:user:"+bare) != nil {
				t.Fatal("another concept must not alias a registration with the same suffix")
			}
			nextID := canonical
			if firstID == canonical {
				nextID = bare
			}
			next := &Worker{RegistrationId: nextID, OwnerUserId: "owner"}
			r.Add(next)
			r.RemoveSession(first)
			if r.WorkerById(bare) != next || len(r.Snapshot()) != 1 || len(r.WorkersForUser("owner")) != 1 {
				t.Fatal("readback must replace the same stream; closing its predecessor must keep the successor")
			}
			r.Remove(canonical)
			if len(r.Snapshot()) != 0 || len(r.WorkersForUser("owner")) != 0 {
				t.Fatal("removal must clear both indexes regardless of ID spelling")
			}
		})
	}
}

func TestRegistryTerminationResolvesOnlyRegistrationIdentity(t *testing.T) {
	r := NewRegistry(slog.Default(), time.Now)
	calls := 0
	w := &Worker{RegistrationId: "machine", OwnerUserId: "owner"}
	w.SetTerminateFunc(func(string) { calls++ })
	r.Add(w)
	if r.Terminate("v1:identity:user:machine", "revoked") || calls != 0 {
		t.Fatal("foreign concept terminated a worker")
	}
	if !r.Terminate(RegistrationConcept+":machine", "revoked") || calls != 1 {
		t.Fatal("registration read from the graph did not terminate its live stream")
	}
	r.RemoveSession(w)
	if r.Terminate("machine", "revoked") {
		t.Fatal("removed stream was still reachable")
	}
}

type registrationAckStream struct {
	fakeWorkerStream
	onAck func(*memqlv1.RegisterAck) error
}

func (s *registrationAckStream) Send(msg *memqlv1.WorkerServerMessage) error {
	if ack := msg.GetRegisterAck(); ack != nil {
		return s.onAck(ack)
	}
	return s.fakeWorkerStream.Send(msg)
}

func TestFailedRegistrationAckCannotRemoveReconnectedSuccessor(t *testing.T) {
	for _, replace := range []bool{false, true} {
		name := "no successor"
		if replace {
			name = "successor with canonical ID"
		}
		t.Run(name, func(t *testing.T) {
			srv := newUpsertTestServer(&fakeRegistrationStore{}, time.Now())
			var successor *Worker
			var id string
			stream := &registrationAckStream{onAck: func(ack *memqlv1.RegisterAck) error {
				id = ack.RegistrationId
				if srv.registry.WorkerById(RegistrationConcept+":"+id) == nil {
					t.Fatal("fresh registration is not dispatchable through its stored identity")
				}
				if replace {
					successor = &Worker{RegistrationId: RegistrationConcept + ":" + id, OwnerUserId: "owner"}
					srv.registry.Add(successor)
				}
				return errors.New("stream closed before acknowledgment")
			}}
			_, err := srv.admitRegistration(context.Background(), stream,
				&WorkerIdentity{IdentityId: "identity", OwnerUserId: "owner", Active: true},
				&memqlv1.Register{Capabilities: []string{CapabilityHeadless, ModelCapability}}, "")
			if err == nil {
				t.Fatal("acknowledgment failure was not returned")
			}
			if id == "" {
				t.Fatalf("registration failed before acknowledgment: %v", err)
			}
			if srv.registry.WorkerById(id) != successor {
				t.Fatal("failed acknowledgment cleanup removed the successor or retained a dead stream")
			}
		})
	}
}
