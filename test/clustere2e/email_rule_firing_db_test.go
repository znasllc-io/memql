package clustere2e

// email_rule_firing_db_test.go -- an email rule fires on the replica where its
// triggering write happened, exactly once, and a rule whose breaker trips stops
// on every replica (memql#6022, F2).
//
// # WHAT WAS BROKEN
//
// The authored owner gate ran an owner's automations on ONE node of the mesh,
// event triggers included. Trigger events for arbitrary concepts are not
// forwarded between nodes (forwarding is default-deny), so an email rule fired
// only when its triggering write happened to land on the owner's node: with two
// replicas, about half of the writes that should have sent mail sent nothing,
// silently. And a rule whose breaker tripped was paused on the tripping node,
// in memory, and nowhere else.
//
// # WHAT THIS PROVES
//
// Two engines over one Postgres, each with its own pool, cache, bus, authored
// registry, authored scheduler, cluster guard and the production authored
// runner -- the rule's real step, the emailRuleFire builtin, through the real
// emailrules integration. The buses are joined by newMeshLink (the real routing
// decision). The owner gate assigns the rule's owner to node A, and the rule
// triggers on v1:todos:todo, a concept the mesh does not forward. Against real
// rows:
//
//   - a write on node B fires the rule once, on B: one firing recorded on the
//     rule, one first-fire claim, one execution claim, one delivery staged;
//   - a write on node A fires it once, on A;
//   - one event that BOTH nodes observe as local runs it once: the execution
//     claim admits one replica and the other's guard counts the duplicate it
//     prevented;
//   - five failed runs on node B trip its breaker, the bundle row reads
//     paused, the rule row reads paused with the sentence the app shows, both
//     nodes disarm the rule, a reconcile pass does not arm it again, and a
//     later write fires nothing;
//   - turned off and on again, the rule comes back clean -- the reused bundle
//     id carries no failure reason -- and fires.
//
// # HOW TO CONFIRM IT IS LOAD-BEARING
//
// Make the authored scheduler consult the owner gate for event triggers again
// (component/automations/authored_scheduler.go, fireOnEvent) and the write on
// node B fires nothing ("node B's write fired the rule 0 times"). Drop the
// runner's ClusterGuard and the doubly-observed event runs on both nodes.

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	automationSteps "github.com/znasllc-io/memql/component/automations/steps"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/emailrules"
	"github.com/znasllc-io/memql/component/events"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// firingReplica is one node wired the way app/engine_authored.go wires one:
// the production authored runner (claim and all) behind the scheduler's Run
// hook, and a breaker whose trip pauses the automation through the engine.
type firingReplica struct {
	db        *bun.DB
	bus       *events.Bus
	eng       *memql.MemQLEngine
	registry  *memql.AuthoredRuntimeRegistry
	scheduler *automations.AuthoredScheduler
	deps      func() memql.AuthoredRuntimeDeps
	guard     *automations.ClusterExecutionGuard
	breaker   *automations.AuthoredBreaker

	// failRuns makes every firing on this node fail before it runs, which is
	// how the test drives a rule into its breaker.
	failRuns atomic.Bool

	mu    sync.Mutex
	ran   map[string]int // automation name -> firings handed to the runner here
	gated map[string]int // owner -> owner gate consultations here
	trips []error        // the error of each pause a trip ran here
	// tripOutcomes is what each of those pauses did.
	tripOutcomes []memql.AuthoredTripOutcome
}

func (r *firingReplica) bump(counter map[string]int, key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	counter[key]++
}

func (r *firingReplica) read(counter map[string]int, key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return counter[key]
}

func quietFiringLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// materializeEmailRules registers the emailrules integration on eng over this
// replica's own pool, as app/plugins.go materializes it on a node.
func materializeEmailRules(t *testing.T, eng *memql.MemQLEngine, db *bun.DB, logger *slog.Logger) {
	t.Helper()
	for _, p := range memql.RegisteredPlugins() {
		if p.Name != emailrules.IntegrationName {
			continue
		}
		prov, err := p.Factory(memql.PluginContext{Logger: logger, Engine: eng, BunDB: func() *bun.DB { return db }})
		if err != nil || prov == nil {
			t.Fatalf("materialize the emailrules integration: %v (provider %v)", err, prov)
		}
		if err := eng.RegisterIntegration(prov); err != nil {
			t.Fatalf("register the emailrules integration: %v", err)
		}
		return
	}
	t.Fatal("the emailrules integration is not registered as a plug-in")
}

func newFiringReplica(t *testing.T, dsn string, ownsTheOwner bool) *firingReplica {
	t.Helper()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	ping, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ping); err != nil {
		_ = db.Close()
		dbtest.Unreachable(t, "email rule firing across replicas", dsn, err)
		return nil
	}
	t.Cleanup(func() { _ = db.Close() })

	logger := quietFiringLogger()
	eng, err := memql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	eng.Logger = logger
	if err := eng.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}
	r := &firingReplica{
		db: db, bus: events.NewBus(), eng: eng, registry: memql.NewAuthoredRuntimeRegistry(),
		guard: automations.NewClusterExecutionGuard(func() *bun.DB { return db }, logger),
		ran:   map[string]int{}, gated: map[string]int{},
	}
	t.Cleanup(r.bus.Close)
	eng.SetEventBus(r.bus)
	materializeEmailRules(t, eng, db, logger)

	run := automations.NewAuthoredRunner(automations.AuthoredRunnerOptions{
		Logger: logger, Engine: eng, EventBus: r.bus,
		StepRegistry: automationSteps.NewRegistry(), ClusterGuard: r.guard,
	})
	// As app/engine_authored.go wires it: a trip pauses the version that
	// failed through the engine, off the failing run's goroutine, unless the
	// breaker has been reset by then.
	r.breaker = automations.NewAuthoredBreaker(0, func(info automations.BreakerTripInfo) {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			outcome, err := eng.PauseTrippedAutomation(ctx, r.deps(), memql.AuthoredTrip{
				Owner: info.Owner, Automation: info.Automation, BundleId: info.BundleId, Version: info.Version,
				Reason:       fmt.Sprintf("Paused automatically after %d failed runs in a row.", info.Failures),
				StillTripped: func() bool { return r.breaker.IsOpen(info.Owner, info.Automation) },
			})
			r.mu.Lock()
			r.trips = append(r.trips, err)
			r.tripOutcomes = append(r.tripOutcomes, outcome)
			r.mu.Unlock()
		}()
	})
	scheduler, err := automations.NewAuthoredScheduler(automations.AuthoredSchedulerOptions{
		Loader:   automations.NewLoader(automations.LoaderOptions{Registry: memorynodes.DefaultRegistry()}),
		EventBus: r.bus,
		Breaker:  r.breaker,
		Run: func(ctx context.Context, a *automations.Automation, ev *events.Event) error {
			r.bump(r.ran, a.Name)
			if r.failRuns.Load() {
				return fmt.Errorf("the rule's run failed on purpose")
			}
			return run(ctx, a, ev)
		},
		OwnerGate: func(owner string) bool {
			r.bump(r.gated, owner)
			return ownsTheOwner
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(scheduler.Stop)
	r.scheduler = scheduler

	deps := memql.AuthoredRuntimeDeps{Registry: r.registry, RegisterHook: scheduler.Activate, DeregisterHook: scheduler.Deactivate}
	r.deps = func() memql.AuthoredRuntimeDeps { return deps }
	eng.SetAuthoredRuntimeDeps(r.deps)
	// The App's handoff to emailrules, which also registers the listener that
	// marks a rule paused when this node records its breaker's pause.
	emailrules.Bind(emailrules.EngineAdapter{Engine: eng}, r.deps)
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	eng.StartAuthoringArmSubscriber(ctx)
	return r
}

// firingFacts reads, straight from the tables, what the rule's firings left
// behind.
type firingFacts struct {
	firedCount  int64
	firstClaims int64 // emailrules' once-per-row claims for the rule
	runClaims   int64 // the authored executor's per-(automation, event) claims
	deliveries  int64 // outbound request versions staged to the owner with this template's subject
}

func readFiringFacts(t *testing.T, db *bun.DB, rule, name, ownerEmail, subject string) firingFacts {
	t.Helper()
	ctx := context.Background()
	var f firingFacts
	if err := db.QueryRowContext(ctx, `
		SELECT COALESCE((payload->>'firedCount')::bigint, 0) FROM "MemoryNodes"
		 WHERE id = ? ORDER BY "createdAt" DESC LIMIT 1`, "v1:campaigns:emailRule:"+rule).Scan(&f.firedCount); err != nil {
		t.Fatalf("read the rule's fired count: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM automation_execution_claims WHERE automation_name LIKE ?`,
		"emailrules.firstFire:%"+rule).Scan(&f.firstClaims); err != nil {
		t.Fatalf("read the first-fire claims: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM automation_execution_claims WHERE automation_name = ?`,
		name).Scan(&f.runClaims); err != nil {
		t.Fatalf("read the execution claims: %v", err)
	}
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM "MemoryNodes"
		 WHERE concept = 'v1:platform:outboundRequest' AND payload->>'target' = ? AND payload->>'subject' = ?`,
		ownerEmail, subject).Scan(&f.deliveries); err != nil {
		t.Fatalf("read the staged deliveries: %v", err)
	}
	return f
}

func readBundleRow(t *testing.T, db *bun.DB, bundleID string) (status, reason string) {
	t.Helper()
	err := db.QueryRowContext(context.Background(), `
		SELECT COALESCE(payload->>'status', ''), COALESCE(payload->>'failureReason', '') FROM "MemoryNodes"
		 WHERE concept = 'v1:authoring:bundle' AND (id = ? OR id = ?) ORDER BY "createdAt" DESC LIMIT 1`,
		bundleID, "v1:authoring:bundle:"+memql.BareShortId(bundleID)).Scan(&status, &reason)
	if err != nil {
		t.Fatalf("read bundle %s: %v", bundleID, err)
	}
	return status, reason
}

func TestAnEmailRuleFiresOnceWhereItsWriteHappened(t *testing.T) {
	dsn := armingDSN(t)
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	a := newFiringReplica(t, dsn, true) // the owner gate assigns the rule's owner to node A
	b := newFiringReplica(t, dsn, false)
	if a == nil || b == nil {
		return
	}
	link := newMeshLink(t, "node-a", a.bus, "node-b", b.bus)
	ctx := context.Background()

	// --- Seed: an owner, an organization, a template in it and a rule on todos ---
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	short, org, template, rule := "fire-user-"+suffix, "fire-org-"+suffix, "fire-tpl-"+suffix, "fire-rule-"+suffix
	owner := "v1:identity:user:" + short
	ownerEmail := short + "@example.test"
	subject := "Todo added " + suffix
	q := langparser.QuoteString

	system := auth.ContextWithInternalOrigin(auth.ContextWithAccess(ctx, auth.SystemActor("email-rule-firing-test")))
	system = auth.ContextWithToken(system, &auth.TokenInfo{Subject: "email-rule-firing-test"})
	ownerCtx := auth.ContextWithToken(auth.ContextWithAccess(ctx,
		&auth.AccessContext{UserId: owner, Role: auth.RoleOwner}), &auth.TokenInfo{Subject: owner})
	execute := func(r *firingReplica, ctx context.Context, query string) {
		t.Helper()
		if _, err := r.eng.Execute(ctx, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	execute(a, system, fmt.Sprintf(`insert("v1:identity:user", id=%s, payload={"displayName":"Rule author","primaryEmail":%s,"role":"owner","active":true})`,
		q(short), q(ownerEmail)))
	execute(a, ownerCtx, fmt.Sprintf(`mutation createClientAccount(accountId: %s, name: "Firing organization")`, q(org)))
	execute(a, auth.ContextWithInternalOrigin(ownerCtx), fmt.Sprintf(`mutation createTemplate(versionTime: "2026-10-01T00:00:00Z", templateId: %s, name: "Firing template", subject: %s, textBody: "A todo was added.", accountId: %s)`,
		q(template), q(subject), q(org)))
	execute(a, ownerCtx, fmt.Sprintf(`mutation createEmailRule(emailRuleId: %s, name: "Tell me about new todos", triggerConcept: "v1:todos:todo", eventKind: "created", templateId: %s, recipientMode: "cluster_roles", accountId: %s)`,
		q(rule), q(template), q(org)))

	// --- Arm on node A; the arm broadcast arms node B ---
	activator := emailrules.NewActivator(emailrules.EngineAdapter{Engine: a.eng}, a.deps)
	armed, err := activator.Activate(ownerCtx, owner, rule)
	if err != nil {
		t.Fatalf("activate the rule on node A: %v", err)
	}
	t.Cleanup(func() { _, _ = activator.Retire(ownerCtx, owner, rule) })
	if armed.Status != "active" {
		t.Fatalf("the rule activated as %q, want active (%+v)", armed.Status, armed)
	}
	name := emailrules.ConstructNameFor(rule)
	var armedOwner string
	for _, c := range a.registry.ListAll() {
		if c.Kind == "automation" && c.Name == name {
			armedOwner = c.OwnerUserId
		}
	}
	if armedOwner == "" || !a.scheduler.IsActive(armedOwner, name) {
		t.Fatalf("node A activated the rule but does not hold automation %s", name)
	}
	if !armingEventually(15*time.Second, func() bool { return b.scheduler.IsActive(armedOwner, name) }) {
		t.Fatal("setup: node B never armed the rule node A activated")
	}

	facts := func() firingFacts { return readFiringFacts(t, a.db, rule, name, ownerEmail, subject) }
	waitFired := func(want int64) firingFacts {
		t.Helper()
		var f firingFacts
		armingEventually(20*time.Second, func() bool { f = facts(); return f.firedCount >= want })
		// Settle: a second firing would land right behind the first.
		time.Sleep(500 * time.Millisecond)
		return facts()
	}
	createTodo := func(r *firingReplica, id string) {
		t.Helper()
		execute(r, ownerCtx, fmt.Sprintf(`mutation createTodo(todoId: %s, title: "Water the plants")`, q(id)))
	}
	const todoTopic = "graph.node.created.v1:todos:todo"

	// --- A write on node B: the node that does NOT own the owner ---
	createTodo(b, "fire-on-b-"+suffix)
	got := waitFired(1)
	if got.firedCount != 1 {
		t.Fatalf("node B's write fired the rule %d times, want 1: the write happened on B and no other node hears it "+
			"(todo events crossed the link: %d)", got.firedCount, link.count(todoTopic))
	}
	if link.count(todoTopic) != 0 {
		t.Fatalf("a todo event crossed the mesh (%d); this test needs a trigger the mesh does not forward", link.count(todoTopic))
	}
	if b.read(b.ran, name) != 1 || a.read(a.ran, name) != 0 {
		t.Fatalf("node B's write ran the rule %d times on B and %d on A, want once, on B", b.read(b.ran, name), a.read(a.ran, name))
	}
	if got.firstClaims != 1 || got.runClaims != 1 || got.deliveries != 1 {
		t.Fatalf("one firing should leave one first-fire claim, one execution claim and one delivery; got %+v", got)
	}

	// --- A write on node A: the owner's node ---
	createTodo(a, "fire-on-a-"+suffix)
	got = waitFired(2)
	if got.firedCount != 2 || a.read(a.ran, name) != 1 || b.read(b.ran, name) != 1 {
		t.Fatalf("node A's write fired the rule to a count of %d (ran %d on A, %d on B), want 2, once on A",
			got.firedCount, a.read(a.ran, name), b.read(b.ran, name))
	}
	if got.firstClaims != 2 || got.runClaims != 2 || got.deliveries != 2 {
		t.Fatalf("two firings should leave two of each claim and two deliveries; got %+v", got)
	}
	if a.read(a.gated, armedOwner) != 0 || b.read(b.gated, armedOwner) != 0 {
		t.Fatalf("the owner gate was consulted for an event trigger (A %d, B %d); it decides where schedules run",
			a.read(a.gated, armedOwner), b.read(b.gated, armedOwner))
	}

	// --- One event both nodes observe as local: the claim admits one ---
	// The same event, built twice so the two nodes share no map: what a write
	// would publish, observed as local on each node.
	both := "v1:todos:todo:fire-on-both-" + suffix
	sameEvent := func() events.Event {
		return events.NewEvent(todoTopic, events.KindNodeCreated, map[string]any{
			"id": both, "nodeId": both, "concept": "v1:todos:todo", "nodeType": "object",
			"title": "Water the plants", "firstVersion": true,
			"payload": map[string]any{"title": "Water the plants"},
		})
	}
	claimedBefore := a.guard.Claimed() + b.guard.Claimed()
	preventedBefore := a.guard.DuplicatesPrevented() + b.guard.DuplicatesPrevented()
	var wg sync.WaitGroup
	for _, r := range []*firingReplica{a, b} {
		wg.Add(1)
		go func(bus *events.Bus, ev events.Event) { defer wg.Done(); bus.PublishSync(ev) }(r.bus, sameEvent())
	}
	wg.Wait()
	got = waitFired(3)
	claimed := a.guard.Claimed() + b.guard.Claimed() - claimedBefore
	prevented := a.guard.DuplicatesPrevented() + b.guard.DuplicatesPrevented() - preventedBefore
	if got.firedCount != 3 || claimed != 1 || prevented != 1 {
		t.Fatalf("one event observed on both nodes: fired count %d (want 3), execution claims won %d and duplicates prevented %d "+
			"(want 1 and 1)", got.firedCount, claimed, prevented)
	}
	if got.runClaims != 3 || got.deliveries != 3 {
		t.Fatalf("one event observed on both nodes left %+v; want one more execution claim and one more delivery", got)
	}

	// --- A rule that keeps failing on node B is paused on every replica ---
	b.failRuns.Store(true)
	for i := 0; i < automations.DefaultBreakerThreshold; i++ {
		createTodo(b, fmt.Sprintf("fail-on-b-%d-%s", i, suffix))
	}
	if !armingEventually(20*time.Second, func() bool {
		return !a.scheduler.IsActive(armedOwner, name) && !b.scheduler.IsActive(armedOwner, name)
	}) {
		b.mu.Lock()
		trips := b.trips
		b.mu.Unlock()
		t.Fatalf("CROSS-NODE FAILURE: a rule whose breaker tripped on B is still armed (A %v, B %v; trips on B %v; "+
			"disarms that crossed the link %d)", a.scheduler.IsActive(armedOwner, name), b.scheduler.IsActive(armedOwner, name),
			trips, link.count(events.TopicAuthoringDisarmForBundle(armed.BundleID)))
	}
	var trips []error
	armingEventually(10*time.Second, func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		trips = append([]error(nil), b.trips...)
		return len(trips) > 0
	})
	b.mu.Lock()
	outcomes := append([]memql.AuthoredTripOutcome(nil), b.tripOutcomes...)
	b.mu.Unlock()
	if len(trips) != 1 || trips[0] != nil || len(outcomes) != 1 || outcomes[0] != memql.TripPaused {
		t.Fatalf("the trip on node B did %v with %v, want one clean pause", outcomes, trips)
	}
	status, reason := readBundleRow(t, a.db, armed.BundleID)
	if status != "paused" || !strings.Contains(reason, "failed runs in a row") {
		t.Fatalf("the tripped rule's bundle reads %q (%q); a reboot or a reconcile pass would arm it again", status, reason)
	}
	if err := a.eng.ReconcileAuthoredRuntime(ctx); err != nil {
		t.Fatalf("reconcile node A: %v", err)
	}
	if a.scheduler.IsActive(armedOwner, name) {
		t.Fatal("a reconcile pass on node A armed a rule whose breaker tripped")
	}
	b.failRuns.Store(false)
	createTodo(a, "after-trip-"+suffix)
	time.Sleep(time.Second)
	if got := facts(); got.firedCount != 3 {
		t.Fatalf("a paused rule fired: count %d, want the 3 from before the trip", got.firedCount)
	}

	// --- The rule's own row says it was stopped, and why ---
	rs, re, rb := readRuleRow(t, a.db, rule)
	if rs != "paused" || re != emailrules.BreakerStoppedSentence || memql.BareShortId(rb) != memql.BareShortId(armed.BundleID) {
		t.Fatalf("after the trip the rule row reads status %q, lastError %q, bundleId %q; want paused, %q, the paused bundle %s",
			rs, re, rb, emailrules.BreakerStoppedSentence, armed.BundleID)
	}

	// --- Turned off and on again, the rule comes back clean and fires ---
	if _, err := activator.Retire(ownerCtx, owner, rule); err != nil {
		t.Fatalf("turn the rule off: %v", err)
	}
	if status, _ := readBundleRow(t, a.db, armed.BundleID); status != "retired" {
		t.Fatalf("turning the rule off left its paused bundle %q", status)
	}
	again, err := activator.Activate(ownerCtx, owner, rule)
	if err != nil || again.Status != "active" {
		t.Fatalf("turn the rule on again: %+v, %v", again, err)
	}
	// Version 1 again, so the same bundle id: the trip's reason must not ride
	// along into the live bundle.
	if status, reason := readBundleRow(t, a.db, again.BundleID); status != "active" || reason != "" {
		t.Fatalf("the re-activated bundle reads %q with failureReason %q; an active bundle carries no failure reason", status, reason)
	}
	if rs, re, _ := readRuleRow(t, a.db, rule); rs != "active" || re != "" {
		t.Fatalf("the re-activated rule reads %q with lastError %q", rs, re)
	}
	if !armingEventually(15*time.Second, func() bool { return b.scheduler.IsActive(armedOwner, name) }) {
		t.Fatal("node B never armed the rule turned on again")
	}
	createTodo(b, "after-turn-on-"+suffix)
	if got := waitFired(4); got.firedCount != 4 {
		t.Fatalf("the rule turned on again fired to a count of %d, want 4", got.firedCount)
	}
}

func readRuleRow(t *testing.T, db *bun.DB, rule string) (status, lastError, bundleID string) {
	t.Helper()
	err := db.QueryRowContext(context.Background(), `
		SELECT COALESCE(payload->>'status', ''), COALESCE(payload->>'lastError', ''), COALESCE(payload->>'bundleId', '')
		  FROM "MemoryNodes" WHERE id = ? ORDER BY "createdAt" DESC LIMIT 1`,
		"v1:campaigns:emailRule:"+rule).Scan(&status, &lastError, &bundleID)
	if err != nil {
		t.Fatalf("read the rule row: %v", err)
	}
	return status, lastError, bundleID
}
