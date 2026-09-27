package work

// head.go -- Move a run's head to an earlier version of a step (epic memql#5414, #5415).
//
// A STUB until its stream lands: the capability resolves and refuses by name,
// because a builtin the DSL declares with no registered executor fails every
// node's boot.

import (
	"context"
	"fmt"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

func (i *Integration) handleMoveRunHead(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error) {
	return nil, fmt.Errorf("work: moveRunHead is not wired yet")
}
