package pipelinerun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
)

// Ordinary owner-scoped queries, never system authority. The native inspection
// also binds the journal's exact source/run to the authorized pipeline row.
func readSARIFEvidence(ctx context.Context, engine Engine, owner string, r Run, key string) (pipelines.ReleaseWorkReceipt, error) {
	one := func(query string) (map[string]any, error) {
		res, err := engine.Execute(memql.ContextWithFreshRead(ctx), query)
		if err != nil {
			return nil, err
		}
		if res == nil {
			return nil, errors.New("SARIF journal read returned no result")
		}
		body, err := json.Marshal(res.OutputPayload())
		if err != nil || len(body) > 1<<20 {
			return nil, errors.New("SARIF journal exceeds its read bound")
		}
		var rows []map[string]any
		if err := json.Unmarshal(body, &rows); err != nil || len(rows) != 1 {
			return nil, errors.New("SARIF requires exactly one authorized journal row")
		}
		for _, field := range []string{"id", "ownerUserId", "runId"} {
			if v, ok := rows[0][field].(string); ok {
				rows[0][field] = memql.BareShortId(v)
			}
		}
		return rows[0], nil
	}
	run, err := one(fmt.Sprintf("query workRunForOwner(runId: %s)", langparser.QuoteString(r.WorkRunID)))
	if err != nil {
		return pipelines.ReleaseWorkReceipt{}, err
	}
	step, err := one(fmt.Sprintf("query workStepForOwnerRun(runId: %s, stepKey: %s)", langparser.QuoteString(r.WorkRunID), langparser.QuoteString(key)))
	if err != nil {
		return pipelines.ReleaseWorkReceipt{}, err
	}
	proof, err := pipelines.InspectScannerReceipt(owner, r.WorkRunID, key, run, step)
	if err != nil {
		return proof, err
	}
	input, ok := run["input"].(map[string]any)
	if !ok || input["pipelineRunId"] != r.ID || input["pipelineId"] != r.PipelineID ||
		proof.Repository != r.Repository || proof.Commit != r.SHA || proof.Event != string(r.Event) || proof.Mode != string(r.Mode) {
		return proof, errors.New("SARIF work journal differs from its pipeline source")
	}
	return proof, nil
}
