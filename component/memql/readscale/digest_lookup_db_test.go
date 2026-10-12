package readscale_test

import (
	"context"
	"crypto/md5"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

type captureKey struct{}

// scanCapture records the concept scans a read issues -- the statements over
// the executor's latest_keys enumeration: the scan and its latest-version
// recheck -- for reads whose context carries captureKey.
type scanCapture struct {
	mu      sync.Mutex
	queries []string
}

func (h *scanCapture) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}

func (h *scanCapture) AfterQuery(ctx context.Context, event *bun.QueryEvent) {
	if ctx.Value(captureKey{}) != true || event.Err != nil || !strings.HasPrefix(event.Query, "SELECT") || !strings.Contains(event.Query, "latest_keys") {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.queries = append(h.queries, event.Query)
}

func (h *scanCapture) take() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	queries := h.queries
	h.queries = nil
	return queries
}

// explainBuffers runs query under EXPLAIN (ANALYZE, BUFFERS) and returns the
// shared buffers its plan touched, hits and reads alike: a count that does not
// depend on how warm the cache or how fast the runner is.
func explainBuffers(t *testing.T, db *bun.DB, query string) int {
	t.Helper()
	var raw []byte
	require.NoError(t, db.QueryRowContext(context.Background(), `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) `+query).Scan(&raw))
	var plans []map[string]any
	require.NoError(t, json.Unmarshal(raw, &plans))
	plan := plans[0]["Plan"].(map[string]any)
	hit, _ := plan["Shared Hit Blocks"].(float64)
	read, _ := plan["Shared Read Blocks"].(float64)
	return int(hit + read)
}

// A KEYED READ COSTS ITS MATCHES, NOT ITS CONCEPT.
//
// 200,000 recipients in one audience, two versions each, in production's
// chunking (TimescaleDB's default 7 days) across a year. Before a lookup index
// could narrow the read, a digest check collapsed every id of the concept to
// its latest version before comparing the digest: 804,053 buffers and about a
// second with the rows in five chunks, and 7.1 s across this fixture's 53.
// Measured in buffers, which do not depend on the runner: the scan and its
// latest-version recheck together must stay more than two orders of magnitude
// under that, for a digest that is there -- on a recipient whose versions sit
// in an early chunk, so the latest-version probe walks back across most of the
// year -- and for the one a new subscriber brings.
func TestDigestLookupReadsOnlyTheMatchingRows(t *testing.T) {
	const recipients = 200000
	const bound = 2000
	ctx := context.Background()
	admin := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "digest lookup at scale", dbtest.DSN(), err)
	}
	var timescale bool
	require.NoError(t, admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_extension WHERE extname='timescaledb')`).Scan(&timescale))
	if !timescale {
		t.Skip("requires TimescaleDB chunks, as production runs")
	}
	schema := fmt.Sprintf("readscale_%d", time.Now().UnixNano())
	_, err := admin.ExecContext(ctx, `CREATE SCHEMA `+schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = admin.ExecContext(ctx, `DROP SCHEMA `+schema+` CASCADE`) })
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()), pgdriver.WithConnParams(map[string]any{"search_path": schema}))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	started := time.Now()
	for _, stmt := range []string{
		// INCLUDING ALL copies every index the migrations built on public,
		// the digest lookup indexes among them.
		`CREATE TABLE "MemoryNodes" (LIKE public."MemoryNodes" INCLUDING ALL)`,
		`SELECT public.create_hypertable('"MemoryNodes"','createdAt',chunk_time_interval=>interval '7 days')`,
		fmt.Sprintf(`INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",schema,payload)
         SELECT 'v1:campaigns:recipient:r-'||n,'v1:campaigns:recipient',
           TIMESTAMPTZ '2025-01-01'+(n %% 364)*interval '1 day'+n*interval '1 millisecond'+v*interval '1 hour','fixture','{}',
           jsonb_build_object('ownerUserId','v1:identity:user:readscale-owner','accountId','v1:accounts:account:readscale-account',
             'audienceId','v1:campaigns:audience:readscale-audience','email','user-'||n||'@example.test',
             'emailDigest',md5('user-'||n||'@example.test'),'displayName','Recipient '||n,
             'subscriptionStatus',CASE WHEN v=0 THEN 'subscribed' ELSE 'unsubscribed' END,'source','signup')
         FROM generate_series(1,%d) n CROSS JOIN generate_series(0,1) v`, recipients),
		`VACUUM ANALYZE "MemoryNodes"`,
	} {
		_, err := db.ExecContext(ctx, stmt)
		require.NoError(t, err)
	}
	var lookupIndexes int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM pg_indexes WHERE schemaname=? AND tablename='MemoryNodes'
	   AND indexdef LIKE '%(payload ->> ''emailDigest''::text)%' AND indexdef LIKE '%v1:campaigns:recipient%'`, schema).Scan(&lookupIndexes))
	require.Equal(t, 1, lookupIndexes, "public.\"MemoryNodes\" must carry the recipient digest lookup index the migrations build")
	t.Logf("seeded %d recipients x 2 versions in %s", recipients, time.Since(started).Round(time.Millisecond))

	_, err = memql.LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	eng, err := memql.New(db)
	require.NoError(t, err)
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	require.NoError(t, eng.Init(memorynodes.DefaultRegistry()))
	hook := &scanCapture{}
	db.AddQueryHook(hook)

	reader := auth.ContextWithToken(auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: "readscale-owner",
		Role:   auth.RoleOwner,
	}), &auth.TokenInfo{Subject: "readscale-owner"})
	reader = context.WithValue(memql.ContextWithFreshRead(reader), captureKey{}, true)
	for _, tc := range []struct {
		name, email string
		want        []string
	}{
		{"present", "user-123457@example.test", []string{"v1:campaigns:recipient:r-123457"}},
		{"new subscriber", "nobody@example.test", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query := fmt.Sprintf(`concept==v1:campaigns:recipient && payload.audienceId=="v1:campaigns:audience:readscale-audience" && payload.emailDigest==%q`,
				fmt.Sprintf("%x", md5.Sum([]byte(tc.email))))
			readStarted := time.Now()
			result, err := eng.Execute(reader, query)
			elapsed := time.Since(readStarted)
			require.NoError(t, err)
			ids := []string{}
			for _, n := range result.Bundle.GetNodes() {
				ids = append(ids, n.GetId())
			}
			require.Equal(t, tc.want, ids)
			scans := hook.take()
			require.NotEmpty(t, scans, "the read must reach the concept scan")
			total := 0
			for _, scan := range scans {
				total += explainBuffers(t, db, scan)
			}
			t.Logf("%d buffers over %d scans, read in %s", total, len(scans), elapsed.Round(time.Millisecond))
			require.LessOrEqual(t, total, bound,
				"a digest lookup over %d recipients touched %d buffers: the read enumerated the concept instead of the digest's ids\n%s",
				recipients, total, strings.Join(scans, "\n"))
			require.Contains(t, scans[0], "(payload->>'emailDigest')", "the read must enumerate its ids through the digest lookup")
		})
	}
}
