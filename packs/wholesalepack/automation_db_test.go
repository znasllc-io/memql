package wholesalepack_test

// automation_db_test.go -- PROVISIONING FROM A CLIENT'S OWN AUTOMATION, booted
// against a real database with the automation scheduler running.
//
// The grant is the client's: testdata/clients/contoso triggers on a recorded
// approve decision and calls wholesaleProvisionEntitlement. That automation
// runs as the scheduler's system actor, which owns nothing, so the builtin
// has to read as the deployment and write as the application's merchant --
// and the only way to know it does is to let the real scheduler fire it.
//
// Postgres-gated like every *_db_test.go: skips when no database is
// reachable, and MEMQL_REQUIRE_DB=1 turns that skip into a failure.

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	automationSteps "github.com/znasllc-io/memql/component/automations/steps"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
	wholesalepack "github.com/znasllc-io/memql/packs/wholesalepack"
)

// automationFakeAdapter is the adapter the automation test grants through:
// it reaches no store, and it records the actor every Provision ran as.
const automationFakeAdapter = "automationTestFake"

type recordingAdapter struct {
	mu     sync.Mutex
	actors []string
	apps   []string
}

func (a *recordingAdapter) Name() string           { return automationFakeAdapter }
func (a *recordingAdapter) Available(string) error { return nil }

func (a *recordingAdapter) Provision(ctx context.Context, _ wholesalepack.Caller, g wholesalepack.Grant) (wholesalepack.Outcome, error) {
	actor := ""
	if ac, ok := auth.AccessFromContext(ctx); ok {
		actor = ac.UserId
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.actors = append(a.actors, actor)
	a.apps = append(a.apps, g.ApplicationID)
	return wholesalepack.Outcome{Reference: "fake-" + g.ApplicationID, Detail: "granted by the test adapter"}, nil
}

func (a *recordingAdapter) Revoke(context.Context, wholesalepack.Caller, wholesalepack.Grant) (wholesalepack.Outcome, error) {
	return wholesalepack.Outcome{}, nil
}

// callsFor is the actors every Provision for one application ran as.
func (a *recordingAdapter) callsFor(applicationID string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for i, app := range a.apps {
		if app == applicationID {
			out = append(out, a.actors[i])
		}
	}
	return out
}

// lockedBuffer is a log sink the scheduler's goroutines write to safely.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// lines is every logged line that mentions want.
func (b *lockedBuffer) lines(want string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for _, l := range strings.Split(b.buf.String(), "\n") {
		if strings.Contains(l, want) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

var (
	fakeAdapterOnce sync.Once
	fakeAdapter     = &recordingAdapter{}
)

// automatedWholesaleEngine boots the live engine with the contoso fixture
// mounted, as TestLiveE2E_BothFixtureClientsBootOverTheUneditedPack does, and
// the automation scheduler running on the engine's own event bus.
func automatedWholesaleEngine(t *testing.T) *memql.MemQLEngine {
	t.Helper()
	fakeAdapterOnce.Do(func() { wholesalepack.RegisterAdapter(fakeAdapter) })

	dir := filepath.Join("testdata", "clients", clientContoso)
	memqldsl.RegisterTree(clientContoso, os.DirFS(dir))
	t.Cleanup(func() { memqldsl.UnregisterTree(clientContoso) })

	eng := liveWholesaleEngine(t)

	// ONE BUS: the engine publishes graph.node.created.* on it and the
	// scheduler subscribes on it (examples/referencepack's live harness).
	bus := events.NewBus()
	eng.SetEventBus(bus)
	steps := automationSteps.NewRegistry()
	eng.SetLogicRunner(automations.NewLogicRunner(eng, steps, eng.Logger))
	// THE SCHEDULER'S LOG IS KEPT and printed with a failure, so a grant
	// automation that failed says why beside the assertion that noticed.
	schedLog := &lockedBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("automation scheduler log (fixture lines):\n%s", schedLog.lines("contoso"))
		}
	})
	sched, err := automations.NewScheduler(automations.SchedulerOptions{
		Loader:       automations.NewLoader(automations.LoaderOptions{Registry: memoryNodes.DefaultRegistry()}),
		Engine:       eng,
		EventBus:     bus,
		StepRegistry: steps,
		Logger:       slog.New(slog.NewTextHandler(schedLog, nil)),
	})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	sched.Start(context.Background())
	t.Cleanup(func() { sched.Stop(context.Background()) })
	// START LOADS AND SUBSCRIBES ON ITS OWN GOROUTINE. A decision written
	// before Ready() closes is an event nobody is listening for yet.
	select {
	case <-sched.Ready():
	case <-time.After(60 * time.Second):
		t.Fatal("the automation scheduler was not ready within 60s")
	}
	return eng
}

// THE CLIENT'S GRANT AUTOMATION PROVISIONS AN APPROVED APPLICATION. The
// merchant approves; contoso's automation fires on the decision and calls the
// pack's builtin; the entitlement lands owned by the merchant, so the
// merchant reads it back.
func TestAClientGrantAutomationProvisionsAnApprovedApplication(t *testing.T) {
	eng := automatedWholesaleEngine(t)
	merchant, storeLive, _ := wholesaleScope()
	ctx := asSiteOwner(merchant)

	openApplications(t, eng, ctx, storeLive, automationFakeAdapter)
	appID := submit(t, eng, ctx, storeLive, "Automated Trading", "buyer@automated.example")
	decide(t, eng, asDecider(t, eng, merchant), appID, wholesalepack.TransitionApprove, merchant, "approved by the merchant")

	var ent map[string]any
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		rows := wsRows(t, eng, ctx,
			`query entitlementForApplication(applicationId: `+wsQuote(appID)+`)`)
		if len(rows) > 0 && rows[0]["state"] == wholesalepack.EntitlementGranted {
			ent = rows[0]
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if ent == nil {
		t.Fatalf("within 10s of the approval no granted entitlement was readable by the "+
			"merchant; the client's automation did not provision it (adapter calls: %v)",
			fakeAdapter.callsFor(appID))
	}
	if got, _ := ent["ownerUserId"].(string); got != userRef(merchant) {
		t.Fatalf("the entitlement's ownerUserId = %q, want the application's merchant %q -- "+
			"a row written as the automation's system actor is owned by nobody", got, merchant)
	}
	calls := fakeAdapter.callsFor(appID)
	if len(calls) != 1 {
		t.Fatalf("the adapter was called %d times for one approval, want 1: %v", len(calls), calls)
	}
	if calls[0] != userRef(merchant) {
		t.Fatalf("the adapter ran as %q, want the application's merchant %q", calls[0], merchant)
	}
}

// A DIRECT CLIENT CALL TO THE DECISION MUTATION IS REFUSED. The decision is
// the builtin's to judge -- the legality fold, and later the acceptance rule
// -- so a client calling the write around it is refused by the engine and
// leaves no decision behind.
func TestADirectClientCallToAppendApplicationDecisionIsRefused(t *testing.T) {
	eng := liveWholesaleEngine(t)
	merchant, storeLive, _ := wholesaleScope()
	ctx := asSiteOwner(merchant)

	openApplications(t, eng, ctx, storeLive, wholesalepack.AdapterCustomerTag)
	appID := submit(t, eng, ctx, storeLive, "Direct Caller", "buyer@direct.example")

	client := auth.ContextWithClientOrigin(ctx)
	_, err := eng.Execute(client, `appendApplicationDecision(applicationId: `+wsQuote(appID)+
		`, note: "around the builtin", storeId: `+wsQuote(storeLive)+`, transition: "approve")`)
	if err == nil {
		t.Fatal("a client-origin call to appendApplicationDecision was accepted; it must be " +
			"refused, because the decision is the builtin's to judge")
	}
	// THE SERVER-ONLY REFUSAL, not a parse error or a missing argument.
	if !strings.Contains(err.Error(), `"appendApplicationDecision" is server-only and cannot be called by a client`) {
		t.Fatalf("the direct call was refused, but not as server-only: %v", err)
	}
	if rows := wsRows(t, eng, ctx,
		`query decisionsForApplication(applicationId: `+wsQuote(appID)+`)`); len(rows) != 0 {
		t.Fatalf("decisionsForApplication answered %d rows after a refused direct call, want 0: %+v",
			len(rows), rows)
	}
}
