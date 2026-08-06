package baseparser

import (
	"strings"
	"testing"
)

// TestQueryRejectsRowAnnotationWithPointedHint locks in memql#2779: `@row` on
// a query is the mistake an author makes on the way to filtering by row id,
// because `@row` IS how a shape opts into the row surface. The generic
// unknown-annotation error listed the allow-list but never said where `@row`
// belongs or what to write instead, so the author had no path forward.
func TestQueryRejectsRowAnnotationWithPointedHint(t *testing.T) {
	source := `@row
query getClusterById {
  args { clusterId string! }
  filter row.id == args.clusterId
}`

	allowed := map[string]bool{"description": true, "enabled": true, "actor": true}
	err := ValidateConstructAnnotations(source, "query", allowed)
	if err == nil {
		t.Fatal("ValidateConstructAnnotations = nil, want @row rejected on a query")
	}

	msg := err.Error()
	for _, want := range []string{
		"@row",      // names the offending annotation
		"SHAPE",     // says where it belongs
		"signature", // says why a query does not need it
		"row.id",    // says what to write instead
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error must mention %q to be actionable, got: %s", want, msg)
		}
	}
	if strings.Contains(msg, "unknown query annotation") {
		t.Errorf("expected the pointed hint, got the generic unknown-annotation error: %s", msg)
	}
}

// TestAllowedAnnotationUnaffected -- the hint is consulted only AFTER the
// allow-list rejects a name, so a kind that legitimately accepts it (a shape
// accepting @row) must be untouched.
func TestAllowedAnnotationUnaffected(t *testing.T) {
	source := `@row
shape spaceCard {
  row.id
}`
	allowed := map[string]bool{"description": true, "row": true, "actor": true}
	if err := ValidateConstructAnnotations(source, "shape", allowed); err != nil {
		t.Fatalf("@row on a shape must stay valid, got: %v", err)
	}
}

// TestUnrelatedUnknownAnnotationKeepsGenericError guards against the hint
// swallowing the normal path: an annotation with no misplacement entry still
// gets the allow-list error.
func TestUnrelatedUnknownAnnotationKeepsGenericError(t *testing.T) {
	source := `@bogusThing
query q { }`
	allowed := map[string]bool{"description": true, "enabled": true}
	err := ValidateConstructAnnotations(source, "query", allowed)
	if err == nil {
		t.Fatal("expected @bogusThing to be rejected")
	}
	if !strings.Contains(err.Error(), "unknown query annotation @bogusThing") {
		t.Errorf("expected the generic allow-list error, got: %v", err)
	}
}
