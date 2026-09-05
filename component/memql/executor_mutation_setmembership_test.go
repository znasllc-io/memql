package memql

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// Set membership on the update path (memql#4951).
//
// The gap these close: `update{}` read-merges, `@appendFields` adds to an
// array, and NOTHING removed from one. So an owner-editable set had exactly
// one expressible form -- read the list, change one member, write the whole
// thing back -- which is correct for one console and carries a race nothing in
// the DSL declares: two windows changing two different members at the same
// instant clobber, and the loser is never told.
//
// The tests below are written against the two properties that make `add` and
// `remove` inverses of each other, because that is what a toggle needs and
// what append-plus-a-mirror-image would NOT have given: dedup on the way in,
// and a removal that does not care whether the member was there.

func TestAddToSet_FirstMemberStartsFresh(t *testing.T) {
	prior := map[string]any{"name": "acme"}
	partial := map[string]any{"disabledDeployables": "web", "name": "acme"}

	setMembershipFields(prior, partial, []string{"disabledDeployables"}, membershipAdd)

	require.Equal(t, []any{"web"}, partial["disabledDeployables"])
	require.Equal(t, "acme", partial["name"], "an unnamed field is untouched")
}

func TestAddToSet_UnionsWithoutClobbering(t *testing.T) {
	prior := map[string]any{"disabledDeployables": []any{"web", "docs"}}
	partial := map[string]any{"disabledDeployables": "storefront"}

	setMembershipFields(prior, partial, []string{"disabledDeployables"}, membershipAdd)

	require.Equal(t, []any{"web", "docs", "storefront"}, partial["disabledDeployables"],
		"existing members keep their positions and the new one lands at the end")
}

// THE PROPERTY @appendFields DOES NOT HAVE. Append is documented as not
// deduped, which is right for a list and wrong for a set: a toggle built on it
// yields ["web", "web"] on a double click, and one removal then leaves the
// name still present -- so the app reads "still disabled" for somebody who has
// clicked enable.
func TestAddToSet_IsDeduped(t *testing.T) {
	prior := map[string]any{"disabledDeployables": []any{"web"}}
	partial := map[string]any{"disabledDeployables": []any{"web", "docs", "web"}}

	setMembershipFields(prior, partial, []string{"disabledDeployables"}, membershipAdd)

	require.Equal(t, []any{"web", "docs"}, partial["disabledDeployables"])
}

func TestRemoveFromSet_TakesTheMemberOut(t *testing.T) {
	prior := map[string]any{"disabledDeployables": []any{"web", "docs", "storefront"}}
	partial := map[string]any{"disabledDeployables": "docs"}

	setMembershipFields(prior, partial, []string{"disabledDeployables"}, membershipRemove)

	require.Equal(t, []any{"web", "storefront"}, partial["disabledDeployables"],
		"what remains keeps its order")
}

// Idempotent by construction: two people enabling the same app both succeed,
// and a retry after a lost response is safe.
func TestRemoveFromSet_AnAbsentMemberIsANoOp(t *testing.T) {
	prior := map[string]any{"disabledDeployables": []any{"web"}}
	partial := map[string]any{"disabledDeployables": "docs"}

	setMembershipFields(prior, partial, []string{"disabledDeployables"}, membershipRemove)

	require.Equal(t, []any{"web"}, partial["disabledDeployables"])
}

func TestRemoveFromSet_EmptyingIsExpressible(t *testing.T) {
	prior := map[string]any{"disabledDeployables": []any{"web"}}
	partial := map[string]any{"disabledDeployables": "web"}

	setMembershipFields(prior, partial, []string{"disabledDeployables"}, membershipRemove)

	require.Equal(t, []any{}, partial["disabledDeployables"],
		"an empty set is a value, not an absent field")
}

// THE RACE THE PAIR EXISTS TO REMOVE. Two callers each toggling a DIFFERENT
// member, against the same stored value, both land -- which is exactly what a
// whole-list write could not do.
func TestTwoWindowsTogglingDifferentMembersBothLand(t *testing.T) {
	stored := []any{"web"}

	// Window A disables `docs`.
	priorA := map[string]any{"disabledDeployables": stored}
	partialA := map[string]any{"disabledDeployables": "docs"}
	setMembershipFields(priorA, partialA, []string{"disabledDeployables"}, membershipAdd)

	// Window B enables `web`, having read the SAME stored value A did.
	priorB := map[string]any{"disabledDeployables": partialA["disabledDeployables"]}
	partialB := map[string]any{"disabledDeployables": "web"}
	setMembershipFields(priorB, partialB, []string{"disabledDeployables"}, membershipRemove)

	require.Equal(t, []any{"docs"}, partialB["disabledDeployables"],
		"A's disable and B's enable are both present; with a whole-list write "+
			"whichever landed second would have erased the other")
}

func TestSetMembership_OmittedFieldIsUntouched(t *testing.T) {
	prior := map[string]any{"disabledDeployables": []any{"web"}}
	partial := map[string]any{"name": "acme"}

	setMembershipFields(prior, partial, []string{"disabledDeployables"}, membershipAdd)

	_, present := partial["disabledDeployables"]
	require.False(t, present,
		"a field not written stays out of the delta, so the read-merge inherits the stored array")
}

// A number and its string spelling are ONE member. A stored array arrives
// through a JSON round-trip, so comparing the Go values would keep both.
func TestSetMembership_MembersCompareByRenderedForm(t *testing.T) {
	prior := map[string]any{"ids": []any{float64(1), "2"}}
	partial := map[string]any{"ids": []any{"1", float64(3)}}

	setMembershipFields(prior, partial, []string{"ids"}, membershipAdd)

	require.Len(t, partial["ids"], 3, "1 and \"1\" are one member: %v", partial["ids"])
}

// The annotations reach the runtime node. Parsed from source, through the
// loader, through the renderer -- the same end-to-end shape
// TestMutationTemplate_MergeFieldsAnnotationPlumbing pins for @mergeFields,
// and the hop where an annotation that parses cleanly can still arrive nowhere.
func TestMutationTemplate_SetAnnotationPlumbing(t *testing.T) {
	src := `
@description("probe")
@addToSet("tags")
mutate thing probeAddTag {
  args {
    thingId string!
    tag     string!
  }
  update {
    id:   args.thingId
    tags: args.tag
  }
}
`
	registry := newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:probe:thing": {Name: "v1:probe:thing"},
	})

	fn, err := tryParseNewFunctionSyntax("probeAddTag", "mutation", src, "probe/mutations.memql", registry)
	require.NoError(t, err)
	require.NotNil(t, fn.MutationTemplate)
	require.Equal(t, []string{"tags"}, fn.MutationTemplate.AddToSetFields)
	require.Empty(t, fn.MutationTemplate.RemoveFromSetFields)

	// ...and through the renderer onto the node executeWrite consults. An
	// annotation the loader read correctly and the renderer dropped is a
	// membership change that silently becomes a whole-array replace.
	engine := &MemQLEngine{}
	mutation, err := engine.renderMutationTemplate(context.Background(), fn.MutationTemplate, map[string]any{
		"thingId": "thing-1",
		"tag":     "beta",
	})
	require.NoError(t, err)
	require.Equal(t, []string{"tags"}, mutation.AddToSet)
	require.Empty(t, mutation.RemoveFromSet)
}

// The real tree carries them, so a refactor that drops the annotation off the
// mutation fails here rather than in a browser.
func TestThePackageDeployableVerbsCarryTheirSetAnnotations(t *testing.T) {
	e := newParserTestEngine(t)

	for name, want := range map[string]struct {
		add    []string
		remove []string
	}{
		"disablePackageDeployables": {add: []string{"disabledDeployables"}},
		"enablePackageDeployables":  {remove: []string{"disabledDeployables"}},
	} {
		fn, err := e.functions.Get(name)
		require.NoError(t, err, name)
		require.NotNil(t, fn.MutationTemplate, name)
		require.Equal(t, want.add, fn.MutationTemplate.AddToSetFields, name)
		require.Equal(t, want.remove, fn.MutationTemplate.RemoveFromSetFields, name)
	}
}

// Only on an update. An insert writes the full payload, so there is no stored
// set to change and the annotation would be a silent no-op.
func TestSetAnnotationsAreRefusedOnAnInsert(t *testing.T) {
	for _, attr := range []string{languageParser.AttrAddToSet, languageParser.AttrRemoveFromSet} {
		src := `
@description("probe")
@` + attr + `("tags")
mutate thing probeInsert {
  args {
    thingId string!
    tag     string!
  }
  insert {
    id:   args.thingId
    tags: args.tag
  }
}
`
		registry := newMemoryRegistry(map[string]*memoryNodes.Concept{
			"v1:probe:thing": {Name: "v1:probe:thing"},
		})
		_, err := tryParseNewFunctionSyntax("probeInsert", "mutation", src, "probe/mutations.memql", registry)
		require.Error(t, err, attr)
		require.Contains(t, err.Error(), "only valid on update mutations", attr)
	}
}

// A field claimed by two of the array-rewriting annotations is refused at
// load. Each rewrites the same key of the write, so which one wins would be
// decided by the executor's order -- an implementation detail nobody authoring
// the mutation can see.
func TestAFieldCannotBeClaimedByTwoArrayAnnotations(t *testing.T) {
	err := validateSetMembershipFields([]string{"tags"}, []string{"tags"}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "@appendFields")
	require.Contains(t, err.Error(), "@addToSet")

	require.NoError(t, validateSetMembershipFields([]string{"a"}, []string{"b"}, []string{"c"}),
		"different fields are fine; a mutation may add to one set and remove from another")
}
