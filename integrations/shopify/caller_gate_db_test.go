package shopify

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// caller_gate_db_test.go -- the caller gates against a real engine over a
// real Postgres: the new builtin through the engine's own capability check,
// the store row's gate it does not widen, and the two scheduled passes run
// by their automations.

// clientCall is a signed-in person's call as the wire stamps it: client
// origin, their real role.
func clientCall(userID string, role auth.Role) context.Context {
	return auth.ContextWithClientOrigin(actorCtx(userID, role))
}

func TestAStoresOwnerWhoIsNotAClusterOwnerPausesAndResumesIt(t *testing.T) {
	eng, db := authorityEngine(t)
	if eng == nil {
		return
	}
	t.Setenv("MEMQL_DOMAIN", "example.test")
	suffix := fmt.Sprint(time.Now().UnixNano())
	storeID, owner := "dbtest-recording-"+suffix, "dbtest-recording-owner-"+suffix
	// Seeded the way Connect Shopify leaves a store: createStore's status,
	// configured, which nothing moves to live.
	seedOwnedStore(t, eng, db, storeID, owner)
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept = 'v1:identity:auditEvent' AND payload->>'targetId' = $1`, storeID)
	})
	audits := func() [][2]string {
		t.Helper()
		rows, err := db.QueryContext(context.Background(),
			`SELECT payload->>'action', payload->>'actorUserId' FROM "MemoryNodes" WHERE concept = 'v1:identity:auditEvent' AND payload->>'targetId' = $1 ORDER BY "createdAt"`, storeID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out [][2]string
		for rows.Next() {
			var action, actor string
			if err := rows.Scan(&action, &actor); err != nil {
				t.Fatal(err)
			}
			// The engine stores the actor canonical; the person is the bare id.
			out = append(out, [2]string{action, memql.BareShortId(actor)})
		}
		return out
	}

	registry := NewStoreRegistry(eng, nil)
	registry.ttl = 0
	c := NewConnector(eng, slog.New(slog.DiscardHandler), registry, NewAdminClient())
	now := time.Now().UTC().Truncate(time.Second)
	c.now = func() time.Time { return now }
	if err := eng.RegisterIntegration(NewIntegration(c)); err != nil {
		t.Fatal(err)
	}
	// The sync runtime, as on a node: a resume returns the store's held
	// deliveries and queues its unstarted backfills through its builtins.
	syncRuntimeOn(t, eng, c)
	cleanStoreRows(t, db, storeID)
	ctx := context.Background()
	status := func() string {
		t.Helper()
		s, found, err := c.storeByID(memql.ContextWithFreshRead(operatorContext(ctx)), storeID)
		if err != nil || !found {
			t.Fatalf("read the store: found %t, %v", found, err)
		}
		return s.Status
	}
	// The store's owner, a developer: holds the store part, and is not a
	// cluster owner.
	person := clientCall(owner, auth.RoleDeveloper)
	set := func(recording bool) map[string]any {
		t.Helper()
		return builtinReply(t, eng, person, "builtin "+renderCall("shopifyStoreSetRecording", map[string]any{"storeId": storeID, "recording": recording}))
	}

	if got := status(); got != StatusConfigured {
		t.Fatalf("a newly connected store reads %q, want configured", got)
	}
	// The store row's own gate is not widened: the owner's own setStoreStatus
	// still changes nothing.
	_, _ = eng.Execute(person, renderCall("setStoreStatus", map[string]any{"storeId": storeID, "status": StatusPaused}))
	if got := status(); got != StatusConfigured {
		t.Fatalf("the owner's own setStoreStatus moved the store to %q; the row's gate was widened", got)
	}

	// Somebody else holding the same part is refused, and nothing moves.
	if _, err := eng.Execute(clientCall("dbtest-recording-other-"+suffix, auth.RoleDeveloper),
		"builtin "+renderCall("shopifyStoreSetRecording", map[string]any{"storeId": storeID, "recording": false})); err == nil ||
		!strings.Contains(err.Error(), "shopify: shopifyStoreSetRecording "+actRefusal) {
		t.Fatalf("another developer: err = %v, want the refusal", err)
	}
	if got := status(); got != StatusConfigured {
		t.Fatalf("a refused pause moved the store to %q", got)
	}
	if got := audits(); len(got) != 0 {
		t.Fatalf("a refusal was audited: %v", got)
	}

	if reply := set(false); reply["status"] != StatusPaused || reply["storeId"] != storeID {
		t.Errorf("pause answered %v", reply)
	}
	if got := status(); got != StatusPaused {
		t.Fatalf("after Pause recording the row reads %q, want paused", got)
	}
	// Asking again writes nothing, and audits nothing.
	set(false)
	if got := audits(); len(got) != 1 || got[0] != [2]string{"shopify_store_paused", owner} {
		t.Fatalf("after a pause and a repeated pause the audit trail reads %v, want one pause by %s", got, owner)
	}

	// The watch records the paused verdict, as its pass would.
	store, _, _ := c.storeByID(memql.ContextWithFreshRead(operatorContext(ctx)), storeID)
	paused, err := c.storeVerdict(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	if paused.State != VerdictPaused {
		t.Fatalf("the verdict after a pause is %q, want paused", paused.State)
	}
	if err := c.watchStoreVerdict(ctx, store, paused, defaultStoreAlertWait); err != nil {
		t.Fatalf("record the paused verdict: %v", err)
	}

	if reply := set(true); reply["status"] != StatusLive {
		t.Errorf("resume answered %v", reply)
	}
	if got := status(); got != StatusLive {
		t.Fatalf("after Resume the row reads %q, want live", got)
	}
	if got := audits(); len(got) != 2 || got[1] != [2]string{"shopify_store_resumed", owner} {
		t.Fatalf("after the resume the audit trail reads %v, want a resume by %s", got, owner)
	}

	// The next verdict says when the store left paused, and the next watch
	// pass records it.
	reply := builtinReply(t, eng, person, "builtin "+renderCall("shopifyStoreVerdict", map[string]any{"storeId": storeID}))
	if reply["state"] == VerdictPaused || reply["resumedAt"] != now.Format(time.RFC3339) {
		t.Errorf("the next verdict = state %v, resumedAt %v; want resumed at %s", reply["state"], reply["resumedAt"], now.Format(time.RFC3339))
	}
	store, _, _ = c.storeByID(memql.ContextWithFreshRead(operatorContext(ctx)), storeID)
	resumed, err := c.storeVerdict(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.watchStoreVerdict(ctx, store, resumed, defaultStoreAlertWait); err != nil {
		t.Fatalf("record the resumed verdict: %v", err)
	}
	store, _, _ = c.storeByID(memql.ContextWithFreshRead(operatorContext(ctx)), storeID)
	recorded, _ := store.Health["verdict"].(map[string]any)
	if mapString(recorded, "resumedAt") != now.Format(time.RFC3339) || mapString(recorded, "state") == VerdictPaused {
		t.Errorf("the watch recorded %v; want the resume at %s", recorded, now.Format(time.RFC3339))
	}
}

// passProbe is the Shopify integration with each handler's caller recorded:
// whether the call carried internal origin, and as whom.
type passProbe struct {
	inner *Integration
	mu    stdsync.Mutex
	seen  []passCall
}

type passCall struct {
	capability string
	internal   bool
	userID     string
	role       auth.Role
	err        error
}

func (p *passProbe) IntegrationName() string { return p.inner.IntegrationName() }

func (p *passProbe) Capabilities() []memql.IntegrationCapability {
	caps := p.inner.Capabilities()
	for i := range caps {
		name, handle := caps[i].Name, caps[i].Handler
		caps[i].Handler = func(ctx context.Context, args map[string]any, depth int) ([]memorynodes.MemoryNode, error) {
			nodes, err := handle(ctx, args, depth)
			call := passCall{capability: name, internal: auth.OriginFromContext(ctx).IsInternal(), err: err}
			if ac, ok := auth.AccessFromContext(ctx); ok && ac != nil {
				call.userID, call.role = ac.UserId, ac.Role
			}
			p.mu.Lock()
			p.seen = append(p.seen, call)
			p.mu.Unlock()
			return nodes, err
		}
	}
	return caps
}

func (p *passProbe) callsTo(capability string) []passCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []passCall
	for _, c := range p.seen {
		if c.capability == capability {
			out = append(out, c)
		}
	}
	return out
}

// noStoresEngine is the real engine, answering the two passes' first reads
// with nothing, so a pass the gate admits touches no store or job another
// test left in the shared database.
type noStoresEngine struct {
	*memql.MemQLEngine
}

func (e *noStoresEngine) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	switch callName(q) {
	case "storesForConnector", "complianceJobsDue":
		return &memql.ExecuteResult{}, nil
	}
	return e.MemQLEngine.Execute(ctx, q)
}

func TestTheScheduledPassesStillRunFromTheirAutomations(t *testing.T) {
	eng, _ := authorityEngine(t)
	if eng == nil {
		return
	}
	quiet := &noStoresEngine{MemQLEngine: eng}
	c := NewConnector(quiet, slog.New(slog.DiscardHandler), NewStoreRegistry(quiet, nil), NewAdminClient())
	probe := &passProbe{inner: NewIntegration(c)}
	if err := eng.RegisterIntegration(probe); err != nil {
		t.Fatal(err)
	}
	loader := automations.NewLoader(automations.LoaderOptions{Functions: eng.Functions(), Logger: eng.Logger})
	executor := automations.NewExecutor(automations.ExecutorOptions{Engine: eng, StepRegistry: steps.NewRegistry(), Logger: eng.Logger})
	defer executor.Close()

	for automation, pass := range map[string]struct{ capability, builtin string }{
		"reconcileShopifySubscriptions": {"ensureSubscriptions", "shopifyEnsureSubscriptions"},
		"runShopifyComplianceJobs":      {"runComplianceJobs", "shopifyRunComplianceJobs"},
	} {
		a, err := loader.LoadByName(automation)
		if err != nil {
			t.Fatalf("load %s: %v", automation, err)
		}
		if !a.Trusted {
			t.Fatalf("%s is not from the registered tree, so its steps would not carry internal origin", automation)
		}
		exec, err := executor.Execute(context.Background(), a, "schedule")
		if err != nil {
			t.Fatalf("%s: %v", automation, err)
		}
		calls := probe.callsTo(pass.capability)
		if len(calls) != 1 {
			t.Fatalf("%s reached %s %d times, want once (run %s)", automation, pass.builtin, len(calls), exec.Status)
		}
		got := calls[0]
		if !got.internal {
			t.Errorf("%s reached %s without internal origin", automation, pass.builtin)
		}
		// The automation's own actor: a reader, never a cluster owner. Internal
		// origin, not its role, is what admits it.
		if got.role != auth.RoleReader || got.userID != "system:automation:"+automation {
			t.Errorf("%s reached %s as %q (%s), want the automation's reader actor", automation, pass.builtin, got.userID, got.role)
		}
		if got.err != nil {
			t.Errorf("%s: %s refused the automation: %v", automation, pass.builtin, got.err)
		}
		if exec.Status != "completed" {
			t.Errorf("%s ended %s", automation, exec.Status)
		}

		// The same builtin asked by a writer on the wire is refused.
		if _, err := eng.Execute(clientCall("dbtest-pass-writer", auth.RoleWriter), "builtin "+pass.builtin+"()"); err == nil ||
			!strings.Contains(err.Error(), "shopify: "+pass.builtin+" is for a cluster owner") {
			t.Errorf("%s from a writer: err = %v, want the refusal", pass.builtin, err)
		}
	}
}
