-- #1758: HNSW index for the action library's intent embeddings so the
-- planner's searchActions() cosine search is fast. Mirrors the existing
-- content/profile partial indexes (one per vector_field).
CREATE INDEX IF NOT EXISTS idx_node_vectors_intent_hnsw
  ON node_vectors USING hnsw (embedding vector_cosine_ops)
  WHERE vector_field = 'intent';
