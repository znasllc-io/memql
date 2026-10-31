package gamespack

import (
	"context"
	"database/sql"
	"errors"
	"hash/fnv"
	"io"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// harness_db_test.go -- a real engine, a real database, the pack's real DSL
// and Go half, and ONE stand-in: the embedder. A database test cannot reach a
// model, so testSpace embeds by hashing words into a small vector and stores
// it in a vector table of its own. Everything else -- the mutations, the
// owner tier, the write gates, the hand-written statements -- is the thing
// that ships.

// testDims is the test embedder's width; testProvider names its space.
const (
	testDims     = 16
	testProvider = "games-test-bag-of-words"
)

type liveGames struct {
	eng      *memql.MemQLEngine
	provider *Provider
	db       *bun.DB
	table    string
}

// runID makes every identifier unique per run: db-gated tests do not clean
// up, and a count asserted against a fixed id fails on the second run.
func runID() string { return strconv.FormatInt(time.Now().UnixNano(), 36) }

// asOwner is a person's actor, exactly as auth.ContextWithUserActor builds it
// for a signed-in caller.
func asOwner(userID string) context.Context {
	return auth.ContextWithUserActor(context.Background(), userID)
}

func newLiveGames(t *testing.T, disabled ...string) *liveGames {
	t.Helper()
	dsn := dbtest.DSN()
	probe := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn))), pgdialect.New())
	if err := probe.PingContext(context.Background()); err != nil {
		_ = probe.Close()
		dbtest.Unreachable(t, "games pack live e2e", dsn, err)
	}
	_ = probe.Close()

	_ = os.Setenv("MEMQL_DATABASE_DSN", dsn)
	mnd, err := memoryNodes.NewMemoryNodesDatabase()
	if err != nil {
		t.Fatalf("NewMemoryNodesDatabase: %v", err)
	}
	mnd.Start(context.Background())
	select {
	case <-mnd.Ready():
	case <-time.After(60 * time.Second):
		t.Fatal("database did not become ready within 60s")
	}
	bunDB := mnd.BunDB()
	if bunDB == nil {
		t.Fatal("nil bun.DB after migrate")
	}
	// The database component goes with the test. Its pool would otherwise stay open
	// until the process ends, about a connection for every two tests, and a run of
	// the whole suite a few times over (-count) would meet the server's limit.
	t.Cleanup(func() { mnd.Stop(context.Background()) })

	registerOnce.Do(func() { Register(Domain) })
	memqldsl.SetDisabledPackDomains(disabled)
	t.Cleanup(func() { memqldsl.SetDisabledPackDomains(nil) })

	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	eng, err := memql.New(bunDB)
	if err != nil {
		t.Fatalf("engine New: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(memoryNodes.DefaultRegistry()); err != nil {
		t.Fatalf("engine.Init: %v", err)
	}
	lg := &liveGames{eng: eng, db: bunDB}
	if slices.Contains(disabled, Domain) {
		return lg
	}

	gates, err := database.NewGatePool(dsn, 0)
	if err != nil {
		t.Fatalf("NewGatePool: %v", err)
	}
	t.Cleanup(func() { _ = gates.Close() })
	table, err := memql.EnsureEmbeddingVectorTable(context.Background(), bunDB.DB, testProvider, testDims)
	if err != nil {
		t.Fatalf("EnsureEmbeddingVectorTable: %v", err)
	}
	lg.table = table
	lg.provider = &Provider{
		store:      engineStore{engine: eng},
		space:      testSpace{db: bunDB.DB, tableName: table},
		db:         func() *sql.DB { return bunDB.DB },
		gates:      func() *sql.DB { return gates },
		invalidate: eng.InvalidateCacheForConcept,
		log:        eng.Logger,
		now:        time.Now,
	}
	if err := eng.RegisterIntegration(lg.provider); err != nil {
		t.Fatalf("RegisterIntegration: %v", err)
	}
	return lg
}

// call runs one construct of the given kind with rendered arguments.
func (lg *liveGames) call(t *testing.T, ctx context.Context, kind, name string, args map[string]any) ([]map[string]any, error) {
	t.Helper()
	rendered, err := langparser.RenderCall(name, compact(args))
	if err != nil {
		t.Fatalf("RenderCall %s: %v", name, err)
	}
	res, err := lg.eng.Execute(ctx, kind+" "+rendered)
	if err != nil {
		return nil, err
	}
	return memql.MaterializeRows(res), nil
}

// must is call that fails the test on an error.
func (lg *liveGames) must(t *testing.T, ctx context.Context, kind, name string, args map[string]any) []map[string]any {
	t.Helper()
	rows, err := lg.call(t, ctx, kind, name, args)
	if err != nil {
		t.Fatalf("%s %s: %v", kind, name, err)
	}
	return rows
}

// testSpace is the stand-in embedder: each lower-cased word hashes into one of
// testDims buckets, so texts sharing words are near and texts sharing none are
// orthogonal.
type testSpace struct {
	db        *sql.DB
	tableName string
}

func (s testSpace) table(context.Context) (string, error) { return s.tableName, nil }

func (s testSpace) embedQuery(_ context.Context, text string) ([]float32, error) {
	return bagOfWords(text), nil
}

func (s testSpace) store(ctx context.Context, nodeID, concept, text string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO `+s.tableName+` (id, concept, vector_field, embedding, created_at, updated_at)
VALUES ($1, $2, 'content', $3::vector, NOW(), NOW())
ON CONFLICT (id, vector_field) DO UPDATE SET embedding = EXCLUDED.embedding, concept = EXCLUDED.concept, updated_at = NOW()`,
		nodeID, concept, vectorLiteral(bagOfWords(text)))
	return err
}

func bagOfWords(text string) []float32 {
	vec := make([]float32, testDims)
	for _, w := range strings.Fields(strings.ToLower(text)) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(strings.Trim(w, ".,!?")))
		vec[h.Sum32()%testDims]++
	}
	vec[testDims-1] += 0.01 // never the zero vector
	return vec
}

// failingSpace is the embedder while the model is away: the space exists, but
// nothing can be embedded, for a read or for a write.
type failingSpace struct{ testSpace }

var errModelAway = errors.New("no model is reachable right now")

func (failingSpace) embedQuery(context.Context, string) ([]float32, error) { return nil, errModelAway }

func (failingSpace) store(context.Context, string, string, string) error { return errModelAway }
