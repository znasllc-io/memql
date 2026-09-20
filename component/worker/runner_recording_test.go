package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// runner_recording_test.go -- the runner's half of the recording (epic
// memql#5396, task memql#5398).
//
// TO CONFIRM THESE ARE LOAD-BEARING: send the event chunks to the transcript
// collector instead of the recorder, or drop the owner check, and they fail.
// If they pass either way they measure nothing.

// fakeRecorder records what the runner asked it to write.
type fakeRecorder struct {
	mu       sync.Mutex
	openedAs RecordingOpen
	actions  []RecordedAction
	gaps     []RecordedGap
	closed   []RecordingClose
	runId    string
	openErr  error
}

func (f *fakeRecorder) OpenRecording(_ context.Context, r RecordingOpen) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.openedAs = r
	if f.openErr != nil {
		return "", f.openErr
	}
	if r.RunId != "" {
		f.runId = r.RunId
	} else if f.runId == "" {
		f.runId = "v1:work:run:opened-here"
	}
	return f.runId, nil
}

func (f *fakeRecorder) RecordAction(_ context.Context, r RecordedAction) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions = append(f.actions, r)
	return nil
}

func (f *fakeRecorder) RecordGap(_ context.Context, r RecordedGap) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gaps = append(f.gaps, r)
	return nil
}

func (f *fakeRecorder) CloseRecording(_ context.Context, r RecordingClose) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, r)
	return nil
}

func (f *fakeRecorder) recorded() ([]RecordedAction, []RecordedGap, []RecordingClose) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := make([]RecordedAction, len(f.actions))
	copy(a, f.actions)
	g := make([]RecordedGap, len(f.gaps))
	copy(g, f.gaps)
	c := make([]RecordingClose, len(f.closed))
	copy(c, f.closed)
	return a, g, c
}

// fakeContents is a content-addressed store in a map, so the dedup property
// is observable without a Library.
type fakeContents struct {
	mu     sync.Mutex
	stored map[string]ContentRequest
	byName []ContentRequest
}

func (f *fakeContents) StoreContent(_ context.Context, req ContentRequest) (ContentResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stored == nil {
		f.stored = map[string]ContentRequest{}
	}
	f.byName = append(f.byName, req)
	digest := fmt.Sprintf("sha-%d-%x", len(req.Bytes), req.Bytes[:min(8, len(req.Bytes))])
	if _, ok := f.stored[digest]; ok {
		return ContentResult{FileId: "v1:library:file:" + digest, Sha256: digest, Deduplicated: true}, nil
	}
	f.stored[digest] = req
	return ContentResult{FileId: "v1:library:file:" + digest, Sha256: digest}, nil
}

func (f *fakeContents) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.stored)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func recordingFixture(t *testing.T) (*SessionRunner, *streamSession, *recordingAppSessionStore, *fakeRecorder, *fakeContents) {
	t.Helper()
	runner, session, store, _ := newRunnerFixture(t, SubscriptionPresent)
	rec := &fakeRecorder{}
	contents := &fakeContents{}
	runner.Recorder = rec
	runner.Contents = contents
	return runner, session, store, rec, contents
}

func actionChunk(t *testing.T, chunkSeq uint64, a map[string]any) *memqlv1.AppSessionChunk {
	t.Helper()
	a["kind"] = "action"
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	return &memqlv1.AppSessionChunk{
		SessionId: "sess-run", Stream: AppSessionStreamEvent, Data: raw, Seq: chunkSeq,
	}
}

// TestASessionOfNActionsRecordsNActionsAndOneTranscript -- issue #5398's
// headline acceptance. Before this, every chunk was flattened into one string
// and a session was one step.
func TestASessionOfNActionsRecordsNActionsAndOneTranscript(t *testing.T) {
	runner, session, _, rec, contents := recordingFixture(t)

	go func() {
		waitForSession(t, session, "sess-run")
		for i := 1; i <= 4; i++ {
			session.handleAppSessionChunk(actionChunk(t, uint64(i*2), map[string]any{
				"id": fmt.Sprintf("toolu_%d", i), "seq": i, "tool": "exec",
				"args": map[string]any{"command": "step " + fmt.Sprint(i)},
			}))
			session.handleAppSessionChunk(&memqlv1.AppSessionChunk{
				SessionId: "sess-run", Stream: "stdout",
				Data: []byte("thinking about step " + fmt.Sprint(i) + "\n"), Seq: uint64(i*2 + 1),
			})
		}
		session.handleAppSessionEnd(&memqlv1.AppSessionEnd{SessionId: "sess-run"})
	}()

	result, err := runner.Run(context.Background(), session.worker, runSpec(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	actions, gaps, closes := rec.recorded()
	if len(actions) != 4 {
		t.Fatalf("recorded %d actions, want 4 -- an event chunk must become a row, not transcript text", len(actions))
	}
	for i, a := range actions {
		if a.Seq != i {
			t.Errorf("action %d recorded at seq %d, want %d -- the client orders by seq", i, a.Seq, i)
		}
		if a.RunId == "" {
			t.Errorf("action %d has no recording run", i)
		}
	}
	if len(gaps) != 0 {
		t.Errorf("a contiguous sequence reported %d gaps", len(gaps))
	}
	if len(closes) != 1 || closes[0].RecordedActions != 4 {
		t.Fatalf("close = %+v, want one close reporting 4 recorded actions", closes)
	}

	// THE PROSE IS THE TRANSCRIPT AND NOTHING ELSE. An event chunk reaching
	// the transcript would be the old flattening, with the action recorded
	// twice in two shapes.
	if strings.Contains(result.Transcript, "toolu_") {
		t.Fatalf("an event chunk leaked into the transcript: %q", result.Transcript)
	}
	if !strings.Contains(result.Transcript, "thinking about step 1") {
		t.Fatalf("stdout did not reach the transcript: %q", result.Transcript)
	}
	if result.TranscriptFileId == "" {
		t.Fatal("the session produced no transcript file")
	}
	if closes[0].TranscriptFileId != result.TranscriptFileId {
		t.Errorf("the recording closed naming %q and the result says %q",
			closes[0].TranscriptFileId, result.TranscriptFileId)
	}
	// One transcript file, plus nothing else: these actions carried no
	// contents.
	if contents.count() != 1 {
		t.Errorf("stored %d contents, want exactly the one transcript", contents.count())
	}
}

// TestAnOutOfOrderActionIsDroppedAndTheGapRecorded -- issue #5398's second
// acceptance. The loss has to be visible: a procedure lifted from an
// incomplete recording would otherwise be lifted as if it were complete.
func TestAnOutOfOrderActionIsDroppedAndTheGapRecorded(t *testing.T) {
	runner, session, store, rec, _ := recordingFixture(t)

	go func() {
		waitForSession(t, session, "sess-run")
		// Action seqs 1, then 4: two never arrived. The chunk seqs stay
		// monotonic, because the chunk layer would otherwise drop them before
		// the recorder ever saw them -- which is exactly how a hole appears.
		session.handleAppSessionChunk(actionChunk(t, 1, map[string]any{"id": "a1", "seq": 1, "tool": "exec"}))
		session.handleAppSessionChunk(actionChunk(t, 2, map[string]any{"id": "a4", "seq": 4, "tool": "exec"}))
		// And a repeat, which is refused and is NOT a loss: the first copy
		// was recorded.
		session.handleAppSessionChunk(actionChunk(t, 3, map[string]any{"id": "a4", "seq": 4, "tool": "exec"}))
		session.handleAppSessionEnd(&memqlv1.AppSessionEnd{SessionId: "sess-run"})
	}()

	if _, err := runner.Run(context.Background(), session.worker, runSpec(), nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	actions, gaps, closes := rec.recorded()
	if len(actions) != 2 {
		t.Fatalf("recorded %d actions, want 2 -- the repeat must be dropped", len(actions))
	}
	if len(gaps) != 1 {
		t.Fatalf("recorded %d gaps, want 1", len(gaps))
	}
	if gaps[0].Missing != 2 || gaps[0].AfterSeq != 1 || gaps[0].BeforeSeq != 4 {
		t.Errorf("gap = %+v, want 2 missing between 1 and 4", gaps[0])
	}
	if closes[0].DroppedActions != 2 {
		t.Errorf("close reported %d dropped, want 2 -- a refused repeat is not a loss",
			closes[0].DroppedActions)
	}

	// The count reaches the ROW, which is what a Fleet reader sees and what
	// the MCP node allocates its own step's seq from.
	var sawProgress bool
	for _, p := range store.appends {
		if p.RecordedSteps > 0 {
			sawProgress = true
		}
	}
	if !sawProgress {
		t.Error("the recording's progress never reached the session row")
	}
}

// TestASessionWithAnUnresolvableOwnerIsRefusedBeforeItStarts -- issue #5398's
// third acceptance, and the workspace_owner_unresolved rule (memql#4354).
//
// BEFORE it starts is the whole assertion. Every row the session would write
// is owner-tiered; under a blank owner they are written and readable by
// nobody, including the operator asking what an agent did on their machine.
func TestASessionWithAnUnresolvableOwnerIsRefusedBeforeItStarts(t *testing.T) {
	runner, session, store, rec, _ := recordingFixture(t)
	spec := runSpec()
	spec.OwnerUserId = "   "

	_, err := runner.Run(context.Background(), session.worker, spec, nil)
	if err == nil {
		t.Fatal("a session with no owner ran")
	}
	if !strings.Contains(err.Error(), "workspace_owner_unresolved") {
		t.Errorf("the refusal must name the rule so a reader can find it: %v", err)
	}
	if len(store.created) != 0 {
		t.Error("a refused session left a row")
	}
	if actions, _, _ := rec.recorded(); len(actions) != 0 {
		t.Error("a refused session recorded actions")
	}
	if rec.openedAs.SessionId != "" {
		t.Error("a refused session opened a recording run")
	}
}

// TestTwoIdenticalWritesYieldOneFile -- issue #5400's acceptance, driven
// through the runner because that is where an action's contents are resolved.
func TestTwoIdenticalWritesYieldOneFile(t *testing.T) {
	runner, session, _, rec, contents := recordingFixture(t)
	body := []byte("package main\n\nfunc main() {}\n")

	go func() {
		waitForSession(t, session, "sess-run")
		for i := 1; i <= 2; i++ {
			session.handleAppSessionChunk(actionChunk(t, uint64(i), map[string]any{
				"id": fmt.Sprintf("w%d", i), "seq": i, "tool": "fs_write",
				"contentInline": []map[string]any{
					{"path": "/w/main.go", "mimeType": "text/plain", "bytes": body},
				},
			}))
		}
		session.handleAppSessionEnd(&memqlv1.AppSessionEnd{SessionId: "sess-run"})
	}()

	if _, err := runner.Run(context.Background(), session.worker, runSpec(), nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	actions, _, _ := rec.recorded()
	if len(actions) != 2 {
		t.Fatalf("recorded %d actions, want 2", len(actions))
	}
	if len(actions[0].ContentRefs) != 1 || len(actions[1].ContentRefs) != 1 {
		t.Fatalf("each write must reference its content: %v / %v",
			actions[0].ContentRefs, actions[1].ContentRefs)
	}
	if actions[0].ContentRefs[0] != actions[1].ContentRefs[0] {
		t.Errorf("two identical writes referenced %q and %q, want ONE file referenced twice -- "+
			"a branch from a recorded step must cost no copy",
			actions[0].ContentRefs[0], actions[1].ContentRefs[0])
	}
	// ONE distinct content for two identical writes. A second would mean the
	// contents were not addressed by their bytes. There is no transcript
	// among them because this session emitted no prose, and an empty
	// transcript file would be a Library row saying a run produced output it
	// did not.
	if contents.count() != 1 {
		t.Errorf("stored %d distinct contents, want 1 -- the same bytes twice is one file", contents.count())
	}
}

// TestAContentAboveTheCapRecordsOmittedAndNeverFailsTheSession -- issue
// #5400's second acceptance.
func TestAContentAboveTheCapRecordsOmittedAndNeverFailsTheSession(t *testing.T) {
	runner, session, _, rec, _ := recordingFixture(t)
	runner.Contents = &cappedContents{}

	go func() {
		waitForSession(t, session, "sess-run")
		session.handleAppSessionChunk(actionChunk(t, 1, map[string]any{
			"id": "w1", "seq": 1, "tool": "fs_write",
			"contentInline": []map[string]any{
				{"path": "/w/huge.bin", "bytes": []byte("enormous")},
			},
		}))
		session.handleAppSessionEnd(&memqlv1.AppSessionEnd{SessionId: "sess-run"})
	}()

	result, err := runner.Run(context.Background(), session.worker, runSpec(), nil)
	if err != nil {
		t.Fatalf("Run: %v -- a content above the cap must never fail the session", err)
	}
	if result.Status != AppSessionStatusEnded {
		t.Fatalf("status = %q, want ended", result.Status)
	}
	actions, _, _ := rec.recorded()
	if len(actions) != 1 {
		t.Fatalf("recorded %d actions, want the action itself", len(actions))
	}
	if len(actions[0].ContentRefs) != 0 {
		t.Errorf("an omitted content left a reference: %v", actions[0].ContentRefs)
	}
	if len(actions[0].ContentOmitted) != 1 {
		t.Fatalf("contentOmitted = %v, want the omission recorded", actions[0].ContentOmitted)
	}
	// THE DIGEST SURVIVES EVEN THOUGH THE BYTES DID NOT (design D5): a
	// content above the cap is referenced by digest only, so two runs can
	// still be compared.
	if !strings.Contains(actions[0].ContentOmitted[0], "sha256") {
		t.Errorf("the omission does not carry the digest: %q", actions[0].ContentOmitted[0])
	}
}

// cappedContents refuses every content, the way the real store refuses one
// above MaxRecordedContentBytes.
type cappedContents struct{}

func (cappedContents) StoreContent(_ context.Context, req ContentRequest) (ContentResult, error) {
	if req.MimeType == "text/plain" && strings.HasPrefix(req.Name, "transcript-") {
		return ContentResult{FileId: "v1:library:file:transcript", Sha256: "sha-t"}, nil
	}
	return ContentResult{Sha256: "sha-big", Omitted: "above the per-file cap"}, nil
}

// TestTheRecordingUsesTheRunTheCallerOpened. Epic memql#5391's delegate
// stamps childRunId on the delegating step; the actions must land in THAT
// run, or the pointer aims at a run holding one step and no actions.
func TestTheRecordingUsesTheRunTheCallerOpened(t *testing.T) {
	runner, session, store, rec, _ := recordingFixture(t)
	spec := runSpec()
	spec.RecordingRunId = "v1:work:run:child-from-the-delegate"

	go func() {
		waitForSession(t, session, "sess-run")
		session.handleAppSessionChunk(actionChunk(t, 1, map[string]any{"id": "a1", "seq": 1, "tool": "exec"}))
		session.handleAppSessionEnd(&memqlv1.AppSessionEnd{SessionId: "sess-run"})
	}()

	if _, err := runner.Run(context.Background(), session.worker, spec, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	actions, _, _ := rec.recorded()
	if len(actions) != 1 || actions[0].RunId != spec.RecordingRunId {
		t.Fatalf("the action landed in %q, want the caller's run %q",
			actions[0].RunId, spec.RecordingRunId)
	}
	// THE ROW NAMES IT, which is how the MCP node -- another replica -- finds
	// the run to write its own step into.
	if len(store.created) != 1 || store.created[0].SessionRunId != spec.RecordingRunId {
		t.Fatalf("the session row names %q as its recording run, want %q",
			store.created[0].SessionRunId, spec.RecordingRunId)
	}
}

// TestASessionRunsWhenNothingRecords. A node with no recorder wired must
// behave exactly as it did before the recording existed -- that is what lets
// this be added to a live path without a branch at every call site.
func TestASessionRunsWhenNothingRecords(t *testing.T) {
	runner, session, store, _ := newRunnerFixture(t, SubscriptionPresent)

	go func() {
		waitForSession(t, session, "sess-run")
		session.handleAppSessionChunk(actionChunk(t, 1, map[string]any{"id": "a1", "seq": 1, "tool": "exec"}))
		session.handleAppSessionChunk(&memqlv1.AppSessionChunk{
			SessionId: "sess-run", Stream: "stdout", Data: []byte("hello"), Seq: 2,
		})
		session.handleAppSessionEnd(&memqlv1.AppSessionEnd{SessionId: "sess-run"})
	}()

	result, err := runner.Run(context.Background(), session.worker, runSpec(), nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != AppSessionStatusEnded {
		t.Fatalf("status = %q, want ended", result.Status)
	}
	if len(store.terminal()) != 1 {
		t.Fatal("the session still writes its terminal row")
	}
	if result.TranscriptFileId != "" {
		t.Error("a node that stores no content must not claim a transcript file")
	}
}
