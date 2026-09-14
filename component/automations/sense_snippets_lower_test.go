package automations

import (
	"regexp"
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

// snippetStubRegistry answers every registry question with nothing: the
// snippets come from the DSL spec and the parser, not from the registries.
type snippetStubRegistry struct{}

func (snippetStubRegistry) FunctionNames() []string                        { return nil }
func (snippetStubRegistry) FunctionGet(string) (*sense.FunctionInfo, bool) { return nil, false }
func (snippetStubRegistry) ConceptNames() []string                         { return nil }
func (snippetStubRegistry) ConceptGet(string) (*sense.ConceptInfo, bool)   { return nil, false }
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
// keeps its default text, the final cursor ($0) takes cursor, and any other
// tabstop is left empty.
func fillSnippet(text, cursor string) string {
	out := snippetPlaceholder.ReplaceAllString(text, "$1")
	out = strings.ReplaceAll(out, "$0", cursor)
	return snippetTabstop.ReplaceAllString(out, "")
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
}
