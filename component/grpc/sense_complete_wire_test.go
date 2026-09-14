package memql

import (
	"testing"

	"github.com/znasllc-io/memql/component/memql/sense"
)

// TestSenseCompletionWireDropsItemsItCannotCarry: SenseCompletionItem has no
// field for a completion's additional edits, so an item that needs one -- a
// concept import, whose label promises the `use` line an edit elsewhere in
// the file adds -- is not offered on the wire, where only its name would be
// inserted (memql#5359). Every other item passes through unchanged.
func TestSenseCompletionWireDropsItemsItCannotCarry(t *testing.T) {
	at := sense.Position{Line: 1, Column: 1}
	got := senseCompletionItemsToProto([]sense.CompletionItem{
		{Label: "folder", Kind: "concept", InsertText: "folder", SortPriority: 1},
		{Label: "use library.concepts.{ folder }", Kind: "snippet", InsertText: "folder", SortPriority: 2,
			AdditionalEdits: []sense.TextEdit{{Range: sense.Range{Start: at, End: at}, NewText: "use library.concepts.{ folder }\n\n"}}},
		{Label: "args { ... }", Kind: "snippet", InsertText: "args {\n\t$0\n}", IsSnippet: true, SortPriority: 1},
	})
	if len(got) != 2 {
		t.Fatalf("got %d items, want 2 (the import dropped): %v", len(got), got)
	}
	if got[0].GetLabel() != "folder" || got[1].GetLabel() != "args { ... }" || !got[1].GetIsSnippet() {
		t.Errorf("the items that need no extra edit must pass through unchanged, got %v", got)
	}
}
