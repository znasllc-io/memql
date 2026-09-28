package worker

import (
	"context"
	"testing"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// runner_heartbeat_test.go -- the HOLDER's heartbeat, which is what the stale
// sweeps judge a live session by.
//
// Two rows go stale when the replica holding a session dies, and nothing else
// ends either: the v1:worker:appSession row (workerAppSessionStaleSweep reads
// its heartbeatAt) and, when the session is recorded, the v1:work:run holding
// the recording (the work sweep abandons a running run whose heartbeat is
// older than a minute). Both beats come from the drain, so both are measured
// here, with the flush shortened so a test does not have to wait the
// production two seconds.
//
// TO CONFIRM THESE ARE LOAD-BEARING: drop the heartbeat from the progress
// write, or the nil-recording branch from the flush, and they fail.

func (s *recordingAppSessionStore) heartbeats() []AppSessionRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []AppSessionRow
	for _, r := range s.appends {
		if !r.HeartbeatAt.IsZero() {
			out = append(out, r)
		}
	}
	return out
}

// endAfter ends the fixture's session once it has been held for a while, so
// several flushes happen in between.
func endAfter(t *testing.T, session *streamSession, held time.Duration) {
	t.Helper()
	go func() {
		waitForSession(t, session, "sess-run")
		time.Sleep(held)
		session.handleAppSessionEnd(&memqlv1.AppSessionEnd{SessionId: "sess-run"})
	}()
}

// TestTheHolderHeartbeatsTheRowWithNoRecorder: a node with no recorder wired
// writes nothing from the recording, and it used to write nothing at all --
// so the only evidence that the session was held was a row that never changed,
// which the sweep cannot tell from a row whose replica has gone.
func TestTheHolderHeartbeatsTheRowWithNoRecorder(t *testing.T) {
	runner, session, store, _ := newRunnerFixture(t, SubscriptionPresent)
	runner.flushEvery = 5 * time.Millisecond
	// deliberately no Recorder.
	endAfter(t, session, 80*time.Millisecond)

	if _, err := runner.Run(context.Background(), session.worker, runSpec(), nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	beats := store.heartbeats()
	if len(beats) == 0 {
		t.Fatal("a session held with no recorder wired never heartbeat its row; the stale sweep would fail it while it ran")
	}
	for _, b := range beats {
		// A heartbeat is not a chunk. `running` means the first chunk
		// arrived, and a write that promoted the row would say something
		// the session never did.
		if b.Status != "" {
			t.Errorf("a no-recorder heartbeat named status %q; it must name none", b.Status)
		}
	}
}

// TestTheRecordingPublishIsTheHoldersHeartbeat: with a recorder, the progress
// write the drain already makes carries the row's heartbeat, and the recording
// RUN is kept alive too -- throttled, because every run write is broadcast to
// every replica and the work sweep's window is a minute, not two seconds.
func TestTheRecordingPublishIsTheHoldersHeartbeat(t *testing.T) {
	runner, session, store, rec, _ := recordingFixture(t)
	runner.flushEvery = 5 * time.Millisecond
	runner.runHeartbeatEvery = 30 * time.Millisecond
	endAfter(t, session, 150*time.Millisecond)

	if _, err := runner.Run(context.Background(), session.worker, runSpec(), nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(store.heartbeats()) == 0 {
		t.Fatal("the drain's progress write carried no heartbeat; the stale sweep would fail a held session")
	}

	beats := rec.runBeats()
	if len(beats) < 2 {
		t.Fatalf("the recording run was heartbeat %d time(s) over several throttle windows, want at least 2 -- "+
			"a run with no heartbeat is abandoned by the work sweep a minute in", len(beats))
	}
	publishes := len(store.appends)
	if len(beats) >= publishes {
		t.Errorf("the run was heartbeat on %d of %d publishes; it must be throttled, not written every flush", len(beats), publishes)
	}
	for _, b := range beats {
		if b.RunId != "v1:work:run:opened-here" || b.OwnerUserId != "user-1" || b.At.IsZero() {
			t.Errorf("heartbeat = %+v, want the recording run under the owner, with a time", b)
		}
	}
}
