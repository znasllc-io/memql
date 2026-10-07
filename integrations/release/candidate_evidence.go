package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/znasllc-io/memql/component/memql"
	pl "github.com/znasllc-io/memql/component/pipelines"
)

type candidateEvidenceReader struct {
	engine interface {
		Execute(context.Context, string) (*memql.ExecuteResult, error)
	}
}

// read uses existing owner-scoped reads without escalating origin or changing
// the actor. The fresh-read flag avoids another replica's result cache. Every
// identity and terminal verdict is checked again after the engine's row gate.
func (r candidateEvidenceReader) read(ctx context.Context, runID, stepKey string) (pl.ReleaseWorkReceipt, error) {
	owner, err := candidateOwner(ctx)
	if err != nil {
		return pl.ReleaseWorkReceipt{}, err
	}
	if r.engine == nil {
		return pl.ReleaseWorkReceipt{}, errors.New("release evidence reader is unavailable")
	}
	if runID == "" || stepKey == "" || len(runID) > 512 || len(stepKey) > 512 {
		return pl.ReleaseWorkReceipt{}, errors.New("release evidence requires a bounded run and step identity")
	}
	one := func(name string, args map[string]any) (map[string]any, error) {
		res, err := r.engine.Execute(memql.ContextWithFreshRead(ctx), "query "+renderCall(name, args))
		if err != nil {
			return nil, err
		}
		if res == nil {
			return nil, errors.New("release evidence returned no journal row")
		}
		// These queries have explicit @row shapes. A changed envelope refuses
		// rather than treating arbitrary nested data as a successful receipt.
		b, err := json.Marshal(res.OutputPayload())
		if err != nil || len(b) > 1<<20 {
			return nil, errors.New("release evidence exceeds its result bound")
		}
		var rows []map[string]any
		if err := json.Unmarshal(b, &rows); err != nil || len(rows) != 1 {
			return nil, errors.New("release evidence requires exactly one authorized journal row")
		}
		for _, key := range []string{"id", "ownerUserId", "runId"} {
			if value, ok := rows[0][key].(string); ok {
				rows[0][key] = memql.BareShortId(value)
			}
		}
		return rows[0], nil
	}
	run, err := one("workRunForOwner", map[string]any{"runId": runID})
	if err != nil {
		return pl.ReleaseWorkReceipt{}, err
	}
	step, err := one("workStepForOwnerRun", map[string]any{"runId": runID, "stepKey": stepKey})
	if err != nil {
		return pl.ReleaseWorkReceipt{}, err
	}
	return pl.InspectReleaseWorkReceipt(owner, runID, stepKey, run, step)
}

// verify checks all evidence and all producing steps, including artifacts not
// explicitly listed as a check. It is an integrity gate, not required-check
// selection: the installed DSL must first choose a complete release plan.
func (r candidateEvidenceReader) verify(ctx context.Context, candidate pl.ReleaseCandidate) error {
	owner, err := candidateOwner(ctx)
	if err != nil {
		return err
	}
	if candidate.OwnerUserID != owner {
		return errors.New("candidate evidence belongs to another owner")
	}
	if _, _, err := pl.CanonicalReleaseCandidate(candidate); err != nil {
		return err
	}
	components := map[string]pl.ReleaseComponent{}
	for _, c := range candidate.Components {
		components[c.Name] = c
	}
	for _, e := range candidate.Evidence {
		actual, err := r.read(ctx, e.WorkRunID, e.StepKey)
		if err != nil {
			return fmt.Errorf("check %s: %w", e.Name, err)
		}
		want := components[e.Component]
		ids := slices.Clone(e.ArtifactIntentIDs)
		slices.Sort(ids)
		if actual.Repository != want.Repository || actual.Commit != want.Commit || actual.Attempt != e.Attempt || actual.ReceiptID != e.ReceiptID ||
			actual.ReceiptDigest != e.ReceiptDigest || actual.DefinitionDigest != e.DefinitionDigest || !slices.Equal(actual.ArtifactIntentIDs, ids) {
			return fmt.Errorf("check %s does not match the candidate's exact source and receipt", e.Name)
		}
	}
	for _, c := range candidate.Components {
		for _, a := range c.Artifacts {
			expected := a.Receipt
			actual, err := r.read(ctx, expected.WorkRunID, expected.StepKey)
			if err != nil {
				return fmt.Errorf("artifact %s/%s: %w", c.Name, a.Name, err)
			}
			if actual.Repository != c.Repository || actual.Commit != c.Commit || actual.Attempt != expected.Attempt ||
				actual.ReceiptDigest != expected.ReceiptDigest || actual.DefinitionDigest != expected.DefinitionDigest || !slices.Contains(actual.ArtifactIntentIDs, expected.IntentID) {
				return fmt.Errorf("artifact %s/%s does not match its producing execution receipt", c.Name, a.Name)
			}
		}
	}
	return nil
}
