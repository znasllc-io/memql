package packages

import (
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// The process-wide concept registry -- the one every live read consults for
// schemas and row-authz tiers. Kept in its own file so the import that reaches
// it is visible: the analysis under test is supposed to leave it alone, and a
// test that could not see it could not say so.

func conceptRegistryCount() int {
	return len(memoryNodes.DefaultRegistry().List())
}

// conceptRegistryHas reports whether a canonical id resolves in the process
// registry. The leak this guards against is a candidate package's concept
// still being resolvable after an analysis pass returned.
func conceptRegistryHas(id string) bool {
	c, err := memoryNodes.DefaultRegistry().Get(id)
	return err == nil && c != nil
}
