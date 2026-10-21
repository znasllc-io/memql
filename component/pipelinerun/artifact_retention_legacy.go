package pipelinerun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

func (i *Integration) handleAdoptLegacyLog(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	r, err := i.AdoptLegacyLog(handlerContext(ctx), stringArg(args, "runId"), stringArg(args, "stepKey"))
	if err != nil {
		return nil, err
	}
	return resultNode(map[string]any{"intentId": r.IntentID, "fileId": r.FileID, "bytes": r.Size}), nil
}

// The caller selects a step, never an object URL or a file to delete. Its
// completed journal supplies the exact log identity and attempt. Adoption
// verifies bytes and enrolls a receipt; it performs no retirement itself.
func (i *Integration) AdoptLegacyLog(ctx context.Context, runID, key string) (pipelinesteps.StoredFileReceipt, error) {
	var zero pipelinesteps.StoredFileReceipt
	owner, err := personFrom(ctx)
	if err != nil {
		return zero, err
	}
	owner = memql.BareShortId(owner)
	if runID == "" || len(runID) > 512 || key == "" || len(key) > 512 {
		return zero, errors.New("legacy log adoption requires a run and step")
	}
	d := i.snapshot()
	if d.Store == nil || d.ArtifactRetention == nil || d.ArtifactRetention.Engine == nil || d.ArtifactRetention.Library == nil {
		return zero, errors.New("native pipeline artifact retention is unavailable")
	}
	ctx, cancel := context.WithTimeout(memql.ContextWithFreshRead(ctx), 2*time.Minute)
	defer cancel()
	r, err := d.Store.RunForOwner(ctx, runID)
	if err != nil {
		return zero, err
	}
	if r == nil || memql.BareShortId(r.OwnerUserID) != owner || r.Status != StatusCompleted {
		return zero, errors.New("log adoption requires the caller's completed pipeline")
	}
	if err := retentionProducerStopped(ctx, d.ArtifactRetention.Engine, owner, *r); err != nil {
		return zero, err
	}
	raw, err := d.ArtifactRetention.Engine.Execute(ctx, "query workStepForOwnerRun(runId: "+langparser.QuoteString(r.WorkRunID)+", stepKey: "+langparser.QuoteString(key)+")")
	if err != nil {
		return zero, err
	}
	rows := rowsOf(raw)
	if len(rows) != 1 {
		return zero, errors.New("legacy log adoption requires its authorized step journal")
	}
	step := rows[0]
	if memql.BareShortId(fmt.Sprint(step["ownerUserId"])) != owner || memql.BareShortId(fmt.Sprint(step["runId"])) != r.WorkRunID || step["key"] != key {
		return zero, errors.New("log journal differs from its pipeline")
	}
	switch step["status"] {
	case "done", "failed", "cancelled":
	default:
		return zero, errors.New("legacy log producer is not terminal")
	}
	// Decode the integer exactly; a fractional or oversized attempt is refused.
	data, err := json.Marshal(step["attempt"])
	var attempt int
	if err != nil || json.Unmarshal(data, &attempt) != nil || attempt < 1 {
		return zero, errors.New("legacy log has no valid producer attempt")
	}
	file, ok := step["logFileId"].(string)
	if !ok || file == "" {
		return zero, errors.New("completed step has no retained log")
	}
	return d.ArtifactRetention.Library.AdoptLegacyRunLog(auth.ContextWithInternalOrigin(ctx), pipelinesteps.RunFileReceiptScope{OwnerUserID: owner, WorkRunID: r.WorkRunID, StepKey: key, Attempt: attempt}, memql.BareShortId(file))
}
