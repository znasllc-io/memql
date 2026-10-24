package worker

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"

	"github.com/znasllc-io/memql/cockpit/internal/worker/inference"
)

// modelpull.go is the ModelPullStart / ModelPullProgress / ModelPullEnd /
// ModelPullCancel arm of the worker stream -- the cockpit half of engine
// epic memql#5103, shipped under the install wizard's D13 (memql#5218).
//
// THE CLUSTER ASKS; THE MACHINE RUNS THE PATH IT ALREADY HAD. Nothing in
// this file knows how to pull a model. It runs, in order, exactly what
// `memql worker models --pull` runs from a terminal: inference.Pull against
// the Ollama the discoverer resolved, inference.Allow on this machine's
// policy.yaml, and a re-advertise request -- with one difference forced by
// where it runs. The CLI is a SECOND PROCESS and reaches the running
// worker through a SIGHUP, whose handler reloads policy.yaml and then asks
// for the re-advertise. This arm IS the running worker, so it reloads the
// policy itself; without that the inventory's next probe reads the allow
// list from before the pull, the reconnect re-registers the old labels,
// and a pull that succeeded reads to the person watching as one that did
// nothing.
//
// A PULL IS NOT A TOOL CALL. It is not counted in Runner.active, because
// that group is waited on before a reconnect and a pull is hours; it is
// not given a ModelCall concurrency slot, because it competes for
// bandwidth and disk rather than for the model runner. What it shares
// with both is the shape -- a start, a stream, an end, correlated by
// request id -- and the two exits every long-running arm on this stream
// has: the cluster cancels it by name, or the stream goes and StopAll
// cancels everything.
//
// THE END GOES OUT BEFORE THE RE-ADVERTISE IS REQUESTED, and a live pull
// counts as busy. Re-advertising means reconnecting (labels bind at
// Register), and a reconnect closes the stream the End rides. So the
// order is: End, a short settle for the transport to flush it, release,
// THEN the request -- and until the release the pull holds the busy
// guard, so a model landing cannot cut its sibling's download short
// either. The recommended set is a pair; the OS asks for both at once.

// ModelPullOptions wires what a cluster-driven pull needs from this
// machine. Every field is a seam so the arm is exercisable with no
// runtime, no policy.yaml and no network; a nil Pull or Allow is the real
// one (inference.Pull, inference.Allow), and the other three default to
// the permissive reading a build that wired nothing should get.
type ModelPullOptions struct {
	// PolicyPath is the policy.yaml Allow writes the model into.
	PolicyPath string
	// OllamaBase is where the runtime answers, resolved by the SAME
	// discoverer the inventory probes with. Never a second reading of
	// OLLAMA_HOST: two resolvers drift, and the failure is a pull that
	// lands where discovery will never look, so the model arrives and is
	// never advertised.
	OllamaBase func() string
	// PullAllowed reads models.pull from the LIVE policy. A function
	// rather than a value because the file is re-read on SIGHUP, and a
	// switch the owner turned off must refuse the next pull, not the one
	// after the next reconnect.
	PullAllowed func() bool
	// ReloadPolicy re-reads policy.yaml into the running worker after
	// Allow wrote it. See the file comment for why this arm cannot leave
	// that to a signal.
	ReloadPolicy func() error
	// Pull streams the model in. inference.Pull when nil.
	Pull func(ctx context.Context, base, model string, onProgress func(inference.Progress)) error
	// Allow merges the model into models.allow. inference.Allow when nil.
	Allow func(policyPath string, ids ...string) error
}

// pullSender is what the arm needs from the worker's connection. An
// interface for the reason modelcall.Sender is one: a test puts a
// recorder here and asserts the frames, on a machine with no cluster.
type pullSender interface {
	SendModelPullProgress(p *memqlv1.ModelPullProgress) error
	SendModelPullEnd(end *memqlv1.ModelPullEnd) error
}

// modelPullSettle is how long a finished pull waits between its End and
// the re-advertise request.
//
// The End is the last frame this pull puts on a stream the re-advertise is
// about to close, and gRPC hands a sent message to a writer goroutine
// rather than to the wire -- a Close that follows too closely can drop it.
// Half a second is far below the reconnect's own cost and far above any
// flush, and the alternative is the OS reporting "worker disconnected" for
// a pull that succeeded, which is the exact reading this whole arm exists
// to prevent.
const modelPullSettle = 500 * time.Millisecond

// modelPullUnsupported is the answer from a build with no local-model
// support. It names the command that DOES work on that machine, because
// the OS shows this sentence in surface and a person reading it is one
// step from a working pull.
const modelPullUnsupported = "this worker runs without local-model support, so it cannot pull a model for the cluster; run `memql worker models --pull <model>` on the machine instead"

// modelPuller owns the cluster-driven pulls in flight on this worker.
type modelPuller struct {
	logger *slog.Logger
	// opts is nil when this build pulls nothing, and every Start is then
	// refused in a sentence rather than dropped.
	opts *ModelPullOptions
	// readvertise is Runner.RequestImmediateReadvertise.
	readvertise func()
	settle      time.Duration

	mu   sync.Mutex
	live map[string]*modelPull
}

// modelPull is one pull in flight.
type modelPull struct {
	model  string
	cancel context.CancelFunc

	mu sync.Mutex
	// reason is the sentence an abort left for the End, or "" while the
	// pull is running. FIRST abort wins, as in modelcall: a cancel that
	// lands while StopAll is already tearing the pull down must not
	// rewrite the reason the cluster will be told.
	reason string
}

// newModelPuller builds the arm. A nil opts is a build that pulls nothing.
func newModelPuller(logger *slog.Logger, opts *ModelPullOptions, readvertise func()) *modelPuller {
	if logger == nil {
		logger = slog.Default()
	}
	if readvertise == nil {
		readvertise = func() {}
	}
	m := &modelPuller{
		logger:      logger,
		readvertise: readvertise,
		settle:      modelPullSettle,
		live:        make(map[string]*modelPull),
	}
	if opts == nil {
		return m
	}
	// A copy, so filling the defaults does not write into the caller's
	// struct.
	o := *opts
	if o.Pull == nil {
		o.Pull = inference.Pull
	}
	if o.Allow == nil {
		o.Allow = inference.Allow
	}
	if o.OllamaBase == nil {
		// Empty takes the same default the models package probes; see
		// inference.Pull.
		o.OllamaBase = func() string { return "" }
	}
	if o.PullAllowed == nil {
		// The reading tools.Policy gives a nil policy: a build that loaded
		// no policy has refused nothing.
		o.PullAllowed = func() bool { return true }
	}
	if o.ReloadPolicy == nil {
		o.ReloadPolicy = func() error { return nil }
	}
	m.opts = &o
	return m
}

// Live reports how many pulls are running. The runner reads it before
// spending a reconnect on a changed model set.
func (m *modelPuller) Live() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.live)
}

// Start admits and runs one pull. It returns at once; the pull reports
// itself on the stream.
//
// Every refusal is an End with ok=false and a SENTENCE, never a dropped
// message: the OS shows the text in surface, and a silent absence there
// is indistinguishable from a machine that is asleep.
func (m *modelPuller) Start(ctx context.Context, sender pullSender, start *memqlv1.ModelPullStart) {
	if start == nil || sender == nil {
		return
	}
	requestID := start.GetRequestId()
	model := strings.TrimSpace(start.GetModel())
	if requestID == "" {
		// Nothing to correlate a refusal with, so there is nothing to
		// send. Dropping it is the only honest option.
		m.log().Warn("model pull arrived with no request id; dropped", "model", model)
		return
	}
	if m == nil || m.opts == nil {
		m.refuse(sender, requestID, model, modelPullUnsupported)
		return
	}
	if model == "" {
		m.refuse(sender, requestID, model, "no model was named, so there is nothing to pull")
		return
	}
	if !m.opts.PullAllowed() {
		// The CLI's own sentence, so the two surfaces agree about why.
		m.refuse(sender, requestID, model, "this machine will not pull a model: models.pull is false in "+
			m.opts.PolicyPath+". Remove that key, or set it to true, and ask again.")
		return
	}

	// REGISTERED SYNCHRONOUSLY, for modelcall's reason: a Cancel arriving
	// right behind the Start has to find the pull. Everything slow
	// happens in the goroutine, because Start runs on the one goroutine
	// that reads every inbound message.
	pullCtx, cancel := context.WithCancel(ctx)
	p := &modelPull{model: model, cancel: cancel}
	m.mu.Lock()
	_, duplicate := m.live[requestID]
	if !duplicate {
		m.live[requestID] = p
	}
	m.mu.Unlock()
	if duplicate {
		cancel()
		m.refuse(sender, requestID, model, "a pull with request id "+requestID+" is already running on this worker")
		return
	}

	m.logger.Info("the cluster asked this machine to pull a model",
		"request_id", requestID,
		"model", model,
		"registration_id", start.GetRegistrationId(),
	)
	go m.run(pullCtx, sender, p, requestID)
}

// run is the pull, from the first byte to the End.
func (m *modelPuller) run(ctx context.Context, sender pullSender, p *modelPull, requestID string) {
	defer p.cancel()
	model := p.model

	var lastStatus string
	err := m.opts.Pull(ctx, m.opts.OllamaBase(), model, func(pr inference.Progress) {
		if pr.Status != lastStatus {
			// One line per phase at debug, never per byte count: a
			// four-gigabyte pull reports thousands of times.
			lastStatus = pr.Status
			m.logger.Debug("model pull progress", "request_id", requestID, "model", model, "status", pr.Status)
		}
		if err := sender.SendModelPullProgress(&memqlv1.ModelPullProgress{
			RequestId:      requestID,
			Model:          model,
			CompletedBytes: pr.Completed,
			TotalBytes:     pr.Total,
			Status:         pr.Status,
			Layer:          pullLayer(pr.Status),
		}); err != nil {
			// A progress frame that could not be sent is a stream that is
			// going away, and StopAll is about to cancel this pull by
			// name. The End says what happened; this does not need to.
			m.logger.Debug("model pull progress not sent", "request_id", requestID, "error", err)
		}
	})
	if err != nil {
		text := err.Error()
		if reason := p.abortReason(); reason != "" {
			// The cancel reason wins over the runtime's text. Pull reports
			// a cancelled context as "pulling X was cancelled: context
			// canceled", which is true and says nothing about who asked
			// or why -- and the End is what the person who asked reads.
			text = reason
		}
		m.logger.Info("model pull did not complete", "request_id", requestID, "model", model, "error", text)
		m.end(sender, requestID, model, false, text, false)
		m.release(requestID)
		return
	}

	// PULL, THEN ALLOW, in that order and never the reverse: allowing a
	// model puts it in front of the cluster's router, and a window in
	// which the machine advertises a model it cannot serve fails on
	// somebody else's prompt.
	if err := m.opts.Allow(m.opts.PolicyPath, model); err != nil {
		m.logger.Warn("model pulled but not allowed", "request_id", requestID, "model", model, "error", err)
		m.end(sender, requestID, model, false, fmt.Sprintf("the model was pulled and %v", err), false)
		m.release(requestID)
		return
	}
	if err := m.opts.ReloadPolicy(); err != nil {
		// Allowed on disk and not in the running worker: the next
		// reconnect would advertise the old list, so ok=true here would
		// be the pull that did nothing. The next attempt is a fast
		// no-op on the bytes and repairs this.
		m.logger.Warn("model pulled and allowed but the policy did not reload", "request_id", requestID, "model", model, "error", err)
		m.end(sender, requestID, model, false, fmt.Sprintf("the model was pulled and allowed in %s, and the running worker could not reload that file: %v",
			m.opts.PolicyPath, err), false)
		m.release(requestID)
		return
	}

	// readvertised=true is a statement about the REQUEST, which is armed
	// and survives the busy guard, not about a reconnect that has
	// already happened -- the reconnect is what closes this stream, so
	// it cannot be reported on it. See the file comment for the order.
	m.logger.Info("model pulled and allowed; re-advertising", "request_id", requestID, "model", model)
	m.end(sender, requestID, model, true, "", true)
	select {
	case <-time.After(m.settle):
	case <-ctx.Done():
	}
	m.release(requestID)
	m.readvertise()
}

// Cancel stops a running pull. A cancel for a pull this worker is not
// running is ignored rather than reported, for modelcall's reason: the
// engine cancels optimistically on its own timeout paths, and a refusal
// for a pull that already ended would be noise.
func (m *modelPuller) Cancel(c *memqlv1.ModelPullCancel) {
	if m == nil || c == nil {
		return
	}
	reason := strings.TrimSpace(c.GetReason())
	if reason == "" {
		reason = "no reason was given"
	}
	m.abort(c.GetRequestId(), "the cluster cancelled this pull: "+reason)
}

// StopAll ends every live pull. Called on disconnect and on drain.
//
// The stream is the only channel back to the caller, and the engine has
// already given every pull on it up (worker_disconnected). What is on the
// disk stays: a cancelled pull leaves its fetched blobs, and the next pull
// of the same model resumes them rather than starting over.
func (m *modelPuller) StopAll(reason string) {
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
		m.abort(id, "this pull was stopped: "+reason)
	}
}

func (m *modelPuller) abort(requestID, reason string) {
	m.mu.Lock()
	p := m.live[requestID]
	m.mu.Unlock()
	if p == nil {
		return
	}
	p.mu.Lock()
	if p.reason == "" {
		p.reason = reason
	}
	p.mu.Unlock()
	p.cancel()
}

func (p *modelPull) abortReason() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reason
}

func (m *modelPuller) release(requestID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.live, requestID)
}

// refuse answers a Start that will not run, in a sentence.
func (m *modelPuller) refuse(sender pullSender, requestID, model, sentence string) {
	m.end(sender, requestID, model, false, sentence, false)
}

func (m *modelPuller) end(sender pullSender, requestID, model string, ok bool, text string, readvertised bool) {
	if err := sender.SendModelPullEnd(&memqlv1.ModelPullEnd{
		RequestId:    requestID,
		Model:        model,
		Ok:           ok,
		Error:        text,
		Readvertised: readvertised,
	}); err != nil {
		m.log().Warn("failed to send model pull end", "request_id", requestID, "error", err)
	}
}

// log is the logger, safe on the nil receiver Start tolerates.
func (m *modelPuller) log() *slog.Logger {
	if m != nil && m.logger != nil {
		return m.logger
	}
	return slog.Default()
}

// pullLayer names the blob a status line is about, or "" when the line is
// not a download.
//
// Derived from the status because inference.Progress carries no digest of
// its own -- the status already names it ("pulling 8934d96d3f08") and a
// second spelling of one fact is one more thing for two surfaces to
// disagree about. The wire's `layer` field exists so a surface can key a
// bar per blob without parsing the line itself, which is why it is filled
// here rather than left for the OS to guess at.
func pullLayer(status string) string {
	if !isByteStatus(status) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(status, "pulling "))
}
