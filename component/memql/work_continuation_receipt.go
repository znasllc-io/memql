package memql

import (
	"encoding/json"
	"fmt"

	"github.com/znasllc-io/memql/core/common"
)

// ValidateWorkContinuationReceipt checks durable source identity without
// releasing a run or granting execution. Callers must read the latest receipt
// under its owner's authority; native execution revalidates it at admission.
func ValidateWorkContinuationReceipt(run common.RunContext, receipt map[string]any, expectedID string) error {
	if !run.IsRun() || run.OwnerUserId == "" || run.StepKey == "" || expectedID == "" || !run.Override.Empty() || run.Mode == common.RunModeReplay ||
		BareShortId(stringField(receipt, "id")) != BareShortId(expectedID) ||
		BareShortId(stringField(receipt, "ownerUserId")) != BareShortId(run.OwnerUserId) ||
		BareShortId(stringField(receipt, "runId")) != BareShortId(run.RunId) || stringField(receipt, "stepKey") != run.StepKey {
		return fmt.Errorf("the saved work checkpoint does not match this execution")
	}
	saved, _ := receipt["data"].(map[string]any)
	if saved["continuation"] != true || stringField(saved, "rerunRequestId") != run.RerunRequestId {
		return fmt.Errorf("the saved checkpoint belongs to another execution request")
	}
	messages, err := decodeWorkContinuation(saved)
	if err != nil || len(messages) == 0 {
		return fmt.Errorf("the saved work checkpoint is invalid")
	}
	canonical, err := json.Marshal(messages)
	if err != nil || BareShortId(expectedID) != continuationID(run, canonical) {
		return fmt.Errorf("the saved work checkpoint content changed")
	}
	return nil
}
