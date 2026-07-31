package automations

import (
	"fmt"
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

// ComputeInitialChainHead creates the starting chain state for an execution.
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
// # IF YOU ARE BUILDING A CACHE KEY, READ THIS FIRST (memql#2823 / #2867)
//
// This note outlives the code it was learned from. memql#2899 deleted the
// automation step cache, and memql#2941 deleted the key-computation chain that
// fed it (FingerprintStepInput -> Evaluator.ContextFingerprint ->
// fingerprintCustoms) together with the tests that pinned this behaviour. The
// lesson is kept here, on the surviving fingerprint helper, because it is
// about cache keys in general and whoever builds the next one will start
// somewhere near this function.
//
// A WALL-CLOCK READING IS AN ACCIDENTAL CACHE DISCRIMINATOR, NOT A DESIGNED
// ONE. It differs on every evaluation by construction, so a key containing one
// guarantees a miss. Two seeds put one into the automation evaluator's
// customs: `timestamp` (executor.go, RFC3339, second granularity) and
// `actor.now` (auth.ActorEnvelopeMap, RFC3339 NANO). The first only made hits
// improbable; once #2801 bound the actor envelope onto every evaluator, the
// nanosecond field made the function-step key a guaranteed miss.
//
// Two consequences that cost real time to work out, neither obvious:
//
//   - Strip such fields SURGICALLY, never with a recursive sweep for
//     clock-shaped keys. `event` is a custom too, so a recursive strip also
//     drops `event.payload.timestamp` -- a genuine business field that SHOULD
//     discriminate a cached result. Dropping a real input from a key is a
//     CORRECTNESS bug; leaving a clock in one is merely a slow one. That
//     asymmetry, not tidiness, is what decides the design.
//   - Removing the clock is not free, and pretending otherwise hides a second
//     defect. The DSL's `now` resolves from that same `timestamp` custom, so
//     for a step whose body reads `now` the per-second key churn WAS what kept
//     it re-running. That is not a cache-invalidation strategy -- it narrows
//     the window rather than closing it, since entries were TTL-bounded
//     regardless. The real defect was that a side-effecting `logic` step was
//     classified cacheable at all (memql#2869). A clock in the key MASKS a
//     misclassification; fix the classification, do not keep the clock.
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
