package main

import (
	"sync"
	"testing"
	"time"

	"github.com/tliron/commonlog"
	glsp "github.com/tliron/glsp"
	protocol "github.com/tliron/glsp/protocol_3_16"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/memql/sense"
)

// capturingNotify returns a NotifyFunc that records publishDiagnostics params.
func capturingNotify() (glsp.NotifyFunc, *[]protocol.PublishDiagnosticsParams) {
	var got []protocol.PublishDiagnosticsParams
	fn := func(method string, params any) {
		if method == protocol.ServerTextDocumentPublishDiagnostics {
			if p, ok := params.(protocol.PublishDiagnosticsParams); ok {
				got = append(got, p)
			}
		}
	}
	return fn, &got
}

func TestToLSPDiagnostic(t *testing.T) {
	content := "line one\nsecond line"
	d := sense.Diagnostic{
		Range:    sense.Range{Start: sense.Position{Line: 2, Column: 1}, End: sense.Position{Line: 2, Column: 7}},
		Severity: sense.SeverityWarning, // 2
		Message:  "beware",
		Code:     "unknown-thing",
	}
	got := toLSPDiagnostic(content, d)
	if got.Range.Start.Line != 1 || got.Range.Start.Character != 0 {
		t.Errorf("start = %+v; want (1,0)", got.Range.Start)
	}
	if got.Range.End.Character != 6 {
		t.Errorf("end char = %d; want 6", got.Range.End.Character)
	}
	if got.Severity == nil || *got.Severity != protocol.DiagnosticSeverity(2) {
		t.Errorf("severity = %v; want 2", got.Severity)
	}
	if got.Code == nil || got.Code.Value != "unknown-thing" {
		t.Errorf("code = %v; want unknown-thing", got.Code)
	}
	if got.Message != "beware" {
		t.Errorf("message = %q", got.Message)
	}
	if got.Source == nil || *got.Source != lsName {
		t.Error("source should be memql-lsp")
	}
}

func TestToLSPDiagnostic_NoCode(t *testing.T) {
	got := toLSPDiagnostic("x", sense.Diagnostic{Severity: sense.SeverityError, Message: "m"})
	if got.Code != nil {
		t.Errorf("empty code should stay nil, got %v", got.Code)
	}
}

func newTestServerWithSense(t *testing.T, svc *sense.Service) *server {
	t.Helper()
	commonlog.Configure(-4, nil)
	s := newServer(".", commonlog.GetLogger(lsName))
	s.setSense(svc)
	return s
}

// Integration: a clean document publishes zero diagnostics; a broken one
// publishes at least one. Uses the real offline Sense over the embedded tree.
func TestPublishDiagnostics_CleanAndBroken(t *testing.T) {
	svc, err := memql.BuildOfflineSense(nil)
	if err != nil {
		t.Fatalf("BuildOfflineSense: %v", err)
	}
	s := newTestServerWithSense(t, svc)
	notify, got := capturingNotify()

	const clean = "file:///clean.memql"
	s.docs.open(clean, "@namespace(\"wp3\")\n@description(\"A widget.\")\nconcept widget {\n  label string @required @description(\"Label\")\n}\n")
	s.publishDiagnostics(notify, clean)

	const broken = "file:///broken.memql"
	s.docs.open(broken, "logic oops {\n") // logic missing its mandatory body { } -> lowering error
	s.publishDiagnostics(notify, broken)

	if len(*got) != 2 {
		t.Fatalf("expected 2 publish notifications, got %d", len(*got))
	}
	if n := len((*got)[0].Diagnostics); n != 0 {
		t.Errorf("clean document produced %d diagnostics; want 0: %+v", n, (*got)[0].Diagnostics)
	}
	if n := len((*got)[1].Diagnostics); n == 0 {
		t.Error("broken document produced 0 diagnostics; want >= 1")
	}
}

func TestPublishDiagnostics_UnknownDocumentNoop(t *testing.T) {
	s := newTestServerWithSense(t, sense.New(nil))
	notify, got := capturingNotify()
	s.publishDiagnostics(notify, "file:///missing.memql")
	if len(*got) != 0 {
		t.Errorf("unknown document should publish nothing, got %d", len(*got))
	}
}

// fakeTimer is a debounceTimer the test fires by hand. It mirrors *time.Timer
// where it matters: Stop reports false and has no effect once the callback has
// already run, so the fake cannot grant a cancellation real time would refuse.
type fakeTimer struct {
	clock   *fakeClock
	fn      func()
	stopped bool
	fired   bool
}

func (t *fakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if t.stopped || t.fired {
		return false
	}
	t.stopped = true
	return true
}

// fakeClock hands the debouncer timers instead of wall-clock ones. Nothing in
// it reads the clock -- time "passes" only when the test calls fire. That is
// what removes the flake class from TestDiagnosticsDebouncer (memql#3253)
// rather than merely widening the window it used to race against.
type fakeClock struct {
	mu    sync.Mutex
	armed []*fakeTimer
}

// afterFunc is the debounceTimerFunc the debouncer is built with. The delay is
// ignored: the test, not the scheduler, decides when the callback runs.
func (c *fakeClock) afterFunc(_ time.Duration, fn func()) debounceTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{clock: c, fn: fn}
	c.armed = append(c.armed, t)
	return t
}

// fire runs every armed timer still live, in arming order, and reports how many
// callbacks ran. Stopped timers are dropped without running, and every armed
// timer is retired whether or not it ran -- a one-shot timer fires at most once.
func (c *fakeClock) fire() int {
	c.mu.Lock()
	armed := c.armed
	c.armed = nil
	live := make([]*fakeTimer, 0, len(armed))
	for _, t := range armed {
		if t.stopped {
			continue
		}
		t.fired = true
		live = append(live, t)
	}
	c.mu.Unlock()
	for _, t := range live {
		t.fn()
	}
	return len(live)
}

func TestDiagnosticsDebouncer(t *testing.T) {
	clock := &fakeClock{}
	d := newDiagnosticsDebouncerWithTimers(10*time.Millisecond, clock.afterFunc)

	fired := 0
	count := func() { fired++ }

	// Two rapid schedules coalesce into one fire: the second must stop the
	// first's timer, so firing the clock runs exactly one callback. No wall
	// time passes between the two schedules, so "rapid" is now a property of
	// the code under test rather than of how the OS happened to interleave two
	// consecutive statements against a 10ms window.
	d.schedule("u", count)
	d.schedule("u", count)
	if n := clock.fire(); n != 1 {
		t.Fatalf("live timers = %d; want 1 -- the first (superseded) schedule should have been cancelled", n)
	}
	if fired != 1 {
		t.Fatalf("callbacks run = %d; want 1", fired)
	}

	// cancel stops a pending timer: it never fires.
	fired = 0
	d.schedule("v", count)
	d.cancel("v")
	if n := clock.fire(); n != 0 {
		t.Fatalf("live timers after cancel = %d; want 0", n)
	}
	if fired != 0 {
		t.Fatalf("cancelled timer ran its callback %d time(s); want 0", fired)
	}

	// A schedule arriving after the pending timer has ALREADY fired does not
	// retro-actively cancel it -- Timer.Stop cannot un-fire a callback that has
	// already run. That is correct product behaviour (an edit a full debounce
	// interval after the previous one earns its own Diagnose pass), and it is
	// precisely the interleaving that made the old wall-clock test flaky: on a
	// loaded runner its two schedule statements straddled the debounce window,
	// the first callback had already run, and the "was it cancelled?" assertion
	// saw an extra fire. Asserted here deliberately, not raced for.
	fired = 0
	d.schedule("w", count)
	if n := clock.fire(); n != 1 {
		t.Fatalf("live timers = %d; want 1 for the first schedule", n)
	}
	d.schedule("w", count)
	if n := clock.fire(); n != 1 {
		t.Fatalf("live timers = %d; want 1 for the re-schedule", n)
	}
	if fired != 2 {
		t.Fatalf("callbacks run = %d; want 2 -- an already-fired timer cannot be un-fired", fired)
	}
}
