package knowledge

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/core/num"
)

// The knowledge chunk's memory-search source (the rebuild recipe a model switch
// walks; component/memql/memory_sources.go). Every writer embeds the chunk's
// row text (ingest, embedChunk, embedDomainItems, the capability and release
// seeds), except storeSeedChunk, which stores a metadata header above the
// body but embeds the body alone. KnowledgeChunkEmbedText is that one rule, and
// storeSeedChunk calls it, so the writer and the rebuild cannot disagree.

const seedHeaderPrefix = "<!--seed:"

// KnowledgeChunkEmbedText returns the text a chunk row is embedded under.
//
// A seed chunk's row text is `<!--seed:{json}-->\n\n## title\n\nbody`; the
// title and metadata are retrieval metadata, not content the agent reads, so
// only the body is embedded. The JSON is decoded as one value (a key term may
// itself contain "-->") and the header must then match exactly what
// seedChunkRowText writes, with the title taken from the metadata. Anything
// that does not match is an ordinary chunk and is returned unchanged.
func KnowledgeChunkEmbedText(rowText string) string {
	if !strings.HasPrefix(rowText, seedHeaderPrefix) {
		return rowText
	}
	rest := rowText[len(seedHeaderPrefix):]
	dec := json.NewDecoder(bytes.NewReader([]byte(rest)))
	var meta map[string]any
	if dec.Decode(&meta) != nil {
		return rowText
	}
	title, _ := meta["chunkTitle"].(string)
	after := rest[dec.InputOffset():]
	prefix := "-->\n\n## " + title + "\n\n"
	if !strings.HasPrefix(after, prefix) {
		return rowText
	}
	return after[len(prefix):]
}

func init() {
	memql.RegisterMemorySource(memql.MemorySource{
		Name: "knowledgeChunk", Concept: conceptKnowledgeDocumentChunk, VectorField: "content",
		Rows: func(p map[string]any) (string, bool) {
			// embed_domain.go refuses a rejected chunk ("refusing to index").
			// Superseded chunks are still rebuilt: the writers embedded them
			// when written and nothing removes a superseded chunk's vector.
			if p["validationStatus"] == "rejected" {
				return "", false
			}
			text, _ := p["text"].(string)
			text = KnowledgeChunkEmbedText(text)
			return text, strings.TrimSpace(text) != ""
		},
		ContentAddressed: knowledgeChunkHashesItsText,
	})
}

// knowledgeChunkHashesItsText reports whether a chunk row's id is chunkIdFor of
// the row's own domain, sourceRef, seq and text: the ids ingest, the seed
// corpora and the canonical and release seeds write, under which the text never
// changes, though the row is written again (a review stamp, a boot's re-seed).
// Memory search then takes such a row as current whenever its vector is
// present (ruling R7).
//
// RECOMPUTED FROM THE ROW, not inferred from its source or provenance, so any
// other way of keying a chunk is versioned by construction, and new text under
// its id is embedded again. storeSeedChunk keys the LLM-seeded domains and the
// cross-domain bridges by seedChunkId -- the recipe version, the domain and the
// index, not the text -- and the seed workflow writes every chunk each time it
// runs, so a re-run at the same recipe version writes new text under the same
// ids. The Trainer's writeKnowledgeChunk takes whatever id its caller chose. A
// row missing a field its id hashes is versioned too: the cost of a wrong
// "versioned" is one more embedding, of a wrong "content-addressed" a vector
// that no longer says what its row says.
func knowledgeChunkHashesItsText(rowId string, p map[string]any) bool {
	domainId, okDomain := p["domainId"].(string)
	sourceRef, okRef := p["sourceRef"].(string)
	text, okText := p["text"].(string)
	seq, okSeq := p["seq"].(float64)
	if !okDomain || !okRef || !okText || !okSeq {
		return false
	}
	whole, ok := num.WholeInt64(seq)
	if !ok {
		return false
	}
	return rowId == id.BuildNodeId(conceptKnowledgeDocumentChunk, chunkIdFor(domainId, sourceRef, num.ClampInt64(whole), text))
}
