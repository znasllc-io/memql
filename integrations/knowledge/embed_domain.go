// embed_domain.go
//
// The real chunk-embedding path (#645 -- lazy embedding for semantic
// retrieval of chunks). Two DSL-callable capabilities live here:
//
//	integration.knowledge.embedChunk       -- embed ONE chunk by id.
//	integration.knowledge.embedDomainItems -- embed every unembedded
//	                                          validated chunk for a
//	                                          domain (optionally scoped
//	                                          to one Document) and drive
//	                                          the parent Document's
//	                                          embeddingStatus none ->
//	                                          partial -> complete.
//
// Both materialize embeddings into the SAME node_vectors table the
// seed / ingest / training paths use (vector_field='content', keyed by
// the chunk id), so retrieval through the generic similarTo() operator
// (integrations/similarity) picks them up with no per-source special
// casing. The embed loop is idempotent: a chunk that already has a
// vector is skipped (cheap point-lookup), so re-running embedDomainItems
// after adding chunks only embeds the new ones.
//
// embedChunk is the Trainer Agent's tool-loop sentinel (it writes a
// chunk via writeKnowledgeChunk, then calls embedChunk to make it
// retrievable). embedDomainItems is the bulk path the planner's
// EmbedDomainItemsDispatcher drives when a domain is seeded or a user
// uploads a file -- it warms the whole domain's index in one pass.
package knowledge

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// embedChunkHandler resolves one chunk's text, embeds it, and writes the
// vector to node_vectors keyed by the chunk id. Idempotent: a chunk that
// already carries a 'content' vector is acknowledged without re-embedding
// (the Trainer can call embedChunk redundantly without burning tokens).
// Rejected chunks are refused so the Trainer's tool loop can't push
// soft-deleted content into the retrievable index.
func (i *Integration) embedChunkHandler(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	chunkId, _ := args["chunkId"].(string)
	chunkId = strings.TrimSpace(chunkId)
	if chunkId == "" {
		return nil, fmt.Errorf("knowledge.embedChunk: chunkId is required")
	}
	providerName, _ := args["provider"].(string)
	if providerName == "" {
		providerName = defaultProvider
	}

	if i.db() == nil {
		return nil, fmt.Errorf("knowledge.embedChunk: database not configured")
	}
	if i.embeddingProvider == nil {
		return nil, fmt.Errorf("knowledge.embedChunk: embedding provider not configured")
	}

	chunk, err := i.loadChunk(ctx, chunkId)
	if err != nil {
		return nil, fmt.Errorf("knowledge.embedChunk: load chunk %q: %w", chunkId, err)
	}
	if chunk == nil {
		return nil, fmt.Errorf("knowledge.embedChunk: chunk %q not found", chunkId)
	}
	if chunk.validationStatus == "rejected" {
		return nil, fmt.Errorf("knowledge.embedChunk: chunk %q is rejected; refusing to index", chunkId)
	}
	if strings.TrimSpace(chunk.text) == "" {
		return nil, fmt.Errorf("knowledge.embedChunk: chunk %q has empty text", chunkId)
	}

	// Already indexed -- skip the embed call and acknowledge.
	if has, herr := i.hasVector(ctx, chunkId); herr == nil && has {
		return embedChunkResult(chunkId, false, true), nil
	}

	provider, err := i.embeddingProvider(providerName)
	if err != nil {
		return nil, fmt.Errorf("knowledge.embedChunk: resolve provider %q: %w", providerName, err)
	}
	vec, err := provider.Embed(ctx, chunk.text)
	if err != nil {
		return nil, fmt.Errorf("knowledge.embedChunk: embed chunk %q: %w", chunkId, err)
	}
	if err := i.storeVector(ctx, chunkId, "v1:knowledge:documentChunk", vec); err != nil {
		return nil, fmt.Errorf("knowledge.embedChunk: store vector for %q: %w", chunkId, err)
	}

	if i.Logger != nil {
		i.Logger.Info("knowledge.embedChunk: embedded",
			"chunkId", chunkId, "documentId", chunk.documentId,
			"provider", providerName, "dimensions", len(vec))
	}
	return embedChunkResult(chunkId, true, false), nil
}

// embedDomainItemsHandler embeds every retrievable chunk attached to a
// domain that doesn't already have a vector, then recomputes the parent
// Document(s)' embeddingStatus. This is the bulk warm-up the
// EmbedDomainItemsDispatcher runs after a domain is seeded or a user
// uploads a file. Optional documentId scopes the run to a single
// Document's chunks (so a per-upload Plan only touches its own items).
//
// Drives document.embeddingStatus none -> partial -> complete: after the
// embed loop, every Document whose chunks were (partly) embedded gets its
// embeddingStatus + embeddedItemCount recomputed from the live vector
// coverage of its chunks. Seeded chunks have no parent Document, so they
// embed but drive no Document rollup -- exactly the intended split.
func (i *Integration) embedDomainItemsHandler(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	handlerStart := time.Now()

	domainId, _ := args["domainId"].(string)
	domainId = strings.TrimSpace(domainId)
	if domainId == "" {
		return nil, fmt.Errorf("knowledge.embedDomainItems: domainId is required")
	}
	documentId, _ := args["documentId"].(string)
	documentId = strings.TrimSpace(documentId)
	providerName, _ := args["provider"].(string)
	if providerName == "" {
		providerName = defaultProvider
	}

	if i.db() == nil {
		return nil, fmt.Errorf("knowledge.embedDomainItems: database not configured")
	}
	if i.embeddingProvider == nil {
		return nil, fmt.Errorf("knowledge.embedDomainItems: embedding provider not configured")
	}

	chunks, err := i.queryChunksForDomain(ctx, domainId, documentId)
	if err != nil {
		return nil, fmt.Errorf("knowledge.embedDomainItems: list chunks for domain %q: %w", domainId, err)
	}

	provider, err := i.embeddingProvider(providerName)
	if err != nil {
		return nil, fmt.Errorf("knowledge.embedDomainItems: resolve provider %q: %w", providerName, err)
	}

	embedded := 0
	already := 0
	failed := 0
	// touchedDocuments collects every parent Document id we saw so its
	// embeddingStatus rollup gets recomputed once at the end.
	touchedDocuments := make(map[string]struct{})

	for _, c := range chunks {
		if c.documentId != "" {
			touchedDocuments[c.documentId] = struct{}{}
		}
		has, herr := i.hasVector(ctx, c.id)
		if herr != nil {
			i.Logger.Warn("knowledge.embedDomainItems: hasVector check failed",
				"chunkId", c.id, "err", herr)
			failed++
			continue
		}
		if has {
			already++
			continue
		}
		vec, eerr := provider.Embed(ctx, c.text)
		if eerr != nil {
			i.Logger.Warn("knowledge.embedDomainItems: embed chunk failed",
				"chunkId", c.id, "err", eerr)
			failed++
			continue
		}
		if serr := i.storeVector(ctx, c.id, "v1:knowledge:documentChunk", vec); serr != nil {
			i.Logger.Warn("knowledge.embedDomainItems: store vector failed",
				"chunkId", c.id, "err", serr)
			failed++
			continue
		}
		embedded++
	}

	// Drive the Document embeddingStatus rollup. When the caller scoped
	// the run to a single documentId, only that Document is rolled up;
	// otherwise every parent Document whose chunks we touched is.
	docsUpdated := 0
	if documentId != "" {
		touchedDocuments[documentId] = struct{}{}
	}
	for docId := range touchedDocuments {
		if err := i.rollupDocumentEmbeddingStatus(ctx, docId); err != nil {
			i.Logger.Warn("knowledge.embedDomainItems: document rollup failed",
				"documentId", docId, "err", err)
			continue
		}
		docsUpdated++
	}

	i.Logger.Info("knowledge.embedDomainItems: completed",
		"elapsed_ms", time.Since(handlerStart).Milliseconds(),
		"domainId", domainId,
		"documentId", documentId,
		"chunks", len(chunks),
		"embedded", embedded,
		"already", already,
		"failed", failed,
		"documentsRolledUp", docsUpdated,
		"provider", providerName,
	)

	body, _ := json.Marshal(map[string]any{
		"domainId":          domainId,
		"documentId":        documentId,
		"total":             len(chunks),
		"embedded":          embedded,
		"already":           already,
		"failed":            failed,
		"documentsRolledUp": docsUpdated,
	})
	return []memorynodes.MemoryNode{{
		ID:        "knowledge-embed-domain-result",
		Concept:   "integration:knowledge:embedDomainItems",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   body,
	}}, nil
}

// --- helpers --------------------------------------------------------------

type chunkRow struct {
	id               string
	text             string
	documentId       string
	validationStatus string
}

// loadChunk reads the latest version of one documentChunk by id.
func (i *Integration) loadChunk(ctx context.Context, chunkId string) (*chunkRow, error) {
	row := i.db().QueryRowContext(
		ctx,
		`SELECT chunk.id,
		        chunk.payload->>'text',
		        COALESCE(chunk.payload->>'documentId', ''),
		        COALESCE(chunk.payload->>'validationStatus', '')
		 FROM "MemoryNodes" chunk
		 WHERE chunk.concept = 'v1:knowledge:documentChunk'
		   AND chunk.id = $1
		 ORDER BY chunk."createdAt" DESC
		 LIMIT 1`,
		chunkId,
	)
	var c chunkRow
	err := row.Scan(&c.id, &c.text, &c.documentId, &c.validationStatus)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// queryChunksForDomain returns the live, retrievable chunks attached to a
// domain (optionally scoped to one Document). Mirrors the training
// integration's domain-content query: chunks are pulled DIRECTLY by
// domainId (seeded chunks have no parent Document, so a Document-mediated
// query would miss them), and a chunk is excluded only when its parent
// Document is validationStatus='rejected' or its own validationStatus is
// 'rejected'. DISTINCT ON keeps the latest version of each chunk id.
func (i *Integration) queryChunksForDomain(
	ctx context.Context,
	domainId string,
	documentId string,
) ([]chunkRow, error) {
	// chunk.payload.domainId is the BARE domain slug by contract (see
	// docs/public/concepts/identifiers.md); documentId is a canonical
	// v1:knowledge:document id (relationship fields canonicalize at
	// insert). Parameters: $1 domainId, $2 documentId ("" = no scope).
	rows, err := i.db().QueryContext(
		ctx,
		`SELECT DISTINCT ON (chunk.id)
		        chunk.id,
		        chunk.payload->>'text' AS text,
		        COALESCE(chunk.payload->>'documentId', '') AS document_id,
		        COALESCE(chunk.payload->>'validationStatus', '') AS validation_status
		 FROM "MemoryNodes" chunk
		 WHERE chunk.concept = 'v1:knowledge:documentChunk'
		   AND chunk.payload->>'domainId' = $1
		   AND ($2 = '' OR chunk.payload->>'documentId' = $2)
		   AND COALESCE(chunk.payload->>'validationStatus', '') <> 'rejected'
		   AND COALESCE((chunk.payload->>'superseded')::boolean, false) = false
		   AND (
		     chunk.payload->>'documentId' IS NULL
		     OR chunk.payload->>'documentId' = ''
		     OR NOT EXISTS (
		       SELECT 1 FROM "MemoryNodes" doc
		       WHERE doc.concept = 'v1:knowledge:document'
		         AND doc.id = chunk.payload->>'documentId'
		         AND doc.payload->>'validationStatus' = 'rejected'
		     )
		   )
		 ORDER BY chunk.id, chunk."createdAt" DESC`,
		domainId, documentId,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []chunkRow
	for rows.Next() {
		var c chunkRow
		if err := rows.Scan(&c.id, &c.text, &c.documentId, &c.validationStatus); err != nil {
			return nil, err
		}
		if strings.TrimSpace(c.text) == "" {
			continue
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// hasVector reports whether a node already has a 'content' embedding.
// Cheap point-lookup against the (id, vector_field) PK.
func (i *Integration) hasVector(ctx context.Context, nodeId string) (bool, error) {
	row := i.db().QueryRowContext(
		ctx,
		`SELECT 1 FROM node_vectors WHERE id = $1 AND vector_field = 'content' LIMIT 1`,
		nodeId,
	)
	var marker int
	err := row.Scan(&marker)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// rollupDocumentEmbeddingStatus recomputes one Document's embeddingStatus
// + embeddedItemCount from the live vector coverage of its chunks, then
// writes the result via mutationUpdateDocumentEmbeddingStatus.
//
//	0 chunks embedded                 -> none
//	some but not all chunks embedded  -> partial
//	every chunk embedded              -> complete
//
// Counts run against node_vectors (the source of truth for "is this
// retrievable") rather than a stored flag, so the rollup self-heals if a
// vector was added or removed out of band.
func (i *Integration) rollupDocumentEmbeddingStatus(ctx context.Context, documentId string) error {
	if documentId == "" {
		return nil
	}
	var total, embedded int
	err := i.db().QueryRowContext(
		ctx,
		`SELECT
		   COUNT(*) AS total,
		   COUNT(nv.id) AS embedded
		 FROM (
		   SELECT DISTINCT ON (chunk.id) chunk.id
		   FROM "MemoryNodes" chunk
		   WHERE chunk.concept = 'v1:knowledge:documentChunk'
		     AND chunk.payload->>'documentId' = $1
		     AND COALESCE(chunk.payload->>'validationStatus', '') <> 'rejected'
		   ORDER BY chunk.id, chunk."createdAt" DESC
		 ) live_chunk
		 LEFT JOIN node_vectors nv
		   ON nv.id = live_chunk.id AND nv.vector_field = 'content'`,
		documentId,
	).Scan(&total, &embedded)
	if err != nil {
		return fmt.Errorf("count document chunks: %w", err)
	}

	status := "none"
	switch {
	case total == 0 || embedded == 0:
		status = "none"
	case embedded >= total:
		status = "complete"
	default:
		status = "partial"
	}

	if i.engine == nil {
		return fmt.Errorf("engine not configured for document rollup")
	}
	q := fmt.Sprintf(
		`mutation mutationUpdateDocumentEmbeddingStatus(documentId: %s, embeddingStatus: %s, embeddedItemCount: %d)`,
		quoteString(documentId),
		quoteString(status),
		embedded,
	)
	if _, err := i.engine.Execute(ctx, q); err != nil {
		return fmt.Errorf("update embeddingStatus: %w", err)
	}
	if i.Logger != nil {
		i.Logger.Info("knowledge.embedDomainItems: document rollup",
			"documentId", documentId, "embeddingStatus", status,
			"embedded", embedded, "total", total)
	}
	return nil
}

// embedChunkResult builds the single-node return payload for embedChunk.
func embedChunkResult(chunkId string, embedded, alreadyEmbedded bool) []memorynodes.MemoryNode {
	body, _ := json.Marshal(map[string]any{
		"chunkId":         chunkId,
		"embedded":        embedded,
		"alreadyEmbedded": alreadyEmbedded,
	})
	return []memorynodes.MemoryNode{{
		ID:        "knowledge-embedchunk-result",
		Concept:   "integration:knowledge:embedChunk",
		Type:      memorynodes.NodeTypeObject,
		CreatedAt: time.Now().UTC(),
		Payload:   body,
	}}
}
