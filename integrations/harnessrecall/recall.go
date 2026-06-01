// Package harnessrecall exposes the `recall` DSL operator -- the
// MemQL-native hybrid recency x relevance memory query (#585, epic
// #590). It is the strongest demonstration of why the time-series +
// pgvector substrate matters: a bolt-on architecture has to query a
// vector store, separately query a recency/log store, merge, and
// re-rank in application code. Because embeddings live in
// `node_vectors` keyed by node id and `createdAt` lives on the same
// `MemoryNodes` hypertable, recall scores recency x relevance in ONE
// SQL statement against ONE table -- no app-side merge.
//
// DSL surface (dsl/harness/queries.memql -> builtin recall):
//
//	recall({
//	  text:     "<free-form query, embedded server-side>",
//	  concept:  "v1:harness:observation",  // recall source (default)
//	  window:   86400,    // seconds; time-window for hypertable pruning
//	  k:        10,        // top-k
//	  halfLife: 3600,      // recency half-life (seconds)
//	  wSem:     0.7,        // semantic weight
//	  wRec:     0.3,        // recency weight
//	  provider: "embedding3Small"
//	})
//
// The single statement:
//   - embeds the query text via the named embedding provider,
//   - selects the latest version of each node of `concept`,
//   - filters by owner (partition isolation -- an agent only recalls
//     its own workspace's memory),
//   - filters createdAt >= now()-window so TimescaleDB prunes chunks
//     outside the window (the hypertable is partitioned on createdAt),
//   - joins node_vectors for cosine similarity,
//   - scores  w_sem*cosine + w_rec*exp(-ln2*age/halfLife)  in SQL,
//   - orders by score desc, limits k.
//
// Provenance is preserved: the full payload (including createdBy,
// stepId, planId, kind) rides back on every recalled row, alongside
// the computed _score / _similarity / _recency components for
// auditability.
package harnessrecall

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	componentAuth "github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

const (
	// defaultConcept is the primary recall source. Observations carry
	// `content` embedded into node_vectors keyed by the observation id.
	defaultConcept = memorynodes.ConceptHarnessObservation
	// defaultProvider matches the embedding write-path + similarTo.
	defaultProvider = "embedding3Small"
	// defaultK is the top-k when the caller omits it.
	defaultK = 10
	// defaultHalfLifeSeconds: 1h. Recent, relevant memories rank
	// highest; the contribution of recency halves every halfLife.
	defaultHalfLifeSeconds = 3600.0
	// defaultWindowSeconds: 0 means "no time-window filter" (recall
	// across all history). A positive value prunes hypertable chunks.
	defaultWindowSeconds = 0.0
	// Default weights favour relevance but keep recency meaningful.
	defaultWSem = 0.7
	defaultWRec = 0.3
	// ln2 is the decay constant numerator: a half-life means the
	// recency term is exactly 0.5 at age == halfLife.
	ln2 = 0.6931471805599453
)

// Integration holds the state the recall handler needs. Constructed by
// the plug-in factory and populated via setters at registration time,
// mirroring integrations/similarity.
type Integration struct {
	Logger *slog.Logger

	dbGetter          func() *sql.DB
	embeddingProvider func(name string) (memql.EmbeddingSIProvider, error)
}

// New constructs a recall integration.
func New(logger *slog.Logger) *Integration {
	if logger == nil {
		logger = slog.Default()
	}
	return &Integration{Logger: logger}
}

// SetDBGetter injects the lazy DB handle getter (bun.DB isn't live
// until MemoryNodesDatabase.Start has fired).
func (i *Integration) SetDBGetter(f func() *sql.DB) { i.dbGetter = f }

// SetEmbeddingProvider injects the provider registry lookup.
func (i *Integration) SetEmbeddingProvider(f func(name string) (memql.EmbeddingSIProvider, error)) {
	i.embeddingProvider = f
}

// IntegrationName implements memql.IntegrationProvider.
func (i *Integration) IntegrationName() string { return "harnessRecall" }

// Capabilities lists the DSL-callable functions.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "recall",
			Description: "Recall top-k memories of a concept by a single hybrid recency x relevance score (pgvector cosine + exponential time-decay), in one SQL statement against the MemoryNodes hypertable. Owner-scoped (partition isolation); time-window prunes hypertable chunks. Tunable halfLife + weights.",
			Handler:     i.recallHandler,
			ArgsSchema: map[string]string{
				"text":     "string (required) - free-text query embedded server-side and matched by cosine similarity",
				"concept":  "string (optional) - recall source concept id (default v1:harness:observation)",
				"window":   "number (optional) - time window in seconds; only memories with createdAt >= now()-window are scanned (hypertable partition pruning). 0/omitted = all history",
				"k":        "int (optional) - top-k to return (default 10, or the target param)",
				"halfLife": "number (optional) - recency half-life in seconds (default 3600); the recency term halves every halfLife of age",
				"wSem":     "number (optional) - semantic (cosine) weight (default 0.7)",
				"wRec":     "number (optional) - recency (decay) weight (default 0.3)",
				"provider": "string (optional) - embedding provider name (default embedding3Small)",
			},
			// The handler scores + orders server-side; PreserveOrder
			// stamps monotonic CreatedAt so the engine's default
			// sort-by-createdAt-desc reproduces our score order.
			PreserveOrder: true,
		},
	}
}

// recallParams is the resolved, validated parameter set for one recall.
type recallParams struct {
	text          string
	concept       string
	provider      string
	ownerUserId   string
	k             int
	windowSeconds float64
	halfLife      float64
	wSem          float64
	wRec          float64
}

// recallScore is the pure, table-testable scoring function used by the
// SQL expression. Keeping it in Go lets a unit test prove that
// changing halfLife reorders results, and documents the exact formula
// the SQL implements:
//
//	score = wSem*cosine + wRec*exp(-ln2*ageSeconds/halfLife)
//
// cosine is the pgvector cosine similarity (1 - cosine distance) in
// [-1, 1]; ageSeconds is now-createdAt; a non-positive halfLife
// disables the recency term (treated as weight-0) to avoid div-by-zero.
func recallScore(cosine, ageSeconds, halfLife, wSem, wRec float64) float64 {
	recency := 0.0
	if halfLife > 0 {
		if ageSeconds < 0 {
			ageSeconds = 0
		}
		recency = math.Exp(-ln2 * ageSeconds / halfLife)
	}
	return wSem*cosine + wRec*recency
}

// recallHandler is the single DSL-facing entry point. It embeds the
// query, then issues ONE SQL statement that filters (owner + window),
// joins node_vectors, scores recency x relevance, orders, and limits.
func (i *Integration) recallHandler(ctx context.Context, args map[string]any, target int) ([]memorynodes.MemoryNode, error) {
	handlerStart := time.Now()

	p, err := i.resolveParams(ctx, args, target)
	if err != nil {
		return nil, err
	}

	if i.dbGetter == nil || i.dbGetter() == nil {
		return nil, fmt.Errorf("harnessRecall.recall: database not configured")
	}
	if i.embeddingProvider == nil {
		return nil, fmt.Errorf("harnessRecall.recall: embedding provider not configured")
	}

	provider, err := i.embeddingProvider(p.provider)
	if err != nil {
		return nil, fmt.Errorf("harnessRecall.recall: resolve provider %q: %w", p.provider, err)
	}
	embedStart := time.Now()
	vec, err := provider.Embed(ctx, p.text)
	if err != nil {
		return nil, fmt.Errorf("harnessRecall.recall: embed query: %w", err)
	}
	embedElapsed := time.Since(embedStart)
	vecLiteral := vectorLiteral(vec)

	rows, err := i.dbGetter().QueryContext(ctx, recallSQL, recallSQLArgs(p, vecLiteral)...)
	if err != nil {
		return nil, fmt.Errorf("harnessRecall.recall: query: %w", err)
	}
	defer rows.Close()

	results := make([]memorynodes.MemoryNode, 0, p.k)
	for rows.Next() {
		var (
			id         string
			createdAt  time.Time
			payload    json.RawMessage
			similarity float64
			recency    float64
			score      float64
		)
		if err := rows.Scan(&id, &createdAt, &payload, &similarity, &recency, &score); err != nil {
			i.Logger.Warn("harnessRecall.recall: scan row", "error", err)
			continue
		}
		var payloadMap map[string]any
		_ = json.Unmarshal(payload, &payloadMap)
		if payloadMap == nil {
			payloadMap = make(map[string]any)
		}
		// Preserve provenance + expose the score components for
		// auditability / citation ranking downstream.
		payloadMap["_similarity"] = similarity
		payloadMap["_recency"] = recency
		payloadMap["_score"] = score
		merged, _ := json.Marshal(payloadMap)
		results = append(results, memorynodes.MemoryNode{
			ID:        id,
			Concept:   p.concept,
			Type:      memorynodes.NodeTypeObject,
			CreatedAt: createdAt.UTC(),
			Payload:   merged,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("harnessRecall.recall: rows: %w", err)
	}

	i.Logger.Info("harnessRecall.recall: completed",
		"elapsed_ms", time.Since(handlerStart).Milliseconds(),
		"embed_ms", embedElapsed.Milliseconds(),
		"concept", p.concept,
		"k", p.k,
		"window_s", p.windowSeconds,
		"half_life_s", p.halfLife,
		"w_sem", p.wSem,
		"w_rec", p.wRec,
		"rows", len(results),
	)
	return results, nil
}

// resolveParams validates + defaults the caller args and resolves the
// owner-scope key (partition isolation) from the auth context.
func (i *Integration) resolveParams(ctx context.Context, args map[string]any, target int) (recallParams, error) {
	p := recallParams{
		concept:  defaultConcept,
		provider: defaultProvider,
		k:        defaultK,
		halfLife: defaultHalfLifeSeconds,
		wSem:     defaultWSem,
		wRec:     defaultWRec,
	}

	p.text, _ = args["text"].(string)
	if strings.TrimSpace(p.text) == "" {
		return p, fmt.Errorf("harnessRecall.recall: text is required")
	}
	if c, _ := args["concept"].(string); strings.TrimSpace(c) != "" {
		p.concept = c
	}
	if pr, _ := args["provider"].(string); strings.TrimSpace(pr) != "" {
		p.provider = pr
	}

	if k, ok := toInt(args["k"]); ok && k > 0 {
		p.k = k
	} else if target > 0 {
		p.k = target
	}

	p.windowSeconds = defaultWindowSeconds
	if w, ok := toFloat(args["window"]); ok && w > 0 {
		p.windowSeconds = w
	}
	if hl, ok := toFloat(args["halfLife"]); ok && hl > 0 {
		p.halfLife = hl
	}
	if ws, ok := toFloat(args["wSem"]); ok {
		p.wSem = ws
	}
	if wr, ok := toFloat(args["wRec"]); ok {
		p.wRec = wr
	}

	// Owner-scope = partition isolation. The same actor.userId the
	// mutation stamps onto ownerUserId; an agent recalls only its
	// workspace's memory. Empty (dev mode / no auth) falls back to an
	// unscoped read so local dev still works -- production always has
	// an access context.
	if ac, ok := componentAuth.AccessFromContext(ctx); ok && ac != nil {
		p.ownerUserId = strings.TrimSpace(ac.UserId)
	}
	return p, nil
}

// recallSQL is the single statement. Parameters:
//
//	$1 query vector literal
//	$2 concept id
//	$3 owner user id ('' = unscoped, dev only)
//	$4 window seconds (0 = no window)
//	$5 half-life seconds
//	$6 w_sem
//	$7 w_rec
//	$8 limit k
//
// `latest` picks the newest version per id (memQL is append-only /
// time-series). The owner + window predicates live INSIDE the latest
// CTE so the time-window prunes hypertable chunks before the vector
// join. Cosine similarity is `1 - (embedding <=> qv)`; the recency
// term is `exp(-ln2 * age / halfLife)`; the combined score is the
// weighted sum. One round-trip, one table family, no app-side merge.
const recallSQL = `
WITH latest AS (
    SELECT DISTINCT ON (id) id, "createdAt", payload
    FROM "MemoryNodes"
    WHERE concept = $2
      AND ($3 = '' OR (payload->>'ownerUserId') = $3)
      AND ($4 <= 0 OR "createdAt" >= now() - make_interval(secs => $4))
    ORDER BY id, "createdAt" DESC
)
SELECT latest.id,
       latest."createdAt",
       latest.payload,
       (1 - (nv.embedding <=> $1::vector))                                  AS similarity,
       exp(-0.6931471805599453 * GREATEST(extract(epoch FROM (now() - latest."createdAt")), 0) / $5) AS recency,
       $6 * (1 - (nv.embedding <=> $1::vector))
         + $7 * exp(-0.6931471805599453 * GREATEST(extract(epoch FROM (now() - latest."createdAt")), 0) / $5) AS score
FROM latest
JOIN node_vectors nv ON nv.id = latest.id
WHERE nv.vector_field = 'content'
ORDER BY score DESC
LIMIT $8
`

// recallSQLArgs binds the params in the order recallSQL expects.
func recallSQLArgs(p recallParams, vecLiteral string) []any {
	halfLife := p.halfLife
	if halfLife <= 0 {
		// Guard the SQL divisor; a non-positive half-life means "no
		// recency decay", which we model as an enormous half-life so
		// exp(...) ~= 1 and the recency term is effectively constant.
		halfLife = math.MaxFloat32
	}
	return []any{
		vecLiteral,      // $1
		p.concept,       // $2
		p.ownerUserId,   // $3
		p.windowSeconds, // $4
		halfLife,        // $5
		p.wSem,          // $6
		p.wRec,          // $7
		p.k,             // $8
	}
}

// --- helpers -------------------------------------------------------------

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case int32:
		return int(n), true
	case float64:
		return int(n), true
	case float32:
		return int(n), true
	}
	return 0, false
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	}
	return 0, false
}

func vectorLiteral(vec []float32) string {
	parts := make([]string, len(vec))
	for i, v := range vec {
		parts[i] = fmt.Sprintf("%g", v)
	}
	return "[" + strings.Join(parts, ",") + "]"
}
