package memql

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// expr_condition_check_test.go -- D8's load check at the in-process row
// positions (memql#5366). A refine clause whose condition is a bare field of a
// declared non-boolean type -- alone, under `!`, `&&` or `||`, or as a
// ternary's condition or branch -- is refused at Init, naming the field, its
// declared type and the fix, exactly as Lower refuses one in a filter. The run
// time rule ("a stored non-boolean in condition position is not true") would
// otherwise have loaded it and emptied every page. The trigger-filter half is
// component/automations' TestTriggerFilterRefusesANonBooleanField.

// conditionCheckConcepts is lowerInitConcepts' ticket plus the types the
// check has to tell apart: a boolean, a datetime, an open object (keys as
// data) and a closed block.
const conditionCheckConcepts = `@version("1.0.0")
@namespace("lowerinit")
@description("A ticket the condition-check boot tests read.")
concept ticket {
  status    string!   @description("Workflow state.")
  title     string    @description("Title.")
  priority  int       @description("Priority.")
  tags      []string  @description("Tags.")
  due       datetime  @description("Due date.")
  archived  bool      @description("Archived.")
  info      object    @description("Free-form details, keys as data.")
  details {
    label   string    @description("A label.")
    pinned  bool      @description("Pinned.")
  }
}
`

// refineQuery is one query over the ticket whose refine clause is refine.
func refineQuery(name, refine string) string {
	return `
/// A refine the condition check reads.
query ticket ` + name + ` {
  filter   row => row.status != nil
  paginate 20
  refine   ` + refine + `
}
`
}

func TestInit_RefusesANonBooleanFieldAsARefineCondition(t *testing.T) {
	const ticket = "v1:lowerinit:ticket"
	stringFix := func(f string) string {
		return "Test whether it is set, `" + f + " != nil`, or compare it: `" + f + " == \"...\"`"
	}
	cases := []struct{ name, refine, field, typ, declared, fix string }{
		{"refinesAString", `row => row.title`, "row.title", "string", "declared `string` on " + ticket, stringFix("row.title")},
		{"refinesANegatedNumber", `row => !row.priority`, "row.priority", "number", "declared `int` on " + ticket, "Compare it with a number, as in `row.priority > 0`"},
		{"refinesAListUnderAnd", `row => row.archived && row.tags`, "row.tags", "list", "declared `[]string` on " + ticket, "Test its elements, as in `row.tags.any(x => x == \"...\")`, or its size: `row.tags.count() > 0`"},
		{"refinesADatetimeUnderOr", `row => row.archived || row.due`, "row.due", "datetime", "declared `datetime` on " + ticket, stringFix("row.due")},
		{"refinesATernaryCondition", `row => row.title ? row.archived : false`, "row.title", "string", "declared `string` on " + ticket, stringFix("row.title")},
		{"refinesATernaryBranch", `row => row.archived ? row.status : false`, "row.status", "string", "declared `string` on " + ticket, stringFix("row.status")},
		{"refinesAClosedBlockLeaf", `row => row.?details.label`, "row.?details.label", "string", "declared `string` on " + ticket, stringFix("row.?details.label")},
		{"refinesAnOpenObject", `row => row.info`, "row.info", "map", "declared `object` on " + ticket, "Compare one of its fields, as in `row.info.status == \"...\"`"},
		{"refinesAColumn", `row => row.createdAt`, "row.createdAt", "datetime", "the row's createdAt column", stringFix("row.createdAt")},
	}
	var queries strings.Builder
	queries.WriteString("use lowerinit.concepts.{ ticket }\n")
	for _, c := range cases {
		queries.WriteString(refineQuery(c.name, c.refine))
	}
	eng, err := bootLowerTree(t, map[string]string{
		"concepts.memql": conditionCheckConcepts,
		"queries.memql":  queries.String(),
	})
	require.Error(t, err, "strict boot refuses a refine whose condition is not boolean")
	skips := lowerInitSkips(eng)
	for _, c := range cases {
		var got string
		for _, s := range skips {
			if strings.HasPrefix(s, "query "+c.name+" ") {
				got = s
			}
		}
		require.NotEmpty(t, got, "%s (`%s`) was not refused at load; load report:\n%s", c.name, c.refine, strings.Join(skips, "\n"))
		want := "`" + c.field + "` does not lower in a refine clause: `" + c.field + "` is a " + c.typ +
			" (" + c.declared + "), and a condition must be boolean. " + c.fix
		require.Contains(t, got, want, c.name)
	}
	require.Len(t, skips, len(cases), "one refusal per refused query, and nothing else")
}

// TestInit_LoadsARefineWhoseConditionsAreBoolean is the negative control:
// the same shapes of refine with a boolean in every condition position -- a
// boolean field, a comparison, a method call, a typed read through a closed
// block, and an untyped read through an open object, whose type the load
// cannot know -- load and register.
func TestInit_LoadsARefineWhoseConditionsAreBoolean(t *testing.T) {
	cases := []struct{ name, refine string }{
		{"refinesABoolean", `row => row.archived`},
		{"refinesANegatedBoolean", `row => !row.archived && row.title != nil`},
		{"refinesComparisons", `row => row.priority > 1 || row.due > "2026-01-01"`},
		{"refinesABooleanTernary", `row => row.archived ? row.priority > 1 : row.tags.count() > 0`},
		{"refinesAMethod", `row => row.title.includes("x") && row.tags.any(t => t == "urgent")`},
		{"refinesAClosedBlockBoolean", `row => row.?details.pinned`},
		{"refinesAnOpenObjectKey", `row => row.info.flagged`},
	}
	var queries strings.Builder
	queries.WriteString("use lowerinit.concepts.{ ticket }\n")
	for _, c := range cases {
		queries.WriteString(refineQuery(c.name, c.refine))
	}
	eng, err := bootLowerTree(t, map[string]string{
		"concepts.memql": conditionCheckConcepts,
		"queries.memql":  queries.String(),
	})
	require.NoError(t, err, "a refine whose conditions are boolean boots")
	require.Empty(t, lowerInitSkips(eng))
	for _, c := range cases {
		fn, err := eng.Functions().Get(c.name)
		require.NoError(t, err, c.name)
		require.NotNil(t, refineIn(fn.Expr), "%s registered with its refine", c.name)
	}
}

// TestAuthoringLower_DefineRefusesANonBooleanRefineOnTheAuthorsLine: a
// session-authored query is lowered by the same pass, so its refine gets the
// same refusal at define -- positioned on the field where the author wrote
// it, on the refine's continuation line.
func TestAuthoringLower_DefineRefusesANonBooleanRefineOnTheAuthorsLine(t *testing.T) {
	eng := bootAuthoringEngine(t)
	const bundle = `use lowerinit.concepts.{ ticket }

/// Refused: title is a string, and a refine's condition must be boolean.
query ticket titledSession {
  filter   row => row.status != nil
  paginate 20
  refine   row => row.priority > 1 &&
             row.title
}
`
	var res SessionDefineResult
	var err error
	res, err = eng.DefineSessionBundle(NewAuthoredRuntimeRegistry(), "owner-1", bundle, "")
	require.Error(t, err, "a refine whose condition is not boolean is refused at define")
	d := diagnosticFor(t, res.Diagnostics, "query", "titledSession")
	require.False(t, d.OK)
	require.Contains(t, d.Error, "`row.title` does not lower in a refine clause: `row.title` is a string (declared `string` on v1:lowerinit:ticket), and a condition must be boolean. Test whether it is set, `row.title != nil`")
	line, col := positionOf(t, bundle, "row.title\n")
	require.Equal(t, line, d.Line, "the field's line, the refine's continuation line")
	require.Equal(t, col, d.Column, "and its column")
	require.Equal(t, line, d.EndLine)
	require.Equal(t, col+len("row.title"), d.EndColumn)
}
