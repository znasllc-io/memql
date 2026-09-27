package work

// validator.go -- Check a finished goal run's answer before a person looks (epic memql#5414, #5416).
//
// A STUB until its stream lands: the capability resolves and refuses by name,
// because a builtin the DSL declares with no registered executor fails every
// node's boot.

import (
	"context"
	"fmt"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

func (i *Integration) handleValidateAnswer(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error) {
	return nil, fmt.Errorf("work: validateAnswer is not wired yet")
}
