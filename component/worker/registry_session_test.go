package worker

import (
	"log/slog"
	"testing"
	"time"
)

func TestRemoveSessionPreservesSuccessorAfterReconnect(t *testing.T) {
	// Prod: reclaim/reconnect Add's the new session, then the dying session's
	// close called Remove(registrationId) and wiped the successor while
	// connectedNodeId still named this replica → Ask StreamHeld + WorkerById nil.
	reg := NewRegistry(slog.Default(), time.Now)
	old := &Worker{RegistrationId: "c938433d", OwnerUserId: "owner", Name: "Jose"}
	reg.Add(old)
	successor := &Worker{RegistrationId: "c938433d", OwnerUserId: "owner", Name: "Jose"}
	reg.Add(successor)
	if got := reg.WorkerById("c938433d"); got != successor {
		t.Fatalf("after reconnect Add, live worker = %p, want successor %p", got, successor)
	}
	reg.RemoveSession(old)
	if got := reg.WorkerById("c938433d"); got != successor {
		t.Fatalf("RemoveSession(old) dropped successor: got %p want %p (Connected would still be true in DB)", got, successor)
	}
	reg.RemoveSession(successor)
	if got := reg.WorkerById("c938433d"); got != nil {
		t.Fatalf("RemoveSession(live) must clear, got %p", got)
	}
}

func TestRemoveByIdStillClearsCurrent(t *testing.T) {
	reg := NewRegistry(slog.Default(), time.Now)
	w := &Worker{RegistrationId: "reg-1", OwnerUserId: "owner"}
	reg.Add(w)
	reg.Remove("reg-1")
	if reg.WorkerById("reg-1") != nil {
		t.Fatal("Remove(id) must still clear the current entry (drain / admin paths)")
	}
}
