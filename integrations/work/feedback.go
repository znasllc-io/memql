package work

// feedback.go -- Record a person's verdict on a run or a step version (epic memql#5414, #5416).
//
// A STUB until its stream lands: the capability resolves and refuses by name,
// because a builtin the DSL declares with no registered executor fails every
// node's boot.

import (
	"context"
	"fmt"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

func (i *Integration) handleRecordFeedback(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error) {
	return nil, fmt.Errorf("work: recordFeedback is not wired yet")
}
