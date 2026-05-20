// Package embedding provides an IntegrationProvider that exposes vector
// embedding capabilities to the MemQL DSL. Handlers call the engine's
// ProviderRegistry for embedding computation and store/query vectors in
// the node_vectors and embedding_cache PostgreSQL tables (pgvector).
package embedding

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// EmbeddingProviderFunc resolves an EmbeddingSIProvider by name from the engine.
type EmbeddingProviderFunc func(name string) (memql.EmbeddingSIProvider, error)

// PartitionFunc returns the active partition for the current request context.
type PartitionFunc func(ctx context.Context) string

// Integration provides embedding capabilities callable from the MemQL DSL.
type Integration struct {
	Logger            *slog.Logger
	dbGetter          func() *sql.DB
	embeddingProvider EmbeddingProviderFunc
	partitionFunc     PartitionFunc
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
func (i *Integration) findSimilarHandler(ctx context.Context, args map[string]any, target int) ([]memorynodes.MemoryNode, error) {
	text, _ := args["text"].(string)
	concept, _ := args["concept"].(string)
	vectorField, _ := args["vectorField"].(string)
	if text == "" || concept == "" {
		return nil, fmt.Errorf("findSimilar: text and concept are required")
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

	query := `
		SELECT n.id, n.concept, n.payload,
		       1 - (nv.embedding <=> $1::vector) AS similarity
		FROM "MemoryNodes" n
		JOIN node_vectors nv ON n.id = nv.id
		WHERE n.concept = $2
		  AND nv.vector_field = $3
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
	h := sha256.Sum256([]byte(text + "|" + provider))
	return hex.EncodeToString(h[:])
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
