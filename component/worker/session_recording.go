package worker

// session_recording.go -- the driver that turns a live session's chunks into
// the recording (epic memql#5396, task memql#5398).
//
// It sits between the runner's drain loop and the two seams: SessionRecorder
// writes the rows, ContentStore stores the bytes. Keeping it out of runner.go
// is not tidiness -- the runner's job is the session's LIFECYCLE (mint, start,
// renew, end) and this one's is its CONTENT, and the two failure modes are
// different: a lifecycle fault must fail the run, and a recording fault must
// never.
//
// EVERY FAILURE HERE IS LOGGED AND SWALLOWED. A recording is a record of work,
// not the work. A session that ran on somebody's machine and spent their
// subscription must not be reported as failed because a step row did not land
// or the Library was unreachable -- the caller would retry work that already
// happened, on somebody's real computer.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const (
	// seqAllocationTimeout bounds one step-position claim against the row.
	seqAllocationTimeout = 10 * time.Second
	// closeRecordingTimeout bounds the recording's close. The close and the
	// allocation before it are the one stretch of a session the holder does
	// NOT heartbeat (the runner releases its hold first -- see
	// holdThroughEnd), so together they must stay well inside the stale
	// sweep's stall grace; TestTheUnheldCloseFitsInsideTheStallGrace pins it.
	closeRecordingTimeout = 30 * time.Second
)

// sessionRecording holds one session's recording state.
//
// It is the ONLY writer of actions for its session, which is what lets the
// seq counter live in memory here: the other writer into the same run is the
// MCP node, on another replica, and it allocates by reading the count this
// type publishes onto the session row.
type sessionRecording struct {
	recorder SessionRecorder
	contents ContentStore
	// store is the session row, which is the SEQ ALLOCATOR both writers
	// share. It is not an optimisation to skip: the MCP node counts from the
	// row and this side counting in memory collide systematically -- an app
	// that calls MemQL before its first recorded action, which is ordinary,
	// takes seq 0 and so does the action.
	store  AppSessionStore
	logger *slog.Logger

	sessionId string
	owner     string
	app       string
	runId     string
	// provenance stamps every content file with which intelligence produced
	// it (epic memql#5391, design D9). Model and effort arrive at END, so the
	// stamp a content carries mid-run names the app and the session only --
	// which is honest: they are what was known when the bytes were written.
	provenance ArtifactProvenance

	mu sync.Mutex
	// recorded and dropped are the recording's OWN ACCOUNTING -- how many
	// actions it wrote and how many it lost. They are not the seq: that comes
	// from the row.
	recorded int
	sequence ActionSequence
	// fallbackSeq is used only when the allocator cannot be reached. It keeps
	// a recording going during an engine blip rather than losing the actions,
	// and it is seeded from the last allocation so it does not restart at 0.
	fallbackSeq int
	// fingerprintPending holds the environment until the first action has a
	// step to carry it. The fingerprint is emitted as the session's first
	// event and belongs ON a step (design D16), so it waits for one rather
	// than getting a step of its own that did nothing.
	fingerprintPending map[string]any
	// runBeatEvery throttles the recording RUN's heartbeat, and lastRunBeat
	// is when it last went out. See Publish.
	runBeatEvery time.Duration
	lastRunBeat  time.Time
	// beatsRun is false when the run was SUPPLIED by its driver (a delegated
	// step's child run, design D7) rather than opened here. That driver's
	// journal already heartbeats the run for as long as the step runs, and a
	// second beat from here was a second updateWorkRun per window, broadcast
	// to every replica, saying the same thing.
	beatsRun bool
}

// newSessionRecording opens the recording, or returns nil when this node has
// no recorder wired.
//
// A NIL RECORDING IS A WORKING SESSION. Every method tolerates a nil receiver,
// so a node without the seam behaves exactly as it did before the recording
// existed -- which is what lets this be added to a live path without a branch
// at every call site.
func newSessionRecording(ctx context.Context, r *SessionRunner, spec RunSpec) *sessionRecording {
	if r == nil || r.Recorder == nil {
		return nil
	}
	rec := &sessionRecording{
		recorder:   r.Recorder,
		contents:   r.Contents,
		store:      r.Store,
		logger:     r.Logger,
		sessionId:  spec.SessionId,
		owner:      spec.OwnerUserId,
		app:        spec.App,
		provenance: ArtifactProvenance{App: spec.App, SessionId: spec.SessionId},

		runBeatEvery: r.runHeartbeatInterval(),
		beatsRun:     strings.TrimSpace(spec.RecordingRunId) == "",
	}
	if rec.logger == nil {
		rec.logger = slog.Default()
	}
	runId, err := r.Recorder.OpenRecording(ctx, RecordingOpen{
		SessionId:    spec.SessionId,
		OwnerUserId:  spec.OwnerUserId,
		App:          spec.App,
		Prompt:       spec.Prompt,
		Workspace:    spec.Workspace,
		RunId:        spec.RecordingRunId,
		ParentRunId:  spec.RunId,
		ParentStepId: spec.StepId,
		ModelCall:    spec.ModelCall,
	})
	if err != nil {
		rec.logger.Warn("app session: could not open the recording; the session runs unrecorded",
			"session_id", spec.SessionId, "error", err)
		return nil
	}
	rec.runId = runId
	return rec
}

// RunId is the run the actions are recorded into, or "" when nothing is
// recording.
func (s *sessionRecording) RunId() string {
	if s == nil {
		return ""
	}
	return s.runId
}

// Observe takes one `event` chunk.
//
// An event this engine cannot read is IGNORED and not counted as a lost
// action: "we lost an action" and "a newer cockpit said something we do not
// understand" are different claims, and only the first should make a lifted
// procedure look incomplete.
func (s *sessionRecording) Observe(ctx context.Context, chunk AppSessionChunk) {
	if s == nil {
		return
	}
	ev, ok := DecodeSessionEvent(chunk.Data)
	if !ok {
		s.logger.Debug("app session: an event chunk was not a recordable action",
			"session_id", s.sessionId, "chunk_seq", chunk.Seq)
		return
	}
	switch ev.Kind {
	case SessionEventFingerprint:
		s.mu.Lock()
		s.fingerprintPending = ev.Fingerprint
		s.mu.Unlock()
	case SessionEventAction:
		s.recordAction(ctx, ev.Action)
	}
}

func (s *sessionRecording) recordAction(ctx context.Context, action ActionEvent) {
	s.mu.Lock()
	take, gap := s.sequence.Admit(action.Seq)
	var fingerprint map[string]any
	if take {
		fingerprint = s.fingerprintPending
		s.fingerprintPending = nil
		s.recorded++
	}
	s.mu.Unlock()

	if gap > 0 {
		// The hole goes on the RUN, where a later lift is already reading.
		if err := s.recorder.RecordGap(ctx, RecordedGap{
			SessionId: s.sessionId, OwnerUserId: s.owner, RunId: s.runId,
			AfterSeq: action.Seq - uint64(gap) - 1, BeforeSeq: action.Seq, Missing: gap,
		}); err != nil {
			s.logger.Warn("app session: could not record an action gap",
				"session_id", s.sessionId, "missing", gap, "error", err)
		}
	}
	if !take {
		return
	}
	seq := s.allocateSeq(ctx)

	refs, omitted := s.storeContents(ctx, action)
	argsRef, args := s.spillArgs(ctx, action)
	action.Args = args

	now := time.Now().UTC()
	if err := s.recorder.RecordAction(ctx, RecordedAction{
		SessionId: s.sessionId, OwnerUserId: s.owner, RunId: s.runId,
		Seq: seq, Action: action,
		ContentRefs: refs, ContentOmitted: omitted, ArgsRef: argsRef,
		Fingerprint: fingerprint,
		StartedAt:   now, FinishedAt: now,
	}); err != nil {
		s.logger.Warn("app session: could not record an action",
			"session_id", s.sessionId, "tool", action.Tool, "error", err)
	}
}

// storeContents puts each of an action's file contents in the Library,
// content-addressed and owned by the machine's owner (design D5).
//
// A failure is NEVER fatal: the observation records `contentOmitted` and the
// action is still recorded. Losing the action because one file was too large
// or the Library was unreachable would lose both.
func (s *sessionRecording) storeContents(ctx context.Context, action ActionEvent) (refs, omitted []string) {
	if s.contents == nil {
		for _, c := range action.Contents {
			if len(c.Bytes) > 0 {
				omitted = append(omitted, c.Path+": this node stores no content")
			}
		}
		return nil, omitted
	}
	for _, c := range action.Contents {
		if len(c.Bytes) == 0 {
			continue
		}
		res, err := s.contents.StoreContent(ctx, ContentRequest{
			OwnerUserId: s.owner,
			Name:        contentFileName(c.Path, s.sessionId, action.Id),
			MimeType:    firstNonEmptyString(c.MimeType, "text/plain"),
			Bytes:       c.Bytes,
			Provenance:  s.provenance,
		})
		switch {
		case err != nil:
			omitted = append(omitted, c.Path+": "+err.Error())
		case res.Omitted != "":
			// The DIGEST survives even when the bytes did not (design D5): a
			// content above the cap is referenced by digest only, and a
			// reader comparing two runs still can.
			omitted = append(omitted, c.Path+": "+res.Omitted+" (sha256 "+res.Sha256+")")
		case res.FileId != "":
			refs = append(refs, res.FileId)
		}
	}
	return refs, omitted
}

// spillArgs puts an oversized argument set in the Library and returns the
// file id plus the arguments to record inline.
//
// The inline copy is kept either way: a reader should see the head of the call
// without leaving the row, and the reference is what keeps the action
// reproducible.
func (s *sessionRecording) spillArgs(ctx context.Context, action ActionEvent) (string, map[string]any) {
	if s.contents == nil || len(action.Args) == 0 {
		return "", action.Args
	}
	raw, err := json.Marshal(action.Args)
	if err != nil || len(raw) <= MaxObservationArgsBytes {
		return "", action.Args
	}
	res, err := s.contents.StoreContent(ctx, ContentRequest{
		OwnerUserId: s.owner,
		Name:        "args-" + shortLabel(action.Id) + ".json",
		MimeType:    "application/json",
		Bytes:       raw,
		Provenance:  s.provenance,
	})
	if err != nil || res.FileId == "" {
		// The spill failed. The observation still records argsTruncated, so
		// the reader knows the call is not reproducible from the row alone --
		// which is the honest answer, and better than failing the action.
		s.logger.Warn("app session: could not store oversized action arguments",
			"session_id", s.sessionId, "tool", action.Tool, "bytes", len(raw), "error", err)
		return "", action.Args
	}
	return res.FileId, action.Args
}

// allocateSeq takes the next step position from the SESSION ROW, which is the
// one piece of state this replica and the MCP node both see.
//
// A FAILURE FALLS BACK TO A LOCAL COUNTER rather than dropping the action. An
// engine blip during a session should cost the recording its ordering
// guarantee against the other writer, not the actions themselves -- and the
// fallback is seeded from the last allocation, so it continues rather than
// restarting at 0.
func (s *sessionRecording) allocateSeq(ctx context.Context) int {
	if s.store != nil {
		allocCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), seqAllocationTimeout)
		defer cancel()
		slot, err := s.store.ClaimRecordingSlot(allocCtx, s.sessionId, s.owner)
		if err != nil {
			s.logger.Warn("app session: could not allocate a step position; falling back to a local count",
				"session_id", s.sessionId, "error", err)
		} else {
			s.mu.Lock()
			// The fallback keeps up with the shared counter, so if the NEXT
			// allocation fails it continues from here rather than handing out
			// a position the row already gave away.
			if slot.Seq >= s.fallbackSeq {
				s.fallbackSeq = slot.Seq + 1
			}
			s.mu.Unlock()
			return slot.Seq
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seq := s.fallbackSeq
	s.fallbackSeq++
	return seq
}

// Publish writes how much of the session was LOST onto the row, and is the
// HOLDER'S HEARTBEAT.
//
// It no longer writes the step count: that is the allocator's, and a second
// writer of the same field would undo an allocation the MCP node had already
// taken. The drop count has one writer -- this one -- so it is published from
// here.
//
// THE HEARTBEAT RIDES THE SAME WRITE. The runner's hold calls this on every
// flush for as long as this replica holds the session, so it is the one writer
// that can say "somebody is still holding this": the session row's heartbeatAt,
// which workerAppSessionStaleSweep judges a live row by, and -- throttled --
// the recording run's, which the work sweep abandons a running run without.
// The run's beat is throttled because every run write is broadcast to every
// replica, and the sweep's window is a minute, not a flush; and it is skipped
// for a run its driver supplied, which that driver already beats.
//
// status is named only when non-empty, and the hold names it ONCE (see
// sessionHold). It returns the ROW write's error, logged here as well, so the
// hold can tell whether that one promotion landed.
func (s *sessionRecording) Publish(ctx context.Context, store AppSessionStore, status string) error {
	if s == nil || store == nil {
		return nil
	}
	now := time.Now().UTC()
	s.mu.Lock()
	dropped := s.sequence.Dropped()
	beatRun := s.beatsRun && (s.lastRunBeat.IsZero() || now.Sub(s.lastRunBeat) >= s.runBeatEvery)
	if beatRun {
		s.lastRunBeat = now
	}
	s.mu.Unlock()
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	// -1 for recordedSteps means "do not name it".
	rowErr := store.RecordAppSessionProgress(writeCtx, s.sessionId, -1, dropped, status, now)
	if rowErr != nil {
		s.logger.Warn("app session: could not publish recording progress",
			"session_id", s.sessionId, "error", rowErr)
	}
	if beatRun {
		if err := s.recorder.HeartbeatRecording(writeCtx, RecordingHeartbeat{
			SessionId: s.sessionId, OwnerUserId: s.owner, RunId: s.runId, At: now,
		}); err != nil {
			s.logger.Warn("app session: could not heartbeat the recording run; the work sweep may close it as abandoned",
				"session_id", s.sessionId, "run_id", s.runId, "error", err)
		}
	}
	return rowErr
}

// StoreTranscript puts the session's prose in the Library (design D5: the
// model's own text is one artifact per session, not rows, so the spine stays
// about actions).
func (s *sessionRecording) StoreTranscript(ctx context.Context, text string, truncated bool, model, effort string) string {
	if s == nil || s.contents == nil || strings.TrimSpace(text) == "" {
		return ""
	}
	provenance := s.provenance
	// By END the app has reported what it served with, so the transcript --
	// the artifact a recording pass reads first -- carries the full stamp.
	provenance.Model, provenance.Effort = model, effort
	// A DETACHED CONTEXT, for finishRow's reason: the caller's may already be
	// cancelled -- that is one of the ways a session ends -- and a cancelled
	// run's output is often exactly the output somebody wants to read.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	res, err := s.contents.StoreContent(writeCtx, ContentRequest{
		OwnerUserId: s.owner,
		Name:        "transcript-" + shortLabel(s.sessionId) + ".txt",
		MimeType:    "text/plain",
		Bytes:       []byte(text),
		Provenance:  provenance,
	})
	if err != nil || res.FileId == "" {
		s.logger.Warn("app session: could not store the transcript; the row will name no file",
			"session_id", s.sessionId, "truncated", truncated, "error", err)
		return ""
	}
	return res.FileId
}

// Close writes the app_answer step and closes the recording run.
func (s *sessionRecording) Close(ctx context.Context, result RunResult, transcriptFileId string) (recorded, dropped int) {
	if s == nil {
		return 0, 0
	}
	seq := s.allocateSeq(ctx)
	s.mu.Lock()
	recorded, dropped = s.recorded, s.sequence.Dropped()
	s.mu.Unlock()

	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeRecordingTimeout)
	defer cancel()
	if err := s.recorder.CloseRecording(writeCtx, RecordingClose{
		SessionId: s.sessionId, OwnerUserId: s.owner, RunId: s.runId,
		Seq: seq, Status: result.Status, Answer: result.Result,
		ExitCode: result.ExitCode, ErrorMessage: result.ErrorMessage,
		RecordedActions: recorded, DroppedActions: dropped,
		TranscriptFileId: transcriptFileId,
		// The app's REPORT, for StoreTranscript's reason: by END the app has
		// said what it served with, and the recording run is where a lifted
		// procedure's provenance is read back from.
		Model: result.Model, Effort: result.Effort,
		FinishedAt: time.Now().UTC(),
	}); err != nil {
		s.logger.Warn("app session: could not close the recording",
			"session_id", s.sessionId, "run_id", s.runId, "error", err)
	}
	return recorded, dropped
}

// contentFileName names a stored content in the Library.
//
// It keeps the file's own base name, because the person whose Library this is
// recognises `main.go` and recognises nothing about a digest. The session and
// action ids follow so two different `main.go`s from two sessions do not read
// as one file.
func contentFileName(path, sessionId, actionId string) string {
	base := path
	if i := strings.LastIndexAny(base, "/\\"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSpace(base)
	if base == "" {
		base = "content"
	}
	return fmt.Sprintf("%s.%s.%s", base, shortLabel(sessionId), shortLabel(actionId))
}

// ContentBaseName reads the file's own base name back out of a name
// contentFileName composed: `main.go.<session>.<action>` -> `main.go`. A lift
// needs it to say WHICH file a recorded content was (epic memql#5408): the
// observation references the Library row by id, and the row keeps only this
// name.
//
// The session and action ids make the inverse exact when they are the ones the
// name was composed from. They often are not, and that is not an edge case: a
// recorded content is content-ADDRESSED, so an action that wrote bytes an
// earlier session already filed references that session's row, under that
// session's suffix. So when the expected suffix is absent the last two
// dot-separated labels are stripped instead -- exact whenever the labels carry
// no dot, which every id this tree mints satisfies. ok is false for a name
// without the composed shape; a caller then has no base name rather than a
// wrong one.
func ContentBaseName(name, sessionId, actionId string) (string, bool) {
	name = strings.TrimSpace(name)
	if sessionId != "" && actionId != "" {
		suffix := "." + shortLabel(sessionId) + "." + shortLabel(actionId)
		if base := strings.TrimSuffix(name, suffix); base != name && base != "" {
			return base, true
		}
	}
	last := strings.LastIndexByte(name, '.')
	if last <= 0 || last == len(name)-1 {
		return "", false
	}
	prev := strings.LastIndexByte(name[:last], '.')
	if prev <= 0 || prev == last-1 {
		return "", false
	}
	return name[:prev], true
}

// shortLabel takes the distinctive tail of an id for a file name.
func shortLabel(id string) string {
	s := strings.TrimSpace(id)
	if i := strings.LastIndex(s, ":"); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 12 {
		s = s[:12]
	}
	if s == "" {
		return "unknown"
	}
	return s
}

func firstNonEmptyString(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
