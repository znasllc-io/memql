//go:build clustere2e

// space_named_calls_test.go provides raw named-call string builders for the
// product pack's `space` query/mutation surface.
//
// `space` and its queries/mutations (createSpace, queryActiveSpaces,
// ...) moved OUT of the memql engine core into the product carrier pack in
// memql#2038 (epic #2031), so the engine's generated Go SDK no longer emits
// CreateSpaceBuild / QueryActiveSpacesBuild. Kind-prefix drop (#2853) renamed
// mutationCreateSpace -> createSpace; the engine comment in
// dsl/cognition/mutations.memql is the surviving pointer. The cluster-e2e
// suite runs against the CARRIER cluster (the k3d local overlay), which DOES
// load the pack, so the named constructs still resolve at runtime -- we just
// build the call string locally and dispatch it via QueryClient.ExecuteNamed.
package clustere2e

import "fmt"

// buildCreateSpace renders a `createSpace(...)` call string -- the successor
// of the retired mutationCreateSpace name (memql#4212). status is emitted only
// when non-empty (mirroring the old generated builder's conditional omission
// of optional fields). Args match the last engine-side signature (partitionId,
// name, status) that the pack inherited.
func buildCreateSpace(partitionID, name, status string) string {
	if status == "" {
		return fmt.Sprintf("createSpace(partitionId: %q, name: %q)", partitionID, name)
	}
	return fmt.Sprintf("createSpace(partitionId: %q, name: %q, status: %q)", partitionID, name, status)
}

// buildQueryActiveSpaces renders the `queryActiveSpaces()` call string. The
// query is self-scoped (ownerUserId==actor.userId) and declares its sort +
// paginate directives in the DSL, so the call carries no args.
func buildQueryActiveSpaces() string {
	return "queryActiveSpaces()"
}
