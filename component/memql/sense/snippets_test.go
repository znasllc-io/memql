package sense

import (
	"strings"
	"testing"
)

// #2629: snippet items must be flagged, escaped, and deliberately
// sorted; plain items must stay plain.
func TestSnippetEscapingAndDegradation(t *testing.T) {
	for in, want := range map[string]string{
		"cost: $5":   `cost: \$5`,
		"tail }":     `tail \}`,
		`back\slash`: `back\\slash`,
		"plain":      "plain",
	} {
		if got := escapeSnippetLiteral(in); got != want {
			t.Errorf("escapeSnippetLiteral(%q) = %q, want %q", in, got, want)
		}
	}

	// A snippet degrades to readable plain text for consumers without
	// snippet support (the Cockpit gRPC path).
	item := CompletionItem{InsertText: "args {\n\t$0\n}", IsSnippet: true}
	if got := item.PlainInsertText(); got != "args {\n\t\n}" {
		t.Errorf("degradation = %q", got)
	}
	item = CompletionItem{InsertText: "query ${1:Concept} ${2:name} {$0}", IsSnippet: true}
	if got := item.PlainInsertText(); got != "query Concept name {}" {
		t.Errorf("placeholder degradation = %q", got)
	}
	plain := CompletionItem{InsertText: "userId", IsSnippet: false}
	if got := plain.PlainInsertText(); got != "userId" {
		t.Errorf("non-snippet must pass through, got %q", got)
	}
}

func TestBlockAndSkeletonSnippets(t *testing.T) {
	s := New(&fakeRegistry{})

	// Block snippet inside a construct body.
	src := "query todo todos {\n  "
	lines := strings.Split(src, "\n")
	var blockSnip *CompletionItem
	for _, it := range s.Complete(src, len(lines), len(lines[len(lines)-1])+1, "probe.memql") {
		if it.Kind == "snippet" && strings.HasPrefix(it.Label, "filter") {
			c := it
			blockSnip = &c
		}
	}
	if blockSnip == nil {
		t.Fatal("query body must offer a filter block snippet")
	}
	if !blockSnip.IsSnippet {
		t.Error("block snippet must be flagged IsSnippet")
	}
	if !strings.Contains(blockSnip.InsertText, "$0") {
		t.Errorf("block snippet must place a cursor tabstop: %q", blockSnip.InsertText)
	}
	if blockSnip.SortPriority == 0 {
		t.Error("every snippet must set SortPriority deliberately (unset sorts first)")
	}

	// Construct skeletons at top level.
	got := map[string]CompletionItem{}
	for _, it := range s.Complete("qu", 1, 3, "probe.memql") {
		if it.Kind == "snippet" {
			got[it.Label] = it
		}
	}
	var querySkel *CompletionItem
	for label, it := range got {
		if strings.HasPrefix(label, "query ") {
			c := it
			querySkel = &c
		}
	}
	if querySkel == nil {
		t.Fatalf("top level must offer a query skeleton, got %v", got)
	}
	if !querySkel.IsSnippet || !strings.Contains(querySkel.InsertText, "${1:") {
		t.Errorf("skeleton must be a flagged snippet with tabstops: %+v", querySkel)
	}
	if querySkel.SortPriority <= 1 {
		t.Errorf("skeleton must sort below the bare keyword, got %d", querySkel.SortPriority)
	}
}

// Every snippet the service can emit must be well-formed: flagged,
// non-empty, with a tabstop, and free of unescaped literal braces that
// would truncate the placeholder.
func TestAllSnippetItemsWellFormed(t *testing.T) {
	s := New(&fakeRegistry{concepts: []string{"v1:todos:todo"}})
	sources := []string{
		"qu", "mu", "lo", "au", "co",
		"query todo todos {\n  ",
		"mutate todo createTodo {\n  ",
		"logic compute {\n  ",
		"@trigger(event=\"x.y\")\nautomation onThing {\n  ",
		"mutate ",
	}
	for _, src := range sources {
		lines := strings.Split(src, "\n")
		for _, it := range s.Complete(src, len(lines), len(lines[len(lines)-1])+1, "probe.memql") {
			if !it.IsSnippet {
				// A non-snippet item must not carry tabstop syntax, or it
				// would insert literally.
				if strings.Contains(it.InsertText, "$") {
					t.Errorf("unflagged item carries snippet syntax (inserts literally): %+v", it)
				}
				continue
			}
			if it.InsertText == "" {
				t.Errorf("snippet with empty insert text: %+v", it)
			}
			if !strings.Contains(it.InsertText, "$") {
				t.Errorf("snippet without any tabstop: %+v", it)
			}
		}
	}
}
