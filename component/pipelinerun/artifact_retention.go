package pipelinerun

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations/workflowhost"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

const artifactRetentionPageSize = 32

type ArtifactRetentionLibrary interface {
	pipelinesteps.LibraryArtifactLifecycle
	pipelinesteps.LibraryArtifactScopeRetirement
	pipelinesteps.LibraryArtifactPresentationRetirement
	pipelinesteps.LibraryLegacyLogAdoption
}

type ArtifactRetentionDependencies struct {
	Database func() *sql.DB
	Engine   Engine
	Library  ArtifactRetentionLibrary
}

type ArtifactRetentionResult struct {
	RunID     string `json:"runId"`
	WorkRunID string `json:"workRunId"`
	Inspected int    `json:"inspected"`
	Retained  int    `json:"retained"`
	Retired   int    `json:"retired"`
	Purged    int    `json:"purged"`
	Bytes     int64  `json:"bytes"`
	AfterID   string `json:"afterId"`
	HasMore   bool   `json:"hasMore"`
	DryRun    bool   `json:"dryRun"`
}

type artifactRetentionItem struct {
	ID                                string
	Scope                             pipelinesteps.RunFileReceiptScope
	Bytes                             int64
	Fenced, Retained, Retired, Purged bool
}

func retentionIntentID(value string) bool {
	b, err := hex.DecodeString(value)
	return err == nil && len(b) == 32 && hex.EncodeToString(b) == value
}

func (i *Integration) handleRetireArtifacts(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	out, err := i.RetireArtifacts(handlerContext(ctx), stringArg(args, "runId"), stringArg(args, "afterId"), boolArg(args, "dryRun"))
	if err != nil {
		return nil, err
	}
	return resultNode(retentionResultMap(out)), nil
}

func retentionResultMap(out ArtifactRetentionResult) map[string]any {
	return map[string]any{"runId": out.RunID, "workRunId": out.WorkRunID, "inspected": out.Inspected,
		"retained": out.Retained, "retired": out.Retired, "purged": out.Purged, "bytes": out.Bytes,
		"afterId": out.AfterID, "hasMore": out.HasMore, "dryRun": out.DryRun}
}

// RetireArtifacts binds one bounded page to a caller-owned terminal pipeline.
// The installed recipe owns ordering; each operation retains its own native
// owner/reference/producer fence. A retry can recover from another replica.
func (i *Integration) RetireArtifacts(ctx context.Context, runID, afterID string, dryRun bool) (ArtifactRetentionResult, error) {
	owner, err := personFrom(ctx)
	if err != nil {
		return ArtifactRetentionResult{}, err
	}
	owner = memql.BareShortId(owner)
	if runID == "" || len(runID) > 512 || (afterID != "" && !retentionIntentID(afterID)) {
		return ArtifactRetentionResult{}, errors.New("artifact cleanup requires a run and an exact intent cursor")
	}
	d := i.snapshot()
	if d.Store == nil || d.ArtifactRetention == nil || d.ArtifactRetention.Engine == nil ||
		d.ArtifactRetention.Library == nil || d.ArtifactRetention.Database == nil {
		return ArtifactRetentionResult{}, errors.New("native pipeline artifact retention is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	ctx = memql.ContextWithFreshRead(ctx)
	r, err := d.Store.RunForOwner(ctx, runID)
	if err != nil {
		return ArtifactRetentionResult{}, err
	}
	if r == nil || memql.BareShortId(r.OwnerUserID) != owner || r.Status != StatusCompleted || r.WorkRunID == "" {
		return ArtifactRetentionResult{}, errors.New("artifact cleanup requires the caller's completed pipeline run")
	}
	if err := retentionProducerStopped(ctx, d.ArtifactRetention.Engine, owner, *r); err != nil {
		return ArtifactRetentionResult{}, err
	}
	db := d.ArtifactRetention.Database()
	if db == nil {
		return ArtifactRetentionResult{}, errors.New("artifact retention journal is unavailable")
	}
	items, more, err := readArtifactRetentionPage(ctx, db, owner, r.WorkRunID, afterID)
	if err != nil {
		return ArtifactRetentionResult{}, err
	}
	out := ArtifactRetentionResult{RunID: r.ID, WorkRunID: r.WorkRunID, Inspected: len(items), AfterID: afterID, HasMore: more, DryRun: dryRun}
	if len(items) > 0 {
		out.AfterID = items[len(items)-1].ID
	}
	scope := &artifactRetentionScope{library: d.ArtifactRetention.Library, items: items, result: out}
	_, err = workflowhost.Run(auth.ContextWithInternalOrigin(ctx), "retirePipelineArtifacts", map[string]any{"dryRun": dryRun}, workflowhost.Options{
		Load: d.LoadWorkflow, Operations: scope.operations(), Logger: d.Logger,
	})
	if err != nil {
		return ArtifactRetentionResult{}, err
	}
	if !scope.observed {
		return ArtifactRetentionResult{}, errors.New("artifact cleanup recipe did not report native outcomes")
	}
	return scope.result, nil
}

func retentionProducerStopped(ctx context.Context, engine Engine, owner string, r Run) error {
	res, err := engine.Execute(ctx, "query workRunForOwner(runId: "+langparser.QuoteString(r.WorkRunID)+")")
	if err != nil {
		return err
	}
	rows := rowsOf(res)
	if len(rows) != 1 {
		return errors.New("artifact cleanup requires its authorized work journal")
	}
	w := rows[0]
	input, ok := w["input"].(map[string]any)
	if !ok || memql.BareShortId(fmt.Sprint(w["ownerUserId"])) != owner ||
		memql.BareShortId(fmt.Sprint(w["id"])) != r.WorkRunID || input["pipelineRunId"] != r.ID || input["pipelineId"] != r.PipelineID {
		return errors.New("artifact cleanup work journal differs from its pipeline")
	}
	switch w["status"] {
	case "succeeded", "failed", "cancelled":
		return nil
	}
	return errors.New("artifact cleanup refuses a producer that is still active")
}

func readArtifactRetentionPage(ctx context.Context, db *sql.DB, owner, work, after string) ([]*artifactRetentionItem, bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT intent_id,identity,size_bytes FROM pipeline_artifact_uploads
WHERE owner_user_id=$1 AND identity->>'WorkRunID'=$2 AND intent_id COLLATE "C">$3
 AND (state<>'retired' OR library_purged_at IS NULL)
ORDER BY intent_id COLLATE "C" LIMIT $4`, owner, work, after, artifactRetentionPageSize+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var items []*artifactRetentionItem
	for rows.Next() {
		item := &artifactRetentionItem{}
		var raw []byte
		if err := rows.Scan(&item.ID, &raw, &item.Bytes); err != nil {
			return nil, false, err
		}
		if err := json.Unmarshal(raw, &item.Scope); err != nil {
			return nil, false, err
		}
		if !retentionIntentID(item.ID) || item.Scope.OwnerUserID != owner || item.Scope.WorkRunID != work || item.Scope.StepKey == "" || item.Scope.Attempt < 1 || item.Bytes < 0 || item.Bytes > 2<<30 {
			return nil, false, errors.New("artifact journal contains an inconsistent retirement scope")
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(items) > artifactRetentionPageSize
	if more {
		items = items[:artifactRetentionPageSize]
	}
	return items, more, nil
}

type artifactRetentionScope struct {
	library  ArtifactRetentionLibrary
	items    []*artifactRetentionItem
	result   ArtifactRetentionResult
	observed bool
}

func (s *artifactRetentionScope) item(args map[string]any) (*artifactRetentionItem, error) {
	if len(args) != 1 {
		return nil, errors.New("artifact operation requires its bound intent")
	}
	key, ok := args["intentId"].(string)
	if ok {
		for _, item := range s.items {
			if item.ID == key {
				return item, nil
			}
		}
	}
	return nil, errors.New("artifact is outside this cleanup page")
}

func (s *artifactRetentionScope) operations() map[string]workflowhost.Operation {
	return map[string]workflowhost.Operation{
		"pipelineRetentionItems": func(context.Context, map[string]any) (any, error) {
			items := make([]any, 0, len(s.items))
			for _, item := range s.items {
				items = append(items, map[string]any{"intentId": item.ID, "bytes": item.Bytes})
			}
			return items, nil
		},
		"pipelineRetentionFence": func(ctx context.Context, args map[string]any) (any, error) {
			item, err := s.item(args)
			if err != nil {
				return nil, err
			}
			if s.result.DryRun {
				return nil, errors.New("preview cannot fence a producer")
			}
			if err := s.library.FenceRunFileScope(ctx, item.Scope); err != nil {
				return nil, err
			}
			item.Fenced = true
			return true, nil
		},
		"pipelineRetentionRetire": func(ctx context.Context, args map[string]any) (any, error) {
			item, err := s.item(args)
			if err != nil {
				return nil, err
			}
			if s.result.DryRun || !item.Fenced {
				return nil, errors.New("retirement requires its producer fence")
			}
			_, err = s.library.RetireRunFile(ctx, item.Scope, item.ID)
			if errors.Is(err, pipelinesteps.ErrArtifactRetained) {
				item.Retained = true
				return map[string]any{"retained": true}, nil
			}
			if err != nil {
				return nil, err
			}
			item.Retained = false
			item.Retired = true
			return map[string]any{"retained": false}, nil
		},
		"pipelineRetentionPurge": func(ctx context.Context, args map[string]any) (any, error) {
			item, err := s.item(args)
			if err != nil {
				return nil, err
			}
			if s.result.DryRun || !item.Retired {
				return nil, errors.New("Library cleanup requires confirmed retirement")
			}
			if err := s.library.PurgeRetiredRunFile(ctx, item.Scope, item.ID); err != nil {
				return nil, err
			}
			item.Purged = true
			return true, nil
		},
		"pipelineRetentionReport": func(context.Context, map[string]any) (any, error) {
			out := s.result
			out.Retained = 0
			out.Retired = 0
			out.Purged = 0
			out.Bytes = 0
			if !out.DryRun {
				for _, item := range s.items {
					if item.Retained {
						out.Retained++
						continue
					}
					if !item.Retired || !item.Purged {
						return nil, errors.New("cleanup page has unconfirmed outcomes")
					}
					out.Retired++
					out.Purged++
					out.Bytes += item.Bytes
				}
			}
			s.result = out
			s.observed = true
			return retentionResultMap(out), nil
		},
	}
}

func retentionCapabilities(i *Integration) []memql.IntegrationCapability {
	s := &artifactRetentionScope{}
	caps := workflowhost.ScopedCapabilities(s.operations())
	caps = append(caps, memql.IntegrationCapability{Name: "expiredArtifacts", Description: "Read confirmed expired files from the caller's exact pipeline run; absence alone never proves expiry.", Handler: i.handleExpiredArtifacts,
		ArgsSchema: map[string]string{"runId": "string (required)", "fileIds": "[]string (required) -- at most 128 bare file identities"}})
	caps = append(caps, workflowhost.ScopedCapabilities(map[string]workflowhost.Operation{"pipelineRetentionSweepPage": nil})...)
	caps = append(caps, memql.IntegrationCapability{Name: "sweepArtifacts", Description: "Internal scheduled artifact retention over one leased producer page.", Handler: i.handleSweepArtifacts})
	caps = append(caps, memql.IntegrationCapability{Name: "adoptLegacyLog", Description: "Verify the selected completed step's existing log before enrolling its bytes in native retention.", Handler: i.handleAdoptLegacyLog,
		ArgsSchema: map[string]string{"runId": "string (required)", "stepKey": "string (required)"}})
	return append(caps, memql.IntegrationCapability{Name: "retireArtifacts", Description: "Retire one bounded page of a caller-owned completed pipeline's unretained artifacts; preserve native receipts and permanent storage fences.", Handler: i.handleRetireArtifacts,
		ArgsSchema: map[string]string{"runId": "string (required)", "afterId": "string", "dryRun": "boolean"}})
}
