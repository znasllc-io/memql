package sense

// diagnose_rewrite_position_test.go -- Sense's diagnostic for a refusal the
// struct-form rewriter makes lands on the author's text it refuses, where it
// used to be anchored on the construct's name (memql#5364). The table is the
// parser's (component/language/parser/rewrite_errors_test.go): every refusal
// the rewriter makes of authored text.

import (
	"strconv"
	"strings"
	"testing"
)

const probeTrigger = "@trigger(event=\"node.created\", concept=\"v1:probe:thing\")\n"

var senseRewriteRefusalCases = []struct {
	name   string
	src    string
	needle string
	nth    int
	want   string
}{
	// Query clauses.
	{"refine without paginate", "query thing q {\n  filter row => row.a == 1\n  refine row => row.b == 2\n}\n", "refine", 1, "`refine` requires `paginate`"},
	{"refine with count", "query thing q {\n  filter row => row.a == 1\n  refine row => row.b == 2\n  count\n}\n", "refine", 1, "`refine` cannot be combined with `count`"},
	{"count with shape", "query thing q {\n  filter row => row.a == 1\n  shape thingCard\n  count\n}\n", "count", 1, "`count` and `shape` are mutually exclusive"},
	{"count with paginate", "query thing q {\n  filter row => row.a == 1\n  count\n  paginate 10\n}\n", "count", 1, "`count` cannot be combined with `sort` or `paginate`"},
	{"@unbounded with an empty reason", "@unbounded(\"  \")\nquery thing q {\n  filter row => row.a == 1\n}\n", "@unbounded", 1, "requires a non-empty reason string"},
	{"@unbounded with paginate", "@unbounded(\"every one\")\nquery thing q {\n  filter row => row.a == 1\n  paginate 10\n}\n", "@unbounded", 1, "cannot be combined with `paginate` or `sort`"},
	{"@unbounded with count", "@unbounded(\"every one\")\nquery thing q {\n  filter row => row.a == 1\n  count\n}\n", "@unbounded", 1, "cannot be combined with `count`"},
	{"an inline concept line", "query thing q {\n  concept thing\n  filter row => row.a == 1\n}\n", "concept", 1, "inline `concept` line is no longer supported"},
	{"an unknown clause", "query thing q {\n  filter row => row.a == 1\n  limit 10\n}\n", "limit", 1, "unknown struct-query field"},
	{"a body block in a query", "query thing q {\n  body {\n    return 1\n  }\n}\n", "body", 1, "must not declare a `body { }` block"},
	{"a query with no concept", "query listThings {\n  filter row => row.a == 1\n}\n", "listThings", 1, "missing concept binding"},
	{"an unclosed query", "query thing q {\n  filter row => row.a == 1\n", "{", 1, "missing closing brace"},

	// Mutation blocks and fields.
	{"two write blocks", "mutate thing m {\n  args {\n    id string @required\n  }\n  insert {\n    id: args.id\n  }\n  update {\n    id: args.id\n  }\n}\n", "update", 1, "exactly one write block"},
	{"a write block restating its concept", "mutate thing m {\n  args {\n    id string @required\n  }\n  insert thing {\n    id: args.id\n  }\n}\n", "insert", 1, "is retired -- drop the restated concept"},
	{"an unknown block", "mutate thing m {\n  args {\n    id string @required\n  }\n  insert {\n    id: args.id\n  }\n  extra {\n    a: 1\n  }\n}\n", "extra", 1, "unexpected `extra { ... }` block"},
	{"a field outside the write block", "mutate thing m {\n  args {\n    id string @required\n  }\n  status: \"x\"\n  insert {\n    id: args.id\n  }\n}\n", "status: \"x\"", 1, "unexpected field"},
	{"a second nested accept", "mutate thing m {\n  args {\n    id string @required\n    name string\n  }\n  insert {\n    accept { id }\n    accept { name }\n  }\n}\n", "accept", 2, "more than one nested `accept"},
	{"a field beside a nested accept", "mutate thing m {\n  args {\n    id string @required\n  }\n  insert {\n    accept { id }\n    status: \"x\"\n  }\n}\n", "status: \"x\"", 1, "carries the field"},
	{"an accept entry that is a key: value", "mutate thing m {\n  args {\n    id string @required\n  }\n  accept { id, status: \"active\" }\n}\n", "status: \"active\"", 1, "looks like a `key: value` pair"},
	{"an accepted field with no arg", "mutate thing m {\n  args {\n    id string @required\n  }\n  accept { id, name }\n}\n", "name", 1, "has no matching arg"},
	{"an empty accept", "mutate thing m {\n  args {\n    id string @required\n  }\n  accept { }\n}\n", "accept", 1, "block is empty"},
	{"a top-level accept beside an insert", "mutate thing m {\n  args {\n    id string @required\n  }\n  accept { id }\n  insert {\n    id: args.id\n  }\n}\n", "accept", 1, "cannot mix the accept/stamp form with an explicit"},
	{"a second id", "mutate thing m {\n  args {\n    id string @required\n  }\n  insert {\n    id: args.id\n    id: args.id\n  }\n}\n", "id:", 2, "duplicate `id:` line"},
	{"a bare mirror of a path", "mutate thing m {\n  args {\n    id string @required\n    user object\n  }\n  insert {\n    id: args.id\n    args.user.id\n  }\n}\n", "args.user.id", 1, "has no key"},
	{"an update with no id", "mutate thing m {\n  args {\n    name string\n  }\n  update {\n    name: args.name\n  }\n}\n", "update", 1, "update block requires an `id: <expr>` line"},
	{"a body block in a mutation", "mutate thing m {\n  body {\n    return 1\n  }\n}\n", "body", 1, "must not declare a `body { }` block"},

	// Logic. A logic with no `body { }` block is written in statements, and
	// its refusals are the statement parser's and the load gate's rather than
	// the rewriter's (epic memql#5370): TestDiagnose_StatementRefusalLandsOnTheAuthorsText.
	{"a logic body with no return", "logic noReturn {\n  args {\n    a string\n  }\n  body {\n    x := f(a: args.a)\n  }\n}\n", "x :=", 1, "must end with a `return <expr>` terminator"},

	// Automation steps. An automation with no step block is written in
	// statements too, and so is out of this table the same way.
	{"an empty step", probeTrigger + "automation a {\n  step first {\n  }\n}\n", "step", 1, "body is empty"},
	{"a forEach with no in", probeTrigger + "automation a {\n  step loop {\n    forEach t of event.payload.items {\n      logic touch(x: t)\n    }\n  }\n}\n", "forEach", 1, "expected `in`"},
	{"a conditional step with no body", probeTrigger + "automation a {\n  step s {\n    if event.payload.a == 1\n  }\n}\n", "if", 1, "expected `{` after the if condition"},

	// A terse automation.
	{"an args block before a terse automation", "args {\n  x string\n}\nautomation onThing @trigger(event=\"node.created\", concept=\"v1:probe:thing\") => logic noteThing\n", "args", 1, "must not be preceded by an `args { ... }` block"},

	// Text the stages before moved: a spec StripNonProceduralBlocks folds to
	// one line, and a query the query stage lowers to fewer lines.
	{"a refine below a stripped spec and a lowered query", `use probe.concepts.{ thing }

spec thing isOpen = row => row.status == "open"

query thing first {
  args {
    a string @required
  }
  filter row => row.a == args.a
  sort "row.createdAt", "desc"
  paginate 10
}

query thing second {
  filter row => row.a == "x"
  refine row => row.b == 2
}
`, "refine", 1, "`refine` requires `paginate`"},
}

// nthAt is where the nth (1-based) occurrence of needle starts in src.
func nthAt(t *testing.T, src, needle string, nth int) Position {
	t.Helper()
	off, from := -1, 0
	for i := 0; i < nth; i++ {
		j := strings.Index(src[from:], needle)
		if j < 0 {
			t.Fatalf("%q occurs fewer than %d times", needle, nth)
		}
		off, from = from+j, from+j+1
	}
	lineStart := strings.LastIndex(src[:off], "\n") + 1
	return Position{Line: 1 + strings.Count(src[:off], "\n"), Column: 1 + len([]rune(src[lineStart:off]))}
}

// TestDiagnose_RewriteRefusalLandsOnTheAuthorsText: the lowering refusal is
// one diagnostic whose range is the clause keyword, field or step the
// rewriter refused, and whose message leads with that position.
func TestDiagnose_RewriteRefusalLandsOnTheAuthorsText(t *testing.T) {
	covers := map[string]string{"a second id": "id", "a logic body with no return": "x"}
	for _, c := range senseRewriteRefusalCases {
		t.Run(c.name, func(t *testing.T) {
			errs := errorDiags(New(nil).Diagnose(c.src, "probe/things.memql"))
			if len(errs) != 1 {
				t.Fatalf("want one error diagnostic, got %d: %+v", len(errs), errs)
			}
			d := errs[0]
			if !strings.Contains(d.Message, c.want) {
				t.Fatalf("refused for another reason: %s", d.Message)
			}
			text := c.needle
			if short, ok := covers[c.name]; ok {
				text = short
			}
			start := nthAt(t, c.src, c.needle, c.nth)
			end := Position{Line: start.Line, Column: start.Column + len([]rune(text))}
			if d.Range.Start != start || d.Range.End != end {
				t.Fatalf("range %d:%d-%d:%d, want %d:%d-%d:%d (the %q the author wrote): %s",
					d.Range.Start.Line, d.Range.Start.Column, d.Range.End.Line, d.Range.End.Column,
					start.Line, start.Column, end.Line, end.Column, text, d.Message)
			}
			if want := "rewrite error at line " + strconv.Itoa(start.Line) + ", column " + strconv.Itoa(start.Column) + ": "; !strings.HasPrefix(d.Message, want) {
				t.Fatalf("the message does not lead with the author's position %q: %s", want, d.Message)
			}
		})
	}
}
