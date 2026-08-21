package client

import "context"

// ExecuteNamed dispatches a raw named call string -- a query, mutation,
// logic or builtin in the kind-prefixed named-arg form the generated *Build
// helpers emit (`query notes()`, `mutation createNote(noteId: "...", ...)`),
// or a builtin's object form (`workbenchDispatchHost({action: "exec", ...})`)
// -- against the engine and returns the typed Result, exactly like the
// generated typed methods do under the hood.
//
// It is the supported escape hatch for callers that need to invoke a named
// construct WITHOUT a generated *Build helper -- most notably constructs that
// live OUTSIDE this engine's DSL tree, such as a product pack's queries and
// mutations (`createSpace`, once `mutationCreateSpace`, moved to the carrier
// pack with the space concept in memql#2038). The pack's runtime resolves the
// named call; only the engine-side generated Go binding is absent. Nothing
// here checks that the name exists: a cluster that has not loaded the pack
// refuses the call at execute time, which is what took the cluster-e2e suite
// down in memql#4212. `name` is used solely for error wrapping.
func (qc *QueryClient) ExecuteNamed(ctx context.Context, name, call string) (*Result, error) {
	return qc.executeNamed(ctx, name, call)
}
