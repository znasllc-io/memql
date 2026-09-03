package logstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
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
	log.Warn("beta "+sfx, "component", comp)
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
