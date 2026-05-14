package memql

import "context"

// EmbeddingSIProvider generates vector embeddings from text.
// Implementations: OpenAI text-embedding-3-small, text-embedding-3-large.
type EmbeddingSIProvider interface {
	// Embed returns a vector embedding for the given text.
	Embed(ctx context.Context, text string) ([]float32, error)

	// EmbedBatch embeds multiple texts in a single API call.
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)

	// Dimensions returns the vector dimensionality (e.g., 1536).
	Dimensions() int
}
