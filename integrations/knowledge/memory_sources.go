package knowledge

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/znasllc-io/memql/component/memql"
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
	})
}
