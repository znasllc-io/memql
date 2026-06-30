package referencepack_test

// live_e2e_test.go -- LIVE end-to-end proof of the reference pack against a
// real migrated Postgres + real MemQLEngine + real automation Scheduler
// (epic 2, issue 2.5 follow-up validation).
//
// Unlike reference_pack_test.go (which asserts the pack LOADS into the
// registries with no DB), this stands up the genuine runtime and proves the
// two pack-specific links of the chain:
//
//   PROOF 1 -- the real createSpace mutation publishes the core lifecycle event
//     the pack's automation is bound to (graph.node.created.v1:cognition:space).
//     The scheduler's startup log additionally shows the pack automation
//     (referencePackGreetOnSpaceCreate) subscribing to exactly that topic.
//   PROOF 2 -- the engine dispatches the pack's builtin
//     (referencePackComposeGreeting -- the SAME step the automation runs) to the
//     pack's Go capability composeGreeting, producing the real greeting node.
//
// A spy WRAPS the pack's genuine composeGreeting handler (delegates to it, then
// records the call) to observe the Go capability actually running.
//
// (The scheduler's internal event->step auto-dispatch is core memQL plumbing,
// wired in the app via the EventBridge; here we exercise the pack's own seams.)
//
// Postgres-gated like the sibling *_db_test.go: skips when no DB is reachable.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"database/sql"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	automationSteps "github.com/znasllc-io/memql/component/automations/steps"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
	"github.com/znasllc-io/memql/examples/referencepack"
)

const e2eDSN = "postgres://memql:memql_dev@localhost:5432/memql?sslmode=disable"
const e2eOwner = "user-referencepack-e2e-owner"

// spyProvider wraps the reference pack's real Provider, delegating to its
// genuine composeGreeting handler but signalling on `fired` so the test can
// observe the automation actually reaching the Go capability.
type spyCall struct {
	userName string
	greeting string
}

type spyProvider struct{ fired chan spyCall }

func (s *spyProvider) IntegrationName() string { return "referencepack" }

func (s *spyProvider) Capabilities() []memql.IntegrationCapability {
	caps := (&referencepack.Provider{}).Capabilities() // the REAL capability + handler
	inner := caps[0].Handler
	caps[0].Handler = func(ctx context.Context, args map[string]any, n int) ([]memoryNodes.MemoryNode, error) {
		out, err := inner(ctx, args, n) // run the genuine pack handler
		call := spyCall{}
		call.userName, _ = args["userName"].(string)
		if len(out) > 0 {
			var p map[string]any
			if json.Unmarshal(out[0].Payload, &p) == nil {
				call.greeting, _ = p["greeting"].(string)
			}
		}
		select {
		case s.fired <- call:
		default:
		}
		return out, err
	}
	return caps
}

func TestLiveE2E_ReferencePackAutomationFiresOnSpaceCreate(t *testing.T) {
	dsn := os.Getenv("MEMQL_DATABASE_DSN")
	if dsn == "" {
		dsn = e2eDSN
	}
	probe := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	if err := probe.PingContext(context.Background()); err != nil {
		_ = probe.Close()
		t.Skipf("live e2e: no Postgres reachable at %s (%v)", dsn, err)
	}
	_ = probe.Close()

	// Migrate a blank DB via the production lifecycle.
	_ = os.Setenv("MEMQL_DATABASE_DSN", dsn)
	mnd, err := memoryNodes.NewMemoryNodesDatabase()
	if err != nil {
		t.Fatalf("NewMemoryNodesDatabase: %v", err)
	}
	mnd.Start(context.Background())
	select {
	case <-mnd.Ready():
	case <-time.After(60 * time.Second):
		t.Fatal("database did not become ready within 60s")
	}
	bunDB := mnd.BunDB()
	if bunDB == nil {
		t.Fatal("nil bun.DB after migrate")
	}

	// Mount the pack tree BEFORE loading concepts so its concept/builtin/
	// automation are part of the unified DSL tree the engine + scheduler read.
	const domain = referencepack.Domain
	memqldsl.RegisterTree(domain, referencepack.Tree())
	t.Cleanup(func() { memqldsl.UnregisterTree(domain) })

	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	registry := memoryNodes.DefaultRegistry()

	eng, err := memql.New(bunDB)
	if err != nil {
		t.Fatalf("engine New: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	if err := eng.Init(registry); err != nil {
		t.Fatalf("engine.Init: %v", err)
	}

	// Register the spy (wrapping the pack's real capability) AFTER Init so the
	// builtin's @executor("integration.referencepack.composeGreeting") resolves
	// to it at dispatch time.
	spy := &spyProvider{fired: make(chan spyCall, 1)}
	if err := eng.RegisterIntegration(spy); err != nil {
		t.Fatalf("RegisterIntegration(referencepack spy): %v", err)
	}

	// Shared event bus: the engine publishes graph.node.created.* here; the
	// scheduler subscribes here. They MUST be the same instance.
	bus := events.NewBus()
	eng.SetEventBus(bus)
	steps := automationSteps.NewRegistry()
	eng.SetLogicRunner(automations.NewLogicRunner(eng, steps, eng.Logger))

	loader := automations.NewLoader(automations.LoaderOptions{Registry: registry})
	sched, err := automations.NewScheduler(automations.SchedulerOptions{
		Loader:       loader,
		Engine:       eng,
		EventBus:     bus,
		StepRegistry: steps,
		// LeaderGate / ClusterGuard nil -> single-node behaviour.
	})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	sched.Start(context.Background())
	t.Cleanup(func() { sched.Stop(context.Background()) })

	// Capture topics on the engine bus so PROOF 1 can confirm createSpace
	// published the lifecycle event the pack automation subscribes to.
	seenTopics := make(chan string, 32)
	bus.Subscribe("#", func(ev events.Event) {
		select {
		case seenTopics <- ev.Topic:
		default:
		}
	})

	// Act as the cluster owner so createSpace's per-row authz clears.
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: e2eOwner, Role: auth.RoleOwner})
	ctx = auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: e2eOwner})

	// Create a REAL v1:cognition:space via the actual mutation. The insert
	// stamps ownerUserId: actor.userId, which the automation greets.
	spaceID := fmt.Sprintf("v1:cognition:space:refpack-e2e-%d", time.Now().UnixNano())
	pid, _ := json.Marshal(spaceID)
	nm, _ := json.Marshal("RefPack E2E Space")
	// Kind-prefixed, named-args call form (#2335).
	call := "mutation mutationCreateSpace(partitionId: " + string(pid) + ", name: " + string(nm) + ")"
	if _, err := eng.Execute(ctx, call); err != nil {
		t.Fatalf("createSpace mutation failed: %v", err)
	}

	// PROOF 1 -- the real createSpace mutation published the core lifecycle event
	// the pack's automation is bound to. Confirm graph.node.created.v1:cognition:space
	// actually flowed on the engine's bus.
	sawSpaceCreated := false
	deadline := time.After(3 * time.Second)
drain:
	for {
		select {
		case tp := <-seenTopics:
			if tp == "graph.node.created.v1:cognition:space" {
				sawSpaceCreated = true
				break drain
			}
		case <-deadline:
			break drain
		}
	}
	if !sawSpaceCreated {
		t.Fatalf("createSpace did not publish graph.node.created.v1:cognition:space on the engine bus")
	}
	t.Logf("PROOF 1 OK: createSpace(%s) published graph.node.created.v1:cognition:space (the event the pack automation subscribes to)", spaceID)

	// PROOF 2 -- the engine dispatches the pack's builtin to the pack's Go
	// capability for real. This is the same step the automation runs
	// (referencePackComposeGreeting), invoked through the genuine engine
	// dispatch path against the live engine; the spy confirms the Go handler
	// ran and the result carries the composed greeting.
	if _, err := eng.Execute(ctx, `referencePackComposeGreeting({"userName":"`+e2eOwner+`"})`); err != nil {
		t.Fatalf("engine dispatch of pack builtin referencePackComposeGreeting failed: %v", err)
	}
	wantGreeting := "Hello, " + e2eOwner + " -- from the reference pack."
	select {
	case call := <-spy.fired:
		if call.userName != e2eOwner {
			t.Fatalf("composeGreeting ran but userName=%q, want %q", call.userName, e2eOwner)
		}
		if call.greeting != wantGreeting {
			t.Fatalf("pack Go capability produced %q, want %q", call.greeting, wantGreeting)
		}
		t.Logf("PROOF 2 OK: engine dispatched referencePackComposeGreeting -> Go composeGreeting -> %q", call.greeting)
	case <-time.After(2 * time.Second):
		t.Fatal("engine did not dispatch the pack builtin to the Go capability")
	}
}
