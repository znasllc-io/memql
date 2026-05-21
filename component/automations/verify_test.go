package automations

import (
	"regexp"
	"testing"

	"github.com/znasllc-io/memql/core/id"
)

func TestVerifyExecutionChain_ValidChain(t *testing.T) {
	engine := id.New()
	// Build a valid chain
	initialHead := "initial-chain-head-123"
	step1ContentId := "step1-content-id"
	step2ContentId := "step2-content-id"

	step1PrevHead := initialHead
	step1NewHead := string(engine.Combine(id.ID(step1PrevHead), id.ID(step1ContentId)))

	step2PrevHead := step1NewHead
	finalHead := string(engine.Combine(id.ID(step2PrevHead), id.ID(step2ContentId)))

	exec := &AutomationExecution{
		ID:               "exec-123",
		AutomationName:   "testAutomation",
		Status:           "completed",
		InitialChainHead: initialHead,
		ChainHead:        finalHead,
		StepOrder:        []string{"step1", "step2"},
		Steps: map[string]*StepResult{
			"step1": {
				StepId:            "step1",
				Status:            "success",
				ContentId:         step1ContentId,
				PreviousChainHead: step1PrevHead,
			},
			"step2": {
				StepId:            "step2",
				Status:            "success",
				ContentId:         step2ContentId,
				PreviousChainHead: step2PrevHead,
			},
		},
	}

	err := VerifyExecutionChain(exec)
	if err != nil {
		t.Errorf("valid chain should verify without error: %v", err)
	}
}

func TestVerifyExecutionChain_BrokenChain(t *testing.T) {
	exec := &AutomationExecution{
		ID:               "exec-123",
		AutomationName:   "testAutomation",
		Status:           "completed",
		InitialChainHead: "initial-head",
		ChainHead:        "final-head",
		StepOrder:        []string{"step1", "step2"},
		Steps: map[string]*StepResult{
			"step1": {
				StepId:            "step1",
				Status:            "success",
				ContentId:         "step1-content",
				PreviousChainHead: "initial-head",
			},
			"step2": {
				StepId:            "step2",
				Status:            "success",
				ContentId:         "step2-content",
				PreviousChainHead: "wrong-head", // This breaks the chain
			},
		},
	}

	err := VerifyExecutionChain(exec)
	if err == nil {
		t.Error("broken chain should return error")
	}
}

func TestVerifyExecutionChain_NoChainData(t *testing.T) {
	exec := &AutomationExecution{
		ID:             "exec-123",
		AutomationName: "testAutomation",
		Status:         "completed",
		Steps:          map[string]*StepResult{},
	}

	err := VerifyExecutionChain(exec)
	if err != nil {
		t.Errorf("execution without chain data should verify without error: %v", err)
	}
}

func TestVerifyExecutionChain_NilExecution(t *testing.T) {
	err := VerifyExecutionChain(nil)
	if err == nil {
		t.Error("nil execution should return error")
	}
}

func TestVerifyExecutionChain_SkippedSteps(t *testing.T) {
	engine := id.New()

	initialHead := "initial-head"
	step2ContentId := "step2-content"
	finalHead := string(engine.Combine(id.ID(initialHead), id.ID(step2ContentId)))

	exec := &AutomationExecution{
		ID:               "exec-123",
		AutomationName:   "testAutomation",
		Status:           "completed",
		InitialChainHead: initialHead,
		ChainHead:        finalHead,
		StepOrder:        []string{"step1", "step2"},
		Steps: map[string]*StepResult{
			"step1": {
				StepId: "step1",
				Status: "skipped", // Skipped steps don't advance chain
			},
			"step2": {
				StepId:            "step2",
				Status:            "success",
				ContentId:         step2ContentId,
				PreviousChainHead: initialHead, // Links to initial, not step1
			},
		},
	}

	err := VerifyExecutionChain(exec)
	if err != nil {
		t.Errorf("chain with skipped steps should verify: %v", err)
	}
}

func TestChainTrackingEnabled(t *testing.T) {
	tests := []struct {
		name     string
		exec     *AutomationExecution
		expected bool
	}{
		{
			name:     "nil execution",
			exec:     nil,
			expected: false,
		},
		{
			name: "no chain data",
			exec: &AutomationExecution{
				ID:     "exec-123",
				Status: "completed",
			},
			expected: false,
		},
		{
			name: "only initial head",
			exec: &AutomationExecution{
				ID:               "exec-123",
				Status:           "completed",
				InitialChainHead: "initial",
			},
			expected: false,
		},
		{
			name: "only chain head",
			exec: &AutomationExecution{
				ID:        "exec-123",
				Status:    "completed",
				ChainHead: "final",
			},
			expected: false,
		},
		{
			name: "both chain heads",
			exec: &AutomationExecution{
				ID:               "exec-123",
				Status:           "completed",
				InitialChainHead: "initial",
				ChainHead:        "final",
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ChainTrackingEnabled(tt.exec)
			if result != tt.expected {
				t.Errorf("ChainTrackingEnabled() = %v, want %v", result, tt.expected)
			}
		})
	}
}

// uuidv4Re matches the canonical UUIDv4 string format
// core/id.NewShortId returns. Pinned in the test so the assertion is
// tight enough to catch a regression that swaps the minter for a
// different format (per memql#103, the v1:memql:checkpoint row's
// shortId must match every other instance row in the system).
var uuidv4Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestGenerateExecutionID_Format(t *testing.T) {
	for i := 0; i < 50; i++ {
		idStr := generateExecutionId()
		if !uuidv4Re.MatchString(idStr) {
			t.Fatalf("generateExecutionId returned non-UUIDv4 string %q", idStr)
		}
	}
}

// Tests for child chain verification

func TestVerifyExecutionChain_WithSequentialChildren(t *testing.T) {
	engine := id.New()

	// Build chain with forEach that has sequential children
	initialHead := "initial-chain-head"

	// Child 0 links to parent chain, computes new chain
	child0ContentId := "child0-content"
	child0PrevHead := initialHead
	child0NewHead := string(engine.Combine(id.ID(child0PrevHead), id.ID(child0ContentId)))

	// Child 1 links to child0's advanced chain
	child1ContentId := "child1-content"
	child1PrevHead := child0NewHead
	_ = string(engine.Combine(id.ID(child1PrevHead), id.ID(child1ContentId))) // child1NewHead unused but computed for clarity

	// Parent step links results
	forEachContentId := "forEach-content"
	forEachPrevHead := initialHead
	finalHead := string(engine.Combine(id.ID(forEachPrevHead), id.ID(forEachContentId)))

	exec := &AutomationExecution{
		ID:               "exec-123",
		AutomationName:   "testAutomation",
		Status:           "completed",
		InitialChainHead: initialHead,
		ChainHead:        finalHead,
		StepOrder:        []string{"forEach"},
		Steps: map[string]*StepResult{
			"forEach": {
				StepId:            "forEach",
				Status:            "success",
				ContentId:         forEachContentId,
				PreviousChainHead: forEachPrevHead,
				Children: []*StepResult{
					{
						StepId:            "forEach[0]",
						Status:            "success",
						ContentId:         child0ContentId,
						PreviousChainHead: child0PrevHead,
					},
					{
						StepId:            "forEach[1]",
						Status:            "success",
						ContentId:         child1ContentId,
						PreviousChainHead: child1PrevHead,
					},
				},
				ChildFingerprints: []string{child0ContentId, child1ContentId},
			},
		},
	}

	err := VerifyExecutionChain(exec)
	if err != nil {
		t.Errorf("valid sequential child chain should verify: %v", err)
	}
}

func TestVerifyExecutionChain_WithParallelChildren(t *testing.T) {
	engine := id.New()

	initialHead := "initial-chain-head"

	// All parallel children link to same parent chain head
	branch1ContentId := "branch1-content"
	branch2ContentId := "branch2-content"

	parallelContentId := "parallel-content"
	parallelPrevHead := initialHead
	finalHead := string(engine.Combine(id.ID(parallelPrevHead), id.ID(parallelContentId)))

	exec := &AutomationExecution{
		ID:               "exec-123",
		AutomationName:   "testAutomation",
		Status:           "completed",
		InitialChainHead: initialHead,
		ChainHead:        finalHead,
		StepOrder:        []string{"parallel"},
		Steps: map[string]*StepResult{
			"parallel": {
				StepId:            "parallel",
				Status:            "success",
				ContentId:         parallelContentId,
				PreviousChainHead: parallelPrevHead,
				Children: []*StepResult{
					{
						StepId:            "parallel.branch1",
						Status:            "success",
						ContentId:         branch1ContentId,
						PreviousChainHead: initialHead, // Same as parent
					},
					{
						StepId:            "parallel.branch2",
						Status:            "success",
						ContentId:         branch2ContentId,
						PreviousChainHead: initialHead, // Same as parent
					},
				},
				ChildFingerprints: []string{branch1ContentId, branch2ContentId},
			},
		},
	}

	err := VerifyExecutionChain(exec)
	if err != nil {
		t.Errorf("valid parallel child chain should verify: %v", err)
	}
}

func TestVerifyExecutionChain_BrokenSequentialChildChain(t *testing.T) {
	engine := id.New()

	initialHead := "initial-chain-head"

	child0ContentId := "child0-content"
	child0PrevHead := initialHead
	// Note: child0NewHead would be computed but child1 has wrong prev
	_ = string(engine.Combine(id.ID(child0PrevHead), id.ID(child0ContentId)))

	child1ContentId := "child1-content"

	forEachContentId := "forEach-content"
	forEachPrevHead := initialHead
	finalHead := string(engine.Combine(id.ID(forEachPrevHead), id.ID(forEachContentId)))

	exec := &AutomationExecution{
		ID:               "exec-123",
		AutomationName:   "testAutomation",
		Status:           "completed",
		InitialChainHead: initialHead,
		ChainHead:        finalHead,
		StepOrder:        []string{"forEach"},
		Steps: map[string]*StepResult{
			"forEach": {
				StepId:            "forEach",
				Status:            "success",
				ContentId:         forEachContentId,
				PreviousChainHead: forEachPrevHead,
				Children: []*StepResult{
					{
						StepId:            "forEach[0]",
						Status:            "success",
						ContentId:         child0ContentId,
						PreviousChainHead: child0PrevHead,
					},
					{
						StepId:            "forEach[1]",
						Status:            "success",
						ContentId:         child1ContentId,
						PreviousChainHead: "wrong-chain-head", // This breaks sequential chain
					},
				},
			},
		},
	}

	err := VerifyExecutionChain(exec)
	if err == nil {
		t.Error("broken sequential child chain should return error")
	}
}

func TestVerifyExecutionChain_BrokenParallelChildChain(t *testing.T) {
	engine := id.New()

	initialHead := "initial-chain-head"

	branch1ContentId := "branch1-content"
	branch2ContentId := "branch2-content"

	parallelContentId := "parallel-content"
	parallelPrevHead := initialHead
	finalHead := string(engine.Combine(id.ID(parallelPrevHead), id.ID(parallelContentId)))

	exec := &AutomationExecution{
		ID:               "exec-123",
		AutomationName:   "testAutomation",
		Status:           "completed",
		InitialChainHead: initialHead,
		ChainHead:        finalHead,
		StepOrder:        []string{"parallel"},
		Steps: map[string]*StepResult{
			"parallel": {
				StepId:            "parallel",
				Status:            "success",
				ContentId:         parallelContentId,
				PreviousChainHead: parallelPrevHead,
				Children: []*StepResult{
					{
						StepId:            "parallel.branch1",
						Status:            "success",
						ContentId:         branch1ContentId,
						PreviousChainHead: initialHead,
					},
					{
						StepId:            "parallel.branch2",
						Status:            "success",
						ContentId:         branch2ContentId,
						PreviousChainHead: "different-head", // Should match branch1
					},
				},
			},
		},
	}

	err := VerifyExecutionChain(exec)
	if err == nil {
		t.Error("broken parallel child chain should return error")
	}
}

func TestDetectSequentialChain(t *testing.T) {
	tests := []struct {
		name     string
		children []*StepResult
		expected bool
	}{
		{
			name:     "empty children",
			children: []*StepResult{},
			expected: false,
		},
		{
			name: "single child",
			children: []*StepResult{
				{PreviousChainHead: "head1"},
			},
			expected: false,
		},
		{
			name: "two children same prev head (parallel)",
			children: []*StepResult{
				{PreviousChainHead: "head1"},
				{PreviousChainHead: "head1"},
			},
			expected: false,
		},
		{
			name: "two children different prev head (sequential)",
			children: []*StepResult{
				{PreviousChainHead: "head1"},
				{PreviousChainHead: "head2"},
			},
			expected: true,
		},
		{
			name: "children without chain data",
			children: []*StepResult{
				{PreviousChainHead: ""},
				{PreviousChainHead: ""},
			},
			expected: false,
		},
		{
			name: "mixed chain data",
			children: []*StepResult{
				{PreviousChainHead: "head1"},
				{PreviousChainHead: ""},
				{PreviousChainHead: "head2"},
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := detectSequentialChain(tt.children)
			if result != tt.expected {
				t.Errorf("detectSequentialChain() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestVerifySequentialChildren_ValidChain(t *testing.T) {
	// Build a valid sequential chain
	child0ContentId := "child0-content"
	child0PrevHead := "initial-head"
	child0AdvancedHead := AdvanceChain(child0PrevHead, child0ContentId)

	child1ContentId := "child1-content"
	child1PrevHead := child0AdvancedHead

	children := []*StepResult{
		{
			StepId:            "child0",
			Status:            "success",
			ContentId:         child0ContentId,
			PreviousChainHead: child0PrevHead,
		},
		{
			StepId:            "child1",
			Status:            "success",
			ContentId:         child1ContentId,
			PreviousChainHead: child1PrevHead,
		},
	}

	err := verifySequentialChildren(children)
	if err != nil {
		t.Errorf("valid sequential chain should verify: %v", err)
	}
}

func TestVerifySequentialChildren_BrokenChain(t *testing.T) {
	child0ContentId := "child0-content"
	child0PrevHead := "initial-head"
	// Note: child1 should link to advanced head but doesn't

	child1ContentId := "child1-content"

	children := []*StepResult{
		{
			StepId:            "child0",
			Status:            "success",
			ContentId:         child0ContentId,
			PreviousChainHead: child0PrevHead,
		},
		{
			StepId:            "child1",
			Status:            "success",
			ContentId:         child1ContentId,
			PreviousChainHead: "wrong-head",
		},
	}

	err := verifySequentialChildren(children)
	if err == nil {
		t.Error("broken sequential chain should return error")
	}
}

func TestVerifyParallelChildren_ValidChain(t *testing.T) {
	// All parallel children should have same prev head
	commonHead := "common-parent-head"

	children := []*StepResult{
		{
			StepId:            "branch1",
			Status:            "success",
			ContentId:         "branch1-content",
			PreviousChainHead: commonHead,
		},
		{
			StepId:            "branch2",
			Status:            "success",
			ContentId:         "branch2-content",
			PreviousChainHead: commonHead,
		},
		{
			StepId:            "branch3",
			Status:            "success",
			ContentId:         "branch3-content",
			PreviousChainHead: commonHead,
		},
	}

	err := verifyParallelChildren(children)
	if err != nil {
		t.Errorf("valid parallel chain should verify: %v", err)
	}
}

func TestVerifyParallelChildren_BrokenChain(t *testing.T) {
	children := []*StepResult{
		{
			StepId:            "branch1",
			Status:            "success",
			ContentId:         "branch1-content",
			PreviousChainHead: "common-head",
		},
		{
			StepId:            "branch2",
			Status:            "success",
			ContentId:         "branch2-content",
			PreviousChainHead: "different-head", // Should match branch1
		},
	}

	err := verifyParallelChildren(children)
	if err == nil {
		t.Error("broken parallel chain should return error")
	}
}

func TestVerifyChildChain_NestedChildren(t *testing.T) {
	// Test recursive verification with nested children
	commonHead := "parent-head"

	parent := &StepResult{
		StepId:            "parent",
		Status:            "success",
		PreviousChainHead: commonHead,
		Children: []*StepResult{
			{
				StepId:            "child1",
				Status:            "success",
				ContentId:         "child1-content",
				PreviousChainHead: commonHead,
				// Nested parallel children
				Children: []*StepResult{
					{
						StepId:            "grandchild1",
						Status:            "success",
						ContentId:         "gc1-content",
						PreviousChainHead: commonHead,
					},
					{
						StepId:            "grandchild2",
						Status:            "success",
						ContentId:         "gc2-content",
						PreviousChainHead: commonHead, // Same head = parallel
					},
				},
			},
			{
				StepId:            "child2",
				Status:            "success",
				ContentId:         "child2-content",
				PreviousChainHead: commonHead,
			},
		},
	}

	err := verifyChildChain(parent)
	if err != nil {
		t.Errorf("valid nested chain should verify: %v", err)
	}
}

func TestVerifyChildChain_EmptyChildren(t *testing.T) {
	parent := &StepResult{
		StepId:   "parent",
		Status:   "success",
		Children: []*StepResult{},
	}

	err := verifyChildChain(parent)
	if err != nil {
		t.Errorf("empty children should verify: %v", err)
	}
}

func TestVerifyChildChain_NilChildren(t *testing.T) {
	parent := &StepResult{
		StepId:   "parent",
		Status:   "success",
		Children: nil,
	}

	err := verifyChildChain(parent)
	if err != nil {
		t.Errorf("nil children should verify: %v", err)
	}
}
