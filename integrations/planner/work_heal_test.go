package planner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/healing"
)

type fakeProposer struct {
	patches []healing.Patch
	err     error
	calls   int
}

func (f *fakeProposer) Propose(_ context.Context, _ healing.PreconditionMiss, _ map[string]any) ([]healing.Patch, error) {
	f.calls++
	return f.patches, f.err
}

type recordingWriter struct{ calls []string }

func (r *recordingWriter) Execute(_ context.Context, q string) (any, error) {
	r.calls = append(r.calls, q)
	return nil, nil
}

func missEvent() events.Event {
	return events.Event{
		Topic: events.TopicPreconditionMissed,
		Payload: map[string]any{
			"automationName":          "nightlyExport",
			"executionId":             "run-1",
			"preconditionId":          "toolPresent",
			"check":                   "fileExists",
			"literal":                 "/opt/exporter",
			"preconditionDescription": "the exporter binary is installed",
		},
	}
}

// healerArgs parses the NAMED-ARGS invocation form `name(k: v, k2: v2)`.
// Splitting at TOP-LEVEL commas and colons is what makes these assertions
// about the real wire form rather than about a shape the parser rejects
// (#2335, Story 9): a helper that parsed `name({...})` would keep passing
// against a renderer that writes nothing.
func healerArgs(t *testing.T, call string) map[string]any {
	t.Helper()
	open := strings.Index(call, "(")
	if open < 0 || !strings.HasSuffix(call, ")") {
		t.Fatalf("call %q is not name(k: v, ...)", call)
	}
	out := map[string]any{}
	for _, seg := range splitTopLevel(call[open+1:len(call)-1], ',') {
		kv := splitTopLevel(seg, ':')
		if len(kv) < 2 {
			continue
		}
		key := strings.TrimSpace(kv[0])
		raw := strings.TrimSpace(strings.Join(kv[1:], ":"))
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatalf("argument %q of %q is not a JSON value: %v", key, call, err)
		}
		out[key] = v
	}
	return out
}

// splitTopLevel splits on sep, ignoring separators inside strings, objects
// and arrays.
func splitTopLevel(s string, sep byte) []string {
	var parts []string
	depth, inStr, esc, start := 0, false, false, 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case inStr && c == '\\':
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
		case c == '{' || c == '[':
			depth++
		case c == '}' || c == ']':
			depth--
		case c == sep && depth == 0:
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}

func TestWorkHealer_CarriesPatchesToAPlanReviewApproval(t *testing.T) {
	prop := &fakeProposer{patches: []healing.Patch{
		{Kind: healing.PatchRelativizeLiteral, Target: "steps.run.input.path", Replacement: "$config.exporterPath", Reason: "the path differs per machine"},
		{Kind: healing.PatchAddPrecondition, Precondition: &healing.PatchPrecondition{ID: "toolPresent", Check: "fileExists"}},
	}}
	w := &recordingWriter{}
	h := NewWorkHealer(prop, w, nil, time.Hour)

	h.HandlePreconditionMissed(missEvent())

	if prop.calls != 1 {
		t.Fatalf("Propose called %d times, want 1", prop.calls)
	}
	if len(w.calls) != 1 {
		t.Fatalf("wrote %d calls (%v), want exactly one createWorkApproval", len(w.calls), w.calls)
	}
	if !strings.HasPrefix(w.calls[0], "createWorkApproval(") {
		t.Fatalf("wrote %q", w.calls[0])
	}
	args := healerArgs(t, w.calls[0])
	if args["kind"] != "planReview" {
		t.Errorf("kind = %v, want planReview (D5: never a silent edit)", args["kind"])
	}
	if args["runId"] != "run-1" {
		t.Errorf("runId = %v", args["runId"])
	}
	if args["artifactHash"] == nil || args["artifactHash"] == "" {
		t.Error("an approval must hash what is being approved, or resume cannot tell the patches changed")
	}
	subject, _ := args["subject"].(map[string]any)
	patches, _ := subject["patches"].([]any)
	if len(patches) != 2 {
		t.Fatalf("the person must see the PATCHES, not a count: %+v", subject)
	}
	first, _ := patches[0].(map[string]any)
	if first["kind"] != "relativize-literal" || first["replacement"] != "$config.exporterPath" {
		t.Errorf("patch detail lost: %+v", first)
	}
	ev, _ := args["evidence"].(map[string]any)
	if ev["tier"] != "environment" || ev["ruleId"] != "environment.literal" {
		t.Errorf("evidence = %+v", ev)
	}
}

// D5, stated as a test: the healer proposes and writes ONE approval. It
// never writes the construct.
func TestWorkHealer_NeverEditsTheConstruct(t *testing.T) {
	w := &recordingWriter{}
	h := NewWorkHealer(&fakeProposer{patches: []healing.Patch{{Kind: healing.PatchInsertGuard, Target: "steps.run", Guard: "x == 1"}}}, w, nil, time.Hour)
	h.HandlePreconditionMissed(missEvent())
	for _, c := range w.calls {
		if !strings.HasPrefix(c, "createWorkApproval(") {
			t.Fatalf("the heal arm wrote %q -- D5 forbids a silent edit, even to the run's own draft template", c)
		}
	}
}

func TestWorkHealer_NoPatchesWritesNothingAndIsNotAnError(t *testing.T) {
	w := &recordingWriter{}
	h := NewWorkHealer(&fakeProposer{}, w, nil, time.Hour)
	h.HandlePreconditionMissed(missEvent())
	if len(w.calls) != 0 {
		t.Fatalf("an empty proposal must not raise an empty approval: %v", w.calls)
	}
}

func TestWorkHealer_RefusesAMissWithNoRunToPark(t *testing.T) {
	w := &recordingWriter{}
	h := NewWorkHealer(&fakeProposer{patches: []healing.Patch{{Kind: healing.PatchInsertGuard, Target: "s", Guard: "g"}}}, w, nil, time.Hour)
	ev := missEvent()
	delete(ev.Payload, "executionId")
	h.HandlePreconditionMissed(ev)
	if len(w.calls) != 0 {
		t.Fatal("an approval with no run is a question nobody can answer; it must be refused rather than orphaned")
	}
}

func TestWorkHealer_ProposalFailureWritesNothing(t *testing.T) {
	w := &recordingWriter{}
	h := NewWorkHealer(&fakeProposer{err: errors.New("provider down")}, w, nil, time.Hour)
	h.HandlePreconditionMissed(missEvent())
	if len(w.calls) != 0 {
		t.Fatalf("a failed proposal must not raise an approval carrying no patches: %v", w.calls)
	}
}

func TestNewWorkHealer_NilSeamsYieldANoOpHealer(t *testing.T) {
	if NewWorkHealer(nil, &recordingWriter{}, nil, 0) != nil {
		t.Error("no proposer means no healer")
	}
	if NewWorkHealer(&fakeProposer{}, nil, nil, 0) != nil {
		t.Error("no writer means no healer")
	}
	var h *WorkHealer
	h.HandlePreconditionMissed(missEvent()) // must not panic
}

// The hash covers the whole subject, so an approval for one precondition
// cannot be replayed against another that wants the same edits.
func TestWorkHealer_HashIsScopedToTheMiss(t *testing.T) {
	patches := []healing.Patch{{Kind: healing.PatchInsertGuard, Target: "steps.run", Guard: "g"}}
	hashFor := func(preconditionId string) string {
		w := &recordingWriter{}
		h := NewWorkHealer(&fakeProposer{patches: patches}, w, nil, time.Hour)
		ev := missEvent()
		ev.Payload["preconditionId"] = preconditionId
		h.HandlePreconditionMissed(ev)
		if len(w.calls) != 1 {
			t.Fatalf("expected one approval, got %v", w.calls)
		}
		s, _ := healerArgs(t, w.calls[0])["artifactHash"].(string)
		return s
	}
	if hashFor("toolPresent") == hashFor("networkUp") {
		t.Fatal("the same patch set against a DIFFERENT precondition must hash differently, or one approval covers both")
	}
}

// assertNamedArgsForm is the regression guard for the defect class that
// cost epic A1 a full round of db-gated tests: `name({...})` is REJECTED
// by the parser (component/language/parser/parser.go, #2335 Story 9), and
// nothing in Go notices -- a recording fake accepts any string, and the
// journal logs a Warn and carries on rather than failing the run, so a
// package stays green with not one row written.
//
// The parser's own entry point for this is unexported, so the assertion
// is structural: the rendered call must be `name(k: v, ...)` and must
// never contain the object-literal wrapper.
func assertNamedArgsForm(t *testing.T, call string) {
	t.Helper()
	open := strings.Index(call, "(")
	if open < 0 || !strings.HasSuffix(call, ")") {
		t.Fatalf("call %q is not an invocation", call)
	}
	if strings.HasPrefix(strings.TrimSpace(call[open+1:]), "{") {
		t.Fatalf("call %q uses the REMOVED object-literal wrapper name({...}); the parser refuses it and the write is a silent no-op", call)
	}
	body := strings.TrimSpace(call[open+1 : len(call)-1])
	if body == "" {
		return
	}
	for _, seg := range splitTopLevel(body, ',') {
		kv := splitTopLevel(seg, ':')
		if len(kv) < 2 || strings.TrimSpace(kv[0]) == "" {
			t.Fatalf("call %q has an argument that is not `k: v`: %q", call, seg)
		}
		if strings.HasPrefix(strings.TrimSpace(kv[0]), `"`) {
			t.Fatalf("call %q quotes its argument NAME; named args are bare identifiers", call)
		}
	}
}

func TestWorkHealer_RendersTheNamedArgsForm(t *testing.T) {
	w := &recordingWriter{}
	h := NewWorkHealer(&fakeProposer{patches: []healing.Patch{{Kind: healing.PatchInsertGuard, Target: "steps.run", Guard: "g"}}}, w, nil, time.Hour)
	h.HandlePreconditionMissed(missEvent())
	if len(w.calls) != 1 {
		t.Fatalf("expected one call, got %v", w.calls)
	}
	assertNamedArgsForm(t, w.calls[0])
}

// A nil argument must be DROPPED, not rendered as `null`: an optional
// object field given null fails the concept's type check and the engine
// refuses the WHOLE row, so the approval never lands at all.
func TestWorkHealer_DropsNilArgumentsRatherThanRenderingNull(t *testing.T) {
	w := &recordingWriter{}
	h := NewWorkHealer(&fakeProposer{patches: []healing.Patch{{Kind: healing.PatchInsertGuard, Target: "steps.run", Guard: "g"}}}, w, nil, 0)
	ev := missEvent()
	delete(ev.Payload, "preconditionDescription")
	delete(ev.Payload, "literal")
	h.HandlePreconditionMissed(ev)
	if len(w.calls) != 1 {
		t.Fatalf("expected one call, got %v", w.calls)
	}
	if strings.Contains(w.calls[0], ": null") {
		t.Fatalf("call renders a null argument, which refuses the whole insert: %s", w.calls[0])
	}
	assertNamedArgsForm(t, w.calls[0])
}

func TestDropNil(t *testing.T) {
	got := dropNil(map[string]any{"a": 1, "b": nil, "c": "", "d": "x", "e": map[string]any{}})
	if _, present := got["b"]; present {
		t.Error("a nil value must be dropped")
	}
	if _, present := got["c"]; present {
		t.Error("an empty string is dropped for these optional fields")
	}
	if got["a"] != 1 || got["d"] != "x" {
		t.Errorf("real values must survive: %+v", got)
	}
	if _, present := got["e"]; !present {
		t.Error("an EMPTY object is a real value and must survive -- it is not absent")
	}
}
