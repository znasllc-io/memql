package sense

import "testing"

func labelSet(items []CompletionItem) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, it := range items {
		out[it.Label] = true
	}
	return out
}

func hasTokenColor(toks []Token, literal, tokType string) bool {
	for _, tk := range toks {
		if tk.Literal == literal && tk.Type == tokType {
			return true
		}
	}
	return false
}

// Item 2: completeAnnotationArgs sources keyword args from the annotations
// registry, so the four-annotation hardcoded switch is gone and newly-modeled
// annotations (rateLimit, relationship) are covered.
func TestCompleteAnnotationArgs_DerivedFromRegistry(t *testing.T) {
	s := New(nil)

	trigger := labelSet(s.completeAnnotationArgs(CursorContext{AnnotationName: "trigger"}))
	// partition is currently-required and common in the real DSL (#56 phase 8),
	// so it must be offered alongside event/schedule/concept.
	for _, want := range []string{"event", "schedule", "concept", "partition"} {
		if !trigger[want] {
			t.Errorf("trigger args missing %q (want registry-derived)", want)
		}
	}

	rl := labelSet(s.completeAnnotationArgs(CursorContext{AnnotationName: "rateLimit"}))
	if !rl["maxCalls"] || !rl["periodSeconds"] {
		t.Errorf("rateLimit args not derived from the registry: %v", rl)
	}
	rel := labelSet(s.completeAnnotationArgs(CursorContext{AnnotationName: "relationship"}))
	if !rel["target"] || !rel["direction"] {
		t.Errorf("relationship args not derived from the registry: %v", rel)
	}

	// Keyword args insert with a trailing '='.
	for _, it := range s.completeAnnotationArgs(CursorContext{AnnotationName: "trigger"}) {
		if it.Label == "event" && it.InsertText != "event=" {
			t.Errorf("event insert text = %q; want \"event=\"", it.InsertText)
		}
	}
}

func TestCompleteAnnotationArgs_PrefixFilter(t *testing.T) {
	s := New(nil)
	got := labelSet(s.completeAnnotationArgs(CursorContext{AnnotationName: "trigger", Prefix: "sch"}))
	if !got["schedule"] || got["event"] {
		t.Errorf("prefix 'sch' should offer only schedule, got %v", got)
	}
}

func TestCompleteAnnotationArgs_DefaultProviderSuggestsProviders(t *testing.T) {
	s := New(&stubRegistry{providers: []string{"chat54Mini", "streamClaudeSonnet"}})
	got := labelSet(s.completeAnnotationArgs(CursorContext{AnnotationName: "defaultProvider"}))
	if !got["chat54Mini"] || !got["streamClaudeSonnet"] {
		t.Errorf("defaultProvider should suggest provider names, got %v", got)
	}
}

// Item 3: mapTokenType colors the current keywords. `use` (imports) and `in`
// (membership) color as keyword; the retired has/import audit item does not
// apply (has was removed, import is `use`).
func TestTokenize_ColorsUseAndInKeywords(t *testing.T) {
	s := New(nil)
	if toks := s.Tokenize("use cognition.concepts.{ space }"); !hasTokenColor(toks, "use", "keyword") {
		t.Error("`use` should color as a keyword")
	}
	if toks := s.Tokenize("filter status in kinds"); !hasTokenColor(toks, "in", "keyword") {
		t.Error("`in` should color as a keyword")
	}
}

// Item 4: constructHeaderRe's keyword alternation is derived from dslspec
// (constructKeywords) rather than hand-listed, and excludes the unnamed `use`
// import.
func TestConstructHeaderRe_DerivedFromDslspec(t *testing.T) {
	for _, decl := range []string{"query Foo q {", "mutate Foo m {", "concept foo {", "shape Foo s {"} {
		if !constructHeaderRe.MatchString(decl) {
			t.Errorf("constructHeaderRe should match %q", decl)
		}
	}
	if constructHeaderRe.MatchString("use cognition.concepts.{ space }") {
		t.Error("constructHeaderRe should not match a `use` import line")
	}
	// Every dslspec construct keyword (except `use`) is anchored.
	for kw := range constructKeywords {
		if kw == "use" {
			continue
		}
		if !constructHeaderRe.MatchString(kw + " Foo x {") {
			t.Errorf("constructHeaderRe missing dslspec construct keyword %q", kw)
		}
	}
}
