package library

import (
	"reflect"
	"testing"

	workstate "github.com/znasllc-io/memql/component/work"
)

func TestRevisionItemsKeepMovesAtomicAndApplyOnlyAccepted(t *testing.T) {
	captured := capturedRevision("First paragraph.\n\nDestination.\n\nIndependent.", "First paragraph.", 0, 1)
	captured["comments"] = append(captured["comments"].([]any), map[string]any{"id": "second", "body": "Reword independent", "anchor": map[string]any{"kind": "markdown", "startLine": 4, "endLine": 5, "quote": "Independent.", "sourceQuote": "Independent."}})
	proposal, err := buildRevisionProposal(captured, revisionAnswer{Summary: "Move and reword", Edits: []revisionReplacement{
		{Before: "First paragraph.", After: "", Reason: "Move from here", CommentIDs: []string{"note-1"}},
		{Before: "Destination.", After: "Destination.\n\nFirst paragraph.", Reason: "Move to here", CommentIDs: []string{"note-1"}},
		{Before: "Independent.", After: "Clearer.", Reason: "Reword", CommentIDs: []string{"second"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	items, err := revisionItems(proposal)
	if err != nil || len(items) != 2 || len(items[0].Edits) != 2 {
		t.Fatalf("move split: %+v %v", items, err)
	}
	accepted := map[string]any{"acceptedItemIds": []string{items[1].ID}, "proposalHash": workstate.ArtifactHash(proposal)}
	applied, err := selectedRevisionProposal(proposal, accepted)
	if err != nil {
		t.Fatal(err)
	}
	if applied["revisedContent"] != "First paragraph.\n\nDestination.\n\nClearer." {
		t.Fatal("declined move changed source")
	}
	for _, ids := range [][]string{{}, {"unknown"}, {items[0].ID, items[0].ID}} {
		if _, err = selectedRevisionProposal(proposal, map[string]any{"acceptedItemIds": ids, "proposalHash": workstate.ArtifactHash(proposal)}); err == nil {
			t.Fatal("invalid accepted subset admitted")
		}
	}
	accepted["proposalHash"] = "old"
	if _, err = selectedRevisionProposal(proposal, accepted); err == nil {
		t.Fatal("stale selection admitted")
	}
	captured["amendment"] = map[string]any{"instruction": "Move it and add a note", "edits": items[0].Edits}
	captured["retainedEdits"] = items[1].Edits
	revised, err := buildAmendedRevisionProposal(captured, revisionAnswer{Summary: "Modified move", Edits: []revisionReplacement{
		{Before: "First paragraph.", After: "", Reason: "Move from here", CommentIDs: []string{"note-1"}},
		{Before: "Destination.", After: "Destination.\n\nFirst paragraph. Added note.", Reason: "Move and extend", CommentIDs: []string{"note-1"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	changed, _ := revisionItems(revised)
	if changed[1].ID != items[1].ID || !reflect.DeepEqual(changed[1].Edits, items[1].Edits) {
		t.Fatal("modifying a move changed an unrelated item")
	}
	if err = validateRevisionResult(captured, revised); err != nil {
		t.Fatal(err)
	}
	if _, err = buildAmendedRevisionProposal(captured, revisionAnswer{Summary: "Unrelated", Edits: items[1].Edits}); err == nil {
		t.Fatal("modified an unrelated item")
	}
}
