package memql

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// gateTestEngine boots a DB-less engine carrying the full embedded DSL tree.
// The coarse data-plane gate runs BEFORE the engine touches the database, so
// this is enough to drive the real handler path: a refused request never
// reaches the store, and an admitted request reaches it and fails there --
// which is exactly how the two outcomes are told apart below.
func gateTestEngine(t *testing.T) *memqlengine.MemQLEngine {
	t.Helper()
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := concept.DefaultRegistry()
	if registry == nil || len(registry.List()) == 0 {
		t.Fatal("concept registry empty after LoadUnifiedConcepts")
	}
	eng, err := memqlengine.New(nil)
	if err != nil {
		t.Fatalf("memqlengine.New(nil): %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(registry); err != nil {
		t.Fatalf("engine.Init: %v", err)
	}
	return eng
}

func gateTestSession(t *testing.T, eng *memqlengine.MemQLEngine, role auth.Role) (*streamSession, *captureStream) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	svc := &service{engine: eng, logger: logger}
	cs := newCaptureStream(t)
	cs.ctx = auth.ContextWithToken(context.Background(), &auth.TokenInfo{Subject: "v1:identity:user:gate-3179"})
	return &streamSession{
		service:      svc,
		stream:       cs,
		logger:       logger,
		access:       &auth.AccessContext{UserId: "v1:identity:user:gate-3179", Role: role},
		accessLoaded: true,
	}, cs
}

func driveGateQuery(t *testing.T, s *streamSession, requestId, query string) {
	t.Helper()
	env := &memqlv1.MemqlClientMessage{
		MessageId: requestId,
		Payload: &memqlv1.MemqlClientMessage_ExecuteQuery{
			ExecuteQuery: &memqlv1.ExecuteQueryMsg{RequestId: requestId, Query: query},
		},
	}
	if err := s.handleExecuteQuery(env, env.GetExecuteQuery()); err != nil {
		t.Fatalf("handleExecuteQuery(%q): %v", query, err)
	}
}

// queryErrorFor returns the QueryError the handler emitted for requestId, or
// nil if it emitted none (the request was admitted and is running).
func queryErrorFor(cs *captureStream, requestId string) *memqlv1.QueryError {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, m := range cs.sent {
		if qe := m.GetQueryError(); qe != nil && qe.GetRequestId() == requestId {
			return qe.GetError()
		}
	}
	return nil
}

// awaitQueryError waits for the outcome of an ADMITTED request. The gate
// refuses synchronously inside handleExecuteQuery; execution runs in a
// goroutine and, on the DB-less engine, always ends in an error. So a request
// that got past the gate produces an error too -- a DIFFERENT one -- and
// waiting for it is what makes "admitted" an assertion rather than the absence
// of one.
func awaitQueryError(t *testing.T, cs *captureStream, requestId string) *memqlv1.QueryError {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if qe := queryErrorFor(cs, requestId); qe != nil {
			return qe
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no outcome for request %q within 5s", requestId)
	return nil
}

// A real mutation the shipped DSL exposes (dsl/agents/mutations.memql).
const gateMutationQuery = `createAgent(agentId: "gate-3179", ownerUserId: "v1:identity:user:gate-3179", name: "GateTest")`

// A real query the shipped DSL exposes (dsl/agents/queries.memql).
const gateReadQuery = `agentById(agentId: "gate-3179")`

// TestExecuteQueryGate_ReaderRefusedMutation is AC 5: a reader is refused a
// mutation the DSL exposes, a writer is not. It drives handleExecuteQuery --
// the real request-path handler -- not the engine directly.
func TestExecuteQueryGate_ReaderRefusedMutation(t *testing.T) {
	eng := gateTestEngine(t)

	// Precondition, straight from the consolidated RBAC model.
	if auth.Capable(auth.RoleReader, auth.VerbCreate, auth.ResourceData) {
		t.Fatal("precondition: reader must not hold create-on-data")
	}
	if !auth.Capable(auth.RoleWriter, auth.VerbCreate, auth.ResourceData) {
		t.Fatal("precondition: writer must hold create-on-data")
	}

	t.Run("reader refused", func(t *testing.T) {
		s, cs := gateTestSession(t, eng, auth.RoleReader)
		driveGateQuery(t, s, "req-reader-write", gateMutationQuery)
		qe := queryErrorFor(cs, "req-reader-write")
		if qe == nil {
			t.Fatal("reader was admitted: no QueryError emitted for a mutation")
		}
		if qe.GetCode() != codes.PermissionDenied.String() {
			t.Fatalf("reader refusal code = %q, want %q (message: %s)",
				qe.GetCode(), codes.PermissionDenied.String(), qe.GetMessage())
		}
	})

	t.Run("writer admitted", func(t *testing.T) {
		s, cs := gateTestSession(t, eng, auth.RoleWriter)
		driveGateQuery(t, s, "req-writer-write", gateMutationQuery)
		qe := awaitQueryError(t, cs, "req-writer-write")
		if qe.GetCode() == codes.PermissionDenied.String() {
			t.Fatalf("writer refused a mutation: %s", qe.GetMessage())
		}
		// Positively confirm it reached EXECUTION, not just "wasn't denied".
		if !strings.Contains(strings.ToLower(qe.GetMessage()), "database") {
			t.Fatalf("writer's mutation did not reach the engine; outcome was %s: %s",
				qe.GetCode(), qe.GetMessage())
		}
	})

	t.Run("reader still reads", func(t *testing.T) {
		s, cs := gateTestSession(t, eng, auth.RoleReader)
		driveGateQuery(t, s, "req-reader-read", gateReadQuery)
		qe := awaitQueryError(t, cs, "req-reader-read")
		if qe.GetCode() == codes.PermissionDenied.String() {
			t.Fatalf("reader refused a READ; the gate is coarse, not a lockout: %s", qe.GetMessage())
		}
		if !strings.Contains(strings.ToLower(qe.GetMessage()), "database") {
			t.Fatalf("reader's query did not reach the engine; outcome was %s: %s",
				qe.GetCode(), qe.GetMessage())
		}
	})
}

// TestExecuteQueryGate_UnknownRoleIsReader pins the role-resolution rule: an
// absent or unrecognised role resolves to the least-privileged VALID role
// (reader) rather than to "no capabilities at all". Reads keep working, writes
// are refused. Matters because v1:identity:user.role carries a concept-level
// @default that is NOT applied on insert (memql#2960), so an empty role on a
// user row is reachable.
func TestExecuteQueryGate_UnknownRoleIsReader(t *testing.T) {
	eng := gateTestEngine(t)

	for _, role := range []auth.Role{"", "system", "nonsense"} {
		t.Run(string(role)+"/write refused", func(t *testing.T) {
			s, cs := gateTestSession(t, eng, role)
			driveGateQuery(t, s, "req-unknown-write", gateMutationQuery)
			qe := queryErrorFor(cs, "req-unknown-write")
			if qe == nil || qe.GetCode() != codes.PermissionDenied.String() {
				t.Fatalf("role %q was admitted to a mutation; want PermissionDenied (got %v)", role, qe)
			}
		})
		t.Run(string(role)+"/read admitted", func(t *testing.T) {
			s, cs := gateTestSession(t, eng, role)
			driveGateQuery(t, s, "req-unknown-read", gateReadQuery)
			if qe := awaitQueryError(t, cs, "req-unknown-read"); qe.GetCode() == codes.PermissionDenied.String() {
				t.Fatalf("role %q refused a READ: %s", role, qe.GetMessage())
			}
		})
	}
}

// TestExecuteQueryGate_HasExactlyOneCallSite is AC 3 + AC 4 evidence.
//
// The ruling put the check at the HANDLER layer precisely so there is no
// system bypass list to maintain: every non-user actor in the task's inventory
// (node bootstrap, the authoring promote/demote/rearm paths, the authored
// scheduler, the planner reactive loop, the MCP automation runner) reaches the
// engine IN-PROCESS via Engine.Execute and never sends an ExecuteQueryMsg. The
// assertion that keeps that true is this one: the gate has exactly one call
// site in the whole repository, and it is the gRPC ExecuteQuery handler.
func TestExecuteQueryGate_HasExactlyOneCallSite(t *testing.T) {
	root := repoRootForGateTest(t)

	const callee = "allowDataPlaneAccess("
	callers := map[string]int{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "bin", "gen":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, line := range strings.Split(string(body), "\n") {
			if !strings.Contains(line, callee) {
				continue
			}
			trimmed := strings.TrimSpace(line)
			// Skip the definition itself and doc comments naming it.
			if strings.HasPrefix(trimmed, "func ") || strings.HasPrefix(trimmed, "//") {
				continue
			}
			rel, _ := filepath.Rel(root, path)
			callers[rel]++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}

	want := map[string]int{"component/grpc/server.go": 1}
	if len(callers) != len(want) {
		t.Fatalf("gate call sites = %v, want exactly %v", callers, want)
	}
	for file, n := range want {
		if callers[file] != n {
			t.Fatalf("gate call sites = %v, want exactly %v", callers, want)
		}
	}
}

// TestNodeBootstrapIsNotOnTheGatedPath is the other half of the AC 3 proof:
// component/node -- the package holding the Engine.Execute call at
// bootstrap.go:105 -- does not import the handler package the gate lives in,
// so the gate is structurally unreachable from node bootstrap. Nothing is
// bypassed because nothing arrives.
func TestNodeBootstrapIsNotOnTheGatedPath(t *testing.T) {
	root := repoRootForGateTest(t)

	bootstrap := filepath.Join(root, "component", "node", "bootstrap.go")
	body, err := os.ReadFile(bootstrap)
	if err != nil {
		t.Fatalf("read %s: %v", bootstrap, err)
	}
	if !strings.Contains(string(body), "Engine.Execute(") {
		t.Fatal("component/node/bootstrap.go no longer calls Engine.Execute; " +
			"re-verify that node bootstrap is still off the handler path")
	}

	entries, err := os.ReadDir(filepath.Join(root, "component", "node"))
	if err != nil {
		t.Fatalf("read component/node: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		path := filepath.Join(root, "component", "node", e.Name())
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read %s: %v", path, readErr)
		}
		if strings.Contains(string(src), `"github.com/znasllc-io/memql/component/grpc"`) {
			t.Fatalf("%s imports component/grpc; node bootstrap is no longer "+
				"structurally off the gated handler path", e.Name())
		}
	}
}

func repoRootForGateTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 10; i++ {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate repo root (no go.mod above cwd)")
	return ""
}
