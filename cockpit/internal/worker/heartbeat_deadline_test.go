package worker

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// The peer stays alive but does not read. Like the SDK, its half-close waits
// behind Send; only cancellation of the stream context releases both.
type flowBlockedHeartbeatStream struct {
	ctx     context.Context
	mu      sync.Mutex
	entered chan struct{}
	writes  atomic.Int32
	closes  atomic.Int32
}

func (s *flowBlockedHeartbeatStream) Send(*memqlv1.WorkerClientMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes.Add(1)
	close(s.entered)
	<-s.ctx.Done()
	return s.ctx.Err()
}
func (s *flowBlockedHeartbeatStream) Recv() (*memqlv1.WorkerServerMessage, error) {
	<-s.ctx.Done()
	return nil, s.ctx.Err()
}
func (s *flowBlockedHeartbeatStream) Close() { s.mu.Lock(); defer s.mu.Unlock(); s.closes.Add(1) }

func TestHeartbeatDeadlineEndsFlowControlledSendAndJoinsClose(t *testing.T) {
	oldGrace := closeGrace
	closeGrace = time.Millisecond
	t.Cleanup(func() { closeGrace = oldGrace })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &flowBlockedHeartbeatStream{ctx: ctx, entered: make(chan struct{})}
	conn := &Connection{conn: stream, cancel: cancel}
	fire := make(chan func(), 1)
	r := &Runner{heartbeatDeadline: func(limit time.Duration, expire func()) func() bool {
		if limit != time.Minute {
			t.Errorf("unexpected heartbeat bound %s", limit)
		}
		fire <- expire
		return func() bool { return false }
	}}
	done := make(chan error, 1)
	go func() {
		done <- r.boundedHeartbeat(conn, func() error { return conn.SendHeartbeat(0, nil, nil, nil, nil, nil) })
	}()
	select {
	case <-stream.entered:
	case <-time.After(time.Second):
		t.Fatal("heartbeat did not enter send")
	}
	// Manually advance the watchdog; no one-minute test sleep or reliance on
	// transport keepalive. The real Connection.Close must cancel blocked Send.
	(<-fire)()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "no progress") {
			t.Fatalf("expected timeout, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("heartbeat send/close leaked")
	}
	if stream.writes.Load() != 1 || stream.closes.Load() != 1 || ctx.Err() == nil {
		t.Fatal("watchdog did not close only its stream exactly once")
	}
}

func TestHeartbeatDeadlineCanceledAfterSuccessfulSend(t *testing.T) {
	stream := newScriptStream(0)
	conn := &Connection{conn: stream}
	stopped := 0
	r := &Runner{heartbeatDeadline: func(limit time.Duration, expire func()) func() bool { return func() bool { stopped++; return true } }}
	if err := r.boundedHeartbeat(conn, func() error { return conn.SendHeartbeat(0, nil, nil, nil, nil, nil) }); err != nil {
		t.Fatal(err)
	}
	if stopped != 1 || len(stream.messages()) != 1 {
		t.Fatal("successful heartbeat did not retire its watchdog")
	}
	select {
	case <-stream.closed:
		t.Fatal("healthy idle stream was closed")
	default:
	}
}

func TestNormalHeartbeatReportsHealthAndDisconnectClearsIt(t *testing.T) {
	cluster := newFakeCluster(func(int) answer { return accept() })
	r := runnerAgainst(t, cluster, Options{Config: testHomeConfig(t, "healthy")}, &logBuffer{}, nil)
	r.heartbeat = 5 * time.Millisecond
	observed := make(chan bool, 8)
	r.connectionHealth = func(conn *Connection, healthy bool) {
		if !healthy && r.RegistrationId() != "" {
			t.Error("lost stream stayed registered during cleanup")
		}
		select {
		case observed <- healthy:
		default:
		}
	}
	runInBackground(t, r)
	s := cluster.next(t)
	select {
	case healthy := <-observed:
		if !healthy {
			t.Fatal("normal heartbeat did not establish health")
		}
	case <-time.After(time.Second):
		t.Fatal("no normal heartbeat health")
	}
	s.drop(context.Canceled)
	deadline := time.After(time.Second)
	for {
		select {
		case healthy := <-observed:
			if !healthy {
				return
			}
		case <-deadline:
			t.Fatal("disconnect was not observed")
			return
		}
	}
}
