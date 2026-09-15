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

// blockSnippet builds the body-block snippet for a block clause:
//
//	args {
//	  <cursor>
//	}
//
// SortPriority 1 keeps snippets alongside the plain block keyword
// rather than drowning it out (everything unset sorts as 00000000 --
// first -- which is why every item here sets it deliberately).
func blockSnippet(block, construct string) CompletionItem {
	if block == "filter" {
		// A filter is not a block in v1: it is a clause whose value is a
		// lambda over the row (the `filter { }` block is a pre-v1 spelling).
		return CompletionItem{
			Label:         "filter row => ...",
			Kind:          "snippet",
			Detail:        construct + " clause",
			Documentation: "Insert a `filter row => <predicate>` clause over the bound concept's row.",
			InsertText:    `filter row => row.${1:field} == ${2:"value"}$0`,
			IsSnippet:     true,
			SortPriority:  1,
		}
	}
	return CompletionItem{
		Label:         block + " { ... }",
		Kind:          "snippet",
		Detail:        construct + " block",
		Documentation: "Insert a `" + block + " { }` block with the cursor inside.",
		InsertText:    block + " {\n\t$0\n}",
		IsSnippet:     true,
		SortPriority:  1,
	}
}

// namedBlockSnippet builds the snippet for a block that carries a name
// (parser.IsNamedBlock): the name is the first tabstop and the cursor lands
// inside, because `step { ... }` without one is refused.
//
//	step <name> {
//	  <cursor>
//	}
func namedBlockSnippet(block, construct string) CompletionItem {
	return CompletionItem{
		Label:         block + " <name> { ... }",
		Kind:          "snippet",
		Detail:        construct + " block",
		Documentation: "Insert a named `" + block + " <name> { }` block with the cursor inside.",
		InsertText:    block + " ${1:name} {\n\t$0\n}",
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
		doc: "A read construct bound to a concept: a declared arg, a `filter row => ...` predicate over the row, and a page size.",
		// The arg is declared because the loader refuses an args.X a body reads
		// but never declares, and the page size because a list-returning query
		// must carry paginate, sort, count or @unbounded.
		body: "query ${1:Concept} ${2:name} {\n\targs {\n\t\t${4:value} string\n\t}\n\tfilter row => row.${3:field} == args.${4:value}\n\tpaginate ${5:50}\n\t$0\n}",
	},
	{
		keyword: "spec", label: "spec <Concept> <name> = row => ...",
		doc:  "A predicate over one bound concept, applied as `name(row)`. Over an @actor shape the parameter is `actor`.",
		body: "/// ${1:What the predicate matches.}\nspec ${2:Concept} ${3:name} = row => row.${4:field} == ${5:true}$0",
	},
	{
		keyword: "trait", label: "trait <name> = row => ...",
		doc:  "A predicate over any row, bound to no concept, applied as `name(row)`.",
		body: "/// ${1:What the predicate matches.}\ntrait ${2:name} = row => row.${3:field} == ${4:true}$0",
	},
	{
		keyword: "mutation", label: "mutation <Concept> <name> { ... }",
		doc:  "A write construct: args plus one insert/update block using the accept/stamp form.",
		body: "mutation ${1:Concept} ${2:name} {\n\targs {\n\t\t${3:field} string!\n\t}\n\tinsert {\n\t\taccept { ${3:field} }\n\t\t$0\n\t}\n}",
	},
	{
		keyword: "logic", label: "logic <name> { ... }",
		doc:  "A callable behavioral construct: args, then its statements, ending in return.",
		body: "logic ${1:name} {\n\targs {\n\t\t${2:field} string!\n\t}\n\t$0\n}",
	},
	{
		keyword: "automation", label: "automation <name> { ... }",
		doc:  "A reactive construct: its trigger, then its statements.",
		body: "@trigger(event=\"${1:topic}\")\nautomation ${2:name} {\n\tlogic ${3:logicName}(event: event)$0\n}",
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

// annotationSnippets are the annotation completions that insert a whole v1
// form rather than a name: the trigger filter, whose argument is a lambda over
// the triggering row. Offered where the construct takes @filter.
func annotationSnippets(enc EnclosingConstruct) []CompletionItem {
	if !containsString(annotationsForConstruct(enc), "filter") {
		return nil
	}
	return []CompletionItem{{
		Label:         "@filter(...)",
		Kind:          "snippet",
		Detail:        "trigger filter",
		Documentation: "Insert a trigger filter over the triggering row: `@filter(row => <predicate>)`.",
		InsertText:    `filter(row => row.${1:field} == ${2:"value"})$0`,
		IsSnippet:     true,
		SortPriority:  2,
	}}
}
