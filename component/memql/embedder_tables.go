package memql

import (
	"fmt"
)

// One vector table per model and width (epic memql#5137, D6;
// EmbeddingVectorTable names it, EnsureEmbeddingVectorTable creates it).
//
// ===========================================================================
// WHY NOT ONE TABLE WITH AN UNTYPED COLUMN
// ===========================================================================
// pgvector's `vector` type can be declared without dimensions, and a column
// declared that way accepts vectors of any width. It cannot be INDEXED: hnsw
// and ivfflat both require a fixed dimension, so an untyped column is a table
// that writes fine and searches by sequential scan forever. That failure looks
// exactly like the feature working, right up until the corpus is large enough
// for it not to.
//
// ===========================================================================
// WHY NOT RE-EMBED IN PLACE
// ===========================================================================
// Because a half-written vector column is not a degraded search space, it is a
// corrupt one: half the rows answer in the old model's geometry and half in the
// new one's, and cosine distance between them is a number with no meaning. It
// does not error, it ranks. So a switch fills the new model's own table and
// flips the binding automatically only when every item is in it, or when an
// owner chooses Switch anyway over the items that are not (the memory index
// job, memory_index_job.go) -- until then every read follows the old binding
// and the old table, and a switch interrupted half way leaves a cluster that
// still works.

// createVectorTableSQL builds the DDL for one width.
//
// The two partial HNSW indexes mirror the original `node_vectors` migration:
// `profile` and `content` are the two vector fields the engine writes, and
// indexing them separately keeps each index over one field's rows rather than
// over a mixed set where most entries can never match.
func createVectorTableSQL(dims int) []string {
	table := VectorTableFor(dims)
	return []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
  id           TEXT NOT NULL,
  concept      TEXT NOT NULL,
  vector_field TEXT NOT NULL,
  embedding    vector(%d) NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (id, vector_field)
)`, table, dims),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_%s_profile_hnsw
  ON %s USING hnsw (embedding vector_cosine_ops)
  WHERE vector_field = 'profile'`, table, table),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_%s_content_hnsw
  ON %s USING hnsw (embedding vector_cosine_ops)
  WHERE vector_field = 'content'`, table, table),
	}
}
