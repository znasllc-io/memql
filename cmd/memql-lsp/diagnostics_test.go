package main

import (
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
	s.docs.open(clean, "@version(\"1.0.0\")\n@namespace(\"wp3\")\n@description(\"A widget.\")\nconcept widget {\n  label string @required @description(\"Label\")\n}\n")
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

func TestDiagnosticsDebouncer(t *testing.T) {
	d := newDiagnosticsDebouncer(10 * time.Millisecond)
	fired := make(chan struct{}, 10)

	// Two rapid schedules coalesce into one fire.
	d.schedule("u", func() { fired <- struct{}{} })
	d.schedule("u", func() { fired <- struct{}{} })
	select {
	case <-fired:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("debounced fn never fired")
	}
	select {
	case <-fired:
		t.Fatal("the first (superseded) schedule should have been cancelled")
	case <-time.After(60 * time.Millisecond):
	}

	// cancel stops a pending timer.
	d.schedule("v", func() { fired <- struct{}{} })
	d.cancel("v")
	select {
	case <-fired:
		t.Fatal("cancelled timer must not fire")
	case <-time.After(60 * time.Millisecond):
	}
}
