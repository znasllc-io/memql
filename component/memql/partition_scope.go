package memql

import "context"

// DefaultPartition is the fallback tenant scope when a request carries no
// explicit partition. Matches the engine default set in SetPartition.
const DefaultPartition = "default"

// PartitionScope returns the canonical tenant scope for a request: the
// partition carried on the context, or DefaultPartition when none is set.
//
// This is the single sanctioned way for core (engine + services) to scope
// work to a tenant. `partition` is the canonical tenant/scope primitive;
// product-level notions like `partitionId` are NOT scope keys -- a `space`
// is scoped *by* a partition, never the other way around. New core code must
// scope through this helper (or the engine's ResolvePartitionFromContext,
// which additionally honors the engine default), never by threading a
// product id. See docs/public/concepts/partition-scoping.md.
//
// Unlike (*MemQLEngine).ResolvePartitionFromContext this needs no engine
// state, so any core package can scope a context without reaching the engine.
func PartitionScope(ctx context.Context) string {
	if ctx != nil {
		if p := PartitionFromContext(ctx); p != "" {
			return p
		}
	}
	return DefaultPartition
}
