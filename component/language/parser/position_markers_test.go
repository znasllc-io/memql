package parser

// position_markers_test.go -- a refusal inside a construct the rewriter
// lowered names the author's line and column (memql#5364).
//
// memqlmigrate:keep-file -- the legacy spellings in this file are its cases:
// each is a refusal whose position is the assertion, so the fixture codemod
// must leave them as written.

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/core/repowalk"
)

// authoredAt is where the nth (1-based) occurrence of needle starts in src,
// as an author counts it: 1-based line, 1-based rune column.
func authoredAt(t *testing.T, src, needle string, nth int) (line, col int) {
	t.Helper()
	off, from := -1, 0
	for i := 0; i < nth; i++ {
		j := strings.Index(src[from:], needle)
		if j < 0 {
			t.Fatalf("%q occurs fewer than %d times in the source", needle, nth)
		}
		off = from + j
		from = off + 1
	}
	lineStart := strings.LastIndex(src[:off], "\n") + 1
	return 1 + strings.Count(src[:off], "\n"), 1 + utf8.RuneCountInString(src[lineStart:off])
}

// TestLexerReadsPositionMarkers: an `@at` marker places the token after it and
// the text after that advances from there, across lines; an `@pin` marker
// holds every token to one position; the text before the first marker is the
// identity; a comment that is not a well-formed marker is only a comment.
func TestLexerReadsPositionMarkers(t *testing.T) {
	src := "a /*@at 10:5*/b c\nd /*@pin 3:7*/e f\n/*@at 20:1*/g\n/*@todo*/h /*@at x:1*/i"
	tokens, err := NewLexer(src).Tokenize()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][4]int{ // start line, start col, end line, end col
		"a": {1, 1, 1, 2},
		"b": {10, 5, 10, 6},
		"c": {10, 7, 10, 8},
		"d": {11, 1, 11, 2},
		"e": {3, 7, 3, 8},
		"f": {3, 7, 3, 8},
		"g": {20, 1, 20, 2},
		"h": {21, 10, 21, 11},
		"i": {21, 23, 21, 24},
	}
	for _, tok := range tokens {
		if tok.Type == TokenEOF {
			continue
		}
		w, ok := want[tok.Literal]
		if !ok {
			t.Fatalf("unexpected token %q", tok.Literal)
		}
		got := [4]int{tok.AuthoredLine, tok.AuthoredCol, tok.AuthoredEndLine, tok.AuthoredEndCol}
		if got != w {
			t.Errorf("%s: authored extent %v, want %v", tok.Literal, got, w)
		}
	}

	// A text carrying no marker has no authored positions: At is the lexed
	// position, and every consumer keeps its own.
	plain, err := NewLexer("a b\nc").Tokenize()
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range plain {
		if tok.AuthoredLine != 0 || tok.AuthoredCol != 0 {
			t.Errorf("unmarked %q carries an authored position", tok.Literal)
		}
	}
	if line, col := plain[2].At(); line != 2 || col != 1 {
		t.Errorf("At of an unmarked token = %d:%d, want its lexed 2:1", line, col)
	}

	// A lexer error inside a marked text names the authored position too.
	_, err = NewLexer("/*@at 40:9*/a\n  b /* never closed").Tokenize()
	if err == nil || !strings.Contains(err.Error(), "at line 41, column 5") {
		t.Errorf("an unterminated comment in a marked text: %v, want line 41, column 5", err)
	}
}

// TestStripPositionMarkers: only markers come out; other comments stay.
func TestStripPositionMarkers(t *testing.T) {
	in := "a /*@at 3:4*/b /* note */ c/*@pin 1:1*/ /*@todo*/"
	if got, want := StripPositionMarkers(in), "a b /* note */ c /*@todo*/"; got != want {
		t.Errorf("StripPositionMarkers = %q, want %q", got, want)
	}
}

// v1PositionCases is one refusal per place the rewriter moves or shifts author
// text: needle (its nth occurrence) is the token the refusal must land on, in
// the source exactly as authored.
var v1PositionCases = []struct {
	name   string
	src    string
	needle string
	nth    int
	want   string // in the message
}{
	{
		name: "filter after an args block",
		src: `use probe.concepts.{ thing }

/// Things by a.
query thing byA {
  args {
    a string @required
  }
  filter row => row.a == args.a && row.b == null
  paginate 10
}
`,
		needle: "null", nth: 1, want: "null is retired",
	},
	{
		name: "a filter's continuation line",
		src: `query thing byA {
  filter row => row.a == "x" &&
         row.b == null
  paginate 10
}
`,
		needle: "null", nth: 1, want: "null is retired",
	},
	{
		name: "the second query in a file",
		src: `query thing first {
  args {
    a string @required
  }
  filter row => row.a == args.a
  paginate 10
}

query thing second {
  filter row => row.b == null
  paginate 10
}
`,
		needle: "null", nth: 1, want: "null is retired",
	},
	{
		name: "refine",
		src: `query thing refined {
  filter row => row.a == "x"
  paginate 10
  refine row => row.c == null
}
`,
		needle: "null", nth: 1, want: "null is retired",
	},
	{
		name: "refine that is not a lambda",
		src: `query thing refined {
  filter row => row.a == "x"
  paginate 10
  refine row.c == nil
}
`,
		needle: "row.c", nth: 1, want: "refine takes a lambda of one parameter",
	},
	{
		name: "a hyphen glued into a filter's name",
		src: `query thing kebab {
  filter row => row.total-used > 0
}
`,
		needle: "row.total-used", nth: 1, want: "write `row.total - row.used`",
	},
	{
		name: "a legacy filter",
		src: `query thing legacy {
  args {
    a string
  }
  filter a == 1
  paginate 5
}
`,
		needle: "a == 1", nth: 1, want: "filter <predicate> is retired",
	},
	{
		name: "@filter on an automation below a query",
		src: `query thing first {
  args {
    a string @required
  }
  filter row => row.a == args.a
  sort "row.createdAt", "desc"
  paginate 10
  shape thingCard
}

@filter(row => row.b == null)
@trigger(event="node.created", concept="v1:probe:thing")
automation probe {
  step first {
    logic other(x: 1)
  }
}
`,
		needle: "null", nth: 1, want: "null is retired",
	},
	{
		name: "@filter on a terse automation, hoisted by the lowering",
		src: `/// Note a thing when it is created.
automation onThing @trigger(event="node.created", concept="v1:probe:thing") @filter(row => row.b == null) => logic noteThing
`,
		needle: "null", nth: 1, want: "null is retired",
	},
	{
		name: "@trigger's filter argument, below a query",
		src: `query thing first {
  filter row => row.a == "x"
  sort "row.createdAt", "desc"
  paginate 10
}

@trigger(event="node.created", concept="v1:probe:thing", filter=row => row.b == null)
automation probe {
  step first {
    logic other(x: 1)
  }
}
`,
		needle: "null", nth: 1, want: "null is retired",
	},
	{
		name: "a spec below a query",
		src: `query thing first {
  args {
    a string @required
  }
  filter row => row.a == args.a
  sort "row.createdAt", "desc"
  paginate 10
  shape thingCard
}

spec thing isB = row => row.b == null
`,
		needle: "null", nth: 1, want: "null is retired",
	},
	{
		name: "a trait below a query",
		src: `query thing first {
  filter row => row.a == "x"
  paginate 10
}

trait isB = row => row.b == null
`,
		needle: "null", nth: 1, want: "null is retired",
	},
	{
		name: "a mutation value",
		src: `mutation thing probe {
  args {
    id string @required
  }
  insert {
    id: args.id
    note: null
  }
}
`,
		needle: "null", nth: 1, want: "null is retired",
	},
	{
		name: "a logic return",
		src: `logic probe {
  args {
    a bool
  }
  body {
    return cond(args.a, 1, 2)
  }
}
`,
		needle: "cond", nth: 1, want: "cond(p, a, b) is retired",
	},
	{
		name: "a step condition",
		src: `@trigger(event="node.created", concept="v1:probe:thing")
automation probe {
  step first {
    logic runIt(mode: "x")
  }
  step second {
    if event.payload.y != null {
      builtin doIt(id: event.payload.id)
    }
  }
}
`,
		needle: "null", nth: 1, want: "null is retired",
	},
	{
		name: "a forEach where filter",
		src: `@trigger(event="node.created", concept="v1:probe:thing")
automation probe {
  step loop {
    forEach t in event.payload.items where t.active == null {
      touch { id: t.id }
    }
  }
}
`,
		needle: "null", nth: 1, want: "null is retired",
	},
	{
		name: "a step argument map entry with no key",
		src: `@trigger(event="node.created", concept="v1:probe:thing")
automation probe {
  step first {
    logic record(payload: { delegationId: event.payload.id, event.payload.identityId })
  }
}
`,
		needle: "event.payload.identityId", nth: 1, want: "a map entry needs a key: write identityId: event.payload.identityId",
	},
}

// TestV1RefusalsNameTheAuthorsPosition: at every place the rewriter moves or
// shifts author text, the refusal's position -- its fields and the line and
// column its message prints -- is the offending token as authored.
func TestV1RefusalsNameTheAuthorsPosition(t *testing.T) {
	for _, c := range v1PositionCases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseV1Authored(t, c.src)
			if err == nil {
				t.Fatalf("accepted:\n%s", c.src)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("refused for another reason: %v", err)
			}
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("not a positioned refusal: %T %v", err, err)
			}
			wantLine, wantCol := authoredAt(t, c.src, c.needle, c.nth)
			if line, col := pe.Position(); line != wantLine || col != wantCol {
				t.Fatalf("refusal at %d:%d, want %d:%d (the %q the author wrote): %v", line, col, wantLine, wantCol, c.needle, err)
			}
			if at := "at line " + strconv.Itoa(wantLine) + ", column " + strconv.Itoa(wantCol) + ":"; !strings.Contains(err.Error(), at) {
				t.Fatalf("the message does not print the author's position %q: %v", at, err)
			}
		})
	}
}

// TestV1RefusalCoversTheOffendingToken: a refusal's end is the offending
// token's end, so an editor's squiggle covers exactly it -- and a legacy
// filter's refusal covers its whole predicate, the clause the rewrite
// converts, on the filter's own line.
func TestV1RefusalCoversTheOffendingToken(t *testing.T) {
	for _, c := range v1PositionCases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseV1Authored(t, c.src)
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("not a positioned refusal: %v", err)
			}
			line, col := authoredAt(t, c.src, c.needle, c.nth)
			token := c.needle
			if c.needle == "cond" || c.needle == "row.c" {
				// The refusal names the call, and the lambda check its first
				// operand; the squiggle is that token.
				token = strings.Fields(strings.NewReplacer("(", " ", ")", " ").Replace(c.needle))[0]
			}
			if endLine, endCol := pe.EndPosition(); endLine != line || endCol != col+utf8.RuneCountInString(token) {
				t.Errorf("refusal ends at %d:%d, want %d:%d, the end of %q", endLine, endCol, line, col+utf8.RuneCountInString(token), token)
			}
		})
	}
}

// TestV1OpenerPositionIsTheAuthors: a message that points back at an opener
// ("the `(` at line L, column C") points at the author's opener too.
func TestV1OpenerPositionIsTheAuthors(t *testing.T) {
	src := `query thing grouped {
  args {
    a string
  }
  filter row => row.a == args.a && (row.b == 1 || row.c == 2 row.d)
  paginate 5
}
`
	_, err := parseV1Authored(t, src)
	if err == nil {
		t.Fatal("accepted a group missing its `)`")
	}
	line, col := authoredAt(t, src, "(row.b", 1)
	if want := "the `(` at line " + strconv.Itoa(line) + ", column " + strconv.Itoa(col); !strings.Contains(err.Error(), want) {
		t.Fatalf("got %v, want the message to name %q", err, want)
	}
}

// TestV1SpansAreAuthored: a v1 node's Span is where it sits in the source as
// authored -- a runtime error quotes it.
func TestV1SpansAreAuthored(t *testing.T) {
	src := `query thing spanned {
  args {
    a string @required
  }
  filter row => row.a == args.a
  paginate 10
}
`
	fn := onlyFunction(t, mustParseV1Authored(t, src))
	join, ok := queryBase(fn.Body.(ast.ExpressionNode)).(*LogicalExpr)
	if !ok {
		t.Fatalf("query base is %T", queryBase(fn.Body.(ast.ExpressionNode)))
	}
	lam, ok := join.Right.(*ast.LambdaExpr)
	if !ok {
		t.Fatalf("the filter is %T", join.Right)
	}
	body, ok := lam.Body.(*ast.BinaryExpr)
	if !ok {
		t.Fatalf("the filter's body is %T", lam.Body)
	}
	line, col := authoredAt(t, src, "row.a ==", 1)
	endLine, endCol := authoredAt(t, src, "args.a\n", 1)
	endCol += len("args.a")
	if got := body.Span; got.Line != line || got.Col != col || got.EndLine != endLine || got.EndCol != endCol {
		t.Fatalf("the filter body's span is %d:%d-%d:%d, want %d:%d-%d:%d", got.Line, got.Col, got.EndLine, got.EndCol, line, col, endLine, endCol)
	}
}

// TestPositionLoweringKeepsEveryToken: over every file of the DSL tree, the
// marked lowering lexes to the same tokens, on the same lines, and parses to
// the same outcome as the unmarked one -- markers change positions and
// nothing else -- and every position it hands out is inside the file.
func TestPositionLoweringKeepsEveryToken(t *testing.T) {
	root := "../../../dsl"
	var files, marked int
	var spent time.Duration
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && repowalk.SkipDir(d.Name()) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".memql") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		src := string(raw)
		stripped := src
		if LooksLikeNonProcedural(src) {
			stripped = StripNonProceduralBlocks(src)
		}
		lowered, err := NormaliseAll(stripped)
		if err != nil {
			return nil // a lowering refusal is not this test's business
		}
		files++
		began := time.Now()
		withMarkers := PositionLowering(src, lowered)
		spent += time.Since(began)
		if withMarkers != lowered {
			marked++
		}
		plainTokens, plainErr := NewLexer(lowered).Tokenize()
		markedTokens, markedErr := NewLexer(withMarkers).Tokenize()
		if (plainErr == nil) != (markedErr == nil) {
			t.Errorf("%s: lexing changed outcome: %v vs %v", path, plainErr, markedErr)
			return nil
		}
		if plainErr != nil {
			return nil
		}
		if len(plainTokens) != len(markedTokens) {
			t.Errorf("%s: %d tokens unmarked, %d marked", path, len(plainTokens), len(markedTokens))
			return nil
		}
		lines := 1 + strings.Count(src, "\n")
		for i := range plainTokens {
			p, m := plainTokens[i], markedTokens[i]
			if p.Type != m.Type || p.Literal != m.Literal || p.Line != m.Line {
				t.Errorf("%s: token %d is %v %q on line %d unmarked, %v %q on line %d marked", path, i, p.Type, p.Literal, p.Line, m.Type, m.Literal, m.Line)
				return nil
			}
			if withMarkers != lowered && (m.AuthoredLine < 1 || m.AuthoredLine > lines || m.AuthoredCol < 1) {
				t.Errorf("%s: token %q maps to %d:%d, outside the file's %d lines", path, m.Literal, m.AuthoredLine, m.AuthoredCol, lines)
				return nil
			}
		}
		plainFile, plainParse := ParseFile(lowered)
		markedFile, markedParse := ParseFile(withMarkers)
		switch {
		case (plainParse == nil) != (markedParse == nil):
			t.Errorf("%s: parsing changed outcome: %v vs %v", path, plainParse, markedParse)
		case plainParse != nil:
			var a, b *ParseError
			if errors.As(plainParse, &a) && errors.As(markedParse, &b) && a.Message != b.Message {
				t.Errorf("%s: the refusal changed: %q vs %q", path, a.Message, b.Message)
			}
		case len(plainFile.Definitions) != len(markedFile.Definitions):
			t.Errorf("%s: %d definitions unmarked, %d marked", path, len(plainFile.Definitions), len(markedFile.Definitions))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files == 0 || marked == 0 {
		t.Fatalf("walked %d lowered files, %d of them marked: the corpus did not reach the test", files, marked)
	}
	t.Logf("%d files lowered, %d marked, %v spent marking", files, marked, spent)
}
