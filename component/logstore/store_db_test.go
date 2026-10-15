package logstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/logger"
)

// The store against real Postgres (design J, the db-gated half).
//
// The pure tests cover the handler, the sink's bounds, the SQL text and the
// sweep's ordering against memory. These are the assertions only a table can
// make: that a line written through the PRODUCTION handler chain comes back
// through Search with every facet, that Tail pages from a cursor and from
// none, that Sources counts, that an OS line lands stamped, and that a day
// round-trips through the archive and back.

func testDB(t *testing.T) *bun.DB {
	t.Helper()
	dsn := dbtest.DSN()
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	if err := db.PingContext(context.Background()); err != nil {
		dbtest.Unreachable(t, "log store DB test", dsn, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// uniqueSuffix keeps runs from colliding: the table is shared across the
// whole lane and across sessions on a developer machine.
func uniqueSuffix() string {
	return time.Now().UTC().Format("20060102T150405.000000000")
}

func cleanupNode(t *testing.T, db *bun.DB, node string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = db.DB.ExecContext(context.Background(), `DELETE FROM log_line WHERE node = $1`, node)
	})
}

func startDBSink(t *testing.T, db *bun.DB, node string) *Sink {
	t.Helper()
	s := NewSink(db, SinkOptions{QueueSize: 1000, MaxBatch: 50, FlushInterval: 20 * time.Millisecond,
		MaxLinesPerSecond: 100000, NodeType: "bff", Node: node, Logger: discard()})
	s.Start()
	return s
}

func flushSink(t *testing.T, s *Sink) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	st := s.Stats()
	if st.InsertErrors > 0 || st.Dropped() > 0 {
		t.Fatalf("the sink dropped or failed while writing the fixture: %+v", st)
	}
}

func ids(rows []Row) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Message)
	}
	return out
}

// Written through the production chain -- logger.New's redactor -> fanout ->
// store handler -> the process sink -- and read back through every facet.
func TestWriteThroughTheHandlerChainAndReadBackWithEveryFacet(t *testing.T) {
	db := testDB(t)
	sfx := uniqueSuffix()
	node := "node-" + sfx
	comp, comp2 := "dbtest.alpha."+sfx, "dbtest.beta."+sfx
	cleanupNode(t, db, node)

	s := startDBSink(t, db, node)
	// The pre-boot ring holds the lines TestMain's migration wrote before any
	// sink existed, and SetSink drains it -- which is the designed behaviour
	// (boot lines are kept) and not what this test measures. Drain it into
	// nothing first, so the node facet below counts only this test's lines.
	logger.SetSink(noopSink{})
	logger.SetSink(s)
	t.Cleanup(func() { logger.SetSink(nil) })

	log := logger.New(common.ComponentName("dbtest"), io.Discard, slog.LevelInfo)
	before := time.Now().UTC().Add(-time.Second)
	log.Info("alpha "+sfx, "component", comp, logger.Subject("v1:test:thing", "thing-"+sfx), "token", "mql_pat_secret", "count", 3)
	log.Warn("beta "+sfx, "component", comp, "run", "review-run-"+sfx)
	log.Error("gamma "+sfx, "component", comp2, "app", "files")
	after := time.Now().UTC().Add(time.Second)
	flushSink(t, s)

	window := Query{WindowStart: before, WindowEnd: after, Nodes: []string{node}}
	all, err := Search(context.Background(), db, window)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("Search returned %d rows for node %s, want 3: %v", len(all), node, ids(all))
	}
	// Newest first.
	if all[0].Message != "gamma "+sfx || all[2].Message != "alpha "+sfx {
		t.Errorf("order must be newest first: %v", ids(all))
	}
	alpha := all[2]
	if alpha.Subject != "thing-"+sfx || alpha.SubjectConcept != "v1:test:thing" || alpha.NodeType != "bff" || alpha.Level != "info" {
		t.Errorf("alpha row: %+v", alpha)
	}
	var attrs map[string]any
	if err := json.Unmarshal(alpha.Attributes, &attrs); err != nil {
		t.Fatalf("attributes: %v", err)
	}
	if attrs["token"] != logger.RedactedPlaceholder || attrs["count"] != float64(3) {
		t.Errorf("attributes must be the redacted set: %v", attrs)
	}
	if _, leaked := attrs["subject.id"]; leaked {
		t.Errorf("the subject was stored as an attribute as well as a column: %v", attrs)
	}

	facet := func(name string, q Query, want ...string) {
		t.Helper()
		q.WindowStart, q.WindowEnd = before, after
		if len(q.Nodes) == 0 {
			q.Nodes = []string{node}
		}
		rows, err := Search(context.Background(), db, q)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := ids(rows)
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%s: got %v, want %v", name, got, want)
		}
	}
	facet("components", Query{Components: []string{comp}}, "beta "+sfx, "alpha "+sfx)
	facet("levels", Query{Levels: []string{"warn", "error"}}, "gamma "+sfx, "beta "+sfx)
	facet("nodeTypes", Query{NodeTypes: []string{"bff"}}, "gamma "+sfx, "beta "+sfx, "alpha "+sfx)
	facet("nodeTypes miss", Query{NodeTypes: []string{"voice"}})
	facet("subject", Query{Subject: "thing-" + sfx}, "alpha "+sfx)
	facet("subjectConcept", Query{Subject: "thing-" + sfx, SubjectConcept: "v1:test:thing"}, "alpha "+sfx)
	facet("subjectConcept miss", Query{Subject: "thing-" + sfx, SubjectConcept: "v1:test:other"})
	facet("text", Query{Text: "BETA " + sfx[:8]}, "beta "+sfx)
	facet("text reference", Query{Text: "review-run-" + sfx}, "beta "+sfx)
	facet("text subject", Query{Text: "thing-" + sfx}, "alpha "+sfx)
	facet("text escaped", Query{Text: "100%"})
	facet("apps", Query{Apps: []string{"files"}}, "gamma "+sfx)
	// THE SCOPE RULE: apps OR subjectConcepts, one predicate.
	facet("scope OR", Query{Apps: []string{"files"}, SubjectConcepts: []string{"v1:test:thing"}}, "gamma "+sfx, "alpha "+sfx)
	facet("scope OR ANDs with level", Query{Apps: []string{"files"}, SubjectConcepts: []string{"v1:test:thing"}, Levels: []string{"info"}}, "alpha "+sfx)
	facet("limit", Query{Limit: 1}, "gamma "+sfx)

	// Keyset paging older: after the newest, the next two.
	page, err := Search(context.Background(), db, Query{WindowStart: before, WindowEnd: after, Nodes: []string{node}, BeforeAt: all[0].OccurredAt, BeforeId: all[0].ID})
	if err != nil {
		t.Fatalf("keyset page: %v", err)
	}
	if strings.Join(ids(page), "|") != "beta "+sfx+"|alpha "+sfx {
		t.Errorf("keyset page: %v", ids(page))
	}

	// Tail with no cursor: the newest limit rows, ASCENDING.
	tail, err := Tail(context.Background(), db, Query{Nodes: []string{node}, Limit: 2})
	if err != nil {
		t.Fatalf("Tail baseline: %v", err)
	}
	if strings.Join(ids(tail), "|") != "beta "+sfx+"|gamma "+sfx {
		t.Errorf("tail baseline must be the newest two ascending: %v", ids(tail))
	}
	// Tail from a cursor: strictly newer, ascending; from the newest, empty.
	more, err := Tail(context.Background(), db, Query{Nodes: []string{node}, AfterAt: all[2].OccurredAt, AfterId: all[2].ID})
	if err != nil {
		t.Fatalf("Tail cursor: %v", err)
	}
	if strings.Join(ids(more), "|") != "beta "+sfx+"|gamma "+sfx {
		t.Errorf("tail from the oldest: %v", ids(more))
	}
	none, err := Tail(context.Background(), db, Query{Nodes: []string{node}, AfterAt: all[0].OccurredAt, AfterId: all[0].ID})
	if err != nil {
		t.Fatalf("Tail at head: %v", err)
	}
	if len(none) != 0 {
		t.Errorf("tail from the newest must be empty, got %v", ids(none))
	}

	// Sources: one row per component, per (nodeType, node), per app.
	sources, err := Sources(context.Background(), db, before, after)
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	counts := map[string]int64{}
	for _, src := range sources {
		counts[src.Kind+":"+src.NodeType+":"+src.Value] = src.Count
	}
	if counts["component::"+comp] != 2 || counts["component::"+comp2] != 1 {
		t.Errorf("component sources: %v", counts)
	}
	if counts["node:bff:"+node] != 3 {
		t.Errorf("node source: %v", counts)
	}
	if counts["app::files"] < 1 {
		t.Errorf("app source: %v", counts)
	}

	// Status sees the table.
	st, err := Status(context.Background(), db)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.OldestAt == nil || st.NewestAt == nil || st.NewestAt.Before(before) {
		t.Errorf("status bounds: %+v", st)
	}
}

func TestOSWriteIsStampedUserAndNodeTypeOS(t *testing.T) {
	db := testDB(t)
	sfx := uniqueSuffix()
	node := "node-os-" + sfx
	s := startDBSink(t, db, node)
	session, user := "os-"+sfx[:12], "user-"+sfx
	t.Cleanup(func() {
		_, _ = db.DB.ExecContext(context.Background(), `DELETE FROM log_line WHERE user_id = $1`, user)
	})

	now := time.Now().UTC()
	res := s.WriteLines([]logger.Line{
		{At: now, Level: slog.LevelError, Component: "os.files", App: "files", Message: "os error " + sfx, Attributes: map[string]any{"repeat": 2}},
		{At: now, Level: slog.LevelWarn, Component: "os.shell", Message: "os warn " + sfx},
	}, Stamp{NodeType: NodeTypeOS, Node: "", Session: session, UserId: user})
	if res.Accepted != 2 {
		t.Fatalf("WriteLines: %+v", res)
	}
	flushSink(t, s)

	rows, err := Search(context.Background(), db, Query{WindowStart: now.Add(-time.Minute), WindowEnd: now.Add(time.Minute), UserId: user})
	if err != nil {
		t.Fatalf("Search by userId: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows by userId, want 2", len(rows))
	}
	for _, r := range rows {
		if r.NodeType != NodeTypeOS || r.Node != "" || r.Session != session || r.UserId != user {
			t.Errorf("OS row: %+v", r)
		}
	}
	bySession, err := Search(context.Background(), db, Query{WindowStart: now.Add(-time.Minute), WindowEnd: now.Add(time.Minute), Session: session, Apps: []string{"files"}})
	if err != nil {
		t.Fatalf("Search by session+app: %v", err)
	}
	if len(bySession) != 1 || bySession[0].App != "files" {
		t.Errorf("session + app facet: %v", ids(bySession))
	}
	nodeTypes, err := Search(context.Background(), db, Query{WindowStart: now.Add(-time.Minute), WindowEnd: now.Add(time.Minute), UserId: user, NodeTypes: []string{NodeTypeOS}})
	if err != nil || len(nodeTypes) != 2 {
		t.Errorf("nodeTypes=os facet: %d %v", len(nodeTypes), err)
	}
}

// Two sinks with two node ids in one process, both present in a read.
func TestTwoSinksTwoNodesBothPresentInARead(t *testing.T) {
	db := testDB(t)
	sfx := uniqueSuffix()
	nodeA, nodeB := "node-a-"+sfx, "node-b-"+sfx
	cleanupNode(t, db, nodeA)
	cleanupNode(t, db, nodeB)
	a, b := startDBSink(t, db, nodeA), startDBSink(t, db, nodeB)
	now := time.Now().UTC()
	a.Write(logger.Line{At: now, Level: slog.LevelInfo, Component: "svc", Message: "from a " + sfx})
	b.Write(logger.Line{At: now, Level: slog.LevelInfo, Component: "svc", Message: "from b " + sfx})
	flushSink(t, a)
	flushSink(t, b)
	rows, err := Search(context.Background(), db, Query{WindowStart: now.Add(-time.Minute), WindowEnd: now.Add(time.Minute), Nodes: []string{nodeA, nodeB}})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.Node] = true
	}
	if !seen[nodeA] || !seen[nodeB] || len(rows) != 2 {
		t.Errorf("both nodes must be present: %v", ids(rows))
	}
}

// A day round-trips: archived into an in-memory archiver, deleted from the
// real table, restored, and a second restore restores 0.
func TestSweepRoundTripsADayThroughTheArchiveAndBack(t *testing.T) {
	db := testDB(t)
	sfx := uniqueSuffix()
	node := "node-sweep-" + sfx
	cleanupNode(t, db, node)

	// A day of its own, well past a 30-day window, so a peer's rows on the
	// same day are unlikely; the assertions are about THIS node's rows.
	day := utcDay(time.Now().UTC()).AddDate(0, 0, -(60 + int(time.Now().UnixNano()%200)))
	recent := time.Now().UTC().Add(-time.Hour)
	fixture := []Row{
		{OccurredAt: day.Add(1 * time.Hour), ID: "sw-1-" + sfx, NodeType: "bff", Node: node, Level: "info", Component: "svc", Message: "one " + sfx, Attributes: json.RawMessage(`{"k":"v"}`)},
		{OccurredAt: day.Add(2 * time.Hour), ID: "sw-2-" + sfx, NodeType: "bff", Node: node, Level: "warn", Component: "svc", Message: "two " + sfx},
		{OccurredAt: day.Add(3 * time.Hour), ID: "sw-3-" + sfx, NodeType: "agent", Node: node, Level: "error", Component: "svc", Message: "three " + sfx, Subject: "s", SubjectConcept: "v1:x:y"},
		{OccurredAt: recent, ID: "sw-recent-" + sfx, NodeType: "bff", Node: node, Level: "info", Component: "svc", Message: "recent " + sfx},
	}
	if _, err := db.NewInsert().Model(&fixture).Exec(context.Background()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	countMine := func() int {
		var n int
		if err := db.DB.QueryRowContext(context.Background(), `SELECT count(*) FROM log_line WHERE node = $1`, node).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	if countMine() != 4 {
		t.Fatalf("seeded %d rows, want 4", countMine())
	}

	var journal []string
	arch := newMemArchiver(&journal)
	sw := &Sweeper{DB: db, LockDB: db, Archive: arch, Container: "test-archive", RetentionDays: 30, Logger: discard()}
	rep, err := sw.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Refused != "" || rep.Skipped != "" || rep.DaysArchived < 1 {
		t.Fatalf("report: %+v", rep)
	}
	dayName := day.Format("2006-01-02")
	for _, obj := range []string{"logs/" + dayName + "/bff.ndjson.gz", "logs/" + dayName + "/agent.ndjson.gz"} {
		if _, ok := arch.objects[obj]; !ok {
			t.Errorf("archive object %s missing; objects: %v", obj, journal)
		}
	}
	if got := countMine(); got != 1 {
		t.Fatalf("after the sweep %d of my rows remain, want only the recent one", got)
	}

	// Restore the whole day, then again.
	rr, err := sw.Restore(context.Background(), dayName, "")
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if rr.Restored < 3 {
		t.Errorf("first restore restored %d, want at least my 3: %+v", rr.Restored, rr)
	}
	if got := countMine(); got != 4 {
		t.Errorf("after the restore %d of my rows are present, want 4", got)
	}
	rows, err := Search(context.Background(), db, Query{WindowStart: day, WindowEnd: day.AddDate(0, 0, 1), Nodes: []string{node}})
	if err != nil {
		t.Fatalf("Search restored: %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("restored rows readable: %v", ids(rows))
	}
	var attrs map[string]any
	for _, r := range rows {
		if r.ID == "sw-1-"+sfx {
			if err := json.Unmarshal(r.Attributes, &attrs); err != nil || attrs["k"] != "v" {
				t.Errorf("restored attributes: %s %v", r.Attributes, err)
			}
		}
	}
	rr2, err := sw.Restore(context.Background(), dayName, "")
	if err != nil {
		t.Fatalf("second Restore: %v", err)
	}
	if rr2.Restored != 0 || rr2.Skipped < 3 {
		t.Errorf("a second restore must restore 0 and skip the rest: %+v", rr2)
	}

	// The lock: a held lock skips. Take it on a separate connection.
	conn, err := db.DB.Conn(context.Background())
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	var got bool
	if err := conn.QueryRowContext(context.Background(), `SELECT pg_try_advisory_lock($1)`, sweepAdvisoryLockKey).Scan(&got); err != nil || !got {
		t.Fatalf("take lock: %v %v", got, err)
	}
	skipped, err := sw.Run(context.Background())
	_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, sweepAdvisoryLockKey)
	_ = conn.Close()
	if err != nil {
		t.Fatalf("Run under a held lock: %v", err)
	}
	if skipped.Skipped == "" {
		t.Errorf("a held lock must skip: %+v", skipped)
	}
	if countMine() != 4 {
		t.Errorf("a skipped sweep deleted rows")
	}
	fmt.Fprintf(io.Discard, "%v", journal)
}

// ---------------------------------------------------------------------------
// The sweep removes an archived day by dropping its chunk
// ---------------------------------------------------------------------------
//
// 2026-10-10 and 2026-10-11: the sweep archived a day, and its DELETE over the
// day's compressed chunk hit the 8 s statement_timeout. These cases read off a
// real TimescaleDB which chunk a drop takes (2.29.0 when they were written)
// instead of assuming it: the day's own, compressed or not, and nothing else.

// ancientLogDay is a UTC midnight somewhere in 2010 to 2019. A chunk drop takes
// EVERY row of the day's chunk, not only a test's own, so these cases keep to a
// day no log line, restore or peer test lands on; the sweep round trip above
// picks 60 to 260 days back for the same reason.
func ancientLogDay() time.Time {
	return time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, rand.IntN(3650))
}

// needTimescale skips a case that needs the TimescaleDB extension where there
// is none, and fails it under MEMQL_REQUIRE_DB=1: the db lane runs on
// TimescaleDB, and a run that skipped the chunk assertions would be green for
// the wrong reason.
func needTimescale(t *testing.T, db *bun.DB) {
	t.Helper()
	var installed bool
	if err := db.DB.QueryRowContext(context.Background(), `SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb')`).Scan(&installed); err != nil {
		t.Fatalf("probe for TimescaleDB: %v", err)
	}
	if installed {
		return
	}
	if dbtest.RequireDB() {
		t.Fatalf("TimescaleDB is not installed on this database, and %s=1 wants the chunk assertions to run", dbtest.RequireDBEnv)
	}
	t.Skip("TimescaleDB is not installed on this database; the chunk assertions need it")
}

// isHypertable asks the catalog whether log_line, as db resolves the name, is a
// hypertable. It does not use the code under test to answer.
func isHypertable(t *testing.T, db *bun.DB) bool {
	t.Helper()
	var hypertable bool
	if err := db.DB.QueryRowContext(context.Background(), `
		SELECT EXISTS (
		  SELECT 1
		    FROM timescaledb_information.hypertables AS h
		    JOIN pg_class AS c ON c.oid = to_regclass('log_line')
		    JOIN pg_namespace AS n ON n.oid = c.relnamespace
		   WHERE h.hypertable_schema = n.nspname AND h.hypertable_name = c.relname)`).Scan(&hypertable); err != nil {
		t.Fatalf("read the hypertable catalog: %v", err)
	}
	return hypertable
}

// logLineChunk is one row of timescaledb_information.chunks for log_line.
type logLineChunk struct {
	Name       string
	Start, End time.Time
	Compressed bool
}

// logLineChunks lists the chunks of log_line, as db resolves the name, that
// overlap [from, to), oldest first.
func logLineChunks(t *testing.T, db *bun.DB, from, to time.Time) []logLineChunk {
	t.Helper()
	rows, err := db.DB.QueryContext(context.Background(), `
		SELECT ch.chunk_name, ch.range_start, ch.range_end, ch.is_compressed
		  FROM timescaledb_information.chunks AS ch
		  JOIN pg_class AS c ON c.oid = to_regclass('log_line')
		  JOIN pg_namespace AS n ON n.oid = c.relnamespace
		 WHERE ch.hypertable_schema = n.nspname AND ch.hypertable_name = c.relname
		   AND ch.range_end > $1 AND ch.range_start < $2
		 ORDER BY ch.range_start`, from.UTC(), to.UTC())
	if err != nil {
		t.Fatalf("list the chunks of log_line: %v", err)
	}
	defer rows.Close()
	var out []logLineChunk
	for rows.Next() {
		var c logLineChunk
		if err := rows.Scan(&c.Name, &c.Start, &c.End, &c.Compressed); err != nil {
			t.Fatalf("scan a chunk: %v", err)
		}
		c.Start, c.End = c.Start.UTC(), c.End.UTC()
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list the chunks of log_line: %v", err)
	}
	return out
}

// chunkSpans renders chunks as their UTC ranges, for a failure message and for
// comparing a list of chunks to the days it should hold.
func chunkSpans(chunks []logLineChunk) string {
	spans := make([]string, 0, len(chunks))
	for _, c := range chunks {
		spans = append(spans, c.Start.Format("2006-01-02T15:04")+".."+c.End.Format("2006-01-02T15:04"))
	}
	return strings.Join(spans, " ")
}

// compressLogLineChunk compresses the chunk that starts at start, the state
// the compression policy leaves every chunk older than a day in. An
// already-compressed chunk is left as it is.
func compressLogLineChunk(t *testing.T, db *bun.DB, start time.Time) {
	t.Helper()
	var compressed sql.NullString
	if err := db.DB.QueryRowContext(context.Background(), `
		SELECT compress_chunk(format('%I.%I', ch.chunk_schema, ch.chunk_name)::regclass, if_not_compressed => true)::text
		  FROM timescaledb_information.chunks AS ch
		  JOIN pg_class AS c ON c.oid = to_regclass('log_line')
		  JOIN pg_namespace AS n ON n.oid = c.relnamespace
		 WHERE ch.hypertable_schema = n.nspname AND ch.hypertable_name = c.relname
		   AND ch.range_start = $1::timestamptz`, start.UTC()).Scan(&compressed); err != nil {
		t.Fatalf("compress the chunk starting %s: %v", start.Format(time.RFC3339), err)
	}
}

// cleanupLogLineWindow removes what a case left in [from, to): its rows by
// node, and, when nothing else lives in the window, the window's chunks too, so
// the case leaves no empty chunk behind.
func cleanupLogLineWindow(t *testing.T, db *bun.DB, node string, from, to time.Time) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = db.DB.ExecContext(ctx, `DELETE FROM log_line WHERE node = $1`, node)
		var others int
		if err := db.DB.QueryRowContext(ctx, `SELECT count(*) FROM log_line WHERE occurred_at >= $1 AND occurred_at < $2`, from.UTC(), to.UTC()).Scan(&others); err != nil || others > 0 {
			return
		}
		_, _ = db.DB.ExecContext(ctx, `SELECT count(*) FROM drop_chunks('log_line', older_than => $2::timestamptz, newer_than => $1::timestamptz)`, from.UTC(), to.UTC())
	})
}

// remainingIDs lists the ids of a node's rows, oldest first.
func remainingIDs(t *testing.T, db *bun.DB, node string) []string {
	t.Helper()
	rows, err := db.DB.QueryContext(context.Background(), `SELECT id FROM log_line WHERE node = $1 ORDER BY occurred_at, id`, node)
	if err != nil {
		t.Fatalf("list rows of %s: %v", node, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan an id: %v", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list rows of %s: %v", node, err)
	}
	return out
}

// dropDay on the real log_line, uncompressed and compressed (the state the
// production chunk was in). Three consecutive days, each with rows at both
// edges: the day's chunk goes, with the rows at its first and last microsecond,
// and the previous and next days' chunks stay, with theirs.
func TestPgSourceDropDayDropsTheDaysChunk(t *testing.T) {
	db := testDB(t)
	needTimescale(t, db)
	if !isHypertable(t, db) {
		if dbtest.RequireDB() {
			t.Fatalf("log_line is not a hypertable on this database, and %s=1 wants the chunk assertions to run", dbtest.RequireDBEnv)
		}
		t.Skip("log_line is not a hypertable on this database")
	}

	for _, tc := range []struct {
		name     string
		compress bool
	}{
		{"uncompressed chunk", false},
		{"compressed chunk", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			sfx := uniqueSuffix()
			node := "node-drop-" + sfx
			day := ancientLogDay()
			prev, next, past := day.AddDate(0, 0, -1), day.AddDate(0, 0, 1), day.AddDate(0, 0, 2)
			cleanupLogLineWindow(t, db, node, prev, past)

			row := func(at time.Time, label string) Row {
				return Row{OccurredAt: at, ID: label + "-" + sfx, NodeType: "bff", Node: node, Level: "info", Component: "svc", Message: label + " " + sfx}
			}
			fixture := []Row{
				row(prev.Add(time.Hour), "prev-mid"),
				row(day.Add(-time.Microsecond), "prev-last"),
				row(day, "day-first"),
				row(day.Add(12*time.Hour), "day-mid"),
				row(next.Add(-time.Microsecond), "day-last"),
				row(next, "next-first"),
				row(next.Add(5*time.Hour), "next-mid"),
			}
			if _, err := db.NewInsert().Model(&fixture).Exec(ctx); err != nil {
				t.Fatalf("seed: %v", err)
			}
			if tc.compress {
				compressLogLineChunk(t, db, day)
			}

			// The chunks are the ones the migration's one-day interval makes: one
			// per UTC day, from midnight to midnight.
			before := logLineChunks(t, db, prev, past)
			wantSpans := chunkSpans([]logLineChunk{{Start: prev, End: day}, {Start: day, End: next}, {Start: next, End: past}})
			if chunkSpans(before) != wantSpans {
				t.Fatalf("chunks before the drop: %s, want %s", chunkSpans(before), wantSpans)
			}
			if tc.compress && !before[1].Compressed {
				t.Fatalf("the day's chunk is not compressed after compress_chunk: %+v", before[1])
			}
			t.Logf("the day's chunk %s, compressed=%v", before[1].Name, before[1].Compressed)

			src := &pgSource{db: db}
			dropped, err := src.dropDay(ctx, day)
			if err != nil {
				t.Fatalf("dropDay: %v", err)
			}
			if dropped != 1 {
				t.Errorf("dropDay dropped %d chunks, want the day's one", dropped)
			}

			// WHICH chunk went: the day's. The neighbours are the same chunks as
			// before, not recreated ones.
			after := logLineChunks(t, db, prev, past)
			if got, want := chunkSpans(after), chunkSpans([]logLineChunk{{Start: prev, End: day}, {Start: next, End: past}}); got != want {
				t.Fatalf("chunks after the drop: %s, want %s", got, want)
			}
			if after[0].Name != before[0].Name || after[1].Name != before[2].Name {
				t.Errorf("a neighbouring chunk was replaced: before %s, after %s", chunkSpans(before), chunkSpans(after))
			}

			// WHICH rows went: the day's, to the microsecond at both edges.
			wantIDs := []string{"prev-mid-" + sfx, "prev-last-" + sfx, "next-first-" + sfx, "next-mid-" + sfx}
			if got := remainingIDs(t, db, node); strings.Join(got, ",") != strings.Join(wantIDs, ",") {
				t.Errorf("rows after the drop: %v, want %v", got, wantIDs)
			}
			for _, c := range []struct {
				name string
				at   time.Time
				want bool
			}{{"the day", day, false}, {"the day before", prev, true}, {"the day after", next, true}} {
				got, err := src.dayHasRows(ctx, c.at)
				if err != nil {
					t.Fatalf("dayHasRows(%s): %v", c.name, err)
				}
				if got != c.want {
					t.Errorf("dayHasRows(%s) = %v, want %v", c.name, got, c.want)
				}
			}

			// Nothing is left to drop, which is an answer and not a failure; and a
			// day that never had a chunk is the same.
			for _, again := range []time.Time{day, day.AddDate(0, 0, 5)} {
				n, err := src.dropDay(ctx, again)
				if err != nil || n != 0 {
					t.Errorf("dropDay(%s) again = %d, %v; want 0, nil", again.Format("2006-01-02"), n, err)
				}
			}
		})
	}
}

// scratchLogLine builds a log_line of its own, in a schema of its own, and
// returns a handle whose search_path resolves the name to it. The production
// code names the table unqualified, so it runs against the scratch one
// unchanged, and nothing a case does can reach the shared table. interval is
// the chunk interval ("" leaves a plain table); compress gives the table the
// migration's compression settings.
func scratchLogLine(t *testing.T, admin *bun.DB, interval string, compress bool) *bun.DB {
	t.Helper()
	ctx := context.Background()
	schema := "logstore_scratch_" + strings.NewReplacer(".", "", "T", "_").Replace(uniqueSuffix())
	run := func(stmt string) {
		t.Helper()
		if _, err := admin.DB.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	run(`CREATE SCHEMA ` + schema)
	t.Cleanup(func() {
		_, _ = admin.DB.ExecContext(ctx, `DROP TABLE IF EXISTS `+schema+`.log_line`)
		_, _ = admin.DB.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
	})
	run(`CREATE TABLE ` + schema + `.log_line (LIKE log_line INCLUDING DEFAULTS)`)
	if interval != "" {
		run(fmt.Sprintf(`SELECT create_hypertable('%s.log_line', 'occurred_at', chunk_time_interval => INTERVAL '%s')`, schema, interval))
		if compress {
			run(`ALTER TABLE ` + schema + `.log_line SET (timescaledb.compress, timescaledb.compress_segmentby = 'node_type, component', timescaledb.compress_orderby = 'occurred_at DESC')`)
		}
	}
	// public stays on the path: the TimescaleDB functions live there.
	db := bun.NewDB(sql.OpenDB(searchPathConnector{
		Connector: pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())),
		path:      schema + ", public",
	}), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// searchPathConnector runs SET search_path on every connection it opens.
// pgdriver's connection parameters quote a value as one identifier, so a list
// such as "schema, public" cannot be sent that way.
type searchPathConnector struct {
	driver.Connector
	path string
}

func (c searchPathConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	execer, ok := conn.(driver.ExecerContext)
	if !ok {
		_ = conn.Close()
		return nil, errors.New("logstore test: the driver connection cannot exec")
	}
	if _, err := execer.ExecContext(ctx, "SET search_path TO "+c.path, nil); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// The whole sweep over a real day, whatever shape its chunks have. The
// production shape (a one-day chunk, compressed) goes by drop_chunks. A chunk
// wider than the day is NOT the day's: dropping it would take the next day's
// rows, which are not archived, so it is left alone and the day's rows go by the
// batched DELETE, and the next day's rows in the same chunk survive. A plain
// table has no chunk to drop and no hypertable catalog to ask.
func TestSweepEmptiesADayWhateverShapeItsChunksHave(t *testing.T) {
	admin := testDB(t)

	for _, tc := range []struct {
		name       string
		interval   string // "" is a plain table
		compress   bool
		wantChunks int64
	}{
		{"compressed one-day chunk", "1 day", true, 1},
		{"chunk wider than the day", "7 days", false, 0},
		// On the lane's TimescaleDB, public.log_line is a hypertable beside this
		// one, which a check by bare name would mistake it for.
		{"plain table", "", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.interval != "" {
				needTimescale(t, admin)
			}
			db := scratchLogLine(t, admin, tc.interval, tc.compress)
			sfx := uniqueSuffix()
			day := ancientLogDay()
			next := day.AddDate(0, 0, 1)

			row := func(at time.Time, nodeType, label string) Row {
				return Row{OccurredAt: at, ID: label + "-" + sfx, NodeType: nodeType, Node: "node-" + nodeType, Level: "info", Component: "svc", Message: label + " " + sfx}
			}
			fixture := []Row{
				row(day, "bff", "day-first"),
				row(day.Add(6*time.Hour), "agent", "day-mid"),
				row(next.Add(-time.Microsecond), "bff", "day-last"),
				row(next, "bff", "next-first"),
				row(next.Add(2*time.Hour), "agent", "next-mid"),
			}
			if _, err := db.NewInsert().Model(&fixture).Exec(ctx); err != nil {
				t.Fatalf("seed: %v", err)
			}
			if tc.compress {
				compressLogLineChunk(t, db, day)
				if chunks := logLineChunks(t, db, day, next); len(chunks) != 1 || !chunks[0].Compressed {
					t.Fatalf("the day's chunk is not compressed: %s", chunkSpans(chunks))
				}
			}
			if tc.interval == "7 days" {
				// The case is only the case if a chunk really holds more than the day.
				chunks := logLineChunks(t, db, day, next)
				if len(chunks) != 1 || chunks[0].End.Sub(chunks[0].Start) <= 24*time.Hour {
					t.Fatalf("expected one chunk wider than the day, have %s", chunkSpans(chunks))
				}
			}

			var journal []string
			arch := newMemArchiver(&journal)
			// Now puts the retention boundary at midnight after the day, so exactly
			// the day is expired and the next day's rows are not.
			now := next.AddDate(0, 0, 30).Add(3 * time.Hour)
			sw := &Sweeper{DB: db, Archive: arch, Container: "test-archive", RetentionDays: 30, Logger: discard(),
				Now: func() time.Time { return now }, locker: memLock{acquired: true}}
			rep, err := sw.Run(ctx)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if rep.DaysArchived != 1 || rep.RowsArchived != 3 || rep.RowsDeleted != 3 || rep.ChunksDropped != tc.wantChunks {
				t.Errorf("report: %+v, want 1 day, 3 rows archived and deleted, %d chunks dropped", rep, tc.wantChunks)
			}
			dayName := day.Format("2006-01-02")
			for _, obj := range []string{"logs/" + dayName + "/bff.ndjson.gz", "logs/" + dayName + "/agent.ndjson.gz"} {
				if _, ok := arch.objects[obj]; !ok {
					t.Errorf("archive object %s missing; objects: %v", obj, journal)
				}
			}

			// The day is gone and the next day is whole, in every shape.
			rows, err := Search(ctx, db, Query{WindowStart: day.Add(-time.Hour), WindowEnd: next.AddDate(0, 0, 1)})
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			var got []string
			for _, r := range rows {
				got = append(got, r.ID)
			}
			if want := "next-mid-" + sfx + "," + "next-first-" + sfx; strings.Join(got, ",") != want {
				t.Errorf("rows after the sweep: %v, want only the next day's (newest first): %s", got, want)
			}

			switch tc.interval {
			case "1 day":
				if left := logLineChunks(t, db, day, next.AddDate(0, 0, 1)); chunkSpans(left) != chunkSpans([]logLineChunk{{Start: next, End: next.AddDate(0, 0, 1)}}) {
					t.Errorf("chunks after the sweep: %s, want only the next day's", chunkSpans(left))
				}
			case "7 days":
				if left := logLineChunks(t, db, day, next); len(left) != 1 {
					t.Errorf("the wide chunk must stay: %s", chunkSpans(left))
				}
			}
		})
	}
}
