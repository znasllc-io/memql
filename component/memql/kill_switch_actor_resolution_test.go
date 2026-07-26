package memql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// memql#2840: the computer-use kill switch selects its row by actor.userId,
// and this asserts it RESOLVES -- not merely that the source says so.
//
// A source-text assertion is not enough here, and getting that wrong is how
// this nearly shipped inert. `actor.userId` in an `update { id: ... }` slot is
// lowered to an AST node, and the parser routes BOTH `args.X` and `actor.X`
// through ArgRefExpr with the prefix as the only discriminator. The AST
// evaluator resolved the path against caller args unconditionally, so it looked
// up the literal key "actor.userId", missed, and produced an empty id -- the
// mutation then failed with "update() requires an explicit id" and the kill
// switch could not be flipped by anyone, in either direction. That is strictly
// worse than the caller-supplied-id bug it replaced, and a text-matching test
// passes happily through all of it.
//
// So: render the real shipped mutation with a populated AccessContext and
// assert the id is the actor's.
func TestToggleComputerUseEnabled_ResolvesActorId(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	conceptRegistry := concept.DefaultRegistry()
	require.NotNil(t, conceptRegistry)

	fnRegistry := newFunctionRegistry()
	_, _, err := LoadUnifiedFunctions(nil, fnRegistry, conceptRegistry)
	require.NoError(t, err)

	fn, err := fnRegistry.Get("toggleComputerUseEnabled")
	require.NoError(t, err, "toggleComputerUseEnabled must load from dsl/worker/mutations.memql")
	require.NotNil(t, fn.MutationTemplate)

	eng := &MemQLEngine{}
	const callerID = "user-caller-2840"
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{
		UserId: callerID,
		Role:   auth.RoleWriter,
	})

	// A caller-supplied userId must not influence the target: the arg is gone,
	// and undeclared args are dropped leniently on the engine path, so a stale
	// client passing someone else's id must still only reach its own row.
	node, err := eng.renderMutationTemplate(ctx, fn.MutationTemplate, map[string]any{
		"enabled": false,
		"userId":  "user-someone-else",
	})
	require.NoError(t, err, "rendering must not fail; an empty id here means the kill switch cannot be flipped at all")
	require.Equal(t, callerID, node.ID,
		"the kill switch must target the CALLER's row; %q would mean it targets someone else, and \"\" would mean it targets nobody and the switch is inert", node.ID)
}

// With no actor on the context the id must come back EMPTY rather than
// resolving to something. An empty id makes the update fail loudly ("update()
// requires an explicit id") instead of silently selecting a row -- fail-closed
// is the only acceptable direction for a security switch.
func TestToggleComputerUseEnabled_NoActorYieldsNoTarget(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	fnRegistry := newFunctionRegistry()
	_, _, err := LoadUnifiedFunctions(nil, fnRegistry, concept.DefaultRegistry())
	require.NoError(t, err)
	fn, err := fnRegistry.Get("toggleComputerUseEnabled")
	require.NoError(t, err)

	eng := &MemQLEngine{}
	node, renderErr := eng.renderMutationTemplate(context.Background(), fn.MutationTemplate, map[string]any{
		"enabled": false,
		"userId":  "user-someone-else",
	})
	if renderErr == nil {
		require.Empty(t, node.ID,
			"with no actor the target must be empty so the write fails closed; %q means an unauthenticated caller reached a row", node.ID)
	}
}
