package main

// loadpass.go -- Lower's load refusals on the open document (memql#5434).
//
// Sense's Diagnose finds what lexing, parsing and rewriting find, in a few
// milliseconds. What the load's lowering refuses -- a field the concept does
// not declare, a context spec applied to the row, a read that needs `.?` --
// takes the engine's load of the document's constructs, 10-100 ms on the
// tree's largest files. So the two publish separately, and neither waits on
// the other:
//
//   - the debounced publish (publishDiagnostics) sends Diagnose's diagnostics
//     at once, with the last load result for the document carried across the
//     edit (carryAcrossEdit), and then starts the load;
//   - the load runs off the request path, one at a time per document; when it
//     finishes, and the buffer still holds the text it loaded, the document
//     is published again with both. A buffer that moved on is left to the pass
//     its own edit starts.
//
// Carrying the last result is what keeps a refusal from blinking off and on at
// every pause in typing: a refusal on a line the edit did not touch keeps its
// place (or moves with the lines after it) until the next result replaces it,
// and one on an edited line is dropped rather than drawn in the wrong place.
//
// The load reads the document's text and its path RELATIVE TO THE DSL ROOT,
// which is what the engine derives namespaces from: without it a name two
// domains declare resolves wrongly, so a document outside the tree gets no
// pass (treePath). Unsaved text is what is loaded -- the buffer, not the file.

import (
	"strings"
	"sync"

	"github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/memql/sense"
)

// loadResult is one finished load of a document: the text it loaded and the
// refusals it found, in Sense's coordinates.
type loadResult struct {
	text  string
	diags []sense.Diagnostic
}

// loadPasses holds each open document's last load result and which documents
// have a load running. Safe for concurrent use.
type loadPasses struct {
	mu      sync.Mutex
	results map[protocol.DocumentUri]loadResult
	running map[protocol.DocumentUri]bool
	again   map[protocol.DocumentUri]bool
	// start runs a pass off the caller's goroutine. A test replaces it to run
	// the pass where it can wait for it.
	start func(fn func())
}

func newLoadPasses() *loadPasses {
	return &loadPasses{
		results: map[protocol.DocumentUri]loadResult{},
		running: map[protocol.DocumentUri]bool{},
		again:   map[protocol.DocumentUri]bool{},
		start:   func(fn func()) { go fn() },
	}
}

// begin claims the document's pass. False when one is already running: that
// one is told to run again when it finishes, over whatever the buffer then
// holds, so a burst of edits costs at most one pass in flight and one queued.
func (l *loadPasses) begin(uri protocol.DocumentUri) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running[uri] {
		l.again[uri] = true
		return false
	}
	l.running[uri] = true
	return true
}

// finish releases the document's pass, reporting whether it must run again.
func (l *loadPasses) finish(uri protocol.DocumentUri) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.again[uri] {
		delete(l.again, uri)
		return true
	}
	delete(l.running, uri)
	return false
}

// store records a finished load, reporting whether it changes what the
// document shows -- a pass that found nothing where the last found nothing
// has no reason to publish again.
func (l *loadPasses) store(uri protocol.DocumentUri, r loadResult) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	prev, had := l.results[uri]
	l.results[uri] = r
	return len(r.diags) > 0 || (had && len(prev.diags) > 0)
}

// carried is the document's last load result, placed on text (carryAcrossEdit).
func (l *loadPasses) carried(uri protocol.DocumentUri, text string) []sense.Diagnostic {
	l.mu.Lock()
	r, ok := l.results[uri]
	l.mu.Unlock()
	if !ok || len(r.diags) == 0 {
		return nil
	}
	if r.text == text {
		return r.diags
	}
	return carryAcrossEdit(r.text, text, r.diags)
}

// forget drops a closed document's result.
func (l *loadPasses) forget(uri protocol.DocumentUri) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.results, uri)
}

// carryAcrossEdit places diagnostics computed for old on the text new: one
// wholly within the lines the two share at the top keeps its place, one wholly
// within the lines they share at the bottom moves with them, and one on or
// across a changed line is dropped. Columns are untouched, because the lines
// they are on are.
func carryAcrossEdit(old, new string, diags []sense.Diagnostic) []sense.Diagnostic {
	oldLines := strings.Split(old, "\n")
	newLines := strings.Split(new, "\n")
	shortest := min(len(oldLines), len(newLines))
	head := 0
	for head < shortest && oldLines[head] == newLines[head] {
		head++
	}
	tail := 0
	for tail < shortest-head && oldLines[len(oldLines)-1-tail] == newLines[len(newLines)-1-tail] {
		tail++
	}
	firstTail := len(oldLines) - tail + 1 // the first 1-based old line of the shared tail
	shift := len(newLines) - len(oldLines)

	var out []sense.Diagnostic
	for _, d := range diags {
		switch {
		case d.Range.End.Line <= head:
			out = append(out, d)
		case tail > 0 && d.Range.Start.Line >= firstTail:
			d.Range.Start.Line += shift
			d.Range.End.Line += shift
			out = append(out, d)
		}
	}
	return out
}

// treePath is the document's path relative to the DSL root -- the path the
// engine derives namespaces from -- or ok=false for a document that is not a
// file under that root.
func (s *server) treePath(uri protocol.DocumentUri, lines memql.WorkspaceLanguageLines) (string, bool) {
	rel, ok := s.workspacePath(uri)
	if !ok {
		return "", false
	}
	// The lines were resolved from the tree the domains sit in, which is
	// lines.Root below the workspace root.
	if lines.Root != "" {
		if rel, ok = strings.CutPrefix(rel, lines.Root+"/"); !ok {
			return "", false
		}
	}
	return rel, true
}

// scheduleLoadPass starts the load of a document's buffer, unless the service
// cannot load or the document is not in the tree -- then there is nothing to
// add to what Diagnose published.
func (s *server) scheduleLoadPass(notify glsp.NotifyFunc, uri protocol.DocumentUri) {
	svc, lines := s.getBuild()
	if !svc.CanLoad() {
		return
	}
	if _, placed := s.treePath(uri, lines); !placed {
		return
	}
	if !s.loads.begin(uri) {
		return
	}
	s.loads.start(func() { s.runLoadPass(notify, uri) })
}

// runLoadPass loads the document's buffer and, while the buffer still holds
// that text, publishes it again with the refusals. It repeats while edits
// arrived during a pass.
func (s *server) runLoadPass(notify glsp.NotifyFunc, uri protocol.DocumentUri) {
	for {
		s.loadOnce(notify, uri)
		if !s.loads.finish(uri) {
			return
		}
	}
}

// loadOnce is one pass over the buffer as it is now.
func (s *server) loadOnce(notify glsp.NotifyFunc, uri protocol.DocumentUri) {
	text, open := s.docs.get(uri)
	if !open {
		return
	}
	svc, lines := s.getBuild()
	path, placed := s.treePath(uri, lines)
	if svc == nil || !placed {
		return
	}
	// A rebuild swaps process-global DSL state the load reads (the mounted
	// tree); the build holds this lock for writing while it does.
	s.buildMu.RLock()
	diags := svc.DiagnoseLoad(text, path)
	s.buildMu.RUnlock()

	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	if current, open := s.docs.get(uri); !open || current != text {
		return // the buffer moved on; its own edit schedules the next pass
	}
	if s.loads.store(uri, loadResult{text: text, diags: diags}) {
		s.publishLocked(notify, uri, text)
	}
}
