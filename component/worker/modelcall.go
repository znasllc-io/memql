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

// modelcall.go implements the ModelCall envelope (epic memql#4676,
// task memql#4677) -- the transport half of running the platform's own
// operations on a model a fleet machine hosts.
//
// It is deliberately the AppSession shape (session.go) rather than the
// ToolDispatch shape. A dispatch carries one timeout and returns one
// result; a generation emits tokens for as long as it runs, and the
// caller wants them as they arrive. Start / delta / end / cancel,
// correlated by request id.
//
// WHAT IS NOT HERE. Nothing in this file decides WHICH machine runs a
// call -- that is the router (memql#4678) -- and nothing here executes
// anything: the runtime side (Ollama, an OpenAI-compatible endpoint)
// lives in the memql-cockpit repo. This repo fixes the contract.

// Model call kinds.
const (
	// ModelCallKindChat is messages in, text out.
	ModelCallKindChat = "chat"
	// ModelCallKindEmbedding is strings in, vectors out.
	ModelCallKindEmbedding = "embedding"
)

// Finish reasons carried on ModelCallEnd.
const (
	ModelFinishStop      = "stop"
	ModelFinishLength    = "length"
	ModelFinishCancelled = "cancelled"
	ModelFinishTimeout   = "timeout"
	ModelFinishError     = "error"
)

// Default envelope deadlines.
//
// These are the numbers a caller who states nothing gets, and the two
// are separate because they answer different questions. A local 8B
// model on a cold GPU can be twenty seconds from start to first token,
// which is indistinguishable from a wedged machine to anything holding
// only a wall clock; the idle ceiling is what tells them apart, and the
// keepalive is what makes the idle ceiling enforceable rather than a
// guess.
const (
	// ModelCallTimeoutDefault is the whole-call ceiling.
	ModelCallTimeoutDefault = 10 * time.Minute
	// ModelCallIdleTimeoutDefault bounds the gap BETWEEN deltas.
	ModelCallIdleTimeoutDefault = 90 * time.Second
	// ModelCallKeepaliveDefault is how often a worker with nothing to
	// say must prove it is alive. Comfortably under the idle ceiling so
	// one lost keepalive is not a failed call.
	ModelCallKeepaliveDefault = 20 * time.Second
)

// modelDeltaBuffer is how many deltas the handle buffers before a slow
// consumer backpressures the stream-recv goroutine.
const modelDeltaBuffer = 256

// ErrModelCallUnsupported is returned when the selected worker's stream
// cannot carry model calls.
var ErrModelCallUnsupported = errors.New("worker: worker does not support model calls")

// ErrModelCallNotFound is returned when a message names a call this
// node is not hosting.
var ErrModelCallNotFound = errors.New("worker: model call not found")

// ErrModelCallIdle is the idle-ceiling expiry. It is distinct from a
// context deadline because the two mean different things to a caller
// deciding whether to try another machine: an idle expiry says THIS
// machine stopped answering, where a deadline says the caller ran out
// of patience with a machine that may still be working.
var ErrModelCallIdle = errors.New("worker: model call went silent past its idle ceiling")

// IsValidModelCallKind reports whether kind is one the protocol defines.
func IsValidModelCallKind(kind string) bool {
	switch kind {
	case ModelCallKindChat, ModelCallKindEmbedding:
		return true
	}
	return false
}

// ModelCallMessage is one turn handed to the model.
type ModelCallMessage struct {
	Role    string
	Content string
}

// ModelCallParams are the generation knobs.
//
// Each optional knob carries an explicit *Set companion for the reason
// the proto states: zero is a MEANINGFUL value for temperature and
// top_p, so reading absent as zero would silently pin every
// unspecified call to greedy decoding.
type ModelCallParams struct {
	Temperature     float64
	TemperatureSet  bool
	TopP            float64
	TopPSet         bool
	MaxOutputTokens int64
	Stop            []string
	Seed            int64
	SeedSet         bool
}

// ModelCallLimits are the envelope-owned deadlines.
type ModelCallLimits struct {
	Timeout     time.Duration
	IdleTimeout time.Duration
	Keepalive   time.Duration
}

// withDefaults fills the blanks. Applied on the SERVER side, once, so
// the worker is told the numbers rather than left to invent them --
// which is what "timeout and keepalive are defined on the envelope, not
// left to callers" means in practice.
func (l ModelCallLimits) withDefaults() ModelCallLimits {
	if l.Timeout <= 0 {
		l.Timeout = ModelCallTimeoutDefault
	}
	if l.IdleTimeout <= 0 {
		l.IdleTimeout = ModelCallIdleTimeoutDefault
	}
	if l.Keepalive <= 0 {
		l.Keepalive = ModelCallKeepaliveDefault
	}
	// A keepalive at or past the idle ceiling guarantees a false idle
	// expiry on every quiet call, so it is clamped rather than honoured.
	if l.Keepalive >= l.IdleTimeout {
		l.Keepalive = l.IdleTimeout / 2
		if l.Keepalive <= 0 {
			l.Keepalive = ModelCallKeepaliveDefault
		}
	}
	return l
}

func (l ModelCallLimits) toProto() *memqlv1.ModelCallLimits {
	return &memqlv1.ModelCallLimits{
		TimeoutSeconds:     int64(l.Timeout / time.Second),
		IdleTimeoutSeconds: int64(l.IdleTimeout / time.Second),
		KeepaliveSeconds:   int64(l.Keepalive / time.Second),
	}
}

// ModelCallRequest is what a caller asks a machine to run.
type ModelCallRequest struct {
	RequestId string
	Model     string
	Kind      string
	Messages  []ModelCallMessage
	Params    ModelCallParams
	// ResponseFormatSchema is a JSON Schema for structured output.
	// Empty means free text.
	ResponseFormatSchema []byte
	EmbeddingInput       []string
	Limits               ModelCallLimits
	PlanId               string
	TaskId               string
	Purpose              string
}

// ModelCallDelta is one piece of streamed output handed to the caller.
type ModelCallDelta struct {
	Seq       uint64
	Content   string
	Keepalive bool
}

// ModelCallUsage is what the RUNTIME REPORTED. Known=false means it
// reported nothing, which the ledger records as billing "unknown"
// rather than as a zero-token call (memql#4681).
type ModelCallUsage struct {
	InputTokens  int64
	OutputTokens int64
	Known        bool
	// Model is what the runtime actually ran, which is not always what
	// was asked for.
	Model string
}

// ModelCallOutcome is the terminal state of a call.
type ModelCallOutcome struct {
	FinishReason string
	Usage        ModelCallUsage
	// Content is the assembled text: the worker's own full-text answer
	// when it did not stream, or the concatenation of the deltas THIS
	// SIDE ACCEPTED when it did. Deliberately the accepted set rather
	// than everything that arrived -- a dropped duplicate must not
	// reappear in the assembled text through a second path.
	Content    string
	Embeddings [][]float32
	Error      string
	ErrorCode  string
}

// ModelCallHandle is the caller's view of a running call.
type ModelCallHandle struct {
	requestId string
	worker    *Worker
	limits    ModelCallLimits

	deltas chan ModelCallDelta
	done   chan struct{}

	mu       sync.Mutex
	outcome  ModelCallOutcome
	endErr   error
	ended    bool
	lastSeq  uint64
	seqSeen  bool
	assembly strings.Builder
	// lastActivity is when the last accepted delta arrived, or the
	// call's start. Read by the idle watchdog.
	lastActivity time.Time
	closeOne     sync.Once

	// cancelFn sends a ModelCallCancel to the worker.
	cancelFn func(*memqlv1.ModelCallCancel) error
	// detach removes the call from its stream's table.
	detach func()
	clock  func() time.Time
}

// RequestId returns the server-minted call id.
func (h *ModelCallHandle) RequestId() string {
	if h == nil {
		return ""
	}
	return h.requestId
}

// Deltas is the stream of generated output. Closed when the call ends.
func (h *ModelCallHandle) Deltas() <-chan ModelCallDelta {
	if h == nil {
		return nil
	}
	return h.deltas
}

// Cancel asks the worker to stop. Idempotent; a cancel on an
// already-ended call is a no-op rather than an error.
func (h *ModelCallHandle) Cancel(reason string) error {
	if h == nil {
		return ErrModelCallNotFound
	}
	h.mu.Lock()
	ended := h.ended
	h.mu.Unlock()
	if ended {
		return nil
	}
	return h.cancelFn(&memqlv1.ModelCallCancel{RequestId: h.requestId, Reason: reason})
}

// Wait blocks until the call ends, the whole-call ceiling expires, the
// context is cancelled, or the machine goes silent past the idle
// ceiling. Every giving-up path cancels on the machine first, so a
// caller walking away never leaves a generation running on somebody's
// laptop.
func (h *ModelCallHandle) Wait(ctx context.Context) (ModelCallOutcome, error) {
	if h == nil {
		return ModelCallOutcome{}, ErrModelCallNotFound
	}
	deadline := h.clock().Add(h.limits.Timeout)
	// The idle ceiling is checked on a ticker rather than a timer that
	// every delta resets: a reset-per-delta timer on a fast local model
	// is thousands of timer operations a second to answer a question
	// with a ninety-second resolution.
	tick := time.NewTicker(h.idlePoll())
	defer tick.Stop()

	for {
		select {
		case <-h.done:
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.outcome, h.endErr
		case <-ctx.Done():
			_ = h.Cancel("caller_cancelled")
			return ModelCallOutcome{}, ctx.Err()
		case now := <-tick.C:
			if now.After(deadline) {
				_ = h.Cancel("call_timeout")
				return ModelCallOutcome{}, fmt.Errorf("worker: model call exceeded its %s ceiling", h.limits.Timeout)
			}
			h.mu.Lock()
			idleFor := now.Sub(h.lastActivity)
			h.mu.Unlock()
			if idleFor > h.limits.IdleTimeout {
				_ = h.Cancel("idle_timeout")
				return ModelCallOutcome{}, ErrModelCallIdle
			}
		}
	}
}

// idlePoll is how often Wait re-examines the deadlines. Bounded below
// so a short idle ceiling in a test does not spin.
func (h *ModelCallHandle) idlePoll() time.Duration {
	p := h.limits.IdleTimeout / 4
	if p < 10*time.Millisecond {
		p = 10 * time.Millisecond
	}
	if p > 5*time.Second {
		p = 5 * time.Second
	}
	return p
}

// deliverDelta hands one delta to the caller, enforcing monotonic seq.
//
// OUT-OF-ORDER AND DUPLICATE DELTAS ARE DROPPED -- the AppSession chunk
// rule, inherited because it is the same problem. A generation is a
// record: splicing a replayed delta back into the middle produces text
// no later reader can tell is wrong.
//
// A dropped delta still counts as ACTIVITY for the idle watchdog. The
// machine demonstrably spoke, and killing a call for silence while its
// duplicates arrive would be a lie about which failure occurred.
func (h *ModelCallHandle) deliverDelta(d ModelCallDelta) {
	h.mu.Lock()
	if h.ended {
		h.mu.Unlock()
		return
	}
	h.lastActivity = h.clock()
	if h.seqSeen && d.Seq <= h.lastSeq {
		h.mu.Unlock()
		return
	}
	h.lastSeq = d.Seq
	h.seqSeen = true
	if !d.Keepalive && d.Content != "" {
		h.assembly.WriteString(d.Content)
	}
	h.mu.Unlock()

	// A keepalive is a liveness signal, not output. It has already reset
	// the idle timer above; forwarding it would put empty deltas into
	// every consumer's loop for no reader's benefit.
	if d.Keepalive {
		return
	}

	select {
	case h.deltas <- d:
	case <-h.done:
	}
}

// finish records the terminal state and releases every waiter.
func (h *ModelCallHandle) finish(outcome ModelCallOutcome, err error) {
	h.closeOne.Do(func() {
		h.mu.Lock()
		h.ended = true
		if outcome.Content == "" {
			// The worker streamed rather than answering in one piece, so
			// the text is what this side accepted.
			outcome.Content = h.assembly.String()
		}
		h.outcome = outcome
		h.endErr = err
		h.mu.Unlock()
		close(h.done)
		close(h.deltas)
		if h.detach != nil {
			h.detach()
		}
	})
}

// ModelCallFunc is the per-stream hook that opens a call.
type ModelCallFunc func(ctx context.Context, req ModelCallRequest) (*ModelCallHandle, error)

// SetModelCallFunc wires the per-stream model-call hook. Called once
// per stream alongside SetDispatchFunc.
func (w *Worker) SetModelCallFunc(fn ModelCallFunc) {
	if w == nil {
		return
	}
	w.modelCallFn = fn
}

// ModelCapability is the capability name a machine advertises to be
// eligible for model calls at all. It sits beside HEADLESS /
// COMPUTERUSE in the same `capabilities` list.
const ModelCapability = "MODEL"

// StartModelCall opens a model call on this worker.
//
// It DOES take a concurrency slot, and that is the difference from an
// app session. A session is bounded by the delegation policy because it
// is one human-scale run; model calls are issued by the platform in
// whatever number the work needs, and the machine advertised a
// per-model ceiling precisely so something would honour it. Without the
// slot, `leastLoaded` would be ordering by a number nothing incremented.
func (w *Worker) StartModelCall(ctx context.Context, req ModelCallRequest) (*ModelCallHandle, error) {
	if w == nil || w.modelCallFn == nil {
		return nil, ErrModelCallUnsupported
	}
	if !IsValidModelCallKind(req.Kind) {
		return nil, fmt.Errorf("worker: unknown model call kind %q", req.Kind)
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, fmt.Errorf("worker: model call requires a model")
	}
	if !w.RunsModel(req.Model) {
		return nil, fmt.Errorf("worker: %s does not offer model %q", w.RegistrationId, req.Model)
	}
	return w.modelCallFn(ctx, req)
}

// ModelLabelPrefix is how a machine advertises a model it will serve.
// The router selects on the exact string, so no translation happens
// between what the cockpit reports and what a policy names.
const ModelLabelPrefix = "model:"

// RuntimeLabelPrefix names the runtime serving a model (ollama,
// openai-compatible). Reported for the operator's benefit; it steers no
// selection, because two machines running the same model through
// different runtimes are interchangeable to a caller.
const RuntimeLabelPrefix = "runtime:"

// ModelLabel renders the advertisement label for a model id.
func ModelLabel(modelId string) string {
	return ModelLabelPrefix + strings.TrimSpace(modelId)
}

// ModelIdFromLabel returns the model id a label names, and whether the
// label was one.
func ModelIdFromLabel(label string) (string, bool) {
	if !strings.HasPrefix(label, ModelLabelPrefix) {
		return "", false
	}
	id := strings.TrimSpace(strings.TrimPrefix(label, ModelLabelPrefix))
	return id, id != ""
}

// RunsModel reports whether the machine advertised this model.
//
// It reads the label MERGE the same way RunsApp reads the app
// inventory, and for the same reason: the question is what the machine
// offers right now, which is a fact the machine reports.
func (w *Worker) RunsModel(modelId string) bool {
	modelId = strings.TrimSpace(modelId)
	if w == nil || modelId == "" {
		return false
	}
	labels := w.LabelsSnapshot()
	_, ok := labels[ModelLabel(modelId)]
	return ok
}

// ModelsOffered lists the model ids the machine advertises.
func (w *Worker) ModelsOffered() []string {
	if w == nil {
		return nil
	}
	var out []string
	for k := range w.LabelsSnapshot() {
		if id, ok := ModelIdFromLabel(k); ok {
			out = append(out, id)
		}
	}
	return out
}
