package parser

// Fixtures for the memql#2635 dedup codemod (DoD list): verbatim duplicate,
// paraphrase above threshold, non-duplicate left alone, Arguments/Returns
// with and without extra per-arg prose, escaped quotes un-escaped -- plus
// the equivalence property: the resolved description (LeadingDocComment
// after the rewrite) equals the original @description text.

import (
	"strings"
	"testing"
)

func rewriteDoc(t *testing.T, src string) string {
	t.Helper()
	out, err := RewriteDocCommentDescriptions([]byte(src))
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	return string(out)
}

func TestRewriteDocComments_VerbatimDuplicateHeaderDeleted(t *testing.T) {
	src := `// Lists active spaces for the calling user, newest first.
@actor
@description("Lists active spaces for the calling user, newest first.")
query space querySpacesProbe {
  filter ownerUserId == actor.userId
}
`
	got := rewriteDoc(t, src)
	for _, l := range strings.Split(got, "\n") {
		if strings.HasPrefix(l, "// Lists active spaces") {
			t.Errorf("verbatim duplicate header must be deleted:\n%s", got)
		}
	}
	if !strings.Contains(got, "/// Lists active spaces for the calling user, newest first.") {
		t.Errorf("description must become a /// block:\n%s", got)
	}
	if strings.Contains(got, "@description") {
		t.Errorf("@description line must be dropped:\n%s", got)
	}
	if LeadingDocComment(got) != "Lists active spaces for the calling user, newest first." {
		t.Errorf("resolved description changed: %q", LeadingDocComment(got))
	}
}

func TestRewriteDocComments_ParaphraseAboveThresholdDeleted(t *testing.T) {
	src := `// Active spaces belonging to the calling user, returned newest first.
@description("Lists the active spaces for the calling user, newest first.")
query space querySpacesProbe {
  filter ownerUserId == actor.userId
}
`
	got := rewriteDoc(t, src)
	if strings.Contains(got, "// Active spaces belonging") {
		t.Errorf("near-verbatim paraphrase must be deleted:\n%s", got)
	}
}

func TestRewriteDocComments_NonDuplicateHeaderKept(t *testing.T) {
	src := `// HISTORICAL: replaced the v0 partition walk; see epic #1234 for the
// migration constraints and the keyset-pagination contract details.
@description("Lists active spaces for the calling user.")
query space querySpacesProbe {
  filter ownerUserId == actor.userId
}
`
	got := rewriteDoc(t, src)
	if !strings.Contains(got, "// HISTORICAL: replaced the v0 partition walk") {
		t.Errorf("non-duplicate header must be kept:\n%s", got)
	}
	if !strings.Contains(got, "/// Lists active spaces for the calling user.") {
		t.Errorf("description still converts:\n%s", got)
	}
}

func TestRewriteDocComments_ArgumentsBlockRestatingDropped(t *testing.T) {
	src := `// Arguments:
//   planId string
//   limit number
@description("Traces for a plan.")
query candidate queryTracesProbe {
  args {
    planId string!
    limit  number
  }
  filter planId == args.planId
}
`
	got := rewriteDoc(t, src)
	if strings.Contains(got, "// Arguments:") || strings.Contains(got, "//   planId string") {
		t.Errorf("restating Arguments block must be dropped:\n%s", got)
	}
}

func TestRewriteDocComments_ArgumentsProseMovesToFieldDoc(t *testing.T) {
	src := `// Arguments:
//   planId -- the plan whose traces to list; bare id, canonicalized server-side
//   limit: page size cap
@description("Traces for a plan.")
query candidate queryTracesProbe {
  args {
    planId string!
    limit  number
  }
  filter planId == args.planId
}
`
	got := rewriteDoc(t, src)
	if !strings.Contains(got, "    /// the plan whose traces to list; bare id, canonicalized server-side\n    planId string!") {
		t.Errorf("per-arg prose must move to a /// above the field:\n%s", got)
	}
	if !strings.Contains(got, "    /// page size cap\n    limit  number") {
		t.Errorf("colon-form prose must move too:\n%s", got)
	}
	if strings.Contains(got, "// Arguments:") {
		t.Errorf("the Arguments section itself must be dropped:\n%s", got)
	}
}

func TestRewriteDocComments_UnplaceableSectionKept(t *testing.T) {
	src := `// Returns:
//   a projection whose rows interleave the join in a way the schema cannot express
@description("Probe.")
query space queryOddProbe {
  filter ownerUserId == "x"
}
`
	got := rewriteDoc(t, src)
	if strings.Contains(got, "// Returns:") {
		// Returns sections have no field slot; restating ones drop, but this
		// carries free prose -- it must NOT silently vanish. (It parses as an
		// arg-form line? No: no leading field name pattern.)
		t.Logf("kept as expected")
	}
	if !strings.Contains(got, "interleave the join") {
		t.Errorf("unplaceable prose must never be deleted:\n%s", got)
	}
}

func TestRewriteDocComments_EscapedQuotesUnescaped(t *testing.T) {
	src := `@description("Marks the row \"done\" and clears the backslash \\ escape.")
mutate candidate mutateDoneProbe {
  args {
    id string!
  }
  update {
    status: "done"
  }
}
`
	got := rewriteDoc(t, src)
	if !strings.Contains(got, `/// Marks the row "done" and clears the backslash \ escape.`) {
		t.Errorf("escapes must unwrap:\n%s", got)
	}
	if want := `Marks the row "done" and clears the backslash \ escape.`; LeadingDocComment(got) != want {
		t.Errorf("resolved = %q, want %q", LeadingDocComment(got), want)
	}
}

func TestRewriteDocComments_LongDescriptionWrapsAndRoundTrips(t *testing.T) {
	long := strings.Repeat("alpha beta gamma delta epsilon ", 12)
	long = strings.TrimSpace(long)
	src := "@description(\"" + long + "\")\nquery space queryLongProbe {\n  filter ownerUserId == \"x\"\n}\n"
	got := rewriteDoc(t, src)
	if LeadingDocComment(got) != long {
		t.Errorf("wrapped description must round-trip through the join:\ngot  %q\nwant %q", LeadingDocComment(got), long)
	}
	for _, l := range strings.Split(got, "\n") {
		if strings.HasPrefix(l, "///") && len(l) > 104 {
			t.Errorf("line exceeds wrap width: %q", l)
		}
	}
}

func TestRewriteDocComments_InlineFieldDescriptionsUntouched(t *testing.T) {
	src := `@handler(type="function", name="probeTool")
@description("Tool level.")
tool probeTool {
  role string! @description("Field level stays.")
}
`
	got := rewriteDoc(t, src)
	if !strings.Contains(got, `role string! @description("Field level stays.")`) {
		t.Errorf("inline field @description must be untouched:\n%s", got)
	}
	if strings.Contains(got, "\n@description(\"Tool level.\")") {
		t.Errorf("construct-level must convert:\n%s", got)
	}
}

func TestRewriteDocComments_Idempotent(t *testing.T) {
	src := `// Lists active spaces for the calling user, newest first.
@description("Lists active spaces for the calling user, newest first.")
query space querySpacesProbe {
  filter ownerUserId == actor.userId
}
`
	once := rewriteDoc(t, src)
	twice := rewriteDoc(t, once)
	if once != twice {
		t.Errorf("rewrite must be idempotent:\nonce:\n%s\ntwice:\n%s", once, twice)
	}
}

// Parse()'s file/expression heuristic must recognize an annotation-free,
// ///-documented construct file (before #2635 every corpus construct carried
// @description, so the TokenAt branch masked this gap); expression uses of
// the contextual keywords stay expressions.
func TestParseHeuristic_BareConstructKeywordIsFile(t *testing.T) {
	for name, src := range map[string]string{
		"bare-trait": "trait t1 {\n  return effect == \"allow\"\n}\n",
		"bare-spec":  "spec actorEnvelope s1 {\n  return role == \"admin\"\n}\n",
		"doc-trait":  "/// Doc.\ntrait t1 {\n  return effect == \"allow\"\n}\n",
	} {
		t.Run(name, func(t *testing.T) {
			tokens, err := NewLexer(src).Tokenize()
			if err != nil {
				t.Fatal(err)
			}
			p := NewParser(tokens)
			p.SetSource(src)
			node, err := p.Parse()
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if _, ok := node.(*File); !ok {
				t.Errorf("want *File, got %T", node)
			}
		})
	}
	t.Run("expression-keyword-stays-expression", func(t *testing.T) {
		tokens, err := NewLexer(`spec == "x"`).Tokenize()
		if err != nil {
			t.Fatal(err)
		}
		p := NewParser(tokens)
		node, err := p.Parse()
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if _, ok := node.(*File); ok {
			t.Error("expression use of a contextual keyword must stay an expression")
		}
	})
}
