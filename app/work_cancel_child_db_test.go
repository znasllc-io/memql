package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
	work "github.com/znasllc-io/memql/integrations/work"
)

type cancelChildRegistry struct {
	*steps.FunctionExecutor
	entered   chan string
	release   chan struct{}
	effects   map[string]int
	failure   error
	announced sync.Once
	waitCalls int
}

func (p *cancelChildRegistry) Execute(ctx context.Context, step *automations.Step, sc *automations.StepContext) (*automations.StepResult, error) {
	if step.Function == nil || !strings.HasPrefix(step.Function.Name, "cancelFixture") {
		return p.FunctionExecutor.Execute(ctx, step, sc)
	}
	now := time.Now()
	result := &automations.StepResult{StepId: step.ID, Status: "success", StartedAt: now, CompletedAt: now, Result: "kept"}
	if step.Function.Name == "cancelFixtureWait" {
		p.announced.Do(func() { p.entered <- sc.Execution.ID })
		p.waitCalls++
		<-p.release
		result.Status, result.Error = "failed", p.failure.Error()
		return result, p.failure
	}
	p.effects[sc.Execution.AutomationName+":"+step.Function.Name]++
	return result, nil
}

type observedLifecycleGate struct {
	*work.Integration
}

type childStopObservationKey struct{}

type childStopObservation struct {
	entered chan struct{}
	once    sync.Once
}

func (p *observedLifecycleGate) HoldWorkRunLifecycle(ctx context.Context) (*memql.GateHold, error) {
	if observation, ok := ctx.Value(childStopObservationKey{}).(*childStopObservation); ok {
		observation.once.Do(func() { close(observation.entered) })
	}
	return p.Integration.HoldWorkRunLifecycle(ctx)
}

type childStopFixture struct {
	writer, reader *memql.MemQLEngine
	db             *bun.DB
	peer           *work.Integration
}

func newChildStopFixture(t *testing.T, withGate bool) *childStopFixture {
	t.Helper()
	writer, db := workTemplateDBEngineAndDB(t)
	reader := workTemplateDBEngine(t)
	var gates *sql.DB
	if withGate {
		var err error
		gates, err = database.NewGatePool(dbtest.DSN(), 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = gates.Close() })
	}
	fixture := &childStopFixture{writer: writer, reader: reader, db: db}
	for _, engine := range []*memql.MemQLEngine{writer, reader} {
		integration := work.New(engine, engine.Logger, func() *bun.DB { return db })
		if withGate {
			integration.SetGateDB(func() *sql.DB { return gates })
		}
		if engine == writer {
			if err := engine.RegisterIntegration(&observedLifecycleGate{Integration: integration}); err != nil {
				t.Fatal(err)
			}
		} else {
			fixture.peer = integration
			if err := engine.RegisterIntegration(integration); err != nil {
				t.Fatal(err)
			}
		}
	}
	return fixture
}

// The public decision lands on a peer with no executor state. Both the remote
// string error and a cancelled caller must close the native child and its root,
// without retrying effects or following the authored continue branch.
func TestWorkDBParentStopFinalizesSynchronousChild(t *testing.T) {
	// Share only the compiled engines and fixed integration wiring under this
	// parent. The no-Stop controls retain a separate pair with no gate configured;
	// each case still creates its own actor, runs, executor and context observation.
	stopped := newChildStopFixture(t, true)
	var ordinary *childStopFixture
	for _, mode := range []string{"remote error", "cancelled context", "rerun parent", "new root generation", "new child generation", "independent child goal", "foreign child", "changed parent", "child rerun", "foreign ancestor", "changed ancestor", "no Stop provider", "no Stop deadline", "no Stop context cancelled"} {
		fixture := stopped
		if strings.HasPrefix(mode, "no Stop") {
			if ordinary == nil {
				ordinary = newChildStopFixture(t, false)
			}
			fixture = ordinary
		}
		t.Run(mode, func(t *testing.T) { exerciseChildStop(t, mode, fixture) })
	}
}
func exerciseChildStop(t *testing.T, mode string, fixture *childStopFixture) {
	writer, reader, db, peer := fixture.writer, fixture.reader, fixture.db, fixture.peer
	var err error
	key := id.NewShortId()
	owner, rootID, goalID := key+"owner", key+"root", key+"goal"
	person := auth.ContextWithUserActor(context.Background(), owner)
	person = auth.ContextWithAccess(person, &auth.AccessContext{UserId: owner, Role: auth.RoleWriter})
	ctx, cancel := context.WithCancel(common.ContextWithRun(person, common.RunContext{RunId: rootID, GoalId: goalID, OwnerUserId: owner, Mode: common.RunModeLive}))
	defer cancel()
	noStop := strings.HasPrefix(mode, "no Stop")
	waitingGate := make(chan struct{})
	ctx = context.WithValue(ctx, childStopObservationKey{}, &childStopObservation{entered: waitingGate})
	probe := &cancelChildRegistry{FunctionExecutor: &steps.FunctionExecutor{}, entered: make(chan string, 1), release: make(chan struct{}), effects: map[string]int{}, failure: errors.New("remote call: the person asked this work to stop")}
	loader := automations.NewLoader(automations.LoaderOptions{Functions: writer.Functions()})
	tpl := &workTemplate{members: map[string]*automations.Automation{}}
	for _, source := range []string{
		"@template\n@journalRequired\nautomation cancelChild { builtin cancelFixturePrefix()\n builtin cancelFixtureWait() retry(2) on error continue\n builtin cancelFixtureTail() }",
		"@template\n@journalRequired\nautomation cancelMiddle { builtin cancelFixturePrefix()\n automation cancelChild() retry(2) on error continue\n builtin cancelFixtureTail() }",
		"@template\n@journalRequired\nautomation cancelParent { builtin cancelFixturePrefix()\n automation cancelMiddle() retry(2) on error continue\n builtin cancelFixtureTail() }",
	} {
		a, e := loader.CompileSource(source, "cancellation-fixture.memql")
		if e != nil {
			t.Fatal(e)
		}
		tpl.members[a.Name] = a
	}
	registry := steps.NewRegistry()
	registry.Register(automations.StepTypeFunction, probe)
	trigger := &workTemplateTrigger{app: &App{engine: writer, stepRegistry: registry, automationLoader: loader, Logger: writer.Logger}, template: tpl}
	write := func(name string, args map[string]any) {
		templateMutation(t, reader, auth.ContextWithInternalOrigin(person), name, args)
	}
	write("createWorkGoal", map[string]any{"goalId": goalID, "statement": "Synthetic nested cancellation", "origin": "user"})
	write("createWorkRun", map[string]any{"runId": rootID, "goalId": goalID, "automationName": "cancelParent", "templateFingerprint": tpl.members["cancelParent"].DefinitionFingerprint(id.New()), "status": "running", "mode": "live", "startedAt": time.Now().UTC().Format(time.RFC3339Nano), "heartbeatAt": time.Now().UTC().Format(time.RFC3339Nano)})
	write("updateWorkRun", map[string]any{"runId": rootID, "spent": map[string]any{"modelCalls": 3}})
	var prepared *automations.RunJournal
	var resumeOptions *automations.ResumeOptions
	if mode == "rerun parent" {
		requestID := key + "existing-rerun"
		ctx = common.ContextWithRun(ctx, common.RunContext{RunId: rootID, GoalId: goalID, OwnerUserId: owner, Mode: common.RunModeLive, RerunRequestId: requestID})
		write("createWorkStep", map[string]any{"stepId": rootID + "-cancelFixturePrefix", "runId": rootID, "key": "cancelFixturePrefix", "seq": 0, "stepType": "function", "status": "done", "attempt": 1, "version": 1, "result": map[string]any{"value": "retained previous effect"}})
		write("updateWorkRun", map[string]any{"runId": rootID, "head": map[string]any{"cancelFixturePrefix": map[string]any{"version": 1}}, "rerun": map[string]any{"requestId": requestID, "reason": "rerun", "stepKey": "cancelMiddle", "versions": map[string]any{"cancelMiddle": 2, "cancelFixtureTail": 2}}})
		journal, loadErr := automations.LoadRunJournal(memql.ContextWithFreshRead(person), writer, rootID)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		prepared, resumeOptions, err = automations.PrepareRerun(journal, nil, tpl.members["cancelParent"])
		if err != nil {
			t.Fatal(err)
		}
	}
	executor := trigger.executor()
	defer executor.Close()
	type outcome struct {
		run *automations.AutomationExecution
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		if prepared != nil {
			r, e := executor.ResumeFrom(ctx, prepared, tpl.members["cancelParent"], resumeOptions)
			done <- outcome{r, e}
		} else {
			r, e := executor.ExecuteAdopted(ctx, tpl.members["cancelParent"], automations.RunAdoption{RunId: rootID})
			done <- outcome{r, e}
		}
	}()
	var childID string
	select {
	case childID = <-probe.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("child did not enter")
	}
	var middleID string
	if err := db.NewRaw(`SELECT payload->>'parentRunId' FROM "MemoryNodes" WHERE id=? ORDER BY "createdAt" DESC LIMIT 1`, "v1:work:run:"+childID).Scan(context.Background(), &middleID); err != nil {
		t.Fatal(err)
	}
	middleID = memql.BareShortId(middleID)
	if middleID == "" || middleID == rootID {
		t.Fatal("missing real intermediate child")
	}
	// Warm a result cache before the other engine records Stop.
	if _, err := writer.Execute(person, fmt.Sprintf("query workRunForOwner(runId: %s)", langparser.QuoteString(rootID))); err != nil {
		t.Fatal(err)
	}
	if !noStop {
		if _, err := reader.Execute(person, fmt.Sprintf("builtin cancelGoal(goalId: %s)", langparser.QuoteString(goalID))); err != nil {
			t.Fatal(err)
		}
	}
	var hold *memql.GateHold
	if mode == "new root generation" {
		hold, err = peer.HoldWorkRunLifecycle(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer hold.Release()
	}
	switch mode {
	case "no Stop deadline":
		probe.failure = context.DeadlineExceeded
	case "no Stop context cancelled":
		probe.failure = context.Canceled
	case "child rerun":
		write("updateWorkRun", map[string]any{"runId": childID, "rerun": map[string]any{"requestId": "independent-child-attempt", "stepKey": "cancelFixtureWait"}})
	case "cancelled context":
		cancel()
		probe.failure = context.Canceled
	case "new child generation":
		write("createWorkStep", map[string]any{"stepId": childID + "-cancelFixtureWait", "runId": childID, "key": "cancelFixtureWait", "seq": 1, "stepType": "function", "status": "running", "attempt": 9, "version": 9, "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	case "independent child goal":
		// createWorkRun is the existing server-only insert/merge path; no user row
		// is ever touched by this isolated fixture.
		write("createWorkRun", map[string]any{"runId": childID, "goalId": goalID, "automationName": "cancelChild", "templateFingerprint": tpl.members["cancelChild"].DefinitionFingerprint(id.New()), "status": "running", "mode": "live", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	case "foreign child", "foreign ancestor":
		target := childID
		if mode == "foreign ancestor" {
			target = middleID
		}
		var original map[string]any
		if err := db.NewRaw(`SELECT payload FROM "MemoryNodes" WHERE id=? ORDER BY "createdAt" DESC LIMIT 1`, "v1:work:run:"+target).Scan(context.Background(), &original); err != nil {
			t.Fatal(err)
		}
		original["ownerUserId"] = key + "stranger"
		insertWorkRow(t, db, "v1:work:run:"+target, "v1:work:run", time.Now(), original)
	case "changed ancestor":
		write("updateWorkRun", map[string]any{"runId": middleID, "parentRunId": key + "other"})
	case "changed parent":
		write("updateWorkRun", map[string]any{"runId": childID, "parentRunId": key + "other"})
	}
	snapshot := func(runID string) string {
		var value string
		if err := db.NewRaw(`SELECT jsonb_build_object('status',payload->'status','head',payload->'head','rerun',payload->'rerun','owner',payload->'ownerUserId','parent',payload->'parentRunId','fingerprint',payload->'templateFingerprint')::text FROM "MemoryNodes" WHERE id=? ORDER BY "createdAt" DESC LIMIT 1`, "v1:work:run:"+runID).Scan(context.Background(), &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	beforeRoot, beforeChild, beforeMiddle := snapshot(rootID), snapshot(childID), snapshot(middleID)
	close(probe.release)
	if hold != nil {
		select {
		case <-waitingGate:
		case <-time.After(3 * time.Second):
			t.Fatal("cleanup did not reach lifecycle gate")
		}
		write("updateWorkRun", map[string]any{"runId": rootID, "rerun": map[string]any{"requestId": "new-attempt", "reason": "rerun", "stepKey": "cancelChild"}})
		beforeRoot = snapshot(rootID)
		hold.Release()
	}
	var result outcome
	select {
	case result = <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("cancellation cleanup did not finish")
	}
	if noStop {
		if result.err != nil || result.run.Status != "completed" || probe.waitCalls != 3 || len(probe.effects) != 6 {
			t.Fatalf("ordinary authored retry/continue changed: status=%s err=%v calls=%d effects=%v", result.run.Status, result.err, probe.waitCalls, probe.effects)
		}
		select {
		case <-waitingGate:
			t.Fatal("ordinary error required cancellation coordination")
		default:
		}
		for _, runID := range []string{rootID, middleID, childID} {
			var stopped bool
			if err := db.NewRaw(`SELECT COALESCE((payload->'outcome'->>'cancelledAtStepBoundary')::boolean,false) FROM "MemoryNodes" WHERE id=? ORDER BY "createdAt" DESC LIMIT 1`, "v1:work:run:"+runID).Scan(context.Background(), &stopped); err != nil {
				t.Fatal(err)
			}
			if stopped {
				t.Fatal("ordinary provider/caller error attributed to human Stop")
			}
		}
		return
	}
	if probe.waitCalls != 1 {
		t.Fatalf("stopped call retried: %d", probe.waitCalls)
	}
	wantParent, wantEffects := 1, 3
	if mode == "rerun parent" {
		wantParent, wantEffects = 0, 2
	}
	if probe.effects["cancelParent:cancelFixturePrefix"] != wantParent || probe.effects["cancelChild:cancelFixturePrefix"] != 1 || probe.effects["cancelMiddle:cancelFixturePrefix"] != 1 || len(probe.effects) != wantEffects {
		t.Fatalf("completed effects replayed or tail ran: %v", probe.effects)
	}
	if mode != "remote error" && mode != "cancelled context" && mode != "rerun parent" {
		if !errors.Is(result.err, automations.ErrJournalRequired) {
			t.Fatalf("changed generation was finalized: status=%s err=%v", result.run.Status, result.err)
		}
		if snapshot(rootID) != beforeRoot || snapshot(childID) != beforeChild || snapshot(middleID) != beforeMiddle {
			t.Fatal("invalid generation terminally overwrote latest run rows")
		}
		return
	}
	if result.err != nil || result.run.Status != "cancelled" {
		t.Fatalf("root cancellation: status=%s err=%v", result.run.Status, result.err)
	}
	for _, runID := range []string{rootID, middleID, childID} {
		journal, err := automations.LoadRunJournal(memql.ContextWithFreshRead(person), reader, runID)
		if err != nil {
			t.Fatal(err)
		}
		if journal.Status != "cancelled" || journal.Steps["cancelFixturePrefix"] == nil {
			t.Fatalf("terminal/prefix lost for %s: %+v", runID, journal)
		}
	}
	var calls int
	if err := db.NewRaw(`SELECT (payload->'spent'->>'modelCalls')::int FROM "MemoryNodes" WHERE id=? ORDER BY "createdAt" DESC LIMIT 1`, "v1:work:run:"+rootID).Scan(context.Background(), &calls); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("spend changed: %d", calls)
	}
}

// A caller deadline cannot silently release the database fence while a
// dispatched terminal write is still returning. The holder releases it.
func TestWorkDBCancellationLifecycleHoldOutlivesCleanupContext(t *testing.T) {
	engine, db := workTemplateDBEngineAndDB(t)
	gates, err := database.NewGatePool(dbtest.DSN(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer gates.Close()
	integration := work.New(engine, engine.Logger, func() *bun.DB { return db })
	integration.SetGateDB(func() *sql.DB { return gates })
	owner := id.NewShortId()
	ctx := auth.ContextWithUserActor(context.Background(), owner)
	ctx = common.ContextWithRun(ctx, common.RunContext{RunId: id.NewShortId(), GoalId: id.NewShortId(), OwnerUserId: owner})
	cleanup, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	hold, err := integration.HoldWorkRunLifecycle(cleanup)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Release()
	cancel()
	if err := hold.Verify(ctx); err != nil {
		t.Fatalf("cleanup context released retained fence: %v", err)
	}
	blocked, stop := context.WithTimeout(ctx, 400*time.Millisecond)
	defer stop()
	if second, err := integration.HoldWorkRunLifecycle(blocked); err == nil {
		second.Release()
		t.Fatal("peer acquired lifecycle fence before explicit release")
	}
	hold.Release()
	second, err := integration.HoldWorkRunLifecycle(ctx)
	if err != nil {
		t.Fatal(err)
	}
	second.Release()
}
