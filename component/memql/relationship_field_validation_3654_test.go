package memql

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// TestOutgoingRelationshipFieldMustExistOnTheConcept pins the first half of
// memql#3654.
//
// Nothing checked that a relationship's `field` was a field the concept
// actually declares. deriveRelationshipFieldSource only asks whether the first
// segment is a RESERVED name, to choose table-vs-payload source; it never
// consults the concept's own fields. So a typo loaded clean, and the write-path
// canonicalizer then did an exact `payload[field]` lookup, missed, and
// continued -- the same silent-corruption shape as memql#3653, and the likeliest
// authoring mistake there is.
func TestOutgoingRelationshipFieldMustExistOnTheConcept(t *testing.T) {
	// `assigneId` is a typo; the declared field is `assigneeId`.
	ticket := parseFixtureConcept(t, "v1/acme/ticket", `
@description("A ticket assigned to somebody.")
concept ticket {
  assigneeId  string  @description("Who it is assigned to.")

  @relationship(type="references", field="assigneId", target="v1:acme:user", direction="outgoing")
}
`)
	user := parseFixtureConcept(t, "v1/acme/user", `
@description("Somebody a ticket can be assigned to.")
concept user {
  name  string  @description("Display name.")
}
`)

	err := newQuietEngine(t).Init(withFixtureConcepts(t, ticket, user))
	if err == nil {
		t.Fatal("Init accepted a relationship whose field the concept does not declare")
	}
	for _, want := range []string{"v1:acme:ticket", "assigneId"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
}

// TestIncomingRelationshipFieldLivesOnTheTarget is the constraint that makes
// the check above direction-AWARE rather than simply wrong.
//
// For an incoming edge the foreign key lives on the FAR side, so `field` names
// a column on the target concept, not on the declaring one. The live fixture in
// relationship_incoming_target_3432_db_test.go does exactly this: `hub`
// declares field="hubId" where hubId is a field on `spoke`. A naive
// "field must be declared here" rule makes every incoming relationship illegal.
func TestIncomingRelationshipFieldLivesOnTheTarget(t *testing.T) {
	hub := parseFixtureConcept(t, "v1/acme/hub", `
@description("A hub other rows point at.")
concept hub {
  name  string  @description("Hub name.")

  @relationship(type="parent", field="hubId", target="v1:acme:spoke", direction="incoming")
}
`)
	spoke := parseFixtureConcept(t, "v1/acme/spoke", `
@description("A spoke pointing at its hub.")
concept spoke {
  hubId  string  @description("FK to the owning hub.")
}
`)

	if err := newQuietEngine(t).Init(withFixtureConcepts(t, hub, spoke)); err != nil {
		t.Fatalf("Init rejected a valid incoming relationship whose field lives on the target: %v", err)
	}
}

// TestIncomingRelationshipFieldMissingOnTargetFails is the negative half of the
// direction-aware rule: an incoming edge naming a field the TARGET does not
// declare is still a typo and must still fail.
func TestIncomingRelationshipFieldMissingOnTargetFails(t *testing.T) {
	hub := parseFixtureConcept(t, "v1/acme/hub", `
@description("A hub other rows point at.")
concept hub {
  name  string  @description("Hub name.")

  @relationship(type="parent", field="hubbId", target="v1:acme:spoke", direction="incoming")
}
`)
	spoke := parseFixtureConcept(t, "v1/acme/spoke", `
@description("A spoke pointing at its hub.")
concept spoke {
  hubId  string  @description("FK to the owning hub.")
}
`)

	if err := newQuietEngine(t).Init(withFixtureConcepts(t, hub, spoke)); err == nil {
		t.Fatal("Init accepted an incoming relationship whose field the target does not declare")
	}
}

// TestRelationshipFieldMatchingIsCaseSensitive pins the second half of
// memql#3654.
//
// The write path did an exact `payload[field]` lookup while the read path used
// strings.EqualFold, so a case-mismatched field canonicalized on filter but not
// on write. An edge that half-works is worse than one that does not work at
// all: writes land non-canonical while reads look correct. Exact match is
// already the de-facto contract everywhere else (write path and traversal both
// use exact lookups), so the mismatch is settled by rejecting it at load.
func TestRelationshipFieldMatchingIsCaseSensitive(t *testing.T) {
	ticket := parseFixtureConcept(t, "v1/acme/ticket", `
@description("A ticket assigned to somebody.")
concept ticket {
  assigneeId  string  @description("Who it is assigned to.")

  @relationship(type="references", field="AssigneeId", target="v1:acme:user", direction="outgoing")
}
`)
	user := parseFixtureConcept(t, "v1/acme/user", `
@description("Somebody a ticket can be assigned to.")
concept user {
  name  string  @description("Display name.")
}
`)

	if err := newQuietEngine(t).Init(withFixtureConcepts(t, ticket, user)); err == nil {
		t.Fatal("Init accepted a case-mismatched relationship field, which half-works at runtime")
	}
}

// TestDottedRelationshipFieldValidatesRootSegment guards a live pattern:
// @relationship binds top-level fields, and the conformance exemption table
// documents real nested sub-field cases. The check must validate the ROOT
// segment of a dotted path, not the whole path.
func TestDottedRelationshipFieldValidatesRootSegment(t *testing.T) {
	ticket := parseFixtureConcept(t, "v1/acme/ticket", `
@description("A ticket carrying a nested source object.")
concept ticket {
  source  object  @description("Provenance of the ticket.")

  @relationship(type="references", field="source.agentId", target="v1:acme:user", direction="outgoing")
}
`)
	user := parseFixtureConcept(t, "v1/acme/user", `
@description("Somebody a ticket can reference.")
concept user {
  name  string  @description("Display name.")
}
`)

	if err := newQuietEngine(t).Init(withFixtureConcepts(t, ticket, user)); err != nil {
		t.Fatalf("Init rejected a dotted field whose root segment is declared: %v", err)
	}
}

// TestShippedTreeDeclaresEveryRelationshipField sweeps the whole embedded tree
// and reports EVERY relationship whose field the owning concept does not
// declare, rather than stopping at the first.
//
// It exists because the load gate is fail-fast: without this, fixing the corpus
// means re-running Init once per violation. It also stays useful afterwards as
// a corpus-level guard with a readable failure list.
func TestShippedTreeDeclaresEveryRelationshipField(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	all := concept.DefaultRegistry().List()

	byName := make(map[string]*concept.Concept, len(all))
	for _, c := range all {
		if c != nil {
			byName[c.Name] = c
		}
	}

	var violations []string
	for _, c := range all {
		if c == nil {
			continue
		}
		for idx, rel := range c.Relationships {
			normalized, err := normalizeRelationshipDefinition(rel)
			if err != nil {
				continue // a different gate's problem
			}
			if err := checkRelationshipFieldDeclared(normalized, c, byName[normalized.TargetConcept]); err != nil {
				violations = append(violations, fmt.Sprintf("%s relationship[%d]: %v", c.Name, idx, err))
			}
		}
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Errorf("%d relationship(s) name a field the owning concept does not declare:\n  %s",
			len(violations), strings.Join(violations, "\n  "))
	}
}
