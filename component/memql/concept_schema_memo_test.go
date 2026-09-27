package memql

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// schemaMemoConcept is a concept reaching both readers: an enum field, a
// closed nested block and an open one.
func schemaMemoConcept(t *testing.T, name, extraField string) *memoryNodes.Concept {
	t.Helper()
	return mustConcept(t, `concept `+name+` {
  title    string!
  status   enum("open", "closed")
  lineage {
    runId  string!
  }
  extras   object @open {
    known  string
  }
`+extraField+`}
`, "v1/schemamemo/"+name)
}

// TestConceptSchemaDecodesOncePerDocument: the load's many questions of one
// schema decode it once, however many Concept values carry that document --
// asserted as a count of decodes.
func TestConceptSchemaDecodesOncePerDocument(t *testing.T) {
	name := "schemaMemoOnce" + strings.ReplaceAll(uniqueSuffix("c"), "-", "")
	first := schemaMemoConcept(t, name, "")
	again := schemaMemoConcept(t, name, "") // a second Concept, the same document

	before := conceptSchemaDecodes.Load()
	for _, c := range []*memoryNodes.Concept{first, again, first} {
		if _, err := flattenConceptFields(c); err != nil {
			t.Fatal(err)
		}
		if _, err := closedObjectPaths(c); err != nil {
			t.Fatal(err)
		}
	}
	if got := conceptSchemaDecodes.Load() - before; got != 1 {
		t.Errorf("six reads of one schema document decoded it %d times, want 1", got)
	}
}

// TestConceptSchemaReadsAreWhatADecodeReads: the memoized answers are the
// decode's, for every concept of the embedded tree and the fixture -- the
// memo changes when the work is done, never what it finds.
func TestConceptSchemaReadsAreWhatADecodeReads(t *testing.T) {
	if _, err := LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	concepts := append(memoryNodes.DefaultRegistry().List(), schemaMemoConcept(t, "schemaMemoEquiv", ""))
	if len(concepts) < 100 {
		t.Fatalf("%d concepts; the comparison needs the tree", len(concepts))
	}
	closedSeen := 0
	for _, c := range concepts {
		raw, err := c.DefinitionSchema()
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		wantFields, wantClosed := map[string]conceptFieldShape{}, map[string]bool{}
		collectConceptSchemaFields("", doc, wantFields)
		collectClosedObjectPaths("", doc, wantClosed)

		gotFields, err := flattenConceptFields(c)
		if err != nil || !reflect.DeepEqual(gotFields, wantFields) {
			t.Errorf("%s: flattenConceptFields = %v, %v; a decode reads %v", c.Name, gotFields, err, wantFields)
		}
		gotClosed, err := closedObjectPaths(c)
		if err != nil || !reflect.DeepEqual(gotClosed, wantClosed) {
			t.Errorf("%s: closedObjectPaths = %v, %v; a decode reads %v", c.Name, gotClosed, err, wantClosed)
		}
		closedSeen += len(wantClosed)
	}
	if closedSeen == 0 {
		t.Fatal("no concept closes a block; closedObjectPaths was compared over nothing")
	}
}

// TestConceptSchemaCallersGetTheirOwnCopy: a caller that edits what it was
// handed -- a field removed, an enum value rewritten, a path added -- cannot
// change what the next caller is told.
func TestConceptSchemaCallersGetTheirOwnCopy(t *testing.T) {
	c := schemaMemoConcept(t, "schemaMemoCopy", "")
	fields, err := flattenConceptFields(c)
	if err != nil {
		t.Fatal(err)
	}
	status := fields["status"]
	if len(status.Enum) != 2 {
		t.Fatalf("fixture: status enum = %v", status.Enum)
	}
	firstValue := status.Enum[0]
	status.Enum[0] = "edited"
	delete(fields, "title")
	closed, err := closedObjectPaths(c)
	if err != nil {
		t.Fatal(err)
	}
	if !closed["lineage"] || closed["extras"] {
		t.Fatalf("fixture: closed = %v, want lineage closed and extras open", closed)
	}
	closed["extras"] = true

	again, _ := flattenConceptFields(c)
	if _, ok := again["title"]; !ok || again["status"].Enum[0] != firstValue {
		t.Errorf("an edit by one caller reached the next: %v", again)
	}
	if closedAgain, _ := closedObjectPaths(c); closedAgain["extras"] {
		t.Error("a path one caller added reached the next")
	}
}

// TestConceptSchemaChangeIsANewDocument: the memo is keyed by the document, so
// a concept whose schema changed is read anew rather than answered from the
// old one's entry.
func TestConceptSchemaChangeIsANewDocument(t *testing.T) {
	before, _ := flattenConceptFields(schemaMemoConcept(t, "schemaMemoChange", ""))
	after, _ := flattenConceptFields(schemaMemoConcept(t, "schemaMemoChange", "  addedLater  string\n"))
	if _, ok := before["addedLater"]; ok {
		t.Fatal("fixture: the first schema already declares addedLater")
	}
	if _, ok := after["addedLater"]; !ok {
		t.Errorf("a changed schema was answered from the old document's entry: %v", after)
	}
}

// TestConceptSlicesAreTheCallersCopy: the concept slicer's memo hands each
// caller its own slices, equal to what the slicer finds.
func TestConceptSlicesAreTheCallersCopy(t *testing.T) {
	src := "/// A.\nconcept sliceMemoA {\n  x  string\n}\n\n/// B.\nconcept sliceMemoB {\n  y  string\n"
	wantSlices, wantOpen := languageParser.ExtractDeclarationSlicesReporting(src, conceptHeaderRe)
	slices, open := conceptSlicesReporting(src)
	if !reflect.DeepEqual(slices, wantSlices) || !reflect.DeepEqual(open, wantOpen) || len(slices) != 1 || len(open) != 1 {
		t.Fatalf("conceptSlicesReporting = %v, %v; the slicer finds %v, %v", slices, open, wantSlices, wantOpen)
	}
	slices[0].Name, open[0].Name = "edited", "edited"
	again, openAgain := conceptSlicesReporting(src)
	if again[0].Name != "sliceMemoA" || openAgain[0].Name != "sliceMemoB" {
		t.Errorf("an edit by one caller reached the next: %v, %v", again, openAgain)
	}
}
