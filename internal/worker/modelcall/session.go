package modelcall

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
)

// Stable error codes on ModelCallEnd. They are read by the engine's
// refusal report, which lists every machine considered and why each was
// ruled out, so each one has to name a DIFFERENT fix.
const (
	// CodeModelNotOffered: this machine does not currently advertise the
	// model. Reached when the router's view is a beat stale, or when the
	// owner removed it from models.allow since the advertisement.
	CodeModelNotOffered = "model_not_offered"
	// CodeConcurrencyExceeded: the model is offered but at its cap.
	CodeConcurrencyExceeded = "model_concurrency_exceeded"
	// CodeUnsupportedKind: not a kind this worker serves at all.
	CodeUnsupportedKind = "unsupported_kind"
	// CodeModalityUnsupported: the kind is one this worker serves and
	// the model never advertised the modality it needs.
	//
	// SEPARATE FROM CodeUnsupportedKind because the fixes are
	// different: an unknown kind is a version skew between the engine
	// and this cockpit, where an unadvertised modality is a stale
	// routing decision or a policy change -- and the refusal report
	// lists every machine considered and why, so two causes that send
	// an operator to different places get two codes.
	CodeModalityUnsupported = "modality_unsupported"
	// CodePayloadUnavailable: the kind is served and the call carried
	// no payload for it.
	//
	// Refuse missing media instead of silently turning a modality request into chat.
	CodePayloadUnavailable = "payload_unavailable"
	// CodeSchemaUnsupported: a response schema arrived for a model this
	// machine never advertised structured output for.
	CodeSchemaUnsupported = "schema_unsupported"
	// CodeToolsUnsupported: the call offered tools to a model this
	// machine never advertised tool calling for. It is separate from
	// CodeSchemaUnsupported because the FIX is separate -- structured
	// output and tool calling are two attributes on the label and two
	// lines in models.allow, and a report that conflated them would send
	// the operator to the wrong one.
	CodeToolsUnsupported = "tools_unsupported"
	// CodeDuplicateRequest: a request id already live on this worker.
	CodeDuplicateRequest = "duplicate_request"
	// CodeRuntimeError: the local runtime failed the call.
	CodeRuntimeError = "runtime_error"
	// CodeCancelled: the cluster cancelled it.
	CodeCancelled = "cancelled"
	// CodeTimeout: a deadline on the envelope expired.
	CodeTimeout = "timeout"
	// CodeWorkerStopped: the stream went, or the worker is draining.
	CodeWorkerStopped = "worker_stopped"
)

// Sender is what the manager needs from the worker's connection.
type Sender interface {
	SendModelCallDelta(requestID string, seq uint64, content string, keepalive bool) error
	SendModelCallEnd(end *memqlv1.ModelCallEnd) error
}

// Inventory is the live view of what this machine offers. It is read at
// ADMISSION rather than captured at connect, so a model dropped from
// models.allow stops being servable at the next call rather than at the
// next reconnect -- the advertisement is a promise, and honouring a call
// outside it would keep a revoked model running on somebody's hardware.
type Inventory interface {
	Models(ctx context.Context) models.Inventory
}

// Options configures NewManager.
type Options struct {
	Logger     *slog.Logger
	Inventory  Inventory
	HTTPClient *http.Client
	// Getenv resolves a declared runtime's api_key_env. Defaults to
	// os.Getenv.
	Getenv func(string) string
	// Limiter is the machine's concurrency ceiling. A worker serving
	// several clusters passes ONE to every Manager (see Limiter); nil
	// builds a private one, which is the whole machine when this Manager
	// is the only one.
	Limiter *Limiter
	// SlotWaitLogFloor is the wait below which a call that had to queue for
	// a concurrency slot is not logged. Zero or less is one second: a slot
	// that frees within a second is the machine doing its job.
	SlotWaitLogFloor time.Duration
}

// Manager owns the live model calls on this worker.
type Manager struct {
	logger    *slog.Logger
	inventory Inventory
	http      *http.Client
	getenv    func(string) string
	limiter   *Limiter

	slotWaitFloor time.Duration

	mu   sync.Mutex
	live map[string]*call
}

type call struct {
	requestID string
	cancel    context.CancelFunc
	// level and purpose are what the engine said about the call. They steer
	// nothing here -- the router chose the model -- and exist so the line
	// logged when a call waits for a slot can say which kind of work was
	// kept waiting. The level is logged as call_level: "level" is the severity
	// key of the JSON handler, and a second one hides the severity.
	level, purpose string

	mu sync.Mutex
	// modelID is set once the model RESOLVES and its slot is taken in the
	// Limiter, which is after the call is already registered -- see Start.
	// Empty means no concurrency slot was ever taken, so releasing must
	// not give one back.
	modelID string
	reason  string // why it was aborted: FinishCancelled / FinishTimeout
	code    string
	detail  string // the human sentence, when the aborter supplied one
}

// NewManager builds the manager the worker runs with.
func NewManager(opts Options) *Manager {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	client := opts.HTTPClient
	if client == nil {
		// No client-level timeout: the envelope owns the deadlines, and
		// a second one here would cut a legitimate ten-minute generation
		// off at whatever number this file happened to pick.
		client = &http.Client{}
	}
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	limiter := opts.Limiter
	if limiter == nil {
		limiter = NewLimiter()
	}
	floor := opts.SlotWaitLogFloor
	if floor <= 0 {
		floor = time.Second
	}
	return &Manager{
		logger:        opts.Logger,
		inventory:     opts.Inventory,
		http:          client,
		getenv:        getenv,
		limiter:       limiter,
		slotWaitFloor: floor,
		live:          make(map[string]*call),
	}
}

// Live reports how many calls are running. The runner reads it before
// spending a reconnect on a changed model set.
func (m *Manager) Live() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.live)
}

// Start admits and runs one call. It returns immediately; the call
// reports itself on the stream.
func (m *Manager) Start(ctx context.Context, sender Sender, start *memqlv1.ModelCallStart) {
	if m == nil || start == nil {
		return
	}
	requestID := start.GetRequestId()
	if requestID == "" {
		// Nothing to correlate a refusal with, so there is nothing to
		// send. Dropping it is the only honest option.
		m.logger.Warn("model call arrived with no request id; dropped")
		return
	}

	// EVERYTHING SLOW HAPPENS IN THE GOROUTINE, and the split is not
	// stylistic. Start runs on the worker's stream-recv goroutine -- the
	// one goroutine that reads every inbound message -- and resolving the
	// model reads the live inventory, which can mean probing a runtime
	// that is not answering. Doing that here would stall tool dispatches,
	// cancels and drain behind a socket timeout.
	//
	// What stays synchronous is the REGISTRATION, and that is not
	// stylistic either: a Cancel arriving immediately after Start has to
	// find the call. Registering in the goroutine leaves a window in
	// which a cancel is silently dropped and the generation runs on to
	// its own timeout -- exactly the outcome cancel exists to prevent.
	limits := limitsFrom(start.GetLimits())
	callCtx, cancel := context.WithTimeout(ctx, limits.timeout)
	c := &call{requestID: requestID, cancel: cancel, level: start.GetLevel(), purpose: start.GetPurpose()}

	m.mu.Lock()
	_, duplicate := m.live[requestID]
	if !duplicate {
		m.live[requestID] = c
	}
	m.mu.Unlock()

	if duplicate {
		cancel()
		m.sendRefusal(sender, requestID, CodeDuplicateRequest,
			"a call with this request id is already running on this worker")
		return
	}

	go func() {
		defer cancel()
		defer m.release(c)
		info, inv, refusal := m.resolve(ctx, start)
		// The delta stream exists from here, before the slot: a call that
		// waits for one keeps the cluster's idle clock alive while it does
		// (see admit), and the seq it numbered its keepalives with carries
		// on into the generation.
		stream := &deltaStream{sender: sender, requestID: requestID}
		stream.touch()
		if refusal == nil {
			refusal = m.admit(callCtx, c, info, inv, limits, stream)
		}
		if refusal != nil {
			refusal.RequestId = requestID
			if err := sender.SendModelCallEnd(refusal); err != nil {
				m.logger.Warn("failed to send model call refusal", "request_id", requestID, "error", err)
			}
			return
		}
		m.run(callCtx, sender, c, info, limits, start, stream)
	}()
}

func (m *Manager) sendRefusal(sender Sender, requestID, code, message string) {
	end := refuse(code, message)
	end.RequestId = requestID
	if err := sender.SendModelCallEnd(end); err != nil {
		m.logger.Warn("failed to send model call refusal", "request_id", requestID, "error", err)
	}
}

// Cancel stops a running call. A cancel for a call this worker is not
// running is ignored rather than reported: the engine cancels
// optimistically on its own timeout paths, and a refusal for a call that
// already ended would be noise the operator has to learn to skip.
func (m *Manager) Cancel(c *memqlv1.ModelCallCancel) {
	if m == nil || c == nil {
		return
	}
	m.abort(c.GetRequestId(), FinishCancelled, CodeCancelled, c.GetReason())
}

// StopAll ends every live call. Called on disconnect and on drain.
//
// The stream is the only channel back to the caller, so a call that
// outlived it has nowhere to report -- but the GENERATION is still
// running on somebody's GPU, and cancelling it is the point. Whether the
// End reaches the cluster is secondary; freeing the hardware is not.
func (m *Manager) StopAll(reason string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	ids := make([]string, 0, len(m.live))
	for id := range m.live {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.abort(id, FinishError, CodeWorkerStopped, reason)
	}
}

func (m *Manager) abort(requestID, finish, code, reason string) {
	m.mu.Lock()
	c := m.live[requestID]
	m.mu.Unlock()
	if c == nil {
		return
	}
	c.mu.Lock()
	// FIRST abort wins. A cancel that arrives while the watchdog is
	// already tearing the call down must not rewrite the reason the
	// caller will be told, or the End would name whichever goroutine
	// happened to be scheduled second.
	if c.reason == "" {
		c.reason, c.code, c.detail = finish, code, reason
	}
	c.mu.Unlock()
	c.cancel()
}

// release drops the call and the slot it took, if it took one. A call
// refused before its model resolved never took anything, so releasing it
// must not give a slot back -- that would let the next call past a ceiling
// this one never occupied.
func (m *Manager) release(c *call) {
	c.mu.Lock()
	modelID := c.modelID
	c.mu.Unlock()

	m.mu.Lock()
	delete(m.live, c.requestID)
	m.mu.Unlock()
	if modelID != "" {
		m.limiter.release(modelID)
	}
}

// -----------------------------------------------------------------------------
// Admission
// -----------------------------------------------------------------------------

type callLimits struct {
	timeout   time.Duration
	idle      time.Duration
	keepalive time.Duration
}

func limitsFrom(l *memqlv1.ModelCallLimits) callLimits {
	out := callLimits{timeout: DefaultTimeout, idle: DefaultIdleTimeout, keepalive: DefaultKeepalive}
	if l == nil {
		return out
	}
	if n := l.GetTimeoutSeconds(); n > 0 {
		out.timeout = time.Duration(n) * time.Second
	}
	if n := l.GetIdleTimeoutSeconds(); n > 0 {
		out.idle = time.Duration(n) * time.Second
	}
	if n := l.GetKeepaliveSeconds(); n > 0 {
		out.keepalive = time.Duration(n) * time.Second
	}
	// A keepalive at or above the idle ceiling cannot keep anything
	// alive: the first one would arrive after the deadline it exists to
	// push back. Rather than fail a call over a caller's arithmetic, the
	// cadence is tightened to something that works.
	if out.keepalive >= out.idle {
		out.keepalive = out.idle / 2
	}
	if out.keepalive <= 0 {
		out.keepalive = DefaultKeepalive
	}
	return out
}

// resolve finds the model this call names, or returns the End that
// refuses the call. The slot is admit's to take.
//
// It runs AFTER the call is registered (see Start), so a refusal here is a
// refusal for a call that already exists and is already cancellable. The
// inventory is read at this moment rather than captured at connect: a
// model dropped from models.allow stops being servable at the next call
// rather than at the next reconnect, because the advertisement is a
// promise and honouring a call outside it would keep a revoked model
// running on somebody's hardware.
func (m *Manager) resolve(ctx context.Context, start *memqlv1.ModelCallStart) (models.Info, models.Inventory, *memqlv1.ModelCallEnd) {
	kind := start.GetKind()
	modality, isModality := modalityKinds[kind]
	if kind != KindChat && kind != KindEmbedding && !isModality {
		return models.Info{}, models.Inventory{}, refuse(CodeUnsupportedKind,
			fmt.Sprintf("this worker serves %s; the call asked for %q",
				strings.Join(quoteAll(ServedKinds()), ", "), kind))
	}

	var inv models.Inventory
	if m.inventory != nil {
		inv = m.inventory.Models(ctx)
	}
	info, ok := inv.Find(start.GetModel())
	if !ok {
		return models.Info{}, models.Inventory{}, refuse(CodeModelNotOffered,
			fmt.Sprintf("this machine does not currently offer model %q", start.GetModel()))
	}
	// A SCHEMA ONLY MEANS SOMETHING ON A CHAT-SHAPED CALL. Vision is
	// one; transcription, speech and image generation are not -- their
	// answers are a transcript, audio bytes and image bytes, and there
	// is no text for a schema to constrain. A schema arriving on one of
	// those is refused rather than dropped, because dropping it would
	// let a caller believe it had asked for something.
	if len(start.GetResponseFormatSchema()) > 0 && isModalityKind(kind) && kind != KindVision {
		return models.Info{}, models.Inventory{}, refuse(CodeSchemaUnsupported,
			fmt.Sprintf("a %q call returns no text for a response schema to constrain", kind))
	}
	if len(start.GetResponseFormatSchema()) > 0 && !info.StructuredOutput {
		// The router only sends a schema to a machine that advertised
		// the capability, so this is a stale advertisement rather than a
		// routing bug -- and answering prose instead would defeat the
		// gating that put the call here.
		return models.Info{}, models.Inventory{}, refuse(CodeSchemaUnsupported,
			fmt.Sprintf("model %q does not advertise structured output on this machine", info.ID))
	}
	if end := toolsRefusal(info, toolsFromStart(start)); end != nil {
		return models.Info{}, models.Inventory{}, end
	}
	if kind == KindEmbedding && !info.Embeddings {
		return models.Info{}, models.Inventory{}, refuse(CodeModelNotOffered,
			fmt.Sprintf("model %q does not advertise embeddings on this machine", info.ID))
	}
	if isModality {
		// The advertised flag gates the call, exactly as the structured
		// and tools flags above do and for the same reason: the router
		// only sends a modality to a machine that advertised it, so
		// arriving here without one is a stale advertisement, and
		// serving it anyway would defeat the gating that put the call
		// here.
		if !modality.Advertised(info.Attributes) {
			return models.Info{}, models.Inventory{}, refuse(CodeModalityUnsupported,
				fmt.Sprintf("model %q does not advertise %s on this machine", info.ID, modality.Word))
		}
		// Required media must actually be present in the envelope.
		if _, ok := payloadFor(start); !ok {
			return models.Info{}, models.Inventory{}, refuse(CodePayloadUnavailable, modalityUnavailableSentence(modality.Word))
		}
	}

	return info, inv, nil
}

// admit takes this call's concurrency slot, WAITING for one when the
// machine is full, or returns the End that refuses the call.
//
// Both ceilings are this machine's to hold. The engine rations by the
// advertised numbers, but the advertisement is a claim about this
// hardware: two replicas selecting at the same moment is an ordinary race
// rather than a bug to fix upstream, and on a machine serving several
// clusters every one of them was told the same numbers. The Limiter is
// shared by every cluster's Manager for that reason (memql-cockpit#432).
//
// WAITING, NOT REFUSING, because a refusal is a FAILED call: the engine
// treats any error a worker ends a call with as the call having run, and
// does not try another machine. Before the ceiling was shared, a second
// cluster's call went straight to the runtime and queued there; a call
// that finds the machine full now queues here instead, where the ceiling
// can see it. It waits as long as a call queued in the runtime could have
// -- its own idle ceiling, after which the watchdog would have ended it
// for silence -- and says it is alive meanwhile, with keepalives on the
// same cadence the generation will use, so the cluster's copy of that
// ceiling does not end it first. Only then is it refused.
func (m *Manager) admit(ctx context.Context, c *call, info models.Info, inv models.Inventory, limits callLimits, stream *deltaStream) *memqlv1.ModelCallEnd {
	ceiling := machineCap(inv)
	if m.claim(c, info, ceiling) {
		return nil
	}
	began := time.Now()
	m.logger.Debug("model call waiting for a concurrency slot", "request_id", c.requestID, "model", info.ID)
	deadline := time.NewTimer(limits.idle)
	defer deadline.Stop()
	keepalive := time.NewTicker(limits.keepalive)
	defer keepalive.Stop()
	for {
		freed := m.limiter.changed()
		// Re-checked AFTER taking the channel, so a release between the
		// failed claim and the wait is never missed.
		if m.claim(c, info, ceiling) {
			m.slotWaited(c, info, time.Since(began))
			return nil
		}
		select {
		case <-freed:
		case <-keepalive.C:
			if err := stream.keepalive(); err != nil {
				return refuse(CodeWorkerStopped, "the stream to the cluster went while this call waited for a concurrency slot")
			}
		case <-deadline.C:
			m.logger.Warn("model call refused after waiting for a concurrency slot",
				"request_id", c.requestID, "model", info.ID, "call_level", c.level, "purpose", c.purpose,
				"waited_ms", time.Since(began).Milliseconds(), "model_max", info.MaxConcurrent,
				"error_code", CodeConcurrencyExceeded)
			return refuse(CodeConcurrencyExceeded, concurrencyRefusal(info, ceiling, limits.idle))
		case <-ctx.Done():
			// A cancel, or the call's own timeout, ends a wait with neither a
			// grant nor a refusal, so the refusal above never runs for it. The
			// Debug line before the loop is the only other record of the wait,
			// and the default level never prints it.
			finish, code, detail := m.classify(c, ctx.Err())
			if waited := time.Since(began); waited >= m.slotWaitFloor {
				m.logger.Warn("model call ended while waiting for a concurrency slot",
					"request_id", c.requestID, "model", info.ID, "call_level", c.level, "purpose", c.purpose,
					"waited_ms", waited.Milliseconds(), "model_max", info.MaxConcurrent,
					"error_code", code)
			}
			end := refuse(code, detail)
			end.FinishReason = finish
			return end
		}
	}
}

// slotWaited says that a call waited for its slot, and for how long.
//
// IT IS THE EVIDENCE for a decision this cockpit deliberately has not
// made: reserving slots for the fast level (D6 in the fleet-tuning design,
// docs/superpowers/specs/2026-10-18-fleet-tuning-design.md). Whether
// background work starves fast work here is a fact about this machine's
// traffic, and level and purpose are the only things the call says about
// itself.
func (m *Manager) slotWaited(c *call, info models.Info, waited time.Duration) {
	if waited < m.slotWaitFloor {
		return
	}
	m.logger.Info("model call waited for a concurrency slot",
		"request_id", c.requestID, "model", info.ID, "call_level", c.level, "purpose", c.purpose,
		"waited_ms", waited.Milliseconds(), "model_max", info.MaxConcurrent)
}

// claim takes the slot if one is free. Taking it and recording which one
// was taken happen together; release reads the same field to know
// whether to give it back.
func (m *Manager) claim(c *call, info models.Info, ceiling int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !m.limiter.tryAcquire(info.ID, info.MaxConcurrent, ceiling) {
		return false
	}
	c.modelID = info.ID
	return true
}

// concurrencyRefusal names the ceiling that stayed full, and for how long.
func concurrencyRefusal(info models.Info, ceiling int, waited time.Duration) string {
	if info.MaxConcurrent > 0 {
		return fmt.Sprintf("model %q stayed at its concurrency limit of %d on this machine for %s", info.ID, info.MaxConcurrent, waited)
	}
	return fmt.Sprintf("this machine stayed at its model concurrency limit of %d for %s", ceiling, waited)
}

// machineCap is the machine-wide ceiling, derived from the SAME numbers
// the registration advertises, so what is enforced and what is claimed
// cannot drift apart.
func machineCap(inv models.Inventory) int {
	total := 0
	for _, mi := range inv.Advertised() {
		total += mi.MaxConcurrent
	}
	return total
}

// toolsRefusal is the tool-calling admission gate. A call that offers
// tools to a model this machine never advertised `tools=1` for is refused
// HERE, before any request is built, so the runtime is never handed a
// catalogue it would silently ignore -- a runtime that ignores one
// answers in prose, and the caller then reads prose where it was waiting
// for a call. That failure surfaces wherever the tool result was due and
// names nothing on this machine.
func toolsRefusal(info models.Info, tools []Tool) *memqlv1.ModelCallEnd {
	if len(tools) == 0 || info.Tools {
		return nil
	}
	return refuse(CodeToolsUnsupported,
		fmt.Sprintf("model %q does not advertise tool calling on this machine", info.ID))
}

func refuse(code, message string) *memqlv1.ModelCallEnd {
	return &memqlv1.ModelCallEnd{
		FinishReason: FinishError,
		Error:        message,
		ErrorCode:    code,
	}
}

// -----------------------------------------------------------------------------
// Execution
// -----------------------------------------------------------------------------

func (m *Manager) run(ctx context.Context, sender Sender, c *call, info models.Info, limits callLimits, start *memqlv1.ModelCallStart, stream *deltaStream) {
	// The level rides along for the owner's own logs (memql#5393). It never
	// changes the model: the router chose this one from this machine's
	// advertisement, and a runtime has the model it was asked for. The one
	// knob it reaches is thinking -- a fast call on Ollama runs with it off
	// (ollamaThink).
	m.logger.Debug("model call running",
		"request_id", c.requestID, "model", info.ID, "kind", start.GetKind(),
		"call_level", start.GetLevel(), "purpose", start.GetPurpose())

	// The send clock starts after admission. The idle verdict starts only
	// after actual runtime output: model loading and prompt evaluation can
	// legitimately outlast the inter-output ceiling. The absolute deadline
	// still bounds startup and time spent queued.
	stream.touch()
	client := m.clientFor(info)
	// Ollama buffers each tool call until its arguments are complete. Even
	// after streamed reasoning, silence can mean active tool generation.
	// That protocol cannot prove inter-output idleness for tool-enabled calls;
	// the absolute deadline and cancellation still bound the whole request.
	_, buffersTools := client.(*ollamaClient)
	observeIdle := !buffersTools || len(start.GetTools()) == 0

	// The watchdog is stopped BEFORE the End is sent, not by a defer that
	// runs after it. A keepalive racing out behind the End is a delta for
	// a call the engine has already closed -- harmless there, but it is a
	// protocol violation this side can simply not commit.
	watchdogDone := make(chan struct{})
	var stopOnce sync.Once
	stopWatchdog := func() { stopOnce.Do(func() { close(watchdogDone) }) }
	defer stopWatchdog()
	go m.watchdog(ctx, c, stream, limits, observeIdle, watchdogDone)

	ctx = withRuntimeProgress(ctx, stream.runtimeProgress)
	var (
		res   Result
		modal modalityResult
		err   error
	)
	switch kind := start.GetKind(); {
	case kind == KindEmbedding:
		res, err = client.Embed(ctx, EmbedRequest{Model: info.ID, Input: start.GetEmbeddingInput()})

	case isModalityKind(kind):
		// Admission has validated the required media payload.
		payload, _ := payloadFor(start)
		res, modal, err = m.runModality(ctx, client, info, start, payload, stream.emit)

	default:
		res, err = client.Chat(ctx, ChatRequest{
			Model:    info.ID,
			Messages: messagesFrom(start.GetMessages()),
			Params:   paramsFrom(start.GetParams()),
			Schema:   start.GetResponseFormatSchema(),
			Tools:    toolsFromStart(start),
			Level:    start.GetLevel(),
		}, stream.emit)
	}

	stopWatchdog()

	end := &memqlv1.ModelCallEnd{RequestId: c.requestID}
	if err != nil {
		finish, code, detail := m.classify(c, err)
		end.FinishReason = finish
		end.ErrorCode = code
		end.Error = detail
	} else {
		end.FinishReason = res.FinishReason
		if end.FinishReason == "" {
			end.FinishReason = FinishStop
		}
	}
	// Usage and embeddings ride even on a failure: a call that produced
	// three hundred tokens and then lost its runtime still SPENT those
	// tokens, and a loop cap that never heard about them is a loop cap
	// that misses exactly the runaway it exists to catch.
	end.Usage = usageProto(res.Usage)
	end.Embeddings = embeddingsProto(res.Embeddings)
	attachToolCalls(end, res.ToolCalls)
	attachModalityResult(end, modal)

	if err := sender.SendModelCallEnd(end); err != nil {
		m.logger.Warn("failed to send model call end", "request_id", c.requestID, "error", err)
	}
}

// watchdog enforces the idle ceiling and emits the keepalives that make
// it enforceable on the other side too.
func (m *Manager) watchdog(ctx context.Context, c *call, stream *deltaStream, limits callLimits, observeIdle bool, done <-chan struct{}) {
	// THE CHECK CADENCE IS HALF THE KEEPALIVE INTERVAL, and the halving
	// is what makes keepalives fire at all.
	//
	// Ticking at exactly limits.keepalive and then testing
	// `sinceSend() >= limits.keepalive` makes every tick land within
	// scheduling jitter of the threshold it is testing: the elapsed
	// time at tick N is the ticker period, which is the threshold, so
	// whether the comparison is true is a coin flip. Half the calls
	// emit no keepalive at all, and the engine -- whose idle ceiling
	// the keepalive exists to keep enforceable -- sees silence it
	// cannot distinguish from a wedged machine.
	//
	// Halved, a keepalive lands somewhere in [keepalive, 1.5 x
	// keepalive], comfortably inside the idle ceiling (which limitsFrom
	// holds at strictly more than the keepalive), and the idle check
	// keeps its own granularity well under the deadline it guards.
	tick := limits.keepalive / 2
	if tick <= 0 {
		tick = limits.keepalive
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			if observeIdle && stream.idleExpired(limits.idle) {
				// This machine's own runtime has gone quiet past the
				// ceiling. Ending it here frees the GPU; waiting for the
				// engine's copy of the same deadline would not.
				c.mu.Lock()
				if c.reason == "" {
					c.reason, c.code = FinishTimeout, CodeTimeout
				}
				c.mu.Unlock()
				c.cancel()
				return
			}
			if stream.sinceSend() >= limits.keepalive {
				// A delta with no content, carrying its own seq so it can
				// never be mistaken for a replayed content delta.
				//
				// The cadence is measured against the last SEND, not the
				// last content: two keepalives in a row are correct
				// while a runtime is thinking, and measuring this
				// against the idle clock would emit one on every tick.
				if err := stream.keepalive(); err != nil {
					c.cancel()
					return
				}
			}
		}
	}
}

// runModality serves one of the four modality kinds.
//
// THE CLIENT IS ASKED, NOT ASSUMED. Each kind needs a capability the
// runtime may not have, so the type assertion is the check -- and its
// failure is a refusal naming the runtime rather than a panic or a
// silent fall back to a text completion. A machine reaches here only
// when it advertised the flag, so a failure here means the flag and the
// runtime disagree, which is worth saying plainly.
func (m *Manager) runModality(
	ctx context.Context,
	client Client,
	info models.Info,
	start *memqlv1.ModelCallStart,
	payload Payload,
	emit Emit,
) (Result, modalityResult, error) {
	messages := messagesFrom(start.GetMessages())
	params := paramsFrom(start.GetParams())

	switch start.GetKind() {
	case KindVision:
		c, ok := client.(VisionClient)
		if !ok {
			return Result{}, modalityResult{}, unservedByRuntime(info, "vision")
		}
		res, err := c.Vision(ctx, VisionRequest{
			Model: info.ID, Messages: messages, Images: payload.Images, Params: params,
			// The schema travels. A vision call IS a chat call, and
			// resolve only admitted this one because the model
			// advertises structured output -- so dropping it here
			// would answer prose to a call that was routed on the
			// promise of JSON.
			Schema: start.GetResponseFormatSchema(),
		}, emit)
		return res, modalityResult{}, err

	case KindTranscribe:
		c, ok := client.(Transcriber)
		if !ok {
			return Result{}, modalityResult{}, unservedByRuntime(info, "transcription")
		}
		// The container is CONVERTED from the wire's media type, and a
		// media type this side does not know yields no container --
		// which Transcribe refuses by name rather than guessing.
		format := audioFormatFor(payload.AudioMediaType)
		if format == "" {
			return Result{}, modalityResult{}, fmt.Errorf(
				"transcribe: the audio arrived as %q, which this machine cannot name a container for",
				payload.AudioMediaType)
		}
		out, err := c.Transcribe(ctx, TranscribeRequest{
			Model: info.ID, Audio: payload.Audio, Format: format, Prompt: promptFrom(messages),
		})
		if err != nil {
			return Result{}, modalityResult{}, err
		}
		// The transcript is CONTENT, so it goes out as a delta the same
		// way a generation would: the engine assembles what it accepts,
		// and a caller that streams and a caller that does not are the
		// same shape on the other side.
		if out.Text != "" {
			if err := emit(out.Text); err != nil {
				return Result{}, modalityResult{}, err
			}
		}
		return Result{FinishReason: FinishStop, Usage: out.Usage},
			modalityResult{Segments: out.Segments}, nil

	case KindSpeak:
		c, ok := client.(Speaker)
		if !ok {
			return Result{}, modalityResult{}, unservedByRuntime(info, "speech")
		}
		req := payload.Speech
		req.Model, req.Text = info.ID, promptFrom(messages)
		out, err := c.Speak(ctx, req)
		if err != nil {
			return Result{}, modalityResult{}, err
		}
		return Result{FinishReason: FinishStop, Usage: out.Usage},
			modalityResult{Audio: out.Audio, AudioMediaType: out.MediaType}, nil

	case KindImage:
		c, ok := client.(ImageGenerator)
		if !ok {
			return Result{}, modalityResult{}, unservedByRuntime(info, "image generation")
		}
		req := payload.Image
		req.Model, req.Prompt = info.ID, promptFrom(messages)
		out, err := c.GenerateImage(ctx, req)
		if err != nil {
			return Result{}, modalityResult{}, err
		}
		return Result{FinishReason: FinishStop, Usage: out.Usage},
			modalityResult{Images: out.Images}, nil
	}
	return Result{}, modalityResult{}, fmt.Errorf("modelcall: %q is not a modality kind", start.GetKind())
}

// unservedByRuntime names the disagreement rather than the symptom. A
// call only reaches runModality when the machine ADVERTISED the flag,
// so a runtime that cannot serve it means the advertisement and the
// runtime disagree -- and the operator needs the model and the modality
// to find out which.
func unservedByRuntime(info models.Info, word string) error {
	return fmt.Errorf("model %q advertises %s and its runtime (%s) does not serve it",
		info.ID, word, info.Runtime)
}

// promptFrom is the text half of a modality call: the LAST user turn.
//
// A speak call's text and an image call's prompt ride `messages` as
// ordinary user turns (memql#5137), so there is no separate field to
// read -- and the LAST one is the request, where an earlier one is
// context the caller chose to include.
func promptFrom(messages []Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			return messages[i].Content
		}
	}
	return ""
}

// classify turns a transport error into the envelope's closed set. An
// abort reason recorded by Cancel or the watchdog WINS over the context
// error it produced, because "cancelled" and "timed out" are different
// answers to the caller and both present here as context.Canceled.
func (m *Manager) classify(c *call, err error) (finish, code, detail string) {
	c.mu.Lock()
	reason, recorded, supplied := c.reason, c.code, c.detail
	c.mu.Unlock()
	if reason != "" {
		if supplied != "" {
			return reason, recorded, supplied
		}
		switch reason {
		case FinishCancelled:
			return FinishCancelled, recorded, "the cluster cancelled this call"
		case FinishTimeout:
			return FinishTimeout, recorded, "the local runtime stopped producing output past the idle ceiling"
		default:
			return reason, recorded, err.Error()
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return FinishTimeout, CodeTimeout, "the call exceeded its timeout"
	}
	if errors.Is(err, context.Canceled) {
		return FinishCancelled, CodeCancelled, "the call was cancelled"
	}
	return FinishError, CodeRuntimeError, err.Error()
}

func (m *Manager) clientFor(info models.Info) Client {
	return NewClient(info, m.http, m.getenv)
}

// NewClient builds the runtime client for one model.
//
// It is the SINGLE place that knows how to reach a runtime from a
// models.Info, and it is exported so internal/worker/probe reaches it
// the same way this manager does. Two constructors would be two places
// for the api_key_env resolution and the base URL to drift, and the
// failure is a probe that authenticates where the serving path does not
// -- which presents as a model that measures fine and refuses every
// real call.
//
// getenv may be nil, which resolves no api_key_env: a declared runtime
// that needs a bearer then fails its call rather than sending an empty
// one, which is the fail-closed direction.
func NewClient(info models.Info, httpClient *http.Client, getenv func(string) string) Client {
	if httpClient == nil {
		// No client-level timeout, for the reason NewManager gives: the
		// envelope owns the deadlines and a second one here would cut a
		// legitimate long generation off at whatever number this file
		// happened to pick.
		httpClient = &http.Client{}
	}
	if info.Kind == models.KindOpenAICompatible {
		key := ""
		if info.APIKeyEnv != "" && getenv != nil {
			key = getenv(info.APIKeyEnv)
		}
		return &openAIClient{baseURL: info.BaseURL, apiKey: key, http: httpClient, transcription: info.Transcription, voices: info.Voices}
	}
	return &ollamaClient{
		baseURL: info.BaseURL, http: httpClient,
		keepAlive: info.KeepAlive, workingContext: info.WorkingContext,
	}
}

// -----------------------------------------------------------------------------
// The delta stream
// -----------------------------------------------------------------------------

// deltaStream assigns the monotonic seq and tracks when output last
// moved. Content deltas and keepalives share the counter on purpose: the
// engine's rule is "strictly increasing", and two sources numbering
// independently would collide on the first keepalive.
type deltaStream struct {
	sender    Sender
	requestID string

	mu  sync.Mutex
	seq uint64
	// lastSend is when this worker last put ANYTHING on the stream,
	// content or keepalive. It drives the keepalive CADENCE.
	lastSend time.Time
	// lastContent is when the RUNTIME last produced answer, reasoning or tool
	// output. It drives
	// the idle VERDICT, and the two are separate clocks for a reason
	// that is easy to get wrong: a keepalive is this worker's own
	// output and says nothing whatever about the runtime. A single
	// clock that a keepalive reset would make the idle ceiling
	// unreachable -- every keepalive pushes the deadline it exists to
	// enforce, so a wedged runtime is never noticed and the call runs
	// to the whole-call timeout while the engine, receiving keepalives,
	// believes the machine is healthy.
	lastContent time.Time
	started     bool // actual runtime output; worker keepalives do not start generation
}

func (s *deltaStream) touch() {
	s.mu.Lock()
	now := time.Now()
	s.lastSend, s.lastContent = now, now
	s.mu.Unlock()
}

// Private reasoning and partial tool arguments move only the runtime clock.
// Keepalive cadence stays independent, and none of that text leaves the adapter.
func (s *deltaStream) runtimeProgress() {
	s.mu.Lock()
	s.lastContent = time.Now()
	s.started = true
	s.mu.Unlock()
}

// idleFor is how long THE RUNTIME has been silent. Keepalives do not
// reset it -- see lastContent.
func (s *deltaStream) idleFor() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastContent)
}

// Loading the model and evaluating its prompt happen before the first output.
// They are bounded by the call's absolute deadline, not the inter-output idle
// ceiling. Once any answer, reasoning or tool output arrives, that shorter
// ceiling applies. Keepalives never turn startup into apparent model progress.
func (s *deltaStream) idleExpired(limit time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started && time.Since(s.lastContent) >= limit
}

// sinceSend is how long since anything went out on the stream, which is
// what the keepalive cadence is measured against.
func (s *deltaStream) sinceSend() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastSend)
}

func (s *deltaStream) emit(content string) error {
	return s.send(content, false)
}

func (s *deltaStream) keepalive() error {
	return s.send("", true)
}

func (s *deltaStream) send(content string, keepalive bool) error {
	s.mu.Lock()
	seq := s.seq
	s.seq++
	now := time.Now()
	s.lastSend = now
	// Only runtime output moves the idle clock (including private output via
	// runtimeProgress). A keepalive proves this worker
	// is alive to the engine; it proves nothing about the runtime, and
	// letting it reset the idle verdict is what makes the ceiling
	// unenforceable from this side.
	if !keepalive {
		s.started = true
		s.lastContent = now
	}
	s.mu.Unlock()
	return s.sender.SendModelCallDelta(s.requestID, seq, content, keepalive)
}

// -----------------------------------------------------------------------------
// Wire conversions
//
// Wire mappings preserve the scoped catalogue, tool history and binary media.
// -----------------------------------------------------------------------------

// toolsFromStart preserves the engine's scoped catalogue through the worker hop.
func toolsFromStart(start *memqlv1.ModelCallStart) []Tool {
	var out []Tool
	for _, t := range start.GetTools() {
		out = append(out, Tool{Name: t.GetName(), Description: t.GetDescription(), ParametersJSON: t.GetParametersJson()})
	}
	return out
}
func messagesFrom(in []*memqlv1.ModelCallMessage) []Message {
	out := make([]Message, 0, len(in))
	for _, m := range in {
		out = append(out, messageFrom(m))
	}
	return out
}
func messageFrom(m *memqlv1.ModelCallMessage) Message {
	out := Message{Role: m.GetRole(), Content: m.GetContent(), ToolCallID: m.GetToolCallId(), Name: m.GetName()}
	for _, call := range m.GetToolCalls() {
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: call.GetId(), Name: call.GetName(), ArgumentsJSON: call.GetArgumentsJson()})
	}
	for _, image := range m.GetImages() {
		out.Images = append(out.Images, ImagePart{Data: image.GetData(), MediaType: image.GetMediaType()})
	}
	return out
}

// The authoritative complete list is returned on End, even for streaming runtimes.
func attachToolCalls(end *memqlv1.ModelCallEnd, calls []ToolCall) {
	for i, call := range calls {
		end.ToolCalls = append(end.ToolCalls, &memqlv1.ModelCallToolCall{Id: call.ID, Name: call.Name, ArgumentsJson: call.ArgumentsJSON, Index: int32(i)})
	}
}

func paramsFrom(p *memqlv1.ModelCallParams) Params {
	if p == nil {
		return Params{}
	}
	return Params{
		Temperature:     p.GetTemperature(),
		TemperatureSet:  p.GetTemperatureSet(),
		TopP:            p.GetTopP(),
		TopPSet:         p.GetTopPSet(),
		MaxOutputTokens: p.GetMaxOutputTokens(),
		ContextTokens:   p.GetContextTokens(),
		Stop:            p.GetStop(),
		Seed:            p.GetSeed(),
		SeedSet:         p.GetSeedSet(),
	}
}

// usageProto returns nil when the runtime reported nothing. Absent is not
// zero: the engine records the first as billing "unknown" and the second
// as a measured zero.
//
// `model` is the served-model report design D9 asks of a model call, and
// `effort` beside it (memql#5393) is left EMPTY on purpose: neither Ollama
// nor an OpenAI-compatible response states an effort, so the only value
// that could go there is one taken from the request -- and the engine
// records this field as what SERVED.
func usageProto(u Usage) *memqlv1.ModelCallUsage {
	if !u.Known && u.Model == "" {
		return nil
	}
	return &memqlv1.ModelCallUsage{
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		Known:        u.Known,
		Model:        u.Model,
	}
}

func embeddingsProto(in [][]float32) []*memqlv1.ModelCallEmbedding {
	if len(in) == 0 {
		return nil
	}
	out := make([]*memqlv1.ModelCallEmbedding, 0, len(in))
	for _, v := range in {
		out = append(out, &memqlv1.ModelCallEmbedding{Values: v})
	}
	return out
}
