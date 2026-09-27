package memql

import (
	"testing"

	"github.com/znasllc-io/memql/component/memql/sense"
)

// TestSenseCompletionWireCarriesAdditionalEdits (memql#5426): a completion
// that makes an edit elsewhere in the document -- a concept import, whose
// label promises the `use` line an edit with the file's imports adds -- is
// offered on the wire WITH that edit (SenseCompletionItem.additional_edits),
// where it used to be dropped because the message had no field for it
// (memql#5359). Every other item passes through unchanged, with no edits.
func TestSenseCompletionWireCarriesAdditionalEdits(t *testing.T) {
	at := sense.Position{Line: 3, Column: 1}
	got := senseCompletionItemsToProto([]sense.CompletionItem{
		{Label: "folder", Kind: "concept", InsertText: "folder", SortPriority: 1},
		{Label: "use library.concepts.{ folder }", Kind: "snippet", InsertText: "folder", SortPriority: 2,
			AdditionalEdits: []sense.TextEdit{{Range: sense.Range{Start: at, End: at}, NewText: "use library.concepts.{ folder }\n"}}},
		{Label: "args { ... }", Kind: "snippet", InsertText: "args {\n\t$0\n}", IsSnippet: true, SortPriority: 1},
	})
	if len(got) != 3 {
		t.Fatalf("got %d items, want all 3 offered: %v", len(got), got)
	}
	if got[0].GetLabel() != "folder" || len(got[0].GetAdditionalEdits()) != 0 ||
		got[2].GetLabel() != "args { ... }" || !got[2].GetIsSnippet() || len(got[2].GetAdditionalEdits()) != 0 {
		t.Errorf("the items that need no extra edit must pass through unchanged, got %v", got)
	}
	imp := got[1]
	if imp.GetLabel() != "use library.concepts.{ folder }" || imp.GetInsertText() != "folder" {
		t.Fatalf("the import item's label and insert text changed: %v", imp)
	}
	edits := imp.GetAdditionalEdits()
	if len(edits) != 1 {
		t.Fatalf("the import item must carry its one `use` edit, got %v", edits)
	}
	e := edits[0]
	if e.GetNewText() != "use library.concepts.{ folder }\n" ||
		e.GetRange().GetStart().GetLine() != 3 || e.GetRange().GetStart().GetColumn() != 1 ||
		e.GetRange().GetEnd().GetLine() != 3 || e.GetRange().GetEnd().GetColumn() != 1 {
		t.Errorf("the edit did not survive the wire: %v", e)
	}
}
