package parser

// rewrite_errors_test.go -- a refusal the struct-form rewriter makes names the
// author's line and column (memql#5364).

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// rewriteRefusalCases is one input per refusal the rewriter makes of authored
// text: needle (its nth occurrence, 1-based) is where the refusal must land,
// and want is in its message.
var rewriteRefusalCases = []struct {
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
	{"two write blocks", "mutation thing m {\n  args {\n    id string @required\n  }\n  insert {\n    id: args.id\n  }\n  update {\n    id: args.id\n  }\n}\n", "update", 1, "exactly one write block"},
	{"a write block restating its concept", "mutation thing m {\n  args {\n    id string @required\n  }\n  insert thing {\n    id: args.id\n  }\n}\n", "insert", 1, "is retired -- drop the restated concept"},
	{"an unknown block", "mutation thing m {\n  args {\n    id string @required\n  }\n  insert {\n    id: args.id\n  }\n  extra {\n    a: 1\n  }\n}\n", "extra", 1, "unexpected `extra { ... }` block"},
	{"a field outside the write block", "mutation thing m {\n  args {\n    id string @required\n  }\n  status: \"x\"\n  insert {\n    id: args.id\n  }\n}\n", "status: \"x\"", 1, "unexpected field"},
	{"a second nested accept", "mutation thing m {\n  args {\n    id string @required\n    name string\n  }\n  insert {\n    accept { id }\n    accept { name }\n  }\n}\n", "accept", 2, "more than one nested `accept"},
	{"a field beside a nested accept", "mutation thing m {\n  args {\n    id string @required\n  }\n  insert {\n    accept { id }\n    status: \"x\"\n  }\n}\n", "status: \"x\"", 1, "carries the field"},
	{"an accept entry that is a key: value", "mutation thing m {\n  args {\n    id string @required\n  }\n  accept { id, status: \"active\" }\n}\n", "status: \"active\"", 1, "looks like a `key: value` pair"},
	{"an accepted field with no arg", "mutation thing m {\n  args {\n    id string @required\n  }\n  accept { id, name }\n}\n", "name", 1, "has no matching arg"},
	{"an empty accept", "mutation thing m {\n  args {\n    id string @required\n  }\n  accept { }\n}\n", "accept", 1, "block is empty"},
	{"a top-level accept beside an insert", "mutation thing m {\n  args {\n    id string @required\n  }\n  accept { id }\n  insert {\n    id: args.id\n  }\n}\n", "accept", 1, "cannot mix the accept/stamp form with an explicit"},
	{"a second id", "mutation thing m {\n  args {\n    id string @required\n  }\n  insert {\n    id: args.id\n    id: args.id\n  }\n}\n", "id:", 2, "duplicate `id:` line"},
	{"a bare mirror of a path", "mutation thing m {\n  args {\n    id string @required\n    user object\n  }\n  insert {\n    id: args.id\n    args.user.id\n  }\n}\n", "args.user.id", 1, "has no key"},
	{"an update with no id", "mutation thing m {\n  args {\n    name string\n  }\n  update {\n    name: args.name\n  }\n}\n", "update", 1, "update block requires an `id: <expr>` line"},
	{"a body block in a mutation", "mutation thing m {\n  body {\n    return 1\n  }\n}\n", "body", 1, "must not declare a `body { }` block"},

	// A logic and an automation are not the rewriter's: the statement parser
	// reads them as written, and their refusals are its own
	// (v1BodyRefusalCases) and the compiler's (epic memql#5370).

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

// lowerAuthored runs the rewrite the way compiler.ParseFileSource and Sense
// do -- StripNonProceduralBlocks, then NormaliseAll -- and places a refusal in
// src.
func lowerAuthored(src string) error {
	stripped := src
	if LooksLikeNonProcedural(src) {
		stripped = StripNonProceduralBlocks(src)
	}
	_, err := NormaliseAll(stripped)
	return PositionRewriteError(src, err)
}

// TestRewriteRefusalsNameTheAuthorsPosition: every refusal the rewriter makes
// of authored text is placed on that text as the author wrote it -- its line
// and column in the message, its extent in the ParseError it carries.
func TestRewriteRefusalsNameTheAuthorsPosition(t *testing.T) {
	for _, c := range rewriteRefusalCases {
		t.Run(c.name, func(t *testing.T) {
			err := lowerAuthored(c.src)
			if err == nil {
				t.Fatalf("the rewrite accepted:\n%s", c.src)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("refused for another reason: %v", err)
			}
			var placed *PositionedRewriteError
			if !errors.As(err, &placed) {
				t.Fatalf("the refusal carries no position: %T %v", err, err)
			}
			line, col := authoredAt(t, c.src, c.needle, c.nth)
			if gl, gc := placed.Parse.Position(); gl != line || gc != col {
				t.Fatalf("refusal at %d:%d, want %d:%d (the %q the author wrote): %v", gl, gc, line, col, c.needle, err)
			}
			prefix := "rewrite error at line " + strconv.Itoa(line) + ", column " + strconv.Itoa(col) + ": "
			if !strings.HasPrefix(err.Error(), prefix) {
				t.Fatalf("the message does not lead with the author's position %q: %v", prefix, err)
			}
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatal("a placed refusal must unwrap to the *ParseError editors read")
			}
			endLine, endCol := pe.EndPosition()
			if endLine < line || (endLine == line && endCol <= col) {
				t.Fatalf("the refusal's extent %d:%d-%d:%d is empty", line, col, endLine, endCol)
			}
			var re *RewriteError
			if !errors.As(err, &re) {
				t.Fatal("the placed refusal must still unwrap to the rewriter's *RewriteError")
			}
		})
	}
}

// TestRewriteRefusalCoversItsText: a refusal of a clause or field covers the
// text it names, so a squiggle is that keyword or field and nothing past it.
func TestRewriteRefusalCoversItsText(t *testing.T) {
	// Where the text covered is shorter than the needle that finds it: a
	// second `id:` field is named at its key.
	covers := map[string]string{"a second id": "id"}
	for _, c := range rewriteRefusalCases {
		t.Run(c.name, func(t *testing.T) {
			var pe *ParseError
			if !errors.As(lowerAuthored(c.src), &pe) {
				t.Fatal("no positioned refusal")
			}
			text := c.needle
			if short, ok := covers[c.name]; ok {
				text = short
			}
			line, col := authoredAt(t, c.src, c.needle, c.nth)
			if endLine, endCol := pe.EndPosition(); endLine != line || endCol != col+len(text) {
				t.Errorf("refusal covers %d:%d-%d:%d, want %d:%d-%d:%d, the text %q", line, col, endLine, endCol, line, col, line, col+len(text), text)
			}
		})
	}
}

// TestPositionRewriteErrorLeavesOtherErrors: an error with no rewriter
// refusal in it, and nil, come back as they were.
func TestPositionRewriteErrorLeavesOtherErrors(t *testing.T) {
	if PositionRewriteError("x", nil) != nil {
		t.Error("nil became an error")
	}
	plain := errors.New("plain")
	if PositionRewriteError("x", plain) != plain {
		t.Error("a plain error was rewrapped")
	}
	err := lowerAuthored(rewriteRefusalCases[0].src)
	if again := PositionRewriteError(rewriteRefusalCases[0].src, err); again != err {
		t.Error("placing a placed refusal again changed it")
	}
}

// TestLegacyProceduralFormRefusalNamesItsFunc: the load gate that refuses an
// author's `func (Query) name(...)` names the `func` it refuses.
func TestLegacyProceduralFormRefusalNamesItsFunc(t *testing.T) {
	src := "/// A legacy query.\n@enabled\n  func (Query) things(ctx any) (any, error) {\n  return concept==v1:probe:thing, nil\n}\n"
	err := PositionRewriteError(src, RejectLegacyProceduralAuthorForm(src))
	if err == nil || !strings.Contains(err.Error(), "legacy procedural form `func (Query) ...` is retired") {
		t.Fatalf("got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "rewrite error at line 3, column 3: ") {
		t.Fatalf("got %q, want the refusal placed at the `func` on line 3, column 3", err.Error())
	}
}
