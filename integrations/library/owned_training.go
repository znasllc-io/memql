package library

// This is separate from generic similarTo: knowledge chunks have no ownership
// field. An uploaded lesson qualifies through its current owned Library source.
// The DSL recipe chooses relevance and size; this reader enforces provenance.
import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/embedding"
	"github.com/znasllc-io/memql/integrations/knowledge"
	"github.com/znasllc-io/memql/integrations/knowledge/seeddata"
)

type trainingReader struct {
	db     func() *sql.DB
	staged func(string) bool
	admit  func(context.Context, memorynodes.MemoryNode) bool
	embed  *embedding.Integration
}

func (i *Integration) setTrainingReader(p memql.PluginContext) error {
	if p.BunDB == nil || p.ConceptDataIsStaged == nil || p.AdmitSourceRow == nil || p.ResolveEmbeddingProvider == nil {
		return fmt.Errorf("library: owned Training reader requires database, staging, row admission and embedding wiring")
	}
	r := &trainingReader{db: func() *sql.DB {
		if db := p.BunDB(); db != nil {
			return db.DB
		}
		return nil
	}, staged: p.ConceptDataIsStaged, admit: p.AdmitSourceRow, embed: embedding.New(p.Logger)}
	r.embed.SetDBGetter(r.db)
	r.embed.SetEmbeddingProvider(p.ResolveEmbeddingProvider)
	i.training = r
	return nil
}

func (i *Integration) ownedTrainingScope(ctx context.Context) (common.RunContext, map[string]any, error) {
	run, ok := common.RunFromContext(ctx)
	actor := memql.BareShortId(actingUserId(ctx))
	if auth.OriginFromContext(ctx) != auth.OriginInternal || !ok || !run.IsRun() || run.GoalId == "" || actor == "" || actor != memql.BareShortId(run.OwnerUserId) || run.Mode == common.RunModeReplay {
		return run, nil, fmt.Errorf("Training context requires the current owned live work run")
	}
	read := func(name, key, value string) (map[string]any, error) {
		call, err := parser.RenderCall(name, map[string]any{key: value})
		if err != nil {
			return nil, err
		}
		result, err := i.engine.Execute(memql.ContextWithFreshRead(ctx), "query "+call)
		if err != nil {
			return nil, err
		}
		rows := memql.MaterializeRows(result)
		if len(rows) != 1 {
			return nil, fmt.Errorf("Training work scope is unavailable")
		}
		return rows[0], nil
	}
	row, err := read("workRunForOwner", "runId", run.RunId)
	if err != nil {
		return run, nil, err
	}
	if memql.BareShortId(stringField(row, "ownerUserId")) != actor || memql.BareShortId(stringField(row, "goalId")) != memql.BareShortId(run.GoalId) || row["cancelRequested"] == true {
		return run, nil, fmt.Errorf("Training work scope changed")
	}
	goal, err := read("workGoalForOwner", "goalId", run.GoalId)
	if err != nil {
		return run, nil, err
	}
	if memql.BareShortId(stringField(goal, "ownerUserId")) != actor {
		return run, nil, fmt.Errorf("Training goal owner changed")
	}
	return run, goal, nil
}

func (i *Integration) handleWorkTrainingContext(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	_, goal, err := i.ownedTrainingScope(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(map[string]any{"goal": stringField(goal, "statement")})
	return []memorynodes.MemoryNode{{ID: "work-training-context", Concept: resultConcept, Payload: raw}}, err
}

type trainingCandidate struct {
	node               memorynodes.MemoryNode
	artifact, file     memorynodes.MemoryNode
	payload            map[string]any
	artifactID, fileID string
	score              float64
}

// Publication and current source ownership precede top-K, so other owners'
// nearer hits cannot crowd this caller out. Every table folds latest first.
// staged-data: GATE -- handleOwnedTrainingLessons withholds all four source concepts before these reads.
const ownedTrainingSourceSQL = `WITH chunks AS (
 SELECT DISTINCT ON(id) id,payload,"createdAt" FROM "MemoryNodes"
 WHERE concept='v1:knowledge:documentChunk' ORDER BY id,"createdAt" DESC
), artifacts AS (
 SELECT DISTINCT ON(id) id,payload FROM "MemoryNodes"
 WHERE concept='v1:library:artifact' ORDER BY id,"createdAt" DESC
), files AS (
 SELECT DISTINCT ON(id) id,payload FROM "MemoryNodes"
 WHERE concept='v1:library:file' ORDER BY id,"createdAt" DESC
)
SELECT k.id,k.payload,k."createdAt",a.id AS artifact_id,f.id AS file_id,a.payload AS artifact_payload,f.payload AS file_payload
FROM chunks k
JOIN artifacts a ON k.payload->>'sourceRef' IN ('artifact:'||a.id,'artifact:'||substring(a.id from 21))
JOIN files f ON a.payload->>'sourceConceptRef'=f.id
WHERE k.payload->>'source'='fileUpload' AND k.payload->>'validationStatus'='validated'
 AND COALESCE(k.payload->>'superseded','false')='false'
 AND a.payload->>'ownerUserId' IN ($1,'v1:identity:user:'||$1)
 AND f.payload->>'ownerUserId' IN ($1,'v1:identity:user:'||$1)
 AND COALESCE(a.payload->>'archived','false')='false'
 AND f.payload->>'status'='ready'
 AND COALESCE(f.payload->>'archived','false')='false'
 AND jsonb_typeof(f.payload->'trainedIntoDomainIds')='array'
 AND (f.payload->'trainedIntoDomainIds') ? (k.payload->>'domainId')`

var trainingSourceSQL = ownedTrainingSourceSQL + `
UNION ALL SELECT k.id,k.payload,k."createdAt",'' AS artifact_id,'' AS file_id,'{}'::jsonb AS artifact_payload,'{}'::jsonb AS file_payload
FROM chunks k WHERE k.payload->>'validationStatus'='validated'
 AND COALESCE(k.payload->>'superseded','false')='false' AND ` + seeddata.CurrentSQL("k", true)

// staged-data: GATE -- handleOwnedTrainingLessons checks all source concepts before this ranked read.
var trainingCandidateSQL = `SELECT s.id,s.payload,s."createdAt",s.artifact_id,s.file_id,s.artifact_payload,s.file_payload,1-(v.embedding <=> $2::vector)
FROM (` + trainingSourceSQL + `) s
JOIN %s v ON v.id=s.id AND v.concept='v1:knowledge:documentChunk' AND v.vector_field='content'
WHERE 1-(v.embedding <=> $2::vector) >= $3
ORDER BY v.embedding <=> $2::vector,(s.artifact_id<>'') DESC,s.id LIMIT $4`

func (i *Integration) handleOwnedTrainingLessons(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	run, _, err := i.ownedTrainingScope(ctx)
	if err != nil {
		return nil, err
	}
	r := i.training
	if r == nil || r.db() == nil {
		return nil, fmt.Errorf("owned Training retrieval is not wired")
	}
	for _, concept := range []string{"v1:knowledge:documentChunk", "v1:library:artifact", "v1:library:file", "v1:library:fileChunk"} {
		// staged-data: GATE -- all source concepts are checked before raw reads.
		if r.staged(concept) {
			return nil, nil
		}
	}
	limit, valid := intArg(args["limit"])
	tokens, validTokens := intArg(args["maxTokens"])
	threshold := floatField(args, "minimumSimilarity")
	text := strings.TrimSpace(asString(args["text"]))
	if !valid || !validTokens || limit < 1 || limit > 10 || tokens < 1 || tokens > 4000 || math.IsNaN(threshold) || threshold < 0 || threshold > 1 || text == "" || len(text) > 12000 {
		return nil, fmt.Errorf("owned Training retrieval requires bounded text, count, similarity and context limits")
	}
	// Empty first-use corpora require neither an embedding binding nor a model.
	var present bool
	if err := r.db().QueryRowContext(ctx, "SELECT EXISTS("+trainingSourceSQL+")", memql.BareShortId(run.OwnerUserId)).Scan(&present); err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}

	// Newly published release lessons can precede inference setup/indexing. They
	// must not make an otherwise empty optional reader a prerequisite for work.
	var ownedPresent bool
	if err := r.db().QueryRowContext(ctx, "SELECT EXISTS("+ownedTrainingSourceSQL+")", memql.BareShortId(run.OwnerUserId)).Scan(&ownedPresent); err != nil {
		return nil, err
	}
	if !ownedPresent {
		binding, err := memql.CurrentEmbedderBinding(ctx)
		if errors.Is(err, memql.ErrNoEmbedderBound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		table, err := memql.EmbeddingVectorTable(binding.ProviderRef, binding.Dimensions)
		if err != nil {
			return nil, err
		}
		var exists bool
		if err = r.db().QueryRowContext(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			return nil, nil
		}
		if err = r.db().QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM ("+trainingSourceSQL+") s JOIN "+table+" v ON v.id=s.id AND v.concept='v1:knowledge:documentChunk' AND v.vector_field='content')", memql.BareShortId(run.OwnerUserId)).Scan(&exists); err != nil {
			return nil, err
		}
		if !exists {
			return nil, nil
		}
	}
	provider, err := memql.ResolveEmbedderProvider(ctx)
	if err != nil {
		return nil, err
	}
	vector, err := r.embed.Embed(ctx, text, provider)
	if err != nil {
		return nil, fmt.Errorf("embed Training query: %w", err)
	}
	table, err := memql.EmbeddingVectorTable(provider, len(vector))
	if err != nil {
		return nil, err
	}
	vectorJSON, err := json.Marshal(vector)
	if err != nil {
		return nil, err
	}
	rows, err := r.db().QueryContext(ctx, fmt.Sprintf(trainingCandidateSQL, table), memql.BareShortId(run.OwnerUserId), string(vectorJSON), threshold, limit*4)
	if err != nil {
		return nil, err
	}
	var candidates []trainingCandidate
	for rows.Next() {
		c := trainingCandidate{node: memorynodes.MemoryNode{Concept: "v1:knowledge:documentChunk"}}
		if err = rows.Scan(&c.node.ID, &c.node.Payload, &c.node.CreatedAt, &c.artifactID, &c.fileID, &c.artifact.Payload, &c.file.Payload, &c.score); err != nil {
			break
		}
		c.artifact.ID, c.artifact.Concept = c.artifactID, "v1:library:artifact"
		c.file.ID, c.file.Concept = c.fileID, "v1:library:file"
		// Admit the actual latest source rows before materializing a passage.
		if !r.admit(ctx, c.node) || (c.artifactID != "" && (!r.admit(ctx, c.artifact) || !r.admit(ctx, c.file))) {
			continue
		}
		if err = json.Unmarshal(c.node.Payload, &c.payload); err != nil {
			break
		}
		candidates = append(candidates, c)
	}
	readErr := rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if readErr != nil {
		return nil, readErr
	}
	out := []memorynodes.MemoryNode{}
	seenText := map[string]bool{}
	remaining := tokens * 3 // WorkContextSize's conservative byte budget, including provenance.
	for _, c := range candidates {
		if len(out) == limit {
			break
		}
		payload, ok, err := i.revalidateTrainingCandidate(ctx, c)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		text, _ := payload["text"].(string)
		if seenText[text] {
			continue
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		if len(raw) > remaining {
			continue
		} // Never turn a reviewed passage into a truncated claim.
		remaining -= len(raw)
		seenText[text] = true
		out = append(out, memorynodes.MemoryNode{ID: c.node.ID, Concept: resultConcept, Type: memorynodes.NodeTypeObject, CreatedAt: time.Now().UTC(), Payload: raw})
	}
	if _, _, err := i.ownedTrainingScope(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// staged-data: GATE -- called only after handleOwnedTrainingLessons checks all source concepts.
func (i *Integration) revalidateTrainingCandidate(ctx context.Context, c trainingCandidate) (map[string]any, bool, error) {
	ctx = memql.ContextWithFreshRead(ctx)
	if c.artifactID == "" {
		return i.revalidateReleaseLesson(ctx, c)
	}
	read := func(name, key, value string) (map[string]any, error) {
		call, err := parser.RenderCall(name, map[string]any{key: memql.BareShortId(value)})
		if err != nil {
			return nil, err
		}
		result, err := i.engine.Execute(ctx, "query "+call)
		if err != nil {
			return nil, err
		}
		rows := memql.MaterializeRows(result)
		if len(rows) != 1 {
			return nil, nil
		}
		return rows[0], nil
	}
	a, err := read("libraryArtifactById", "artifactId", c.artifactID)
	if err != nil {
		return nil, false, err
	}
	f, err := read("libraryFileById", "fileId", c.fileID)
	if err != nil {
		return nil, false, err
	}
	actor := memql.BareShortId(actingUserId(ctx))
	if a == nil || f == nil || a["archived"] == true || f["archived"] == true || stringField(f, "status") != "ready" || memql.BareShortId(stringField(a, "ownerUserId")) != actor || memql.BareShortId(stringField(f, "ownerUserId")) != actor || stringField(a, "sourceConceptRef") != c.fileID || !slices.Contains(stringSliceField(f, "trainedIntoDomainIds"), stringField(c.payload, "domainId")) {
		return nil, false, nil
	}
	chunks, err := i.fileChunks(ctx, memql.BareShortId(c.fileID))
	if err != nil {
		return nil, false, err
	}
	// Use Teach's same bounded source read/rejoin/chunker. A source label alone
	// never establishes provenance; changed or unprovable content stays withheld.
	text := joinChunkTexts(chunks)
	if len(text) > 200000 || !utf8.ValidString(text) {
		return nil, false, nil
	}
	parts := knowledge.Chunk(text, analysisChunkSize, analysisChunkOverlap)
	seq, ok := intArg(c.payload["seq"])
	if !ok || seq < 0 || seq >= len(parts) || parts[seq] != stringField(c.payload, "text") {
		return nil, false, nil
	}
	expected := id.New().MustFromMap(map[string]any{"kind": "knowledge-chunk", "domainId": stringField(c.payload, "domainId"), "sourceRef": stringField(c.payload, "sourceRef"), "seq": seq, "text": parts[seq]})
	if string(expected) != memql.BareShortId(c.node.ID) {
		return nil, false, nil
	}
	var latest memorynodes.MemoryNode
	latest.ID = c.node.ID
	latest.Concept = c.node.Concept
	err = i.training.db().QueryRowContext(ctx, `SELECT payload,"createdAt" FROM "MemoryNodes" WHERE id=$1 AND concept='v1:knowledge:documentChunk' ORDER BY "createdAt" DESC LIMIT 1`, c.node.ID).Scan(&latest.Payload, &latest.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !latest.CreatedAt.Equal(c.node.CreatedAt) || !i.training.admit(ctx, latest) {
		return nil, false, nil
	}
	return map[string]any{"chunkId": memql.BareShortId(c.node.ID), "chunkVersionAt": c.node.CreatedAt.Format(time.RFC3339Nano), "domainId": c.payload["domainId"], "source": "fileUpload", "sourceRef": c.payload["sourceRef"], "artifactId": memql.BareShortId(c.artifactID), "fileId": memql.BareShortId(c.fileID), "citation": stringField(a, "title"), "similarity": c.score, "text": parts[seq]}, true, nil
}

// staged-data: GATE -- release lessons share the chunk gate but never borrow an
// upload's provenance. Only exact manifest bytes, current review and row
// admission can produce this distinct release-owned result.
func (i *Integration) revalidateReleaseLesson(ctx context.Context, c trainingCandidate) (map[string]any, bool, error) {
	seed, ok := seeddata.Find(c.node.ID)
	if !ok || !seed.Lesson || !seed.Matches(c.payload) {
		return nil, false, nil
	}
	var latest memorynodes.MemoryNode
	latest.ID = c.node.ID
	latest.Concept = c.node.Concept
	err := i.training.db().QueryRowContext(ctx, `SELECT payload,"createdAt" FROM "MemoryNodes" WHERE id=$1 AND concept='v1:knowledge:documentChunk' ORDER BY "createdAt" DESC LIMIT 1`, c.node.ID).Scan(&latest.Payload, &latest.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !latest.CreatedAt.Equal(c.node.CreatedAt) || !i.training.admit(ctx, latest) {
		return nil, false, nil
	}
	if c.payload["validationStatus"] != "validated" || c.payload["superseded"] == true {
		return nil, false, nil
	}
	return map[string]any{"chunkId": seed.ID, "chunkVersionAt": latest.CreatedAt.Format(time.RFC3339Nano), "domainId": seed.Domain, "source": "appStructure", "sourceRef": seed.SourceRef, "seedRevision": seed.Revision, "citation": "Shipped knowledge: " + seed.SourceRef, "similarity": c.score, "text": seed.Text}, true, nil
}
