package knowledge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
)

// canonicalSeedChunk is derived only from the compiled catalog/pack registry,
// using the same chunker as ingestion. Neither a source label nor a caller's
// claimed chunk ID is proof that arbitrary content was reviewed.
type canonicalSeedChunk struct {
	ID, Domain, SourceRef, Text string
	Seq                         int
}

func canonicalSeedChunks() map[string]canonicalSeedChunk {
	out := map[string]canonicalSeedChunk{}
	add := func(domain string, entries []SeedCorpusEntry) {
		for _, entry := range entries {
			for seq, text := range Chunk(entry.Text, defaultChunkSize, defaultOverlap) {
				key := chunkIdFor(domain, entry.SourceRef, seq, text)
				out[key] = canonicalSeedChunk{key, domain, entry.SourceRef, text, seq}
			}
		}
	}
	add("computer-use", computerUseSeedCorpus)
	add("workbench", workbenchSeedCorpus)
	add("recent-chat", recentChatSeedCorpus)
	add("work-guidance", workGuidanceCorpus)
	for _, reg := range RegisteredSeedDomains() {
		add(reg.Domain.ID, reg.Corpus)
	}
	return out
}

type seedChunkSnapshot struct {
	Payload map[string]any
	At      time.Time
}

// staged-data: MUST-NOT-GATE -- publication repair must observe the actual
// latest review decision, even while a concept is staged. This read never
// sends content to an agent; normal retrieval still applies the staged gate.
func (i *Integration) seedChunkSnapshot(ctx context.Context, key string) (*seedChunkSnapshot, error) {
	if i.db() == nil {
		return nil, fmt.Errorf("knowledge seed: database not configured")
	}
	var raw []byte
	row := &seedChunkSnapshot{}
	err := i.db().QueryRowContext(ctx, `SELECT payload, "createdAt" FROM "MemoryNodes"
 WHERE concept='v1:knowledge:documentChunk' AND id=$1 ORDER BY "createdAt" DESC LIMIT 1`,
		id.BuildNodeId(conceptKnowledgeDocumentChunk, key)).Scan(&raw, &row.At)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(raw, &row.Payload); err != nil {
		return nil, err
	}
	return row, nil
}

func (seed canonicalSeedChunk) matches(row *seedChunkSnapshot) bool {
	p := row.Payload
	return p["domainId"] == seed.Domain && p["sourceRef"] == seed.SourceRef &&
		p["text"] == seed.Text && p["source"] == "appStructure" && p["seq"] == float64(seed.Seq)
}

func (i *Integration) reconcileCanonicalSeed(ctx context.Context, seed canonicalSeedChunk) (bool, error) {
	row, err := i.seedChunkSnapshot(ctx, seed.ID)
	if err != nil || row == nil {
		return false, err
	}
	if !seed.matches(row) {
		return false, fmt.Errorf("knowledge seed: stored chunk does not match registered content")
	}
	value, hasStatus := row.Payload["validationStatus"]
	status, _ := value.(string)
	// A recorded rejection, supersession or unknown state is never repaired
	// into approval. The only legacy state eligible here is no decision yet.
	if row.Payload["superseded"] == true || (hasStatus && status != "unvalidated") {
		return true, nil
	}
	ctx = memql.ContextWithRowVersionFence(ctx, conceptKnowledgeDocumentChunk, seed.ID, row.At)
	_, err = i.engine.Execute(ctx, fmt.Sprintf(`mutation setChunkValidationStatus(chunkId: %s, status: "validated")`, quoteString(seed.ID)))
	return true, err
}

// ReconcileCanonicalSeeds repairs existing exact shipped/registered chunks.
// It creates no domains/chunks, makes no model/embedding calls, and does not
// infer trust from source labels. Called by bounded startup maintenance.
func (i *Integration) ReconcileCanonicalSeeds(ctx context.Context) error {
	if i.engine == nil || i.db() == nil {
		return fmt.Errorf("knowledge seed: engine/database not configured")
	}
	trusted := auth.ContextWithInternalOrigin(auth.ContextWithSystemActor(ctx, "knowledge-seed"))
	tables, err := memql.EmbeddingVectorTables(trusted, i.db())
	if err != nil {
		return err
	}
	var failures []error
	for _, seed := range canonicalSeedChunks() {
		if err := trusted.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		present, err := i.reconcileCanonicalSeed(trusted, seed)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if !present {
			continue
		}
		// Older ingest versions stored vectors under a bare ID. Copy only an
		// exact registered chunk's vector, keeping an existing canonical vector.
		// Review state is still checked independently on every retrieval.
		canonical := id.BuildNodeId(conceptKnowledgeDocumentChunk, seed.ID)
		for _, table := range tables {
			_, err = i.db().ExecContext(trusted, `INSERT INTO `+table+` (id,concept,vector_field,embedding,created_at,updated_at)
    SELECT $1,concept,vector_field,embedding,created_at,updated_at FROM `+table+`
    WHERE id=$2 AND concept=$3 AND vector_field='content'
    ON CONFLICT (id,vector_field) DO NOTHING`, canonical, seed.ID, conceptKnowledgeDocumentChunk)
			if err != nil {
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
}

// writeCanonicalSeed is reachable only from the server-selected corpus path.
// A generic ingest call, even one copying its source label, never calls it.
func (i *Integration) writeCanonicalSeed(ctx context.Context, key, query string) error {
	seed, ok := canonicalSeedChunks()[key]
	if !ok {
		return fmt.Errorf("knowledge seed: chunk is outside registered content")
	}
	row, err := i.seedChunkSnapshot(ctx, key)
	if err != nil {
		return err
	}
	if row != nil && !seed.matches(row) {
		return fmt.Errorf("knowledge seed: stored chunk does not match registered content")
	}
	if row == nil {
		if _, err = i.engine.Execute(memql.ContextWithRowVersionFence(ctx, conceptKnowledgeDocumentChunk, key, time.Time{}), query); err != nil {
			return err
		}
	}
	_, err = i.reconcileCanonicalSeed(ctx, seed)
	return err
}
