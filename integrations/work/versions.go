package work

// versions.go -- Read every version of every step of a run (epic memql#5414, #5415).
//
// A STUB until its stream lands: the capability resolves and refuses by name,
// because a builtin the DSL declares with no registered executor fails every
// node's boot.

import (
	"context"
	"fmt"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

func (i *Integration) handleStepVersions(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error) {
	return nil, fmt.Errorf("work: stepVersions is not wired yet")
}
