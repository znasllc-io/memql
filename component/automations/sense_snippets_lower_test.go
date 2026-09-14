package automations

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/dslspec"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql/sense"
)

// sense_snippets_lower_test.go -- memql#5359.
//
// Every snippet the editor inserts must lower once an author fills its
// placeholders: the construct skeletons at the top level, and the clause
// snippets inside each construct's body. A snippet that inserts a form the
// parser refuses -- a nameless `step { }` or `precondition { }` did -- teaches
// the one spelling that fails. It lives here rather than beside the snippets in
// component/memql/sense because an automation's precondition lowers only
// through this package's extraction, which sense cannot import.

// snippetStubRegistry answers the registry questions with one concept from a
// domain other than the probe file's -- the case that makes the concept slot
// of query / mutate / seed / shape offer to import it -- and nothing else:
// the other snippets come from the DSL spec and the parser.
type snippetStubRegistry struct{}

const snippetProbeConcept = "v1:library:folder"

func (snippetStubRegistry) FunctionNames() []string                        { return nil }
func (snippetStubRegistry) FunctionGet(string) (*sense.FunctionInfo, bool) { return nil, false }
func (snippetStubRegistry) ConceptNames() []string                         { return []string{snippetProbeConcept} }
func (snippetStubRegistry) ConceptGet(name string) (*sense.ConceptInfo, bool) {
	if name != snippetProbeConcept {
		return nil, false
	}
	return &sense.ConceptInfo{Name: name, Description: "a Library folder"}, true
}
func (snippetStubRegistry) SpecNames() []string                            { return nil }
func (snippetStubRegistry) ToolNames() []string                            { return nil }
func (snippetStubRegistry) ToolGet(string) (*sense.ToolInfo, bool)         { return nil, false }
func (snippetStubRegistry) PromptNames() []string                          { return nil }
func (snippetStubRegistry) PromptGet(string) (*sense.PromptInfo, bool)     { return nil, false }
func (snippetStubRegistry) ProviderNames() []string                        { return nil }
func (snippetStubRegistry) ProviderGet(string) (*sense.ProviderInfo, bool) { return nil, false }
func (snippetStubRegistry) ShapeNames() []string                           { return nil }
func (snippetStubRegistry) ShapeGet(string) (*sense.ShapeInfo, bool)       { return nil, false }
func (snippetStubRegistry) IntegrationCapabilities() []string              { return nil }

var (
	snippetPlaceholder = regexp.MustCompile(`\$\{\d+:([^}]*)\}`)
	snippetTabstop     = regexp.MustCompile(`\$\d+`)
)

// fillSnippet fills a snippet the way an author would: each placeholder
// keeps its default text, the final cursor ($0) takes cursor, any other
// tabstop is left empty, and the literals snippet syntax escapes (`\$`, `\}`,
// `\\`) read as themselves.
func fillSnippet(text, cursor string) string {
	out := snippetPlaceholder.ReplaceAllString(text, "$1")
	out = strings.ReplaceAll(out, "$0", cursor)
	out = snippetTabstop.ReplaceAllString(out, "")
	out = strings.ReplaceAll(out, `\$`, "$")
	out = strings.ReplaceAll(out, `\}`, "}")
	return strings.ReplaceAll(out, `\\`, `\`)
}

// conceptSlots is, per concept-binding construct, the text before its concept
// slot and the rest of a construct that lowers once a concept fills the slot.
var conceptSlots = map[string][2]string{
	"query":  {"query ", " probe {\n  filter row.id != \"\"\n}\n"},
	"mutate": {"mutate ", " probe {\n  insert {\n    id: \"x\"\n  }\n}\n"},
	"seed":   {"seed ", " probe {\n  name: \"x\"\n}\n"},
	"shape":  {"shape ", " probe {\n  row.id\n}\n"},
}

// lowerAuthored runs src down the path an authored file takes: the
// precondition extraction for an automation, then the struct-form rewriter and
// the parser. It returns the preconditions the extraction found.
func lowerAuthored(t *testing.T, keyword, src string) (int, error) {
	t.Helper()
	preconditions := 0
	if keyword == "automation" {
		found, stripped, err := extractPreconditions(src)
		if err != nil {
			return 0, err
		}
		preconditions, src = len(found), stripped
	}
	lowered, err := languageParser.NormaliseAll(src)
	if err != nil {
		return preconditions, err
	}
	_, err = languageParser.ParseFile(lowered)
	return preconditions, err
}

// snippetBodies is, per construct, the body the completion is asked in and,
// per clause, the construct a filled clause snippet sits in (%s) and what an
// author writes at the snippet's cursor.
var snippetBodies = map[string]struct {
	open    string
	clauses map[string][2]string // clause -> {construct with %s, cursor text}
}{
	"query": {"query thing probe {\n  ", map[string][2]string{
		"args": {"query thing probe {\n  %s\n  filter row.id == args.x\n}", "x string"},
	}},
	"mutate": {"mutate thing probe {\n  ", map[string][2]string{
		"args":   {"mutate thing probe {\n  %s\n  update {\n    id: args.x\n  }\n}", "x string"},
		"insert": {"mutate thing probe {\n  %s\n}", `id: "x"`},
		"update": {"mutate thing probe {\n  %s\n}", `id: "x"`},
		"accept": {"mutate thing probe {\n  args {\n    name string!\n  }\n  %s\n}", "name"},
		"stamp":  {"mutate thing probe {\n  args {\n    name string!\n  }\n  accept { name }\n  %s\n}", "createdAt: now"},
	}},
	"logic": {"logic probe {\n  ", map[string][2]string{
		"args": {"logic probe {\n  %s\n  body {\n    return args.x\n  }\n}", "x string"},
		"body": {"logic probe {\n  %s\n}", "return 1"},
	}},
	"automation": {"@trigger(event=\"x.y\")\nautomation probe {\n  ", map[string][2]string{
		"args":         {"automation probe {\n  %s\n  step run {\n    mutation createThing (id: \"x\")\n  }\n}", "x any"},
		"step":         {"automation probe {\n  %s\n}", `mutation createThing (id: "x")`},
		"precondition": {"automation probe {\n  %s\n  step run {\n    mutation createThing (id: \"x\")\n  }\n}", "check: 1 == 1"},
	}},
	"action": {"action probe {\n  ", map[string][2]string{
		"args": {"action probe {\n  %s\n  capability script(script: args.x)\n}", "x string"},
	}},
	"capability": {"capability integration.probe.run {\n  ", map[string][2]string{
		"args": {"capability integration.probe.run {\n  %s\n}", "x string"},
	}},
	"provider": {"provider probe {\n  ", map[string][2]string{
		"params": {"provider probe {\n  %s\n}", "contextWindow 1"},
		"auth":   {"provider probe {\n  %s\n}", `key "x"`},
	}},
}

// skeletonCursor is what an author writes at a construct skeleton's cursor.
var skeletonCursor = map[string]string{
	"query": "", "mutate": "", "logic": "return 1", "automation": "", "concept": "",
}

// TestEverySnippetCompletionLowers: every snippet completion lowers once its
// placeholders are filled -- the top-level skeletons and every construct's
// clause snippets. A named block's snippet must also produce the block it
// names: a precondition snippet that lowers to no precondition taught nothing.
func TestEverySnippetCompletionLowers(t *testing.T) {
	s := sense.New(snippetStubRegistry{})

	skeletons := 0
	for _, it := range s.Complete("", 1, 1, "probe.memql") {
		if !it.IsSnippet {
			continue
		}
		skeletons++
		keyword := strings.Fields(it.Label)[0]
		cursor, ok := skeletonCursor[keyword]
		if !ok {
			t.Errorf("skeleton %q: no cursor text in skeletonCursor", it.Label)
			continue
		}
		src := fillSnippet(it.InsertText, cursor)
		if _, err := lowerAuthored(t, keyword, src); err != nil {
			t.Errorf("skeleton %q does not lower once filled:\n%s\n%v", it.Label, src, err)
		}
	}
	if skeletons == 0 {
		t.Fatal("the top level offered no skeleton snippets -- this test drove nothing")
	}

	// Every construct whose body takes a block clause is driven, so a
	// construct added to the spec cannot slip past with an untested snippet.
	for _, c := range dslspec.Build().Constructs {
		for _, clause := range c.BodyBlocks {
			if languageParser.IsLineClause(clause) {
				continue
			}
			if _, ok := snippetBodies[c.Keyword]; !ok {
				t.Errorf("%s: its body takes the %q block but snippetBodies has no entry for it", c.Keyword, clause)
			}
			break
		}
	}
	for keyword, body := range snippetBodies {
		lines := strings.Split(body.open, "\n")
		offered := 0
		for _, it := range s.Complete(body.open, len(lines), len(lines[len(lines)-1])+1, "probe.memql") {
			if !it.IsSnippet {
				continue
			}
			offered++
			clause := strings.Fields(it.Label)[0]
			c, ok := body.clauses[clause]
			if !ok {
				t.Errorf("%s: snippet %q has no fixture in snippetBodies", keyword, it.Label)
				continue
			}
			src := strings.Replace(c[0], "%s", fillSnippet(it.InsertText, c[1]), 1)
			preconditions, err := lowerAuthored(t, keyword, src)
			if err != nil {
				t.Errorf("%s: the %q snippet does not lower once filled:\n%s\n%v", keyword, it.Label, src, err)
				continue
			}
			if clause == "precondition" && preconditions != 1 {
				t.Errorf("%s: the %q snippet lowers to %d preconditions, want 1:\n%s", keyword, it.Label, preconditions, src)
			}
		}
		for _, clause := range languageParser.BodyClauses(keyword) {
			if !languageParser.IsLineClause(clause) {
				if _, ok := body.clauses[clause]; !ok {
					t.Errorf("%s: block clause %q has no fixture -- its snippet is untested", keyword, clause)
				}
			}
		}
		if offered == 0 {
			t.Errorf("%s: the body offered no snippets -- this test drove nothing for it", keyword)
		}
	}

	// The concept slot of every concept-binding construct: EVERY item it
	// offers -- the bare concept, and the one that also imports it -- must
	// lower once applied. The import is the one data-driven completion: with no
	// concept in the registry it is never produced, and it inserted a whole
	// `use` line at the slot (`query use library.concepts.{ folder }`).
	// Each slot is driven in a file with no imports and in one that already
	// has one, so the import lands both at the top and after the last `use`.
	for keyword, slot := range conceptSlots {
		for _, preamble := range []string{"", "use cluster.concepts.{ node }\n"} {
			open := preamble + slot[0]
			lines := strings.Split(open, "\n")
			offered, imports := 0, 0
			for _, it := range s.Complete(open, len(lines), len(lines[len(lines)-1])+1, "probe.memql") {
				offered++
				if len(it.AdditionalEdits) > 0 {
					imports++
				}
				src := applyEdits(t, open+fillSnippet(it.InsertText, "")+slot[1], it.AdditionalEdits)
				if _, err := lowerAuthored(t, keyword, src); err != nil {
					t.Errorf("%s: the concept-slot completion %q does not lower once applied:\n%s\n%v", keyword, it.Label, src, err)
				}
			}
			if offered == 0 || imports == 0 {
				t.Errorf("%s: the concept slot offered %d items, %d of them imports -- this test drove nothing for it", keyword, offered, imports)
			}
		}
	}
}

// applyEdits applies a completion's additional edits, which sit before its
// insert position (the file's imports), to src. Positions are 1-based lines
// and columns, as Sense reports them.
func applyEdits(t *testing.T, src string, edits []sense.TextEdit) string {
	t.Helper()
	offset := func(p sense.Position) int {
		lines := strings.Split(src, "\n")
		if p.Line < 1 || p.Line > len(lines)+1 {
			t.Fatalf("edit line %d is outside the %d-line document", p.Line, len(lines))
		}
		at := 0
		for i := 0; i < p.Line-1; i++ {
			at += len(lines[i]) + 1
		}
		return at + p.Column - 1
	}
	ordered := append([]sense.TextEdit(nil), edits...)
	sort.Slice(ordered, func(i, j int) bool { return offset(ordered[i].Range.Start) > offset(ordered[j].Range.Start) })
	for _, e := range ordered {
		start, end := offset(e.Range.Start), offset(e.Range.End)
		src = src[:start] + e.NewText + src[end:]
	}
	return src
}
