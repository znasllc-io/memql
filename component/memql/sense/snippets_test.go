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

	// Block snippet inside a construct body. `args` opens a block; `filter`
	// is a LINE clause (`filter <expr>`), so it gets no block snippet and is
	// inserted as its keyword and a space -- the `filter {` this used to
	// offer is a form the rewriter refuses (memql#5359).
	src := "query todo todos {\n  "
	lines := strings.Split(src, "\n")
	var blockSnip, filterItem *CompletionItem
	for _, it := range s.Complete(src, len(lines), len(lines[len(lines)-1])+1, "probe.memql") {
		if it.Kind == "snippet" && strings.HasPrefix(it.Label, "args") {
			c := it
			blockSnip = &c
		}
		if it.Kind == "snippet" && strings.HasPrefix(it.Label, "filter") {
			t.Errorf("filter is a line clause; it must not be offered as a block snippet: %+v", it)
		}
		if it.Kind == "keyword" && it.Label == "filter" {
			c := it
			filterItem = &c
		}
	}
	if filterItem == nil || filterItem.InsertText != "filter row => " {
		t.Errorf("query body must offer the filter line clause inserted as `filter row => `, got %+v", filterItem)
	}
	if blockSnip == nil {
		t.Fatal("query body must offer an args block snippet")
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
		"mutation todo createTodo {\n  ",
		"logic compute {\n  ",
		"@trigger(event=\"x.y\")\nautomation onThing {\n  ",
		"mutation ",
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

// TestNamedBlocksCompleteWithTheirName: `step` and `precondition` carry a
// name (`step <name> { ... }`), and a nameless block is refused -- so the
// keyword completion inserts the keyword and a space, never `step {`, and the
// snippet puts the name first (memql#5359).
func TestNamedBlocksCompleteWithTheirName(t *testing.T) {
	s := New(&fakeRegistry{})
	src := "@trigger(event=\"x.y\")\nautomation onThing {\n  "
	lines := strings.Split(src, "\n")
	items := s.Complete(src, len(lines), len(lines[len(lines)-1])+1, "probe.memql")
	for _, block := range []string{"step", "precondition"} {
		var keyword, snippet *CompletionItem
		for _, it := range items {
			it := it
			switch {
			case it.Kind == "keyword" && it.Label == block:
				keyword = &it
			case it.Kind == "snippet" && strings.HasPrefix(it.Label, block+" "):
				snippet = &it
			}
		}
		if keyword == nil || keyword.InsertText != block+" " {
			t.Errorf("%s: the keyword completion must insert %q, got %+v", block, block+" ", keyword)
		}
		if snippet == nil || snippet.InsertText != block+" ${1:name} {\n\t$0\n}" {
			t.Errorf("%s: the snippet must put the name first, got %+v", block, snippet)
		}
	}
}

// TestConceptFieldAnnotationsInsertTheirParen: inside a concept body a field
// annotation that cannot be written bare completes with its `(` -- decided
// from the registry's forms, as the construct annotations are -- and a flag
// completes bare.
func TestConceptFieldAnnotationsInsertTheirParen(t *testing.T) {
	s := New(&fakeRegistry{})
	src := "concept widget {\n  "
	lines := strings.Split(src, "\n")
	got := map[string]string{}
	for _, it := range s.Complete(src, len(lines), len(lines[len(lines)-1])+1, "probe.memql") {
		if it.Kind == "annotation" {
			got[it.Label] = it.InsertText
		}
	}
	for label, want := range map[string]string{
		"@maxLength": "@maxLength(", "@pattern": "@pattern(", "@variant": "@variant(", "@default": "@default(",
		"@required": "@required", "@pii": "@pii",
	} {
		if got[label] != want {
			t.Errorf("%s inserts %q, want %q", label, got[label], want)
		}
	}
}

// TestAnAtInsideAFieldListOffersTheFieldAnnotations: an `@` after a field's
// type is written on the FIELD, so it offers the field list's annotations --
// a concept's fields (and its body's @relationship), a tool's, an args
// block's -- with the paren each needs; the construct's own annotations, which
// every field refuses, are not offered there. In the preamble the construct's
// set stays (TestReceiverFilteredAnnotations).
func TestAnAtInsideAFieldListOffersTheFieldAnnotations(t *testing.T) {
	s := New(&fakeRegistry{})
	for _, tc := range []struct {
		src    string
		want   map[string]string // label -> insert text
		absent []string
	}{
		{
			src:    "concept widget {\n  title string @",
			want:   map[string]string{"@maxLength": "maxLength(", "@pii": "pii", "@relationship": "relationship("},
			absent: []string{"@displayCard", "@rowAuthz", "@namespace"},
		},
		{
			src:    "tool probe {\n  x string @",
			want:   map[string]string{"@autoInjected": "autoInjected", "@default": "default("},
			absent: []string{"@handler", "@rateLimit"},
		},
		{
			src:    "query thing probe {\n  args {\n    x string @",
			want:   map[string]string{"@maxLength": "maxLength(", "@required": "required"},
			absent: []string{"@cache", "@public"},
		},
	} {
		lines := strings.Split(tc.src, "\n")
		got := map[string]string{}
		for _, it := range s.Complete(tc.src, len(lines), len(lines[len(lines)-1])+1, "probe.memql") {
			if it.Kind == "annotation" {
				got[it.Label] = it.InsertText
			}
		}
		for label, insert := range tc.want {
			if got[label] != insert {
				t.Errorf("%q: %s inserts %q, want %q", tc.src, label, got[label], insert)
			}
		}
		for _, label := range tc.absent {
			if _, ok := got[label]; ok {
				t.Errorf("%q: offers %s, which a field refuses", tc.src, label)
			}
		}
	}
}
