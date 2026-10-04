package steps

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/automations"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
	"google.golang.org/protobuf/types/known/structpb"
)

// Regression suite for memql#1706: first-class event-context binding.
//
// Every event-trigger Logic reads the triggering event through its declared
// `event` input (e.g. `args.event.payload.partitionId`). The LogicRunner must
// thread that event into the per-step argument-resolution scope so the
// references resolve to their event-derived values inside the NESTED steps --
// not to empty/undefined (the #1706 failure shape, where the first nested
// step that feeds an `args.event.*` reference to a @required argument fails
// with "required argument <x> is missing").
//
// This test fires each affected Logic (loaded from the real embedded DSL
// tree) through the real LogicRunner with a representative triggering event,
// captures the FULLY-RESOLVED query/args every nested step would send to the
// engine, and asserts:
//
//  1. the event-derived values appear in the resolved nested-step args
//     (the event threaded all the way into step scope), and
//  2. no unresolved `args.event.payload.` reference TEXT leaks through (no
//     dropped-to-nil / literal-passthrough).
//
// It is DB-free: the LogicRunner dispatches each construct call through the
// step registry, so a capturing registry renders each engine call exactly as
// the real function executor does (ResolveV1Map + renderV1CallArgs) without
// ever calling engine.Execute. An expression statement never reaches it: the
// sequence runner evaluates it in process.

// capturingRegistry renders each step's outbound call exactly like the real
// executors and records it, returning a canned two-row result so
// result-dependent guards (`.empty()`, `.count() == 2`) let the downstream
// event-referencing steps run and be captured too.
type capturingRegistry struct{ resolved []string }

func twoRowResult() *memql.ExecuteResult {
	var nodes []*memqlv1.MemoryNode
	for i := 0; i < 2; i++ {
		p, _ := structpb.NewStruct(map[string]any{"name": fmt.Sprintf("row%d", i)})
		nodes = append(nodes, &memqlv1.MemoryNode{Id: fmt.Sprintf("v1:test:row:%d", i), Payload: p})
	}
	return &memql.ExecuteResult{Bundle: &memqlv1.GraphBundle{Nodes: nodes}}
}

func (r *capturingRegistry) Execute(ctx context.Context, step *automations.Step, sc *automations.StepContext) (*automations.StepResult, error) {
	var q string
	switch {
	case step.Function != nil:
		resolved, err := sc.Evaluator.ResolveV1Map(ctx, step.Function.Args)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", step.ID, err)
		}
		q = step.Function.Name + "(" + renderV1CallArgs(resolved) + ")"
	default:
		q = fmt.Sprintf("<%s/%s>", step.Type, step.ID)
	}
	r.resolved = append(r.resolved, q)
	return &automations.StepResult{StepId: step.ID, Status: "success", Result: twoRowResult()}, nil
}

// bootEmbeddedEngine boots a PRIVATE db-less engine over the embedded tree,
// for a test that changes it: installs a logic runner, an event bus, a
// database getter or an integration, or registers a function. A test that only
// reads it -- resolves a function, runs a logic through a runner of its own --
// borrows sharedEmbeddedEngine instead (memql#5668).
func bootEmbeddedEngine(t *testing.T) *memql.MemQLEngine {
	t.Helper()
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := memql.New(nil)
	if err != nil {
		t.Fatalf("memql.New: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(concept.DefaultRegistry()); err != nil {
		t.Fatalf("engine.Init: %v", err)
	}
	return eng
}

// THE READ-ONLY HALF (memql#5668), component/memql's memql#4569 applied here.
//
// The sandbox tests boot an engine per subtest only to look up how a function
// is classified: eleven boots at about 0.4s each, more than any other file
// costs in a package the db-tests lane gives 180s. They and the event-threading test read
// the engine and leave it as they found it, so they share ONE, booted by
// whichever of them runs first.
//
// WHY A PACKAGE-LEVEL ONCE IS SAFE HERE, when component/memql learned that one
// can leak a mounted fixture into every test that walks memorynodes.All():
// that leak needs the engine to boot while a fixture is mounted and outlive
// the fixture's restore, and no test in this package mounts one -- none calls
// dsl.RegisterTree, MountOverlayDomains or memorynodes.ReplaceAll. That is
// checked rather than assumed: the boot refuses when the tree or the concept
// registry differs from a pristine one, and every borrow re-checks both.
var sharedEmbeddedEngineState struct {
	once sync.Once
	// boots counts Once-body executions, so
	// TestSharedEmbeddedEngine_SharesOneBoot asserts the sharing as a NUMBER
	// rather than inferring it from wall time.
	boots int
	eng   *memql.MemQLEngine
	// domains and concepts are the global state the engine booted against; a
	// borrow that reads different ones is running beside a mounted fixture.
	domains, concepts []string
	// bootErr is recorded, never reported, inside the Once: every borrower
	// reports it, so the first one's t cannot answer for the package.
	bootErr error
}

// pristineDomains is the DSL tree's top-level domains as this package starts:
// the embedded ones plus whatever an imported package registered from init().
// A package-level variable is initialized after every import's init() and
// before TestMain, so no test has mounted anything yet.
var pristineDomains = treeDomains()

// treeDomains lists the top-level domains of the tree every loader walks,
// sorted.
func treeDomains() []string {
	entries, err := fs.ReadDir(memqldsl.Tree(), ".")
	if err != nil {
		return []string{"unreadable: " + err.Error()}
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// conceptNames lists the process-wide concept registry, sorted.
func conceptNames() []string {
	names := make([]string, 0, 512)
	for name := range concept.All() {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// drift names what now holds and was did not (+name), and the reverse
// (-name); "" when the two agree.
func drift(was, now []string) string {
	delta := map[string]int{}
	for _, n := range was {
		delta[n]--
	}
	for _, n := range now {
		delta[n]++
	}
	var out []string
	for n, d := range delta {
		switch {
		case d > 0:
			out = append(out, "+"+n)
		case d < 0:
			out = append(out, "-"+n)
		}
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

// sharedEmbeddedEngine returns the package's shared db-less engine, booting it
// on first use. Borrow it only to read; bootEmbeddedEngine is for a test that
// changes the engine.
func sharedEmbeddedEngine(t *testing.T) *memql.MemQLEngine {
	t.Helper()
	s := &sharedEmbeddedEngineState
	s.once.Do(func() {
		s.boots++
		if d := drift(pristineDomains, treeDomains()); d != "" {
			s.bootErr = fmt.Errorf("the DSL tree's domains differ from the package's start (%s): a fixture is "+
				"mounted, and the shared engine would load it and keep it after the fixture's test restores the "+
				"tree. A test that mounts a fixture boots its own engine", d)
			return
		}
		n, err := memql.LoadUnifiedConcepts(nil)
		if err != nil {
			s.bootErr = fmt.Errorf("LoadUnifiedConcepts: %w", err)
			return
		}
		if held := len(concept.All()); held != n {
			s.bootErr = fmt.Errorf("the concept registry holds %d concepts and the tree declares %d: an earlier "+
				"fixture's concepts outlived its test, and the shared engine would boot against them", held, n)
			return
		}
		eng, err := memql.New(nil)
		if err != nil {
			s.bootErr = fmt.Errorf("memql.New: %w", err)
			return
		}
		eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
		if err := eng.Init(concept.DefaultRegistry()); err != nil {
			s.bootErr = fmt.Errorf("engine.Init: %w", err)
			return
		}
		s.eng, s.domains, s.concepts = eng, treeDomains(), conceptNames()
	})
	if s.bootErr != nil {
		t.Fatalf("the shared embedded engine did not boot (recorded on first use, reported per borrower): %v", s.bootErr)
	}
	if d, c := drift(s.domains, treeDomains()), drift(s.concepts, conceptNames()); d != "" || c != "" {
		t.Fatalf("this test borrows the shared embedded engine beside a mounted fixture -- a t.Parallel test, or a "+
			"mount that did not restore in its t.Cleanup. Since the engine booted, the DSL domains drifted by [%s] and "+
			"the registered concepts by [%s]", d, c)
	}
	return s.eng
}

// TestSharedEmbeddedEngine_SharesOneBoot pins the mechanism memql#5668 buys
// its time with: three borrows hand back one engine, and the process booted it
// exactly once, whichever borrower ran first.
func TestSharedEmbeddedEngine_SharesOneBoot(t *testing.T) {
	first := sharedEmbeddedEngine(t)
	for i := 0; i < 2; i++ {
		if eng := sharedEmbeddedEngine(t); eng != first {
			t.Fatalf("borrow %d returned a different engine: the borrowers are not sharing one", i+2)
		}
	}
	if got := sharedEmbeddedEngineState.boots; got != 1 {
		t.Fatalf("the shared embedded engine booted %d times in this process, want exactly 1: each boot is a full "+
			"LoadUnifiedConcepts + New + Init, and one per borrower is the cost memql#5668 removed", got)
	}
}

func TestEventContextThreadsIntoNestedSteps(t *testing.T) {
	eng := sharedEmbeddedEngine(t)

	cases := []struct {
		logic string
		event map[string]any
		// wantValues must each appear in at least one resolved nested-step query.
		wantValues []string
	}{
		{
			logic: "releaseWorkspaceOnRunTerminal",
			event: map[string]any{"topic": "node.updated", "kind": "node.updated", "payload": map[string]any{
				"id": "run-7f3a", "status": "completed",
			}},
			wantValues: []string{"run-7f3a"},
		},
		// (conflictDetection, this suite's first fixture, published, so the
		// flip moved its statements into the automation that called it and
		// deleted the logic (D14, memql#5373). The automation reads its bound
		// args, not an event envelope.)
		// (generateResponse -- the cognition.response.requested fixture --
		// went with the cognition namespace in epic memql#4988. logicAutoJoinAI
		// moved to the product pack in B2 (#2038) alongside the `space`
		// concept; the pack's own load tests cover the moved logic.)
		// (logicEnsureDailySpaceOnAuthSession -- the coalesce-in-step-body
		// event-threading fixture, memql#1065 -- moved to the product pack in
		// #1976; the remaining core logics keep this coverage. The pack's
		// own load tests cover the moved logic.)
	}

	for _, tc := range cases {
		t.Run(tc.logic, func(t *testing.T) {
			fn, err := eng.Functions().Get(tc.logic)
			if err != nil || fn == nil {
				t.Fatalf("Functions().Get(%s): %v", tc.logic, err)
			}
			if fn.LogicBody == nil {
				t.Fatalf("%s has no statement body", tc.logic)
			}

			reg := &capturingRegistry{}
			runner := automations.NewLogicRunner(eng, reg, eng.Logger)
			if _, err := runner.RunLogicBody(context.Background(), tc.logic, fn.LogicBody, map[string]any{"event": tc.event}); err != nil {
				t.Fatalf("RunLogicBody(%s): %v", tc.logic, err)
			}

			if len(reg.resolved) == 0 {
				t.Fatalf("%s: no nested steps were dispatched", tc.logic)
			}
			joined := strings.Join(reg.resolved, "\n")
			t.Logf("%s resolved nested-step queries:\n%s", tc.logic, joined)

			// (1) Every event-derived value must have threaded into a nested step.
			for _, want := range tc.wantValues {
				if !strings.Contains(joined, want) {
					t.Errorf("%s: event-derived value %q did not thread into any nested step (the #1706 failure -- event.* resolved to empty); resolved:\n%s",
						tc.logic, want, joined)
				}
			}

			// (2) No unresolved event-reference TEXT may leak into a step's args.
			for _, leak := range []string{"args.event.payload", "event.payload"} {
				if strings.Contains(joined, leak) {
					t.Errorf("%s: unresolved event reference %q leaked into a nested step's args (must resolve to a value, never pass through as text):\n%s",
						tc.logic, leak, joined)
				}
			}
		})
	}
}
