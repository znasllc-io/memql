package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
)

// THE LATEST-ROW INDEX, ENSURED RATHER THAN ASSUMED (memql#5252).
//
// Every concept read the executor emits is "the latest row per id within one
// concept" -- `SELECT DISTINCT ON (id) ... WHERE concept = $1 ORDER BY id ASC,
// "createdAt" DESC` -- and (concept, id, "createdAt" DESC) is the index that
// keeps that read proportional to the concept it names rather than to the
// table. 20260913000000_memory_nodes_concept_id_created_at_idx.go carries the
// incident that made it load-bearing; this file is how a migration makes sure
// the index is really there.
//
// ===========================================================================
// AN INDEX WITH THE RIGHT NAME IS NOT AN INDEX THAT ANSWERS THE READ
// ===========================================================================
// Measured on TimescaleDB 2.29.2 against a 400-chunk scratch hypertable
// (2026-09-13):
//
//   - a `transaction_per_chunk` build cancelled part-way leaves the root index
//     indisvalid=false, indisready=true with 281 of 400 chunk indexes, and a
//     following `CREATE INDEX IF NOT EXISTS` of the same name prints "already
//     exists, skipping" and SUCCEEDS -- so IF NOT EXISTS alone reports success
//     over an index the planner cannot use;
//   - a build still IN PROGRESS reads exactly the same in pg_index (invalid,
//     ready) and holds no lock on the root between chunks, so the catalog
//     cannot tell "interrupted" from "running" and only
//     pg_stat_progress_create_index can -- which is why nothing here drops an
//     index while another session is building one on the table;
//   - one chunk's index can be dropped by hand, leaving the root VALID and that
//     one chunk read without it.
//
// So ensureLatestRowIndex inspects before it trusts: it decides COMPLETE from
// the catalog, acts only when the index is not complete, and RE-INSPECTS after
// acting. It never reports success over an index it has not seen complete.
//
// ===========================================================================
// COVERAGE IS JUDGED BY SHAPE, CHUNK BY CHUNK, AND BY COLUMN NAME
// ===========================================================================
// TimescaleDB 2.29 records no link from a chunk's index to the hypertable
// index it was made for: no pg_inherits row, no pg_depend row, and
// _timescaledb_catalog.chunk_index no longer exists (all three measured). So a
// chunk counts as covered when it carries ANY valid, ready, live index of the
// latest-row shape -- which is exactly what the planner needs from it, since it
// plans each chunk's scan over that chunk's own indexes.
//
// Key columns are compared by NAME per relation, never by attnum: a chunk
// created after a column was dropped from the root numbers its columns
// differently (measured: root id=2, concept=3; chunk id=1, concept=2).
//
// A COMPRESSED chunk without the index is logged and does not fail the check.
// 2.29 builds the index on compressed chunks too (measured), but a Timescale
// that skipped them would otherwise leave this migration unable ever to
// succeed, and a migration that can never succeed bricks every upgrade behind
// it. A foreign-table chunk (tiered storage) cannot carry an index at all and
// is treated the same way.
//
// ===========================================================================
// HOW IT BUILDS, AND WHY THE PLAIN-TABLE BUILD IS NOT CONCURRENT
// ===========================================================================
// On a hypertable: `CREATE INDEX ... WITH (timescaledb.transaction_per_chunk)`,
// one chunk per transaction, so the write lock is held on one chunk at a time
// rather than on the whole table for the whole build -- the bounded build the
// deployment owner measured in production. It is sent as the ONLY statement of
// its query: two statements in one simple-query message run as an implicit
// transaction block, which transaction_per_chunk refuses (measured). And the
// option is written only for a hypertable, which cannot exist without the
// extension: on a database without it, merely naming the option fails
// (`unrecognized parameter namespace "timescaledb"`, PR #5353).
//
// On a plain table: a plain CREATE INDEX, and deliberately not CONCURRENTLY.
// CREATE INDEX CONCURRENTLY waits for every transaction in the DATABASE holding
// a snapshot older than its own, and that includes a session blocked in
// pg_advisory_lock -- measured: the CIC backend sat on Lock/virtualxid behind
// such a waiter for as long as the lock's holder did. The db-tests lane is
// exactly that shape (dbtest.EnsureSchema: sibling processes block in
// pg_advisory_lock while one migrates, releasing only after the migration
// ends), a cycle Postgres cannot detect because one edge runs through a Go
// process. And a plain MemoryNodes at migration time is an EMPTY one: a fresh
// install creates it plain (20260324000000_initial_setup looks for
// create_hypertable in a `timescaledb` schema while the extension installs
// into `public`) and the post-migration hook converts it after the whole set
// has run. CONCURRENTLY would buy a lock-free build of nothing at the price of
// a hang; a plain build holds its ShareLock for milliseconds.
//
// A DROP runs under a 5 s lock_timeout. DROP INDEX needs ACCESS EXCLUSIVE on
// the table, and a DROP queued behind one long read queues every later read
// and write behind itself -- a migration taking the table down while it
// waits. Refusing after 5 s fails the migration instead, and the next run
// tries again.
//
// ===========================================================================
// THE TEN-SECOND READ DEADLINE, AND WHY THE BUILD LOCK IS A SESSION LOCK
// ===========================================================================
// pgdriver gives every connection a 10 s read deadline (Config.ReadTimeout,
// and nothing in this package's DSNs overrides it). Measured through the
// migration pool: `SELECT pg_sleep(12)` fails at 10.0 s with "i/o timeout",
// with or without a context deadline, and the backend keeps running; a context
// deadline cuts the read the same way and sends no cancel either. The
// migration pool dropping statement_timeout does not lift this: the deadline
// is the CLIENT's. So an index build over a large table outlives its
// statement, and the build carries on server-side as an orphan.
//
// The build lock is a SESSION advisory lock for exactly that reason: it lives
// as long as the backend does, so an orphaned build still holds it, and the
// next run -- this node's, or another node's migrating at the same moment --
// waits for the build to finish instead of finding an invalid index and
// dropping it mid-build. When a statement here is cut off, this run waits for
// its own orphan the same way (the lock comes free when the backend exits) and
// then lets the re-inspection say whether the build worked. The lock is taken
// by POLLING pg_try_advisory_lock, because a blocking pg_advisory_lock is
// itself a statement the read deadline cuts off, leaving one more orphan
// waiting in the queue.

// latestRowIndexKey is the key the latest-row read needs, in order: equality on
// concept, then the DISTINCT ON column, then the version order.
var latestRowIndexKey = [...]struct {
	column     string
	descending bool
}{
	{"concept", false},
	{"id", false},
	{"createdAt", true},
}

// latestRowIndexLockTimeout bounds how long a DROP INDEX may wait for the
// table's ACCESS EXCLUSIVE lock. See the header.
const latestRowIndexLockTimeout = "5s"

// latestRowIndexPoll is the first interval between polls for the build lock
// or for another session's build to finish; it doubles to latestRowIndexPollMax.
const (
	latestRowIndexPoll    = 100 * time.Millisecond
	latestRowIndexPollMax = time.Second
)

// latestRowIndexOutcome names what ensureLatestRowIndex found and did.
type latestRowIndexOutcome string

const (
	// latestRowIndexValid: the canonical index was complete; nothing was done.
	latestRowIndexValid latestRowIndexOutcome = "valid"
	// latestRowIndexEquivalent: another complete index of the same shape --
	// an operator's, under a different name -- already answers the read, so
	// no duplicate was built. A canonical-named index that was not complete
	// was dropped.
	latestRowIndexEquivalent latestRowIndexOutcome = "equivalent"
	// latestRowIndexBuilt: no index existed under the canonical name; it was
	// built.
	latestRowIndexBuilt latestRowIndexOutcome = "built"
	// latestRowIndexRebuilt: the canonical-named index existed but was not
	// complete -- an invalid root, a chunk without its index, or a different
	// index squatting on the name -- so it was dropped and built again.
	latestRowIndexRebuilt latestRowIndexOutcome = "rebuilt"
)

// latestRowIndexReport is what one ensure (or one inspection) found.
type latestRowIndexReport struct {
	Table string
	Index string
	// Outcome is what was done (ensure), or what would be done (inspect).
	Outcome latestRowIndexOutcome
	// Covering is the index that answers the read once the run is done: the
	// canonical one, or the equivalent one an operator built. Empty from an
	// inspection whose outcome is a build.
	Covering string
	// Equivalents lists complete indexes of the latest-row shape other than
	// the canonical one. Beside a valid canonical index they are redundant --
	// every write maintains each of them -- and that is the operator's call.
	Equivalents []string
	// Incomplete says why the canonical index was not complete. Empty when
	// the outcome is valid.
	Incomplete []string
	// Dropped is the definition of the canonical-named index this run
	// dropped, or empty. Logged in full so a deliberately different index an
	// operator had put on the name can be recreated under another.
	Dropped string
	// Hypertable reports whether the table is a TimescaleDB hypertable.
	Hypertable bool
	// Chunks is how many chunks were checked (zero for a plain table).
	Chunks int
	// ExemptWithoutIndex lists chunks that carry no latest-row index and are
	// not required to (compressed, or foreign), each with the reason.
	ExemptWithoutIndex []string
	Duration           time.Duration
}

// indexFacts is one index as the catalog describes it, reduced to what decides
// whether it answers the latest-row read.
type indexFacts struct {
	Name       string
	Definition string // pg_get_indexdef; filled for root indexes only
	Method     string // access method
	Valid      bool
	Ready      bool
	Live       bool
	Partial    bool // carries a WHERE predicate
	Expression bool // indexes an expression rather than columns
	// The KEY columns in order (INCLUDE columns are not key columns and are
	// allowed), whether each sorts descending, and whether each uses its
	// column's default operator class and own collation -- an index built
	// `COLLATE "C"` or with text_pattern_ops cannot serve ORDER BY id under
	// the column's collation, however right its column names look.
	KeyColumns    []string
	KeyDescending []bool
	KeyPlain      []bool
}

// shapeProblem says why this index cannot answer the latest-row read, or ""
// when it can.
func (f indexFacts) shapeProblem() string {
	switch {
	case f.Method != "btree":
		return fmt.Sprintf("it is a %s index; the read needs a btree", f.Method)
	case f.Partial:
		return "it is partial (it carries a WHERE predicate), so it holds only some rows"
	case f.Expression:
		return "it indexes an expression rather than the columns"
	}
	if len(f.KeyColumns) != len(latestRowIndexKey) || len(f.KeyDescending) != len(f.KeyColumns) || len(f.KeyPlain) != len(f.KeyColumns) {
		return fmt.Sprintf("its key is (%s), not (%s)", f.keyText(), latestRowIndexKeyText())
	}
	for i, want := range latestRowIndexKey {
		if f.KeyColumns[i] != want.column || f.KeyDescending[i] != want.descending {
			return fmt.Sprintf("its key is (%s), not (%s)", f.keyText(), latestRowIndexKeyText())
		}
	}
	for i, want := range latestRowIndexKey {
		if !f.KeyPlain[i] {
			return fmt.Sprintf("its %s column carries a non-default operator class or collation", want.column)
		}
	}
	return ""
}

// usableProblem says why the planner may not use this index, or "" when it
// may.
func (f indexFacts) usableProblem() string {
	switch {
	case !f.Live:
		return "being dropped (indislive=false)"
	case !f.Ready:
		return "not ready (indisready=false)"
	case !f.Valid:
		return "invalid (indisvalid=false: an interrupted build, or one still running)"
	}
	return ""
}

// answersRead reports whether this index is complete for the read on its own
// relation.
func (f indexFacts) answersRead() bool {
	return f.shapeProblem() == "" && f.usableProblem() == ""
}

func (f indexFacts) keyText() string {
	parts := make([]string, len(f.KeyColumns))
	for i, column := range f.KeyColumns {
		if column == "" {
			column = "<expression>"
		}
		if i < len(f.KeyDescending) && f.KeyDescending[i] {
			column += " DESC"
		}
		parts[i] = column
	}
	return strings.Join(parts, ", ")
}

func latestRowIndexKeyText() string {
	parts := make([]string, len(latestRowIndexKey))
	for i, k := range latestRowIndexKey {
		parts[i] = k.column
		if k.descending {
			parts[i] += " DESC"
		}
	}
	return strings.Join(parts, ", ")
}

// latestRowIndexColumnsSQL renders the key for CREATE INDEX from the same
// declaration the inspection checks, so the two cannot drift.
func latestRowIndexColumnsSQL() string {
	parts := make([]string, len(latestRowIndexKey))
	for i, k := range latestRowIndexKey {
		parts[i] = quoteIdentifier(k.column)
		if k.descending {
			parts[i] += " DESC"
		}
	}
	return strings.Join(parts, ", ")
}

// chunkFacts is one chunk of a hypertable and its indexes.
type chunkFacts struct {
	Name    string // schema-qualified, for messages
	OID     int64
	Indexes []indexFacts
	// Exempt is why this chunk is not required to carry the index
	// ("compressed", "foreign table"), or empty when it is.
	Exempt string
}

func (c chunkFacts) covered() bool {
	for _, f := range c.Indexes {
		if f.answersRead() {
			return true
		}
	}
	return false
}

// latestRowIndexState is everything the catalog says about the table's
// latest-row indexes.
type latestRowIndexState struct {
	OID        int64
	Schema     string
	Name       string
	Hypertable bool
	Root       []indexFacts
	Chunks     []chunkFacts
}

func (s latestRowIndexState) qualifiedTable() string {
	return quoteIdentifier(s.Schema) + "." + quoteIdentifier(s.Name)
}

// latestRowIndexDecision is what the state calls for.
type latestRowIndexDecision struct {
	Outcome     latestRowIndexOutcome
	Covering    string
	Equivalents []string
	// Drop is the canonical-named index to drop before anything else, or
	// nil.
	Drop *indexFacts
	// Build says the canonical index is to be built.
	Build              bool
	Incomplete         []string
	ExemptWithoutIndex []string
}

// needsAction reports whether the decision changes anything.
func (d latestRowIndexDecision) needsAction() bool {
	return d.Drop != nil || d.Build
}

// decideLatestRowIndex is the whole policy, as a function of the catalog.
//
// The canonical index is COMPLETE when it exists, has the latest-row shape, is
// valid, ready and live on the root, and -- on a hypertable -- every chunk not
// exempt carries a usable latest-row index. Complete is valid. Otherwise a
// complete equivalent index under another name is accepted as it stands (an
// operator's hand-built one: no duplicate), and a canonical-named index that
// is not complete is dropped beside it, because the name belongs to this
// migration. Otherwise the canonical index is built, after dropping whatever
// incomplete index held its name.
func decideLatestRowIndex(st latestRowIndexState, index string) latestRowIndexDecision {
	var d latestRowIndexDecision

	var canonical *indexFacts
	for i := range st.Root {
		f := &st.Root[i]
		if f.Name == index {
			canonical = f
			continue
		}
		if f.answersRead() {
			d.Equivalents = append(d.Equivalents, f.Name)
		}
	}
	sort.Strings(d.Equivalents)

	var uncovered []string
	for _, c := range st.Chunks {
		if c.covered() {
			continue
		}
		if c.Exempt != "" {
			d.ExemptWithoutIndex = append(d.ExemptWithoutIndex, c.Name+" ("+c.Exempt+")")
			continue
		}
		uncovered = append(uncovered, c.Name)
	}

	switch {
	case canonical == nil:
		d.Incomplete = append(d.Incomplete, fmt.Sprintf("no index named %s", index))
	case canonical.shapeProblem() != "":
		d.Incomplete = append(d.Incomplete, fmt.Sprintf("index %s is not a latest-row index: %s", index, canonical.shapeProblem()))
	case canonical.usableProblem() != "":
		d.Incomplete = append(d.Incomplete, fmt.Sprintf("index %s is %s", index, canonical.usableProblem()))
	}
	for _, chunk := range uncovered {
		d.Incomplete = append(d.Incomplete, fmt.Sprintf("chunk %s carries no usable latest-row index", chunk))
	}

	switch {
	case canonical != nil && canonical.answersRead() && len(uncovered) == 0:
		d.Outcome, d.Covering, d.Incomplete = latestRowIndexValid, index, nil
	case len(d.Equivalents) > 0 && len(uncovered) == 0:
		d.Outcome, d.Covering, d.Drop = latestRowIndexEquivalent, d.Equivalents[0], canonical
	case canonical != nil:
		d.Outcome, d.Drop, d.Build = latestRowIndexRebuilt, canonical, true
	default:
		d.Outcome, d.Build = latestRowIndexBuilt, true
	}
	return d
}

// catalogQuerier is what the inspection reads through: a *bun.DB or a
// bun.Conn.
type catalogQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// inspectLatestRowIndex reports what ensureLatestRowIndex would do and why,
// changing nothing and taking no lock.
func inspectLatestRowIndex(ctx context.Context, q catalogQuerier, table, index string) (latestRowIndexReport, error) {
	report := latestRowIndexReport{Table: table, Index: index}
	st, err := readLatestRowIndexState(ctx, q, table)
	if err != nil {
		return report, err
	}
	d := decideLatestRowIndex(st, index)
	report.fill(st, d)
	return report, nil
}

func (r *latestRowIndexReport) fill(st latestRowIndexState, d latestRowIndexDecision) {
	r.Outcome = d.Outcome
	r.Covering = d.Covering
	r.Equivalents = d.Equivalents
	r.Incomplete = d.Incomplete
	r.Hypertable = st.Hypertable
	r.Chunks = len(st.Chunks)
	r.ExemptWithoutIndex = d.ExemptWithoutIndex
}

// ensureLatestRowIndex makes sure table carries a complete latest-row index
// under the name index, or a complete equivalent one, and reports what it
// found and did. It runs on one dedicated connection holding the table's
// build lock, and returns an error naming exactly what is still incomplete
// rather than ever reporting success over an index it has not seen complete.
//
// Idempotent, and cheap when there is nothing to do: one inspection.
func ensureLatestRowIndex(ctx context.Context, db *bun.DB, logger *slog.Logger, table, index string) (latestRowIndexReport, error) {
	if logger == nil {
		logger = slog.Default()
	}
	started := time.Now()
	report := latestRowIndexReport{Table: table, Index: index}

	s := &latestRowIndexSession{db: db, logger: logger, table: table, key: latestRowIndexLockKey(table)}
	if err := s.lock(ctx); err != nil {
		return report, err
	}
	defer s.close()

	st, d, err := s.settle(ctx, index)
	if err != nil {
		return report, err
	}
	report.fill(st, d)

	if d.needsAction() {
		if d.Drop != nil {
			logger.Warn("latest-row index: dropping the incomplete index on the canonical name",
				"table", table, "index", index, "definition", d.Drop.Definition,
				"why", strings.Join(d.Incomplete, "; "))
			if err := s.drop(ctx, st, d.Drop.Name); err != nil {
				return report, fmt.Errorf("latest-row index %s on %s: dropping the incomplete index: %w", index, table, err)
			}
			report.Dropped = d.Drop.Definition
		}
		if d.Build {
			if err := s.build(ctx, st, index); err != nil {
				return report, fmt.Errorf("latest-row index %s on %s: building it: %w", index, table, err)
			}
		}

		// THE RE-INSPECTION IS THE RESULT. A build statement that returned
		// cleanly, or one cut off client-side whose orphan has since exited,
		// says nothing about what the catalog now holds.
		final, err := readLatestRowIndexState(ctx, s.conn, table)
		if err != nil {
			return report, fmt.Errorf("latest-row index %s on %s: re-inspecting after the %s: %w", index, table, d.Outcome, err)
		}
		after := decideLatestRowIndex(final, index)
		if after.needsAction() {
			return report, fmt.Errorf("latest-row index %s on %s is still not complete after the %s: %s",
				index, table, d.Outcome, strings.Join(after.Incomplete, "; "))
		}
		report.Covering = after.Covering
		report.Equivalents = after.Equivalents
		report.Chunks = len(final.Chunks)
		report.ExemptWithoutIndex = after.ExemptWithoutIndex
	}

	if len(report.ExemptWithoutIndex) > 0 {
		logger.Warn("latest-row index: chunks exempt from the index carry none; reads of their rows do not use it",
			"table", table, "index", index, "chunks", report.ExemptWithoutIndex)
	}
	report.Duration = time.Since(started)
	logger.Info("latest-row index ensured",
		"table", table, "index", index, "outcome", string(report.Outcome),
		"covering", report.Covering, "equivalents", report.Equivalents,
		"hypertable", report.Hypertable, "chunks", report.Chunks,
		"duration", report.Duration.Round(time.Millisecond).String())
	return report, nil
}

// latestRowIndexLockKey is the session advisory-lock key for one table's
// latest-row index build: FNV-1a over a fixed prefix and the table name, so
// every node migrating the same table contends on the same key and a
// different table never does.
func latestRowIndexLockKey(table string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("memql:latest-row-index:" + table))
	return int64(h.Sum64())
}

// latestRowIndexSession is one dedicated connection holding the build lock.
// Every statement of an ensure runs on it -- the migration pool has ONE
// connection, so a statement sent through the pool while this holds it would
// wait for itself.
type latestRowIndexSession struct {
	db     *bun.DB
	logger *slog.Logger
	table  string
	key    int64

	conn   bun.Conn
	open   bool
	pid    int64 // this session's backend, named when it is orphaned
	locked bool
}

// lock takes a fresh connection and polls pg_try_advisory_lock on it until
// the lock is held or ctx ends. See the header for why it polls.
func (s *latestRowIndexSession) lock(ctx context.Context) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("latest-row index on %s: taking a connection: %w", s.table, err)
	}
	var pid int64
	if err := conn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		_ = conn.Close()
		return fmt.Errorf("latest-row index on %s: reading the backend pid: %w", s.table, err)
	}

	// gaveUp names the holder through the pool, AFTER releasing this
	// connection: a context that ended mid-poll can leave the connection
	// unusable, and the migration pool has only the one.
	gaveUp := func() error {
		_ = conn.Close()
		describeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return fmt.Errorf("latest-row index on %s: the build lock is held by %s -- a build from another "+
			"node, or an earlier run's that is still going; it finishes on its own and the next run verifies it: %w",
			s.table, describeAdvisoryLockHolder(describeCtx, s.db, s.key), ctx.Err())
	}

	wait := latestRowIndexPoll
	logged := false
	for {
		var held bool
		if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock(?)`, s.key).Scan(&held); err != nil {
			if ctx.Err() != nil {
				return gaveUp()
			}
			_ = conn.Close()
			return fmt.Errorf("latest-row index on %s: taking the build lock: %w", s.table, err)
		}
		if held {
			s.conn, s.open, s.pid, s.locked = conn, true, pid, true
			return nil
		}
		if !logged {
			s.logger.Warn("latest-row index: another session holds the build lock; waiting for it",
				"table", s.table, "holder", describeAdvisoryLockHolder(ctx, conn, s.key))
			logged = true
		}
		select {
		case <-ctx.Done():
			return gaveUp()
		case <-time.After(wait):
		}
		if wait < latestRowIndexPollMax {
			wait *= 2
		}
	}
}

// settle inspects the table and decides; when the decision would change
// something while another session is building an index on the table, it
// waits for that build to end and inspects again. An in-progress build and
// an interrupted one are the same in pg_index (see the header), and dropping
// one that is running breaks it.
//
// A build by a role this one cannot see in pg_stat_progress_create_index is
// invisible here. That is why the re-inspection after acting, not this check,
// is what a successful return rests on.
func (s *latestRowIndexSession) settle(ctx context.Context, index string) (latestRowIndexState, latestRowIndexDecision, error) {
	wait := latestRowIndexPoll
	logged := false
	for {
		st, err := readLatestRowIndexState(ctx, s.conn, s.table)
		if err != nil {
			return st, latestRowIndexDecision{}, fmt.Errorf("latest-row index %s on %s: inspecting: %w", index, s.table, err)
		}
		d := decideLatestRowIndex(st, index)
		if !d.needsAction() {
			return st, d, nil
		}
		builds, err := otherIndexBuilds(ctx, s.conn, st)
		if err != nil {
			return st, d, fmt.Errorf("latest-row index %s on %s: reading index builds in progress: %w", index, s.table, err)
		}
		if len(builds) == 0 {
			return st, d, nil
		}
		if !logged {
			s.logger.Warn("latest-row index: another session is building an index on the table; waiting before acting",
				"table", s.table, "index", index, "builds", builds)
			logged = true
		}
		select {
		case <-ctx.Done():
			return st, d, fmt.Errorf("latest-row index %s on %s: waited for index builds still running (%s): %w",
				index, s.table, strings.Join(builds, "; "), ctx.Err())
		case <-time.After(wait):
		}
		if wait < latestRowIndexPollMax {
			wait *= 2
		}
	}
}

// drop removes the canonical-named index under a bounded lock wait. Plain
// DROP INDEX on both layouts: on a hypertable it cascades to every chunk's
// index (measured, including an interrupted build's 281), and on a plain
// table CONCURRENTLY would reintroduce the snapshot wait the header explains.
func (s *latestRowIndexSession) drop(ctx context.Context, st latestRowIndexState, index string) error {
	if _, err := s.conn.ExecContext(ctx, `SET lock_timeout = '`+latestRowIndexLockTimeout+`'`); err != nil {
		return err
	}
	err := s.ddl(ctx, `DROP INDEX IF EXISTS `+quoteIdentifier(st.Schema)+`.`+quoteIdentifier(index))
	if s.open {
		_, _ = s.conn.ExecContext(ctx, `RESET lock_timeout`)
	}
	return err
}

// build creates the canonical index: per chunk on a hypertable, plainly on a
// plain table (see the header for both halves).
func (s *latestRowIndexSession) build(ctx context.Context, st latestRowIndexState, index string) error {
	stmt := `CREATE INDEX IF NOT EXISTS ` + quoteIdentifier(index) + ` ON ` + st.qualifiedTable() +
		` (` + latestRowIndexColumnsSQL() + `)`
	if st.Hypertable {
		stmt += ` WITH (timescaledb.transaction_per_chunk)`
	}
	return s.ddl(ctx, stmt)
}

// ddl runs one statement on the session. A statement the driver's read
// deadline cut off is still running in this session's backend, which still
// holds the build lock: ddl waits for that backend to exit -- the lock comes
// free when it does -- takes the lock again on a fresh connection, and returns
// nil. Whether the statement did what it was for is then the re-inspection's
// to say, never this return value's.
func (s *latestRowIndexSession) ddl(ctx context.Context, stmt string) error {
	_, err := s.conn.ExecContext(ctx, stmt)
	if err == nil || !isClientReadDeadline(err) {
		return err
	}
	orphan := s.pid
	s.logger.Warn("latest-row index: the statement outlived the driver's read deadline and is still running "+
		"server-side; waiting for its backend to finish",
		"table", s.table, "backend", orphan, "statement", stmt)
	s.discard()
	if err := s.lock(ctx); err != nil {
		return fmt.Errorf("%q is still running server-side in backend %d, which holds the build lock "+
			"(it finishes on its own and the next run verifies the result): %w", stmt, orphan, err)
	}
	return nil
}

// discard drops the session's connection without returning it to the pool:
// a connection whose read was cut off mid-statement is not reusable, and one
// still holding a session lock must never go back to the pool holding it.
func (s *latestRowIndexSession) discard() {
	if !s.open {
		return
	}
	_ = s.conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = s.conn.Close()
	s.open, s.locked = false, false
}

// close releases the build lock and the connection. If the unlock cannot be
// confirmed the connection is discarded rather than pooled, so the lock ends
// with the session instead of outliving it on an idle pooled connection.
func (s *latestRowIndexSession) close() {
	if !s.open {
		return
	}
	if s.locked {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var released bool
		err := s.conn.QueryRowContext(ctx, `SELECT pg_advisory_unlock(?)`, s.key).Scan(&released)
		cancel()
		if err != nil || !released {
			s.discard()
			return
		}
		s.locked = false
	}
	_ = s.conn.Close()
	s.open = false
}

// isClientReadDeadline reports whether err is the driver's read deadline
// cutting a statement off on the client side -- the case where the statement
// is still running on the server.
//
// A context error is NOT that case, although context.DeadlineExceeded is a
// net.Error whose Timeout() is true: database/sql returns it when the context
// ended before the statement was sent, so nothing is running to wait for.
func isClientReadDeadline(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// describeAdvisoryLockHolder names the backend holding key, for a log line or
// an error: pid, application, state, how long its statement has run, and the
// statement. A bigint advisory key is stored split across classid (high half)
// and objid (low half) with objsubid = 1.
func describeAdvisoryLockHolder(ctx context.Context, q catalogQuerier, key int64) string {
	hi, lo := int64(uint64(key)>>32), int64(uint64(key)&0xffffffff)
	var (
		pid        int64
		app, state string
		seconds    int64
		query      string
	)
	err := q.QueryRowContext(ctx, `
SELECT a.pid, COALESCE(a.application_name, ''), COALESCE(a.state, ''),
       COALESCE(round(extract(epoch FROM now() - a.query_start))::int8, 0),
       left(COALESCE(a.query, ''), 160)
  FROM pg_locks l
  JOIN pg_stat_activity a ON a.pid = l.pid
 WHERE l.locktype = 'advisory' AND l.granted AND l.objsubid = 1
   AND l.classid::int8 = ? AND l.objid::int8 = ?
 LIMIT 1`, hi, lo).Scan(&pid, &app, &state, &seconds, &query)
	if err != nil {
		return "an unidentified session"
	}
	return fmt.Sprintf("backend %d (application %q, %s for %ds: %s)", pid, app, state, seconds, query)
}

// otherIndexBuilds lists index builds other sessions are running on the table
// or any of its chunks.
func otherIndexBuilds(ctx context.Context, q catalogQuerier, st latestRowIndexState) ([]string, error) {
	oids := []int64{st.OID}
	for _, c := range st.Chunks {
		oids = append(oids, c.OID)
	}
	rows, err := q.QueryContext(ctx, `
SELECT p.pid, COALESCE(a.application_name, ''), p.phase,
       COALESCE(p.index_relid::regclass::text, ''),
       COALESCE(round(extract(epoch FROM now() - a.query_start))::int8, 0)
  FROM pg_stat_progress_create_index p
  LEFT JOIN pg_stat_activity a ON a.pid = p.pid
 WHERE p.pid <> pg_backend_pid()
   AND p.relid = ANY(?::oid[])
 ORDER BY p.pid`, pgdialect.Array(oids))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var (
			pid           int64
			app, phase    string
			indexName     string
			runningForSec int64
		)
		if err := rows.Scan(&pid, &app, &phase, &indexName, &runningForSec); err != nil {
			return nil, err
		}
		out = append(out, fmt.Sprintf("backend %d (application %q) building %s, %s, for %ds", pid, app, indexName, phase, runningForSec))
	}
	return out, rows.Err()
}

// readLatestRowIndexState reads the table, whether it is a hypertable, its
// chunks and every index on the table and its chunks.
func readLatestRowIndexState(ctx context.Context, q catalogQuerier, table string) (latestRowIndexState, error) {
	var st latestRowIndexState
	var relkind string
	err := q.QueryRowContext(ctx, `
SELECT c.oid::int8, n.nspname, c.relname, c.relkind::text
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE c.oid = to_regclass(?)`, quoteIdentifier(table)).Scan(&st.OID, &st.Schema, &st.Name, &relkind)
	if errors.Is(err, sql.ErrNoRows) {
		return st, fmt.Errorf("table %s does not exist", quoteIdentifier(table))
	}
	if err != nil {
		return st, fmt.Errorf("resolving table %s: %w", quoteIdentifier(table), err)
	}
	if relkind != "r" {
		return st, fmt.Errorf("%s is not an ordinary table (relkind %q)", st.qualifiedTable(), relkind)
	}

	// The information view exists only with the extension, so ask
	// pg_extension first: on plain Postgres even naming the view fails.
	var extension bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb')`).Scan(&extension); err != nil {
		return st, fmt.Errorf("checking for the timescaledb extension: %w", err)
	}
	if extension {
		if err := q.QueryRowContext(ctx, `
SELECT EXISTS (SELECT 1 FROM timescaledb_information.hypertables
                WHERE hypertable_schema = ? AND hypertable_name = ?)`, st.Schema, st.Name).Scan(&st.Hypertable); err != nil {
			return st, fmt.Errorf("checking whether %s is a hypertable: %w", st.qualifiedTable(), err)
		}
	}
	if st.Hypertable {
		if st.Chunks, err = readChunks(ctx, q, st); err != nil {
			return st, err
		}
	}

	oids := []int64{st.OID}
	byOID := map[int64]*chunkFacts{}
	for i := range st.Chunks {
		oids = append(oids, st.Chunks[i].OID)
		byOID[st.Chunks[i].OID] = &st.Chunks[i]
	}
	rows, err := q.QueryContext(ctx, `
SELECT i.indrelid::int8,
       ic.relname,
       am.amname,
       i.indisvalid, i.indisready, i.indislive,
       i.indpred IS NOT NULL,
       i.indexprs IS NOT NULL,
       ARRAY(SELECT COALESCE(a.attname::text, '')
               FROM generate_series(0, i.indnkeyatts - 1) AS k
               LEFT JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = i.indkey[k]
              ORDER BY k),
       ARRAY(SELECT (i.indoption[k] & 1) = 1
               FROM generate_series(0, i.indnkeyatts - 1) AS k
              ORDER BY k),
       ARRAY(SELECT COALESCE(oc.opcdefault, false) AND a.attcollation IS NOT DISTINCT FROM i.indcollation[k]
               FROM generate_series(0, i.indnkeyatts - 1) AS k
               LEFT JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = i.indkey[k]
               LEFT JOIN pg_opclass oc ON oc.oid = i.indclass[k]
              ORDER BY k),
       CASE WHEN i.indrelid = ?::oid THEN pg_get_indexdef(i.indexrelid) ELSE '' END
  FROM pg_index i
  JOIN pg_class ic ON ic.oid = i.indexrelid
  JOIN pg_am am ON am.oid = ic.relam
 WHERE i.indrelid = ANY(?::oid[])
 ORDER BY i.indrelid, ic.relname`, st.OID, pgdialect.Array(oids))
	if err != nil {
		return st, fmt.Errorf("reading the indexes of %s: %w", st.qualifiedTable(), err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			rel int64
			f   indexFacts
		)
		if err := rows.Scan(&rel, &f.Name, &f.Method, &f.Valid, &f.Ready, &f.Live, &f.Partial, &f.Expression,
			pgdialect.Array(&f.KeyColumns), pgdialect.Array(&f.KeyDescending), pgdialect.Array(&f.KeyPlain),
			&f.Definition); err != nil {
			return st, fmt.Errorf("reading the indexes of %s: %w", st.qualifiedTable(), err)
		}
		if rel == st.OID {
			st.Root = append(st.Root, f)
		} else if c, ok := byOID[rel]; ok {
			c.Indexes = append(c.Indexes, f)
		}
	}
	if err := rows.Err(); err != nil {
		return st, fmt.Errorf("reading the indexes of %s: %w", st.qualifiedTable(), err)
	}
	return st, nil
}

// readChunks lists a hypertable's chunks -- its pg_inherits children -- and
// marks the ones exempt from carrying the index.
func readChunks(ctx context.Context, q catalogQuerier, st latestRowIndexState) ([]chunkFacts, error) {
	compressed := map[string]bool{}
	rows, err := q.QueryContext(ctx, `
SELECT chunk_schema, chunk_name
  FROM timescaledb_information.chunks
 WHERE hypertable_schema = ? AND hypertable_name = ? AND is_compressed`, st.Schema, st.Name)
	if err != nil {
		return nil, fmt.Errorf("reading the compressed chunks of %s: %w", st.qualifiedTable(), err)
	}
	for rows.Next() {
		var schema, name string
		if err := rows.Scan(&schema, &name); err != nil {
			rows.Close()
			return nil, fmt.Errorf("reading the compressed chunks of %s: %w", st.qualifiedTable(), err)
		}
		compressed[schema+"."+name] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the compressed chunks of %s: %w", st.qualifiedTable(), err)
	}

	rows, err = q.QueryContext(ctx, `
SELECT c.oid::int8, n.nspname, c.relname, c.relkind::text
  FROM pg_inherits inh
  JOIN pg_class c ON c.oid = inh.inhrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE inh.inhparent = ?::oid
 ORDER BY n.nspname, c.relname`, st.OID)
	if err != nil {
		return nil, fmt.Errorf("reading the chunks of %s: %w", st.qualifiedTable(), err)
	}
	defer rows.Close()
	var chunks []chunkFacts
	for rows.Next() {
		var (
			c            chunkFacts
			schema, name string
			relkind      string
		)
		if err := rows.Scan(&c.OID, &schema, &name, &relkind); err != nil {
			return nil, fmt.Errorf("reading the chunks of %s: %w", st.qualifiedTable(), err)
		}
		c.Name = schema + "." + name
		switch {
		case relkind == "f":
			c.Exempt = "foreign table"
		case compressed[c.Name]:
			c.Exempt = "compressed"
		}
		chunks = append(chunks, c)
	}
	return chunks, rows.Err()
}
