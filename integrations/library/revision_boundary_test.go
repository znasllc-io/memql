package library

import (
	"strings"
	"testing"
)

func TestRevisionEndInsertionAndSubsumedRewording(t *testing.T) {
	for _, ending := range []string{"\n", "\r\n"} {
		source := "# Coffee" + ending + ending + "Coffee is enjoyed worldwide." + ending
		captured := capturedRevision(source, "Coffee is enjoyed worldwide.", 2, 3)
		captured["comments"] = append(captured["comments"].([]any), map[string]any{"id": "note-2", "body": "Add a latte recipe", "anchor": map[string]any{"kind": "document-end"}})
		reword := revisionReplacement{Before: "Coffee is enjoyed worldwide.", After: "People around the world enjoy coffee.", Reason: "Rephrase", CommentIDs: []string{"note-1"}}
		addition := "\n\n## Latte\n\nCombine espresso and steamed milk."
		for _, combined := range []bool{false, true} {
			second := revisionReplacement{Before: "", After: addition, Reason: "Add recipe", CommentIDs: []string{"note-2"}}
			want := strings.Replace(source, reword.Before, reword.After, 1) + strings.ReplaceAll(addition, "\n", ending)
			if combined {
				second.Before = reword.Before
				second.After = reword.After + addition
				second.CommentIDs = []string{"note-1", "note-2"}
				want = strings.Replace(source, reword.Before, strings.ReplaceAll(second.After, "\n", ending), 1)
			}
			for _, reverse := range []bool{false, true} {
				edits := []revisionReplacement{reword, second}
				if reverse {
					edits[0], edits[1] = edits[1], edits[0]
				}
				proposal, err := buildRevisionProposal(captured, revisionAnswer{Summary: "Reworded and added a recipe", Edits: edits})
				if err != nil || proposal["revisedContent"] != want {
					t.Fatalf("combined=%v reverse=%v: %v / %q", combined, reverse, err, proposal["revisedContent"])
				}
				if err = validateRevisionResult(captured, proposal); err != nil {
					t.Fatal(err)
				}
			}
		}
		for _, ids := range [][]string{{"note-1"}, {"note-1", "note-2"}, {"unknown"}} {
			_, err := buildRevisionProposal(captured, revisionAnswer{Summary: "Ambiguous insertion", Edits: []revisionReplacement{{Before: "", After: addition, Reason: "Append", CommentIDs: ids}}})
			if err == nil {
				t.Fatalf("unanchored insertion allowed: %v", ids)
			}
		}
		insert := revisionReplacement{After: addition, Reason: "Append", CommentIDs: []string{"note-2"}}
		if _, err := buildRevisionProposal(captured, revisionAnswer{Summary: "Duplicate boundary", Edits: []revisionReplacement{insert, insert}}); err == nil {
			t.Fatal("duplicate boundary accepted")
		}
		conflicting := reword
		conflicting.After = "A competing change.\n\nMore text."
		conflicting.CommentIDs = []string{"note-1", "note-2"}
		if _, err := buildRevisionProposal(captured, revisionAnswer{Summary: "Conflicting edits", Edits: []revisionReplacement{reword, conflicting}}); err == nil {
			t.Fatal("conflicting edits accepted")
		}
	}
}
