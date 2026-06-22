-- Semantic (vector) AI-call cache  --  Epic 5, issue 5.9.
--
-- Stores the rendered-prompt embedding alongside the cached AIResult, scoped
-- by classification namespace, with a hard expiry. This is the persistence
-- substrate behind component/memql's SemanticVectorStore (pgvector store).
-- It sits beside node_vectors (which has no value column and a (id,
-- vector_field) PK) rather than overloading it: the semantic cache needs to
-- carry the cached value and a per-row TTL.
--
-- A near-neighbour read returns the closest row WITHIN the namespace by cosine
-- distance; the application gates the hit on a per-namespace similarity
-- threshold (a wrong cached answer is the whole risk -- the threshold, not the
-- query, is the guard).
CREATE TABLE IF NOT EXISTS semantic_ai_cache (
  id          BIGSERIAL PRIMARY KEY,
  namespace   TEXT        NOT NULL,
  embedding   vector(1536) NOT NULL,
  value       JSONB       NOT NULL,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  expires_at  TIMESTAMPTZ NOT NULL
);

-- Cosine ANN index per namespace lookup. HNSW over the whole table; the
-- namespace predicate is applied as a filter on the candidate set.
CREATE INDEX IF NOT EXISTS idx_semantic_ai_cache_hnsw
  ON semantic_ai_cache USING hnsw (embedding vector_cosine_ops);

CREATE INDEX IF NOT EXISTS idx_semantic_ai_cache_ns
  ON semantic_ai_cache (namespace);

CREATE INDEX IF NOT EXISTS idx_semantic_ai_cache_expires
  ON semantic_ai_cache (expires_at);
