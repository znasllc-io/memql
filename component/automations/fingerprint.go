package automations

import (
	"fmt"
	"github.com/znasllc-io/memql/component/events"
	"sort"

	"github.com/znasllc-io/memql/core/id"
)

// fingerprintEngine is the shared engine for automation fingerprinting.
var fingerprintEngine = id.New()

// StepDeterministicFingerprint computes a content-addressed ID from the
// deterministic aspects of a step execution.
func StepDeterministicFingerprint(step *Step, result *StepResult) string {
	if step == nil || result == nil {
		return ""
	}

	fp := map[string]any{
		"stepId":   step.ID,
		"stepType": string(step.Type),
		"status":   result.Status,
	}

	// Include deterministic result shape based on step type
	switch step.Type {
	case StepTypeQuery, StepTypeFunction:
		fp["resultShape"] = fingerprintQueryResult(result.Result)

	case StepTypeMutation:
		fp["resultShape"] = fingerprintMutationResult(result.Result)

	case StepTypeWebhook:
		fp["resultShape"] = fingerprintWebhookResult(result.Result)

	case StepTypeEvent:
		fp["resultShape"] = fingerprintEventResult(result.Result)

	case StepTypeShape:
		fp["resultShape"] = fingerprintShapeResult(step, result.Result)

	case StepTypeForEach:
		fp["resultShape"] = fingerprintForEachResult(result)

	case StepTypeParallel:
		fp["resultShape"] = fingerprintParallelResult(result)

	default:
		fp["resultShape"] = map[string]any{
			"type":       "generic",
			"resultType": fmt.Sprintf("%T", result.Result),
		}
	}

	// Include error state (not message - may contain timestamps)
	if result.Error != "" {
		fp["hasError"] = true
	}

	// Include deterministic metadata
	if result.Metadata != nil {
		if itemCount, ok := result.Metadata["itemCount"]; ok {
			fp["itemCount"] = itemCount
		}
	}

	return string(fingerprintEngine.MustFromMap(fp))
}

// fingerprintQueryResult extracts deterministic shape from query results.
func fingerprintQueryResult(result any) map[string]any {
	fp := map[string]any{"type": "query"}

	switch r := result.(type) {
	case []any:
		fp["count"] = len(r)
		ids := extractNodeIds(r)
		sort.Strings(ids)
		fp["nodeIds"] = ids

	case map[string]any:
		if nodes, ok := r["nodes"].([]any); ok {
			fp["count"] = len(nodes)
			ids := extractNodeIds(nodes)
			sort.Strings(ids)
			fp["nodeIds"] = ids
		}
	}

	return fp
}

// fingerprintMutationResult extracts deterministic shape from mutation results.
func fingerprintMutationResult(result any) map[string]any {
	fp := map[string]any{"type": "mutation"}

	if m, ok := result.(map[string]any); ok {
		if id, ok := m["id"].(string); ok {
			fp["targetId"] = id
		}
		if concept, ok := m["concept"].(string); ok {
			fp["concept"] = concept
		}
	}

	return fp
}

// fingerprintWebhookResult - hash status code only.
func fingerprintWebhookResult(result any) map[string]any {
	fp := map[string]any{"type": "webhook"}

	if m, ok := result.(map[string]any); ok {
		if status, ok := m["statusCode"].(int); ok {
			fp["statusCode"] = status
		}
	}

	return fp
}

// fingerprintEventResult - hash topic only.
func fingerprintEventResult(result any) map[string]any {
	fp := map[string]any{"type": "event"}

	if m, ok := result.(map[string]any); ok {
		if topic, ok := m["topic"].(string); ok {
			fp["topic"] = topic
		}
	}

	return fp
}

// fingerprintShapeResult - hash template structure and count.
func fingerprintShapeResult(step *Step, result any) map[string]any {
	fp := map[string]any{"type": "shape"}

	if step.Shape != nil {
		fp["template"] = string(step.Shape.Template)
	}

	if arr, ok := result.([]any); ok {
		fp["count"] = len(arr)
	}

	return fp
}

// fingerprintForEachResult - parent + ordered child fingerprints.
func fingerprintForEachResult(result *StepResult) map[string]any {
	fp := map[string]any{
		"type":       "forEach",
		"childCount": len(result.Children),
	}

	// Ordered list of child fingerprints (preserves iteration order)
	childFPs := make([]string, 0, len(result.Children))
	for _, child := range result.Children {
		if child.ContentId != "" {
			childFPs = append(childFPs, child.ContentId)
		}
	}
	fp["childFingerprints"] = childFPs

	return fp
}

// fingerprintParallelResult - sorted child fingerprints (order-independent).
func fingerprintParallelResult(result *StepResult) map[string]any {
	fp := map[string]any{
		"type":       "parallel",
		"childCount": len(result.Children),
	}

	// Sorted list of child fingerprints (parallel execution order is nondeterministic)
	childFPs := make([]string, 0, len(result.Children))
	for _, child := range result.Children {
		if child.ContentId != "" {
			childFPs = append(childFPs, child.ContentId)
		}
	}
	sort.Strings(childFPs)
	fp["childFingerprints"] = childFPs

	return fp
}

// extractNodeIds pulls IDs from a slice of nodes.
func extractNodeIds(nodes []any) []string {
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if m, ok := n.(map[string]any); ok {
			if id, ok := m["id"].(string); ok {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// eventFingerprintData projects a triggering event down to the fields that may
// enter the execution's chain head. Returns nil for a schedule/manual run.
//
// # WHAT MUST NOT GO IN HERE, AND WHY (memql#2823 / #2867)
//
// events.Event carries a Timestamp (time.Now().UTC()). It is deliberately NOT
// projected, and this function exists so that omission is a named, testable
// decision instead of four lines inlined in ExecuteWithEvent.
//
// A WALL-CLOCK READING IS AN ACCIDENTAL DISCRIMINATOR, NOT A DESIGNED ONE. It
// differs on every evaluation by construction, so any key containing one is
// unique every time. That was merely slow for the (now deleted) step cache. It
// is a CORRECTNESS failure here: this value flows into ComputeInitialChainHead
// -> the per-process dedup check AND clusterGuard.Claim, the cross-replica
// gate that stops two replicas running the same automation. Add the timestamp
// and every event-triggered automation executes once per replica, silently, in
// exactly the 2-replica topology this project runs everywhere.
//
// The corollary, learned the expensive way: strip clock fields SURGICALLY,
// never with a recursive sweep for clock-shaped keys. `payload` legitimately
// contains business fields like `payload.timestamp` that SHOULD discriminate.
// Dropping a real input from a key is a correctness bug; leaving a clock in
// one is merely a slow one -- that asymmetry, not tidiness, decides the design.
//
// Pinned by TestEventFingerprintDataExcludesTheWallClock.
func eventFingerprintData(triggeringEvent *events.Event) map[string]any {
	if triggeringEvent == nil {
		return nil
	}
	return map[string]any{
		"topic":   triggeringEvent.Topic,
		"kind":    triggeringEvent.Kind.String(),
		"payload": triggeringEvent.Payload,
	}
}

// ComputeInitialChainHead creates the starting chain state for an execution.
//
// Feeds execution dedup and the cross-replica cluster guard, so it must be a
// pure function of the automation identity plus real inputs. See
// eventFingerprintData above for the wall-clock rule and what it costs to
// break it.
func ComputeInitialChainHead(automationName, triggeredBy string, triggeringEvent map[string]any, inputFingerprint string) string {
	fp := map[string]any{
		"automation":  automationName,
		"triggeredBy": triggeredBy,
	}

	if triggeringEvent != nil {
		if topic, ok := triggeringEvent["topic"]; ok {
			fp["eventTopic"] = topic
		}
		// Include canonicalized event payload for dedup
		if payload, ok := triggeringEvent["payload"].(map[string]any); ok {
			fp["eventPayloadFingerprint"] = string(fingerprintEngine.MustFromMap(payload))
		}
	}

	if inputFingerprint != "" {
		fp["inputFingerprint"] = inputFingerprint
	}

	return string(fingerprintEngine.MustFromMap(fp))
}

// FingerprintInput creates a deterministic hash of input query results.
//
// Reached only when automation.Input is set, which nothing populates today
// (see the note at its call site in executor.go). The wall-clock rule that
// governs every key in this file lives on eventFingerprintData above, next to
// the key that IS live -- an earlier revision of this change put it here, on
// the strength of "whoever builds the next key will start at the surviving
// helper", which was wrong: a reader of running code never lands here.
func FingerprintInput(input any) string {
	return string(fingerprintEngine.MustFromMap(map[string]any{
		"input": fingerprintQueryResult(input),
	}))
}

// ComputeChildFingerprint creates a ContentId for a forEach child iteration.
func ComputeChildFingerprint(parentStepId string, index int, result *StepResult) string {
	if result == nil {
		return ""
	}

	fp := map[string]any{
		"parentStep": parentStepId,
		"childType":  "forEachIteration",
		"index":      index,
		"status":     result.Status,
	}

	// Include deterministic result shape
	if result.Result != nil {
		fp["resultShape"] = fingerprintGenericResult(result.Result)
	}

	if result.Error != "" {
		fp["hasError"] = true
	}

	return string(fingerprintEngine.MustFromMap(fp))
}

// ComputeBranchFingerprint creates a ContentId for a parallel branch.
func ComputeBranchFingerprint(parentStepId, branchName string, result *StepResult) string {
	if result == nil {
		return ""
	}

	fp := map[string]any{
		"parentStep": parentStepId,
		"childType":  "parallelBranch",
		"branchName": branchName,
		"status":     result.Status,
	}

	// Include deterministic result shape
	if result.Result != nil {
		fp["resultShape"] = fingerprintGenericResult(result.Result)
	}

	if result.Error != "" {
		fp["hasError"] = true
	}

	return string(fingerprintEngine.MustFromMap(fp))
}

// AdvanceChain computes the next chain head after a step.
func AdvanceChain(prevHead, contentId string) string {
	if prevHead == "" || contentId == "" {
		return contentId
	}
	return string(fingerprintEngine.Combine(
		id.ID(prevHead),
		id.ID(contentId),
	))
}

// fingerprintGenericResult creates a deterministic shape for any result type.
func fingerprintGenericResult(result any) map[string]any {
	fp := map[string]any{"type": "generic"}

	switch r := result.(type) {
	case []any:
		fp["count"] = len(r)
	case map[string]any:
		fp["keyCount"] = len(r)
	case string:
		fp["length"] = len(r)
	case nil:
		fp["isNil"] = true
	default:
		fp["resultType"] = fmt.Sprintf("%T", result)
	}

	return fp
}
