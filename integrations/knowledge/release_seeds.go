package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/knowledge/seeddata"
)

func (i *Integration) SetGateDB(fn func() *sql.DB) { i.gateDB = fn }

// staged-data: MUST-NOT-GATE -- publication observes review decisions, not agent
// content. A previous rejection withholds automatic successor approval. A
// superseded predecessor is not a domain-wide disable: an accepted replacement
// still counts. Exact current decisions are handled independently by the writer.
func (i *Integration) releasePredecessorDecision(ctx context.Context, seed canonicalSeedChunk) (bool, error) {
	var rejected, disabled, accepted bool
	err := i.db().QueryRowContext(ctx, `WITH latest AS (
 SELECT DISTINCT ON (id) id,payload FROM "MemoryNodes"
 WHERE concept='v1:knowledge:documentChunk' ORDER BY id,"createdAt" DESC
 ) SELECT COALESCE(bool_or(payload->>'validationStatus'='rejected'),false),
 COALESCE(bool_or(COALESCE(payload->>'superseded','false')='true'),false),
 COALESCE(bool_or(payload->>'validationStatus'='validated' AND COALESCE(payload->>'superseded','false')='false' AND payload->'seq'=to_jsonb($4::int)),false)
 FROM latest WHERE payload->>'source'='appStructure' AND payload->>'domainId'=$1 AND payload->>'sourceRef'=$2 AND id<>$3`, seed.Domain, seed.SourceRef, id.BuildNodeId(conceptKnowledgeDocumentChunk, seed.ID), seed.Seq).Scan(&rejected, &disabled, &accepted)
	return rejected || (disabled && !accepted), err
}

// releaseSeedGate holds independent of a cancelled request until all dispatched
// writes settle. Each bounded commit verifies this exact retained hold. The
// same source key serializes publications and paid indexing across replicas.
func (i *Integration) releaseSeedGate(ctx context.Context, seed canonicalSeedChunk, fn func(context.Context, func(context.Context) error) error) error {
	if i.gateDB == nil || i.gateDB() == nil {
		return fmt.Errorf("release knowledge requires shared write coordination")
	}
	holdctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
	defer cancel()
	hold, err := memql.AcquireGateHold(holdctx, i.gateDB(), 6*time.Second, "knowledge-release:"+seed.Domain+":"+seed.SourceRef)
	if err != nil {
		return err
	}
	defer hold.Release()
	if err = ctx.Err(); err != nil {
		return err
	}
	operation, stop := context.WithTimeout(ctx, 2*time.Minute)
	defer stop()
	return fn(operation, hold.Verify)
}

func (i *Integration) publishReleaseChunk(ctx context.Context, seed canonicalSeedChunk) error {
	return i.releaseSeedGate(ctx, seed, func(ctx context.Context, verify func(context.Context) error) error {
		commit, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if err := verify(commit); err != nil {
			return err
		}
		query := fmt.Sprintf(`mutation createDocumentChunk(chunkId:%s,domainId:%s,text:%s,source:"appStructure",sourceRef:%s,seq:%d,tokenCount:%d)`, quoteString(seed.ID), quoteString(seed.Domain), quoteString(seed.Text), quoteString(seed.SourceRef), seed.Seq, approxTokens(seed.Text))
		return i.writeCanonicalSeed(commit, seed.ID, query)
	})
}

// ReconcileReleaseSeeds is a bounded server lifecycle operation. Its authority
// covers only the hash-checked explicit release manifest, never caller text,
// arbitrary future catalogs, packs or owner-uploaded lessons. Publication needs
// no owner or embedder; missing indexing dependencies remain an honest error.
func (i *Integration) ReconcileReleaseSeeds(ctx context.Context, index bool) error {
	if auth.OriginFromContext(ctx) != auth.OriginInternal {
		return fmt.Errorf("release knowledge requires internal origin")
	}
	if run, ok := common.RunFromContext(ctx); ok && run.IsRun() {
		actor, _ := auth.AccessFromContext(ctx)
		if run.OwnerUserId != "" || run.GoalId != "" || run.RerunRequestId != "" || run.Mode != common.RunModeLive || actor == nil || actor.UserId != "system:automation:reconcileReleaseKnowledge" {
			return fmt.Errorf("release knowledge is a server lifecycle operation")
		}
	}
	if i.engine == nil || i.db() == nil {
		return fmt.Errorf("release knowledge requires engine and database")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	ctx = auth.ContextWithSystemActor(ctx, "knowledge-release")
	chunks := map[string]canonicalSeedChunk{}
	keys := []string{}
	for _, s := range seeddata.Release() {
		chunks[s.ID] = canonicalSeedChunk{s.ID, s.Domain, s.SourceRef, s.Text, s.Seq}
		keys = append(keys, s.ID)
	}
	var failures []error
	attempted := 0
	_, err := workflowhost.Run(ctx, "knowledgeReconcileReleaseSeeds", map[string]any{"chunkIds": keys, "index": index}, workflowhost.Options{Logger: i.Logger, Operations: map[string]workflowhost.Operation{
		"knowledgePublishReleaseChunk": func(ctx context.Context, args map[string]any) (any, error) {
			seed, ok := chunks[stringArg(args, "chunkId")]
			if !ok {
				return nil, fmt.Errorf("chunk outside release manifest")
			}
			err := i.publishReleaseChunk(ctx, seed)
			if err != nil {
				failures = append(failures, err)
			}
			return nil, err
		},
		"knowledgeIndexReleaseChunk": func(ctx context.Context, args map[string]any) (any, error) {
			seed, ok := chunks[stringArg(args, "chunkId")]
			if !ok {
				return nil, fmt.Errorf("chunk outside release manifest")
			}
			if attempted >= 8 {
				return nil, nil
			}
			err := i.releaseSeedGate(ctx, seed, func(ctx context.Context, verify func(context.Context) error) error {
				row, err := i.seedChunkSnapshot(ctx, seed.ID)
				if err != nil {
					return err
				}
				if row == nil || !seed.matches(row) || row.Payload["validationStatus"] != "validated" || row.Payload["superseded"] == true {
					return nil
				}
				binding, err := memql.CurrentEmbedderBinding(ctx)
				if err != nil {
					return err
				}
				present, err := i.hasVector(ctx, id.BuildNodeId(conceptKnowledgeDocumentChunk, seed.ID), binding.ProviderRef, binding.Dimensions)
				if err != nil || present {
					return err
				}
				attempted++
				if i.embeddingProvider == nil {
					return fmt.Errorf("release knowledge indexing awaits an embedder")
				}
				provider, err := i.embeddingProvider(ctx, binding.ProviderRef)
				if err != nil {
					return err
				}
				if provider == nil || binding.Dimensions <= 0 {
					return fmt.Errorf("release knowledge indexing awaits a bound embedder")
				}
				if width := provider.Dimensions(); width > 0 && width != binding.Dimensions {
					return fmt.Errorf("release knowledge embedder width changed")
				}
				vector, err := provider.Embed(ctx, seed.Text)
				if err != nil {
					return err
				}
				if len(vector) != binding.Dimensions {
					return fmt.Errorf("release knowledge embedding width mismatch")
				}
				if err = memql.ValidateEmbeddingVector(ctx, binding.ProviderRef, binding.Dimensions, vector); err != nil {
					return err
				}
				commit, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
				defer cancel()
				if err = verify(commit); err != nil {
					return err
				}
				// A peer review during embedding can withhold retrieval; indexing never
				// changes the review or republishes a now-disabled lesson.
				return i.storeVector(commit, binding.ProviderRef, seed.ID, conceptKnowledgeDocumentChunk, vector)
			})
			if err != nil {
				failures = append(failures, err)
			}
			return nil, err
		},
	}})
	return errors.Join(append(failures, err)...)
}

func (i *Integration) reconcileReleaseHandler(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	return nil, i.ReconcileReleaseSeeds(ctx, true)
}
