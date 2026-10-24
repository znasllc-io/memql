package work

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

// The model identifies a need from the goal and observed work; the native
// boundary limits retrieval, verifies the live owner/step, and records what
// actually came back. Relevance is not competence: this operation cannot mark
// work ready, grant a capability, or publish its own findings as Training.
func (i *Integration) handleConsultKnowledge(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	need := strings.TrimSpace(argString(args, "need"))
	evidence := strings.TrimSpace(argString(args, "context"))
	if need == "" || len(need) > 1000 || len(evidence) > 3000 {
		return nil, fmt.Errorf("knowledge consultation requires one need of at most 1000 bytes and at most 3000 bytes of relevant context")
	}
	query := need
	if evidence != "" {
		query += "\nObserved context (verify against cited sources): " + evidence
	}
	// These bounds cannot be increased by model-supplied arguments. The same
	// live scope check as initial selection runs before either catalog is read.
	rows, err := i.selectSkillContext(ctx, map[string]any{"text": query, "limit": 2, "minimumScore": 1, "maxBytes": 4000}, true)
	if err != nil {
		return nil, err
	}
	var skills struct {
		ReceiptID string `json:"receiptId"`
		Selection struct {
			Selected []map[string]any `json:"selected"`
		} `json:"selection"`
	}
	if len(rows) != 1 || json.Unmarshal(rows[0].Payload, &skills) != nil || skills.ReceiptID == "" {
		return nil, fmt.Errorf("knowledge consultation has no skill selection receipt")
	}
	// This fixed reader owns its narrow admission boundary. Never stamp the
	// model's context internal or accept a caller-selected construct here.
	raw, err := i.engine.Execute(ctx, "builtin "+call("libraryConsultWorkTraining", map[string]any{"text": query}))
	if err != nil {
		return nil, err
	}
	// Builtins can return an id-keyed MemoryNode set rather than a shaped
	// query array. Use the engine's canonical decoder; treating that set as
	// empty would lose every real passage while map-only test fixtures pass.
	training := memql.MaterializeRows(raw)
	if len(training) != 1 || argString(training[0], "receiptId") == "" || argString(training[0], "status") == "" {
		return nil, fmt.Errorf("knowledge consultation has no Training selection receipt")
	}
	rc, _ := common.RunFromContext(ctx)
	receiptID := "knowledge-consultation-" + id.NewShortId()
	result := map[string]any{
		"schemaVersion": 1, "receiptId": receiptID, "need": need, "context": evidence,
		"readiness": "not-assessed", "skills": skills.Selection.Selected, "skillReceiptId": skills.ReceiptID,
		"training": map[string]any{"receiptId": training[0]["receiptId"], "status": training[0]["status"], "passages": training[0]["passages"]},
		"scope":    "Reviewed skill instructions and Training passages only; concepts, prior work, artifact contents and external sources are not searched by this operation.",
	}
	// The request is a model-supplied description, not verified evidence. A
	// retained selection proves retrieval only. Keep that distinction beside
	// the receipts so later learning cannot treat self-assessment as validation.
	err = i.store().writeInternal(ctx, "mutation "+call("createWorkObservation", map[string]any{
		"observationId": receiptID, "runId": rc.RunId, "stepKey": rc.StepKey, "kind": "decision",
		"content": "Consulted reviewed knowledge for a discovered need; applicability and readiness remain to be checked.",
		"data":    map[string]any{"knowledgeConsultation": result, "stage": "consulted-for-need"},
	}))
	if err != nil {
		return nil, err
	}
	return i.resultNode(result), nil
}
