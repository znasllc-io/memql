package library

import (
	"context"
	"maps"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

type trainingSelectionReceipt struct {
	SchemaVersion     int              `json:"schemaVersion"`
	Method            string           `json:"method"`
	QueryDigest       string           `json:"queryDigest"`
	Limit             int              `json:"limit"`
	MinimumSimilarity float64          `json:"minimumSimilarity"`
	MaxBytes          int              `json:"maxBytes"`
	CandidateLimit    int              `json:"candidateLimit"`
	Status            string           `json:"status"`
	Selected          []map[string]any `json:"selected"`
	Rejected          []map[string]any `json:"rejected"`
}

func trainingDigest(v string) string { return string(id.NewUntracked().FromString(v)) }

func (i *Integration) saveTrainingSelection(ctx context.Context, run common.RunContext, receiptID string, receipt trainingSelectionReceipt) error {
	// This is the existing owned execution journal, not a second Training store.
	if _, _, err := i.ownedTrainingScope(ctx); err != nil {
		return err
	}
	call, err := parser.RenderCall("createWorkObservation", map[string]any{"observationId": receiptID, "runId": run.RunId, "stepKey": run.StepKey, "kind": "decision", "content": "Training lesson selection; delivery and adherence require separate model-call and outcome evidence.", "data": map[string]any{"trainingSelection": receipt, "stage": "selected-for-context"}})
	if err != nil {
		return err
	}
	_, err = i.engine.Execute(auth.ContextWithInternalOrigin(memql.ContextWithTrainingEvidenceWrite(ctx)), "mutation "+call)
	return err
}

func selectedTrainingEvidence(payload map[string]any) map[string]any {
	// Selected text is already admitted, revalidated and bounded by allocation.
	copy := maps.Clone(payload)
	copy["textDigest"] = trainingDigest(stringField(payload, "text"))
	return copy
}
