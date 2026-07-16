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

// publishDiagnostics runs Sense's Diagnose over the document's current buffer and
// pushes the mapped diagnostics to the client. A closed/unknown document, or a
// nil Sense service, publishes an empty set (which also clears stale squiggles).
func (s *server) publishDiagnostics(notify glsp.NotifyFunc, uri protocol.DocumentUri) {
	text, ok := s.docs.get(uri)
	if !ok {
		return
	}
	svc := s.getSense()
	if svc == nil {
		return
	}
	senseDiags := svc.Diagnose(text, uriToPath(uri))
	lspDiags := make([]protocol.Diagnostic, 0, len(senseDiags))
	for _, d := range senseDiags {
		lspDiags = append(lspDiags, toLSPDiagnostic(text, d))
	}
	notify(protocol.ServerTextDocumentPublishDiagnostics, protocol.PublishDiagnosticsParams{
		URI:         uri,
		Diagnostics: lspDiags,
	})
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

// diagnosticsDebouncer coalesces per-document diagnostic recomputes. Each
// schedule cancels the document's pending timer and starts a new one; the timer
// callback runs on its own goroutine (via time.AfterFunc), which is safe because
// glsp's Notify writes to the persistent JSON-RPC connection.
type diagnosticsDebouncer struct {
	mu     sync.Mutex
	timers map[protocol.DocumentUri]*time.Timer
	delay  time.Duration
}

func newDiagnosticsDebouncer(delay time.Duration) *diagnosticsDebouncer {
	return &diagnosticsDebouncer{
		timers: make(map[protocol.DocumentUri]*time.Timer),
		delay:  delay,
	}
}

func (d *diagnosticsDebouncer) schedule(uri protocol.DocumentUri, fn func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.timers[uri]; ok {
		t.Stop()
	}
	d.timers[uri] = time.AfterFunc(d.delay, fn)
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
