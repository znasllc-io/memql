package main

import (
	"sync"
	"time"

	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"

	"github.com/znasllc-io/memql/cmd/memql-lsp/internal/position"
	"github.com/znasllc-io/memql/component/memql/sense"
)

// diagnosticsDebounce is the quiet period after the last edit before diagnostics
// recompute, so a burst of keystrokes yields one Diagnose pass, not one per key.
const diagnosticsDebounce = 300 * time.Millisecond

// rebuildDebounce is the quiet period after the last save / watched-file change
// before the offline Sense registry rebuilds (an expensive full re-load).
const rebuildDebounce = 300 * time.Millisecond

// publishDiagnostics runs Sense's Diagnose over the document's current buffer and
// pushes the mapped diagnostics to the client, together with the refusal of the
// document's language line when the last build refused it (languageline.go). A
// closed/unknown document, or a nil Sense service, publishes nothing; a
// publish always carries the whole set, so one without a refusal clears the
// squiggle the previous one drew.
func (s *server) publishDiagnostics(notify glsp.NotifyFunc, uri protocol.DocumentUri) {
	// Held from reading the buffer to sending, so a close cannot slip between
	// the two and be overwritten by what was computed before it (didClose).
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	text, ok := s.docs.get(uri)
	if !ok {
		return
	}
	svc, lines := s.getBuild()
	if svc == nil {
		return
	}
	lspDiags := senseDiagnostics(svc, uri, text)
	// Beside Sense's own, never instead of them: a refused domain's files still
	// get their syntax and reference diagnostics.
	lspDiags = append(lspDiags, s.languageLineDiagnostics(uri, text, lines)...)
	notify(protocol.ServerTextDocumentPublishDiagnostics, protocol.PublishDiagnosticsParams{
		URI:         uri,
		Diagnostics: lspDiags,
	})
}

// senseDiagnostics is Sense's Diagnose over a document's buffer, in the LSP
// wire form.
func senseDiagnostics(svc *sense.Service, uri protocol.DocumentUri, text string) []protocol.Diagnostic {
	senseDiags := svc.Diagnose(text, uriToPath(uri))
	out := make([]protocol.Diagnostic, 0, len(senseDiags))
	for _, d := range senseDiags {
		out = append(out, toLSPDiagnostic(text, d))
	}
	return out
}

// toLSPDiagnostic maps a Sense diagnostic to the LSP wire form: Sense's 1-based
// rune positions convert to LSP 0-based UTF-16 positions, its 1-4 severity maps
// directly to the LSP DiagnosticSeverity 1-4, and its code is carried.
func toLSPDiagnostic(content string, d sense.Diagnostic) protocol.Diagnostic {
	severity := protocol.DiagnosticSeverity(d.Severity)
	out := protocol.Diagnostic{
		Range:    toLSPRange(content, d.Range),
		Severity: &severity,
		Message:  d.Message,
		Source:   ptr(lsName),
	}
	if d.Code != "" {
		out.Code = &protocol.IntegerOrString{Value: d.Code}
	}
	return out
}

// toLSPRange converts a Sense range (1-based rune) to an LSP range (0-based
// UTF-16), resolving columns against content.
func toLSPRange(content string, r sense.Range) protocol.Range {
	return protocol.Range{
		Start: position.ToLSPPosition(content, r.Start.Line, r.Start.Column),
		End:   position.ToLSPPosition(content, r.End.Line, r.End.Column),
	}
}

func ptr[T any](v T) *T { return &v }

// debounceTimer is the only thing diagnosticsDebouncer needs from a pending
// timer: an attempt to cancel it before it runs. *time.Timer satisfies it as
// declared, so the production path carries no wrapper.
type debounceTimer interface {
	// Stop cancels the timer, reporting whether the callback was still
	// pending. Crucially it reports false -- and has no effect -- once the
	// callback has already been released to run: nothing can un-fire an
	// already-fired timer. Fakes must honour that or they would let a test
	// assert a cancellation real time would never grant.
	Stop() bool
}

// debounceTimerFunc arms a one-shot timer that runs fn after delay. This is the
// seam tests replace (memql#3253): with a fake clock handing back timers the
// test fires explicitly, the coalescing assertions stop depending on how much
// wall time elapsed between two consecutive statements -- which on a loaded
// two-core CI runner could exceed the whole debounce window.
type debounceTimerFunc func(delay time.Duration, fn func()) debounceTimer

// realDebounceTimer is the production timer source: plain time.AfterFunc.
func realDebounceTimer(delay time.Duration, fn func()) debounceTimer {
	return time.AfterFunc(delay, fn)
}

// diagnosticsDebouncer coalesces per-document diagnostic recomputes. Each
// schedule cancels the document's pending timer and starts a new one; the timer
// callback runs on its own goroutine (via time.AfterFunc), which is safe because
// glsp's Notify writes to the persistent JSON-RPC connection.
type diagnosticsDebouncer struct {
	mu     sync.Mutex
	timers map[protocol.DocumentUri]debounceTimer
	delay  time.Duration
	// newTimer arms each pending timer. Always realDebounceTimer in
	// production; tests substitute a clock they drive by hand.
	newTimer debounceTimerFunc
}

func newDiagnosticsDebouncer(delay time.Duration) *diagnosticsDebouncer {
	return newDiagnosticsDebouncerWithTimers(delay, realDebounceTimer)
}

// newDiagnosticsDebouncerWithTimers is newDiagnosticsDebouncer with the timer
// source injected. Nothing but a test passes anything other than
// realDebounceTimer, and with that default the behaviour is identical.
func newDiagnosticsDebouncerWithTimers(delay time.Duration, newTimer debounceTimerFunc) *diagnosticsDebouncer {
	return &diagnosticsDebouncer{
		timers:   make(map[protocol.DocumentUri]debounceTimer),
		delay:    delay,
		newTimer: newTimer,
	}
}

func (d *diagnosticsDebouncer) schedule(uri protocol.DocumentUri, fn func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.timers[uri]; ok {
		t.Stop()
	}
	d.timers[uri] = d.newTimer(d.delay, fn)
}

func (d *diagnosticsDebouncer) cancel(uri protocol.DocumentUri) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.timers[uri]; ok {
		t.Stop()
		delete(d.timers, uri)
	}
}

func (d *diagnosticsDebouncer) stopAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for uri, t := range d.timers {
		t.Stop()
		delete(d.timers, uri)
	}
}

// rebuildDebouncer coalesces workspace-wide Sense rebuilds behind a single
// timer (the rebuild is not per-document).
type rebuildDebouncer struct {
	mu    sync.Mutex
	timer debounceTimer
	delay time.Duration
	// newTimer arms the pending timer: realDebounceTimer in production, a clock
	// a test fires by hand otherwise -- the seam diagnosticsDebouncer has, for
	// the same reason (memql#3253). A test driving a rebuild end to end then
	// runs it when it says so, rather than sleeping past the debounce.
	newTimer debounceTimerFunc
}

func newRebuildDebouncer(delay time.Duration) *rebuildDebouncer {
	return newRebuildDebouncerWithTimers(delay, realDebounceTimer)
}

// newRebuildDebouncerWithTimers is newRebuildDebouncer with the timer source
// injected. Nothing but a test passes anything other than realDebounceTimer.
func newRebuildDebouncerWithTimers(delay time.Duration, newTimer debounceTimerFunc) *rebuildDebouncer {
	return &rebuildDebouncer{delay: delay, newTimer: newTimer}
}

func (r *rebuildDebouncer) schedule(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timer != nil {
		r.timer.Stop()
	}
	r.timer = r.newTimer(r.delay, fn)
}

func (r *rebuildDebouncer) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.timer != nil {
		r.timer.Stop()
	}
}
