package work

import "fmt"

// HumanWait is a durable suspension, not a tool failure or a completed answer.
// The approval and waiting run must already be committed before it is raised.
type HumanWait struct{ ApprovalID string }

func (w *HumanWait) Error() string {
	return fmt.Sprintf("waiting for an answer to approval %s", w.ApprovalID)
}
