package worker

// render_parses_test.go -- every statement this package hands the engine, run
// through the REAL MemQL front end (no database).
//
// WHY IT EXISTS. All eight writes in this package rendered the legacy
// object-literal wrapper `name({...})`, which the parser has REFUSED since
// Story 9 of memql#2335 (memql#5004). So createWorkerRegistration,
// refreshWorkerRegistration, updateWorkerLastSeen, clearWorkerConnectedNode,
// revokeWorker, createWorkerInvocation, updateWorkerApps and all three
// app-session mutations failed at parse on every cluster -- pairing a machine,
// its heartbeat, its revocation, its per-call telemetry and every local-app
// session -- while this package's suite stayed green, because the rest of it
// drives a recording executor that records the string and parses NOTHING.
// That is the same defect as memql#4209 (component/deploycontrol) and
// memql#4256 (the guest-invitation and auth-session handlers); this is the
// third recurrence, which is why parser.RenderCall now exists.
//
// NO TABLE OF EXPECTED STRINGS, deliberately. This drives the REAL store
// methods against a recorder and parses whatever they actually produced, the
// way component/packages/render_parse_test.go does. A fixture holding the
// rendered text would keep passing after the renderer changed underneath it,
// which is the failure mode one layer up from the one being fixed.
//
// TWO LEVELS. Parsing proves the SYNTAX is a form the grammar accepts;
// resolving proves the construct exists and its arguments are declared. Syntax
// alone was green for the guest mutations while every one of their writes
// failed at execute (memql#4258), so both run here.

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// recordingExecutor captures every statement without executing it. It is the
// shape of fake this file exists to compensate for: on its own it asserts
// nothing about the text.
type recordingExecutor struct{ statements []string }

func (r *recordingExecutor) Execute(_ context.Context, query string) (*memqlengine.ExecuteResult, error) {
	r.statements = append(r.statements, query)
	return &memqlengine.ExecuteResult{}, nil
}

// realWorkerEngine loads the embedded DSL tree and initialises an engine with
// no database, which is all parse + resolve need.
//
// Built per test rather than once per package: a package-level fixture that
// mutates the concept registry leaks into every registry-walking test that
// runs after it.
func realWorkerEngine(t *testing.T) *memqlengine.MemQLEngine {
	t.Helper()
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts (the dsl/ tree): %v", err)
	}
	eng, err := memqlengine.New(nil)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(concept.DefaultRegistry()); err != nil {
		t.Fatalf("engine init: %v", err)
	}
	return eng
}

// awkwardText is what a machine name, a shell command or an error really looks
// like by the time it reaches a row: text this package does not author. Every
// character here has broken a hand-rolled renderer somewhere.
const awkwardText = "O'Brien's \"box\" <lab> & co \\ tab\there\nnewline é 🙂"

const testOwner = "v1:identity:user:owner-1"

// driver is one production write method, with the arguments a real caller
// would pass.
type driver struct {
	// method is the exported method name, matched against the Store and
	// AppSessionStore interfaces so a new write cannot go uncovered.
	method string
	run    func(context.Context, *EngineStore) error
}

func writeDrivers() []driver {
	at := time.Date(2026, 9, 6, 12, 34, 56, 789_000_000, time.UTC)
	reg := RegistrationRow{
		ID:          "v1:worker:registration:reg-1",
		OwnerUserId: testOwner,
		IdentityId:  "v1:identity:identity:wid-1",
		Name:        awkwardText,
		// A machine reports what it reports; none of this is our text.
		Capabilities: []string{"computer_use_headless", "computer_use_embodied"},
		Labels:       map[string]string{"os": "darwin", "note": awkwardText},
		Concurrency:  map[string]uint32{"exec": 4, "fetch": 2},
		Platform:     map[string]any{"arch": "arm64", "kernel": awkwardText},
		Permissions:  map[string]any{"shell": true, "fs": []any{"/tmp"}},
		Version:      "1.2.3",
		BuildTag:     "agent",
		Apps: []AppInfo{{
			Id: "claude-code", Version: "9.9.9", SignedIn: true,
			Subscription: "max", Allowed: true,
		}},
		RegisteredAt:        at,
		LastSeenAt:          at,
		LastConnectedFromIP: "203.0.113.7",
		ConnectedNodeId:     "agent-6b85d65bb6-kq6nf",
	}
	sess := AppSessionRow{
		ID:                  "v1:worker:appSession:sess-1",
		OwnerUserId:         testOwner,
		WorkerId:            reg.ID,
		App:                 "claude-code",
		Kind:                "open",
		PlanId:              "v1:planner:plan:p-1",
		TaskId:              "v1:planner:task:t-1",
		Status:              AppSessionStatusEnded,
		Workspace:           "/Users/x/dev/my repo",
		Prompt:              awkwardText,
		InputArtifactIds:    []string{"v1:library:artifact:a-1"},
		Transcript:          awkwardText,
		TranscriptBytes:     len(awkwardText),
		TranscriptTruncated: true,
		Usage:               AppSessionUsage{InputTokens: 1200, OutputTokens: 340, CostUSD: 0.0125, Known: true},
		Billing:             BillingSubscription,
		ExitCode:            0,
		ProducedArtifactIds: []string{"v1:library:artifact:a-2"},
		AppSessionRef:       "ref-1",
		CredentialRef:       "cred-1",
		CredentialExpiresAt: at.Add(8 * time.Hour),
		MCPEndpoint:         "https://mcp.example.test",
		ErrorMessage:        awkwardText,
		CancelReason:        "",
		StartedAt:           at,
		EndedAt:             at.Add(time.Minute),
	}

	return []driver{
		{"CreateRegistration", func(ctx context.Context, s *EngineStore) error {
			return s.CreateRegistration(ctx, reg)
		}},
		{"RefreshRegistration", func(ctx context.Context, s *EngineStore) error {
			return s.RefreshRegistration(ctx, reg)
		}},
		{"UpdateLastSeen", func(ctx context.Context, s *EngineStore) error {
			return s.UpdateLastSeen(ctx, reg.ID, testOwner, at, "203.0.113.7", "agent-1", 2)
		}},
		{"ClearConnectedNode", func(ctx context.Context, s *EngineStore) error {
			return s.ClearConnectedNode(ctx, reg.ID, testOwner)
		}},
		{"RevokeRegistration", func(ctx context.Context, s *EngineStore) error {
			return s.RevokeRegistration(ctx, reg.ID, testOwner, "v1:identity:user:admin-1", awkwardText, at)
		}},
		{"UpdateApps", func(ctx context.Context, s *EngineStore) error {
			return s.UpdateApps(ctx, reg.ID, testOwner, reg.Apps,
				map[string]string{"app:claude-code": "true", "note": awkwardText}, at, "203.0.113.7")
		}},
		{"CreateInvocation", func(ctx context.Context, s *EngineStore) error {
			return s.CreateInvocation(ctx, InvocationRow{
				ID: "v1:worker:invocation:inv-1", OwnerUserId: testOwner, WorkerId: reg.ID,
				AgentId: "v1:agents:agent:a-1", PlanId: "v1:planner:plan:p-1",
				TaskId: "v1:planner:task:t-1", CorrelationId: "corr-1",
				Tool: "workerHost", Action: "exec",
				ArgsRedacted:  map[string]any{"command": awkwardText, "redacted": true},
				StartedAt:     at,
				CompletedAt:   at.Add(2 * time.Second),
				DurationMs:    2000,
				Outcome:       "ok",
				ExitCode:      0,
				Signal:        "",
				ErrorCode:     "",
				ErrorMessage:  awkwardText,
				BytesIn:       12,
				BytesOut:      2048,
				OutputPreview: awkwardText,
				Routing: map[string]any{
					"strategy": "leastLoaded",
					"candidates": []any{
						map[string]any{"workerId": reg.ID, "score": 0.5},
					},
				},
			})
		}},
		{"CreateAppSession", func(ctx context.Context, s *EngineStore) error {
			return s.CreateAppSession(ctx, sess)
		}},
		{"AppendAppSessionTranscript", func(ctx context.Context, s *EngineStore) error {
			return s.AppendAppSessionTranscript(ctx, sess.ID, awkwardText, len(awkwardText), true, AppSessionStatusRunning)
		}},
		{"EndAppSession", func(ctx context.Context, s *EngineStore) error {
			return s.EndAppSession(ctx, sess)
		}},
	}
}

// renderedStatements drives every write method and returns what each one
// actually handed the engine.
func renderedStatements(t *testing.T) map[string][]string {
	t.Helper()
	out := make(map[string][]string, len(writeDrivers()))
	for _, d := range writeDrivers() {
		rec := &recordingExecutor{}
		store := &EngineStore{Engine: rec, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		if err := d.run(context.Background(), store); err != nil {
			t.Fatalf("%s: the method itself failed before anything could be parsed: %v", d.method, err)
		}
		if len(rec.statements) == 0 {
			t.Fatalf("%s recorded no statement -- it returned nil without reaching the engine, so this "+
				"test would pass having checked nothing", d.method)
		}
		out[d.method] = rec.statements
	}
	return out
}

// TestRenderedStatementsAreSyntacticallyValid is the direct regression for
// memql#5004: before the fix every one of these was `name({...})` and the
// parser refused it with "object-literal call args are removed".
func TestRenderedStatementsAreSyntacticallyValid(t *testing.T) {
	for method, stmts := range renderedStatements(t) {
		for _, stmt := range stmts {
			if _, err := langparser.ParseExpression(stmt); err != nil {
				t.Errorf("%s: the parser refused the statement it renders:\n  %s\n  --> %v", method, stmt, err)
			}
		}
	}
}

// TestRenderedStatementsResolve runs the same statements through the whole
// front end, so the construct name and its declared arguments are checked too
// -- not merely that the text is well-formed.
func TestRenderedStatementsResolve(t *testing.T) {
	eng := realWorkerEngine(t)
	checked := 0
	for method, stmts := range renderedStatements(t) {
		for _, stmt := range stmts {
			checked++
			if _, err := eng.Parse(stmt); err != nil {
				t.Errorf("%s: the engine refused the statement it renders:\n  %s\n  --> %v", method, stmt, err)
			}
		}
	}
	// A loop over an empty map passes. Assert it ran.
	if checked < len(writeDrivers()) {
		t.Fatalf("checked %d statements for %d write methods -- the drive loop skipped work", checked, len(writeDrivers()))
	}
}

// TestNoStatementUsesTheRetiredObjectLiteralForm names the defect in the
// output rather than only in the parser's verdict, so a failure says WHAT is
// wrong and not merely that something is.
//
// It is not redundant with the parse test: if the wrapper were ever re-accepted
// by the grammar, the parse tests would go quiet and this would not.
func TestNoStatementUsesTheRetiredObjectLiteralForm(t *testing.T) {
	for method, stmts := range renderedStatements(t) {
		for _, stmt := range stmts {
			if i := strings.Index(stmt, "("); i >= 0 && strings.HasPrefix(stmt[i+1:], "{") {
				t.Errorf("%s renders the retired object-literal wrapper `name({...})`, refused at parse "+
					"since memql#2335. Render it with parser.RenderCall:\n  %s", method, stmt)
			}
		}
	}
}

// TestEveryWriteMethodIsDriven is the anti-drift half.
//
// The audit that matters is "does every write go through the front end here",
// and a hand-kept list answers it only until the next method lands. This reads
// the Store and AppSessionStore interfaces and requires each method that is
// not a documented read to appear in writeDrivers -- so adding a write and
// forgetting this file is a red build with the method's name in it, which is
// exactly what did not happen for the eight in memql#5004.
func TestEveryWriteMethodIsDriven(t *testing.T) {
	// readOnly names the methods that ask rather than write. An entry here is
	// a claim that the method renders no mutation; adding one to dodge a
	// failure is the drift this test exists to stop.
	readOnly := map[string]string{
		"WorkerByIdentityId":  "reads a registration by identity id",
		"WorkersForUser":      "lists an owner's registrations",
		"IdentityByTokenHash": "resolves a presented mql_wkr_ token",
	}

	driven := make(map[string]bool, len(writeDrivers()))
	for _, d := range writeDrivers() {
		driven[d.method] = true
	}

	var missing []string
	for _, iface := range []reflect.Type{
		reflect.TypeOf((*Store)(nil)).Elem(),
		reflect.TypeOf((*AppSessionStore)(nil)).Elem(),
	} {
		for i := 0; i < iface.NumMethod(); i++ {
			name := iface.Method(i).Name
			if _, ok := readOnly[name]; ok {
				continue
			}
			if !driven[name] {
				missing = append(missing, iface.Name()+"."+name)
			}
		}
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("%s writes through the engine and no driver in writeDrivers() exercises it, so nothing "+
			"parses the statement it renders. Add a driver, or -- if it genuinely reads -- name it in "+
			"readOnly with what it asks.", m)
	}

	// The reverse: a readOnly entry for a method that no longer exists reads
	// as a considered exemption and is not one.
	for name := range readOnly {
		found := false
		for _, iface := range []reflect.Type{
			reflect.TypeOf((*Store)(nil)).Elem(),
			reflect.TypeOf((*AppSessionStore)(nil)).Elem(),
		} {
			if _, ok := iface.MethodByName(name); ok {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("readOnly names %q, which is on neither interface -- a stale exemption", name)
		}
	}
}
