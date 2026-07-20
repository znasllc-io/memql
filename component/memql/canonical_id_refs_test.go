package memql

import (
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

func cidResolver(t *testing.T) *ConceptResolver {
	t.Helper()
	registry := &memoryNodes.MemoryRegistry{}
	registry.ReplaceAll(map[string]*memoryNodes.Concept{
		"v1:cognition:space": {Name: "v1:cognition:space"},
		"v1:identity:user":   {Name: "v1:identity:user"},
		"v1:agents:agent":    {Name: "v1:agents:agent"},
	})
	return NewConceptResolver(registry)
}

// TestResolveCanonicalIdConceptRefs_Typed locks in #987: the typed
// `canonicalId(arg, <importedConcept>)` form resolves the short-name against the
// file's `use ...concepts.{ ... }` imports + the registry and rewrites to the
// canonical-id string form.
func TestResolveCanonicalIdConceptRefs_Typed(t *testing.T) {
	src := `use cognition.concepts.{ space }
use identity.concepts.{ user }

mutate participant joinSpace {
  insert {
    id: concat("participant-", hash(concat(canonicalId(args.partitionId, space), ":", canonicalId(args.userId, user))))
    partitionId: canonicalId(args.partitionId, space)
  }
}`

	got, err := cidResolver(t).ResolveCanonicalIdConceptRefs(src)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !strings.Contains(got, `canonicalId(args.partitionId, "v1:cognition:space")`) {
		t.Fatalf("space ref not resolved to the canonical-id string form:\n%s", got)
	}
	if !strings.Contains(got, `canonicalId(args.userId, "v1:identity:user")`) {
		t.Fatalf("user ref not resolved:\n%s", got)
	}
	if strings.Contains(got, "canonicalId(args.partitionId, space)") {
		t.Fatalf("typed form should have been rewritten away:\n%s", got)
	}
}

// TestResolveCanonicalIdConceptRefs_StringFormUntouched proves the change is
// additive: the existing quoted string form passes through unchanged.
func TestResolveCanonicalIdConceptRefs_StringFormUntouched(t *testing.T) {
	src := `filter  payload.partitionId==canonicalId(args.partitionId, "v1:cognition:space") && isActiveRecord`
	got, err := cidResolver(t).ResolveCanonicalIdConceptRefs(src)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != src {
		t.Fatalf("string form must be untouched\n got:  %s\n want: %s", got, src)
	}
}

// TestResolveCanonicalIdConceptRefs_Unimported errors when the concept name is
// neither imported nor a same-domain ambient name (#2617) -- the load-time
// type check stands.
func TestResolveCanonicalIdConceptRefs_Unimported(t *testing.T) {
	src := `use cognition.concepts.{ space }
mutate x y { insert { id: canonicalId(args.id, widget) } }`
	_, err := cidResolver(t).ResolveCanonicalIdConceptRefs(src)
	if err == nil {
		t.Fatalf("expected an error for the unimported concept %q", "widget")
	}
	if !strings.Contains(err.Error(), "widget") || !strings.Contains(err.Error(), "neither imported nor a same-domain concept") {
		t.Fatalf("expected a not-in-scope concept error, got: %v", err)
	}
}

// TestResolveCanonicalIdConceptRefs_SkipsStringLiteralProse guards against
// corrupting `canonicalId(...)` text that appears inside a string literal
// (e.g. an @description).
func TestResolveCanonicalIdConceptRefs_SkipsStringLiteralProse(t *testing.T) {
	src := `use cognition.concepts.{ space }
@description("derive the id via canonicalId(args.partitionId, space) and hash it")
query space q { filter id==args.x }`
	got, err := cidResolver(t).ResolveCanonicalIdConceptRefs(src)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != src {
		t.Fatalf("canonicalId inside a string literal must be left untouched\n got:  %s", got)
	}
}
