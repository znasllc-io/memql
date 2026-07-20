package sense

import (
	"regexp"
	"strings"
)

// Snippet support (#2629). The VS Code client advertises
// snippetSupport, but until now the server could not express it:
// CompletionItem had no format flag and the LSP mapping never set
// InsertTextFormat, so LSP defaulted every insertText to PlainText and
// snippet syntax would have inserted LITERALLY.

// snippetPlaceholderRe matches `${1:name}` / `${name}` placeholders.
var snippetPlaceholderRe = regexp.MustCompile(`\$\{(?:\d+:)?([^}]*)\}`)

// snippetTabstopRe matches bare `$1` / `$0` tabstops.
var snippetTabstopRe = regexp.MustCompile(`\$\d+`)

// plainFromSnippet degrades snippet text for a consumer without
// snippet support: placeholders collapse to their default text,
// tabstops vanish, and escaped literals are unescaped.
func plainFromSnippet(text string) string {
	out := snippetPlaceholderRe.ReplaceAllString(text, "$1")
	out = snippetTabstopRe.ReplaceAllString(out, "")
	out = strings.ReplaceAll(out, "\\$", "$")
	out = strings.ReplaceAll(out, "\\}", "}")
	return out
}

// escapeSnippetLiteral escapes the characters LSP snippet syntax
// reserves, so generated text carrying a literal `$` or `}` cannot be
// read as a tabstop or a placeholder terminator.
func escapeSnippetLiteral(text string) string {
	out := strings.ReplaceAll(text, `\`, `\\`)
	out = strings.ReplaceAll(out, "$", `\$`)
	out = strings.ReplaceAll(out, "}", `\}`)
	return out
}

// blockSnippet builds the body-block snippet for a named block:
//
//	args {
//	  <cursor>
//	}
//
// SortPriority 1 keeps snippets alongside the plain block keyword
// rather than drowning it out (everything unset sorts as 00000000 --
// first -- which is why every item here sets it deliberately).
func blockSnippet(block, construct string) CompletionItem {
	return CompletionItem{
		Label:         block + " { ... }",
		Kind:          "snippet",
		Detail:        construct + " block",
		Documentation: "Insert an `" + block + " { }` block with the cursor inside.",
		InsertText:    block + " {\n\t$0\n}",
		IsSnippet:     true,
		SortPriority:  1,
	}
}

// constructSkeletons are the top-level construct skeletons, offered
// where a declaration can start. Each places tabstops at the names an
// author must fill and leaves $0 in the body.
var constructSkeletons = []struct {
	keyword, label, doc, body string
}{
	{
		keyword: "query", label: "query <Concept> <name> { ... }",
		doc:  "A read construct bound to a concept: filter clause plus optional args and shape.",
		body: "query ${1:Concept} ${2:name} {\n\tfilter ${1:Concept}.${3:field} == args.${4:arg}\n\t$0\n}",
	},
	{
		keyword: "mutate", label: "mutate <Concept> <name> { ... }",
		doc:  "A write construct: args plus one insert/update block using the accept/stamp form.",
		body: "mutate ${1:Concept} ${2:name} {\n\targs {\n\t\t${3:field} string!\n\t}\n\tinsert {\n\t\taccept { ${3:field} }\n\t\t$0\n\t}\n}",
	},
	{
		keyword: "logic", label: "logic <name> { ... }",
		doc:  "A callable behavioral construct: args plus a body of named steps ending in return.",
		body: "logic ${1:name} {\n\targs {\n\t\t${2:field} string!\n\t}\n\tbody {\n\t\t$0\n\t}\n}",
	},
	{
		keyword: "automation", label: "automation <name> @trigger(...) => logic <name>",
		doc:  "The terse single-step automation form (#2619): make one logic reactive.",
		body: "automation ${1:name} @trigger(event=\"${2:topic}\") => logic ${3:logicName}$0",
	},
	{
		keyword: "concept", label: "concept <name> { ... }",
		doc:  "A schema declaration. The namespace comes from the containing domain directory.",
		body: "@namespace(\"${1:domain}\")\nconcept ${2:name} {\n\t${3:field} string!\n\t$0\n}",
	},
}

// constructSkeletonItems returns the skeleton snippets whose keyword
// matches the prefix.
func constructSkeletonItems(prefix string) []CompletionItem {
	var items []CompletionItem
	for _, sk := range constructSkeletons {
		if !strings.HasPrefix(sk.keyword, prefix) {
			continue
		}
		items = append(items, CompletionItem{
			Label:         sk.label,
			Kind:          "snippet",
			Detail:        "construct skeleton",
			Documentation: sk.doc,
			InsertText:    sk.body,
			IsSnippet:     true,
			// Below the bare keyword (which sorts at 1 in
			// specConstructItems) so the skeleton offers itself without
			// displacing the plain construct keyword.
			SortPriority: 3,
		})
	}
	return items
}
