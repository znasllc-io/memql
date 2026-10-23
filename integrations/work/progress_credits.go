package work

import (
	"context"
	"fmt"
	"slices"

	"github.com/znasllc-io/memql/component/memql"
	workstate "github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

// The active index is bounded; consumed keys live in immutable owned receipts,
// not a lifetime-sized slice. Deterministic IDs make cross-replica duplicates
// observable forever. A credit written before a failed window update retains
// its target window so a retry can finish that update without losing evidence.
// Caller holds the workload decision gate through both writes.
func (i *Integration) claimProgressCredits(ctx context.Context, rc common.RunContext, state map[string]any, keys []string, next int) ([]string, int, error) {
	var fresh []string
	for start := 0; start < len(keys); start += 100 {
		batch, target, err := i.claimProgressCreditBatch(ctx, rc, state, keys[start:min(start+100, len(keys))], next)
		if err != nil {
			return nil, next, err
		}
		next = target
		fresh = append(fresh, batch...)
		if len(fresh) > 100 {
			fresh = fresh[len(fresh)-100:]
		}
	}
	return fresh, next, nil
}

func (i *Integration) claimProgressCreditBatch(ctx context.Context, rc common.RunContext, state map[string]any, keys []string, next int) ([]string, int, error) {
	if len(keys) == 0 {
		return nil, next, nil
	}
	legacy := rowStringSlice(state, "legacyReceipts")
	if rowInt(state, "policyVersion") < 2 {
		legacy = rowStringSlice(state, "receipts")
	}
	ids := make([]string, 0, len(keys))
	byID := map[string]string{}
	for _, key := range keys {
		if slices.Contains(legacy, key) {
			continue
		}
		id := "v1:work:observation:progress-credit-" + workstate.ArtifactHash(map[string]any{"run": memql.BareShortId(rc.RunId), "evidence": key})
		ids = append(ids, id)
		byID[id] = key
	}
	if len(ids) == 0 {
		return nil, next, nil
	}
	rows, err := i.store().query(ctx, "query "+call("workProgressCreditsForOwnerRun", map[string]any{"runId": rc.RunId, "receiptIds": ids}))
	if err != nil {
		return nil, next, err
	}
	existing := map[string]bool{}
	var fresh []string
	for _, row := range rows {
		id := "v1:work:observation:" + memql.BareShortId(rowString(row, "id"))
		key, ok := byID[id]
		credit := rowMap(rowMap(row, "data"), "progressCredit")
		window := rowInt(credit, "window")
		if !ok || existing[id] || rowString(credit, "key") != key || window < 1 || memql.BareShortId(rowString(row, "ownerUserId")) != memql.BareShortId(rc.OwnerUserId) || memql.BareShortId(rowString(row, "runId")) != memql.BareShortId(rc.RunId) {
			return nil, next, fmt.Errorf("progress credit attribution or identity changed")
		}
		existing[id] = true
		if window > rowInt(state, "extensions") {
			fresh = append(fresh, key)
			next = max(next, window)
		}
	}
	for _, id := range ids {
		if existing[id] {
			continue
		}
		key := byID[id]
		// The short active index also prevents duplicate use if a mock or a
		// stale read omits a just-written credit. Production reads are fresh.
		if slices.Contains(rowStringSlice(state, "receipts"), key) {
			continue
		}
		err := i.store().writeInternal(memql.ContextWithTrainingEvidenceWrite(ctx), "mutation "+call("createWorkObservation", map[string]any{"observationId": id, "runId": rc.RunId, "stepKey": rc.StepKey, "kind": "decision", "content": "Consumed native evidence for one renewable work window.", "data": map[string]any{"progressCredit": map[string]any{"key": key, "window": next}}}))
		if err != nil {
			return nil, next, err
		}
		fresh = append(fresh, key)
	}
	return fresh, next, nil
}
