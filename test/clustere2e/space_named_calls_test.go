//go:build clustere2e

// space_named_calls_test.go provides raw named-call string builders for the
// CoPresent `space` query/mutation surface.
//
// `space` and its queries/mutations (mutationCreateSpace, queryActiveSpaces,
// ...) moved OUT of the memql engine core into the CoPresent carrier pack in
// memql#2038 (epic #2031), so the engine's generated Go SDK no longer emits
// MutationCreateSpaceBuild / QueryActiveSpacesBuild. The cluster-e2e suite runs
// against the CARRIER cluster (the k3d local overlay, deploy/k8s/overlays/local),
// which DOES load the pack, so the named constructs still resolve at runtime --
// we just build the call string locally and dispatch it via QueryClient.ExecuteNamed.
package clustere2e

import "fmt"

// buildMutationCreateSpace renders a `mutationCreateSpace({...})` call string.
// status is emitted only when non-empty (mirroring the old generated builder's
// conditional omission of optional fields).
func buildMutationCreateSpace(partitionID, name, status string) string {
	if status == "" {
		return fmt.Sprintf("mutationCreateSpace({partitionId: %q, name: %q})", partitionID, name)
	}
	return fmt.Sprintf("mutationCreateSpace({partitionId: %q, name: %q, status: %q})", partitionID, name, status)
}

// buildQueryActiveSpaces renders the `queryActiveSpaces({})` call string. The
// query is self-scoped (ownerUserId==actor.userId) and declares its sort +
// paginate directives in the DSL, so the call carries no args.
func buildQueryActiveSpaces() string {
	return "queryActiveSpaces({})"
}
