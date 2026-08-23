package identity

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// The audit log and the activity log are two destinations behind ONE writer
// (memql#4328). SlogAuditLogger.Log is the only function every identity-side
// event passes through, so the split is a branch there and nowhere else --
// which is what keeps a writer from having to know which log it is on beyond
// setting one field.

type recordingSink struct {
	events []AuditEvent
}

func (r *recordingSink) WriteAuditEvent(_ context.Context, ev AuditEvent) error {
	r.events = append(r.events, ev)
	return nil
}

func TestLogRoutesByStream(t *testing.T) {
	audit := &recordingSink{}
	activity := &recordingSink{}
	logger := &SlogAuditLogger{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		DB:       audit,
		Activity: activity,
	}

	// The DEFAULT is the audit log. Zero-valued Stream must not silently
	// route a decision onto the high-volume log -- every writer that predates
	// the split leaves the field unset.
	logger.Log(context.Background(), AuditEvent{Action: "session_created"})
	// An explicit activity event goes the other way.
	logger.Log(context.Background(), AuditEvent{Action: "session_refreshed", Stream: StreamActivity})
	// And the explicit audit spelling is the same as the default.
	logger.Log(context.Background(), AuditEvent{Action: "session_revoked", Stream: StreamAudit})

	if got := actionsOf(audit.events); len(got) != 2 ||
		got[0] != "session_created" || got[1] != "session_revoked" {
		t.Errorf("audit sink received %v, want [session_created session_revoked]", got)
	}
	if got := actionsOf(activity.events); len(got) != 1 || got[0] != "session_refreshed" {
		t.Errorf("activity sink received %v, want [session_refreshed]", got)
	}
}

// A node wired with no activity sink must not silently DROP the mechanics.
// Losing them costs the reuse-detection lookup its evidence, so the fallback
// is the audit log -- noisy, and visibly so, rather than absent.
func TestActivityFallsBackToTheAuditSinkWhenUnwired(t *testing.T) {
	audit := &recordingSink{}
	logger := &SlogAuditLogger{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), DB: audit}
	logger.Log(context.Background(), AuditEvent{Action: "session_refreshed", Stream: StreamActivity})
	if got := actionsOf(audit.events); len(got) != 1 || got[0] != "session_refreshed" {
		t.Errorf("with no activity sink the audit sink received %v, want the event to fall back", got)
	}
}

func actionsOf(evs []AuditEvent) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.Action)
	}
	return out
}

// The statement ActivitySink builds is run through the REAL front end, and
// every argument it passes is checked against what the mutation DECLARES.
//
// Both halves are needed, and the second is the one a fake-engine test cannot
// give: eng.Parse binds the construct name and stops, while validateFunctionArgs
// iterates the DECLARED fields -- so an argument the sink invents is silently
// discarded and the row simply never receives it. That is how revokeAuthSession
// came to be called with seven arguments against a two-argument declaration
// (memql#4258). component/grpc/render_query_args_parse_test.go is the same
// guard for the handlers.
func TestActivitySinkStatementParsesAndItsArgumentsAreDeclared(t *testing.T) {
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := memqlengine.New(nil)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(concept.DefaultRegistry()); err != nil {
		t.Fatalf("engine init: %v", err)
	}

	// The awkward shapes a real user agent and a real failure reason carry.
	ev := AuditEvent{
		OccurredAt:    time.Date(2026, 8, 23, 10, 11, 12, 0, time.UTC),
		Stream:        StreamActivity,
		Action:        "session_refresh_blocked",
		Outcome:       AuditOutcomeBlocked,
		ActorUserId:   "u-1",
		ActorEmail:    `o'brien+"tag"@example.com`,
		ActorRole:     "owner",
		ActorIdentity: "id-1",
		TargetId:      "sess-1",
		SourceIP:      "203.0.113.7",
		UserAgent:     "Mozilla/5.0 (X11; Linux x86_64) \"quoted\" \\ back\nslash",
		ClientLabel:   "Firefox on Linux",
		FailureReason: "previous_refresh_grace_expired",
		RetiredHash:   strings.Repeat("ab", 32),
		Detail:        map[string]any{"rotatedAgo": "31s"},
	}

	stmt := (&ActivitySink{}).statementFor(ev, "act-1")
	if _, err := eng.Parse(stmt); err != nil {
		t.Fatalf("the engine refused the rendered statement:\n  %s\n  --> %v", stmt, err)
	}

	fn, err := eng.Functions().Get("createAuthActivity")
	if err != nil || fn == nil {
		t.Fatalf("createAuthActivity is not in the function registry: %v", err)
	}
	declared := map[string]bool{}
	if fn.ArgsSchema != nil {
		for _, f := range fn.ArgsSchema.Fields {
			declared[f.Name] = true
		}
	}
	if len(declared) == 0 {
		t.Fatal("createAuthActivity declares no args block; this gate would check nothing")
	}
	for _, name := range argNamesIn(stmt) {
		if !declared[name] {
			t.Errorf("the sink passes %q, which createAuthActivity does not declare -- "+
				"unknown args are DISCARDED, not refused, so the row would silently lose it "+
				"(memql#3626)", name)
		}
	}

	// A positive control: the gate must be able to SEE the argument names it
	// is judging, or it passes over an empty list.
	if names := argNamesIn(stmt); len(names) < 10 {
		t.Fatalf("only %d argument name(s) extracted from %q -- the extractor is not reading the "+
			"statement this test is about", len(names), stmt)
	}
}

// argNamesIn pulls the `name:` keys out of a rendered `mutation f(a: 1, b: 2)`
// call, ignoring anything inside a string literal or a nested object/array.
func argNamesIn(stmt string) []string {
	open := strings.Index(stmt, "(")
	if open < 0 || !strings.HasSuffix(stmt, ")") {
		return nil
	}
	var out []string
	for _, part := range splitTopLevel(stmt[open+1:len(stmt)-1], ',') {
		if kv := splitTopLevel(part, ':'); len(kv) >= 2 {
			if name := strings.TrimSpace(kv[0]); name != "" {
				out = append(out, name)
			}
		}
	}
	return out
}

// splitTopLevel splits on sep, ignoring separators inside a string literal or
// inside braces/brackets.
func splitTopLevel(s string, sep byte) []string {
	var (
		out    []string
		cur    strings.Builder
		inStr  bool
		escape bool
		depth  int
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escape {
			escape = false
			cur.WriteByte(c)
			continue
		}
		if inStr {
			if c == '\\' {
				escape = true
			} else if c == '"' {
				inStr = false
			}
			cur.WriteByte(c)
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case sep:
			if depth == 0 {
				out = append(out, cur.String())
				cur.Reset()
				continue
			}
		}
		cur.WriteByte(c)
	}
	out = append(out, cur.String())
	return out
}

// BOTH HALVES OF THE ACTIVITY LOG ARE @serverOnly, AND THAT IS AN AUTHORIZATION
// GATE THE CALLER HAS TO PASS.
//
// auth.OriginFromContext DEFAULTS TO OriginClient, and component/memql/engine.go
// refuses a @serverOnly construct on any origin that is not internal. A writer
// that forgets to stamp internal origin gets no compile error, no parse error
// and no failing test -- it gets a refusal at execute, on every single call,
// leaving one WARN per rotation and an empty activity log.
//
// The argument is made in two halves, and both are needed:
//
//  1. THE CONTROL, read off the LOADED registry: these two constructs really
//     do carry @serverOnly, so internal origin really is required. Without it
//     the assertion below would be a statement about nothing -- and the whole
//     gate could be removed from the DSL with these tests still green.
//  2. THE ASSERTION: the production call sites hand Execute a context whose
//     origin is internal.
//
// Asserted rather than executed because the mutation path reaches a
// "database not configured" check before the origin gate, so a DB-less engine
// cannot tell an origin refusal from an unrelated failure -- and a test that
// cannot tell those apart is one that passes for the wrong reason.

// originRecordingEngine records the CallOrigin of every context it is handed.
// Not a stand-in for the engine's behaviour: what is under test is the context
// the production code builds, which is exactly what this observes.
type originRecordingEngine struct {
	origins []auth.CallOrigin
	actors  []string
}

func (e *originRecordingEngine) Execute(ctx context.Context, _ string) (*memqlengine.ExecuteResult, error) {
	e.origins = append(e.origins, auth.OriginFromContext(ctx))
	if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
		e.actors = append(e.actors, ac.UserId)
	} else {
		e.actors = append(e.actors, "")
	}
	return &memqlengine.ExecuteResult{}, nil
}

// loadedFunction resolves a construct from a real engine's registry.
func loadedFunction(t *testing.T, name string) *memqlengine.Function {
	t.Helper()
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := memqlengine.New(nil)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(concept.DefaultRegistry()); err != nil {
		t.Fatalf("engine init: %v", err)
	}
	fn, err := eng.Functions().Get(name)
	if err != nil || fn == nil {
		t.Fatalf("%s is not in the function registry: %v", name, err)
	}
	return fn
}

func TestActivityWritesCarryInternalOrigin(t *testing.T) {
	if fn := loadedFunction(t, "createAuthActivity"); !fn.ServerOnly {
		t.Fatal("createAuthActivity is no longer @serverOnly, so the origin requirement this " +
			"test asserts does not exist and the assertion below proves nothing. If dropping " +
			"the annotation was deliberate, delete this test; if it was not, restore it -- a " +
			"client-reachable createAuthActivity lets a caller forge retiredTokenHash, which " +
			"is the field reuse detection keys on")
	}

	rec := &originRecordingEngine{}
	sink := &ActivitySink{Engine: rec}
	if err := sink.WriteAuditEvent(context.Background(), AuditEvent{
		OccurredAt:  time.Date(2026, 8, 23, 10, 11, 12, 0, time.UTC),
		Stream:      StreamActivity,
		Action:      "session_refreshed",
		Outcome:     AuditOutcomeSuccess,
		ActorUserId: "v1:identity:user:u-1",
		TargetId:    "v1:identity:authSession:s-1",
		RetiredHash: strings.Repeat("cd", 32),
	}); err != nil {
		t.Fatalf("WriteAuditEvent: %v", err)
	}
	if len(rec.origins) != 1 {
		t.Fatalf("the sink made %d engine call(s), want 1", len(rec.origins))
	}
	if !rec.origins[0].IsInternal() {
		t.Errorf("ActivitySink executes createAuthActivity on origin %v, but the construct is "+
			"@serverOnly and the engine refuses any origin that is not internal. Every rotation "+
			"row would be lost, and with it the retiredTokenHash reuse detection keys on. "+
			"Stamp auth.ContextWithInternalOrigin.", rec.origins[0])
	}
	// And the borrowed actor is still there: the origin stamp must not
	// displace it, or createAuthActivity stamps an empty owner and the row
	// becomes unreadable by the person it is about.
	if rec.actors[0] != "v1:identity:user:u-1" {
		t.Errorf("the write ran under actor %q, want the session's user -- the row's owner is "+
			"stamped from actor.userId", rec.actors[0])
	}
}

// The same gate on the READ half. authActivityByRetiredHash is the reuse
// lookup; refused, it answers "no rotation ever retired this hash" for every
// hash -- reuse detection failing OPEN, silently, with a replay degrading to a
// stale cookie.
func TestRetiredHashLookupCarriesInternalOrigin(t *testing.T) {
	if fn := loadedFunction(t, "authActivityByRetiredHash"); !fn.ServerOnly {
		t.Fatal("authActivityByRetiredHash is no longer @serverOnly. Either the origin " +
			"requirement asserted below is gone, or a client can now ask whether an arbitrary " +
			"hash was ever retired -- an oracle over other people's tokens")
	}

	rec := &originRecordingEngine{}
	store := &Store{Engine: rec}
	if _, err := store.AuthActivityByRetiredHash(context.Background(), strings.Repeat("ef", 32)); err != nil {
		t.Fatalf("AuthActivityByRetiredHash: %v", err)
	}
	if len(rec.origins) != 1 {
		t.Fatalf("the lookup made %d engine call(s), want 1", len(rec.origins))
	}
	if !rec.origins[0].IsInternal() {
		t.Errorf("AuthActivityByRetiredHash executes on origin %v, but the query is @serverOnly. "+
			"It would answer 'no rotation retired this hash' for EVERY hash. Route it through "+
			"executeAndExtractInternal.", rec.origins[0])
	}
	// The composite row-authz tier refuses an actorless read outright, so the
	// cluster-owner actor is as load-bearing as the origin stamp.
	if rec.actors[0] == "" {
		t.Error("the lookup ran with no AccessContext; authActivity declares " +
			"@rowAuthz(owner=\"actorUserId\", clusterOwner) and an actorless read is REFUSED " +
			"(memql#3172 finding 4), so reuse detection would error on every call")
	}
}
