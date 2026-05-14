CREATE TABLE IF NOT EXISTS node_vectors (
  partition    TEXT NOT NULL,
  id           TEXT NOT NULL,
  concept      TEXT NOT NULL,
  vector_field TEXT NOT NULL,
  embedding    vector(1536) NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (partition, id, vector_field)
);

CREATE INDEX IF NOT EXISTS idx_node_vectors_profile_hnsw
  ON node_vectors USING hnsw (embedding vector_cosine_ops)
  WHERE vector_field = 'profile';

CREATE INDEX IF NOT EXISTS idx_node_vectors_content_hnsw
  ON node_vectors USING hnsw (embedding vector_cosine_ops)
  WHERE vector_field = 'content';

CREATE TABLE IF NOT EXISTS embedding_cache (
  cache_key    TEXT PRIMARY KEY,
  embedding    vector(1536) NOT NULL,
  provider     TEXT NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  expires_at   TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_embedding_cache_expires
  ON embedding_cache (expires_at)
  WHERE expires_at IS NOT NULL;
