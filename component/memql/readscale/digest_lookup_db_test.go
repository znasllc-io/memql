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
	// pgdriver cuts every read off at 10 s by default, and the seed below runs
	// past that on a CI runner (measured: the INSERT alone takes ~6 s locally,
	// and CI ran out of time inside it). The bound is for seeding, not for the
	// reads under test, which the buffer bound measures.
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()),
		pgdriver.WithConnParams(map[string]any{"search_path": schema}), pgdriver.WithReadTimeout(5*time.Minute))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	started := time.Now()
	for _, stmt := range []string{
		// INCLUDING ALL copies every index the migrations built on public,
		// the digest lookup indexes among them.
		`CREATE TABLE "MemoryNodes" (LIKE public."MemoryNodes" INCLUDING ALL)`,
		`CREATE TABLE memory_liveness (LIKE public.memory_liveness INCLUDING ALL)`,
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

// THE REVOCATION CHECK COSTS ITS SESSION, NOT EVERY SESSION.
//
// authSessionByTokenHash runs for every verified HTTP request and stream open.
// With no lookup index it collapsed every session to its latest version before
// comparing the digest: 474,699 buffers and 1.1 s at 120,000 sessions, hit or
// miss, measured through a real bff. This fixture is 20,000 sessions of two
// versions, in production's chunking across a year. Measured here: 98,404 buffers
// for the concept-wide read, 2,130 for a lookup with no index behind it (a scan of
// the concept's versions), and at most 317 through the index. So the bound below
// fails the first two, and the fixture stays quick to seed.
func TestAuthSessionLookupReadsOnlyTheMatchingRows(t *testing.T) {
	const sessions = 20000
	const bound = 1000
	ctx := context.Background()
	admin := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.PingContext(ctx); err != nil {
		dbtest.Unreachable(t, "auth session lookup at scale", dbtest.DSN(), err)
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
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()),
		pgdriver.WithConnParams(map[string]any{"search_path": schema}), pgdriver.WithReadTimeout(5*time.Minute))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	// Every session has a created version and a later one; session 123 is revoked
	// by that later version, as revokeAuthSession writes it.
	digest := func(n int) string { return fmt.Sprintf("%x", md5.Sum([]byte(fmt.Sprintf("token-%d", n)))) }
	for _, stmt := range []string{
		`CREATE TABLE "MemoryNodes" (LIKE public."MemoryNodes" INCLUDING ALL)`,
		`CREATE TABLE memory_liveness (LIKE public.memory_liveness INCLUDING ALL)`,
		`SELECT public.create_hypertable('"MemoryNodes"','createdAt',chunk_time_interval=>interval '7 days')`,
		fmt.Sprintf(`INSERT INTO "MemoryNodes" (id,concept,"createdAt","createdBy",schema,payload)
         SELECT 'v1:identity:authSession:s-'||n,'v1:identity:authSession',
           TIMESTAMPTZ '2025-01-01'+(n %% 364)*interval '1 day'+n*interval '1 millisecond'+v*interval '1 hour','fixture','{}',
           jsonb_build_object('subject','v1:identity:user:u-'||(n %% 500),'tokenHash',md5('token-'||n),'source','bff_exchange',
             'expiresAt','2030-01-01T00:00:00Z','refreshTokenHash',md5('refresh-'||n||'-'||v),
             'revokedReason',CASE WHEN n=123 AND v=1 THEN 'user_action' ELSE '' END,
             'revokedAt',CASE WHEN n=123 AND v=1 THEN '2025-06-01T00:00:00Z' END)
         FROM generate_series(1,%d) n CROSS JOIN generate_series(0,1) v`, sessions),
		`VACUUM ANALYZE "MemoryNodes"`,
	} {
		_, err := db.ExecContext(ctx, stmt)
		require.NoError(t, err)
	}

	_, err = memql.LoadUnifiedConcepts(nil)
	require.NoError(t, err)
	eng, err := memql.New(db)
	require.NoError(t, err)
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	require.NoError(t, eng.Init(memorynodes.DefaultRegistry()))
	hook := &scanCapture{}
	db.AddQueryHook(hook)

	reader := context.WithValue(context.Background(), captureKey{}, true)
	for _, tc := range []struct {
		name, hash, wantID, wantRevokedAt string
	}{
		{"live session", digest(9876), "v1:identity:authSession:s-9876", ""},
		{"revoked session, at its latest version", digest(123), "v1:identity:authSession:s-123", "2025-06-01T00:00:00Z"},
		{"a digest no session carries", digest(sessions + 1), "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := eng.Execute(reader, fmt.Sprintf(`query authSessionByTokenHash(tokenHash: "%s")`, tc.hash))
			require.NoError(t, err)
			nodes := result.Bundle.GetNodes()
			if tc.wantID == "" {
				require.Empty(t, nodes)
			} else {
				require.Len(t, nodes, 1)
				require.Equal(t, tc.wantID, nodes[0].GetId())
				require.Equal(t, tc.wantRevokedAt, nodes[0].GetPayload().GetFields()["revokedAt"].GetStringValue())
			}
			scans := hook.take()
			require.NotEmpty(t, scans, "the read must reach the concept scan")
			total := 0
			for _, scan := range scans {
				total += explainBuffers(t, db, scan)
			}
			t.Logf("%d buffers over %d scans", total, len(scans))
			require.LessOrEqual(t, total, bound,
				"a token hash lookup over %d sessions touched %d buffers: the read enumerated the concept instead of the digest's ids\n%s",
				sessions, total, strings.Join(scans, "\n"))
			require.Contains(t, scans[0], "(payload->>'tokenHash')", "the read must enumerate its ids through the token hash lookup")
		})
	}
}
