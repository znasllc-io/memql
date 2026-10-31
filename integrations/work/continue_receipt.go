package work

import (
	"context"
	"fmt"
	"maps"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

func (i *Integration) readResultFeedback(ctx context.Context, runID, requestID string) (map[string]any, bool, time.Time, error) {
	id := memql.WorkResultFeedbackID(runID, requestID)
	rows, err := i.store().query(ctx, "query "+call("workResultFeedbackForOwnerRequest", map[string]any{"runId": runID, "observationId": id}))
	if err != nil {
		return nil, false, time.Time{}, err
	}
	if len(rows) == 0 {
		return nil, false, time.Time{}, nil
	}
	if len(rows) != 1 {
		return nil, false, time.Time{}, fmt.Errorf("work: ambiguous result feedback receipt")
	}
	row := rows[0]
	receipt := rowMap(rowMap(row, "data"), "resultFeedback")
	if memql.BareShortId(rowString(row, "id")) != memql.BareShortId(id) ||
		memql.BareShortId(rowString(row, "runId")) != memql.BareShortId(runID) ||
		memql.BareShortId(rowString(row, "ownerUserId")) != memql.BareShortId(callerUserId(ctx)) ||
		rowInt(receipt, "schemaVersion") != 1 || rowString(receipt, "id") != id || rowString(receipt, "requestId") != requestID {
		return nil, false, time.Time{}, fmt.Errorf("work: result feedback attribution changed")
	}
	at, valid := rowTime(row, "createdAt")
	if !valid {
		return nil, false, time.Time{}, fmt.Errorf("work: result feedback receipt version is unavailable")
	}
	return receipt, receipt["accepted"] == true, at, nil
}

// Intent precedes dispatch; acceptance follows it. The run's latest receipt
// bridges a crash between those writes. Before replacing it, continueRun seals
// the prior acceptance into its immutable identity, retaining all directions.
// Pending intents never become guidance unless that exact run receipt proves
// the request committed. Both phases use the same ID and human timestamp.
func (i *Integration) writeResultFeedback(ctx context.Context, runID string, receipt map[string]any, accepted bool) error {
	data := maps.Clone(receipt)
	if rowString(data, "id") != memql.WorkResultFeedbackID(runID, rowString(data, "requestId")) ||
		memql.BareShortId(rowString(data, "requestedBy")) != memql.BareShortId(callerUserId(ctx)) || rowInt(data, "schemaVersion") != 1 {
		return fmt.Errorf("work: result feedback receipt cannot be committed")
	}
	saved, sealed, prior, err := i.readResultFeedback(ctx, runID, rowString(data, "requestId"))
	if err != nil {
		return err
	}
	if saved != nil && !sameResultFeedback(saved, data) {
		return fmt.Errorf("work: saved result feedback changed")
	}
	// Never write an earlier phase, even when a retry lands on a slow clock.
	if sealed || (saved != nil && !accepted) {
		return nil
	}
	data["accepted"] = accepted
	version := workRowVersionAfter(prior, i.clock().UTC())
	return i.store().writeInternal(ctx, "mutation "+call("createWorkObservation", map[string]any{
		"observationId": rowString(data, "id"), "versionTime": rfc(version), "runId": runID, "stepKey": rowString(data, "stepKey"), "kind": "decision",
		"content": "Owner feedback on an unaccepted result", "data": map[string]any{"resultFeedback": data},
	}))
}

func (i *Integration) sealResultFeedback(ctx context.Context, runID string, receipt map[string]any) error {
	if len(receipt) == 0 {
		return nil
	}
	saved, accepted, _, err := i.readResultFeedback(ctx, runID, rowString(receipt, "requestId"))
	if err != nil {
		return err
	}
	if saved != nil && !sameResultFeedback(saved, receipt) {
		return fmt.Errorf("work: saved result feedback changed")
	}
	if accepted {
		return nil
	}
	return i.writeResultFeedback(ctx, runID, receipt, true)
}

func sameResultFeedback(a, b map[string]any) bool {
	for _, key := range []string{"id", "requestId", "runVersion", "feedback", "requestedBy", "requestedAt", "stepKey"} {
		if rowString(a, key) != rowString(b, key) {
			return false
		}
	}
	return true
}

// The run write is the dispatch commit. A later journal failure cannot turn
// accepted work into a refusal. The caller sees the pending seal; retry and the
// next continuation repair it before any new dispatch can replace this proof.
func (i *Integration) resultFeedbackAcknowledgement(ctx context.Context, runID string, receipt map[string]any) map[string]any {
	state := "sealed"
	if err := i.sealResultFeedback(ctx, runID, receipt); err != nil {
		state = "pending"
		i.log().WarnContext(ctx, "work result feedback accepted; receipt seal pending", "run_id", runID, "receipt_id", rowString(receipt, "id"), "error", err)
	}
	return map[string]any{"runId": runID, "continuationRequested": true, "feedbackReceiptState": state}
}
