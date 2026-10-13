package knowledge

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
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
// no owner or embedder; unavailable indexing is deferred, not reported ready.
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
	_, err := i.reconcileRelease(auth.ContextWithSystemActor(ctx, "knowledge-release"), true, index, nil)
	return err
}

type releaseIndexResult struct {
	Indexed  int    `json:"indexed"`
	Deferred string `json:"deferred,omitempty"`
}

// Both callers select the same checked-in manifest. Only publication borrows
// lifecycle authority. Inference retains the original actor, accepted forward
// assertion, run budget/observability context and cancellation all the way to
// the serving worker. An embedder binding does not grant machine access.
func (i *Integration) reconcileRelease(ctx context.Context, publish, index bool, admit func(context.Context) error) (releaseIndexResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	chunks := map[string]canonicalSeedChunk{}
	keys := []string{}
	for _, s := range seeddata.Release() {
		chunks[s.ID] = canonicalSeedChunk{s.ID, s.Domain, s.SourceRef, s.Text, s.Seq}
		keys = append(keys, s.ID)
	}
	var failures []error
	var result releaseIndexResult
	attempted := 0
	indexFailed := false
	_, err := workflowhost.Run(ctx, "knowledgeReconcileReleaseSeeds", map[string]any{"chunkIds": keys, "publish": publish, "index": index}, workflowhost.Options{Logger: i.Logger, Operations: map[string]workflowhost.Operation{
		"knowledgePublishReleaseChunk": func(ctx context.Context, args map[string]any) (any, error) {
			seed, ok := chunks[stringArg(args, "chunkId")]
			if !ok || !publish {
				return nil, fmt.Errorf("chunk outside release publication scope")
			}
			err := i.publishReleaseChunk(auth.ContextWithSystemActor(ctx, "knowledge-release"), seed)
			if err != nil {
				failures = append(failures, err)
			}
			return nil, err
		},
		"knowledgeIndexReleaseChunk": func(ctx context.Context, args map[string]any) (any, error) {
			seed, ok := chunks[stringArg(args, "chunkId")]
			if !ok || !index {
				return nil, fmt.Errorf("chunk outside release indexing scope")
			}
			if attempted >= 8 || indexFailed || result.Deferred != "" {
				return nil, nil
			}
			err := i.releaseSeedGate(ctx, seed, func(ctx context.Context, verify func(context.Context) error) error {
				if admit != nil {
					if err := admit(ctx); err != nil {
						return err
					}
				}
				row, err := i.seedChunkSnapshot(ctx, seed.ID)
				if err != nil {
					return err
				}
				if row == nil || !seed.matches(row) || row.Payload["validationStatus"] != "validated" || row.Payload["superseded"] == true {
					return nil
				}
				binding, err := memql.CurrentEmbedderBinding(ctx)
				if errors.Is(err, memql.ErrNoEmbedderBound) {
					result.Deferred = "embedder-not-bound"
					return nil
				}
				if err != nil {
					return err
				}
				// Ownerless scheduled work cannot create a person or a forwarding
				// assertion. Defer fleet inference until an authorized owned turn.
				if strings.HasPrefix(binding.ProviderRef, memql.FleetReferencePrefix) {
					if _, ok := auth.ForwardedAuthorityFromContext(ctx); !ok {
						result.Deferred = "fleet-authority-unavailable"
						return nil
					}
				}
				present, err := i.hasVector(ctx, id.BuildNodeId(conceptKnowledgeDocumentChunk, seed.ID), binding.ProviderRef, binding.Dimensions)
				if err != nil || present {
					return err
				}
				attempted++
				if i.embeddingProvider == nil {
					return fmt.Errorf("release knowledge embedder resolver is not wired")
				}
				provider, err := i.embeddingProvider(ctx, binding.ProviderRef)
				if err != nil {
					// Defer only unavailable selection, never admission ceilings,
					// caller cancellation, storage failures or a failed model call.
					if ctx.Err() != nil {
						return context.Cause(ctx)
					}
					var refusal interface{ RefusalCode() string }
					if errors.Is(err, memql.ErrFleetUnavailable) || (errors.As(err, &refusal) && refusal.RefusalCode() == work.RefusalEveryDoorShut) {
						result.Deferred = "embedder-unavailable"
						return nil
					}
					return err
				}
				if provider == nil || binding.Dimensions <= 0 {
					return fmt.Errorf("release knowledge indexing requires a bound embedder")
				}
				if width := provider.Dimensions(); width > 0 && width != binding.Dimensions {
					return fmt.Errorf("release knowledge embedder width changed")
				}
				vector, err := provider.Embed(ctx, seed.Text)
				if err != nil {
					return err
				}
				if err = ctx.Err(); err != nil {
					return context.Cause(ctx)
				}
				if len(vector) != binding.Dimensions {
					return fmt.Errorf("release knowledge embedding width mismatch")
				}
				if err = memql.ValidateEmbeddingVector(ctx, binding.ProviderRef, binding.Dimensions, vector); err != nil {
					return err
				}
				if admit != nil {
					if err = admit(ctx); err != nil {
						return err
					}
				}
				commit, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
				defer cancel()
				if err = verify(commit); err != nil {
					return err
				}
				// A peer review during embedding can withhold retrieval; indexing
				// never changes review or republishes a disabled lesson.
				if err = i.storeVector(commit, binding.ProviderRef, seed.ID, conceptKnowledgeDocumentChunk, vector); err != nil {
					return err
				}
				result.Indexed++
				return nil
			})
			if err != nil {
				indexFailed = true
				failures = append(failures, err)
			}
			return nil, err
		},
	}})
	return result, errors.Join(append(failures, err)...)
}

// This is a zero-input native capability, not a model-callable tool. Caller
// text, provider choices and publication decisions cannot enter this scope.
func (i *Integration) indexReleaseForWorkHandler(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if err := i.releaseWorkScope(ctx); err != nil {
		return nil, err
	}
	result, err := i.reconcileRelease(ctx, false, true, i.releaseWorkScope)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(result)
	return []memorynodes.MemoryNode{{ID: "release-knowledge-index", Payload: payload}}, err
}

func (i *Integration) releaseWorkScope(ctx context.Context) error {
	run, ok := common.RunFromContext(ctx)
	actor, hasActor := auth.AccessFromContext(ctx)
	if auth.OriginFromContext(ctx) != auth.OriginInternal || !ok || !run.IsRun() || run.GoalId == "" || run.StepKey == "" || run.Mode == common.RunModeReplay || !hasActor || actor == nil || auth.ActsForNoPerson(ctx, actor.UserId) || memql.BareShortId(actor.UserId) != memql.BareShortId(run.OwnerUserId) {
		return fmt.Errorf("release indexing requires the current owned live work run")
	}
	if i.engine == nil || i.db() == nil {
		return fmt.Errorf("release indexing requires engine and database")
	}
	read := func(name, key, value string) (map[string]any, error) {
		call, err := parser.RenderCall(name, map[string]any{key: value})
		if err != nil {
			return nil, err
		}
		response, err := i.engine.Execute(memql.ContextWithFreshRead(ctx), "query "+call)
		if err != nil {
			return nil, err
		}
		rows := memql.MaterializeRows(response)
		if len(rows) != 1 {
			return nil, fmt.Errorf("release indexing work scope is unavailable")
		}
		return rows[0], nil
	}
	row, err := read("workRunForOwner", "runId", run.RunId)
	if err != nil {
		return err
	}
	rerun, _ := row["rerun"].(map[string]any)
	if memql.BareShortId(stringArg(row, "ownerUserId")) != memql.BareShortId(actor.UserId) || memql.BareShortId(stringArg(row, "goalId")) != memql.BareShortId(run.GoalId) || row["status"] != "running" || row["cancelRequested"] == true || stringArg(rerun, "requestId") != run.RerunRequestId {
		return fmt.Errorf("release indexing work scope changed")
	}
	goal, err := read("workGoalForOwner", "goalId", run.GoalId)
	if err != nil {
		return err
	}
	if memql.BareShortId(stringArg(goal, "ownerUserId")) != memql.BareShortId(actor.UserId) || (goal["status"] != "open" && goal["status"] != "active") {
		return fmt.Errorf("release indexing goal is not active and owned")
	}
	return nil
}

func (i *Integration) reconcileReleaseHandler(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	return nil, i.ReconcileReleaseSeeds(ctx, true)
}
