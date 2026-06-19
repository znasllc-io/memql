package mcp

import (
	"context"
	"strings"
	"testing"
)

// TestTypedWrapperForwardsAllArgs is the #1713 arg-forwarding guard: a
// first-class @mcp-promoted query tool called by its own name (the typed
// wrapper path) must forward EVERY arg to the named-construct executor without
// dropping any -- the exact queryLibraryArtifactById/artifactId regression the
// issue reports ("required argument artifactId is missing" via the typed
// wrapper, even though it was supplied; works via run_query).
//
// The typed-wrapper dispatch (callMCPTool default case -> runNamedConstructDirect)
// must build the same json-encoded invocation run_query builds, so the engine
// binds the arg identically on both paths.
func TestTypedWrapperForwardsAllArgs(t *testing.T) {
	eng := newFakeEngine()
	eng.promotedFns = map[string]string{"queryLibraryArtifactById": "query"}

	res := callMCPTool(context.Background(), eng, "reader", TierAuthoring,
		"queryLibraryArtifactById", map[string]any{"artifactId": "abc"})
	if isErr, _ := res["isError"].(bool); isErr {
		t.Fatalf("typed wrapper returned an error result: %v", res)
	}
	if !strings.HasPrefix(eng.query, "queryLibraryArtifactById(") {
		t.Fatalf("typed wrapper did not route to the named-construct executor; query=%q", eng.query)
	}
	if !strings.Contains(eng.query, `"artifactId":"abc"`) {
		t.Fatalf("ARG DROPPED: artifactId missing from the forwarded invocation %q", eng.query)
	}
}

// TestTypedWrapperAndRunQueryForwardIdentically locks the equivalence the issue
// hinges on: the typed-wrapper path and the run_query dispatcher must produce
// the identical engine invocation for the same construct + args, so neither can
// drop an arg the other keeps.
func TestTypedWrapperAndRunQueryForwardIdentically(t *testing.T) {
	args := map[string]any{"artifactId": "abc", "lens": "x"}

	typed := newFakeEngine()
	typed.promotedFns = map[string]string{"queryLibraryArtifactById": "query"}
	callMCPTool(context.Background(), typed, "reader", TierAuthoring, "queryLibraryArtifactById", args)

	dispatch := newFakeEngine()
	callMCPTool(context.Background(), dispatch, "reader", TierAuthoring, toolRunQuery,
		map[string]any{"name": "queryLibraryArtifactById", "args": args})

	if typed.query == "" || typed.query != dispatch.query {
		t.Fatalf("typed wrapper and run_query diverged:\n  typed   =%q\n  run_query=%q", typed.query, dispatch.query)
	}
}
