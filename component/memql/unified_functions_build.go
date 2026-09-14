package memql

import (
	"fmt"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// BuildFunctionConstruct builds the function-family construct named name --
// a query, a mutation, a logic -- from the source of the .memql file that
// declares it, through the unified loader's own per-construct entry point
// (dispatchPerConstructParser: the annotation allow-list, then
// tryParseNewFunctionSyntax), exactly as LoadUnifiedFunctions builds it at
// boot. origin is the loader's spelling of the file, "unified:<path>", from
// which the file's domain is read; the grammar is edition 2026, as at boot.
//
// It exists for corpus gates outside this package that must hold a construct
// the way boot holds it -- the logic-body equivalence corpus in
// component/automations/steps runs what this returns on the LogicRunner --
// and it builds one construct at a time because such a gate rebuilds each
// construct from the source it is given.
func BuildFunctionConstruct(source, name, origin string, concepts memoryNodes.Registry) (*Function, error) {
	for _, slice := range ExtractFunctionSlices(source) {
		if slice.Name == name {
			return dispatchPerConstructParser(slice, origin, concepts)
		}
	}
	return nil, fmt.Errorf("%s declares no function construct %q", origin, name)
}
