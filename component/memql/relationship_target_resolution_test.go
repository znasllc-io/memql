package memql

import (
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// TestRelationshipTargetsResolveToKnownConcepts pins memql#1067: every
// @relationship target authored as a `use`-imported bare concept name must be
// resolved by LoadUnifiedConcepts to the target's canonical id, and that id
// must be a registered concept. A bare name that the resolver could not map
// (missing `use` import, typo) would survive as a non-canonical string and is
// caught here -- the same failure the engine bootstrap would otherwise only
// warn about at startup.
func TestRelationshipTargetsResolveToKnownConcepts(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if _, err := LoadUnifiedConcepts(logger); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}

	reg := memoryNodes.DefaultRegistry()
	all := reg.List()
	require.NotEmpty(t, all, "expected concepts to load from the unified tree")

	checked := 0
	for _, c := range all {
		if c == nil {
			continue
		}
		id := c.Name
		for _, rel := range c.Relationships {
			checked++
			require.NotEmptyf(t, rel.TargetConcept,
				"concept %q relationship on field %q has an empty target", id, rel.Field)
			// A resolved target is a canonical id (v1:<ns>:<name>); a bare
			// name that failed to resolve has no ':' and would be unknown.
			require.Containsf(t, rel.TargetConcept, ":",
				"concept %q relationship target %q did not resolve to a canonical id -- "+
					"add a `use <ns>.concepts.{ %s }` import (memql#1067)",
				id, rel.TargetConcept, rel.TargetConcept)
			_, getErr := reg.Get(rel.TargetConcept)
			require.NoErrorf(t, getErr,
				"concept %q relationship target %q is not a registered concept", id, rel.TargetConcept)
		}
	}
	require.Positive(t, checked, "expected at least one relationship to validate")
}

// TestRelationshipResolverHandlesSameAndCrossNamespace is a focused unit test
// of the resolver: a same-namespace bare target resolves from the owning
// file's own concepts (no import), a cross-namespace bare target resolves
// through a `use` import, and an already-canonical target is left untouched.
func TestRelationshipResolverHandlesSameAndCrossNamespace(t *testing.T) {
	index := map[string]map[string]string{
		"cognition": {"space": "v1:cognition:space", "state": "v1:cognition:turn:state"},
		"identity":  {"user": "v1:identity:user"},
	}
	uses, err := parsedUseDeclarations("use identity.concepts.{ user }\n")
	require.NoError(t, err)
	require.NotEmpty(t, uses)

	// same-namespace (dir=cognition): bare name resolves with no import.
	require.Equal(t, "v1:cognition:space",
		resolveRelationshipTargetName("space", "cognition", nil, index))
	// sub-namespaced concept in the same file still resolves by bare name.
	require.Equal(t, "v1:cognition:turn:state",
		resolveRelationshipTargetName("state", "cognition", nil, index))
	// cross-namespace: resolves only through the use import.
	require.Equal(t, "v1:identity:user",
		resolveRelationshipTargetName("user", "agents", uses, index))
	// cross-namespace without the import: unresolved.
	require.Equal(t, "",
		resolveRelationshipTargetName("user", "agents", nil, index))
	// already-canonical input is not a bare name; resolver only sees bare
	// names (the caller skips ':'-bearing targets), but confirm the bare
	// lookup of an unknown name returns "".
	require.Equal(t, "",
		resolveRelationshipTargetName("doesNotExist", "cognition", uses, index))
}
