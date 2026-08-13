package memql

import (
	"strings"
	"testing"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// parseFixtureConcept builds a Concept from real .memql source, so the fixture
// carries a valid definition schema and exercises the same construction path
// the loader uses. Hand-built Concept literals skip schema derivation and fail
// Init long before the relationship loop is reached.
func parseFixtureConcept(t *testing.T, dirPath, src string) *concept.Concept {
	t.Helper()
	c, err := concept.ParseConceptMemQL([]byte(src), dirPath)
	if err != nil {
		t.Fatalf("ParseConceptMemQL(%s): %v", dirPath, err)
	}
	return c
}

// withFixtureConcepts loads the real embedded tree and ADDS the given fixture
// concepts to it, restoring the registry afterwards.
//
// Adding rather than replacing is load-bearing. Init does far more than the
// relationship loop -- it loads every function, spec and automation in the
// embedded tree, and each resolves against the concept registry. A registry
// holding only a fixture orphans ~490 real constructs and strict boot refuses
// for that reason instead of the one under test.
//
// The registry is a package-level singleton and Init MUTATES it in place
// (engine_bootstrap.go writes normalized definitions back onto the shared
// *Concept pointers), so the snapshot/restore is what keeps this test from
// corrupting every test that runs after it in the same process.
func withFixtureConcepts(t *testing.T, fixtures ...*concept.Concept) concept.Registry {
	t.Helper()

	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	before := concept.All()
	t.Cleanup(func() { concept.ReplaceAll(before) })

	add := make(map[string]*concept.Concept, len(fixtures))
	for _, c := range fixtures {
		add[c.Name] = c
	}
	concept.MergeAll(add)

	return concept.DefaultRegistry()
}

// TestUnresolvableRelationshipTargetFailsTheLoad pins memql#3653.
//
// A relationship whose target is not a registered concept was SKIPPED with a
// Debug log, on the strength of a comment about `@visibility` filtering -- a
// feature that no longer exists (zero uses in dsl/; unified_loader.go states
// every binary loads every concept). With it gone, a target missing from the
// registry means a typo or a missing `use` import, and nothing else.
//
// The consequence is not a missing edge, it is silent data corruption: the
// relationship is what drives id canonicalization on both the write path
// (partition_context.go) and the filter path (executor_filter.go). No
// relationship means ids persist non-canonical, which means (concept, id)
// lookups quietly return nothing, with no error anywhere.
func TestUnresolvableRelationshipTargetFailsTheLoad(t *testing.T) {
	// A typo in the target: the concept is `user`, not `usr`.
	ticket := parseFixtureConcept(t, "v1/acme/ticket", `
@description("A ticket assigned to somebody.")
concept ticket {
  assigneeId  string  @description("Who it is assigned to.")

  @relationship(type="references", field="assigneeId", target="v1:acme:usr", direction="outgoing")
}
`)

	registry := withFixtureConcepts(t, ticket)

	err := newQuietEngine(t).Init(registry)
	if err == nil {
		t.Fatal("Init accepted a relationship whose target is not registered, want a load error")
	}

	msg := err.Error()
	for _, want := range []string{"v1:acme:ticket", "assigneeId", "v1:acme:usr"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q -- the message must be attributable", msg, want)
		}
	}
}

// TestResolvableRelationshipTargetStillLoads is the positive control. Without
// it, a change that made Init reject everything would satisfy the test above.
func TestResolvableRelationshipTargetStillLoads(t *testing.T) {
	ticket := parseFixtureConcept(t, "v1/acme/ticket", `
@description("A ticket assigned to somebody.")
concept ticket {
  assigneeId  string  @description("Who it is assigned to.")

  @relationship(type="references", field="assigneeId", target="v1:acme:user", direction="outgoing")
}
`)
	user := parseFixtureConcept(t, "v1/acme/user", `
@description("Somebody a ticket can be assigned to.")
concept user {
  name  string  @description("Display name.")
}
`)

	registry := withFixtureConcepts(t, ticket, user)

	if err := newQuietEngine(t).Init(registry); err != nil {
		t.Fatalf("Init rejected a well-formed relationship: %v", err)
	}
}

// TestUnresolvableTargetSuggestsTheLikelyConcept pins that a near-miss target
// gets a "did you mean" pointing at the real concept.
//
// The typo case is overwhelmingly the common one, and the author hitting it may
// be in a product repo with no visibility into which concept ids this engine
// actually registered. Naming the near neighbour turns a lookup into a fix.
func TestUnresolvableTargetSuggestsTheLikelyConcept(t *testing.T) {
	ticket := parseFixtureConcept(t, "v1/acme/ticket", `
@description("A ticket assigned to somebody.")
concept ticket {
  assigneeId  string  @description("Who it is assigned to.")

  @relationship(type="references", field="assigneeId", target="v1:acme:usr", direction="outgoing")
}
`)
	// Registered, so the typo above has an obvious intended neighbour.
	user := parseFixtureConcept(t, "v1/acme/user", `
@description("Somebody a ticket can be assigned to.")
concept user {
  name  string  @description("Display name.")
}
`)

	err := newQuietEngine(t).Init(withFixtureConcepts(t, ticket, user))
	if err == nil {
		t.Fatal("Init accepted an unregistered target, want a load error")
	}

	msg := err.Error()
	if !strings.Contains(msg, "v1:acme:user") {
		t.Errorf("error %q does not suggest the near-miss concept %q", msg, "v1:acme:user")
	}
	if !strings.Contains(strings.ToLower(msg), "did you mean") {
		t.Errorf("error %q carries no did-you-mean hint", msg)
	}
}
