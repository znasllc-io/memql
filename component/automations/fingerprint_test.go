package automations

import (
	"testing"
)

func TestStepDeterministicFingerprint_Deterministic(t *testing.T) {
	step := &Step{ID: "test", Type: StepTypeQuery}
	result := &StepResult{
		StepId: "test",
		Status: "success",
		Result: []any{
			map[string]any{"id": "node-1"},
			map[string]any{"id": "node-2"},
		},
	}

	fp1 := StepDeterministicFingerprint(step, result)
	fp2 := StepDeterministicFingerprint(step, result)

	if fp1 != fp2 {
		t.Errorf("fingerprint should be deterministic: got %s and %s", fp1, fp2)
	}

	if fp1 == "" {
		t.Error("fingerprint should not be empty")
	}
}

func TestStepDeterministicFingerprint_OrderIndependent(t *testing.T) {
	step := &Step{ID: "test", Type: StepTypeQuery}

	// Same nodes, different order
	result1 := &StepResult{
		StepId: "test",
		Status: "success",
		Result: []any{
			map[string]any{"id": "node-1"},
			map[string]any{"id": "node-2"},
		},
	}
	result2 := &StepResult{
		StepId: "test",
		Status: "success",
		Result: []any{
			map[string]any{"id": "node-2"},
			map[string]any{"id": "node-1"},
		},
	}

	fp1 := StepDeterministicFingerprint(step, result1)
	fp2 := StepDeterministicFingerprint(step, result2)

	if fp1 != fp2 {
		t.Errorf("fingerprint should be order-independent for query results: got %s and %s", fp1, fp2)
	}
}

func TestStepDeterministicFingerprint_DifferentResults(t *testing.T) {
	step := &Step{ID: "test", Type: StepTypeQuery}

	result1 := &StepResult{
		StepId: "test",
		Status: "success",
		Result: []any{
			map[string]any{"id": "node-1"},
		},
	}
	result2 := &StepResult{
		StepId: "test",
		Status: "success",
		Result: []any{
			map[string]any{"id": "node-2"},
		},
	}

	fp1 := StepDeterministicFingerprint(step, result1)
	fp2 := StepDeterministicFingerprint(step, result2)

	if fp1 == fp2 {
		t.Error("fingerprints should differ for different results")
	}
}

func TestStepDeterministicFingerprint_DifferentStatus(t *testing.T) {
	step := &Step{ID: "test", Type: StepTypeQuery}

	result1 := &StepResult{
		StepId: "test",
		Status: "success",
	}
	result2 := &StepResult{
		StepId: "test",
		Status: "failed",
	}

	fp1 := StepDeterministicFingerprint(step, result1)
	fp2 := StepDeterministicFingerprint(step, result2)

	if fp1 == fp2 {
		t.Error("fingerprints should differ for different status")
	}
}

func TestStepDeterministicFingerprint_MutationResult(t *testing.T) {
	step := &Step{ID: "mutation", Type: StepTypeMutation}
	result := &StepResult{
		StepId: "mutation",
		Status: "success",
		Result: map[string]any{
			"id":      "user-123",
			"concept": "v1:user",
		},
	}

	fp1 := StepDeterministicFingerprint(step, result)
	fp2 := StepDeterministicFingerprint(step, result)

	if fp1 != fp2 {
		t.Errorf("mutation fingerprint should be deterministic: got %s and %s", fp1, fp2)
	}
}

func TestStepDeterministicFingerprint_WebhookResult(t *testing.T) {
	step := &Step{ID: "webhook", Type: StepTypeWebhook}
	result := &StepResult{
		StepId: "webhook",
		Status: "success",
		Result: map[string]any{
			"statusCode": 200,
			"body":       "response varies", // should be ignored
		},
	}

	fp := StepDeterministicFingerprint(step, result)
	if fp == "" {
		t.Error("webhook fingerprint should not be empty")
	}
}

func TestStepDeterministicFingerprint_ForEachPreservesOrder(t *testing.T) {
	step := &Step{ID: "forEach", Type: StepTypeForEach}

	// Children with different order
	result1 := &StepResult{
		StepId: "forEach",
		Status: "success",
		Children: []*StepResult{
			{ContentId: "child-a"},
			{ContentId: "child-b"},
		},
	}
	result2 := &StepResult{
		StepId: "forEach",
		Status: "success",
		Children: []*StepResult{
			{ContentId: "child-b"},
			{ContentId: "child-a"},
		},
	}

	fp1 := StepDeterministicFingerprint(step, result1)
	fp2 := StepDeterministicFingerprint(step, result2)

	// ForEach should preserve order, so different child order = different fingerprint
	if fp1 == fp2 {
		t.Error("forEach fingerprint should preserve child order")
	}
}

func TestStepDeterministicFingerprint_ParallelOrderIndependent(t *testing.T) {
	step := &Step{ID: "parallel", Type: StepTypeParallel}

	// Children with different order
	result1 := &StepResult{
		StepId: "parallel",
		Status: "success",
		Children: []*StepResult{
			{ContentId: "child-a"},
			{ContentId: "child-b"},
		},
	}
	result2 := &StepResult{
		StepId: "parallel",
		Status: "success",
		Children: []*StepResult{
			{ContentId: "child-b"},
			{ContentId: "child-a"},
		},
	}

	fp1 := StepDeterministicFingerprint(step, result1)
	fp2 := StepDeterministicFingerprint(step, result2)

	// Parallel should sort children, so different order = same fingerprint
	if fp1 != fp2 {
		t.Errorf("parallel fingerprint should be order-independent: got %s and %s", fp1, fp2)
	}
}

func TestStepDeterministicFingerprint_NilInputs(t *testing.T) {
	if fp := StepDeterministicFingerprint(nil, nil); fp != "" {
		t.Errorf("nil inputs should return empty fingerprint, got %s", fp)
	}

	step := &Step{ID: "test", Type: StepTypeQuery}
	if fp := StepDeterministicFingerprint(step, nil); fp != "" {
		t.Errorf("nil result should return empty fingerprint, got %s", fp)
	}

	result := &StepResult{StepId: "test", Status: "success"}
	if fp := StepDeterministicFingerprint(nil, result); fp != "" {
		t.Errorf("nil step should return empty fingerprint, got %s", fp)
	}
}

func TestComputeInitialChainHead_Deterministic(t *testing.T) {
	head1 := ComputeInitialChainHead("testAutomation", "manual", nil, "")
	head2 := ComputeInitialChainHead("testAutomation", "manual", nil, "")

	if head1 != head2 {
		t.Errorf("initial chain head should be deterministic: got %s and %s", head1, head2)
	}
}

func TestComputeInitialChainHead_DifferentAutomations(t *testing.T) {
	head1 := ComputeInitialChainHead("automation1", "manual", nil, "")
	head2 := ComputeInitialChainHead("automation2", "manual", nil, "")

	if head1 == head2 {
		t.Error("different automations should have different chain heads")
	}
}

func TestComputeInitialChainHead_WithEvent(t *testing.T) {
	event := map[string]any{
		"topic":   "test.event",
		"payload": map[string]any{"key": "value"},
	}

	head1 := ComputeInitialChainHead("testAutomation", "event:test.event", event, "")
	head2 := ComputeInitialChainHead("testAutomation", "event:test.event", event, "")

	if head1 != head2 {
		t.Errorf("same event should produce same chain head: got %s and %s", head1, head2)
	}

	// Different event payload should produce different head
	event2 := map[string]any{
		"topic":   "test.event",
		"payload": map[string]any{"key": "different"},
	}
	head3 := ComputeInitialChainHead("testAutomation", "event:test.event", event2, "")

	if head1 == head3 {
		t.Error("different event payloads should produce different chain heads")
	}
}

func TestFingerprintInput_Deterministic(t *testing.T) {
	input := []any{
		map[string]any{"id": "node-1"},
		map[string]any{"id": "node-2"},
	}

	fp1 := FingerprintInput(input)
	fp2 := FingerprintInput(input)

	if fp1 != fp2 {
		t.Errorf("input fingerprint should be deterministic: got %s and %s", fp1, fp2)
	}
}

func TestFingerprintInput_OrderIndependent(t *testing.T) {
	input1 := []any{
		map[string]any{"id": "node-1"},
		map[string]any{"id": "node-2"},
	}
	input2 := []any{
		map[string]any{"id": "node-2"},
		map[string]any{"id": "node-1"},
	}

	fp1 := FingerprintInput(input1)
	fp2 := FingerprintInput(input2)

	if fp1 != fp2 {
		t.Errorf("input fingerprint should be order-independent: got %s and %s", fp1, fp2)
	}
}

func TestComputeChildFingerprint_Deterministic(t *testing.T) {
	result := &StepResult{
		StepId: "forEach[0]",
		Status: "success",
		Result: map[string]any{"key": "value"},
	}

	fp1 := ComputeChildFingerprint("forEach", 0, result)
	fp2 := ComputeChildFingerprint("forEach", 0, result)

	if fp1 != fp2 {
		t.Errorf("child fingerprint should be deterministic: got %s and %s", fp1, fp2)
	}

	if fp1 == "" {
		t.Error("child fingerprint should not be empty")
	}
}

func TestComputeChildFingerprint_DifferentIndex(t *testing.T) {
	result := &StepResult{
		StepId: "forEach[0]",
		Status: "success",
		Result: map[string]any{"key": "value"},
	}

	fp1 := ComputeChildFingerprint("forEach", 0, result)
	fp2 := ComputeChildFingerprint("forEach", 1, result)

	if fp1 == fp2 {
		t.Error("child fingerprints should differ for different indices")
	}
}

func TestComputeChildFingerprint_DifferentParent(t *testing.T) {
	result := &StepResult{
		StepId: "child",
		Status: "success",
	}

	fp1 := ComputeChildFingerprint("forEach1", 0, result)
	fp2 := ComputeChildFingerprint("forEach2", 0, result)

	if fp1 == fp2 {
		t.Error("child fingerprints should differ for different parent steps")
	}
}

func TestComputeChildFingerprint_NilResult(t *testing.T) {
	fp := ComputeChildFingerprint("forEach", 0, nil)
	if fp != "" {
		t.Errorf("nil result should return empty fingerprint, got %s", fp)
	}
}

func TestComputeBranchFingerprint_Deterministic(t *testing.T) {
	result := &StepResult{
		StepId: "parallel.branch1",
		Status: "success",
		Result: map[string]any{"key": "value"},
	}

	fp1 := ComputeBranchFingerprint("parallel", "branch1", result)
	fp2 := ComputeBranchFingerprint("parallel", "branch1", result)

	if fp1 != fp2 {
		t.Errorf("branch fingerprint should be deterministic: got %s and %s", fp1, fp2)
	}

	if fp1 == "" {
		t.Error("branch fingerprint should not be empty")
	}
}

func TestComputeBranchFingerprint_DifferentBranch(t *testing.T) {
	result := &StepResult{
		StepId: "parallel.branch",
		Status: "success",
	}

	fp1 := ComputeBranchFingerprint("parallel", "branch1", result)
	fp2 := ComputeBranchFingerprint("parallel", "branch2", result)

	if fp1 == fp2 {
		t.Error("branch fingerprints should differ for different branch names")
	}
}

func TestComputeBranchFingerprint_DifferentParent(t *testing.T) {
	result := &StepResult{
		StepId: "branch",
		Status: "success",
	}

	fp1 := ComputeBranchFingerprint("parallel1", "branch", result)
	fp2 := ComputeBranchFingerprint("parallel2", "branch", result)

	if fp1 == fp2 {
		t.Error("branch fingerprints should differ for different parent steps")
	}
}

func TestComputeBranchFingerprint_NilResult(t *testing.T) {
	fp := ComputeBranchFingerprint("parallel", "branch", nil)
	if fp != "" {
		t.Errorf("nil result should return empty fingerprint, got %s", fp)
	}
}

func TestAdvanceChain_Basic(t *testing.T) {
	prevHead := "prev-chain-head-123"
	contentId := "step-content-456"

	newHead := AdvanceChain(prevHead, contentId)

	if newHead == "" {
		t.Error("advanced chain head should not be empty")
	}
	if newHead == prevHead {
		t.Error("advanced chain head should differ from previous")
	}
	if newHead == contentId {
		t.Error("advanced chain head should differ from content ID")
	}
}

func TestAdvanceChain_Deterministic(t *testing.T) {
	prevHead := "prev-chain-head"
	contentId := "step-content"

	head1 := AdvanceChain(prevHead, contentId)
	head2 := AdvanceChain(prevHead, contentId)

	if head1 != head2 {
		t.Errorf("advanced chain should be deterministic: got %s and %s", head1, head2)
	}
}

func TestAdvanceChain_EmptyPrevHead(t *testing.T) {
	contentId := "step-content"

	head := AdvanceChain("", contentId)

	if head != contentId {
		t.Errorf("empty prev head should return contentId, got %s", head)
	}
}

func TestAdvanceChain_EmptyContentId(t *testing.T) {
	prevHead := "prev-chain-head"

	head := AdvanceChain(prevHead, "")

	if head != "" {
		t.Errorf("empty contentId should return empty, got %s", head)
	}
}

func TestAdvanceChain_BothEmpty(t *testing.T) {
	head := AdvanceChain("", "")

	if head != "" {
		t.Errorf("both empty should return empty, got %s", head)
	}
}

func TestAdvanceChain_OrderMatters(t *testing.T) {
	id1 := "content-a"
	id2 := "content-b"

	// Chaining in different order should produce different results
	head1 := AdvanceChain(AdvanceChain("init", id1), id2)
	head2 := AdvanceChain(AdvanceChain("init", id2), id1)

	if head1 == head2 {
		t.Error("chain order should matter")
	}
}
