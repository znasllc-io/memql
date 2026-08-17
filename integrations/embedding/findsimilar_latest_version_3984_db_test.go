package embedding

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

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/memql"
)

// findsimilar_latest_version_3984_db_test.go -- the real-Postgres proof that
// `integration.embedding.findSimilar` collapses "MemoryNodes" to ONE row per
// id, at that id's LATEST version, BEFORE the LIMIT applies (memql#3984).
//
// The defect this pins is structural rather than statistical, which is why a
// single seeded fixture settles it. "MemoryNodes" is append-only with
// PK (id, "createdAt"), so one logical row is many table rows; node_vectors is
// PK (id, vector_field), so it holds exactly one row per id. The pre-fix query
// joined them on id alone with no collapse of any kind, so:
//
//   - every version of an id matched, at identical similarity, and each match
//     consumed one slot of the caller's LIMIT; and
//   - nothing ordered on "createdAt", so the payload served for an id was
//     whichever version the planner emitted.
//
// The fixture below makes both visible with three versions of one id: under
// the pre-fix SQL a `limit=2` call returns that ONE document twice and never
// reaches the second document at all, and a `limit=10` call returns four rows
// for two documents. Both assertions FAIL against the uncollapsed query and
// PASS against the CTE form -- which is the property that matters, since the
// bug is otherwise invisible to any test seeding one version per id.
//
// It must stay green before staged-data adds its visibility predicate to this
// handler (memql#3974/#3984): a predicate evaluated against an arbitrary
// version answers a question about the wrong row.
//
// Postgres-gated: skips when no DB is reachable (fails under MEMQL_REQUIRE_DB).

// probeConcept scopes every row this suite writes and reads. findSimilar
// filters on `concept = $2`, so a concept nothing else uses IS the isolation --
// no other suite's rows can reach these assertions and these rows cannot reach
// another suite's.
const probeConcept = "v1:embedding:findSimilarVersionProbe"

const (
	probeMultiVersionID = "v1:embedding:findSimilarVersionProbe:multi"
	probeSingleID       = "v1:embedding:findSimilarVersionProbe:single"
	probeVersions       = 3
)

// embedDimensions matches the node_vectors column type, vector(1536).
const embedDimensions = 1536

// stubEmbeddingProvider returns a fixed vector, so the suite exercises the SQL
// without an API key or a network call. The query vector is the unit basis
// vector e0; the seeded document vectors are chosen against it below.
type stubEmbeddingProvider struct{ vec []float32 }

func (s stubEmbeddingProvider) Embed(context.Context, string) ([]float32, error) { return s.vec, nil }

func (s stubEmbeddingProvider) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = s.vec
	}
	return out, nil
}

func (s stubEmbeddingProvider) Dimensions() int { return embedDimensions }

// basisVector returns the unit vector with 1.0 at position axis. Two distinct
// axes are cosine-orthogonal, which pins the result ORDER deterministically:
// the document sharing the query's axis scores 1.0, the other 0.0.
func basisVector(axis int) []float32 {
	v := make([]float32, embedDimensions)
	v[axis] = 1
	return v
}

func TestFindSimilarReturnsEachIdOnceAtItsLatestVersion(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.DSN()

	db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	// Leaked connections across the db-gated packages exhaust max_connections
	// for every suite sharing the lane's one database.
	t.Cleanup(func() { _ = db.Close() })

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		dbtest.Unreachable(t, "findSimilar latest-version collapse (memql#3984)", dsn, err)
		return
	}

	seedProbeRows(t, ctx, db)

	integration := New(slog.New(slog.NewTextHandler(io.Discard, nil)))
	integration.SetDBGetter(func() *sql.DB { return db })
	integration.SetEmbeddingProvider(func(string) (memql.EmbeddingAIProvider, error) {
		return stubEmbeddingProvider{vec: basisVector(0)}, nil
	})
	// Nothing is staged, stated explicitly rather than left to a default.
	//
	// The staged-data gate (memql#3984) FAILS CLOSED: an unwired predicate
	// refuses the search rather than answering as if nothing were staged, so
	// this line is required and not boilerplate. Wiring it to a constant false
	// is also what keeps this test about the SUBJECT it names -- the
	// latest-version collapse (memql#4037) -- rather than about staging, which
	// has its own coverage. If it ever needs to vary, that is a different test.
	integration.SetStagedConceptPredicate(func(string) bool { return false })

	args := map[string]any{
		// Unique per suite so the embedding_cache row this call writes cannot
		// collide with another suite's text.
		"text":        "memql#3984 findSimilar latest-version collapse probe",
		"concept":     probeConcept,
		"vectorField": "profile",
	}

	// THE LIMIT ASSERTION. Two documents exist and the caller asks for two.
	// Pre-fix, the three versions of the multi-version document filled both
	// slots and the single-version document was never reached.
	t.Run("limit budgets distinct ids, not versions", func(t *testing.T) {
		rows, err := integration.findSimilarHandler(ctx, args, 2)
		require.NoError(t, err)

		ids := make([]string, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.ID)
		}
		require.Equal(t, []string{probeMultiVersionID, probeSingleID}, ids,
			"a limit of 2 over 2 documents must return each once, most-similar first; "+
				"%d versions of %s consuming the whole limit is the memql#3984 fan-out",
			probeVersions, probeMultiVersionID)
	})

	// THE VERSION ASSERTION. With headroom to spare, the row count must equal
	// the DISTINCT id count, and the payload served must be the newest one.
	t.Run("one row per id, carrying the newest payload", func(t *testing.T) {
		rows, err := integration.findSimilarHandler(ctx, args, 10)
		require.NoError(t, err)
		require.Len(t, rows, 2,
			"expected one row per distinct id; %d rows means the join fanned out over versions",
			len(rows))

		require.Equal(t, probeMultiVersionID, rows[0].ID)
		require.Equal(t, probeConcept, rows[0].Concept)

		var payload map[string]any
		require.NoError(t, json.Unmarshal(rows[0].Payload, &payload))
		require.EqualValues(t, probeVersions, payload["version"],
			"served version %v rather than the latest (%d) -- with no ORDER BY on "+
				"\"createdAt\" the version returned is whichever one the planner emitted",
			payload["version"], probeVersions)
		// The handler's own contract, asserted here so a regression in the
		// projection cannot hide behind the collapse assertions.
		require.InDelta(t, 1.0, payload["_similarity"], 1e-6)
	})
}

// seedProbeRows writes the fixture: probeVersions versions of one id and a
// single version of another, plus one node_vectors row per id. Rows are
// removed first (so a previous failed run cannot skew the counts) and again on
// cleanup.
func seedProbeRows(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()

	deleteProbeRows(t, ctx, db)
	t.Cleanup(func() { deleteProbeRows(t, context.Background(), db) })

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Versions land oldest-first with an ASCENDING createdAt, so "the latest"
	// and "the last inserted" agree and a query that ignores ordering can
	// still plausibly return either.
	for v := 1; v <= probeVersions; v++ {
		insertNode(t, ctx, db, probeMultiVersionID, base.Add(time.Duration(v)*time.Hour), v)
	}
	insertNode(t, ctx, db, probeSingleID, base, 1)

	// One vector per id -- node_vectors is PK (id, vector_field), so this is
	// the only shape it can hold. The multi-version document shares the query
	// vector's axis (cosine 1.0); the other is orthogonal to it (cosine 0.0).
	insertVector(t, ctx, db, probeMultiVersionID, basisVector(0))
	insertVector(t, ctx, db, probeSingleID, basisVector(1))
}

func insertNode(t *testing.T, ctx context.Context, db *sql.DB, id string, createdAt time.Time, version int) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"version": version, "text": fmt.Sprintf("v%d", version)})
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO "MemoryNodes"
		   (id, "createdAt", "createdBy", schema, payload, metadata, "type", concept, provenance)
		 VALUES ($1, $2, 'memql#3984-probe', '{}'::jsonb, $3::jsonb, '{}'::jsonb, 'object', $4,
		         '{"kind":"system","name":"findsimilar-3984-probe"}'::jsonb)
		 ON CONFLICT (id, "createdAt") DO UPDATE SET payload = EXCLUDED.payload`,
		id, createdAt, string(payload), probeConcept)
	require.NoError(t, err, "seed %s version %d", id, version)
}

func insertVector(t *testing.T, ctx context.Context, db *sql.DB, id string, vec []float32) {
	t.Helper()
	_, err := db.ExecContext(ctx,
		`INSERT INTO node_vectors (id, concept, vector_field, embedding, created_at, updated_at)
		 VALUES ($1, $2, 'profile', $3::vector, NOW(), NOW())
		 ON CONFLICT (id, vector_field) DO UPDATE SET embedding = EXCLUDED.embedding`,
		id, probeConcept, vectorLiteral(vec))
	require.NoError(t, err, "seed vector for %s", id)
}

func deleteProbeRows(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	ids := []string{probeMultiVersionID, probeSingleID}
	placeholders := make([]string, len(ids))
	params := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		params[i] = id
	}
	in := "(" + strings.Join(placeholders, ",") + ")"
	_, err := db.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE id IN `+in, params...)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DELETE FROM node_vectors WHERE id IN `+in, params...)
	require.NoError(t, err)
}
