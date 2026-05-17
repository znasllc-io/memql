package automations

import (
	"fmt"

	"github.com/znasllc-io/memql/core/id"
)

// VerifyExecutionChain validates that an execution's chain is internally consistent,
// including nested child chains in forEach and parallel steps.
// Returns nil if the chain is valid, or an error describing the break point.
func VerifyExecutionChain(exec *AutomationExecution) error {
	if exec == nil {
		return fmt.Errorf("execution is nil")
	}

	// No chain data means nothing to verify (chain tracking was disabled)
	if exec.InitialChainHead == "" || exec.ChainHead == "" {
		return nil
	}

	if len(exec.StepOrder) == 0 {
		return fmt.Errorf("execution has chain data but no step order")
	}

	engine := id.New()
	expectedHead := id.ID(exec.InitialChainHead)

	for _, stepId := range exec.StepOrder {
		result, ok := exec.Steps[stepId]
		if !ok {
			continue // Step not in results (shouldn't happen)
		}

		// Skipped steps don't advance the chain
		if result.Status == "skipped" {
			continue
		}

		// Step predates chain tracking
		if result.PreviousChainHead == "" {
			continue
		}

		// Verify this step links to expected head
		if result.PreviousChainHead != string(expectedHead) {
			return fmt.Errorf("chain break at step %s: expected prev=%s, got prev=%s",
				stepId, expectedHead, result.PreviousChainHead)
		}

		// Verify child chains recursively
		if len(result.Children) > 0 {
			if err := verifyChildChain(result); err != nil {
				return fmt.Errorf("step %s: %w", stepId, err)
			}
		}

		// Advance expected head
		expectedHead = engine.Combine(expectedHead, id.ID(result.ContentId))
	}

	// Verify final head matches
	if string(expectedHead) != exec.ChainHead {
		return fmt.Errorf("chain head mismatch: computed=%s, stored=%s",
			expectedHead, exec.ChainHead)
	}

	return nil
}

// verifyChildChain validates chain integrity for forEach/parallel children.
func verifyChildChain(parent *StepResult) error {
	if len(parent.Children) == 0 {
		return nil
	}

	// Determine chain model from children:
	// - Sequential (forEach): each child links to previous child's advanced chain
	// - Parallel: all children link to same parent chain head
	isSequential := detectSequentialChain(parent.Children)

	if isSequential {
		return verifySequentialChildren(parent.Children)
	}
	return verifyParallelChildren(parent.Children)
}

// detectSequentialChain determines if children are sequentially chained.
// Sequential: child N-1's chain advanced → child N's PreviousChainHead
// Parallel: all children have same PreviousChainHead
func detectSequentialChain(children []*StepResult) bool {
	if len(children) < 2 {
		return false // Can't determine with < 2 children
	}

	// Find first two children with chain data
	var first, second *StepResult
	for _, child := range children {
		if child != nil && child.PreviousChainHead != "" {
			if first == nil {
				first = child
			} else {
				second = child
				break
			}
		}
	}

	if first == nil || second == nil {
		return false // Not enough chain data
	}

	// If they have different PreviousChainHead values, it's sequential
	return first.PreviousChainHead != second.PreviousChainHead
}

// verifySequentialChildren validates that forEach children chain sequentially.
func verifySequentialChildren(children []*StepResult) error {
	var prevChild *StepResult

	for i, child := range children {
		if child == nil || child.PreviousChainHead == "" {
			continue
		}

		if prevChild != nil && prevChild.ContentId != "" {
			// Current child should link to previous child's advanced chain
			expectedPrev := AdvanceChain(prevChild.PreviousChainHead, prevChild.ContentId)
			if child.PreviousChainHead != expectedPrev {
				return fmt.Errorf("sequential chain break at child %d: expected prev=%s, got=%s",
					i, expectedPrev, child.PreviousChainHead)
			}
		}

		// Recursively verify nested children
		if len(child.Children) > 0 {
			if err := verifyChildChain(child); err != nil {
				return fmt.Errorf("child %d: %w", i, err)
			}
		}

		prevChild = child
	}

	return nil
}

// verifyParallelChildren validates that parallel children all link to same chain head.
func verifyParallelChildren(children []*StepResult) error {
	var expectedHead string

	for i, child := range children {
		if child == nil || child.PreviousChainHead == "" {
			continue
		}

		if expectedHead == "" {
			expectedHead = child.PreviousChainHead
		} else if child.PreviousChainHead != expectedHead {
			return fmt.Errorf("parallel chain mismatch at child %d: expected prev=%s, got=%s",
				i, expectedHead, child.PreviousChainHead)
		}

		// Recursively verify nested children
		if len(child.Children) > 0 {
			if err := verifyChildChain(child); err != nil {
				return fmt.Errorf("child %d: %w", i, err)
			}
		}
	}

	return nil
}

// ChainTrackingEnabled checks if an execution has chain tracking data.
func ChainTrackingEnabled(exec *AutomationExecution) bool {
	if exec == nil {
		return false
	}
	return exec.InitialChainHead != "" && exec.ChainHead != ""
}
