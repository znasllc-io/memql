package worker

import (
	"context"
	"errors"
	"fmt"
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
	PlanId        string
	TaskId        string
	AppSessionRef string
	Limits        AppSessionLimits
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
}

// AppSessionHandle is the caller's view of a running session.
type AppSessionHandle struct {
	sessionId string
	worker    *Worker

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
