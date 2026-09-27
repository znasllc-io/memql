package sense

import (
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// TestCompletionItemsDecodeTheirAdditionalEdits (memql#5426): the SDK decodes
// the edits a completion makes elsewhere in the document -- a concept
// import's `use` line -- so a consumer can apply them, or know it cannot and
// not offer the item. An item with none decodes with none.
func TestCompletionItemsDecodeTheirAdditionalEdits(t *testing.T) {
	at := &memqlv1.SensePosition{Line: 3, Column: 1}
	got := protoCompletionItems([]*memqlv1.SenseCompletionItem{
		{Label: "folder", Kind: "concept", InsertText: "folder", SortPriority: 1},
		{Label: "use library.concepts.{ folder }", Kind: "snippet", InsertText: "folder", SortPriority: 2,
			AdditionalEdits: []*memqlv1.SenseTextEdit{{Range: &memqlv1.SenseRange{Start: at, End: at}, NewText: "use library.concepts.{ folder }\n"}}},
	})
	if len(got) != 2 || len(got[0].AdditionalEdits) != 0 {
		t.Fatalf("decoded %+v", got)
	}
	want := TextEdit{Range: Range{Start: Position{Line: 3, Column: 1}, End: Position{Line: 3, Column: 1}}, NewText: "use library.concepts.{ folder }\n"}
	if len(got[1].AdditionalEdits) != 1 || got[1].AdditionalEdits[0] != want {
		t.Fatalf("the import's edit did not decode: %+v", got[1].AdditionalEdits)
	}
	if got[1].InsertText != "folder" || got[1].SortPriority != 2 {
		t.Errorf("the rest of the item changed: %+v", got[1])
	}
}
