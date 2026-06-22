package memql

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Production wiring for the semantic AI cache: an embedder that wraps the
// engine's existing EmbeddingAIProvider and a SemanticVectorStore backed by
// the semantic_ai_cache pgvector table (migration 20260622000000). Both
// implement the narrow interfaces the primitive (ai_semantic_cache.go) depends
// on, keeping the primitive and its eval hermetic while the real path reuses
// the same embedding provider + pgvector substrate the rest of memQL uses.

// providerEmbedder adapts an EmbeddingAIProvider to the SemanticEmbedder
// interface the semantic cache consumes.
type providerEmbedder struct {
	provider EmbeddingAIProvider
}

// newProviderEmbedder wraps a resolved EmbeddingAIProvider. Returns nil when
// the provider is nil so the caller can leave the semantic cache un-wired
// (Lookup degrades to a clean miss).
func newProviderEmbedder(p EmbeddingAIProvider) SemanticEmbedder {
	if p == nil {
		return nil
	}
	return &providerEmbedder{provider: p}
}

func (e *providerEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if e == nil || e.provider == nil {
		return nil, fmt.Errorf("semantic cache: embedding provider not configured")
	}
	return e.provider.Embed(ctx, text)
}

// pgSemanticStore implements SemanticVectorStore over the semantic_ai_cache
// table. Nearest does a cosine-distance scan scoped to the namespace and
// skips expired rows; Store upserts a fresh row with a TTL-derived expiry.
type pgSemanticStore struct {
	dbGetter func() *sql.DB
}

// newPGSemanticStore builds the Postgres-backed store. A nil dbGetter (or one
// that resolves to nil at call time) makes Nearest/Store error, which the
// primitive treats as fail-open (a clean miss / un-cached result).
func newPGSemanticStore(dbGetter func() *sql.DB) SemanticVectorStore {
	if dbGetter == nil {
		return nil
	}
	return &pgSemanticStore{dbGetter: dbGetter}
}

func (s *pgSemanticStore) db() *sql.DB {
	if s == nil || s.dbGetter == nil {
		return nil
	}
	return s.dbGetter()
}

func (s *pgSemanticStore) Nearest(ctx context.Context, namespace string, vec []float32) (any, float64, bool, error) {
	db := s.db()
	if db == nil {
		return nil, 0, false, fmt.Errorf("semantic cache: database not configured")
	}
	lit := semanticVectorLiteral(vec)
	const q = `
		SELECT value, 1 - (embedding <=> $1::vector) AS similarity
		FROM semantic_ai_cache
		WHERE namespace = $2
		  AND expires_at > NOW()
		ORDER BY embedding <=> $1::vector
		LIMIT 1
	`
	var (
		raw json.RawMessage
		sim float64
	)
	err := db.QueryRowContext(ctx, q, lit, namespace).Scan(&raw, &sim)
	if err == sql.ErrNoRows {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, fmt.Errorf("semantic cache nearest: %w", err)
	}
	if sim < 0 {
		sim = 0
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, 0, false, fmt.Errorf("semantic cache decode value: %w", err)
	}
	return value, sim, true, nil
}

func (s *pgSemanticStore) Store(ctx context.Context, namespace string, vec []float32, value any, ttl time.Duration) error {
	db := s.db()
	if db == nil {
		return fmt.Errorf("semantic cache: database not configured")
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("semantic cache encode value: %w", err)
	}
	lit := semanticVectorLiteral(vec)
	const q = `
		INSERT INTO semantic_ai_cache (namespace, embedding, value, created_at, expires_at)
		VALUES ($1, $2::vector, $3::jsonb, NOW(), NOW() + ($4 || ' seconds')::interval)
	`
	if _, err := db.ExecContext(ctx, q, namespace, lit, string(payload), fmt.Sprintf("%d", int64(ttl.Seconds()))); err != nil {
		return fmt.Errorf("semantic cache store: %w", err)
	}
	return nil
}

// semanticVectorLiteral formats a float32 slice as a pgvector literal.
// Mirrors integrations/embedding.vectorLiteral; duplicated here to keep
// component/memql free of an integrations import.
func semanticVectorLiteral(vec []float32) string {
	parts := make([]string, len(vec))
	for i, v := range vec {
		parts[i] = fmt.Sprintf("%g", v)
	}
	return "[" + strings.Join(parts, ",") + "]"
}
