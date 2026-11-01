//go:build agent

package worker

// THE LEDGER FINDS WHAT A MACHINE SERVED.
//
// A fleet call writes `executionSurface` and fleetSharingLedger reads it, from
// opposite ends of the tree, and nothing but a stored row shows that the two
// spell it alike. So nothing here spells it: the machines are registered through
// the real store, the surface comes back from a real FleetInference.Call over the
// real catalog read, and the answer comes from the real builtin. This package is
// the one place that can do all three, because component/memql cannot import the
// writer.
//
// TO CONFIRM IT IS LOAD-BEARING: render the ledger's surface from the bare id
// again, and every machine here reads "No calls have run on this machine this
// week." with no error.

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	workerservice "github.com/znasllc-io/memql/component/worker"
	"github.com/znasllc-io/memql/core/id"
)

func TestTheLedgerFindsTheCallsAMachineServed(t *testing.T) {
	ctx := context.Background()
	reachable, err := dbtest.EnsureSchema(ctx)
	require.NoError(t, err)
	if !reachable {
		dbtest.Unreachable(t, "sharing ledger", dbtest.DSN(), nil)
		return
	}
	_, err = memql.LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	engine, err := memql.New(db)
	require.NoError(t, err)
	require.NoError(t, engine.Init(memorynodes.DefaultRegistry()))

	const (
		ledgerModel = "ledger-model"
		ledgerNode  = "agent-ledger"
	)
	owner := "v1:identity:user:" + id.NewShortId()
	store := &EngineStore{Engine: engine}
	f := &FleetInference{
		router:     NewRouter(store, testLogger(), time.Now),
		store:      store,
		registry:   workerservice.NewRegistry(testLogger(), time.Now),
		selfNodeId: ledgerNode,
		logger:     testLogger(),
		clock:      time.Now,
	}

	var registered []string
	t.Cleanup(func() {
		_, _ = db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).Where("concept = ?", workerservice.RegistrationConcept).Where("id IN (?)", bun.In(registered)).Exec(context.Background())
	})

	// register does what a cockpit's stream does: a generated short id, written
	// through the real store, held in this replica's registry.
	register := func(name string) string {
		machine := id.NewShortId()
		registered = append(registered, workerservice.RegistrationConcept+":"+machine)
		capabilities := []string{workerservice.CapabilityHeadless, workerservice.ModelCapability}
		labels := map[string]string{
			"runtime:ollama":                      "1",
			workerservice.ModelLabel(ledgerModel): ModelAttributes{ContextWindow: 131072}.String(),
		}
		concurrency := map[string]uint32{workerservice.ModelCapability: 2}
		now := time.Now().UTC()
		require.NoError(t, (&workerservice.EngineStore{Engine: engine}).CreateRegistration(ctx, workerservice.RegistrationRow{
			ID: machine, OwnerUserId: owner, IdentityId: id.NewShortId(), Name: name,
			Capabilities: capabilities, Labels: labels, Concurrency: concurrency,
			RegisteredAt: now, LastSeenAt: now, ConnectedNodeId: ledgerNode,
		}))
		w := &workerservice.Worker{
			RegistrationId: machine, OwnerUserId: owner, Name: name,
			Capabilities: capabilities, Labels: labels, Concurrency: concurrency,
		}
		w.SetModelCallFunc(func(_ context.Context, req workerservice.ModelCallRequest) (*workerservice.ModelCallHandle, error) {
			handle, _, finish := workerservice.NewModelCallLoopback(req, func(string) {})
			go finish(workerservice.ModelCallOutcome{Content: "an answer", FinishReason: workerservice.ModelFinishStop})
			return handle, nil
		})
		f.registry.Add(w)
		return machine
	}
	lent, pinned, quiet := register("lent"), register("pinned"), register("quiet")

	// Lent through the mutation fleetSetSharing writes with, because the case this
	// sentence exists for is a call that reached the machine through the owner's
	// share, which reads the catalog by another query.
	borrower := "v1:identity:user:" + id.NewShortId()
	share, err := parser.RenderCall("setWorkerSharing", map[string]any{
		"registrationId": lent, "mode": workerservice.SharingModePeople, "paused": false, "userIds": []string{borrower},
	})
	require.NoError(t, err)
	_, err = engine.Execute(auth.ContextWithInternalOrigin(auth.ContextWithUserActor(ctx, owner)), "mutation "+share)
	require.NoError(t, err)

	// The surface a call writes, read off the call. A pin names the machine by
	// its bare id, the way a client does, and the surface must not depend on it.
	surfaceOfCall := func(caller, pin string) string {
		res, err := f.Call(authorityCtx(t, caller), memql.FleetCallRequest{
			ActingUserId: caller, RegistrationId: pin, ModelId: ledgerModel, Kind: memql.FleetKindChat,
		})
		require.NoError(t, err)
		require.NotEmpty(t, res.ExecutionSurface)
		return res.ExecutionSurface
	}
	lentSurface, pinnedSurface := surfaceOfCall(borrower, ""), surfaceOfCall(owner, pinned)
	require.NotEqual(t, lentSurface, pinnedSurface)

	record := func(surface, caller, level string) {
		call, err := parser.RenderCall("recordRouterCall", map[string]any{
			"callId": id.NewShortId(), "requestId": id.NewShortId(), "userId": caller,
			"vendor": "local", "model": ledgerModel, "providerName": "fleet:" + ledgerModel,
			"totalDurationMs": 1200, "outcome": "ok", "level": level, "executionSurface": surface,
		})
		require.NoError(t, err)
		_, err = engine.Execute(auth.ContextWithInternalOrigin(auth.ContextWithUserActor(ctx, owner)), "mutation "+call)
		require.NoError(t, err)
	}
	record(lentSurface, owner, "fast")
	record(lentSurface, borrower, "reasoning")
	record(lentSurface, "system:automation:brief", "fast")
	record(pinnedSurface, owner, "fast")

	ledger := func(c require.TestingT, asker, machine string) map[string]any {
		res, err := engine.Execute(auth.ContextWithUserActor(ctx, asker), "builtin fleetSharingLedger(registrationId: "+parser.QuoteString(machine)+")")
		require.NoError(c, err)
		rows := memql.MaterializeRows(res.OutputPayload())
		require.Len(c, rows, 1)
		return rows[0]
	}

	// EVENTUALLY, because the read's window closes on a whole second and is
	// exclusive: a row written this second is in the next second's answer.
	const lentSentence = "Served 3 calls this week, 1 of them for 1 other person and 1 for the cluster's own work."
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		assert.Equal(c, lentSentence, ledger(c, owner, lent)["sentence"])
		assert.Equal(c, "Served 1 call this week, for you.", ledger(c, owner, pinned)["sentence"])
	}, 10*time.Second, 100*time.Millisecond)

	// A client asks with the bare id and the engine's own callers hold the
	// canonical one. Both are the same machine, and the row answers with the id
	// it was asked for.
	for _, asked := range []string{lent, workerservice.RegistrationConcept + ":" + lent} {
		row := ledger(t, owner, asked)
		assert.Equal(t, lentSentence, row["sentence"], "asked as %s", asked)
		assert.Equal(t, true, row["readable"])
		assert.Equal(t, asked, row["machineId"])
		assert.EqualValues(t, 3, row["calls"])
		assert.EqualValues(t, 2, row["people"])
		assert.EqualValues(t, 1, row["otherCalls"])
		assert.EqualValues(t, 1, row["otherPeople"])
		assert.EqualValues(t, 1, row["systemCalls"])
	}

	// The zero stays honest: this machine really did nothing, and its neighbours'
	// calls are not its own.
	assert.Equal(t, "No calls have run on this machine this week.", ledger(t, owner, quiet)["sentence"])

	// Somebody else's machine answers as a made-up id does.
	_, err = engine.Execute(auth.ContextWithUserActor(ctx, "v1:identity:user:"+id.NewShortId()), "builtin fleetSharingLedger(registrationId: "+parser.QuoteString(lent)+")")
	require.ErrorContains(t, err, "no machine of yours")
}
