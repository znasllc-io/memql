package worker

import (
	"context"
	"strings"
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

// statusWrites is every landed progress write that named a status.
func (s *recordingAppSessionStore) statusWrites() []AppSessionRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []AppSessionRow
	for _, r := range s.appends {
		if r.Status != "" {
			out = append(out, r)
		}
	}
	return out
}

func (s *recordingAppSessionStore) heartbeatCount() int { return len(s.heartbeats()) }

func statusesOf(rows []AppSessionRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Status
	}
	return out
}

// TestTheHolderPromotesTheRowOnceThenBeatsWithoutAStatus. `running` means the
// first chunk arrived, and the holder says so ONCE. Every other write it makes
// -- each flush, each action's publish -- is a heartbeat and names no status.
//
// It used to name `running` on every flush, and that is what made the stale
// sweep's close unstable: a holder that missed its heartbeats for the grace
// had its row failed by the sweep, and its next flush two seconds later set
// the row back to `running` while it still carried the sweep's endedAt,
// exitCode and errorMessage. A session past the startedAt backstop flapped
// failed/running every two minutes until it ended.
func TestTheHolderPromotesTheRowOnceThenBeatsWithoutAStatus(t *testing.T) {
	runner, session, store, _, _ := recordingFixture(t)
	runner.flushEvery = 5 * time.Millisecond
	go func() {
		waitForSession(t, session, "sess-run")
		// Flushes before any chunk: the row is held, and still `starting`.
		time.Sleep(30 * time.Millisecond)
		session.handleAppSessionChunk(actionChunk(t, 1, map[string]any{"id": "a1", "seq": 1, "tool": "exec"}))
		time.Sleep(30 * time.Millisecond)
		session.handleAppSessionChunk(&memqlv1.AppSessionChunk{SessionId: "sess-run", Stream: "stdout", Data: []byte("thinking\n"), Seq: 2})
		session.handleAppSessionChunk(actionChunk(t, 3, map[string]any{"id": "a2", "seq": 2, "tool": "exec"}))
		time.Sleep(30 * time.Millisecond)
		session.handleAppSessionEnd(&memqlv1.AppSessionEnd{SessionId: "sess-run"})
	}()

	if _, err := runner.Run(context.Background(), session.worker, runSpec(), nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	named := store.statusWrites()
	if len(named) != 1 || named[0].Status != AppSessionStatusRunning {
		t.Fatalf("the holder named a status on %d write(s) %v, want exactly one naming running -- "+
			"every other write is a heartbeat, and a heartbeat that names running resurrects a row the "+
			"stale sweep has failed", len(named), statusesOf(named))
	}
	if store.heartbeatCount() < 4 {
		t.Errorf("only %d heartbeats over a session held through many flushes", store.heartbeatCount())
	}
}

// TestTheNoRecorderHolderPromotesOnTheFirstChunk: a node with no recorder
// still holds the session, and `running` is still a fact about the first
// chunk -- so it is said once, by the heartbeat, rather than never.
func TestTheNoRecorderHolderPromotesOnTheFirstChunk(t *testing.T) {
	runner, session, store, _ := newRunnerFixture(t, SubscriptionPresent)
	runner.flushEvery = 5 * time.Millisecond
	go func() {
		waitForSession(t, session, "sess-run")
		time.Sleep(20 * time.Millisecond)
		session.handleAppSessionChunk(&memqlv1.AppSessionChunk{SessionId: "sess-run", Stream: "stdout", Data: []byte("working"), Seq: 1})
		time.Sleep(30 * time.Millisecond)
		session.handleAppSessionEnd(&memqlv1.AppSessionEnd{SessionId: "sess-run"})
	}()

	if _, err := runner.Run(context.Background(), session.worker, runSpec(), nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	named := store.statusWrites()
	if len(named) != 1 || named[0].Status != AppSessionStatusRunning {
		t.Fatalf("with no recorder the holder named a status on %d write(s) %v, want exactly one naming running", len(named), statusesOf(named))
	}
	if named[0].HeartbeatAt.IsZero() {
		t.Error("the promotion carried no heartbeat; it is the holder's write like any other")
	}
}

// TestARefusedPromotionIsTriedAgain: the one write naming `running` is not
// fire-and-forget. If the engine refuses it, the next write names it again --
// otherwise one blip leaves a session that ran for an hour reading `starting`.
func TestARefusedPromotionIsTriedAgain(t *testing.T) {
	runner, session, store, _, _ := recordingFixture(t)
	runner.flushEvery = 5 * time.Millisecond
	store.refuseStatusWrites = 1
	go func() {
		waitForSession(t, session, "sess-run")
		session.handleAppSessionChunk(actionChunk(t, 1, map[string]any{"id": "a1", "seq": 1, "tool": "exec"}))
		time.Sleep(40 * time.Millisecond)
		session.handleAppSessionEnd(&memqlv1.AppSessionEnd{SessionId: "sess-run"})
	}()

	if _, err := runner.Run(context.Background(), session.worker, runSpec(), nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	named := store.statusWrites()
	if len(named) != 1 || named[0].Status != AppSessionStatusRunning {
		t.Fatalf("after one refused promotion, %d landed write(s) named a status %v, want exactly one naming running", len(named), statusesOf(named))
	}
}

// slowTranscripts is a ContentStore whose transcript write takes a while, and
// which counts the holder's beats on either side of it.
type slowTranscripts struct {
	fakeContents
	delay  time.Duration
	store  *recordingAppSessionStore
	rec    *fakeRecorder
	before [2]int
	after  [2]int
}

func (s *slowTranscripts) StoreContent(ctx context.Context, req ContentRequest) (ContentResult, error) {
	if !strings.HasPrefix(req.Name, "transcript-") {
		return s.fakeContents.StoreContent(ctx, req)
	}
	s.before = [2]int{s.store.heartbeatCount(), len(s.rec.runBeats())}
	time.Sleep(s.delay)
	s.after = [2]int{s.store.heartbeatCount(), len(s.rec.runBeats())}
	return s.fakeContents.StoreContent(ctx, req)
}

// closeWatch is a recorder that notes how many beats had landed when the
// recording's close began.
type closeWatch struct {
	*fakeRecorder
	store   *recordingAppSessionStore
	atClose [2]int
}

func (c *closeWatch) CloseRecording(ctx context.Context, r RecordingClose) error {
	c.atClose = [2]int{c.store.heartbeatCount(), len(c.fakeRecorder.runBeats())}
	return c.fakeRecorder.CloseRecording(ctx, r)
}

// TestTheHoldOutlastsTheDrain. The session's chunks stop before the session's
// rows are finished: the transcript still has to reach the Library and the
// recording still has to close, and that can take a minute on a slow engine.
// The holder is still holding through all of it, so it keeps beating -- or the
// stale sweep fails a session that is in the middle of ending cleanly, and the
// work sweep abandons its recording run.
//
// It STOPS before the recording closes, and that half matters as much: both
// writes are read-merges of the rows the close writes, and a beat landing
// after the close could read the run before it closed and write it back
// `running`.
func TestTheHoldOutlastsTheDrain(t *testing.T) {
	runner, session, store, rec, _ := recordingFixture(t)
	runner.flushEvery = 5 * time.Millisecond
	runner.runHeartbeatEvery = 10 * time.Millisecond
	contents := &slowTranscripts{delay: 80 * time.Millisecond, store: store, rec: rec}
	watch := &closeWatch{fakeRecorder: rec, store: store}
	runner.Contents, runner.Recorder = contents, watch
	go func() {
		waitForSession(t, session, "sess-run")
		session.handleAppSessionChunk(&memqlv1.AppSessionChunk{SessionId: "sess-run", Stream: "stdout", Data: []byte("the answer"), Seq: 1})
		session.handleAppSessionEnd(&memqlv1.AppSessionEnd{SessionId: "sess-run"})
	}()

	if _, err := runner.Run(context.Background(), session.worker, runSpec(), nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rows := contents.after[0] - contents.before[0]; rows < 2 {
		t.Errorf("the row was heartbeat %d time(s) while the transcript was being stored, want several -- "+
			"the drain had exited, and the stale sweep would take a session ending cleanly for a dead one", rows)
	}
	if runs := contents.after[1] - contents.before[1]; runs < 1 {
		t.Errorf("the recording run was not heartbeat while the transcript was being stored; the work sweep abandons it a minute in")
	}
	if final := [2]int{store.heartbeatCount(), len(rec.runBeats())}; final != watch.atClose {
		t.Errorf("beats landed after the recording began to close (at close %v, final %v) -- a beat is a read-merge "+
			"of the rows the close writes, and one landing after it can write the closed run back to running",
			watch.atClose, final)
	}
}

// TestADriverSuppliedRecordingRunIsLeftToItsDriversHeartbeat. A delegated
// step's child run (design D7) is opened and closed by the delegate's journal,
// which already heartbeats it for as long as the step runs; the session is
// only recorded INTO it. A second beat from here was a second updateWorkRun per
// window, each broadcast to every replica, saying the same thing.
func TestADriverSuppliedRecordingRunIsLeftToItsDriversHeartbeat(t *testing.T) {
	runner, session, store, rec, _ := recordingFixture(t)
	runner.flushEvery = 5 * time.Millisecond
	runner.runHeartbeatEvery = 10 * time.Millisecond
	endAfter(t, session, 80*time.Millisecond)

	spec := runSpec()
	spec.RecordingRunId = "v1:work:run:child-of-the-step"
	if _, err := runner.Run(context.Background(), session.worker, spec, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if beats := rec.runBeats(); len(beats) != 0 {
		t.Errorf("the session heartbeat a run its driver owns %d time(s); the driver's journal already does", len(beats))
	}
	if store.heartbeatCount() == 0 {
		t.Error("the session row was not heartbeat -- the ROW is the holder's however the run was opened")
	}
}
