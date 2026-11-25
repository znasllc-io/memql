package knowledge

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
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

// Only a chunk whose id hashes its own text is presence-only for memory search
// (ruling R7). Ingest, the seed corpora and the canonical and release seeds
// key their chunks by chunkIdFor, so the text under such an id never changes,
// and a review stamp or a boot's re-seed sends nothing to be embedded again.
// Every other id is versioned, so new text under it is embedded again: an
// LLM-seeded or bridge chunk (seedChunkId names the recipe version, the domain
// and the index, and a re-seed at the same version writes new text under it)
// and a Trainer chunk (its caller chooses the id). The answer is recomputed
// from the row, so text that no longer hashes to its id is versioned too.
func TestOnlyAChunkWhoseIdHashesItsTextIsPresenceOnly(t *testing.T) {
	s := knowledgeSource(t)
	// A chunk row's payload as createDocumentChunk stores it, decoded as the
	// walk decodes it.
	stored := func(domainId, sourceRef string, seq int, text string) map[string]any {
		return map[string]any{"domainId": domainId, "sourceRef": sourceRef, "seq": float64(seq), "text": text,
			"source": "appStructure", "validationStatus": "validated"}
	}
	row := func(shortId string) string { return id.BuildNodeId(conceptKnowledgeDocumentChunk, shortId) }
	const domain, ref, text = "computer-use", "manual-1", "Click the button to continue."

	ingested := row(chunkIdFor(domain, ref, 2, text))
	require.True(t, s.ContentAddressed(ingested, stored(domain, ref, 2, text)), "an ingested or corpus-seeded chunk")
	require.False(t, s.ContentAddressed(ingested, stored(domain, ref, 2, "Other text under the same id.")),
		"text that no longer hashes to its id")
	noSeq := stored(domain, ref, 2, text)
	delete(noSeq, "seq")
	require.False(t, s.ContentAddressed(ingested, noSeq), "a row missing a field its id hashes")

	body := seedChunkRowText(seedMetadata("llm-generated", "A", "v1", "fact", "Capitals", nil), "Capitals", "Paris is the capital of France.")
	require.False(t, s.ContentAddressed(row(seedChunkId("v1", domain, 0)), stored(domain, "seed:v1:llm-generated:0", 0, body)),
		"an LLM-seeded chunk is keyed by its recipe, not its text")
	require.False(t, s.ContentAddressed(row("trainer-chosen-id"), stored(domain, "url:https://example.com/a", 0, text)),
		"a Trainer chunk's id is whatever its caller chose")
}
