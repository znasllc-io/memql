package database_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
)

// THE LATEST-ROW INDEX AGAINST A REAL DATABASE (memql#5252).
//
// Every case builds its own scratch table under a random name -- the columns
// and the index set MemoryNodes carries, on a plain table or a hypertable --
// so no case touches MemoryNodes or another case, and each drops its table
// when it ends. The ensurer under test and the oracle that judges it read the
// catalog two different ways (pg_index's arrays there, pg_get_indexdef's text
// here), so a mistake in the inspection cannot vouch for itself.
//
// The hypertable cases need the timescaledb extension and skip, saying so,
// on a database without it; the db-tests lane runs TimescaleDB, so there they
// run. The cases that simulate damage the catalog way (an invalid root) need
// a superuser and skip, saying so, without one.

// lriTable is one scratch table and the names of the indexes on it.
type lriTable struct {
	name      string // mixed case, like "MemoryNodes", so quoting is exercised
	index     string // the canonical latest-row index the ensurer is asked for
	byID      string // (id, "createdAt" DESC): the index the incident's SkipScan walked
	byConcept string // (concept)
}

func (t lriTable) quoted() string { return `"` + t.name + `"` }

// newLatestRowTable creates a scratch table shaped like MemoryNodes, with its
// two other read indexes. Index names stay short so a chunk's copy of each
// ("_hyper_<n>_<m>_chunk_" + name) is never truncated at 63 bytes, and none
// is a suffix of another.
func newLatestRowTable(t *testing.T, db *bun.DB, hypertable bool) lriTable {
	t.Helper()
	raw := make([]byte, 4)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	suffix := hex.EncodeToString(raw)
	tbl := lriTable{
		name:      "LatestRow_" + suffix,
		index:     "lri_" + suffix + "_idx",
		byID:      "lri_" + suffix + "_byid",
		byConcept: "lri_" + suffix + "_byconcept",
	}
	lriExec(t, db, `CREATE TABLE `+tbl.quoted()+` (
		id TEXT NOT NULL,
		"createdAt" TIMESTAMPTZ NOT NULL,
		concept TEXT NOT NULL,
		payload JSONB NOT NULL DEFAULT '{}',
		PRIMARY KEY (id, "createdAt"))`)
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DROP TABLE IF EXISTS `+tbl.quoted()+` CASCADE`)
	})
	if hypertable {
		lriExec(t, db, `SELECT create_hypertable('`+tbl.quoted()+`', 'createdAt', chunk_time_interval => interval '1 day')`)
	}
	lriExec(t, db, `CREATE INDEX `+tbl.byID+` ON `+tbl.quoted()+` (id, "createdAt" DESC)`)
	lriExec(t, db, `CREATE INDEX `+tbl.byConcept+` ON `+tbl.quoted()+` (concept)`)
	return tbl
}

func lriExec(t *testing.T, db bun.IConn, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("%s: %v", strings.Join(strings.Fields(query), " "), err)
	}
}

// seedAcrossDays writes perDay rows on each of days consecutive days, so a
// hypertable with a one-day chunk interval gets one chunk per day.
func seedAcrossDays(t *testing.T, db *bun.DB, tbl lriTable, days, perDay int) {
	t.Helper()
	lriExec(t, db, fmt.Sprintf(`INSERT INTO %s (id, "createdAt", concept, payload)
		SELECT 'v1:test:lri:' || g,
		       timestamptz '2026-01-01' + ((g %% %d) * interval '1 day') + (g * interval '1 second'),
		       'v1:test:lri' || (g %% 3),
		       jsonb_build_object('g', g)
		  FROM generate_series(1, %d) g`, tbl.quoted(), days, days*perDay))
}

func requireTimescale(t *testing.T, db *bun.DB) {
	t.Helper()
	var ok bool
	if err := db.QueryRowContext(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb')`).Scan(&ok); err != nil {
		t.Fatalf("checking for timescaledb: %v", err)
	}
	if !ok {
		t.Skip("this database has no timescaledb extension, so it has no hypertables; the hypertable half " +
			"of the latest-row index is proven where it does (the db-tests lane runs TimescaleDB)")
	}
}

func requireSuperuser(t *testing.T, db *bun.DB) {
	t.Helper()
	var super bool
	if err := db.QueryRowContext(context.Background(),
		`SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&super); err != nil {
		t.Fatalf("checking for superuser: %v", err)
	}
	if !super {
		t.Skip("simulating an interrupted build writes pg_index.indisvalid, which needs a superuser; this role is not one")
	}
}

func chunkCount(t *testing.T, db *bun.DB, tbl lriTable) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM pg_inherits WHERE inhparent = to_regclass(?)`, tbl.quoted()).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// lriOracle is completeness read a DIFFERENT way from the code under test:
// the root index by name, and each chunk through the definition Postgres
// prints for its indexes.
type lriOracle struct {
	rootDef    string // "" when no index of that name exists
	rootUsable bool
	uncovered  []string
}

func (o lriOracle) complete() bool {
	return o.rootUsable && strings.HasSuffix(o.rootDef, `USING btree (concept, id, "createdAt" DESC)`) && len(o.uncovered) == 0
}

func readOracle(t *testing.T, db *bun.DB, tbl lriTable, index string) lriOracle {
	t.Helper()
	ctx := context.Background()
	var o lriOracle
	err := db.QueryRowContext(ctx, `
		SELECT pg_get_indexdef(i.indexrelid), i.indisvalid AND i.indisready AND i.indislive
		  FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		 WHERE i.indrelid = to_regclass(?) AND c.relname = ?`, tbl.quoted(), index).Scan(&o.rootDef, &o.rootUsable)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("oracle: reading index %s: %v", index, err)
	}
	rows, err := db.QueryContext(ctx, `
		SELECT c.relname
		  FROM pg_inherits inh JOIN pg_class c ON c.oid = inh.inhrelid
		 WHERE inh.inhparent = to_regclass(?)
		   AND NOT EXISTS (
		         SELECT 1 FROM pg_index i
		          WHERE i.indrelid = inh.inhrelid AND i.indisvalid AND i.indisready AND i.indpred IS NULL
		            AND pg_get_indexdef(i.indexrelid) LIKE '%(concept, id, "createdAt" DESC)%')
		 ORDER BY 1`, tbl.quoted())
	if err != nil {
		t.Fatalf("oracle: reading chunks: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		o.uncovered = append(o.uncovered, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return o
}

// markInvalid puts a finished index into the catalog state an interrupted
// build leaves behind: indisvalid=false, indisready=true.
func markInvalid(t *testing.T, db *bun.DB, tbl lriTable, index string) {
	t.Helper()
	lriExec(t, db, `UPDATE pg_index SET indisvalid = false
		 WHERE indexrelid = (SELECT i.indexrelid FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		                      WHERE i.indrelid = to_regclass(?) AND c.relname = ?)`, tbl.quoted(), index)
}

func mustEnsure(t *testing.T, db *bun.DB, tbl lriTable, want string) {
	t.Helper()
	r, err := database.EnsureLatestRowIndex(context.Background(), db, nil, tbl.name, tbl.index)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if string(r.Outcome) != want {
		t.Fatalf("ensure outcome = %s, want %s (report %+v)", r.Outcome, want, r)
	}
}

func TestEnsureLatestRowIndexBuildsOnAPlainTable(t *testing.T) {
	db := conceptIndexDB(t)
	ctx := context.Background()
	tbl := newLatestRowTable(t, db, false)
	seedAcrossDays(t, db, tbl, 3, 50)

	r, err := database.EnsureLatestRowIndex(ctx, db, nil, tbl.name, tbl.index)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if r.Outcome != database.LatestRowIndexBuilt || r.Hypertable || r.Chunks != 0 || r.Covering != tbl.index {
		t.Fatalf("report = %+v, want a plain build of %s", r, tbl.index)
	}
	if o := readOracle(t, db, tbl, tbl.index); !o.complete() {
		t.Fatalf("the ensurer reported %s but the index is not complete: %+v", r.Outcome, o)
	}
	mustEnsure(t, db, tbl, string(database.LatestRowIndexValid))

	// The plain table's repair path: an interrupted build's catalog state is
	// dropped and built again, never skipped over.
	requireSuperuser(t, db)
	markInvalid(t, db, tbl, tbl.index)
	mustEnsure(t, db, tbl, string(database.LatestRowIndexRebuilt))
	if o := readOracle(t, db, tbl, tbl.index); !o.complete() {
		t.Fatalf("rebuilt, but the index is not complete: %+v", o)
	}
}

func TestEnsureLatestRowIndexBuildsPerChunkOnAHypertable(t *testing.T) {
	db := conceptIndexDB(t)
	requireTimescale(t, db)
	ctx := context.Background()
	tbl := newLatestRowTable(t, db, true)
	seedAcrossDays(t, db, tbl, 6, 40)
	chunks := chunkCount(t, db, tbl)
	if chunks < 6 {
		t.Fatalf("the fixture made %d chunks, want at least 6", chunks)
	}

	r, err := database.EnsureLatestRowIndex(ctx, db, nil, tbl.name, tbl.index)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if r.Outcome != database.LatestRowIndexBuilt || !r.Hypertable || r.Chunks != chunks {
		t.Fatalf("report = %+v, want a per-chunk build over %d chunks", r, chunks)
	}
	o := readOracle(t, db, tbl, tbl.index)
	if !o.complete() {
		t.Fatalf("the ensurer reported %s but the index is not complete: %+v", r.Outcome, o)
	}
	mustEnsure(t, db, tbl, string(database.LatestRowIndexValid))
}

func TestEnsureLatestRowIndexRepairsAnInvalidRoot(t *testing.T) {
	db := conceptIndexDB(t)
	requireTimescale(t, db)
	requireSuperuser(t, db)
	ctx := context.Background()
	tbl := newLatestRowTable(t, db, true)
	seedAcrossDays(t, db, tbl, 4, 30)
	mustEnsure(t, db, tbl, string(database.LatestRowIndexBuilt))

	// The state a cancelled transaction_per_chunk build leaves, and the one a
	// plain IF NOT EXISTS reports success over.
	markInvalid(t, db, tbl, tbl.index)
	dry, err := database.InspectLatestRowIndex(ctx, db, tbl.name, tbl.index)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if dry.Outcome != database.LatestRowIndexRebuilt || !strings.Contains(strings.Join(dry.Incomplete, ";"), "invalid") {
		t.Fatalf("inspection of an invalid root = %+v, want a rebuild naming it invalid", dry)
	}

	r, err := database.EnsureLatestRowIndex(ctx, db, nil, tbl.name, tbl.index)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if r.Outcome != database.LatestRowIndexRebuilt || r.Dropped == "" {
		t.Fatalf("report = %+v, want the invalid index dropped and rebuilt", r)
	}
	if o := readOracle(t, db, tbl, tbl.index); !o.complete() {
		t.Fatalf("rebuilt, but the index is not complete: %+v", o)
	}
}

func TestEnsureLatestRowIndexRepairsAMissingChunkIndex(t *testing.T) {
	db := conceptIndexDB(t)
	requireTimescale(t, db)
	ctx := context.Background()
	tbl := newLatestRowTable(t, db, true)
	seedAcrossDays(t, db, tbl, 4, 30)
	mustEnsure(t, db, tbl, string(database.LatestRowIndexBuilt))

	// One chunk's index dropped by hand: the root stays VALID, which is the
	// case a check of the root alone would call complete.
	var schema, name string
	if err := db.QueryRowContext(ctx, `
		SELECT n.nspname, ic.relname
		  FROM pg_inherits inh
		  JOIN pg_index i ON i.indrelid = inh.inhrelid
		  JOIN pg_class ic ON ic.oid = i.indexrelid
		  JOIN pg_namespace n ON n.oid = ic.relnamespace
		 WHERE inh.inhparent = to_regclass(?)
		   AND pg_get_indexdef(i.indexrelid) LIKE '%(concept, id, "createdAt" DESC)%'
		 ORDER BY ic.relname LIMIT 1`, tbl.quoted()).Scan(&schema, &name); err != nil {
		t.Fatalf("finding a chunk's index: %v", err)
	}
	lriExec(t, db, `DROP INDEX "`+schema+`"."`+name+`"`)
	if o := readOracle(t, db, tbl, tbl.index); !o.rootUsable || len(o.uncovered) != 1 {
		t.Fatalf("the simulation did not leave a valid root and one uncovered chunk: %+v", o)
	}

	dry, err := database.InspectLatestRowIndex(ctx, db, tbl.name, tbl.index)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if dry.Outcome != database.LatestRowIndexRebuilt || !strings.Contains(strings.Join(dry.Incomplete, ";"), "chunk") {
		t.Fatalf("inspection with a chunk uncovered = %+v, want a rebuild naming the chunk", dry)
	}

	mustEnsure(t, db, tbl, string(database.LatestRowIndexRebuilt))
	if o := readOracle(t, db, tbl, tbl.index); !o.complete() {
		t.Fatalf("rebuilt, but the index is not complete: %+v", o)
	}
}

func TestEnsureLatestRowIndexAcceptsAnEquivalentOperationalIndex(t *testing.T) {
	db := conceptIndexDB(t)
	requireTimescale(t, db)
	ctx := context.Background()
	tbl := newLatestRowTable(t, db, true)
	seedAcrossDays(t, db, tbl, 4, 30)

	// What the deployment owner did in production, under a name of their own
	// -- with an INCLUDE column, which changes nothing the read needs.
	operator := strings.Replace(tbl.index, "_idx", "_ops", 1)
	lriExec(t, db, `CREATE INDEX `+operator+` ON `+tbl.quoted()+
		` (concept, id, "createdAt" DESC) INCLUDE (payload) WITH (timescaledb.transaction_per_chunk)`)

	r, err := database.EnsureLatestRowIndex(ctx, db, nil, tbl.name, tbl.index)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if r.Outcome != database.LatestRowIndexEquivalent || r.Covering != operator {
		t.Fatalf("report = %+v, want the operator's %s accepted", r, operator)
	}
	if o := readOracle(t, db, tbl, tbl.index); o.rootDef != "" {
		t.Fatalf("the ensurer built a duplicate beside the operator's index: %s", o.rootDef)
	}

	// An incomplete index on the canonical name beside it is this migration's
	// to remove, and nothing is built in its place.
	requireSuperuser(t, db)
	lriExec(t, db, `CREATE INDEX `+tbl.index+` ON `+tbl.quoted()+` (concept, id, "createdAt" DESC)`)
	markInvalid(t, db, tbl, tbl.index)
	r, err = database.EnsureLatestRowIndex(ctx, db, nil, tbl.name, tbl.index)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if r.Outcome != database.LatestRowIndexEquivalent || r.Covering != operator || r.Dropped == "" {
		t.Fatalf("report = %+v, want the invalid canonical dropped and the operator's index kept", r)
	}
	if o := readOracle(t, db, tbl, tbl.index); o.rootDef != "" {
		t.Fatalf("the invalid canonical index survived: %s", o.rootDef)
	}
	if o := readOracle(t, db, tbl, operator); !o.rootUsable || len(o.uncovered) != 0 {
		t.Fatalf("the operator's index is no longer complete: %+v", o)
	}
}

func TestEnsureLatestRowIndexReplacesANonEquivalentIndexOnItsName(t *testing.T) {
	db := conceptIndexDB(t)
	requireTimescale(t, db)
	ctx := context.Background()
	tbl := newLatestRowTable(t, db, true)
	seedAcrossDays(t, db, tbl, 4, 30)

	// A partial index squatting on the canonical name: right columns, a
	// fraction of the rows.
	lriExec(t, db, `CREATE INDEX `+tbl.index+` ON `+tbl.quoted()+
		` (concept, id, "createdAt" DESC) WHERE concept = 'v1:test:squatter'`)
	dry, err := database.InspectLatestRowIndex(ctx, db, tbl.name, tbl.index)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if dry.Outcome != database.LatestRowIndexRebuilt || !strings.Contains(strings.Join(dry.Incomplete, ";"), "partial") {
		t.Fatalf("inspection of a partial squatter = %+v, want a rebuild naming it partial", dry)
	}

	r, err := database.EnsureLatestRowIndex(ctx, db, nil, tbl.name, tbl.index)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if r.Outcome != database.LatestRowIndexRebuilt || !strings.Contains(r.Dropped, "WHERE") {
		t.Fatalf("report = %+v, want the partial index dropped (its definition logged) and rebuilt", r)
	}
	if o := readOracle(t, db, tbl, tbl.index); !o.complete() {
		t.Fatalf("rebuilt, but the index is not complete: %+v", o)
	}
}

// A COMPRESSED chunk without the index is reported, not failed: 2.29 builds the
// index on compressed chunks too, but a Timescale that skipped them would
// otherwise leave the migration unable ever to succeed. The same chunk
// uncompressed is a rebuild -- which the case above already pins.
func TestEnsureLatestRowIndexToleratesACompressedChunkWithoutIt(t *testing.T) {
	db := conceptIndexDB(t)
	requireTimescale(t, db)
	ctx := context.Background()
	tbl := newLatestRowTable(t, db, true)
	seedAcrossDays(t, db, tbl, 4, 30)
	mustEnsure(t, db, tbl, string(database.LatestRowIndexBuilt))

	lriExec(t, db, `ALTER TABLE `+tbl.quoted()+` SET (timescaledb.compress,
		timescaledb.compress_segmentby = 'concept', timescaledb.compress_orderby = '"createdAt" DESC')`)
	lriExec(t, db, `SELECT compress_chunk(c) FROM (SELECT c FROM show_chunks('`+tbl.quoted()+`') c ORDER BY 1 LIMIT 2) s`)

	var schema, name string
	if err := db.QueryRowContext(ctx, `
		SELECT n.nspname, ic.relname
		  FROM timescaledb_information.chunks ch
		  JOIN pg_namespace cn ON cn.nspname = ch.chunk_schema
		  JOIN pg_class cc ON cc.relname = ch.chunk_name AND cc.relnamespace = cn.oid
		  JOIN pg_index i ON i.indrelid = cc.oid
		  JOIN pg_class ic ON ic.oid = i.indexrelid
		  JOIN pg_namespace n ON n.oid = ic.relnamespace
		 WHERE ch.hypertable_name = ? AND ch.is_compressed
		   AND pg_get_indexdef(i.indexrelid) LIKE '%(concept, id, "createdAt" DESC)%'
		 ORDER BY ic.relname LIMIT 1`, tbl.name).Scan(&schema, &name); err != nil {
		t.Fatalf("finding a compressed chunk's index: %v", err)
	}
	lriExec(t, db, `DROP INDEX "`+schema+`"."`+name+`"`)

	r, err := database.EnsureLatestRowIndex(ctx, db, nil, tbl.name, tbl.index)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if r.Outcome != database.LatestRowIndexValid || len(r.ExemptWithoutIndex) != 1 ||
		!strings.Contains(r.ExemptWithoutIndex[0], "compressed") {
		t.Fatalf("report = %+v, want valid with the one compressed chunk reported as carrying no index", r)
	}
	if o := readOracle(t, db, tbl, tbl.index); !o.rootUsable || len(o.uncovered) != 1 {
		t.Fatalf("the simulation did not leave exactly the compressed chunk uncovered: %+v", o)
	}
}

// Two nodes migrating at once must not race a build: the second waits on the
// build lock, and one that cannot get it in time names who holds it rather
// than proceeding.
func TestEnsureLatestRowIndexWaitsForTheBuildLock(t *testing.T) {
	db := conceptIndexDB(t)
	ctx := context.Background()
	tbl := newLatestRowTable(t, db, false)
	seedAcrossDays(t, db, tbl, 2, 20)

	holder, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Close() }()
	var holderPid int64
	if err := holder.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&holderPid); err != nil {
		t.Fatal(err)
	}
	key := database.LatestRowIndexLockKey(tbl.name)
	lriExec(t, holder, `SELECT pg_advisory_lock(?)`, key)

	short, cancel := context.WithTimeout(ctx, 600*time.Millisecond)
	_, err = database.EnsureLatestRowIndex(short, db, nil, tbl.name, tbl.index)
	cancel()
	if err == nil {
		t.Fatal("the ensurer proceeded while another session held the build lock")
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("backend %d", holderPid)) {
		t.Fatalf("the error does not name the backend holding the lock (%d): %v", holderPid, err)
	}
	if o := readOracle(t, db, tbl, tbl.index); o.rootDef != "" {
		t.Fatalf("something was built while the lock was held elsewhere: %s", o.rootDef)
	}

	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(400 * time.Millisecond)
		_, _ = holder.ExecContext(ctx, `SELECT pg_advisory_unlock(?)`, key)
	}()
	started := time.Now()
	long, cancelLong := context.WithTimeout(ctx, 15*time.Second)
	defer cancelLong()
	r, err := database.EnsureLatestRowIndex(long, db, nil, tbl.name, tbl.index)
	<-released
	if err != nil {
		t.Fatalf("ensure after the lock came free: %v", err)
	}
	if waited := time.Since(started); waited < 350*time.Millisecond {
		t.Fatalf("the ensurer returned after %s, before the lock was released", waited)
	}
	if r.Outcome != database.LatestRowIndexBuilt {
		t.Fatalf("outcome = %s, want built", r.Outcome)
	}
	if o := readOracle(t, db, tbl, tbl.index); !o.complete() {
		t.Fatalf("built, but not complete: %+v", o)
	}
}

// THE BUILD THE DRIVER GAVE UP ON. pgdriver's read deadline (10 s by default;
// 1 s here) cuts a long CREATE INDEX off on the client while the backend
// keeps building. The ensurer must wait for that orphan -- which still holds
// the build lock -- and then judge the index by inspecting it, rather than
// failing, or finding the index invalid mid-build and dropping it.
//
// A writer holding ROW EXCLUSIVE in an open transaction makes the build wait
// on its ShareLock past the deadline, deterministically; the writer commits
// after 3 s and the orphaned build then finishes. The deadline is a whole
// second rather than a few hundred milliseconds because EVERY statement of the
// ensure runs under it, catalog reads included, and on a shared CI database a
// catalog read that loses a few hundred milliseconds to a neighbour would fail
// the case for a reason that has nothing to do with the orphan.
func TestEnsureLatestRowIndexWaitsOutABuildTheReadDeadlineCutOff(t *testing.T) {
	db := conceptIndexDB(t)
	ctx := context.Background()
	tbl := newLatestRowTable(t, db, false)
	seedAcrossDays(t, db, tbl, 2, 20)

	shortDB := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(
		pgdriver.WithDSN(dbtest.DSN()),
		pgdriver.WithReadTimeout(time.Second),
	)), pgdialect.New())
	defer func() { _ = shortDB.Close() }()

	writer, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback() }()
	lriExec(t, writer, `LOCK TABLE `+tbl.quoted()+` IN ROW EXCLUSIVE MODE`)
	committed := make(chan struct{})
	go func() {
		defer close(committed)
		time.Sleep(3 * time.Second)
		_ = writer.Commit()
	}()

	started := time.Now()
	long, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	r, err := database.EnsureLatestRowIndex(long, shortDB, nil, tbl.name, tbl.index)
	<-committed
	if err != nil {
		t.Fatalf("the ensurer failed a build that finished server-side after the read deadline: %v", err)
	}
	if took := time.Since(started); took < 2800*time.Millisecond {
		t.Fatalf("the ensurer returned after %s, before the writer released the table -- it cannot have seen "+
			"the finished index", took)
	}
	if r.Outcome != database.LatestRowIndexBuilt {
		t.Fatalf("outcome = %s, want built", r.Outcome)
	}
	if o := readOracle(t, db, tbl, tbl.index); !o.complete() {
		t.Fatalf("reported built, but the index is not complete: %+v", o)
	}
}

// ===========================================================================
// LATEST-ROW SEMANTICS AND THE PLANS, OVER REALISTIC VERSION HISTORY
// ===========================================================================

// lriRow is one version in the fixture.
type lriRow struct {
	id, concept string
	at          time.Time
	version     int
}

// The fixture is the shape that produced the incident: a deep-history concept
// (heartbeat-like: few ids, thousands of versions each), a wide one (many
// ids, a few versions each), and a large single-version concept around them,
// all written in TIME order -- concepts and ids interleaved in the heap, the
// way heartbeats land -- across seven one-day chunks. Every version of an id
// sits at a distinct createdAt, and the deep and wide ids' versions span
// several chunks, so the latest row per id is decided ACROSS chunks.
const (
	lriNoise        = 20000
	lriDeepIDs      = 20
	lriDeepVersions = 2000
	lriWideIDs      = 200
	lriWideVersions = 8
)

func latestRowFixture() []lriRow {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	span := 6 * 24 * time.Hour
	rows := make([]lriRow, 0, lriNoise+lriDeepIDs*lriDeepVersions+lriWideIDs*lriWideVersions)
	for g := 1; g <= lriNoise; g++ {
		rows = append(rows, lriRow{fmt.Sprintf("v1:test:noise:%d", g), "v1:test:noise", base.Add(span / lriNoise * time.Duration(g)), 0})
	}
	for i := 1; i <= lriDeepIDs; i++ {
		for k := 0; k < lriDeepVersions; k++ {
			at := base.Add(span / lriDeepVersions * time.Duration(k)).Add(time.Duration(i) * time.Millisecond)
			rows = append(rows, lriRow{fmt.Sprintf("v1:test:deep:%d", i), "v1:test:deep", at, k})
		}
	}
	for i := 1; i <= lriWideIDs; i++ {
		for k := 0; k < lriWideVersions; k++ {
			at := base.Add(time.Hour + time.Duration(k)*17*time.Hour + time.Duration(i)*time.Second)
			rows = append(rows, lriRow{fmt.Sprintf("v1:test:wide:%d", i), "v1:test:wide", at, k})
		}
	}
	sort.SliceStable(rows, func(a, b int) bool { return rows[a].at.Before(rows[b].at) })
	return rows
}

// newestPerID is the expected answer, computed from the fixture itself.
func newestPerID(rows []lriRow, concept string) map[string]lriRow {
	out := map[string]lriRow{}
	for _, r := range rows {
		if r.concept != concept {
			continue
		}
		if cur, ok := out[r.id]; !ok || r.at.After(cur.at) {
			out[r.id] = r
		}
	}
	return out
}

func copyFixture(t *testing.T, db *bun.DB, tbl lriTable, rows []lriRow) {
	t.Helper()
	var buf bytes.Buffer
	for _, r := range rows {
		fmt.Fprintf(&buf, "%s\t%s\t%s\t{\"version\": %d}\n", r.id, r.at.Format("2006-01-02 15:04:05.000000Z07:00"), r.concept, r.version)
	}
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := pgdriver.CopyFrom(ctx, conn, &buf,
		`COPY `+tbl.quoted()+` (id, "createdAt", concept, payload) FROM STDIN`); err != nil {
		t.Fatalf("loading the fixture: %v", err)
	}
}

// readLatest runs the executor's read shape verbatim.
func readLatest(t *testing.T, db *bun.DB, tbl lriTable, concept string) map[string]lriRow {
	t.Helper()
	rows, err := db.QueryContext(context.Background(),
		`SELECT DISTINCT ON (id) id, "createdAt", payload FROM `+tbl.quoted()+
			` WHERE concept = ? ORDER BY id ASC, "createdAt" DESC`, concept)
	if err != nil {
		t.Fatalf("reading %s: %v", concept, err)
	}
	defer rows.Close()
	out := map[string]lriRow{}
	for rows.Next() {
		var (
			r       lriRow
			payload []byte
		)
		if err := rows.Scan(&r.id, &r.at, &payload); err != nil {
			t.Fatal(err)
		}
		var p struct {
			Version int `json:"version"`
		}
		if err := json.Unmarshal(payload, &p); err != nil {
			t.Fatalf("payload %s: %v", payload, err)
		}
		if _, dup := out[r.id]; dup {
			t.Fatalf("the read returned %s twice", r.id)
		}
		r.concept, r.version = concept, p.Version
		out[r.id] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// planNode is one node of EXPLAIN (FORMAT JSON).
type planNode struct {
	NodeType  string     `json:"Node Type"`
	Provider  string     `json:"Custom Plan Provider"`
	IndexName string     `json:"Index Name"`
	IndexCond string     `json:"Index Cond"`
	Plans     []planNode `json:"Plans"`
}

type lriPlan struct{ nodes []planNode }

func (p lriPlan) any(match func(planNode) bool) bool {
	for _, n := range p.nodes {
		if match(n) {
			return true
		}
	}
	return false
}

func (p lriPlan) skipScan() bool {
	return p.any(func(n planNode) bool { return n.Provider == "SkipScan" })
}

// usesIndex matches the hypertable's index or any chunk's copy of it, which
// Timescale names "<chunk>_<index>".
func (p lriPlan) usesIndex(index string) bool {
	return p.any(func(n planNode) bool { return n.IndexName == index || strings.HasSuffix(n.IndexName, "_"+index) })
}

func (p lriPlan) readsConceptThrough(index string) bool {
	return p.any(func(n planNode) bool {
		return (n.NodeType == "Index Scan" || n.NodeType == "Index Only Scan") &&
			(n.IndexName == index || strings.HasSuffix(n.IndexName, "_"+index)) &&
			strings.Contains(n.IndexCond, "concept")
	})
}

func (p lriPlan) sorts() bool {
	return p.any(func(n planNode) bool { return n.NodeType == "Sort" || n.NodeType == "Incremental Sort" })
}

func (p lriPlan) String() string {
	counts := map[string]int{}
	var order []string
	for _, n := range p.nodes {
		label := n.NodeType
		if n.Provider != "" {
			label += "(" + n.Provider + ")"
		}
		if n.IndexName != "" {
			if i := strings.Index(n.IndexName, "_chunk_"); i >= 0 {
				label += " on " + n.IndexName[i+len("_chunk_"):]
			} else {
				label += " on " + n.IndexName
			}
		}
		if counts[label] == 0 {
			order = append(order, label)
		}
		counts[label]++
	}
	parts := make([]string, len(order))
	for i, label := range order {
		parts[i] = fmt.Sprintf("%dx %s", counts[label], label)
	}
	return strings.Join(parts, ", ")
}

// lriCostModel pins the planner inputs an EXPLAIN runs under. The choice the
// planner makes for this read turns on random_page_cost (see below), and a
// test that took whatever the server it happens to run on is tuned to would
// be measuring the server.
type lriCostModel struct {
	name     string
	settings []string
	// composite says this model's honest choice for the deep read is the
	// composite index (measured on TimescaleDB 2.29.2 against this fixture).
	composite bool
}

var lriCostModels = []lriCostModel{
	{
		// The entry preset's memory beside the Postgres default page costs,
		// which no CNPG preset changes: what a production instance plans with.
		name: "production costs (random_page_cost 4)",
		settings: []string{`SET LOCAL random_page_cost = 4`, `SET LOCAL seq_page_cost = 1`,
			`SET LOCAL effective_cache_size = '3GB'`, `SET LOCAL work_mem = '8MB'`},
	},
	{
		// The SSD page cost timescaledb-tune writes, and the CI and scratch
		// TimescaleDB servers run with.
		name: "SSD costs (random_page_cost 1.1)",
		settings: []string{`SET LOCAL random_page_cost = 1.1`, `SET LOCAL seq_page_cost = 1`,
			`SET LOCAL effective_cache_size = '3GB'`, `SET LOCAL work_mem = '8MB'`},
		composite: true,
	},
}

func explainLatestRead(t *testing.T, db *bun.DB, tbl lriTable, concept string, model lriCostModel, skipscan bool) lriPlan {
	t.Helper()
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	settings := append([]string{`SET LOCAL timescaledb.enable_skipscan = ` + map[bool]string{true: "on", false: "off"}[skipscan]}, model.settings...)
	for _, s := range settings {
		lriExec(t, tx, s)
	}
	var raw string
	if err := tx.QueryRowContext(ctx, `EXPLAIN (FORMAT JSON) SELECT DISTINCT ON (id) id, "createdAt", payload FROM `+
		tbl.quoted()+` WHERE concept = '`+concept+`' ORDER BY id ASC, "createdAt" DESC`).Scan(&raw); err != nil {
		t.Fatalf("explain: %v", err)
	}
	var doc []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil || len(doc) != 1 {
		t.Fatalf("parsing the plan (%v): %s", err, raw)
	}
	var p lriPlan
	var walk func(planNode)
	walk = func(n planNode) {
		p.nodes = append(p.nodes, n)
		for _, c := range n.Plans {
			walk(c)
		}
	}
	walk(doc[0].Plan)
	return p
}

// TestLatestRowReadReturnsTheNewestVersionAndNeverTheIncidentPlan is the
// read the index exists for, run the way the executor runs it.
//
// THE NEGATIVE CONTROL COMES FIRST. Before the index exists, the deep read
// must plan as the incident did -- a SkipScan over (id, "createdAt" DESC),
// concept left to a filter -- under BOTH cost models. If it does not, the
// fixture has stopped reproducing what the index prevents and every assertion
// after it would pass over nothing, so that is a failure rather than a skip.
//
// Then, with the index: the newest version per id, exactly, for every
// concept; and under both cost models, with SkipScan enabled and disabled,
// no SkipScan and no read of the id index. Which path the planner then takes
// is a cost decision, measured on TimescaleDB 2.29.2 against this fixture and
// pinned as such: under SSD costs it reads the composite index with an Index
// Cond on concept and no sort; under production costs the deep concept is
// two thirds of the table and a sequential scan plus a sort is cheaper, which
// reads the table once rather than walking every id in it. The latter plan is
// logged rather than asserted beyond "not the incident".
func TestLatestRowReadReturnsTheNewestVersionAndNeverTheIncidentPlan(t *testing.T) {
	db := conceptIndexDB(t)
	requireTimescale(t, db)
	ctx := context.Background()
	tbl := newLatestRowTable(t, db, true)
	fixture := latestRowFixture()
	copyFixture(t, db, tbl, fixture)
	lriExec(t, db, `ANALYZE `+tbl.quoted())

	for _, model := range lriCostModels {
		before := explainLatestRead(t, db, tbl, "v1:test:deep", model, true)
		if !before.skipScan() || !before.usesIndex(tbl.byID) {
			t.Fatalf("under %s, before the index the deep read does not plan as the incident did (a SkipScan "+
				"over %s): %s -- the fixture no longer reproduces what the index prevents", model.name, tbl.byID, before)
		}
	}

	r, err := database.EnsureLatestRowIndex(ctx, db, nil, tbl.name, tbl.index)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if r.Outcome != database.LatestRowIndexBuilt {
		t.Fatalf("outcome = %s, want built", r.Outcome)
	}

	for _, concept := range []string{"v1:test:deep", "v1:test:wide", "v1:test:noise"} {
		want := newestPerID(fixture, concept)
		got := readLatest(t, db, tbl, concept)
		if len(got) != len(want) {
			t.Fatalf("%s: the read returned %d ids, want %d", concept, len(got), len(want))
		}
		for id, w := range want {
			g, ok := got[id]
			if !ok {
				t.Fatalf("%s: the read lost %s", concept, id)
			}
			if !g.at.Equal(w.at) || g.version != w.version {
				t.Fatalf("%s: %s came back as version %d at %s, want the newest, version %d at %s",
					concept, id, g.version, g.at.UTC(), w.version, w.at)
			}
		}
	}

	for _, model := range lriCostModels {
		for _, skipscan := range []bool{true, false} {
			p := explainLatestRead(t, db, tbl, "v1:test:deep", model, skipscan)
			t.Logf("%s, skipscan %v: %s", model.name, skipscan, p)
			if p.skipScan() || p.usesIndex(tbl.byID) {
				t.Fatalf("under %s with skipscan %v the deep read still takes the incident plan: %s", model.name, skipscan, p)
			}
			if model.composite && (!p.readsConceptThrough(tbl.index) || p.sorts()) {
				t.Fatalf("under %s with skipscan %v the deep read does not run through %s in order: %s",
					model.name, skipscan, tbl.index, p)
			}
		}
	}
}
