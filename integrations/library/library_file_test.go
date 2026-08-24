package library

import (
	"context"
	"testing"
)

// library_file_test.go -- memql#4340.
//
// One claim, and it is the #4288 hazard arriving with a second field.
//
// touchArtifact re-versions the artifact index row after a document edit,
// and createArtifact's body is a bare `insert{}`, so the new version
// carries ONLY the fields that call names. Any field that lives on the
// artifact row and has no counterpart on the backing generatedOutput is
// therefore erased unless touchArtifact reads it back and re-sends it.
//
// memql#4288 found that with `labels` -- an edit silently wiped them.
// memql#4340 adds `archived`, the soft delete, and the same omission has
// a worse shape: it does not lose a display hint, it RESURRECTS an
// artifact the owner threw away, as a side effect of editing the document
// behind it. Nothing about the edit path would look wrong; the row would
// simply come back.
//
// So this file asserts the carry-forward on the new field directly, in
// both directions -- an archived row stays archived, an unarchived row is
// not spuriously archived -- and the sibling test in library_test.go
// (TestTouchArtifact_SkipsReStampOnReadFailure) already covers the third
// case: a genuine read failure must skip the re-stamp entirely rather
// than write the zero value over whatever the row holds.

// seededFileBackedArtifact is the index row for the stub's seeded
// document, carrying both index-only fields so a test can assert either
// one survives a re-version. archived is the caller's choice; labels are
// always present, because a test that dropped them would fail for the
// #4288 reason and obscure the #4340 one.
func seededFileBackedArtifact(archived bool) map[string]any {
	sourceRef := "v1:library:generatedOutput:doc-1"
	return map[string]any{
		"id":               stubArtifactId(sourceRef),
		"sourceConceptRef": sourceRef,
		"ownerUserId":      "user-a",
		"lens":             "artifact",
		"kind":             "generated_output",
		"source":           "agent_generated",
		"title":            "Birds",
		"labels":           []any{"reports"},
		"archived":         archived,
	}
}

// editTheDocument drives the real handler, which calls touchArtifact at
// the end of a successful edit.
func editTheDocument(t *testing.T, i *Integration) {
	t.Helper()
	if _, err := i.handleEditDocument(context.Background(), map[string]any{
		"documentId": "doc-1",
		"content":    "edited once",
		"authorKind": "user",
		"authorId":   "user-a",
	}, 0); err != nil {
		t.Fatalf("edit: %v", err)
	}
}

// TestTouchArtifact_CarriesArchivedForward is the regression that fails
// against a touchArtifact which names labels but not archived: the row is
// re-versioned without the field, so `archived` is ABSENT from the new
// version -- which reads as false everywhere, and the artifact reappears
// in the owner's Library.
func TestTouchArtifact_CarriesArchivedForward(t *testing.T) {
	eng := newStubEngine()
	eng.seedDocument(seededDoc())
	artifact := seededFileBackedArtifact(true)
	eng.seedArtifact(artifact)
	artifactId := artifact["id"].(string)

	editTheDocument(t, NewIntegration(eng))

	row, ok := eng.artifacts[artifactId]
	if !ok {
		t.Fatalf("artifact row %s is gone after a document edit", artifactId)
	}
	if _, present := row["archived"]; !present {
		t.Fatalf("archived is ABSENT from the re-versioned artifact row -- touchArtifact must "+
			"read it back and re-send it, exactly as it does for labels. createArtifact's bare "+
			"insert{} keeps only the fields the call names, so an omitted archived un-archives "+
			"a row the owner deleted (the memql#4288 hazard, arriving with the memql#4340 "+
			"field).\n  row: %v", row)
	}
	if got := boolField(row, "archived"); !got {
		t.Fatalf("archived = %v after a document edit, want true (unchanged) -- editing the "+
			"document behind an archived artifact must not bring the artifact back", got)
	}
	// The #4288 field must still survive too: a fix that swapped one
	// carried field for the other would pass the check above.
	if labels := stringSliceField(row, "labels"); len(labels) != 1 || labels[0] != "reports" {
		t.Fatalf("labels = %v after a document edit, want [reports] -- the archived carry-forward "+
			"must be an ADDITION to the label one, not a replacement", labels)
	}
}

// TestTouchArtifact_DoesNotSpuriouslyArchive is the other direction. A
// carry-forward that hardcoded `archived: true`, or that read the field
// off the wrong row, would pass the test above and silently delete every
// artifact whose document was edited.
func TestTouchArtifact_DoesNotSpuriouslyArchive(t *testing.T) {
	eng := newStubEngine()
	eng.seedDocument(seededDoc())
	artifact := seededFileBackedArtifact(false)
	eng.seedArtifact(artifact)
	artifactId := artifact["id"].(string)

	editTheDocument(t, NewIntegration(eng))

	if got := boolField(eng.artifacts[artifactId], "archived"); got {
		t.Fatalf("archived = true after editing a document whose artifact was NOT archived -- "+
			"editing must never remove an artifact from the owner's Library.\n  row: %v",
			eng.artifacts[artifactId])
	}
}

// TestTouchArtifact_CarryForwardSurvivesAnUnpromotedRow: a document with
// no artifact row yet must still edit cleanly. currentArtifactCarryForward
// reports (zero, ok=true) for "no row found", which is a legitimate empty
// carry-forward and must not be confused with the read failure the
// sibling test covers.
func TestTouchArtifact_CarryForwardSurvivesAnUnpromotedRow(t *testing.T) {
	eng := newStubEngine()
	eng.seedDocument(seededDoc())
	// No seedArtifact: the document has never been promoted.

	editTheDocument(t, NewIntegration(eng))

	if n := countCalls(eng, "createArtifact"); n != 1 {
		t.Fatalf("createArtifact called %d times for an unpromoted document, want 1 -- "+
			"'no row yet' is an empty carry-forward, not a read failure", n)
	}
}
