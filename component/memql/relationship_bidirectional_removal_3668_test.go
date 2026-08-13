package memql

import (
	"strings"
	"testing"
)

// directionDef returns a definition that differs from a known-good one only in
// its direction, so a failure can only be attributable to the direction.
func directionDef(direction string) RelationshipDefinition {
	return RelationshipDefinition{
		Type:          "references",
		Field:         "agentId",
		TargetConcept: "v1:agents:agent",
		Direction:     direction,
	}
}

// TestBidirectionalDirectionIsRejected pins memql#3668: `bidirectional` is no
// longer a relationship direction.
//
// It was a category error rather than an unimplemented feature. `direction`
// says which side of the edge carries the pointer field -- outgoing means my
// field holds their id, incoming means their field holds mine. "Both sides
// carry it" is not a third direction, it is two relationships, which is why
// nothing implemented it consistently: `contains` and `parentOf` silently
// collapsed it to outgoing, `aliasOf` ignored it, and both id canonicalizers
// skipped the field entirely -- persisting non-canonical ids that
// (concept, id) lookups then quietly failed to find.
//
// Case variants are included because normalizeRelationshipDefinition lowercases
// before matching, so a rejection that only covered the lowercase spelling
// would leave the others accepted.
func TestBidirectionalDirectionIsRejected(t *testing.T) {
	for _, spelling := range []string{"bidirectional", "Bidirectional", "BIDIRECTIONAL", " bidirectional "} {
		t.Run(strings.TrimSpace(spelling), func(t *testing.T) {
			if _, err := normalizeRelationshipDefinition(directionDef(spelling)); err == nil {
				t.Errorf("direction %q was accepted, want a load error", spelling)
			}
		})
	}
}

// TestBidirectionalRejectionNamesTheValidDirections pins that an author who
// writes the removed value is told what to write instead, rather than only that
// what they wrote is wrong. That is what makes this break safe to take without
// a shim: the error carries the migration.
func TestBidirectionalRejectionNamesTheValidDirections(t *testing.T) {
	_, err := normalizeRelationshipDefinition(directionDef("bidirectional"))
	if err == nil {
		t.Fatal("bidirectional was accepted, want a load error")
	}
	for _, want := range []string{"outgoing", "incoming"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name the valid direction %q", err.Error(), want)
		}
	}
}

// TestValidDirectionsStillLoad is the POSITIVE CONTROL for the rejection above.
// "bidirectional is refused" is equally satisfied by an implementation that
// refuses every direction, so the two surviving values have to be asserted
// alongside it or the gate proves nothing.
func TestValidDirectionsStillLoad(t *testing.T) {
	for _, direction := range []string{"outgoing", "incoming", "Outgoing", "INCOMING"} {
		t.Run(direction, func(t *testing.T) {
			got, err := normalizeRelationshipDefinition(directionDef(direction))
			if err != nil {
				t.Fatalf("direction %q was rejected: %v", direction, err)
			}
			if want := strings.ToLower(direction); got.Direction != want {
				t.Errorf("Direction = %q, want %q", got.Direction, want)
			}
		})
	}
}

// TestContainsDirectionErrorDoesNotOfferBidirectional pins the other half of the
// removal. `contains` refuses an incoming direction, and its error named
// bidirectional as the alternative -- so after the removal that message would
// have instructed the author to write the one value guaranteed to refuse boot.
// An error that hands out a dead value is worse than no suggestion at all.
func TestContainsDirectionErrorDoesNotOfferBidirectional(t *testing.T) {
	def := directionDef("incoming")
	def.Type = "contains"

	_, err := normalizeRelationshipDefinition(def)
	if err == nil {
		t.Fatal("contains accepted an incoming direction, want a load error")
	}
	if strings.Contains(strings.ToLower(err.Error()), "bidirectional") {
		t.Errorf("error %q still offers bidirectional, which no longer loads", err.Error())
	}
	if !strings.Contains(err.Error(), "outgoing") {
		t.Errorf("error %q does not name the direction contains does accept", err.Error())
	}
}

// TestReverseDirectionCheckFiresOnMatchingDirections pins the gate that
// declaring bidirectional used to switch off: relationshipDirectionsCompatible
// returned true whenever EITHER side was bidirectional, so one such declaration
// disabled checkRelationshipDirection's reverse-consistency check for that edge.
//
// This is a regression pin, not a red-green driver -- the pairs below already
// behave this way. It exists because removal simplifies the function, and the
// consistency check it restores had no direct coverage of its own.
func TestReverseDirectionCheckFiresOnMatchingDirections(t *testing.T) {
	compatible := map[[2]string]bool{
		{relationshipDirectionOutgoing, relationshipDirectionIncoming}: true,
		{relationshipDirectionIncoming, relationshipDirectionOutgoing}: true,
		{relationshipDirectionOutgoing, relationshipDirectionOutgoing}: false,
		{relationshipDirectionIncoming, relationshipDirectionIncoming}: false,
	}

	for pair, want := range compatible {
		if got := relationshipDirectionsCompatible(pair[0], pair[1]); got != want {
			t.Errorf("relationshipDirectionsCompatible(%q, %q) = %v, want %v",
				pair[0], pair[1], got, want)
		}
	}
}
