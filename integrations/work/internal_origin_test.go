package work

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestEveryWorkWriteStampsInternalOrigin is the gate on RULE 1 (store.go's
// header).
//
// Every mutation in dsl/work/mutations.memql is @serverOnly. Origin defaults
// to CLIENT, and the function validator refuses a @serverOnly construct on any
// other origin with ONE WARN and carries on -- so an unstamped write inserts
// nothing and reports nothing. This drives every capability that writes with a
// plain CLIENT-origin caller context and asserts the origin the engine
// actually SAW, which is the only place that fact is observable.
func TestEveryWorkWriteStampsInternalOrigin(t *testing.T) {
	writeConstructs := map[string]bool{
		"createWorkGoal":     true,
		"createWorkRun":      true,
		"updateWorkRun":      true,
		"createWorkApproval": true,
		"decideWorkApproval": true,
	}

	cases := []struct {
		name string
		run  func(t *testing.T, i *Integration, eng *recordingEngine, ctx context.Context)
	}{
		{
			name: "createGoal",
			run: func(t *testing.T, i *Integration, eng *recordingEngine, ctx context.Context) {
				if _, err := i.handleCreateGoal(ctx, map[string]any{"statement": "ship the thing"}, 0); err != nil {
					t.Fatalf("createGoal: %v", err)
				}
			},
		},
		{
			name: "cancelGoal",
			run: func(t *testing.T, i *Integration, eng *recordingEngine, ctx context.Context) {
				eng.reply("workGoalForOwner", map[string]any{"id": "v1:work:goal:g1", "ownerUserId": "u1"})
				eng.reply("workRunsForGoal", map[string]any{"id": "v1:work:run:r1", "status": runStatusRunning})
				if _, err := i.handleCancelGoal(ctx, map[string]any{"goalId": "v1:work:goal:g1"}, 0); err != nil {
					t.Fatalf("cancelGoal: %v", err)
				}
			},
		},
		{
			name: "forkRun",
			run: func(t *testing.T, i *Integration, eng *recordingEngine, ctx context.Context) {
				eng.reply("workRunForOwner", map[string]any{
					"id": "v1:work:run:r1", "ownerUserId": "u1", "goalId": "v1:work:goal:g1",
					"automationName": "a", "templateFingerprint": "fp",
				})
				if _, err := i.handleForkRun(ctx, map[string]any{"runId": "v1:work:run:r1", "atStepKey": "s2"}, 0); err != nil {
					t.Fatalf("forkRun: %v", err)
				}
			},
		},
		{
			name: "replayRun",
			run: func(t *testing.T, i *Integration, eng *recordingEngine, ctx context.Context) {
				eng.reply("workRunForOwner", map[string]any{
					"id": "v1:work:run:r1", "ownerUserId": "u1", "goalId": "v1:work:goal:g1",
					"automationName": "a", "templateFingerprint": "fp",
				})
				if _, err := i.handleReplayRun(ctx, map[string]any{"runId": "v1:work:run:r1"}, 0); err != nil {
					t.Fatalf("replayRun: %v", err)
				}
			},
		},
		{
			name: "decideApproval",
			run: func(t *testing.T, i *Integration, eng *recordingEngine, ctx context.Context) {
				subject := map[string]any{"patches": []any{}}
				eng.reply("workApprovalsForOwner", map[string]any{
					"id": "v1:work:approval:a1", "runId": "v1:work:run:r1", "ownerUserId": "u1",
					"kind": "planReview", "subject": subject, "artifactHash": artifactHashOf(subject),
				})
				eng.reply("workRunForOwner", map[string]any{
					"id": "v1:work:run:r1", "ownerUserId": "u1", "status": runStatusWaiting,
					"waitingOn": map[string]any{"kind": "approval", "subject": "v1:work:approval:a1"},
				})
				if _, err := i.handleDecideApproval(ctx, map[string]any{
					"approvalId": "v1:work:approval:a1", "decision": "approved",
				}, 0); err != nil {
					t.Fatalf("decideApproval: %v", err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i, eng := newTestIntegration(t)
			// A PLAIN caller context: no origin stamped, which is exactly
			// what a browser's stream carries. If the handler does not stamp,
			// the engine sees OriginClient and the write is refused.
			ctx := callerContext("u1")
			if origin := auth.OriginFromContext(ctx); origin.IsInternal() {
				t.Fatalf("the caller context already carried internal origin, so this test would pass without the handler doing anything")
			}
			tc.run(t, i, eng, ctx)

			wrote := 0
			for _, call := range eng.recorded() {
				name := call.Name()
				if !writeConstructs[name] {
					continue
				}
				wrote++
				if !call.Origin.IsInternal() {
					t.Errorf("%s reached the engine with origin %v; every work mutation is @serverOnly and would be refused with a WARN and no row",
						call.Construct(), call.Origin)
				}
			}
			if wrote == 0 {
				t.Fatalf("%s made no write at all, so this case asserts nothing (calls: %s)", tc.name, eng.summary())
			}
		})
	}
}

// TestOwnedReadsAreNotStampedWithInternalOrigin is the converse, and it
// matters as much.
//
// The READ gate has no internal-origin bypass, so a stamp buys a read nothing
// -- but it DOES open every @serverOnly read to whatever context inherits it.
// Every caller-scoped read here must therefore reach the engine on the
// caller's own unstamped context, which is what keeps row admission the
// owned tier's decision rather than the engine's.
func TestOwnedReadsAreNotStampedWithInternalOrigin(t *testing.T) {
	ownerScopedReads := map[string]bool{
		"workGoalForOwner":      true,
		"workRunsForGoal":       true,
		"workRunForOwner":       true,
		"workApprovalsForOwner": true,
	}

	i, eng := newTestIntegration(t)
	ctx := callerContext("u1")
	eng.reply("workGoalForOwner", map[string]any{"id": "v1:work:goal:g1", "ownerUserId": "u1"})
	eng.reply("workRunsForGoal", map[string]any{"id": "v1:work:run:r1", "status": runStatusRunning})
	if _, err := i.handleCancelGoal(ctx, map[string]any{"goalId": "v1:work:goal:g1"}, 0); err != nil {
		t.Fatalf("cancelGoal: %v", err)
	}

	read := 0
	for _, call := range eng.recorded() {
		if !ownerScopedReads[call.Name()] {
			continue
		}
		read++
		if call.Origin.IsInternal() {
			t.Errorf("%s was stamped with internal origin; a caller-scoped read must run under the caller's own context so the owned tier decides the scope", call.Construct())
		}
		if call.Actor != "u1" {
			t.Errorf("%s ran under actor %q, want the caller u1", call.Construct(), call.Actor)
		}
	}
	if read == 0 {
		t.Fatalf("no owner-scoped read was made, so this test asserts nothing (calls: %s)", eng.summary())
	}
}

// TestTheStampNeverEscapesItsCall counts the stamp sites in this package's
// non-test source.
//
// One site, applied INLINE as the argument to the one Execute that needs it.
// A second site is not an error in itself -- what it costs is the guarantee
// that a marked context cannot be returned and inherited by a later frame,
// which is the memql#2879 / memql#2989 escalation, and this is what makes
// adding one a deliberate act.
func TestTheStampNeverEscapesItsCall(t *testing.T) {
	files := goFilesInPackage(t)
	sites := 0
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for lineNo, line := range strings.Split(string(src), "\n") {
			if !strings.Contains(line, "auth.ContextWithInternalOrigin(") {
				continue
			}
			// A comment mentioning the rule is prose about it, not a call.
			if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "//") {
				continue
			}
			sites++
			if !strings.Contains(line, "Execute(auth.ContextWithInternalOrigin(") {
				t.Errorf("%s:%d stamps internal origin somewhere other than inline as an Execute argument:\n\t%s\nA stamped context that is stored or returned is inherited by later frames and opens every other @serverOnly construct in the tree.",
					filepath.Base(path), lineNo+1, strings.TrimSpace(line))
			}
		}
	}
	if sites != 1 {
		t.Errorf("expected exactly 1 internal-origin stamp site in integrations/work, found %d -- one seam is what keeps the escalation reasoning true of the whole package", sites)
	}
}

// TestNoMethodReturnsAContext is the structural half of the same rule: if
// nothing here hands a context back, no later frame can inherit the mark.
func TestNoMethodReturnsAContext(t *testing.T) {
	for _, path := range goFilesInPackage(t) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for lineNo, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "func ") {
				continue
			}
			// ownerActor is the one function that returns a context, and it
			// returns the CALLER's actor -- never internal origin. It is
			// named here so a NEW context-returning function is a visible
			// addition rather than a silent one.
			if strings.HasPrefix(trimmed, "func ownerActor(") {
				continue
			}
			if strings.Contains(trimmed, ") context.Context {") || strings.Contains(trimmed, "(context.Context, error)") {
				t.Errorf("%s:%d returns a context:\n\t%s\nA context this package builds must die inside the call it was built for.",
					filepath.Base(path), lineNo+1, trimmed)
			}
		}
	}
}

// TestCapabilitiesRefuseAnUnauthenticatedCaller pins the handler-side floor.
//
// A builtin's annotation set carries no @requiresRank, so the floor is the
// handler's or there is none. Every caller-scoped capability must refuse an
// absent, anonymous or connector actor BEFORE it touches the engine -- and
// "before" is the assertion, because a refusal after a read has already
// happened is a read that happened.
func TestCapabilitiesRefuseAnUnauthenticatedCaller(t *testing.T) {
	cases := []struct {
		name string
		call func(i *Integration, ctx context.Context) error
	}{
		{"createGoal", func(i *Integration, ctx context.Context) error {
			_, err := i.handleCreateGoal(ctx, map[string]any{"statement": "x"}, 0)
			return err
		}},
		{"cancelGoal", func(i *Integration, ctx context.Context) error {
			_, err := i.handleCancelGoal(ctx, map[string]any{"goalId": "g"}, 0)
			return err
		}},
		{"forkRun", func(i *Integration, ctx context.Context) error {
			_, err := i.handleForkRun(ctx, map[string]any{"runId": "r", "atStepKey": "s"}, 0)
			return err
		}},
		{"replayRun", func(i *Integration, ctx context.Context) error {
			_, err := i.handleReplayRun(ctx, map[string]any{"runId": "r"}, 0)
			return err
		}},
		{"decideApproval", func(i *Integration, ctx context.Context) error {
			_, err := i.handleDecideApproval(ctx, map[string]any{"approvalId": "a", "decision": "approved"}, 0)
			return err
		}},
		{"sweepWaiting", func(i *Integration, ctx context.Context) error {
			_, err := i.handleSweepWaiting(ctx, nil, 0)
			return err
		}},
		{"retentionSweep", func(i *Integration, ctx context.Context) error {
			_, err := i.handleRetentionSweep(ctx, nil, 0)
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i, eng := newTestIntegration(t)
			if err := tc.call(i, context.Background()); err == nil {
				t.Fatalf("%s admitted a caller with no access context", tc.name)
			}
			if got := eng.summary(); got != "(none)" {
				t.Errorf("%s reached the engine before refusing: %s", tc.name, got)
			}
		})
	}
}

// TestSweepsAreClusterOwnerFloored: both sweeps read every owner's rows by
// construction, so a writer must not reach them. The maintenance principal
// (RoleOwner) must.
func TestSweepsAreClusterOwnerFloored(t *testing.T) {
	i, _ := newTestIntegration(t)
	if err := requireClusterOwner(callerContext("u1")); err == nil {
		t.Error("a plain writer cleared the sweep floor")
	}
	if err := requireClusterOwner(clusterOwnerContext("system:maintenance:sweepWaitingWorkRuns")); err != nil {
		t.Errorf("the cluster's maintenance principal did not clear the sweep floor: %v", err)
	}
	// And the handler refuses rather than merely logging.
	if _, err := i.handleSweepWaiting(callerContext("u1"), nil, 0); err == nil {
		t.Error("sweepWaiting admitted a plain writer")
	}
	if _, err := i.handleRetentionSweep(callerContext("u1"), nil, 0); err == nil {
		t.Error("retentionSweep admitted a plain writer")
	}
}

// goFilesInPackage lists this package's non-test .go files.
func goFilesInPackage(t *testing.T) []string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate the package directory")
	}
	dir := filepath.Dir(self)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	if len(out) == 0 {
		t.Fatal("found no source files, so the scan asserts nothing")
	}
	return out
}
