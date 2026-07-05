package client

import "context"

// ExecuteNamed dispatches a raw named-query / named-mutation call string
// (e.g. `queryActiveSpaces({})` or `mutationCreateSpace({...})`) against the
// engine and returns the typed Result, exactly like the generated typed
// methods do under the hood.
//
// It is the supported escape hatch for callers that need to invoke a named
// construct WITHOUT a generated *Build helper -- most notably constructs that
// live OUTSIDE this engine's DSL tree (e.g. product-pack-owned queries /
// mutations such as `mutationCreateSpace`, which moved to the carrier pack in
// memql#2038). The carrier runtime still resolves the named call; only the
// engine-side generated Go binding is absent. `name` is used solely for error
// wrapping.
func (qc *QueryClient) ExecuteNamed(ctx context.Context, name, call string) (*Result, error) {
	return qc.executeNamed(ctx, name, call)
}
