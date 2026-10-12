package node

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

// The statement in status_history_purge.go, against real Postgres. Everything
// here is about WHICH versions go, which is the half a fake purger cannot
// answer: the age rule, the "never the latest" rule, the concept scope, and the
// join back onto exactly the versions the sub-select named.

// statusHistoryFixture seeds versions of rows under a unique prefix so parallel
// lanes on one shared database never see each other's.
//
// The statement is scoped by concept, not by prefix, so a run also removes
// whatever stale superseded versions another suite left in the same three
// concepts. That is correct, and it is why the assertions below are about the
// versions THIS fixture wrote -- a returned count is a floor, never an exact
// figure, on a database anyone else is using.
type statusHistoryFixture struct {
	db     *bun.DB
	prefix string
	ctx    context.Context
	now    time.Time
}

func newStatusHistoryFixture(t *testing.T) *statusHistoryFixture {
	t.Helper()
	ctx := context.Background()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "status history purge", dbtest.DSN(), err)
		return nil
	}
	f := &statusHistoryFixture{
		db:     db,
		prefix: fmt.Sprintf("history-%d", time.Now().UnixNano()),
		ctx:    ctx,
		// Whole seconds: "createdAt" is a timestamptz, and a version written at
		// a figure the column rounds would come back unequal to the one asked for.
		now: time.Now().UTC().Truncate(time.Second),
	}
	t.Cleanup(func() {
		_, _ = db.NewDelete().Model((*memorynodes.MemoryNode)(nil)).
			Where("id LIKE ?", "%"+f.prefix+"%").Exec(ctx)
	})
	return f
}

func (f *statusHistoryFixture) id(concept, name string) string {
	return concept + ":" + f.prefix + "-" + name
}

// version writes one version of one row, `ago` before the fixture's now.
func (f *statusHistoryFixture) version(t *testing.T, concept, name string, ago time.Duration) {
	t.Helper()
	at := f.now.Add(-ago)
	payload, err := json.Marshal(map[string]any{"name": name, "lastSeen": at.Format(time.RFC3339)})
	require.NoError(t, err)
	row := memorynodes.MemoryNode{
		ID: f.id(concept, name), CreatedAt: at, CreatedBy: "system:test",
		Concept: concept, Type: memorynodes.NodeTypeObject,
		Schema: json.RawMessage(`{}`), Payload: payload,
		Metadata: json.RawMessage(`{}`), Provenance: json.RawMessage(`{}`),
	}
	_, err = f.db.NewInsert().Model(&row).Exec(f.ctx)
	require.NoError(t, err)
}

// remaining lists how long ago each surviving version of one row was written,
// newest first.
func (f *statusHistoryFixture) remaining(t *testing.T, concept, name string) []time.Duration {
	t.Helper()
	var at []time.Time
	err := f.db.NewSelect().Model((*memorynodes.MemoryNode)(nil)).
		ColumnExpr(`mn."createdAt"`).
		Where("mn.concept = ?", concept).
		Where("mn.id = ?", f.id(concept, name)).
		OrderExpr(`mn."createdAt" DESC`).
		Scan(f.ctx, &at)
	require.NoError(t, err)
	ages := make([]time.Duration, 0, len(at))
	for _, a := range at {
		ages = append(ages, f.now.Sub(a).Round(time.Second))
	}
	return ages
}

// purge runs the statement the way a sweep does, batch after batch until one
// comes back short, and answers how many versions went in all.
func (f *statusHistoryFixture) purge(t *testing.T, concept string, olderThan time.Time) int64 {
	t.Helper()
	p := sqlStatusHistoryPurger{db: f.db}
	var total int64
	for i := 0; i < 1000; i++ {
		n, err := p.purgeSuperseded(f.ctx, concept, olderThan, statusHistoryBatch)
		require.NoError(t, err)
		total += n
		if n < statusHistoryBatch {
			return total
		}
	}
	t.Fatalf("a purge of %s did not converge in 1000 batches", concept)
	return total
}

func TestStatusHistoryPurgeDeletesOnlyOldSuperseded(t *testing.T) {
	f := newStatusHistoryFixture(t)
	if f == nil {
		return
	}
	registration := memqlengine.WorkerRegistrationConcept
	f.version(t, registration, "a", 3*time.Hour)
	f.version(t, registration, "a", 2*time.Hour)
	f.version(t, registration, "a", 10*time.Minute)

	deleted := f.purge(t, registration, f.now.Add(-time.Hour))

	// Both old versions have a newer one behind them and are past the hour, so
	// both go. The one from ten minutes ago is the latest AND inside the hour.
	require.GreaterOrEqual(t, deleted, int64(2), "the two old versions are counted")
	require.Equal(t, []time.Duration{10 * time.Minute}, f.remaining(t, registration, "a"),
		"exactly the two old versions went, and the recent one stayed")
}

// The age rule on its own. The latest version is protected by the other rule,
// so a test that only has the latest inside the hour cannot tell the grace
// from its absence: this one has a SUPERSEDED version inside the hour.
func TestStatusHistoryPurgeKeepsSupersededVersionsInsideTheGrace(t *testing.T) {
	f := newStatusHistoryFixture(t)
	if f == nil {
		return
	}
	registration := memqlengine.WorkerRegistrationConcept
	f.version(t, registration, "g", 3*time.Hour)
	f.version(t, registration, "g", 30*time.Minute)
	f.version(t, registration, "g", 10*time.Minute)

	f.purge(t, registration, f.now.Add(-time.Hour))

	require.Equal(t, []time.Duration{10 * time.Minute, 30 * time.Minute}, f.remaining(t, registration, "g"),
		"the 3h version went; the 30m one is superseded but inside the hour, so it stays")
}

// A heartbeat id whose ONLY versions are older than the grace window: the case
// a purge keyed on age alone gets wrong, and the whole of why the statement asks
// for a newer version rather than only an old one. A worker that has been
// offline for a day still has a registration, and that registration is its
// current state.
func TestStatusHistoryPurgeKeepsLatestEvenWhenOld(t *testing.T) {
	f := newStatusHistoryFixture(t)
	if f == nil {
		return
	}
	registration := memqlengine.WorkerRegistrationConcept
	f.version(t, registration, "b", 5*time.Hour)
	f.version(t, registration, "b", 4*time.Hour)
	// And one whose only version is old: nothing is newer than it at all.
	f.version(t, registration, "lone", 6*time.Hour)

	cutoff := f.now.Add(-time.Hour)
	f.purge(t, registration, cutoff)

	require.Equal(t, []time.Duration{4 * time.Hour}, f.remaining(t, registration, "b"),
		"only the 5h version goes; the 4h one is the latest and stays however old it is")
	require.Equal(t, []time.Duration{6 * time.Hour}, f.remaining(t, registration, "lone"),
		"a row with a single old version keeps it")

	// And it stays on every later pass, not only the first.
	f.purge(t, registration, cutoff)
	f.purge(t, registration, cutoff)
	require.Equal(t, []time.Duration{4 * time.Hour}, f.remaining(t, registration, "b"))
	require.Equal(t, []time.Duration{6 * time.Hour}, f.remaining(t, registration, "lone"))
}

func TestStatusHistoryPurgeLeavesOtherConcepts(t *testing.T) {
	f := newStatusHistoryFixture(t)
	if f == nil {
		return
	}
	identity := memorynodes.ConceptIdentityIdentity
	f.version(t, identity, "who", 5*time.Hour)
	f.version(t, identity, "who", 4*time.Hour)

	// Each status concept gets work to do, so a purge that left the identity
	// alone because it did nothing at all would not pass.
	for _, concept := range statusHistoryConcepts {
		f.version(t, concept, "history", 5*time.Hour)
		f.version(t, concept, "history", 4*time.Hour)
	}

	cutoff := f.now.Add(-time.Hour)
	for _, concept := range statusHistoryConcepts {
		f.purge(t, concept, cutoff)
		require.Equal(t, []time.Duration{4 * time.Hour}, f.remaining(t, concept, "history"),
			"%s: the superseded version went", concept)
	}
	require.Equal(t, []time.Duration{4 * time.Hour, 5 * time.Hour}, f.remaining(t, identity, "who"),
		"an identity's history is not status history and is never touched")

	// Asked directly, the purger refuses the concept rather than running.
	_, err := sqlStatusHistoryPurger{db: f.db}.purgeSuperseded(f.ctx, identity, cutoff, statusHistoryBatch)
	require.Error(t, err)
	require.Equal(t, []time.Duration{4 * time.Hour, 5 * time.Hour}, f.remaining(t, identity, "who"))
}

// Two ids that beat at the same instants: heartbeats flushed together carry
// identical "createdAt" values, which is why the delete joins back on the pair
// (id, "createdAt") and not on the timestamp alone.
func TestStatusHistoryPurgeJoinsOnIdAndCreatedAt(t *testing.T) {
	f := newStatusHistoryFixture(t)
	if f == nil {
		return
	}
	registration := memqlengine.WorkerRegistrationConcept
	f.version(t, registration, "x", 5*time.Hour)
	f.version(t, registration, "x", 4*time.Hour)
	f.version(t, registration, "x", 10*time.Minute)
	// y beat at the same 5h and 4h instants and then went quiet: its 4h version
	// is the latest, and x's 4h version (a victim) shares its timestamp.
	f.version(t, registration, "y", 5*time.Hour)
	f.version(t, registration, "y", 4*time.Hour)

	f.purge(t, registration, f.now.Add(-time.Hour))

	require.Equal(t, []time.Duration{10 * time.Minute}, f.remaining(t, registration, "x"))
	require.Equal(t, []time.Duration{4 * time.Hour}, f.remaining(t, registration, "y"),
		"a victim of another id at the same instant must not take y's latest version with it")
}

// The LIMIT is what keeps one statement inside the serving timeout on a table
// with a backlog, so it is tested as a bound and not only as a convention.
func TestStatusHistoryPurgeNeverDeletesMoreThanTheBatch(t *testing.T) {
	f := newStatusHistoryFixture(t)
	if f == nil {
		return
	}
	registration := memqlengine.WorkerRegistrationConcept
	for _, ago := range []time.Duration{8, 7, 6, 5, 4, 3} {
		f.version(t, registration, "e", ago*time.Hour)
	}

	cutoff := f.now.Add(-time.Hour)
	p := sqlStatusHistoryPurger{db: f.db}
	n, err := p.purgeSuperseded(f.ctx, registration, cutoff, 2)
	require.NoError(t, err)
	require.Equal(t, int64(2), n, "five versions are superseded and the batch is two")
	require.GreaterOrEqual(t, len(f.remaining(t, registration, "e")), 4, "at most two of this row's five went")

	// Drained batch by batch, it converges on the latest version and stops;
	// a further pass over a row with nothing left to remove is not an error.
	f.purge(t, registration, cutoff)
	require.Equal(t, []time.Duration{3 * time.Hour}, f.remaining(t, registration, "e"))
	_, err = p.purgeSuperseded(f.ctx, registration, cutoff, 2)
	require.NoError(t, err)
	require.Equal(t, []time.Duration{3 * time.Hour}, f.remaining(t, registration, "e"))
}
