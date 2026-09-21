package worker

// recording.go -- the vocabulary of an app session's RECORDING (epic
// memql#5396, design D2, D5 and D12).
//
// Until now a session left one flattened 256 KiB string with the stream and
// the sequence discarded. What it DID -- every command, every file it read or
// wrote, every call back into MemQL -- was inside that string, and the only
// way to get it back out was to parse somebody's prose.
//
// The recording makes each of those an ordinary row of the work spine: one
// v1:work:step and one v1:work:observation per action. That is what lets a
// later pass lift a recurring sequence into a construct and replay it without
// spending a model on it again, which is the whole premise of the program this
// epic is the second of.
//
// ============================================================================
// THE ENGINE NEVER PARSES A VENDOR FORMAT
// ============================================================================
// Claude Code emits stream-json `tool_use` / `tool_result` pairs; Codex emits
// completed items with a different shape and different names. The COCKPIT
// normalizes both into the one event below and sends it as an `event` chunk,
// so a third app is a cockpit change and not an engine release. Everything
// here reads that one shape.
//
// ============================================================================
// WHAT IS DELIBERATELY NOT HERE
// ============================================================================
// No engine, no database, no Library. This file is values and decisions over
// them; the WRITES live behind SessionRecorder and ContentStore, implemented
// in the main module and wired in app/. component/worker is its own Go module
// and cannot reach integrations/ or component/server, and pushing the writes
// behind an interface is what keeps the decisions testable without standing
// up either.

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// AppSessionStreamEvent-carried event kinds.
const (
	// SessionEventAction is one completed action the app took.
	SessionEventAction = "action"
	// SessionEventFingerprint is the environment, emitted once as the first
	// event of a session.
	SessionEventFingerprint = "fingerprint"
)

// Caps. VALUES, not constants of the design (design section 5, "values, not
// constants"): each is a number this file names once and every reader takes
// from here.
const (
	// MaxRecordedContentBytes bounds ONE file content the recording stores in
	// the Library. Above it the observation keeps the digest and records
	// `contentOmitted`, because a recording that refused the whole action
	// because one file was large would lose the action too.
	//
	// 8 MiB is chosen against what a coding agent actually reads and writes: a
	// source file, a diff, a log tail. A build artifact is not evidence about
	// a procedure, and storing one per action would fill somebody's Library
	// with bytes they never asked for.
	MaxRecordedContentBytes = 8 << 20

	// MaxObservationArgsBytes is the ceiling above which an action's
	// arguments spill to a Library file rather than staying inline. It
	// mirrors integrations/work's own ceiling, which is what the observation
	// writer enforces; this side decides whether to spill BEFORE the write,
	// because only this side holds a ContentStore.
	MaxObservationArgsBytes = 256 << 10

	// MaxTranscriptFileBytes bounds the session's prose -- the one Library
	// file stdout and stderr go to. Beyond it the tail is cut and
	// `transcriptTruncated` says so.
	MaxTranscriptFileBytes = 8 << 20
)

// ---------------------------------------------------------------------------
// The wire shape
// ---------------------------------------------------------------------------

// SessionEvent is one decoded `event` chunk.
type SessionEvent struct {
	// Kind is SessionEventAction or SessionEventFingerprint.
	Kind string `json:"kind"`
	// Action is populated when Kind is SessionEventAction.
	Action ActionEvent `json:"-"`
	// Fingerprint is populated when Kind is SessionEventFingerprint.
	Fingerprint map[string]any `json:"fingerprint"`
}

// ActionEvent is one completed action, normalized by the cockpit across apps
// (design D12).
type ActionEvent struct {
	// Id is the app's OWN id for the call -- Claude Code's `tool_use` id,
	// Codex's item id. It is what ties a recorded step back to the line in the
	// transcript that produced it, which is the only way to check a recording
	// against the session it came from.
	Id string `json:"id"`
	// Seq is monotonic per session. It is the action's own counter, NOT the
	// chunk seq: several chunks can carry one action's output, and one chunk
	// carries one action event.
	Seq uint64 `json:"seq"`
	// Tool is the normalized name. StepTypeForTool maps it to a step type.
	Tool string `json:"tool"`
	// Args are the call's arguments, WHOLE. D5: an action is only reproducible
	// from its arguments, and a shortened one reproduces a different call.
	Args map[string]any `json:"args"`
	// Cwd is the working directory the action ran in.
	Cwd string `json:"cwd"`
	// ExitCode is a POINTER because absent and zero are different answers: a
	// clean command exits 0 and a file write has no exit code at all.
	ExitCode *int `json:"exitCode"`
	// IsError is the app's own error flag for the call.
	IsError bool `json:"isError"`
	// ResultDigest and ResultType stand in for the result itself (D5), so the
	// spine stays about actions rather than filling with output nobody
	// replays.
	ResultDigest string `json:"resultDigest"`
	ResultType   string `json:"resultType"`
	// Contents are the file contents this action read or wrote, when the
	// harness had them inline. The runner stores each as a content-addressed
	// Library file and the observation references it by id.
	Contents []ActionContent `json:"contentInline"`
	// Error is the failure in the app's words, when it reported one.
	Error string `json:"error"`
}

// ActionContent is one file's bytes as the harness reported them.
type ActionContent struct {
	Path     string `json:"path"`
	MimeType string `json:"mimeType"`
	Bytes    []byte `json:"bytes"`
}

// DecodeSessionEvent reads one `event` chunk.
//
// It reports NOT-OK for anything it cannot act on -- malformed JSON, a kind
// this engine has no protocol for, an action with no tool. A caller must not
// count those as dropped ACTIONS: "we lost an action" and "a newer cockpit
// said something we do not understand" are different claims, and only the
// first should make a lifted procedure look incomplete.
func DecodeSessionEvent(data []byte) (SessionEvent, bool) {
	var probe struct {
		Kind        string         `json:"kind"`
		Tool        string         `json:"tool"`
		Fingerprint map[string]any `json:"fingerprint"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return SessionEvent{}, false
	}

	kind := strings.TrimSpace(probe.Kind)
	if kind == "" {
		// A cockpit that omits the discriminator and carries a tool plainly
		// means an action. The discriminator exists so a FUTURE event kind can
		// arrive without being mistaken for one, not to refuse the harnesses
		// that predate it.
		if strings.TrimSpace(probe.Tool) == "" {
			return SessionEvent{}, false
		}
		kind = SessionEventAction
	}

	switch kind {
	case SessionEventFingerprint:
		if len(probe.Fingerprint) == 0 {
			return SessionEvent{}, false
		}
		return SessionEvent{Kind: SessionEventFingerprint, Fingerprint: probe.Fingerprint}, true
	case SessionEventAction:
		var action ActionEvent
		if err := json.Unmarshal(data, &action); err != nil {
			return SessionEvent{}, false
		}
		if strings.TrimSpace(action.Tool) == "" {
			// An action with no tool names nothing that happened. Recorded, it
			// would be a step whose stepType was guessed.
			return SessionEvent{}, false
		}
		return SessionEvent{Kind: SessionEventAction, Action: action}, true
	}
	return SessionEvent{}, false
}

// StepTypeForTool maps a normalized tool name to a work-spine step type.
//
// The mapping is generous about spelling because the cockpit normalizes across
// apps whose own names differ, and a name nobody anticipated falls to `exec`.
// That default is a judgment call worth stating: an unrecognised action a
// coding agent took is far more likely to be a command than a file read, and
// `exec` is the reading that claims no more than the event said. The
// alternative -- a seventh "unknown" step type -- would put a value in the
// spine that DeriveKind has no answer for and that no later lift can replay.
func StepTypeForTool(tool string) string {
	name := strings.ToLower(strings.TrimSpace(tool))
	// An MCP call is recognised by its PREFIX: Claude Code spells a MemQL tool
	// `mcp__memql__runQuery` and Codex reports `mcp_tool_call`. Both are the
	// same act, and the engine's own MCP recording writes `mcp` for it -- so a
	// prefix match is what keeps the two accounts of one call agreeing.
	if strings.HasPrefix(name, "mcp") {
		return StepTypeMCP
	}
	switch name {
	case "fs_write", "write", "write_file", "edit", "str_replace", "apply_patch", "file_change":
		return StepTypeFSWrite
	case "fs_read", "read", "read_file", "cat", "view":
		return StepTypeFSRead
	case "fetch", "web_fetch", "http", "http_fetch", "webfetch":
		return StepTypeFetch
	case StepTypeAppAnswer, "answer", "agent_message":
		return StepTypeAppAnswer
	}
	return StepTypeExec
}

// The step types, re-exported so a caller holding only this module has the
// vocabulary. They are DEFINED in component/work, which is the leaf module
// DeriveKind lives in; component/worker cannot import it without dragging the
// engine's decision layer into the worker transport, and six string constants
// are a cheaper price than that edge. TestRecordedStepTypesMatchComponentWork
// in the main module pins the two copies together.
const (
	StepTypeExec      = "exec"
	StepTypeFSWrite   = "fs_write"
	StepTypeFSRead    = "fs_read"
	StepTypeFetch     = "fetch"
	StepTypeMCP       = "mcp"
	StepTypeAppAnswer = "app_answer"
)

// ---------------------------------------------------------------------------
// Action sequencing
// ---------------------------------------------------------------------------

// ActionSequence enforces monotonic action order and counts what was lost.
//
// The CHUNK layer already drops an out-of-order or duplicate chunk
// (AppSessionHandle.deliverChunk), so a drop there means an action event never
// arrives at all -- which reaches here as a HOLE in the action sequence rather
// than as a wrong order. The hole is the thing a later lift needs: a procedure
// lifted from an incomplete recording would otherwise be lifted as if it were
// complete, and nothing downstream could tell.
//
// The zero value is ready. Not safe for concurrent use: one session's events
// arrive on one goroutine.
type ActionSequence struct {
	last    uint64
	seen    bool
	dropped int
}

// Admit reports whether to record this action, and how many actions are
// missing before it.
//
// The FIRST action may carry any seq. A harness that numbers from 0 and one
// that numbers from 1 are both legitimate, and treating the opening event as a
// gap would report every session as having lost its first action.
func (s *ActionSequence) Admit(seq uint64) (take bool, gap int) {
	if !s.seen {
		s.last = seq
		s.seen = true
		return true, 0
	}
	if seq <= s.last {
		// REFUSED AND NOT COUNTED AS A LOSS, which is the subtle half. An
		// exact repeat is not a lost action -- the first copy was recorded --
		// and an action arriving after the cursor moved past it was already
		// counted as part of the hole it left. Counting either here would
		// report a recording as more incomplete than it is, and the number
		// exists so a later lift can decide whether to trust the sequence.
		return false, 0
	}
	gap = int(seq - s.last - 1)
	s.dropped += gap
	s.last = seq
	return true, gap
}

// Dropped is how many actions this session LOST: the actions that never
// arrived, counted once each. A refused repeat is not among them.
func (s *ActionSequence) Dropped() int {
	if s == nil {
		return 0
	}
	return s.dropped
}

// ---------------------------------------------------------------------------
// The seams
// ---------------------------------------------------------------------------

// RecordingOpen describes the recording to open.
type RecordingOpen struct {
	SessionId   string
	OwnerUserId string
	App         string
	Prompt      string
	Workspace   string
	// RunId is the recording run the CALLER already opened -- the subrun the
	// delegating step's childRunId points at (epic memql#5391). Empty asks the
	// recorder to open one, which is the delegated-task path, where no step
	// handed the session over and nothing opened a run for it.
	RunId string
	// ParentRunId and ParentStepId name the step that was handed over, so a
	// recorder opening its own run can stamp childRunId on it.
	ParentRunId  string
	ParentStepId string
}

// RecordedAction is one action to write: the event plus everything the runner
// resolved for it.
type RecordedAction struct {
	SessionId   string
	OwnerUserId string
	RunId       string
	// Seq is the step's position in the recording run. Allocated by the
	// CALLER, because the two writers into one run know different things: the
	// replica holding the session is the only writer of ACTIONS and can count
	// them in memory, while the MCP node must read the count off the session
	// row.
	Seq    int
	Action ActionEvent
	// ContentRefs are the Library file ids the action's contents were stored
	// at. ContentOmitted names the ones that were not stored, and why.
	ContentRefs    []string
	ContentOmitted []string
	// ArgsRef is the Library file the arguments spilled to when they exceeded
	// the observation's ceiling. Empty is the ordinary case: arguments are
	// kept WHOLE inline, and the spill is a backstop so that even the
	// pathological call stays reproducible.
	ArgsRef string
	// Fingerprint is written on the session's FIRST recorded step only.
	Fingerprint map[string]any
	StartedAt   time.Time
	FinishedAt  time.Time
}

// RecordedGap is a hole in the action sequence.
type RecordedGap struct {
	SessionId   string
	OwnerUserId string
	RunId       string
	// AfterSeq and BeforeSeq bracket what is missing.
	AfterSeq  uint64
	BeforeSeq uint64
	Missing   int
}

// RecordingClose ends the recording with the session's answer.
type RecordingClose struct {
	SessionId   string
	OwnerUserId string
	RunId       string
	// Seq is the app_answer step's position, allocated like an action's.
	Seq int
	// Status is the session's terminal status: ended, failed or cancelled.
	Status string
	// Answer is the harness's structured final answer as raw JSON, when it
	// produced one.
	Answer       []byte
	ExitCode     int
	ErrorMessage string
	// RecordedActions and DroppedActions are the recording's own accounting.
	RecordedActions int
	DroppedActions  int
	// TranscriptFileId is the Library file the session's prose went to.
	TranscriptFileId string
	FinishedAt       time.Time
}

// SessionRecorder writes an app session's actions into the work spine.
//
// Declared HERE and implemented in integrations/work, so the dependency points
// worker -> work rather than the other way: component/worker is a transport
// module and the writes are @serverOnly mutations whose internal-origin stamp
// is allowlisted per PACKAGE. Same shape as ToolInvocationRecorder, for the
// same reason.
//
// EVERY METHOD IS BEST-EFFORT FROM THE CALLER'S SIDE. A recording is a record
// of work, not the work: a session that ran on somebody's machine and spent
// their subscription must not be reported as failed because a step row did not
// land. Errors are returned so the caller can log them, never so it can refuse
// the run.
type SessionRecorder interface {
	// OpenRecording returns the run id the actions are recorded into, which is
	// r.RunId when the caller supplied one.
	OpenRecording(ctx context.Context, r RecordingOpen) (string, error)
	RecordAction(ctx context.Context, r RecordedAction) error
	RecordGap(ctx context.Context, r RecordedGap) error
	CloseRecording(ctx context.Context, r RecordingClose) error
}

// ContentRequest is one file content to store.
type ContentRequest struct {
	OwnerUserId string
	// Name is what the file is called in the Library.
	Name     string
	MimeType string
	Bytes    []byte
	// Provenance stamps which intelligence produced it (epic memql#5391, D9).
	Provenance ArtifactProvenance
}

// ContentResult is where the bytes ended up.
type ContentResult struct {
	// FileId is the v1:library:file the content is at, or empty when it was
	// not stored.
	FileId string
	// Sha256 is the content digest, computed WHETHER OR NOT the bytes were
	// stored: a content above the cap is referenced by digest only (design
	// D5), so the digest is the part that must never be missing.
	Sha256 string
	// Deduplicated says an identical content was already there. Two identical
	// writes are one file referenced twice, which is what makes a branch from
	// a recorded step cost no copy.
	Deduplicated bool
	// Omitted says why the bytes were not stored, in words a reader can act
	// on. Empty when they were.
	Omitted string
}

// ContentStore stores an action's file contents as content-addressed Library
// files owned by the machine's owner (design D5).
//
// A FAILURE IS NEVER FATAL TO THE SESSION. The implementation reports the
// reason in Omitted and the observation records `contentOmitted`; refusing the
// action because the Library was unreachable would lose the action as well as
// the content.
type ContentStore interface {
	StoreContent(ctx context.Context, req ContentRequest) (ContentResult, error)
}
