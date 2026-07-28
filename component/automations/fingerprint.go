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

// DefinitionId computes a content-addressed ID for the step definition.
//
// UNUSED SINCE memql#2899 -- like FingerprintStepInput below, its only caller was
// the deleted step cache's key computation. Unlike that one it heads no chain and
// nothing downstream depends on it, so it is a straightforward removal whenever
// memql#2941 is picked up. Do not add a caller without reading #2899 first.
// The ID includes all fields that affect output, ensuring cache key
// changes when the step definition changes.
func (s *Step) DefinitionId(engine *id.Engine) id.ID {
	if s == nil || engine == nil {
		return ""
	}

	def := map[string]any{
		"id":   s.ID,
		"type": string(s.Type),
	}

	switch s.Type {
	case StepTypeQuery:
		if s.Query != nil {
			def["query"] = s.Query.Query
		}
	case StepTypeShape:
		if s.Shape != nil {
			def["source"] = s.Shape.Source
			def["template"] = string(s.Shape.Template)
		}
	case StepTypeMutation:
		if s.Mutation != nil {
			def["concept"] = s.Mutation.Concept
		}
	case StepTypeFunction:
		if s.Function != nil {
			def["name"] = s.Function.Name
		}
	case StepTypeForEach:
		if s.ForEach != nil {
			def["source"] = s.ForEach.Source
			def["filter"] = s.ForEach.Filter
		}
	}

	return engine.MustFromMap(def)
}

// FingerprintStepInput creates a deterministic ID from the step's input data.
// The input is resolved from the evaluator based on step type.
//
// UNUSED SINCE memql#2899 -- its only caller was the automation step cache's key
// computation, which went with the cache. Kept for now rather than deleted in the
// same change, because it is the head of a chain that reaches
// ContextFingerprint and its clock-stripping tests; whether that whole chain goes
// is its own decision. Do not add a caller without reading #2899 first.
func FingerprintStepInput(step *Step, evaluator *Evaluator) id.ID {
	if step == nil || evaluator == nil {
		return ""
	}

	var input any

	switch step.Type {
	case StepTypeShape:
		if step.Shape != nil {
			input, _ = evaluator.EvaluateValue(step.Shape.Source)
		}
	case StepTypeForEach:
		if step.ForEach != nil {
			input, _ = evaluator.EvaluateValue(step.ForEach.Source)
		}
	case StepTypeQuery:
		// For queries, evaluate using EvaluateStringForQuery to match actual execution
		// This ensures proper quoting of values and identical cache keys
		if step.Query != nil {
			evaluatedQuery, err := evaluator.EvaluateStringForQuery(step.Query.Query)
			if err != nil {
				// Fall back to raw query if evaluation fails
				input = step.Query.Query
			} else {
				input = evaluatedQuery
			}
		}
	case StepTypeFunction:
		// Functions can read any evaluator state ($input, $steps.*, $item, custom vars)
		// so we must fingerprint the entire context to ensure cache correctness
		input = evaluator.ContextFingerprint()
	default:
		// Use $input to get the automation input data
		input, _ = evaluator.EvaluateValue("$input")
	}

	return fingerprintEngine.MustFromMap(map[string]any{
		"input": input,
	})
}
