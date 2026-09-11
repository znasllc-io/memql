package memql

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/envregistry"
	"github.com/znasllc-io/memql/component/memql/readiness"
)

var inferenceNow = time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)

func aiModule() envregistry.Module {
	return envregistry.Module{Name: "ai", Core: true, Description: "d", Evaluator: envregistry.EvaluatorInferenceStatus}
}

func rowsResolver(rows ...readiness.RegistrationFacts) readinessResolvers {
	return readinessResolvers{
		Registrations: func(context.Context) ([]readiness.RegistrationFacts, error) { return rows, nil },
	}
}

// EVERY NODE TYPE, ONE REPORT.
//
// This is the defect the fold made visible and could not fix. Only an agent
// binary held SetFleetInference and SetAppInference, so `ai` was `configured`
// there and `unconfigured` on every other replica; the fold's disagreement
// rule turned that into `partial`, permanently, on any multi-node cluster --
// a cluster with a working inference door reporting as half set up, on every
// surface, with nothing wrong anywhere.
func TestEveryNodeTypeProducesTheSameInferenceReport(t *testing.T) {
	r := rowsResolver(readiness.RegistrationFacts{
		ConnectedNodeId: "agent-1",
		Labels:          map[string]string{"model:llama3.1:8b": "ctx=8192,structured=true"},
		LastSeenAt:      inferenceNow,
	})
	// The seven node types (root CLAUDE.md, Distributed Node Architecture).
	types := []string{"identity", "bff", "agent", "planner", "workbench", "mcp", "edge"}
	var first readiness.NodeReport
	for i, nodeType := range types {
		got := evaluateModule(context.Background(), r, aiModule(), "node-"+nodeType, nodeType, inferenceNow)
		if i == 0 {
			first = got
			if first.State != readiness.Configured {
				t.Fatalf("a qualifying machine did not configure ai on %s: %+v", nodeType, first)
			}
			continue
		}
		if got.State != first.State {
			t.Errorf("%s reports %s and %s reports %s -- the fold reads any disagreement as partial, "+
				"which is how `ai` came to be stuck at partial on every multi-node cluster",
				nodeType, got.State, first.NodeType, first.State)
		}
		if len(got.Lanes) != len(first.Lanes) {
			t.Errorf("%s reports %d lanes and %s reports %d", nodeType, len(got.Lanes), first.NodeType, len(first.Lanes))
		}
	}
}

// Configured and live are two facts and the row carries both: a laptop closing
// does not change what a cluster is configured to do.
func TestASleepingMachineIsConfiguredAndNotLive(t *testing.T) {
	r := rowsResolver(readiness.RegistrationFacts{
		Labels:          map[string]string{"model:llama3.1:8b": "ctx=8192,structured=true"},
		ConnectedNodeId: "",
		LastSeenAt:      inferenceNow.Add(-readiness.OnlineWindow - time.Minute),
	})
	got := evaluateModule(context.Background(), r, aiModule(), "bff-1", "bff", inferenceNow)
	if got.State != readiness.Configured {
		t.Fatalf("a sleeping machine un-configured the cluster: %s", got.State)
	}
	if readiness.InferenceLive(got.Lanes) {
		t.Errorf("no door is open, so the report must not say one is: %+v", got.Lanes)
	}
	for _, lane := range got.Lanes {
		if lane.Name != readiness.InferenceLaneLocal {
			continue
		}
		if !lane.Complete {
			t.Errorf("the local lane is not complete: %+v", lane)
		}
		for _, slot := range lane.Slots {
			if slot.Name == readiness.InferenceLiveSlot && slot.Present {
				t.Errorf("a machine outside the online window reported live")
			}
		}
	}
}

// A read that FAILS leaves every door SHUT. The core gate branches on this,
// and "we could not ask" reported as an open door sends somebody into a
// console whose every feature then refuses.
func TestAFailedRegistrationReadShutsEveryDoor(t *testing.T) {
	r := readinessResolvers{
		Registrations: func(context.Context) ([]readiness.RegistrationFacts, error) {
			return nil, errors.New("the read did not land")
		},
	}
	got := evaluateModule(context.Background(), r, aiModule(), "bff-1", "bff", inferenceNow)
	if got.State != readiness.Unconfigured {
		t.Fatalf("a failed read produced %s, want unconfigured", got.State)
	}
	if len(got.Lanes) != 3 {
		t.Errorf("a failed read still reports the three doors, all shut: %+v", got.Lanes)
	}
}

// Federation alone configures the module, and it needs no rows at all -- which
// is why a cloud cluster with no fleet is not held on the gate.
func TestFederationAloneConfiguresTheModule(t *testing.T) {
	r := readinessResolvers{FederationConfigured: func() bool { return true }}
	got := evaluateModule(context.Background(), r, aiModule(), "bff-1", "bff", inferenceNow)
	if got.State != readiness.Configured {
		t.Fatalf("federation alone produced %s", got.State)
	}
}

// The floor the gate uses has ONE value. component/memql/readiness cannot
// import its parent, so this is where the two are held equal -- and the
// direction of the failure matters: a readiness floor ABOVE the catalog's
// would hold somebody on the gate while the router happily served them.
func TestTheContextFloorHasOneValue(t *testing.T) {
	if readiness.MinContextWindow != MinimumContextWindow {
		t.Fatalf("readiness.MinContextWindow is %d and MinimumContextWindow is %d",
			readiness.MinContextWindow, MinimumContextWindow)
	}
}

// THE EVALUATION CONTEXT IS THE CLUSTER'S, NEVER THE CALLER'S.
//
// An owner pulling readinessRecompute used to have their OWN machines resolved
// through the fleet seam and written as a node fact. The registration read is
// cluster-wide and must answer identically however it was triggered.
func TestTheEvaluationContextIsTheClustersOwn(t *testing.T) {
	caller := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "v1:identity:user:someone",
		Role:   auth.RoleWriter,
	})
	ctx := readinessEvaluateContext(caller)
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil {
		t.Fatal("no AccessContext on the evaluation context")
	}
	if ac.UserId == "v1:identity:user:someone" {
		t.Fatal("the caller's identity survived into the evaluation, so a recompute writes " +
			"whoever pulled it as a cluster fact")
	}
	if !ac.IsClusterOwner() {
		t.Fatal("the evaluation is not a cluster owner, so allWorkersWithStatus answers ZERO ROWS " +
			"AND NO ERROR -- a cluster full of machines reporting `ai` unconfigured, silently")
	}
	if !ac.Unranked {
		t.Error("the evaluation actor holds a rung on the role ladder; it must be unranked (D4)")
	}
	if !ac.Synthetic {
		t.Error("the evaluation actor is not synthetic; the cluster is acting, not a person")
	}
	if !auth.OriginFromContext(ctx).IsInternal() {
		t.Error("the evaluation context is not internal origin")
	}
}

// The two contexts are DIFFERENT and each is the smallest thing that works.
// Folding them into one would either give the write a cluster owner it does
// not need, or give the evaluation a reader that cannot see a single machine.
func TestTheWriteContextStaysAReader(t *testing.T) {
	wctx := readinessWriteContext(context.Background())
	ac, ok := auth.AccessFromContext(wctx)
	if !ok || ac == nil {
		t.Fatal("no AccessContext on the write context")
	}
	if ac.Role != auth.RoleReader {
		t.Errorf("the write context carries role %q, want %q -- it writes a public concept with no "+
			"owner field behind @serverOnly plus internal origin, and any more authority than that "+
			"is authority nothing here uses", ac.Role, auth.RoleReader)
	}
}

// THE QUERY THE `ai` ARM RUNS MUST LOAD ON EVERY NODE THAT RUNS IT.
//
// `readInferenceRegistrations` executes `allWorkersWithStatus`, and the arm
// leaves every door SHUT when that read fails. So a node type whose embedded
// tree did not carry the query would report `ai` unconfigured while its
// siblings reported configured -- and the fold reads that disagreement as
// `partial`, permanently, which is the exact defect this task exists to end.
//
// The tree is embedded WITHOUT a build tag (dsl/embed.go), so every node type
// carries the same one. This asserts that rather than assuming it, because the
// failure it prevents is silent on every surface.
func TestTheRegistrationQueryLoadsFromTheEmbeddedTree(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if _, err := LoadUnifiedConcepts(logger); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	concepts := memoryNodes.DefaultRegistry()
	registry, err := loadEmbeddedFunctions(logger, concepts)
	if err != nil {
		t.Fatalf("loadEmbeddedFunctions: %v", err)
	}
	if _, _, lerr := LoadUnifiedFunctions(logger, registry, concepts); lerr != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", lerr)
	}
	// A REACHABLE POSITIVE: without it an empty registry passes the assertion
	// below over nothing, which is the failure a registry lookup is most prone
	// to.
	if !registry.Has("moduleReadinessAll") {
		t.Fatal("the readiness feed's own query is missing from this registry, so it is not the " +
			"one the engine loads and the check below would prove nothing")
	}
	if !registry.Has(readinessRegistrationQuery) {
		t.Fatalf("%q does not load from the embedded DSL tree. The ai readiness arm runs it and "+
			"leaves every door shut when the read fails, so this node type would report `ai` "+
			"unconfigured while its siblings reported configured.", readinessRegistrationQuery)
	}
}

// THE EVALUATION ACTOR, OBSERVED RATHER THAN READ (memql#5118, D3).
//
// The engine half of this epic rests on one property: readiness is evaluated
// as the CLUSTER, never as whoever asked. `v1:worker:registration` declares
// `@rowAuthz(owner="ownerUserId", clusterOwner)`, so a read under a caller's
// own actor returns ZERO ROWS AND NO ERROR -- every inference door reads shut,
// `ai` reports `unconfigured`, and the core gate holds a cluster that is set up
// while every manifest and every log line looks correct.
//
// This asserts it through a PROBE RESOLVER, which is the only way to catch it:
// the property had been carried by one assignment in one caller, and reverting
// that line left the whole suite green.
func TestReadinessEvaluatesAsTheClusterAndNotAsTheCaller(t *testing.T) {
	// A caller with exactly the authority that produces the silent failure: a
	// signed-in person who owns none of the cluster's registrations.
	caller := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "v1:identity:user:someone-else",
		Role:   auth.RoleReader,
	})

	var seen []context.Context
	record := func(ctx context.Context) { seen = append(seen, ctx) }
	r := readinessResolvers{
		Registrations: func(ctx context.Context) ([]readiness.RegistrationFacts, error) {
			record(ctx)
			return nil, nil
		},
		Variable: func(ctx context.Context, _ string) (string, error) { record(ctx); return "", nil },
		Secret:   func(ctx context.Context, _ string) (string, error) { record(ctx); return "", nil },
		IsSecret: func(string) bool { return false },
		IntegrationState: func(ctx context.Context, _ string) (string, bool, bool, error) {
			record(ctx)
			return "", false, false, nil
		},
	}

	// One module per evaluator arm, so no arm can be the one that leaks.
	mods := []envregistry.Module{
		aiModule(),
		{Name: "email", Core: true, Evaluator: envregistry.EvaluatorIntegrationPrefix + "email"},
		{Name: "storage", Core: true, Lanes: []envregistry.Lane{{Name: "azure", Slots: []string{"MEMQL_STORAGE_ACCOUNT"}}}},
	}
	evaluateModules(caller, r, mods, "node-a", "bff", inferenceNow)

	if len(seen) < len(mods) {
		t.Fatalf("only %d resolver calls were observed across %d modules -- the probe missed an arm", len(seen), len(mods))
	}
	for i, ctx := range seen {
		ac, ok := auth.AccessFromContext(ctx)
		if !ok || ac == nil {
			t.Fatalf("resolver call %d ran with no access context at all", i)
		}
		// THE CALLER'S IDENTITY IS GONE. This alone is the regression: with it
		// present the registration read is scoped to a person who owns none of
		// the rows.
		if ac.UserId == "v1:identity:user:someone-else" {
			t.Errorf("resolver call %d saw the CALLER's actor (%s) -- readiness would report "+
				"whatever that person happens to be able to read, not what is true of the cluster", i, ac.UserId)
		}
		// AND WHAT REPLACED IT CAN ACTUALLY READ THE ROWS. Each of these is
		// load-bearing for a different gate, so each is named separately.
		//
		// RoleOwner is the `clusterOwner` half of the composite tier, which is
		// what admits a row owned by somebody else.
		if ac.Role != auth.RoleOwner {
			t.Errorf("resolver call %d ran as %q -- the composite owner tier admits another "+
				"person's registration only to a cluster owner", i, ac.Role)
		}
		// Unranked keeps the RANK rules from governing an actor that is the
		// cluster rather than a person (epic memql#4832, D4).
		if !ac.Unranked {
			t.Errorf("resolver call %d ran ranked -- a rank floor it cannot satisfy denies, "+
				"and an unresolvable floor denies silently", i)
		}
		// Synthetic says this can never be a row's owner, so nothing it touches
		// is stamped to it.
		if !ac.Synthetic {
			t.Errorf("resolver call %d ran non-synthetic -- an actor that can own rows is one "+
				"a write path may stamp onto them", i)
		}
		// And internal origin is what @serverOnly and the write guard check.
		if !auth.OriginFromContext(ctx).IsInternal() {
			t.Errorf("resolver call %d ran at client origin", i)
		}
	}
}

// THE CONSEQUENCE, not the mechanism: the same rows, read under two actors.
//
// A resolver that behaves the way row authorization does -- everyone's rows to
// a cluster owner, nobody else's to a plain reader -- must produce `configured`
// on BOTH, because the caller is not part of the question.
func TestAnotherPersonsMachineStillConfiguresInference(t *testing.T) {
	fleet := []readiness.RegistrationFacts{{
		OwnerUserId: "v1:identity:user:the-owner",
		Labels:      map[string]string{"model:llama3.1:8b": "ctx=8192,structured=true"},
		LastSeenAt:  inferenceNow,
	}}
	// Stands in for the composite owner tier: rows come back for a cluster
	// owner, and for anybody else only their own -- which here is none.
	rowAuthz := readinessResolvers{
		Registrations: func(ctx context.Context) ([]readiness.RegistrationFacts, error) {
			ac, _ := auth.AccessFromContext(ctx)
			if ac != nil && ac.Role == auth.RoleOwner {
				return fleet, nil
			}
			var mine []readiness.RegistrationFacts
			for _, row := range fleet {
				if ac != nil && row.OwnerUserId == ac.UserId {
					mine = append(mine, row)
				}
			}
			return mine, nil
		},
	}

	for _, caller := range []struct {
		name string
		ctx  context.Context
	}{
		{"a reader who owns nothing", auth.ContextWithAccess(context.Background(),
			&auth.AccessContext{UserId: "v1:identity:user:someone-else", Role: auth.RoleReader})},
		{"no actor at all", context.Background()},
	} {
		got := evaluateModule(caller.ctx, rowAuthz, aiModule(), "bff-1", "bff", inferenceNow)
		if got.State != readiness.Configured {
			t.Errorf("%s: ai reported %s over a fleet machine somebody else owns -- the core gate "+
				"would hold a configured cluster, and nothing would look wrong", caller.name, got.State)
		}
	}
}

// A FLEET READ THAT BROKE IS NOT A CLUSTER WITH NO DOOR (memql#5118).
//
// Both produce `unconfigured`, and they have to: a read that failed cannot be
// reported as an open door, or the gate lifts and every feature behind it then
// refuses. But the two have completely different repairs -- one is "set up
// inference", the other is "readiness could not read the fleet" -- and the
// verdict has no room to say which. The log line is the only place the
// difference survives, so its absence is the defect this pins.
func TestAFailedFleetReadIsLoggedRatherThanSilentlyShut(t *testing.T) {
	var buf strings.Builder
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	broken := readinessResolvers{
		Registrations: func(context.Context) ([]readiness.RegistrationFacts, error) {
			return nil, errors.New("registration read refused")
		},
		Logger: logger,
	}

	got := evaluateModule(context.Background(), broken, aiModule(), "bff-1", "bff", inferenceNow)

	// THE VERDICT IS STILL SHUT, and that half must not change: a door we
	// could not ask about is not a door we may walk through.
	if got.State != readiness.Unconfigured {
		t.Errorf("a failed fleet read reported %s -- an unaskable door must read shut", got.State)
	}
	line := buf.String()
	if line == "" {
		t.Fatal("a failed fleet read produced no log line at all: an operator sees the same " +
			"'set up inference' screen a fresh cluster gets, with nothing anywhere saying the read broke")
	}
	// The error itself, and enough to find the node it happened on.
	for _, want := range []string{"registration read refused", "bff-1", "ai"} {
		if !strings.Contains(line, want) {
			t.Errorf("the log line does not carry %q, so it cannot be acted on: %s", want, line)
		}
	}
}

// A resolver set with no logger is legal, and the pure tests pass one.
func TestAResolverSetWithNoLoggerStillEvaluates(t *testing.T) {
	got := evaluateModule(context.Background(), readinessResolvers{
		Registrations: func(context.Context) ([]readiness.RegistrationFacts, error) {
			return nil, errors.New("boom")
		},
	}, aiModule(), "bff-1", "bff", inferenceNow)
	if got.State != readiness.Unconfigured {
		t.Errorf("state %s", got.State)
	}
}
