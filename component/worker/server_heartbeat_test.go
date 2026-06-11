package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// newHeartbeatTestSession builds a streamSession wired to the fake
// store with a controllable clock, bypassing the gRPC stream -- the
// heartbeat path never touches the stream.
func newHeartbeatTestSession(store Store, clock func() time.Time) *streamSession {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := newServer(logger, store, NewRegistry(nil, clock), nil, clock)
	w := &Worker{
		RegistrationId: "reg-1",
		OwnerUserId:    "user-1",
	}
	ctx, cancel := context.WithCancel(context.Background())
	return newStreamSession(srv, nil, w, ctx, cancel)
}

func beatAt(s *streamSession, at time.Time) {
	s.handleHeartbeat(&memqlv1.Heartbeat{Ts: timestamppb.New(at)}, "10.0.0.1:1234")
}

// TestHandleHeartbeat_BatchesPersistence is the memql#1340 contract:
// the FIRST heartbeat of a stream persists immediately; subsequent
// beats inside HeartbeatBatchInterval only touch the in-memory
// registry; the first beat at/after the interval boundary persists
// again.
func TestHandleHeartbeat_BatchesPersistence(t *testing.T) {
	t0 := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	store := &fakeRegistrationStore{}
	session := newHeartbeatTestSession(store, func() time.Time { return t0 })
	defer session.cancel()

	// First heartbeat after (re)connect persists immediately.
	beatAt(session, t0)
	if len(store.lastSeen) != 1 {
		t.Fatalf("first heartbeat must persist, got %d writes", len(store.lastSeen))
	}

	// 15s-cadence beats within the interval are registry-only.
	for _, offset := range []time.Duration{15 * time.Second, 30 * time.Second, 45 * time.Second} {
		beatAt(session, t0.Add(offset))
	}
	if len(store.lastSeen) != 1 {
		t.Fatalf("within-interval beats must not persist, got %d writes", len(store.lastSeen))
	}
	if got := session.worker.LastSeenAt; !got.Equal(t0.Add(45 * time.Second)) {
		t.Fatalf("in-memory registry must stay fresh on every beat, got %v", got)
	}

	// The first beat at/after the interval boundary persists again.
	beatAt(session, t0.Add(HeartbeatBatchInterval))
	if len(store.lastSeen) != 2 {
		t.Fatalf("post-interval beat must persist, got %d writes", len(store.lastSeen))
	}
	if got := store.lastSeenAts[1]; !got.Equal(t0.Add(HeartbeatBatchInterval)) {
		t.Fatalf("persisted timestamp must be the beat time, got %v", got)
	}

	// And the throttle re-arms from the new flush.
	beatAt(session, t0.Add(HeartbeatBatchInterval+15*time.Second))
	if len(store.lastSeen) != 2 {
		t.Fatalf("throttle must re-arm after a flush, got %d writes", len(store.lastSeen))
	}
}

// TestHandleHeartbeat_FailedFlushRetriesNextBeat: a failed DB write
// must not advance the throttle window -- the next beat retries
// instead of waiting a full interval.
func TestHandleHeartbeat_FailedFlushRetriesNextBeat(t *testing.T) {
	t0 := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	store := &fakeRegistrationStore{lastSeenErr: errors.New("db down")}
	session := newHeartbeatTestSession(store, func() time.Time { return t0 })
	defer session.cancel()

	beatAt(session, t0)
	if len(store.lastSeen) != 0 {
		t.Fatalf("failed flush must record nothing, got %d writes", len(store.lastSeen))
	}

	store.lastSeenErr = nil
	beatAt(session, t0.Add(15*time.Second))
	if len(store.lastSeen) != 1 {
		t.Fatalf("beat after a failed flush must retry, got %d writes", len(store.lastSeen))
	}

	// Once a flush succeeds the throttle engages normally.
	beatAt(session, t0.Add(30*time.Second))
	if len(store.lastSeen) != 1 {
		t.Fatalf("throttle must engage after the successful retry, got %d writes", len(store.lastSeen))
	}
}
