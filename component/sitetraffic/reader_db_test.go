package sitetraffic

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
)

// reader_db_test.go -- the acceptance criteria of epic memql#4908, against a
// real database.
//
// Nothing here is provable in process. The fold is TimescaleDB's, the
// half-open window is SQL's, and the authorization is a DSL query's --
// so a suite that stubbed any of the three would be asserting its own
// fixtures. What it proves:
//
//   - a served request lands in the log, the aggregate reflects it, and the
//     read returns it for the OWNER and for a CLUSTER OWNER;
//   - a third user gets zero rows for the same deployable, and gets them as
//     an EMPTY ANSWER rather than a refusal, so the call is not an existence
//     oracle;
//   - a window nothing measured answers with no row at all -- never a
//     zero-filled one -- and a window with requests and no errors answers a
//     row whose errorCount is 0;
//   - the summary folds the whole window into one row per deployable, and it
//     agrees with the buckets it summarizes.
//
// Postgres-gated. CI's db-tests lane runs this package with MEMQL_REQUIRE_DB=1,
// so a skip there is a failure rather than a green.

const trafficTestDomain = "traffic-test.example"

// A window well in the past, so the fixtures never collide with real rows and
// the aggregate's refresh policies are irrelevant: a manual refresh covers a
// closed window exactly.
var (
	trafficWindowStart = time.Date(2031, 3, 1, 12, 0, 0, 0, time.UTC)
	trafficWindowEnd   = trafficWindowStart.Add(time.Hour)
)

// trafficEngine builds an engine over the real database, or skips.
//
// ONE BOOT PER PROCESS, the sharedReadMergeEngine seam's rule (memql#4075):
// an engine boot loads the whole DSL tree, and a boot per test is what pushes
// a db-gated package toward its lane budget. The Once records its own failure
// so every caller reports it rather than the first one swallowing it.
var trafficEngineState struct {
	once    sync.Once
	eng     *memql.MemQLEngine
	db      *bun.DB
	dsn     string
	pingErr error
	bootErr error
}

func trafficEngine(t *testing.T) (*memql.MemQLEngine, *bun.DB, context.Context) {
	t.Helper()
	s := &trafficEngineState
	s.once.Do(func() {
		if _, err := dbtest.EnsureSchema(context.Background()); err != nil {
			s.pingErr = err
			return
		}
		s.dsn = dbtest.DSN()
		db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(s.dsn))), pgdialect.New())
		if err := db.PingContext(context.Background()); err != nil {
			s.pingErr = err
			_ = db.Close()
			return
		}
		if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
			s.bootErr = fmt.Errorf("LoadUnifiedConcepts: %w", err)
			_ = db.Close()
			return
		}
		eng, err := memql.New(db)
		if err != nil {
			s.bootErr = fmt.Errorf("New: %w", err)
			_ = db.Close()
			return
		}
		eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
		if err := eng.Init(memorynodes.DefaultRegistry()); err != nil {
			s.bootErr = fmt.Errorf("Init: %w", err)
			_ = db.Close()
			return
		}
		s.eng, s.db = eng, db
	})
	if s.pingErr != nil {
		dbtest.Unreachable(t, "the edge request log's aggregate", dbtest.DSN(), s.pingErr)
		return nil, nil, nil
	}
	if s.bootErr != nil {
		t.Fatalf("the traffic engine failed to boot: %v", s.bootErr)
	}
	// The hostname policy reads the domain from the environment on every
	// write, so it is set per test rather than once at boot.
	t.Setenv("MEMQL_DOMAIN", trafficTestDomain)
	return s.eng, s.db, context.Background()
}

// userCtx is an ordinary authenticated caller.
func userCtx(userId string) context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: userId, Role: auth.RoleWriter})
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: userId})
}

// operatorCtx is a cluster owner.
func operatorCtx(userId string) context.Context {
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: userId, Role: auth.RoleOwner})
	return auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: userId})
}

// suffix keys fixtures on the test's own name: the database is shared with
// other sessions and with the rest of this lane, so isolation comes from the
// ids rather than from having the table to ourselves.
func suffix(t *testing.T) string {
	t.Helper()
	clean := strings.ToLower(strings.NewReplacer("/", "-", "_", "-").Replace(t.Name()))
	return fmt.Sprintf("%s-%d", clean, os.Getpid())
}

// seedDeployable creates a site owned by owner and returns its bare id.
func seedDeployable(t *testing.T, eng *memql.MemQLEngine, owner, slug string) string {
	t.Helper()
	id := "site-traffic-" + slug
	q := fmt.Sprintf(
		`mutation createSite(siteId: %s, hostname: %s, bundleRef: %s, status: "live")`,
		langparser.QuoteString(id),
		langparser.QuoteString(shortHost(slug)+"."+trafficTestDomain),
		langparser.QuoteString("blob://sites/"+id+"/v1/"),
	)
	if _, err := eng.Execute(userCtx(owner), q); err != nil {
		t.Fatalf("seed the deployable: %v", err)
	}
	return id
}

// shortHost derives a hostname inside the slug rule ([a-z0-9-]{3,40}) from
// the whole slug.
//
// A DIGEST RATHER THAN A TRUNCATION, and the truncation is why: taking the
// last forty characters of a test name plus a pid made "mine-<name>-<pid>"
// and "theirs-<name>-<pid>" produce the SAME hostname, and the second
// createSite was refused for colliding with the first. The digest is over the
// whole slug, so two slugs that differ anywhere differ here.
func shortHost(slug string) string {
	sum := sha256.Sum256([]byte(slug))
	return "t" + hex.EncodeToString(sum[:8])
}

// writeRequests puts n rows into the log through the real sink, at instants
// inside the window, and refreshes the aggregates so the fold is visible.
func writeRequests(t *testing.T, db *bun.DB, siteId string, statuses []int) {
	t.Helper()
	s := NewSink(db, SinkOptions{Node: "edge-test", FlushInterval: 10 * time.Millisecond})
	s.Start()
	for i, status := range statuses {
		s.Record(Record{
			SiteId:     siteId,
			ServedAt:   trafficWindowStart.Add(time.Duration(i) * time.Minute),
			Status:     status,
			PathClass:  PathClassDocument,
			Bytes:      int64(100 * (i + 1)),
			DurationNs: 1_000_000,
		})
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatalf("flush the sink: %v", err)
	}

	if written, dropped := s.Stats(); written != uint64(len(statuses)) || dropped != 0 {
		t.Fatalf("the sink wrote %d and dropped %d, want %d written -- the fixture must reach the table before the fold is asked about", written, dropped, len(statuses))
	}
	refreshAggregates(t, db)
}

// refreshAggregates materializes the closed window the fixtures sit in.
//
// A continuous aggregate's refresh POLICY runs on the database's own
// schedule, which a test must not wait on; `refresh_continuous_aggregate`
// over an explicit range is the supported way to make a closed window
// current. On a plain Postgres box the relations are ordinary views and there
// is nothing to refresh -- which is also why the reader has one query rather
// than a branch.
func refreshAggregates(t *testing.T, db *bun.DB) {
	t.Helper()
	ctx := context.Background()
	var installed bool
	if err := db.NewRaw("SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb')").Scan(ctx, &installed); err != nil {
		t.Fatalf("check for timescaledb: %v", err)
	}
	if !installed {
		return
	}
	from := trafficWindowStart.Add(-2 * time.Hour)
	to := trafficWindowEnd.Add(2 * time.Hour)
	for _, view := range []string{"edge_request_1m", "edge_request_1h"} {
		if _, err := db.NewRaw("CALL refresh_continuous_aggregate(?, ?, ?)", bun.Safe("'"+view+"'"), from, to).Exec(ctx); err != nil {
			t.Fatalf("refresh %s: %v", view, err)
		}
	}
}

// The headline criterion: a request lands, the aggregate reflects it, and the
// owner and a cluster owner both read it back.
func TestTrafficIsReadableByTheOwnerAndByAClusterOwner(t *testing.T) {
	eng, db, _ := trafficEngine(t)
	sfx := suffix(t)
	owner := "user-traffic-" + sfx
	siteId := seedDeployable(t, eng, owner, sfx)

	writeRequests(t, db, siteId, []int{200, 200, 500, 404})

	q := Query{SiteIds: []string{siteId}, Bucket: Bucket1h, WindowStart: trafficWindowStart, WindowEnd: trafficWindowEnd}
	reader := NewReader(db, eng)

	for name, ctx := range map[string]context.Context{
		"the owner":       userCtx(owner),
		"a cluster owner": operatorCtx("user-operator-" + sfx),
	} {
		got, err := reader.Read(ctx, q)
		if err != nil {
			t.Fatalf("%s: read: %v", name, err)
		}
		if len(got) != 1 {
			t.Fatalf("%s: got %d rows, want 1 hour bucket", name, len(got))
		}
		r := got[0]
		if r.RequestCount != 4 {
			t.Errorf("%s: requestCount = %d, want 4", name, r.RequestCount)
		}
		if r.ErrorCount != 1 {
			t.Errorf("%s: errorCount = %d, want the one 5xx", name, r.ErrorCount)
		}
		if r.ClientErrorCount != 1 {
			t.Errorf("%s: clientErrorCount = %d, want the one 4xx", name, r.ClientErrorCount)
		}
		if r.BytesTotal != 100+200+300+400 {
			t.Errorf("%s: bytesTotal = %d, want the sum of the rows", name, r.BytesTotal)
		}
		if r.LastServedAt.IsZero() {
			t.Errorf("%s: lastServedAt is unset", name)
		}
	}
}

// A THIRD USER GETS ZERO ROWS, and gets them as an empty answer rather than a
// refusal: a refusal naming the id would answer "does this deployable exist"
// for anybody who can spell one.
func TestTrafficIsInvisibleToAThirdUser(t *testing.T) {
	eng, db, _ := trafficEngine(t)
	sfx := suffix(t)
	owner := "user-traffic-" + sfx
	siteId := seedDeployable(t, eng, owner, sfx)
	writeRequests(t, db, siteId, []int{200, 200})

	got, err := NewReader(db, eng).Read(userCtx("user-stranger-"+sfx), Query{
		SiteIds: []string{siteId}, Bucket: Bucket1h,
		WindowStart: trafficWindowStart, WindowEnd: trafficWindowEnd,
	})
	if err != nil {
		t.Fatalf("a stranger's read must answer, not fail: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a stranger read %d rows of somebody else's traffic", len(got))
	}
}

// A WINDOW NOTHING MEASURED ANSWERS WITH NO ROW, and a window with requests
// and no errors answers a row whose errorCount is 0. The two must not look
// alike: an absent figure and a zero are different answers.
func TestUnmeasuredIsAnAbsentRowAndZeroIsAZero(t *testing.T) {
	eng, db, _ := trafficEngine(t)
	sfx := suffix(t)
	owner := "user-traffic-" + sfx
	siteId := seedDeployable(t, eng, owner, sfx)
	writeRequests(t, db, siteId, []int{200, 200, 200})
	reader := NewReader(db, eng)

	// A window the deployable served in: one row, zero errors.
	got, err := reader.Read(userCtx(owner), Query{
		SiteIds: []string{siteId}, Bucket: Bucket1h,
		WindowStart: trafficWindowStart, WindowEnd: trafficWindowEnd,
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 1 || got[0].RequestCount != 3 {
		t.Fatalf("got %+v, want one row of three requests", got)
	}
	if got[0].ErrorCount != 0 || got[0].ClientErrorCount != 0 {
		t.Errorf("errorCount = %d / %d, want zero errors stated as zero", got[0].ErrorCount, got[0].ClientErrorCount)
	}

	// A window nothing measured: NO ROW.
	quiet := trafficWindowStart.Add(-48 * time.Hour)
	got, err = reader.Read(userCtx(owner), Query{
		SiteIds: []string{siteId}, Bucket: Bucket1h,
		WindowStart: quiet, WindowEnd: quiet.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("read the quiet window: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a window nothing measured returned %+v, want no row at all -- a zero-filled row would be a measurement nobody made", got)
	}
}

// The summary folds the window into one row per deployable, and it agrees
// with the buckets it summarizes. If the two could disagree, a list row and
// the stop beside it would show different numbers for the same window.
func TestTheSummaryAgreesWithTheBucketsItFolds(t *testing.T) {
	eng, db, _ := trafficEngine(t)
	sfx := suffix(t)
	owner := "user-traffic-" + sfx
	siteId := seedDeployable(t, eng, owner, sfx)
	writeRequests(t, db, siteId, []int{200, 500, 200, 404, 200})
	reader := NewReader(db, eng)
	ctx := userCtx(owner)

	buckets, err := reader.Read(ctx, Query{
		SiteIds: []string{siteId}, Bucket: Bucket1m,
		WindowStart: trafficWindowStart, WindowEnd: trafficWindowEnd,
	})
	if err != nil {
		t.Fatalf("read the buckets: %v", err)
	}
	if len(buckets) != 5 {
		t.Fatalf("got %d minute buckets, want 5 -- one per request, a minute apart", len(buckets))
	}

	summary, err := reader.Read(ctx, Query{
		SiteIds: []string{siteId}, Bucket: Bucket1m, Summary: true,
		WindowStart: trafficWindowStart, WindowEnd: trafficWindowEnd,
	})
	if err != nil {
		t.Fatalf("read the summary: %v", err)
	}
	if len(summary) != 1 {
		t.Fatalf("got %d summary rows, want 1", len(summary))
	}

	var requests, errs, clientErrs, bytes int64
	var newest time.Time
	for _, b := range buckets {
		requests += b.RequestCount
		errs += b.ErrorCount
		clientErrs += b.ClientErrorCount
		bytes += b.BytesTotal
		if b.LastServedAt.After(newest) {
			newest = b.LastServedAt
		}
	}
	s := summary[0]
	if s.RequestCount != requests || s.ErrorCount != errs || s.ClientErrorCount != clientErrs || s.BytesTotal != bytes {
		t.Errorf("summary = %+v, want the buckets' totals (%d, %d, %d, %d)", s, requests, errs, clientErrs, bytes)
	}
	if !s.LastServedAt.Equal(newest) {
		t.Errorf("summary lastServedAt = %v, want the newest bucket's %v", s.LastServedAt, newest)
	}
	// The summary's span is the CALLER'S window, which is how a reader tells
	// a summary from a bucket.
	if !s.WindowStart.Equal(trafficWindowStart) || !s.WindowEnd.Equal(trafficWindowEnd) {
		t.Errorf("summary window = [%v, %v), want the caller's [%v, %v)", s.WindowStart, s.WindowEnd, trafficWindowStart, trafficWindowEnd)
	}
}

// The window is HALF-OPEN, so two adjacent windows add up rather than
// double-counting the instant they share.
func TestTheWindowIsHalfOpen(t *testing.T) {
	eng, db, _ := trafficEngine(t)
	sfx := suffix(t)
	owner := "user-traffic-" + sfx
	siteId := seedDeployable(t, eng, owner, sfx)
	writeRequests(t, db, siteId, []int{200, 200, 200})
	reader := NewReader(db, eng)

	// A window ending exactly at the second request's bucket must exclude it.
	got, err := reader.Read(userCtx(owner), Query{
		SiteIds: []string{siteId}, Bucket: Bucket1m,
		WindowStart: trafficWindowStart, WindowEnd: trafficWindowStart.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 1 || got[0].RequestCount != 1 {
		t.Errorf("got %+v, want exactly the first minute's one request", got)
	}
}

// An unreadable id mixed in with a readable one narrows the answer rather
// than failing it: a list showing a colleague's deployable beside your own
// still renders your own figure.
func TestAnUnreadableIdNarrowsRatherThanFails(t *testing.T) {
	eng, db, _ := trafficEngine(t)
	sfx := suffix(t)
	mine := "user-traffic-" + sfx
	theirs := "user-other-" + sfx
	mySite := seedDeployable(t, eng, mine, "mine-"+sfx)
	theirSite := seedDeployable(t, eng, theirs, "theirs-"+sfx)
	writeRequests(t, db, mySite, []int{200})
	writeRequests(t, db, theirSite, []int{200, 200})

	got, err := NewReader(db, eng).Read(userCtx(mine), Query{
		SiteIds: []string{mySite, theirSite}, Bucket: Bucket1h, Summary: true,
		WindowStart: trafficWindowStart, WindowEnd: trafficWindowEnd,
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want only my own deployable's", len(got))
	}
	if got[0].SiteId != memql.BareShortId(mySite) {
		t.Errorf("row is for %q, want %q", got[0].SiteId, mySite)
	}
	if got[0].RequestCount != 1 {
		t.Errorf("requestCount = %d, want my own deployable's 1 -- not their 2", got[0].RequestCount)
	}
}

// ---------------------------------------------------------------------------
// Retention
// ---------------------------------------------------------------------------

// ApplyRetention runs at EDGE BOOT and only warns when it fails, which is the
// right posture -- an edge that refused to serve sites because a metric's
// retention window did not take would be trading the product for a number --
// and it is exactly the shape that goes wrong silently. So the behaviour is
// pinned here rather than trusted.
//
// What it proves beyond "the call returns nil":
//
//   - all THREE relations move, the raw rows and both aggregates, which is
//     what makes "unmeasured" mean the same thing at every horizon;
//   - a SECOND call with a different window actually changes them. That is
//     the whole reason the function removes before it adds:
//     `add_retention_policy` will not alter an existing policy's interval, so
//     an add alone leaves the migration's thirty days in force and reports
//     success -- a configured window that silently did not take.
func TestApplyRetentionMovesAllThreeRelationsAndCanChangeThem(t *testing.T) {
	_, db, ctx := trafficEngine(t)

	var installed bool
	if err := db.NewRaw("SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb')").Scan(ctx, &installed); err != nil {
		t.Fatalf("check for timescaledb: %v", err)
	}
	if !installed {
		t.Skip("no timescaledb: there are no retention policies to move, which is the plain-Postgres case ApplyRetention no-ops for")
	}

	// The two aggregates are backed by internal hypertables, so the policies
	// are counted by their WINDOW rather than looked up by view name -- which
	// is also the only property worth asserting.
	countAt := func(days int) int {
		var n int
		q := `SELECT count(*) FROM timescaledb_information.jobs
		      WHERE proc_name = 'policy_retention'
		        AND config->>'drop_after' = ?
		        AND (hypertable_name = 'edge_request' OR hypertable_name LIKE '\_materialized\_hypertable\_%')`
		if err := db.NewRaw(q, fmt.Sprintf("%d days", days)).Scan(ctx, &n); err != nil {
			t.Fatalf("count retention jobs at %d days: %v", days, err)
		}
		return n
	}

	// Two windows nothing else in the tree uses, so a policy found at one of
	// them is this call's and not a neighbour's.
	const first, second = 23, 29

	if err := ApplyRetention(ctx, db, first); err != nil {
		t.Fatalf("ApplyRetention(%d): %v", first, err)
	}
	if got := countAt(first); got < 3 {
		t.Fatalf("%d retention policies at %d days, want at least the raw table and both aggregates", got, first)
	}

	if err := ApplyRetention(ctx, db, second); err != nil {
		t.Fatalf("ApplyRetention(%d): %v", second, err)
	}
	if got := countAt(second); got < 3 {
		t.Fatalf("%d retention policies at %d days after a second call, want at least three -- add_retention_policy alone cannot change an interval, so this is the remove-then-add doing its job", got, second)
	}
	if got := countAt(first); got != 0 {
		t.Errorf("%d retention policies still at the OLD %d days; the change did not take and the call reported success", got, first)
	}

	// Leave the shared database on the default, so a later run in this lane
	// reads the window the migration ships rather than this test's.
	if err := ApplyRetention(ctx, db, DefaultRetentionDays); err != nil {
		t.Fatalf("restore the default window: %v", err)
	}
}

// A cluster without TimescaleDB has no policies to move, and that is a no-op
// rather than an error: the views the migration created there are computed
// live from rows nothing drops. Asserted without a database, so it runs
// everywhere.
func TestApplyRetentionRefusesOnlyANilHandle(t *testing.T) {
	if err := ApplyRetention(context.Background(), nil, 30); err == nil {
		t.Error("a nil handle must be an error -- silently doing nothing would leave an operator's configured window unapplied with nothing said")
	}
}
