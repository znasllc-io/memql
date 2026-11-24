package knowledge

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/memql"
)

func knowledgeSource(t *testing.T) memql.MemorySource {
	t.Helper()
	for _, s := range memql.MemorySources() {
		if s.Name == "knowledgeChunk" {
			return s
		}
	}
	t.Fatal("no knowledgeChunk memory source")
	return memql.MemorySource{}
}

// storeSeedChunk stores `<!--seed:{json}-->\n\n## title\n\nbody` as the row's
// text but embeds only the body. The rebuild must embed the body too.
func TestTheSeedChunkRecipeEmbedsTheBody(t *testing.T) {
	s := knowledgeSource(t)
	require.Equal(t, "v1:knowledge:documentChunk", s.Concept)

	const body = "Paris is the capital of France.\n\nIt sits on the Seine --> yes."
	for _, c := range []struct {
		name, title string
		terms       []string
	}{
		{"plain", "Capitals", []string{"france"}},
		{"a key term containing the header terminator", "Capitals", []string{"a-->b"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Build the stored text with the writer's own function.
			stored := seedChunkRowText(seedMetadata("seed", "A", "v1", "fact", c.title, c.terms), c.title, body)
			got, ok := s.Rows(map[string]any{"text": stored, "validationStatus": "validated"})
			require.True(t, ok)
			require.Equal(t, body, got)
			require.Equal(t, body, KnowledgeChunkEmbedText(stored))
		})
	}
}

func TestOrdinaryAndRejectedChunks(t *testing.T) {
	s := knowledgeSource(t)
	got, ok := s.Rows(map[string]any{"text": "ordinary chunk text", "validationStatus": "validated"})
	require.True(t, ok)
	require.Equal(t, "ordinary chunk text", got)
	_, ok = s.Rows(map[string]any{"text": "x", "validationStatus": "rejected"})
	require.False(t, ok, "a rejected chunk is never indexed (embed_domain.go)")
	_, ok = s.Rows(map[string]any{"text": "  ", "validationStatus": "validated"})
	require.False(t, ok)
	// A header the body of an ordinary chunk merely resembles is not stripped.
	require.Equal(t, "<!--seed:oops", KnowledgeChunkEmbedText("<!--seed:oops"))
}

// A chunk's id hashes its text or names a seed's recipe version, so its rows
// are rewritten (review stamps, every boot's re-seed) without the text
// changing: the source is content-addressed, and a newer row version does not
// send the chunk to be embedded again (ruling R7).
func TestKnowledgeChunksAreContentAddressed(t *testing.T) {
	require.True(t, knowledgeSource(t).ContentAddressed)
}
