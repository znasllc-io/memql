package library

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/integrations/knowledge"
	"github.com/znasllc-io/memql/integrations/knowledge/seeddata"
)

type trainingVector struct{ calls int }

func (p *trainingVector) Embed(_ context.Context, text string) ([]float32, error) {
	p.calls++
	v := make([]float32, 1024)
	v[0] = 1
	if strings.HasPrefix(text, "unrelated") {
		v[0] = 0
		v[1] = 1
	}
	return v, nil
}
func (p *trainingVector) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	var out [][]float32
	for _, s := range texts {
		v, _ := p.Embed(ctx, s)
		out = append(out, v)
	}
	return out, nil
}
func (p *trainingVector) Dimensions() int { return 1024 }

func TestOwnedTrainingReadsReviewedTaughtSourcesAcrossReplicas(t *testing.T) {
	f := newRevisionDB(t)
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { db.Close() })
	provider := &trainingVector{}
	binding := memql.EmbedderBinding{ProviderRef: f.owner, Dimensions: 1024}
	old, had := memql.ActiveEmbedderBinding()
	memql.SetActiveEmbedderBinding(binding)
	t.Cleanup(func() {
		if had {
			memql.SetActiveEmbedderBinding(old)
		} else {
			memql.ClearActiveEmbedderBinding()
		}
	})
	table, err := memql.EnsureEmbeddingVectorTable(f.ctx, db.DB, f.owner, 1024)
	require.NoError(t, err)
	t.Cleanup(func() {
		db.ExecContext(context.Background(), "DROP TABLE "+table)
		db.ExecContext(context.Background(), `DELETE FROM embedding_cache WHERE provider=?`, f.owner)
	})
	for n, lib := range []*Integration{f.first, f.second} {
		engine := []*memql.MemQLEngine{f.engine, f.other}[n]
		require.NoError(t, lib.setTrainingReader(memql.PluginContext{Logger: engine.Logger, BunDB: func() *bun.DB { return db }, ConceptDataIsStaged: engine.ConceptDataIsStaged, AdmitSourceRow: memql.AdmitSourceRow, ResolveEmbeddingProvider: func(context.Context, string) (memql.EmbeddingAIProvider, error) { return provider, nil }}))
		k := knowledge.New(engine.Logger)
		k.SetEngine(engine)
		k.SetDBGetter(func() *sql.DB { return db.DB })
		k.SetStagedConceptPredicate(engine.ConceptDataIsStaged)
		k.SetEmbeddingProvider(func(context.Context, string) (memql.EmbeddingAIProvider, error) { return provider, nil })
		require.NoError(t, engine.RegisterIntegration(k))
	}
	person := auth.ContextWithInternalOrigin(revisionActor(f.owner, auth.RoleOwner))
	runID, goalID := f.owner+"run", f.owner+"goal"
	f.query(f.engine, person, "mutation", "createWorkGoal", map[string]any{"goalId": goalID, "statement": "Verify a change with evidence", "origin": "user"})
	f.query(f.engine, person, "mutation", "createWorkRun", map[string]any{"runId": runID, "goalId": goalID, "status": "running", "mode": "live", "automationName": "fixture", "templateFingerprint": "fixture", "startedAt": time.Now().UTC().Format(time.RFC3339Nano)})
	ctx := common.ContextWithRun(person, common.RunContext{RunId: runID, GoalId: goalID, OwnerUserId: f.owner, StepKey: "section", Mode: common.RunModeLive})
	search := func(lib *Integration) []map[string]any {
		rows, err := lib.handleOwnedTrainingLessons(ctx, map[string]any{"text": "Verify a change", "limit": 1, "minimumSimilarity": 0.6, "maxTokens": 1200}, 0)
		require.NoError(t, err)
		var out []map[string]any
		for _, row := range rows {
			var p map[string]any
			require.NoError(t, json.Unmarshal(row.Payload, &p))
			out = append(out, p)
		}
		return out
	}
	require.Empty(t, search(f.second))
	require.Zero(t, provider.calls, "first-use empty corpus does not call an embedder")
	teach := func(actor context.Context, suffix, text string) (fileID, chunkID, artifactID string) {
		fileID = f.owner + suffix
		f.query(f.engine, actor, "mutation", "createLibraryFile", map[string]any{"fileId": fileID, "name": suffix + ".txt", "mimeType": "text/plain", "size": len(text), "blobUrl": "fixture://owned-source", "source": "uploaded", "format": "text"})
		f.query(f.engine, actor, "mutation", "createArtifact", map[string]any{"sourceConceptRef": "v1:library:file:" + fileID, "ownerUserId": actingUserId(actor), "lens": "artifact", "kind": "file", "source": "uploaded", "title": "Reviewed practice " + suffix, "format": "text", "mimeType": "text/plain"})
		artifacts := f.query(f.engine, actor, "query", "libraryArtifactForFile", map[string]any{"fileId": fileID})
		require.Len(t, artifacts, 1)
		artifactID = stringField(artifacts[0], "id")
		f.query(f.engine, actor, "mutation", "createLibraryFileChunk", map[string]any{"chunkId": fileID + "source", "fileId": fileID, "artifactId": artifactID, "seq": 0, "text": text})
		f.query(f.engine, actor, "mutation", "setLibraryFileStatus", map[string]any{"fileId": fileID, "status": "ready"})
		domain := f.owner + suffix + "-domain" // deliberately absent from any domain/skill catalog
		_, err := f.first.handleTrainFile(actor, map[string]any{"fileId": fileID, "domainId": domain}, 0)
		require.NoError(t, err)
		chunks := f.query(f.engine, actor, "query", "documentChunksForDomain", map[string]any{"domainId": domain})
		require.Len(t, chunks, 1)
		chunkID = stringField(chunks[0], "id")
		return
	}
	file, chunk, artifact := teach(person, "lesson", "Run required checks and retain their evidence before claiming completion.")
	require.Empty(t, search(f.second), "pending lesson cannot reach another replica")
	f.query(f.engine, person, "mutation", "setChunkValidationStatus", map[string]any{"chunkId": chunk, "status": "validated"})
	got := search(f.second)
	require.Len(t, got, 1)
	require.Equal(t, memql.BareShortId(chunk), got[0]["chunkId"])
	require.Equal(t, memql.BareShortId(artifact), got[0]["artifactId"])
	require.Contains(t, got[0]["text"], "required checks")
	receiptID := stringField(got[0], "selectionReceiptId")
	require.NotEmpty(t, receiptID)
	receipts := f.query(f.engine, memql.ContextWithFreshRead(person), "query", "workObservationsForOwnerRun", map[string]any{"runId": runID})
	var selectedReceipt map[string]any
	for _, row := range receipts {
		if memql.BareShortId(stringField(row, "id")) == receiptID {
			selectedReceipt = row
		}
	}
	require.NotNil(t, selectedReceipt, "another replica reads the same owned selection receipt")
	require.Contains(t, fmt.Sprint(selectedReceipt), "textDigest")
	require.Contains(t, fmt.Sprint(selectedReceipt), "required checks")
	foreignActor := auth.ContextWithInternalOrigin(revisionActor(f.owner+"receipt-outsider", auth.RoleOwner))
	require.Empty(t, f.query(f.other, foreignActor, "query", "workObservationsForOwnerRun", map[string]any{"runId": runID}))
	// A reviewed passage too large for its allocation is withheld whole and the
	// bounded decision records the reason, without treating it as delivered.
	tiny, err := f.second.handleOwnedTrainingLessons(ctx, map[string]any{"text": "Verify a change", "limit": 1, "minimumSimilarity": 0.6, "maxTokens": 1}, 0)
	require.NoError(t, err)
	require.Empty(t, tiny)
	receipts = f.query(f.engine, memql.ContextWithFreshRead(person), "query", "workObservationsForOwnerRun", map[string]any{"runId": runID})
	require.Contains(t, fmt.Sprint(receipts), "context-byte-limit")

	originalAdmit := f.second.training.admit
	f.second.training.admit = func(ctx context.Context, row memorynodes.MemoryNode) bool {
		return row.Concept != "v1:library:artifact" && originalAdmit(ctx, row)
	}
	require.Empty(t, search(f.second), "raw source-row admission happens before repacking")
	f.second.training.admit = originalAdmit
	originalStaged := f.second.training.staged
	f.second.training.staged = func(concept string) bool { return concept == "v1:library:fileChunk" || originalStaged(concept) }
	require.Empty(t, search(f.second), "staged extracted source cannot supply a passage")
	f.second.training.staged = originalStaged
	changedReview := false
	f.second.training.admit = func(ctx context.Context, row memorynodes.MemoryNode) bool {
		if !changedReview && row.Concept == "v1:knowledge:documentChunk" {
			changedReview = true
			f.query(f.engine, person, "mutation", "setChunkValidationStatus", map[string]any{"chunkId": chunk, "status": "rejected"})
		}
		return originalAdmit(ctx, row)
	}
	require.Empty(t, search(f.second), "a review changed after candidate selection is revalidated before output")
	f.second.training.admit = originalAdmit
	require.True(t, changedReview)
	f.query(f.engine, person, "mutation", "setChunkValidationStatus", map[string]any{"chunkId": chunk, "status": "validated"})
	other := auth.ContextWithInternalOrigin(revisionActor(f.owner+"other", auth.RoleOwner))
	_, foreign, _ := teach(other, "foreign", "A nearer unrelated owner's lesson.")
	f.query(f.engine, other, "mutation", "setChunkValidationStatus", map[string]any{"chunkId": foreign, "status": "validated"})
	_, unrelated, _ := teach(person, "different", "unrelated botany only")
	f.query(f.engine, person, "mutation", "setChunkValidationStatus", map[string]any{"chunkId": unrelated, "status": "validated"})
	require.Equal(t, memql.BareShortId(chunk), search(f.second)[0]["chunkId"], "foreign and below-threshold lessons cannot consume top-K")
	f.query(f.engine, person, "mutation", "setChunkValidationStatus", map[string]any{"chunkId": chunk, "status": "rejected"})
	require.Empty(t, search(f.second), "another replica's cached embedding must not resurrect rejection")
	f.query(f.engine, person, "mutation", "setChunkValidationStatus", map[string]any{"chunkId": chunk, "status": "validated"})
	f.query(f.engine, person, "mutation", "markChunkSuperseded", map[string]any{"chunkId": chunk, "reason": "Replaced by reviewed evidence", "supersededAt": time.Now().UTC().Format(time.RFC3339Nano)})
	require.Empty(t, search(f.second))
	// A direct fixture head models a still-approved record whose owned source
	// changed after teaching. It must not inherit authority from a source label.
	_, err = db.ExecContext(person, `INSERT INTO "MemoryNodes" (id,"createdAt","createdBy",schema,payload,metadata,type,concept,provenance) SELECT id,"createdAt"+interval '1 second',"createdBy",schema,payload||'{"superseded":false}'::jsonb,metadata,type,concept,provenance FROM "MemoryNodes" WHERE id=? ORDER BY "createdAt" DESC LIMIT 1`, "v1:knowledge:documentChunk:"+memql.BareShortId(chunk))
	require.NoError(t, err)
	f.query(f.engine, person, "mutation", "createLibraryFileChunk", map[string]any{"chunkId": file + "source", "fileId": file, "artifactId": artifact, "seq": 0, "text": "The source has changed."})
	require.Empty(t, search(f.second), "source text must still prove the reviewed passage")
	// Runtime identities are read from the actual forwarded context and rows.
	forged := common.ContextWithRun(other, common.RunContext{RunId: runID, GoalId: goalID, OwnerUserId: f.owner})
	_, err = f.second.handleWorkTrainingContext(forged, nil, 0)
	require.Error(t, err)
	_, err = f.second.handleWorkTrainingContext(auth.ContextWithClientOrigin(ctx), nil, 0)
	require.ErrorContains(t, err, "current owned live work run")
	require.NotEmpty(t, fmt.Sprint(got[0]["chunkVersionAt"]))
	// Release-owned lessons use a distinct exact checked-in provenance branch;
	// neither a trusted-looking label nor a stale revision is enough.
	var seed seeddata.ChunkRecord
	for _, s := range seeddata.Release() {
		if s.Lesson {
			seed = s
			break
		}
	}
	require.NotEmpty(t, seed.ID)
	f.query(f.engine, person, "mutation", "createDocumentChunk", map[string]any{"chunkId": seed.ID, "domainId": seed.Domain, "sourceRef": seed.SourceRef, "source": "appStructure", "text": seed.Text, "seq": seed.Seq})
	seedID := "v1:knowledge:documentChunk:" + seed.ID
	t.Cleanup(func() { db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE id=?`, seedID) })
	vector, _ := provider.Embed(person, "registered release fixture")
	encoded, _ := json.Marshal(vector)
	_, err = db.ExecContext(person, `INSERT INTO `+table+`(id,concept,vector_field,embedding) VALUES(?,'v1:knowledge:documentChunk','content',?::vector)`, seedID, string(encoded))
	require.NoError(t, err)
	require.Empty(t, search(f.second), "a pending exact release chunk is still withheld")
	f.query(f.engine, person, "mutation", "setChunkValidationStatus", map[string]any{"chunkId": seed.ID, "status": "validated"})
	shared := search(f.second)
	require.Len(t, shared, 1)
	require.Equal(t, "appStructure", shared[0]["source"])
	require.Equal(t, seed.Revision, shared[0]["seedRevision"])
	require.NotContains(t, shared[0], "artifactId")
	// A previously taught byte-identical private copy keeps its own provenance,
	// but cannot consume a second bounded passage slot beside the release copy.
	_, duplicate, _ := teach(person, "same-release", seed.Text)
	f.query(f.engine, person, "mutation", "setChunkValidationStatus", map[string]any{"chunkId": duplicate, "status": "validated"})
	entries, err := f.second.handleOwnedTrainingLessons(ctx, map[string]any{"text": "Verify a change", "limit": 3, "minimumSimilarity": 0.6, "maxTokens": 1200}, 0)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	var selected map[string]any
	require.NoError(t, json.Unmarshal(entries[0].Payload, &selected))
	require.Equal(t, "fileUpload", selected["source"])
	f.query(f.engine, person, "mutation", "setChunkValidationStatus", map[string]any{"chunkId": duplicate, "status": "rejected"})
	require.Equal(t, "appStructure", search(f.second)[0]["source"], "rejecting a private copy does not rewrite the independent shared source")
	f.query(f.engine, person, "mutation", "setChunkValidationStatus", map[string]any{"chunkId": seed.ID, "status": "rejected"})
	require.Empty(t, search(f.second))
	f.query(f.engine, person, "mutation", "setChunkValidationStatus", map[string]any{"chunkId": seed.ID, "status": "validated"})
	f.query(f.engine, person, "mutation", "createDocumentChunk", map[string]any{"chunkId": seed.ID, "domainId": seed.Domain, "sourceRef": seed.SourceRef, "source": "appStructure", "text": "A forged payload under a registered ID.", "seq": seed.Seq})
	f.query(f.engine, person, "mutation", "setChunkValidationStatus", map[string]any{"chunkId": seed.ID, "status": "validated"})
	require.Empty(t, search(f.second), "registered identity alone cannot approve different content")

}
