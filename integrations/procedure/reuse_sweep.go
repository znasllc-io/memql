package procedure

// reuse_sweep.go -- Decide every construct's reuse label from the evidence (epic memql#5414, #5418).
//
// A STUB until its stream lands: the capability resolves and refuses by name,
// because a builtin the DSL declares with no registered executor fails every
// node's boot.

import (
	"context"
	"fmt"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

func (i *Integration) handleReuseSweep(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error) {
	return nil, fmt.Errorf("procedure: reuseSweep is not wired yet")
}
