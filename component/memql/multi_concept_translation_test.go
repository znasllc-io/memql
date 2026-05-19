package memql

import (
	"strings"
	"testing"
)

// extractAllUseConceptNames + extractAllUseShapeNames return every
// distinct bareName declared in the source. Used by the loader's
// per-construct payload translation pass that runs after the
// rewriter to handle multi-construct files (Phase 2 of the
// import-model refactor).

func TestExtractAllUseConceptNames_MultipleDistinct(t *testing.T) {
	source := `@useConcept(participant)
query queryActiveParticipants { filter participant.spaceId == args.spaceId; shape participantFull }

@useConcept(space)
query queryActiveSpaces { filter space.ownerId == args.userId; shape spaceFull }`

	got := extractAllUseConceptNames(source)
	want := []string{"participant", "space"}
	if !equalSlices(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExtractAllUseConceptNames_DuplicateCollapsed(t *testing.T) {
	// Two constructs bound to the same concept -- only one entry
	// in the output so the translation loop doesn't try to rewrite
	// twice.
	source := `@useConcept(space)
query queryA { filter space.id == args.id; shape spaceFull }

@useConcept(space)
query queryB { filter space.ownerId == args.userId; shape spaceFull }`

	got := extractAllUseConceptNames(source)
	if len(got) != 1 || got[0] != "space" {
		t.Errorf("got %v, want [space]", got)
	}
}

func TestExtractAllUseConceptNames_Empty(t *testing.T) {
	got := extractAllUseConceptNames(`query queryNoUseConcept { filter id == args.id; shape fooFull }`)
	if got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

// TestTranslateConceptPathsToPayload_MultiConceptFile is the
// end-to-end property the loader fix guarantees. A file containing
// two queries bound to different concepts must have BOTH concepts'
// payload references translated, not just the first.
func TestTranslateConceptPathsToPayload_MultiConceptFile(t *testing.T) {
	source := `@useConcept(participant)
query queryA { filter participant.spaceId == args.spaceId; shape participantFull }

@useConcept(space)
query queryB { filter space.ownerId == args.userId; shape spaceFull }`

	out := source
	for _, name := range extractAllUseConceptNames(out) {
		out = translateConceptPathsToPayload(out, name)
	}

	if strings.Contains(out, "participant.spaceId") {
		t.Errorf("participant.spaceId should have been translated to payload.spaceId, got:\n%s", out)
	}
	if strings.Contains(out, "space.ownerId") {
		t.Errorf("space.ownerId should have been translated to payload.ownerId, got:\n%s", out)
	}
	if !strings.Contains(out, "payload.spaceId") {
		t.Errorf("expected payload.spaceId in output, got:\n%s", out)
	}
	if !strings.Contains(out, "payload.ownerId") {
		t.Errorf("expected payload.ownerId in output, got:\n%s", out)
	}
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
