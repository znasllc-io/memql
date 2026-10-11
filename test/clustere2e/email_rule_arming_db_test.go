package clustere2e

// email_rule_arming_db_test.go -- an email rule armed on one replica fires on
// another (memql#6022, F1).
//
// # WHAT WAS BROKEN
//
// Activating an email rule registers its generated automation into the
// authored runtime of the node that handled the activation, and nowhere else.
// Every other replica learned of the rule at its next boot, through the boot
// re-arm. The authored owner gate runs an owner's automations on exactly ONE
// node of the mesh, so whenever that node was not the one the activation
// landed on, a rule somebody saved simply did not fire -- and a rule somebody
// retired kept firing on every replica that had re-armed it at boot.
//
// # WHAT THIS PROVES
//
// Two engines over one Postgres and nothing else shared: each has its own
// pool, its own result cache, its own bus, its own authored registry and its
// own authored scheduler. The buses are joined by newMeshLink, which forwards
// an event only when node.ForwardDecisionFor -- the decision EventBridge
// applies -- says it may, and round-trips its payload through structpb as the
// wire does. The owner gate names node B, so the rule can only ever fire on B,
// and it is activated on node A. B arms it only if the authoring.arm broadcast
// crosses, and B reads the bundle it names out of the shared database.
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
// failure.
//
// # HOW TO CONFIRM IT IS LOAD-BEARING
//
// Delete the authoring.arm.* rule from component/node/routing.go: the arm
// event never crosses, B never arms, and the test fails at "never armed".
// Delete the authoring.disarm.* rule instead and it fails at "kept the rule
// armed" after the retire.

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync/atomic"
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

// armingReplica is one node: its own pool, engine, bus, authored registry and
// authored scheduler. The scheduler's Run hook counts firings rather than
// executing the rule's step, because the claim under test is WHERE the rule
// fires, not what the fire builtin then sends.
type armingReplica struct {
	bus       *events.Bus
	eng       *memql.MemQLEngine
	registry  *memql.AuthoredRuntimeRegistry
	scheduler *automations.AuthoredScheduler
	deps      func() memql.AuthoredRuntimeDeps

	fired atomic.Int64 // firings that ran
	gated atomic.Int64 // firings that reached the owner gate, ran or not
}

func newArmingReplica(t *testing.T, runsTheOwner bool) *armingReplica {
	t.Helper()
	dsn := dbtest.DSN()
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

	r := &armingReplica{bus: events.NewBus(), eng: eng, registry: memql.NewAuthoredRuntimeRegistry()}
	t.Cleanup(r.bus.Close)
	eng.SetEventBus(r.bus)

	scheduler, err := automations.NewAuthoredScheduler(automations.AuthoredSchedulerOptions{
		Loader:   automations.NewLoader(automations.LoaderOptions{Registry: memorynodes.DefaultRegistry()}),
		EventBus: r.bus,
		Run: func(context.Context, *automations.Automation, *events.Event) error {
			r.fired.Add(1)
			return nil
		},
		// The owner gate runs an owner's authored automations on ONE node of
		// the mesh. Counting its consultations is what lets the test say the
		// node that did not fire was asked, and declined.
		OwnerGate: func(string) bool {
			r.gated.Add(1)
			return runsTheOwner
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(scheduler.Stop)
	r.scheduler = scheduler

	deps := memql.AuthoredRuntimeDeps{
		Registry:       r.registry,
		RegisterHook:   scheduler.Activate,
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
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatal(err)
	}
	a := newArmingReplica(t, false)
	b := newArmingReplica(t, true)
	if a == nil || b == nil {
		return
	}
	link := newMeshLink(t, "node-a", a.bus, "node-b", b.bus)

	// --- Seed: an owner, an organization, a template in it and a rule in it ---
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	short, org, template, rule := "arm-user-"+suffix, "arm-org-"+suffix, "arm-tpl-"+suffix, "arm-rule-"+suffix
	// Canonical, as the identity resolver stamps every real caller: the
	// activation compares it with the bundle's stored ownerUserId.
	owner := "v1:identity:user:" + short
	q := langparser.QuoteString

	system := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), auth.SystemActor("email-rule-arming-test")))
	system = auth.ContextWithToken(system, &auth.TokenInfo{Subject: "email-rule-arming-test"})
	// Activation is owner-or-developer only (auth.CanAuthor).
	ownerCtx := auth.ContextWithToken(auth.ContextWithAccess(context.Background(),
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
	execute(ownerCtx, fmt.Sprintf(`mutation createEmailRule(emailRuleId: %s, name: "Arming rule", triggerConcept: "v1:identity:user", eventKind: "created", templateId: %s, recipientMode: "cluster_roles", accountId: %s)`,
		q(rule), q(template), q(org)))

	// --- Arm on node A ---
	activator := emailrules.NewActivator(emailrules.EngineAdapter{Engine: a.eng}, a.deps)
	res, err := activator.Activate(ownerCtx, owner, rule)
	if err != nil {
		t.Fatalf("activate the rule on node A: %v", err)
	}
	if res.Status != "active" {
		t.Fatalf("the rule activated as %q, want active (%+v)", res.Status, res)
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

	if !armingEventually(15*time.Second, func() bool { return b.scheduler.IsActive(armedOwner, name) }) {
		t.Fatalf("CROSS-NODE FAILURE: node B never armed the rule node A activated "+
			"(authoring.arm broadcasts that crossed the link: %d). Without the arm broadcast B learns "+
			"of a rule at its next boot, and the owner gate names B, so the rule does not fire at all.",
			link.count(events.TopicAuthoringArmForBundle(res.BundleID)))
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
	firedA, firedB, gatedA := a.fired.Load(), b.fired.Load(), a.gated.Load()
	created("arm-newcomer-" + suffix)
	if got := b.fired.Load() - firedB; got != 1 {
		t.Fatalf("node B fired the rule %d times for one created user, want 1", got)
	}
	// The event also crosses to A (graph.node.created.v1:identity:user is
	// broadcast). A holds the rule too, and its gate must decline it.
	if !armingEventually(5*time.Second, func() bool { return a.gated.Load() > gatedA }) {
		t.Fatal("node A never consulted its owner gate for the forwarded event, so its zero firings prove nothing")
	}
	if got := a.fired.Load() - firedA; got != 0 {
		t.Fatalf("node A fired the rule %d times; the owner gate names node B", got)
	}

	// --- Retire on node A ---
	if _, err := activator.Retire(ownerCtx, owner, rule); err != nil {
		t.Fatalf("retire the rule on node A: %v", err)
	}
	if a.scheduler.IsActive(armedOwner, name) {
		t.Fatal("node A retired the rule but kept it scheduled")
	}
	if !armingEventually(15*time.Second, func() bool { return !b.scheduler.IsActive(armedOwner, name) }) {
		t.Fatalf("CROSS-NODE FAILURE: node B kept the rule armed after node A retired it "+
			"(authoring.disarm broadcasts that crossed the link: %d)",
			link.count(events.TopicAuthoringDisarmForBundle(res.BundleID)))
	}
	if _, ok := b.registry.Lookup(armedOwner, "automation", name); ok {
		t.Fatal("node B stopped scheduling the rule but kept it in its authored registry")
	}

	created("arm-latecomer-" + suffix)
	if got := b.fired.Load() - firedB; got != 1 {
		t.Fatalf("node B fired a retired rule: %d firings in all, want the 1 from before the retire", got)
	}
}
