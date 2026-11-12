package memql

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// accountStampingFleet is a schema of its own holding what the account-stamping
// handshake reads and writes: the row table with its v1:cluster:node rows, the
// observed view the fleet is read through, and the switch's two tables, each
// built by the migration that builds it in a cluster.
//
// A schema of its own, as the liveness handshakes' tests have, for two reasons.
// The switch turns on once and stays on, so the package's shared schema would
// hand every run after the first a switch that is already on. And the handshake
// reads every node row in the table, so a node another test left registered
// would hold it off for good.
//
// db and other are two pools, standing for two replicas: nothing passes between
// them but the database.
type accountStampingFleet struct {
	t         *testing.T
	ctx       context.Context
	db, other *bun.DB
}

func newAccountStampingFleet(t *testing.T) *accountStampingFleet {
	t.Helper()
	ctx := context.Background()
	dsn := dbtest.DSN()
	admin := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "account stamping handshake", dsn, err)
		return nil // Unreachable skipped or failed; only a non-stdlib TB reaches this
	}
	schema := fmt.Sprintf("account_stamping_%d", time.Now().UnixNano())
	_, err := admin.ExecContext(ctx, `CREATE SCHEMA ?`, bun.Ident(schema))
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = admin.ExecContext(context.Background(), `DROP SCHEMA ? CASCADE`, bun.Ident(schema)) })
	open := func() *bun.DB {
		db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn), pgdriver.WithConnParams(map[string]any{"search_path": schema}))), pgdialect.New())
		t.Cleanup(func() { _ = db.Close() })
		return db
	}
	f := &accountStampingFleet{t: t, ctx: ctx, db: open(), other: open()}
	for _, table := range []string{"MemoryNodes", "SecretMemoryNodes"} {
		_, err = f.db.ExecContext(ctx, `CREATE TABLE ? (LIKE public.? INCLUDING ALL)`, bun.Ident(table), bun.Ident(table))
		require.NoError(t, err)
	}
	// The first two build the observed view, the third the switch.
	for _, name := range []string{
		"20261013020000_shared_row_schemas.up.sql",
		"20261013050000_liveness.up.sql",
		"20261112100000_account_ownership.up.sql",
	} {
		applyLivenessMigration(t, ctx, f.db, name)
	}
	return f
}

// node records a version of a node's row carrying what the handshake reads of
// it: its id and its health.
func (f *accountStampingFleet) node(id, health string) {
	f.t.Helper()
	_, err := f.db.ExecContext(f.ctx, `INSERT INTO "MemoryNodes" (id,"createdAt","createdBy",concept,type,schema,payload,provenance) VALUES (?,clock_timestamp(),'test','v1:cluster:node','object','{}',jsonb_build_object('health',?::text),'{}')`, "v1:cluster:node:"+id, health)
	require.NoError(f.t, err)
}

// due makes the next handshake the one that looks at the fleet. The switch does
// that at most once per five seconds for the whole cluster; a test moves the
// mark instead of sleeping, and leaves the readiness query itself as it is.
func (f *accountStampingFleet) due() {
	f.t.Helper()
	_, err := f.db.ExecContext(f.ctx, `UPDATE account_stamping_storage SET next_check_at=clock_timestamp()`)
	require.NoError(f.t, err)
}

// ask is one node's handshake, at a moment the fleet is due to be looked at.
func (f *accountStampingFleet) ask(db *bun.DB, nodeID string, allowActivation bool) bool {
	f.t.Helper()
	f.due()
	on, err := memorynodes.EnableAccountStampingWhenReady(f.ctx, db, nodeID, allowActivation)
	require.NoError(f.t, err)
	return on
}

// on reads the switch itself, as a node that has not asked yet would find it.
func (f *accountStampingFleet) on() bool {
	f.t.Helper()
	var on bool
	require.NoError(f.t, f.db.QueryRowContext(f.ctx, `SELECT enabled FROM account_stamping_storage WHERE singleton`).Scan(&on))
	return on
}

// advertised reports whether the node has said, recently enough to count, that
// it knows the field.
func (f *accountStampingFleet) advertised(nodeID string) bool {
	f.t.Helper()
	var n int
	require.NoError(f.t, f.db.QueryRowContext(f.ctx, `SELECT count(*) FROM account_stamping_readers WHERE node_id=? AND seen_at>clock_timestamp()-interval '30 seconds'`, nodeID).Scan(&n))
	return n == 1
}

// Old and new nodes serve side by side while a release rolls out, and a node of
// the old release refuses to update a row carrying a key its concept does not
// declare. So stamping waits for every node that has not stopped to say it
// knows the field, and a node that said so a while ago has not said so.
func TestAccountStampingWaitsForEveryLiveNodeToAdvertise(t *testing.T) {
	f := newAccountStampingFleet(t)
	f.node("a", "healthy")
	f.node("b", "healthy")

	require.False(t, f.ask(f.db, "a", true), "replica b has not said it knows the field")
	require.True(t, f.advertised("a"))
	// Not the five-second mark holding it: the fleet is scanned again and b is
	// still missing.
	require.False(t, f.ask(f.db, "a", true), "asking again turned the switch on with b still silent")

	// b ran the new engine a minute ago and has not been heard from since: it
	// may have been replaced by the previous release in a rollback.
	_, err := f.db.ExecContext(f.ctx, `INSERT INTO account_stamping_readers VALUES ('b',clock_timestamp()-interval '1 minute')`)
	require.NoError(t, err)
	require.False(t, f.ask(f.db, "a", true), "an advertisement older than 30 s counted")
	require.False(t, f.on())

	// The fleet is looked at once per interval for the whole cluster, not once
	// per replica per tick: b advertises now and the switch turns on at the
	// next look, not at this call.
	_, err = f.db.ExecContext(f.ctx, `UPDATE account_stamping_storage SET next_check_at=clock_timestamp()+interval '1 hour'`)
	require.NoError(t, err)
	on, err := memorynodes.EnableAccountStampingWhenReady(f.ctx, f.other, "b", true)
	require.NoError(t, err)
	require.False(t, on, "a replica looked at the fleet before it was due")
	require.True(t, f.advertised("b"))

	require.True(t, f.ask(f.other, "b", true), "both replicas advertised within 30 s")
	require.True(t, f.on())
	// The replica that did not turn it on finds it on.
	require.True(t, f.ask(f.db, "a", true))
}

// MEMQL_ACCOUNT_STAMPING_ACTIVATE=false is the operator's hold. A held node
// still says it knows the field, so that the hold released on one replica takes
// effect at that replica's next check without the others being rolled first,
// and it turns nothing on however ready the fleet is.
func TestAccountStampingHeldNodeAdvertisesAndTurnsNothingOn(t *testing.T) {
	f := newAccountStampingFleet(t)
	f.node("a", "healthy")
	f.node("b", "healthy")

	require.False(t, f.ask(f.db, "a", false))
	require.False(t, f.ask(f.other, "b", false))
	require.True(t, f.advertised("a"), "a held node did not advertise")
	require.True(t, f.advertised("b"), "a held node did not advertise")
	// Everything but the hold is ready, on every replica, and it stays off.
	require.False(t, f.ask(f.db, "a", false), "a held node turned the switch on")
	require.False(t, f.on())

	// The hold is released on b alone. a is still held and has kept advertising.
	require.True(t, f.ask(f.other, "b", true), "releasing the hold on one replica waited for the other")
	require.True(t, f.on())
	// A hold is on turning it on. It is not a way to turn it off, and a held
	// node is told the truth about the cluster.
	require.True(t, f.ask(f.db, "a", false))
}

// Rows carry the field once the switch is on, so nothing turns it off: not a
// replica that stops advertising, not a node that joins without having
// advertised, not a hold set afterwards.
func TestAccountStampingStaysOnOnceOn(t *testing.T) {
	f := newAccountStampingFleet(t)
	f.node("a", "healthy")
	f.node("b", "healthy")
	require.False(t, f.ask(f.db, "a", true))
	require.True(t, f.ask(f.other, "b", true))

	_, err := f.db.ExecContext(f.ctx, `DELETE FROM account_stamping_readers`)
	require.NoError(t, err)
	f.node("c", "healthy")

	require.True(t, f.ask(f.db, "a", true), "a replica that stopped advertising turned the switch off")
	require.True(t, f.ask(f.db, "a", false), "a hold set after the switch turned on turned it off")
	require.True(t, f.ask(f.db, "", true), "a process with no node id read the switch as off")
	require.True(t, f.on())
	// Once on there is nothing left to say: nobody advertises again.
	require.False(t, f.advertised("a"))
}

// A node counts until the latest version of its row says it stopped, and the
// latest version is the only one read: a node that stopped is gone, and one
// that came back is back.
func TestAccountStampingIsHeldByLiveNodesOnly(t *testing.T) {
	t.Run("a stopped node that never advertised does not hold it", func(t *testing.T) {
		f := newAccountStampingFleet(t)
		f.node("a", "healthy")
		f.node("retired", "healthy")
		f.node("retired", "stopped")
		require.True(t, f.ask(f.db, "a", true))
	})

	// What a cluster actually stores: health is one of the fields the liveness
	// store keeps as the row's one current observation, so "stopped" is not a
	// version in the row table at all. Read from the row table alone, this node
	// would still be healthy and would hold the switch for good.
	t.Run("stopped recorded as the row's current observation", func(t *testing.T) {
		f := newAccountStampingFleet(t)
		_, err := f.db.ExecContext(f.ctx, `UPDATE memory_liveness_storage SET enabled=true`)
		require.NoError(t, err)
		f.node("a", "healthy")
		f.node("retired", "healthy")
		f.node("retired", "stopped")
		var versions, observations int
		require.NoError(t, f.db.QueryRowContext(f.ctx, `SELECT count(*) FROM "MemoryNodes" WHERE id='v1:cluster:node:retired'`).Scan(&versions))
		require.NoError(t, f.db.QueryRowContext(f.ctx, `SELECT count(*) FROM memory_liveness WHERE id='v1:cluster:node:retired'`).Scan(&observations))
		require.Equal(t, 1, versions, "the fixture did not store the stop as an observation")
		require.Equal(t, 1, observations, "the fixture did not store the stop as an observation")
		require.True(t, f.ask(f.db, "a", true), "a node whose stop is an observation held the switch")
	})

	t.Run("a node that came back holds it again", func(t *testing.T) {
		f := newAccountStampingFleet(t)
		f.node("a", "healthy")
		f.node("returned", "stopped")
		f.node("returned", "healthy")
		require.False(t, f.ask(f.db, "a", true), "a node serving again, on an engine that never advertised, was not waited for")
		require.True(t, f.ask(f.other, "returned", true))
	})
}

// The node that turns the switch on is one of the fleet it speaks for. A
// process that has no node id yet says nothing, and one whose row is not in the
// fleet cannot turn it on even when every node in the fleet has advertised.
func TestAccountStampingIsTurnedOnOnlyByARegisteredNode(t *testing.T) {
	f := newAccountStampingFleet(t)
	f.node("a", "healthy")

	require.False(t, f.ask(f.db, "", true))
	require.False(t, f.ask(f.db, "  ", true))
	var readers int
	require.NoError(t, f.db.QueryRowContext(f.ctx, `SELECT count(*) FROM account_stamping_readers`).Scan(&readers))
	require.Zero(t, readers, "a process with no node id advertised")

	_, err := f.db.ExecContext(f.ctx, `INSERT INTO account_stamping_readers VALUES ('a',clock_timestamp())`)
	require.NoError(t, err)
	require.False(t, f.ask(f.other, "unregistered", true), "a process that is not a node of the fleet turned the switch on")
	require.True(t, f.ask(f.db, "a", true))
}

// The down migration removes what the up migration creates and nothing else,
// and the up applies again over the result with the switch off. Compared as
// the schema's whole list of relations, so a table or an index added to one
// file and not the other fails here without this test naming it.
func TestAccountStampingMigrationDownRemovesWhatUpCreates(t *testing.T) {
	f := newAccountStampingFleet(t)
	relations := func() []string {
		t.Helper()
		var names []string
		rows, err := f.db.QueryContext(f.ctx, `SELECT c.relname::text FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=current_schema() ORDER BY 1`)
		require.NoError(t, err)
		defer rows.Close()
		for rows.Next() {
			var name string
			require.NoError(t, rows.Scan(&name))
			names = append(names, name)
		}
		require.NoError(t, rows.Err())
		return names
	}
	const up, down = "20261112100000_account_ownership.up.sql", "20261112100000_account_ownership.down.sql"

	with := relations()
	applyLivenessMigration(t, f.ctx, f.db, down)
	without := relations()
	require.Less(t, len(without), len(with), "the down migration removed nothing")

	// Turned on, then taken down and put back: it comes back off.
	applyLivenessMigration(t, f.ctx, f.db, up)
	require.Equal(t, with, relations(), "the up migration did not apply again over its own down")
	f.node("a", "healthy")
	require.True(t, f.ask(f.db, "a", true))
	applyLivenessMigration(t, f.ctx, f.db, down)
	require.Equal(t, without, relations(), "the down migration left something of the up migration behind")
	applyLivenessMigration(t, f.ctx, f.db, up)
	require.False(t, f.on(), "the switch came back on after its tables were dropped and created again")
}
