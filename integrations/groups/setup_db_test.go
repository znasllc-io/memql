package groups

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/automations/steps"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

type setupDBEngine struct{ *memql.MemQLEngine }

func (e setupDBEngine) Execute(ctx context.Context, q string) (any, error) {
	return e.MemQLEngine.Execute(ctx, q)
}

// Run the authored boot seed and the real ownership completion in an empty
// connection-local graph. No installation account or principal is modified.
func TestSeedSelfAccountThenConfigureWithRealEngine(t *testing.T) {
	t.Setenv("MEMQL_DOMAIN", "memql.localhost")
	ctx := context.Background()
	reachable, err := dbtest.EnsureSchema(ctx)
	require.NoError(t, err)
	if !reachable {
		dbtest.Unreachable(t, "self account seed", dbtest.DSN(), sql.ErrConnDone)
	}
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `CREATE TEMP TABLE "MemoryNodes" (LIKE public."MemoryNodes" INCLUDING ALL)`)
	require.NoError(t, err)
	_, err = memql.LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	eng, err := memql.New(db)
	require.NoError(t, err)
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, eng.Init(memorynodes.DefaultRegistry()))
	reg := steps.NewRegistry()
	eng.SetLogicRunner(automations.NewLogicRunner(eng, reg, eng.Logger))
	loaded, err := automations.NewLoader(automations.LoaderOptions{Logger: eng.Logger, Functions: eng.Functions()}).LoadFromTree(memqldsl.Tree())
	require.NoError(t, err)
	var seed *automations.Automation
	for _, a := range loaded {
		if a.Name == "seedSelfAccount" {
			seed = a
		}
	}
	require.NotNil(t, seed)
	bus := events.NewBus()
	t.Cleanup(bus.Close)
	executor := automations.NewExecutor(automations.ExecutorOptions{Logger: eng.Logger, Engine: eng, EventBus: bus, StepRegistry: reg, SandboxRun: true})
	event := &events.Event{Topic: "system.startup", Payload: map[string]any{"domain": "memql.localhost", "node": map[string]any{"type": "bff"}}}
	run, err := executor.ExecuteWithEvent(ctx, seed, "system.startup", event)
	require.NoError(t, err)
	require.Equal(t, "completed", run.Status)
	readSelf := func() map[string]any {
		t.Helper()
		var row memorynodes.MemoryNode
		require.NoError(t, db.NewSelect().Model(&row).Where("concept = ?", "v1:accounts:account").Where("id = ?", "v1:accounts:account:self").OrderExpr(`"createdAt" DESC`).Limit(1).Scan(ctx))
		var payload map[string]any
		require.NoError(t, json.Unmarshal(row.Payload, &payload))
		return payload
	}
	initial := readSelf()
	require.Empty(t, initial["ownerUserId"])
	require.Equal(t, "unverified", initial["domainStatus"])
	for _, key := range []string{"domain", "memqlDomain", "domainVerifiedAt", "memqlReservedAt", "configuredAt"} {
		require.Empty(t, initial[key], key)
	}
	internal := SystemActorContext(ctx)
	_, err = eng.Execute(internal, `insert("v1:identity:user", id="seed-claim-owner", payload={"displayName":"Claim owner", "primaryEmail":"seed-owner@example.test", "role":"owner", "active":true})`)
	require.NoError(t, err)
	integration := New(setupDBEngine{eng}, nil)
	require.NoError(t, integration.ConfigureSelfAccount(internal, "Claimed company", "seed-claim-owner"))
	claimed := readSelf()
	require.Equal(t, "Claimed company", claimed["name"])
	require.Equal(t, "seed-claim-owner", memql.BareShortId(asString(claimed["ownerUserId"])))
	require.NotEmpty(t, claimed["configuredAt"])
	members, err := integration.store.MembersOfGroup(internal, AccountGroupID("self"))
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.Equal(t, "seed-claim-owner", members[0].UserID)
	require.Equal(t, StatusActive, members[0].Status)
	// Another boot cannot restore the placeholder or replace the person.
	run, err = executor.ExecuteWithEvent(ctx, seed, "system.startup", event)
	require.NoError(t, err)
	require.Equal(t, "completed", run.Status)
	require.Equal(t, claimed, readSelf())
	_, err = eng.Execute(internal, `insert("v1:identity:user", id="seed-other-owner", payload={"displayName":"Other owner", "primaryEmail":"seed-other@example.test", "role":"owner", "active":true})`)
	require.NoError(t, err)
	require.ErrorContains(t, integration.ConfigureSelfAccount(internal, "Forbidden", "seed-other-owner"), "different owner")
	require.Equal(t, claimed, readSelf())
	// The client domain policy must still refuse a reservation under the
	// installation hostname; the seed fix does not introduce a DNS exemption.
	_, err = eng.Execute(internal, `mutation createClientAccount(accountId:"seed-forbidden-domain", name:"Forbidden", domain:"memql.localhost")`)
	require.ErrorContains(t, err, "domain_under_cluster_domain")

}
