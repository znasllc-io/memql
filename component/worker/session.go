package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// session.go implements the app-session envelope (memql#4359).
//
// Why a session and not a ToolDispatch: a dispatch carries ONE
// timeout and returns ONE result. A headless `claude -p` can run for
// an hour and emits output the whole way, so the run needs a start, a
// stream, a steering channel (cancel, credential renewal) and an end,
// all correlated by a session id -- and no ceiling but the delegation
// policy's.

// Session kinds. `run` is headless and autonomous; `open` hands the
// app to the HUMAN with the workspace and prompt loaded; `attach`
// streams a run the human started.
//
// ONLY `run` HAS AN ENGINE-SIDE INITIATOR TODAY. A planner Task is
// autonomous by definition, so cockpit-app opens `run` sessions; `open`
// and `attach` are carried by the protocol and accepted here, and
// nothing in this repository starts one. That is stated rather than left
// to be discovered, because this file sits next to a seam that spent two
// years being described as running when it was empty (memql#4120).
const (
	AppSessionKindRun    = "run"
	AppSessionKindOpen   = "open"
	AppSessionKindAttach = "attach"
)

// Session control actions.
const (
	AppSessionActionCancel          = "cancel"
	AppSessionActionRenewCredential = "renew_credential"
	// AppSessionActionMessage sends a FOLLOW-UP into a session that is
	// already open (epic memql#5096, design D7): the app keeps its
	// context, its workspace and its credential, and takes another turn.
	//
	// It carries its own `prompt` field rather than riding `reason`,
	// which is documented as transcript free-text on cancel. A session
	// whose machine reported FollowUps=false is refused BEFORE the wire,
	// so the caller learns the machine cannot do this instead of waiting
	// on a turn that will never arrive.
	AppSessionActionMessage = "message"
)

// Chunk streams.
const (
	AppSessionStreamStdout = "stdout"
	AppSessionStreamStderr = "stderr"
	AppSessionStreamEvent  = "event"
)

// ErrAppSessionUnsupported is returned when the selected worker's
// stream cannot carry app sessions.
var ErrAppSessionUnsupported = errors.New("worker: worker does not support app sessions")

// ErrAppSessionNotFound is returned when a control message names a
// session this node is not hosting.
var ErrAppSessionNotFound = errors.New("worker: app session not found")

// IsValidAppSessionKind reports whether kind is one of the three the
// protocol defines.
func IsValidAppSessionKind(kind string) bool {
	switch kind {
	case AppSessionKindRun, AppSessionKindOpen, AppSessionKindAttach:
		return true
	}
	return false
}

// AppSessionRequest is what a caller asks the worker to run.
type AppSessionRequest struct {
	SessionId     string
	App           string
	Kind          string
	Prompt        string
	Inputs        []string
	Workspace     string
	Credential    string
	MCPEndpoint   string
	RunId         string
	StepId        string
	AppSessionRef string
	Limits        AppSessionLimits
	// ResponseSchema is the JSON Schema the harness is asked to answer
	// against. EMPTY MEANS NONE WAS ASKED FOR, which is a different state
	// from asking and getting nothing back: only a session that asked can
	// report a missing structured answer as a disappointment.
	ResponseSchema string
	// Level is the call's LEVEL -- one of core/airoute's closed four -- which
	// says how much intelligence this run needs rather than which model serves
	// it (epic memql#5391, design D8). THE COCKPIT owns the translation into
	// the app's own knobs, from one table per app the machine's owner may
	// override, because the knob names are the app's and only the machine
	// knows which app, at which version, is installed.
	//
	// EMPTY MEANS NO LEVEL WAS NAMED, and the app runs at its own defaults --
	// which is what every session did before this field and what a person's
	// `open` still does. A level is never invented here: a session run at a
	// level nobody chose reports a model nobody asked for.
	Level string
}

// AppSessionLimits are the policy ceilings the session runs under.
type AppSessionLimits struct {
	CredentialLifetime time.Duration
	MaxDuration        time.Duration
	MaxTranscriptBytes int64
}

// AppSessionChunk is one piece of streamed output handed to the caller.
type AppSessionChunk struct {
	Stream string
	Data   []byte
	Seq    uint64
}

// AppSessionUsage is what the app REPORTED about its own spend.
// Known=false means the app said nothing, which the ledger records as
// billing "unknown" -- never as a free call.
type AppSessionUsage struct {
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
	Known        bool
}

// AppSessionOutcome is the terminal state of a session.
type AppSessionOutcome struct {
	ExitCode            int32
	Usage               AppSessionUsage
	AppSessionRef       string
	ProducedArtifactIds []string
	Error               string
	// Result is the harness's structured final answer as raw JSON, when it
	// produced one. It DOES NOT IMPLY SUCCESS and is deliberately separate
	// from Error and ExitCode: a harness can answer the schema and still
	// exit non-zero, and folding the two would make an answer we have
	// unreadable because the run that produced it also failed.
	Result []byte
	// Model and Effort are what the APP REPORTED serving this session with
	// (epic memql#5391, design D9): Claude Code's result event, Codex's thread
	// settings, replaced by any reroute the app announced.
	//
	// NOT what was asked for. A request is not a report, and a served model
	// copied from the request would record as measured something nobody
	// measured. EMPTY MEANS THE APP DID NOT SAY, which the engine records as
	// unknown -- and for effort that is the common case rather than the edge
	// one, since Claude Code's headless output states none.
	Model  string
	Effort string
}

// AppSessionHandle is the caller's view of a running session.
type AppSessionHandle struct {
	sessionId string
	worker    *Worker
	// app is the id this session is running, kept so a follow-up can ask
	// the machine's descriptor whether the harness takes one.
	app string

	chunks chan AppSessionChunk
	done   chan struct{}

	mu       sync.Mutex
	outcome  AppSessionOutcome
	endErr   error
	ended    bool
	lastSeq  uint64
	seqSeen  bool
	closeOne sync.Once

	// control sends a steering message to the worker.
	control func(*memqlv1.AppSessionControl) error
	// detach removes the session from its stream's session table.
	detach func()
}

// SessionId returns the server-minted session id.
func (h *AppSessionHandle) SessionId() string {
	if h == nil {
		return ""
	}
	return h.sessionId
}

// Chunks is the stream of output. Closed when the session ends.
func (h *AppSessionHandle) Chunks() <-chan AppSessionChunk {
	if h == nil {
		return nil
	}
	return h.chunks
}

// Cancel asks the worker to stop the session. Idempotent; a cancel
// on an already-ended session is a no-op rather than an error.
func (h *AppSessionHandle) Cancel(reason string) error {
	if h == nil {
		return ErrAppSessionNotFound
	}
	h.mu.Lock()
	ended := h.ended
	h.mu.Unlock()
	if ended {
		return nil
	}
	return h.control(&memqlv1.AppSessionControl{
		SessionId: h.sessionId,
		Action:    AppSessionActionCancel,
		Reason:    reason,
	})
}

// RenewCredential replaces the bearer the app uses against the MCP
// endpoint, for a run that outlives its first credential.
func (h *AppSessionHandle) RenewCredential(credential string) error {
	if h == nil {
		return ErrAppSessionNotFound
	}
	h.mu.Lock()
	ended := h.ended
	h.mu.Unlock()
	if ended {
		return ErrAppSessionNotFound
	}
	return h.control(&memqlv1.AppSessionControl{
		SessionId:  h.sessionId,
		Action:     AppSessionActionRenewCredential,
		Credential: credential,
	})
}

// ErrAppSessionNoFollowUps is the refusal for a follow-up on a machine whose
// descriptor says its harness cannot take one. Typed rather than a formatted
// string because the caller's next move differs: this is "start a new
// session", not "retry".
var ErrAppSessionNoFollowUps = errors.New("worker: this app's harness on this machine does not take follow-up turns")

// Message sends a follow-up prompt into a running session (design D7).
//
// The refusal is asked HERE rather than on the far side, because the far side
// answers by not answering: a cockpit whose harness cannot continue a session
// has nowhere to put the prompt and no turn to end, so the caller would wait
// out its own deadline for a capability the registration already told us was
// absent. A machine that reported NO descriptor at all is allowed through --
// absent is not the same as declared-false, and refusing on silence would
// take follow-ups away from every cockpit that predates the field.
func (h *AppSessionHandle) Message(prompt string) error {
	if h == nil {
		return ErrAppSessionNotFound
	}
	if strings.TrimSpace(prompt) == "" {
		return fmt.Errorf("worker: a follow-up needs a prompt")
	}
	h.mu.Lock()
	ended := h.ended
	app := h.app
	h.mu.Unlock()
	if ended {
		return ErrAppSessionNotFound
	}
	if desc, ok := h.worker.AppDescriptor(app); ok && !desc.FollowUps {
		return fmt.Errorf("%w (%s via %s)", ErrAppSessionNoFollowUps, app, desc.Harness)
	}
	return h.control(&memqlv1.AppSessionControl{
		SessionId: h.sessionId,
		Action:    AppSessionActionMessage,
		Prompt:    prompt,
	})
}

// Wait blocks until the session ends, the context is cancelled, or
// the worker disconnects. A ctx cancellation asks the worker to stop
// before returning, so a caller giving up never leaves a headless
// agent running on somebody's laptop.
func (h *AppSessionHandle) Wait(ctx context.Context) (AppSessionOutcome, error) {
	if h == nil {
		return AppSessionOutcome{}, ErrAppSessionNotFound
	}
	select {
	case <-h.done:
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.outcome, h.endErr
	case <-ctx.Done():
		_ = h.Cancel("caller_cancelled")
		return AppSessionOutcome{}, ctx.Err()
	}
}

// deliverChunk hands one chunk to the caller, enforcing monotonic
// seq. Out-of-order and duplicate chunks are DROPPED rather than
// appended: a transcript is a record, and silently interleaving a
// replayed chunk corrupts it in a way no later reader can detect.
func (h *AppSessionHandle) deliverChunk(c AppSessionChunk) {
	h.mu.Lock()
	if h.ended {
		h.mu.Unlock()
		return
	}
	if h.seqSeen && c.Seq <= h.lastSeq {
		h.mu.Unlock()
		return
	}
	h.lastSeq = c.Seq
	h.seqSeen = true
	h.mu.Unlock()

	select {
	case h.chunks <- c:
	case <-h.done:
	}
}

// finish records the terminal state and releases every waiter.
func (h *AppSessionHandle) finish(outcome AppSessionOutcome, err error) {
	h.closeOne.Do(func() {
		h.mu.Lock()
		h.ended = true
		h.outcome = outcome
		h.endErr = err
		h.mu.Unlock()
		close(h.done)
		close(h.chunks)
		if h.detach != nil {
			h.detach()
		}
	})
}

// StartAppSession opens a session on this worker. The returned handle
// streams chunks until the session ends.
//
// The session does NOT take a concurrency slot from the tool-dispatch
// pools: an app session is bounded by the delegation policy's
// max-concurrent-sessions, not by the machine's per-capability tool
// cap, and blocking a one-hour run behind a five-minute tool queue
// would deadlock the caller for reasons nothing in the request says.
func (w *Worker) StartAppSession(ctx context.Context, req AppSessionRequest) (*AppSessionHandle, error) {
	if w == nil || w.appSessionFn == nil {
		return nil, ErrAppSessionUnsupported
	}
	if !IsValidAppSessionKind(req.Kind) {
		return nil, fmt.Errorf("worker: unknown app session kind %q", req.Kind)
	}
	if !IsKnownAppId(req.App) {
		return nil, fmt.Errorf("worker: app %q is not one this engine drives", req.App)
	}
	if !w.RunsApp(req.App) {
		return nil, fmt.Errorf("worker: %s is not allowed and signed in on %s", req.App, w.RegistrationId)
	}
	return w.appSessionFn(ctx, req)
}

// AppSessionFunc is the per-stream hook that opens a session.
type AppSessionFunc func(ctx context.Context, req AppSessionRequest) (*AppSessionHandle, error)

// SetAppSessionFunc wires the per-stream app-session hook. Called
// once per stream alongside SetDispatchFunc.
func (w *Worker) SetAppSessionFunc(fn AppSessionFunc) {
	if w == nil {
		return
	}
	w.appSessionFn = fn
}

// toProtoLimits renders the policy ceilings for the wire.
func (l AppSessionLimits) toProto() *memqlv1.AppSessionLimits {
	return &memqlv1.AppSessionLimits{
		CredentialLifetimeSeconds: int64(l.CredentialLifetime / time.Second),
		MaxDurationSeconds:        int64(l.MaxDuration / time.Second),
		MaxTranscriptBytes:        l.MaxTranscriptBytes,
	}
}
