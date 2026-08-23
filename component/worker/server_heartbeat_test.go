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
	srv := newServer(logger, store, NewRegistry(nil, clock), nil, clock, testNodeId)
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
//
// The offsets are sub-interval fractions rather than the cockpit's beat,
// because since memql#4350 HeartbeatBatchInterval IS the cockpit's beat: at
// 15s every real heartbeat lands on the boundary and flushes, which is the
// point of the change (`online` is derived from lastSeenAt, so its freshness
// budget is this cadence). The throttle still exists and still works -- what
// this walks is that it does, using beats closer together than one interval.
func TestHandleHeartbeat_BatchesPersistence(t *testing.T) {
	t0 := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	third := HeartbeatBatchInterval / 3
	store := &fakeRegistrationStore{}
	session := newHeartbeatTestSession(store, func() time.Time { return t0 })
	defer session.cancel()

	// First heartbeat after (re)connect persists immediately.
	beatAt(session, t0)
	if len(store.lastSeen) != 1 {
		t.Fatalf("first heartbeat must persist, got %d writes", len(store.lastSeen))
	}

	// Beats within the interval are registry-only.
	for _, offset := range []time.Duration{third, 2 * third} {
		beatAt(session, t0.Add(offset))
	}
	if len(store.lastSeen) != 1 {
		t.Fatalf("within-interval beats must not persist, got %d writes", len(store.lastSeen))
	}
	if got := session.worker.LastSeenAt; !got.Equal(t0.Add(2 * third)) {
		t.Fatalf("in-memory registry must stay fresh on every beat, got %v", got)
	}

	// The first beat at/after the interval boundary persists again -- which is
	// every beat at the cockpit's own cadence.
	beatAt(session, t0.Add(HeartbeatBatchInterval))
	if len(store.lastSeen) != 2 {
		t.Fatalf("post-interval beat must persist, got %d writes", len(store.lastSeen))
	}
	if got := store.lastSeenAts[1]; !got.Equal(t0.Add(HeartbeatBatchInterval)) {
		t.Fatalf("persisted timestamp must be the beat time, got %v", got)
	}

	// And the throttle re-arms from the new flush.
	beatAt(session, t0.Add(HeartbeatBatchInterval+third))
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
	retryAt := t0.Add(HeartbeatBatchInterval / 3)
	beatAt(session, retryAt)
	if len(store.lastSeen) != 1 {
		t.Fatalf("beat after a failed flush must retry, got %d writes", len(store.lastSeen))
	}

	// Once a flush succeeds the throttle engages normally -- measured from the
	// SUCCESSFUL flush, not from the stream's start.
	beatAt(session, retryAt.Add(HeartbeatBatchInterval/3))
	if len(store.lastSeen) != 1 {
		t.Fatalf("throttle must engage after the successful retry, got %d writes", len(store.lastSeen))
	}
}
