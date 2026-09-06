package work

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// recorder_test.go -- the RECORDING EXECUTOR every test in this package runs
// against.
//
// # Why it records the CONTEXT and not only the query
//
// A fake engine that matches query strings and returns canned rows proves that
// the handler composed the call it meant to. It proves NOTHING about the two
// rules this package exists to get right, and both of them live in the
// context:
//
//   - internal origin, without which every @serverOnly write is refused with
//     ONE WARN and the row is simply never written;
//   - the borrowed owner actor, without which an owned read answers zero rows
//     and an owned write is refused by the guard.
//
// Both failures are SILENT at the seam a query-matching fake watches. So every
// call is recorded with auth.OriginFromContext and the AccessContext's UserId
// beside it, and the assertions are about those.
//
// # The control comes from the loaded registry, not from this file
//
// A test that asserts "we stamped internal origin" is only meaningful if the
// construct being called actually REQUIRES it. serveronly_test.go takes that
// fact from the engine's own loaded function registry
// (eng.Functions().Get(name).ServerOnly) rather than restating it here, so the
// assertion is about something real: if a mutation stopped being @serverOnly,
// that test fails rather than quietly passing.

// recordedCall is one Execute the handler made.
type recordedCall struct {
	Query  string
	Origin auth.CallOrigin
	// Actor is the AccessContext's UserId, or "" when there is none.
	Actor string
	// Role is the AccessContext's role.
	Role string
}

// Construct returns the construct the call names: `mutation createWorkGoal`,
// `query workRunForOwner`.
func (c recordedCall) Construct() string { return firstConstruct(c.Query) }

// Name returns just the construct's name.
func (c recordedCall) Name() string {
	f := c.Construct()
	if i := strings.LastIndex(f, " "); i >= 0 {
		return f[i+1:]
	}
	return f
}

// Args decodes the call's named arguments. Deliberately a re-parse of the
// rendered text rather than a hook inside the composer: what reaches the
// engine is the STRING, and a test that inspected the map would not notice a
// composer that dropped a field on the way out.
func (c recordedCall) Args(t *testing.T) map[string]any {
	t.Helper()
	open := strings.Index(c.Query, "(")
	if open < 0 || !strings.HasSuffix(strings.TrimSpace(c.Query), ")") {
		t.Fatalf("call %q has no argument list", c.Query)
	}
	body := strings.TrimSpace(c.Query)[open+1 : len(strings.TrimSpace(c.Query))-1]
	out := map[string]any{}
	for _, part := range splitTopLevel(body) {
		colon := strings.Index(part, ":")
		if colon < 0 {
			continue
		}
		key := strings.TrimSpace(part[:colon])
		raw := strings.TrimSpace(part[colon+1:])
		var v any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatalf("argument %s of %q did not decode as JSON: %v (raw %s)", key, c.Name(), err, raw)
		}
		out[key] = v
	}
	return out
}

// splitTopLevel splits on commas outside strings, objects and arrays.
func splitTopLevel(s string) []string {
	var (
		parts   []string
		depth   int
		inStr   bool
		escaped bool
		cur     strings.Builder
	)
	for _, r := range s {
		switch {
		case escaped:
			escaped = false
		case r == '\\' && inStr:
			escaped = true
		case r == '"':
			inStr = !inStr
		case inStr:
		case r == '{' || r == '[':
			depth++
		case r == '}' || r == ']':
			depth--
		case r == ',' && depth == 0:
			parts = append(parts, strings.TrimSpace(cur.String()))
			cur.Reset()
			continue
		}
		cur.WriteRune(r)
	}
	if strings.TrimSpace(cur.String()) != "" {
		parts = append(parts, strings.TrimSpace(cur.String()))
	}
	return parts
}

// recordingEngine records every Execute with the context it arrived on.
type recordingEngine struct {
	mu    sync.Mutex
	calls []recordedCall
	// replies maps a construct NAME to the rows it answers with. An
	// unlisted name answers no rows, which is the honest default: a read
	// with nothing behind it returns nothing.
	replies map[string][]map[string]any
	// fail maps a construct NAME to an error it raises.
	fail map[string]error
}

func newRecordingEngine() *recordingEngine {
	return &recordingEngine{replies: map[string][]map[string]any{}, fail: map[string]error{}}
}

func (e *recordingEngine) Execute(ctx context.Context, query string) (*memqlengine.ExecuteResult, error) {
	call := recordedCall{Query: query, Origin: auth.OriginFromContext(ctx)}
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		call.Actor = ac.UserId
		call.Role = string(ac.Role)
	}
	e.mu.Lock()
	e.calls = append(e.calls, call)
	name := call.Name()
	rows := e.replies[name]
	err := e.fail[name]
	e.mu.Unlock()

	if err != nil {
		return nil, err
	}
	// A query carrying a shape returns `output` rows and NILS the bundle,
	// which is exactly the shape every work read has -- so the fake answers
	// in that shape rather than in a bundle a shaped read never produces.
	payload := make([]any, 0, len(rows))
	for _, r := range rows {
		payload = append(payload, r)
	}
	return memqlengine.NewResultWithOutput(payload), nil
}

func (e *recordingEngine) reply(name string, rows ...map[string]any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.replies[name] = rows
}

func (e *recordingEngine) refuse(name string, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.fail[name] = err
}

func (e *recordingEngine) recorded() []recordedCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]recordedCall, len(e.calls))
	copy(out, e.calls)
	return out
}

// callTo returns the single recorded call to the named construct, failing when
// there is not exactly one. "Exactly one" is the assertion worth making: a
// handler that wrote a row twice is as wrong as one that wrote it never.
func (e *recordingEngine) callTo(t *testing.T, name string) recordedCall {
	t.Helper()
	var found []recordedCall
	for _, c := range e.recorded() {
		if c.Name() == name {
			found = append(found, c)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly 1 call to %s, got %d (calls: %s)", name, len(found), e.summary())
	}
	return found[0]
}

func (e *recordingEngine) callsTo(name string) []recordedCall {
	var found []recordedCall
	for _, c := range e.recorded() {
		if c.Name() == name {
			found = append(found, c)
		}
	}
	return found
}

func (e *recordingEngine) summary() string {
	var names []string
	for _, c := range e.recorded() {
		names = append(names, c.Construct())
	}
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

var testNow = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

// newTestIntegration builds an Integration over a recording engine with a
// FIXED clock, so a rendered timestamp is an assertable value.
func newTestIntegration(t *testing.T) (*Integration, *recordingEngine) {
	t.Helper()
	eng := newRecordingEngine()
	i := New(eng, testLogger())
	i.SetNow(func() time.Time { return testNow })
	// Admit every row by default: these tests are about the actor and the
	// origin, and a gate that denied everything would make every sweep
	// assertion vacuous. sweep_test.go pins the REFUSAL case explicitly.
	i.admitRow = func(context.Context, memorynodes.MemoryNode) bool { return true }
	return i, eng
}

// callerContext is a signed-in person.
func callerContext(userId string) context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: userId,
		Role:   auth.RoleWriter,
	})
}

// clusterOwnerContext is what the maintenance principal and a hand-running
// owner both look like to a handler.
func clusterOwnerContext(userId string) context.Context {
	return auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId:    userId,
		Role:      auth.RoleOwner,
		Unranked:  true,
		Synthetic: true,
	})
}
