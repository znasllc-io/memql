// Package embedding provides an IntegrationProvider that exposes vector
// embedding capabilities to the MemQL DSL. Handlers call the engine's
// ProviderRegistry for embedding computation and store/query vectors in
// the node_vectors and embedding_cache PostgreSQL tables (pgvector).
package embedding

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// EmbeddingProviderFunc resolves an EmbeddingAIProvider by name from the engine.
type EmbeddingProviderFunc func(name string) (memql.EmbeddingAIProvider, error)

// PartitionFunc returns the active partition for the current request context.
type PartitionFunc func(ctx context.Context) string

// Integration provides embedding capabilities callable from the MemQL DSL.
type Integration struct {
	Logger            *slog.Logger
	dbGetter          func() *sql.DB
	embeddingProvider EmbeddingProviderFunc
	partitionFunc     PartitionFunc
	stagedConcept     func(conceptId string) bool
}

// New creates a new embedding integration.
func New(logger *slog.Logger) *Integration {
	return &Integration{Logger: logger}
}

// SetDBGetter wires a lazy database accessor. The integration resolves the
// handle at capability-invocation time rather than at construction: the
// MemoryNodesDatabase returns a live *sql.DB only after its Start() runs,
// which happens after the plug-in factory has already fired.
func (i *Integration) SetDBGetter(fn func() *sql.DB) { i.dbGetter = fn }

// db resolves the current database handle, or nil if none is configured.
// Capability handlers that require SQL access guard on nil explicitly.
func (i *Integration) db() *sql.DB {
	if i.dbGetter == nil {
		return nil
	}
	return i.dbGetter()
}

// SetEmbeddingProvider sets the function that resolves embedding providers by name.
func (i *Integration) SetEmbeddingProvider(fn EmbeddingProviderFunc) { i.embeddingProvider = fn }

// SetPartitionFunc sets the function that resolves the active partition from context.
func (i *Integration) SetPartitionFunc(fn PartitionFunc) { i.partitionFunc = fn }

// SetStagedConceptPredicate injects the staged-DATA visibility question
// (epic memql#3974, task memql#3984). findSimilar takes its concept from the
// DSL call, so this is the surface an authored, still-staged concept reaches
// first.
func (i *Integration) SetStagedConceptPredicate(fn func(conceptId string) bool) { i.stagedConcept = fn }

// IntegrationName returns the stable identifier for this integration.
func (i *Integration) IntegrationName() string { return "embedding" }

// Capabilities returns DSL-callable embedding operations.
func (i *Integration) Capabilities() []memql.IntegrationCapability {
	return []memql.IntegrationCapability{
		{
			Name:        "embed",
			Description: "Compute a vector embedding for input text",
			Handler:     i.embedHandler,
			ArgsSchema: map[string]string{
				"text":     "string (required) - text to embed",
				"provider": "string (optional) - embedding provider name (default: embedding3Small)",
			},
		},
		{
			Name:        "findSimilar",
			Description: "Find nodes similar to a text query by vector cosine distance",
			Handler:     i.findSimilarHandler,
			ArgsSchema: map[string]string{
				"text":        "string (required) - query text",
				"concept":     "string (required) - concept to search within",
				"vectorField": "string (optional) - which embedding field to compare (default: profile)",
				"limit":       "int (optional) - max results (default: 10, or target param)",
			},
		},
		{
			Name:        "store",
			Description: "Store an embedding vector for a node",
			Handler:     i.storeHandler,
			ArgsSchema: map[string]string{
				"nodeId":      "string (required) - node to attach the embedding to",
				"text":        "string (required) - text to embed and store",
				"concept":     "string (optional) - concept of the node (stored for filtered queries)",
				"vectorField": "string (optional) - field name for the vector (default: content)",
				"provider":    "string (optional) - embedding provider name (default: embedding3Small)",
			},
		},
	}
}

// Embed is a public method that returns a vector embedding for the given text.
// This allows other integrations (e.g., cognition) to call embedding directly
// without going through the DSL.
func (i *Integration) Embed(ctx context.Context, text, providerName string) ([]float32, error) {
	if text == "" {
		return nil, fmt.Errorf("embed: text is required")
	}
	if providerName == "" {
		providerName = "embedding3Small"
	}

	if i.embeddingProvider == nil {
		return nil, fmt.Errorf("embed: embedding provider not configured")
	}

	// Check cache first.
	key := cacheKey(text, providerName)
	if i.db() != nil {
		cached, err := i.lookupCache(ctx, key)
		if err == nil && cached != nil {
			return cached, nil
		}
	}

	// Resolve provider and compute embedding.
	provider, err := i.embeddingProvider(providerName)
	if err != nil {
		return nil, fmt.Errorf("embed: resolve provider %q: %w", providerName, err)
	}

	vec, err := provider.Embed(ctx, text)
	if err != nil {
		return nil, fmt.Errorf("embed: compute embedding: %w", err)
	}

	// Store in cache (best-effort).
	if i.db() != nil {
		_ = i.storeCache(ctx, key, providerName, vec)
	}

	return vec, nil
}

// embedHandler computes an embedding for the given text.
func (i *Integration) embedHandler(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	text, _ := args["text"].(string)
	if text == "" {
		return nil, fmt.Errorf("embed: text is required")
	}
	providerName, _ := args["provider"].(string)
	if providerName == "" {
		providerName = "embedding3Small"
	}

	vec, err := i.Embed(ctx, text, providerName)
	if err != nil {
		return nil, err
	}

	i.Logger.Debug("embedding: embed completed", "text_len", len(text), "provider", providerName, "dimensions", len(vec))

	payloadBytes, _ := json.Marshal(map[string]any{
		"status":     "ok",
		"provider":   providerName,
		"textLen":    len(text),
		"dimensions": len(vec),
		"cacheKey":   cacheKey(text, providerName),
	})

	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("embedding:embed:%d", time.Now().UnixNano()),
		Concept:   "integration:embedding:result",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payloadBytes,
	}}, nil
}

// findSimilarHandler queries node_vectors for nodes similar to a text query.
//
// staged-data: GATE -- the highest-exposure hand-rolled read in the tree. This
// is the DSL-callable `findSimilar` tool: its `concept` comes from the CALLER,
// and the rows it returns go straight into LLM context. A newly promoted,
// still-staged, vector-indexed concept reaches an agent through here on the
// first call with no further change anywhere.
//
// The check is a Go pre-check rather than a conjunct in the statement, for the
// reason recorded at integrations/similarity/capabilities.go: the query pins one
// concept, staging is a pure function of the concept (memql#3980), so deciding
// before the round-trip produces exactly the result an in-query conjunct would.
//
// Sequencing note: the statement below gets its LATEST-VERSION COLLAPSE from
// memql#3984's other half (PR #4037), and that ordering is load-bearing rather
// than incidental -- an uncollapsed join returns an ARBITRARY version of an id,
// and a visibility predicate answered about the wrong version is not a
// visibility predicate. The gate here is concept-grain, so it is correct either
// way, which is why the two halves could land independently.
func (i *Integration) findSimilarHandler(ctx context.Context, args map[string]any, target int) ([]memorynodes.MemoryNode, error) {
	text, _ := args["text"].(string)
	concept, _ := args["concept"].(string)
	vectorField, _ := args["vectorField"].(string)
	if text == "" || concept == "" {
		return nil, fmt.Errorf("findSimilar: text and concept are required")
	}
	if i.stagedConcept == nil {
		return nil, fmt.Errorf("findSimilar: staged-concept predicate not wired; " +
			"refusing to search rather than answer as if nothing were staged")
	}
	if i.stagedConcept(concept) {
		// Empty, not an error: a staged concept has to be indistinguishable
		// from one nothing has been written to. An error would disclose that
		// it exists and is being withheld.
		i.Logger.Info("findSimilar: concept data is STAGED; returning no rows", "concept", concept)
		return nil, nil
	}
	if vectorField == "" {
		vectorField = "profile"
	}

	limit := target
	if limit <= 0 {
		limit = 10
	}

	if i.db() == nil {
		return nil, fmt.Errorf("findSimilar: database not configured")
	}

	// Embed query text.
	providerName, _ := args["provider"].(string)
	vec, err := i.Embed(ctx, text, providerName)
	if err != nil {
		return nil, fmt.Errorf("findSimilar: %w", err)
	}

	// Format embedding as pgvector literal: [0.1,0.2,...]
	vecLiteral := vectorLiteral(vec)

	// LATEST-VERSION COLLAPSE (memql#3984). This query used to join
	// `"MemoryNodes" n` to `node_vectors nv ON n.id = nv.id` with no collapse
	// at all -- no DISTINCT ON, no MAX("createdAt"), no correlated subquery --
	// and the two tables are keyed differently in exactly the way that makes
	// that fatal. "MemoryNodes" is append-only with PK (id, "createdAt"), so
	// one logical row is MANY table rows, one per version; node_vectors is
	// PK (id, vector_field) (migration 20260325000002_vector_tables.up.sql),
	// so it holds ONE row per id. Two consequences, both real:
	//
	//  1. VERSION FAN-OUT. The join matched every version of an id, so a node
	//     edited 15 times contributed 15 result rows at identical similarity,
	//     each consuming one slot of LIMIT $4. A caller asking for the top 10
	//     could receive one document 10 times and nothing else.
	//  2. ARBITRARY VERSION RETURNED. With nothing ordering on "createdAt",
	//     the payload served for an id was whichever version the planner
	//     emitted. A field redacted or corrected in version 12 could be served
	//     from version 3.
	//
	// These rows reach LLM context through the DSL-callable `findSimilar`
	// tool, so this is a correctness and data-freshness defect, not a
	// performance nit -- and it has to be fixed BEFORE staged-data adds its
	// visibility predicate here (memql#3974/#3984), because a predicate
	// evaluated against an arbitrary version answers about the wrong row.
	//
	// WHY A CTE OVER "MemoryNodes" RATHER THAN `DISTINCT ON` OVER THE JOINED
	// RESULT -- the two are NOT equivalent:
	//
	//  1. Postgres requires a `DISTINCT ON (expr)` to match the LEADING
	//     ORDER BY terms, and picking the latest version needs
	//     `ORDER BY n.id, n."createdAt" DESC`. So a joined-result DISTINCT ON
	//     cannot ALSO be ordered by similarity; it needs an enclosing query to
	//     re-sort, which sorts the whole collapsed set a second time before
	//     LIMIT can apply. The CTE form leaves the statement exactly ONE
	//     ordering -- the similarity one -- which is the shape the pgvector
	//     HNSW index on node_vectors exists to serve.
	//  2. Joining first fans the join itself out to (versions per id) rows
	//     that the collapse then discards. Collapsing first makes the join
	//     1:1: node_vectors' PK (id, vector_field) admits at most one row per
	//     id once the outer WHERE pins vector_field.
	//
	// Either shape must collapse BEFORE the LIMIT, or the limit keeps
	// budgeting versions instead of distinct ids. The four sibling retrieval
	// paths -- integrations/similarity/capabilities.go,
	// integrations/actionsearch/actionsearch.go,
	// integrations/harnessrecall/recall.go and
	// integrations/knowledge/embed_domain.go -- all already take this CTE
	// shape; making the fifth read alike is deliberate.
	//
	// The concept predicate lives INSIDE the CTE (it prunes rows before the
	// DISTINCT ON sort, and an id's concept does not change across versions);
	// vector_field is a node_vectors column, so it stays in the outer WHERE.
	// Both original WHERE semantics are preserved, as is the parameter
	// order/count the call site below passes: $1 query vector, $2 concept,
	// $3 vector field, $4 limit.
	query := `
		WITH latest AS (
			SELECT DISTINCT ON (id) id, concept, payload
			FROM "MemoryNodes"
			WHERE concept = $2
			ORDER BY id, "createdAt" DESC
		)
		SELECT latest.id, latest.concept, latest.payload,
		       1 - (nv.embedding <=> $1::vector) AS similarity
		FROM latest
		JOIN node_vectors nv ON nv.id = latest.id
		WHERE nv.vector_field = $3
		ORDER BY nv.embedding <=> $1::vector
		LIMIT $4
	`

	rows, err := i.db().QueryContext(ctx, query, vecLiteral, concept, vectorField, limit)
	if err != nil {
		return nil, fmt.Errorf("findSimilar: query: %w", err)
	}
	defer rows.Close()

	var results []memorynodes.MemoryNode
	for rows.Next() {
		var (
			id         string
			nodeConcpt string
			payload    json.RawMessage
			similarity float64
		)
		if err := rows.Scan(&id, &nodeConcpt, &payload, &similarity); err != nil {
			i.Logger.Warn("findSimilar: scan row", "error", err)
			continue
		}

		// Merge similarity into payload.
		var payloadMap map[string]any
		_ = json.Unmarshal(payload, &payloadMap)
		if payloadMap == nil {
			payloadMap = make(map[string]any)
		}
		payloadMap["_similarity"] = similarity
		mergedPayload, _ := json.Marshal(payloadMap)

		results = append(results, memorynodes.MemoryNode{
			ID:        id,
			Concept:   nodeConcpt,
			Type:      memorynodes.NodeTypeObject,
			CreatedAt: time.Now().UTC(),
			Payload:   mergedPayload,
		})
	}

	i.Logger.Debug("embedding: findSimilar completed",
		"concept", concept, "vectorField", vectorField, "text_len", len(text), "results", len(results))

	return results, nil
}

// storeHandler stores an embedding for a specific node and field.
func (i *Integration) storeHandler(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	nodeId, _ := args["nodeId"].(string)
	vectorField, _ := args["vectorField"].(string)
	text, _ := args["text"].(string)
	providerName, _ := args["provider"].(string)
	conceptName, _ := args["concept"].(string)
	if nodeId == "" || text == "" {
		return nil, fmt.Errorf("store: nodeId and text are required")
	}
	if vectorField == "" {
		vectorField = "content"
	}
	if providerName == "" {
		providerName = "embedding3Small"
	}

	if i.db() == nil {
		return nil, fmt.Errorf("store: database not configured")
	}

	// Embed the text.
	vec, err := i.Embed(ctx, text, providerName)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}

	vecLiteral := vectorLiteral(vec)

	query := `
		INSERT INTO node_vectors (id, concept, vector_field, embedding, created_at, updated_at)
		VALUES ($1, $2, $3, $4::vector, NOW(), NOW())
		ON CONFLICT (id, vector_field) DO UPDATE SET
		  embedding = EXCLUDED.embedding,
		  concept = EXCLUDED.concept,
		  updated_at = NOW()
	`

	_, err = i.db().ExecContext(ctx, query, nodeId, conceptName, vectorField, vecLiteral)
	if err != nil {
		return nil, fmt.Errorf("store: insert node_vectors: %w", err)
	}

	i.Logger.Debug("embedding: store completed",
		"nodeId", nodeId, "vectorField", vectorField, "text_len", len(text), "provider", providerName)

	payloadBytes, _ := json.Marshal(map[string]any{
		"status":      "stored",
		"nodeId":      nodeId,
		"vectorField": vectorField,
		"provider":    providerName,
		"dimensions":  len(vec),
	})

	return []memorynodes.MemoryNode{{
		ID:        fmt.Sprintf("embedding:store:%d", time.Now().UnixNano()),
		Concept:   "integration:embedding:result",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   payloadBytes,
	}}, nil
}

// resolvePartition returns the active partition from context or "default".
func (i *Integration) resolvePartition(ctx context.Context) string {
	if i.partitionFunc != nil {
		if p := i.partitionFunc(ctx); p != "" {
			return p
		}
	}
	return "default"
}

// lookupCache checks the embedding_cache table for a cached embedding.
func (i *Integration) lookupCache(ctx context.Context, key string) ([]float32, error) {
	var vecStr string
	err := i.db().QueryRowContext(ctx,
		`SELECT embedding::text FROM embedding_cache WHERE cache_key = $1 AND (expires_at IS NULL OR expires_at > NOW())`,
		key,
	).Scan(&vecStr)
	if err != nil {
		return nil, err
	}
	return parseVectorLiteral(vecStr)
}

// storeCache inserts an embedding into the embedding_cache table.
func (i *Integration) storeCache(ctx context.Context, key, provider string, vec []float32) error {
	vecLiteral := vectorLiteral(vec)
	_, err := i.db().ExecContext(ctx,
		`INSERT INTO embedding_cache (cache_key, embedding, provider, created_at) VALUES ($1, $2::vector, $3, NOW()) ON CONFLICT (cache_key) DO UPDATE SET embedding = EXCLUDED.embedding, provider = EXCLUDED.provider, created_at = NOW()`,
		key, vecLiteral, provider,
	)
	return err
}

// cacheKey generates a deterministic key for the embedding cache.
func cacheKey(text, provider string) string {
	return string(id.New().MustFromMap(map[string]any{
		"kind":     "embedding-cache",
		"text":     text,
		"provider": provider,
	}))
}

// vectorLiteral formats a float32 slice as a pgvector literal: [0.1,0.2,...].
func vectorLiteral(vec []float32) string {
	parts := make([]string, len(vec))
	for i, v := range vec {
		parts[i] = fmt.Sprintf("%g", v)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// parseVectorLiteral parses a pgvector text representation back to []float32.
func parseVectorLiteral(s string) ([]float32, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	if s == "" {
		return nil, fmt.Errorf("empty vector literal")
	}
	parts := strings.Split(s, ",")
	vec := make([]float32, len(parts))
	for i, p := range parts {
		var v float64
		_, err := fmt.Sscanf(strings.TrimSpace(p), "%g", &v)
		if err != nil {
			return nil, fmt.Errorf("parse vector element %d: %w", i, err)
		}
		vec[i] = float32(v)
	}
	return vec, nil
}
