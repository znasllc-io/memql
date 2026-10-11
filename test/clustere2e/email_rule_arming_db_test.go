package clustere2e

// email_rule_arming_db_test.go -- an email rule armed on one replica fires on
// another, follows an edit there, and stops there when retired (memql#6022, F1).
//
// # WHAT WAS BROKEN
//
// Activating an email rule registers its generated automation into the
// authored runtime of the node that handled the activation, and nowhere else.
// Every other replica learned of the rule, and of its retirement, only by
// restarting. The authored owner gate runs an owner's automations on exactly
// ONE node of the mesh, so whenever that node was not the one the activation
// landed on, a rule somebody saved simply did not fire -- and a rule somebody
// retired kept firing on every replica that had armed it at boot.
//
// # WHAT THIS PROVES
//
// Two engines over one Postgres and nothing else shared: each has its own
// pool, its own result cache, its own bus, its own authored registry and its
// own authored scheduler. The buses are joined by newMeshLink, which forwards
// an event only when node.ForwardDecisionFor -- the decision EventBridge
// applies -- says it may, and round-trips its payload through structpb as the
// wire does. The owner gate names node B, so the rule can only ever fire on B,
// and every change to it is made on node A. The test proves, against real
// rows:
//
//   - the arm broadcast crosses, and B arms the bundle it names;
//   - a reconcile pass on B leaves a node that already agrees alone, and
//     re-arms a rule whose arm B lost;
//   - the rule fires on B and not on A;
//   - an edit on A moves B to the new version, and a late disarm of the old
//     version leaves the new one armed (B reads the new bundle, by id, as
//     active);
//   - the retire broadcast crosses, and B stops firing the rule.
//
// The trigger is published on B, the gate node: arbitrary-concept trigger
// events are not forwarded across nodes (plan Ruling 13), so the event has to
// originate where the rule runs.
//
// # WHY THIS FILE CARRIES NO BUILD TAG
//
// The rest of this package is `//go:build clustere2e` and needs a live
// cluster; a gate that is skipped wherever no cluster runs cannot be what
// stands between this fix and the bug. This file is Postgres-gated instead:
// test/clustere2e is in DB_GATED_TREES (scripts/ci/db-gated-packages.sh), so
// CI's db-tests lane runs it with MEMQL_REQUIRE_DB=1, where a skip is a
// failure. It writes users, rules and bundles, so it runs ONLY against a
// database named in MEMQL_DATABASE_DSN -- never dbtest's default, which in this
// package is the developer's local cluster (see main_dbschema_test.go).
//
// # HOW TO CONFIRM IT IS LOAD-BEARING
//
// Delete the authoring.arm.* rule from component/node/routing.go and the arm
// never crosses ("the arm broadcast never crossed"). Delete the
// authoring.disarm.* rule and the retire never does ("kept the rule armed").

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/emailrules"
	"github.com/znasllc-io/memql/component/events"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// armingDSN is the database this test may write to: one named explicitly, or
// none at all.
func armingDSN(t *testing.T) string {
	t.Helper()
	if dsn := strings.TrimSpace(os.Getenv(databaseDSNEnv)); dsn != "" {
		return dsn
	}
	if dbtest.RequireDB() {
		t.Fatalf("%s=1 but %s is unset; this test writes users, rules and bundles, so it needs a database named explicitly",
			dbtest.RequireDBEnv, databaseDSNEnv)
	}
	t.Skipf("%s is unset: this test writes users, rules and bundles, so it runs only against a database named "+
		"explicitly -- never dbtest's default (localhost:5432), which is the developer's local cluster", databaseDSNEnv)
	return ""
}

// armingReplica is one node: its own pool, engine, bus, authored registry and
// authored scheduler. The scheduler's Run hook counts firings rather than
// executing the rule's step, because the claim under test is WHERE the rule
// fires, not what the fire builtin then sends.
//
// Every counter is keyed by automation name or owner. A reconcile pass arms
// EVERY active bundle in the database, and the db-tests lane shares one
// database between packages, so a node here may hold another test's
// automations too.
type armingReplica struct {
	bus       *events.Bus
	eng       *memql.MemQLEngine
	registry  *memql.AuthoredRuntimeRegistry
	scheduler *automations.AuthoredScheduler
	deps      func() memql.AuthoredRuntimeDeps

	mu        sync.Mutex
	fired     map[string]int // automation name -> firings that ran
	gated     map[string]int // owner -> firings that reached the owner gate
	scheduled map[string]int // automation name -> times handed to the scheduler
}

func (r *armingReplica) bump(counter map[string]int, key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	counter[key]++
}

func (r *armingReplica) read(counter map[string]int, key string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return counter[key]
}

func newArmingReplica(t *testing.T, dsn string, runsTheOwner bool) *armingReplica {
	t.Helper()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	ping, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ping); err != nil {
		_ = db.Close()
		dbtest.Unreachable(t, "email rule arming across replicas", dsn, err)
		return nil
	}
	t.Cleanup(func() { _ = db.Close() })

	eng, err := memql.New(db)
	if err != nil {
		t.Fatal(err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(memorynodes.DefaultRegistry()); err != nil {
		t.Fatal(err)
	}

	r := &armingReplica{
		bus: events.NewBus(), eng: eng, registry: memql.NewAuthoredRuntimeRegistry(),
		fired: map[string]int{}, gated: map[string]int{}, scheduled: map[string]int{},
	}
	t.Cleanup(r.bus.Close)
	eng.SetEventBus(r.bus)

	scheduler, err := automations.NewAuthoredScheduler(automations.AuthoredSchedulerOptions{
		Loader:   automations.NewLoader(automations.LoaderOptions{Registry: memorynodes.DefaultRegistry()}),
		EventBus: r.bus,
		Run: func(_ context.Context, a *automations.Automation, _ *events.Event) error {
			r.bump(r.fired, a.Name)
			return nil
		},
		// The owner gate runs an owner's authored automations on ONE node of
		// the mesh. Counting its consultations is what lets the test say the
		// node that did not fire was asked, and declined.
		OwnerGate: func(owner string) bool {
			r.bump(r.gated, owner)
			return runsTheOwner
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(scheduler.Stop)
	r.scheduler = scheduler

	deps := memql.AuthoredRuntimeDeps{
		Registry: r.registry,
		RegisterHook: func(c *memql.AuthoredConstruct) error {
			r.bump(r.scheduled, c.Name)
			return scheduler.Activate(c)
		},
		DeregisterHook: scheduler.Deactivate,
	}
	r.deps = func() memql.AuthoredRuntimeDeps { return deps }
	eng.SetAuthoredRuntimeDeps(r.deps)

	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	eng.StartAuthoringArmSubscriber(ctx)
	return r
}

// armingEventually polls cond until it holds or the timeout elapses.
func armingEventually(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

func TestAnEmailRuleArmedOnOneReplicaFiresOnAnother(t *testing.T) {
	dsn := armingDSN(t)
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	a := newArmingReplica(t, dsn, false)
	b := newArmingReplica(t, dsn, true)
	if a == nil || b == nil {
		return
	}
	link := newMeshLink(t, "node-a", a.bus, "node-b", b.bus)
	ctx := context.Background()

	// --- Seed: an owner, an organization, a template in it and a rule in it ---
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	short, org, template, rule := "arm-user-"+suffix, "arm-org-"+suffix, "arm-tpl-"+suffix, "arm-rule-"+suffix
	// Canonical, as the identity resolver stamps every real caller: the
	// activation compares it with the bundle's stored ownerUserId.
	owner := "v1:identity:user:" + short
	q := langparser.QuoteString

	system := auth.ContextWithInternalOrigin(auth.ContextWithAccess(ctx, auth.SystemActor("email-rule-arming-test")))
	system = auth.ContextWithToken(system, &auth.TokenInfo{Subject: "email-rule-arming-test"})
	// Activation is owner-or-developer only (auth.CanAuthor).
	ownerCtx := auth.ContextWithToken(auth.ContextWithAccess(ctx,
		&auth.AccessContext{UserId: owner, Role: auth.RoleOwner}), &auth.TokenInfo{Subject: owner})
	execute := func(ctx context.Context, query string) {
		t.Helper()
		if _, err := a.eng.Execute(ctx, query); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	execute(system, fmt.Sprintf(`insert("v1:identity:user", id=%s, payload={"displayName":"Rule author","primaryEmail":"%s@example.test","role":"owner","active":true})`,
		q(short), short))
	execute(ownerCtx, fmt.Sprintf(`mutation createClientAccount(accountId: %s, name: "Arming organization")`, q(org)))
	// Server-only, so internal origin. Arming reads the template; firing is
	// counted by the Run hook, so it need not be published.
	execute(auth.ContextWithInternalOrigin(ownerCtx), fmt.Sprintf(`mutation createTemplate(versionTime: "2026-10-01T00:00:00Z", templateId: %s, name: "Arming template", subject: "Hello", textBody: "Hello", accountId: %s)`,
		q(template), q(org)))
	ruleForm := func(name string) string {
		return fmt.Sprintf(`emailRuleId: %s, name: %s, triggerConcept: "v1:identity:user", eventKind: "created", templateId: %s, recipientMode: "cluster_roles", accountId: %s`,
			q(rule), q(name), q(template), q(org))
	}
	execute(ownerCtx, "mutation createEmailRule("+ruleForm("Arming rule")+")")

	// --- Arm on node A ---
	activator := emailrules.NewActivator(emailrules.EngineAdapter{Engine: a.eng}, a.deps)
	first, err := activator.Activate(ownerCtx, owner, rule)
	if err != nil {
		t.Fatalf("activate the rule on node A: %v", err)
	}
	// Whatever happens below, the rule does not outlive the test: the db-tests
	// lane shares this database, and every other package's reconcile pass
	// would arm an active bundle it finds there.
	t.Cleanup(func() { _, _ = activator.Retire(ownerCtx, owner, rule) })
	if first.Status != "active" {
		t.Fatalf("the rule activated as %q, want active (%+v)", first.Status, first)
	}

	// The owner the construct is registered under is the bundle row's
	// ownerUserId. Read it off node A's registry rather than composing it.
	name := emailrules.ConstructNameFor(rule)
	var armedOwner string
	for _, c := range a.registry.ListAll() {
		if c.Kind == "automation" && c.Name == name {
			armedOwner = c.OwnerUserId
		}
	}
	if armedOwner == "" {
		t.Fatalf("node A activated the rule but its registry holds no automation %s", name)
	}
	if !a.scheduler.IsActive(armedOwner, name) {
		t.Fatal("node A activated the rule but did not schedule it")
	}

	armTopic := events.TopicAuthoringArmForBundle(first.BundleID)
	if !armingEventually(15*time.Second, func() bool { return b.scheduler.IsActive(armedOwner, name) }) {
		t.Fatalf("CROSS-NODE FAILURE: node B never armed the rule node A activated "+
			"(authoring.arm broadcasts that crossed the link: %d). Without the arm broadcast B learns "+
			"of a rule at its next reconcile pass, and the owner gate names B, so the rule does not fire until then.",
			link.count(armTopic))
	}
	// B's own reconcile pass, which starts with its subscriber, could have
	// armed the rule as well; the broadcast must have crossed regardless.
	if link.count(armTopic) == 0 {
		t.Fatal("CROSS-NODE FAILURE: the arm broadcast never crossed the link; node B armed the rule only by reconciling")
	}

	// --- Reconcile on node B, against real rows ---
	scheduledOnB := b.read(b.scheduled, name)
	if err := b.eng.ReconcileAuthoredRuntime(ctx); err != nil {
		t.Fatalf("reconcile node B: %v", err)
	}
	if !b.scheduler.IsActive(armedOwner, name) || b.read(b.scheduled, name) != scheduledOnB {
		t.Fatalf("a reconcile pass over a node that already holds the rule changed it (scheduled %d -> %d, active %v): "+
			"the bundle it lists and the entry it holds must read as the same construct",
			scheduledOnB, b.read(b.scheduled, name), b.scheduler.IsActive(armedOwner, name))
	}
	// A lost arm: B loses the rule, and the next pass restores it from the
	// database.
	b.scheduler.Deactivate(armedOwner, name)
	if err := b.registry.Retire(armedOwner, "automation", name); err != nil {
		t.Fatalf("setup: drop the rule from node B: %v", err)
	}
	if err := b.eng.ReconcileAuthoredRuntime(ctx); err != nil {
		t.Fatalf("reconcile node B after a lost arm: %v", err)
	}
	if !b.scheduler.IsActive(armedOwner, name) {
		t.Fatal("a reconcile pass did not re-arm the rule node B had lost")
	}

	// --- Fire on node B, the gate node ---
	created := func(person string) {
		id := "v1:identity:user:" + person
		email := person + "@example.test"
		payload := map[string]any{
			"id": id, "nodeId": id, "concept": "v1:identity:user", "nodeType": "node",
			"primaryEmail": email, "payload": map[string]any{"primaryEmail": email},
			"firstVersion": true,
		}
		b.bus.PublishSync(events.NewEvent("graph.node.created.v1:identity:user", events.KindNodeCreated, payload))
	}
	firedA, firedB, gatedA := a.read(a.fired, name), b.read(b.fired, name), a.read(a.gated, armedOwner)
	created("arm-newcomer-" + suffix)
	if got := b.read(b.fired, name) - firedB; got != 1 {
		t.Fatalf("node B fired the rule %d times for one created user, want 1", got)
	}
	// The event also crosses to A (graph.node.created.v1:identity:user is
	// broadcast). A holds the rule too, and its gate must decline it.
	if !armingEventually(5*time.Second, func() bool { return a.read(a.gated, armedOwner) > gatedA }) {
		t.Fatal("node A never consulted its owner gate for the forwarded event, so its zero firings prove nothing")
	}
	if got := a.read(a.fired, name) - firedA; got != 0 {
		t.Fatalf("node A fired the rule %d times; the owner gate names node B", got)
	}

	// --- Edit on node A: version 2 supersedes version 1 ---
	execute(ownerCtx, "mutation updateEmailRule("+ruleForm("Arming rule, edited")+")")
	second, err := activator.Activate(ownerCtx, owner, rule)
	if err != nil {
		t.Fatalf("re-activate the edited rule on node A: %v", err)
	}
	if second.Status != "active" || second.BundleID == first.BundleID {
		t.Fatalf("the edit should arm a new bundle: %+v (first bundle %q)", second, first.BundleID)
	}
	onVersion2 := func() bool {
		c, ok := b.registry.Lookup(armedOwner, "automation", name)
		return ok && c.Version == 2 && b.scheduler.IsActive(armedOwner, name)
	}
	if !armingEventually(15*time.Second, onVersion2) {
		c, _ := b.registry.Lookup(armedOwner, "automation", name)
		t.Fatalf("node B did not follow the edit to version 2: holds %+v", c)
	}
	// A late disarm for version 1 -- a retired bundle defining the same name --
	// must leave version 2 armed: B reads version 2's bundle, by id, as active.
	late := events.NewEvent(events.TopicAuthoringDisarmForBundle(first.BundleID), events.KindAuthoringDisarm,
		map[string]any{"bundleId": first.BundleID, "owner": armedOwner})
	late.OriginNodeId = "node-a"
	b.bus.PublishSync(late)
	if !onVersion2() {
		c, _ := b.registry.Lookup(armedOwner, "automation", name)
		t.Fatalf("a late disarm of version 1 took version 2 down on node B: holds %+v, active %v",
			c, b.scheduler.IsActive(armedOwner, name))
	}

	// --- Retire on node A ---
	if _, err := activator.Retire(ownerCtx, owner, rule); err != nil {
		t.Fatalf("retire the rule on node A: %v", err)
	}
	if a.scheduler.IsActive(armedOwner, name) {
		t.Fatal("node A retired the rule but kept it scheduled")
	}
	disarmTopic := events.TopicAuthoringDisarmForBundle(second.BundleID)
	if !armingEventually(15*time.Second, func() bool { return !b.scheduler.IsActive(armedOwner, name) }) {
		t.Fatalf("CROSS-NODE FAILURE: node B kept the rule armed after node A retired it "+
			"(authoring.disarm broadcasts that crossed the link: %d)", link.count(disarmTopic))
	}
	if _, ok := b.registry.Lookup(armedOwner, "automation", name); ok {
		t.Fatal("node B stopped scheduling the rule but kept it in its authored registry")
	}

	created("arm-latecomer-" + suffix)
	if got := b.read(b.fired, name) - firedB; got != 1 {
		t.Fatalf("node B fired a retired rule: %d firings in all, want the 1 from before the retire", got)
	}
}
