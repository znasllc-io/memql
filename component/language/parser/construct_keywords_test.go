package parser

import (
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The construct keywords are DERIVED from the parser's own tables -- the
// struct-form rewriter chain and the contextual dispatch table -- plus the
// file-top `use` import, never restated (memql#5358 review).
func TestConstructKeywordsDeriveFromTheParserTables(t *testing.T) {
	want := map[string]bool{"use": true}
	for _, k := range StructFormKeywords {
		want[k] = true
	}
	for _, k := range TopLevelDeclKeywords {
		want[k] = true
	}
	var wantList []string
	for k := range want {
		wantList = append(wantList, k)
	}
	sort.Strings(wantList)
	if got := ConstructKeywords(); !reflect.DeepEqual(got, wantList) {
		t.Errorf("ConstructKeywords() = %v, want %v", got, wantList)
	}
}

func TestFindUnknownConstructKeywords(t *testing.T) {
	src := strings.Join([]string{
		`use demo.concepts.{ item }`, // 1
		``,
		`/// A doc comment saying predicate foo { is not code.`, // 3
		`@description("predicate in a string (")`,
		`@relationship(type="references",`, // 5
		`  field="x", target=item)`,
		`concept item {`, // 7
		`  predicate  string`,
		`}`, // 9
		``,
		`qurey item listItems {`, // 11
		`  filter name == "x"`,
		`}`, // 13
		``,
		`predicate itemPredicate {`, // 15
		`  return active == true`,
		`}`, // 17
		``,
		`func (Query) legacyForm(ctx any) { }`, // 19: refused elsewhere, with its migration hint
		`func helper() { }`,                    // 20
		`automation terse @trigger(event="a") => logic x`,
		`  => logic continued`, // 22
	}, "\n")
	got := FindUnknownConstructKeywords(src)

	type hit struct {
		line    int
		keyword string
	}
	var hits []hit
	for _, u := range got {
		hits = append(hits, hit{u.Line, u.Keyword})
		if !strings.HasSuffix(u.Message, " ["+CodeConstructUnknown+"]") {
			t.Errorf("line %d: message must end with \" [%s]\", got %q", u.Line, CodeConstructUnknown, u.Message)
		}
	}
	want := []hit{{11, "qurey"}, {15, "predicate"}, {20, "func"}}
	if !reflect.DeepEqual(hits, want) {
		t.Fatalf("unknown construct keywords = %+v, want %+v", hits, want)
	}

	all := strings.Join(ConstructKeywords(), ", ")
	if wantMsg := "qurey is not a construct keyword: did you mean query? The constructs are: " + all + " [construct_unknown]"; got[0].Message != wantMsg {
		t.Errorf("did-you-mean message:\n got %q\nwant %q", got[0].Message, wantMsg)
	}
	if wantMsg := "predicate is not a construct keyword. The constructs are: " + all + " [construct_unknown]"; got[1].Message != wantMsg {
		t.Errorf("no-suggestion message:\n got %q\nwant %q", got[1].Message, wantMsg)
	}
}

// The retired file-top `import ( ... )` block loaded before epic memql#5356;
// it is refused now, BY NAME, with its replacement -- not with the generic
// keyword list. Wherever the parser cannot take the word as a statement
// (after a construct), it raises the same refusal, so one statement is one
// refusal.
func TestTheImportBlockIsRefusedNamingUse(t *testing.T) {
	const want = "the import ( ... ) block is retired: a construct is imported with a file-top use line, use <domain>.<file>.{ names } [construct_unknown]"
	got := FindUnknownConstructKeywords("import (\n\t\"../shop/concepts\"\n)\n\nconcept a {\n  b string\n}\n")
	if len(got) != 1 || got[0].Line != 1 || got[0].Keyword != "import" || got[0].Message != want {
		t.Fatalf("the import block: got %+v, want one refusal at line 1 reading %q", got, want)
	}

	_, err := ParseFile("concept a {\n  b string\n}\nimport (\n\t\"x\"\n)\n")
	var cause *UnknownConstructKeyword
	if !errors.As(err, &cause) || cause.Message != want {
		t.Errorf("an import the parser cannot take must raise the gate's refusal, got: %v", err)
	}
}

// The parser refuses a top-level token by its KIND. Only a word is a
// statement the load gate reads, so only a word gets construct_unknown: a
// stray string keeps the unexpected-token error, and the gate says nothing
// about it either. A `use` line after a construct is told where use lines go,
// and `use` is not offered among the words expected there.
func TestTheParserRefusesATopLevelTokenByItsKind(t *testing.T) {
	src := "concept a {\n  b string\n}\n\"hello\"\n"
	_, err := ParseFile(src)
	var cause *UnknownConstructKeyword
	if err == nil || errors.As(err, &cause) || strings.Contains(err.Error(), "[construct_unknown]") ||
		!strings.Contains(err.Error(), `unexpected token "hello"`) {
		t.Errorf("a stray string must be an unexpected token, not construct_unknown; got: %v", err)
	}
	if got := FindUnknownConstructKeywords(src); len(got) != 0 {
		t.Errorf("the load gate reads no statement in a stray string, and must say nothing; got %+v", got)
	}

	_, err = ParseFile("concept a {\n  b string\n}\nuse shop.concepts.{ order }\n")
	if err == nil || !strings.Contains(err.Error(), "a use line must come before the file's first construct") {
		t.Fatalf("a use line after a construct must be told where use lines go; got: %v", err)
	}
	if strings.Contains(err.Error(), "one of") {
		t.Errorf("the refusal of a late use line must not list `use` among the words expected; got: %v", err)
	}
}

// Braces, parens and brackets that open and close on one line, and an
// unbalanced closer, leave the scan in step with the file.
func TestFindUnknownConstructKeywordsStaysInStep(t *testing.T) {
	src := "}\nbogus thing {\n}\nshape a b { x }\nqurey x y {\n}\n"
	got := FindUnknownConstructKeywords(src)
	if len(got) != 2 || got[0].Keyword != "bogus" || got[0].Line != 2 || got[1].Keyword != "qurey" || got[1].Line != 5 {
		t.Errorf("got %+v, want bogus at line 2 and qurey at line 5", got)
	}
	if got := FindUnknownConstructKeywords("concept a {\n  b string\n}\n"); len(got) != 0 {
		t.Errorf("a clean file reported %+v", got)
	}
}

// TopLevelStatements reads the same lines FindUnknownConstructKeywords does,
// and names what each declares in every construct form: the struct forms, a
// two-identifier signature, a predicate's `=` body, the terse automation
// header and the loose one whose brace is on the next line. A construct
// keyword inside a body -- an automation's own `automation x(...)` call, a
// query statement in a logic, a keyword inside a string or a comment -- is
// not top level (memql#5437).
func TestTopLevelStatementsNameWhatEachDeclares(t *testing.T) {
	src := strings.Join([]string{
		`use fleet.logic.{ runningInstances }`, // 1
		``,
		`/// automation docOnly { is a comment.`, // 3
		`concept brief {`,                        // 4
		`  note string @description("automation inString {")`,
		`}`,
		`query brief briefsForOwner {`, // 7
		`  filter row => row.note == "x"`,
		`}`,
		`spec brief hasNote = row => row.note != nil`, // 10
		`trait isOpen = row => row.open == true`,      // 11
		`@trigger(event="node.created", concept="v1:x:brief")`,
		`automation strict {`, // 13
		`  automation nested(x: 1)`,
		`  sources := query briefsForOwner()`,
		`}`,
		`automation loose`, // 17
		`{`,
		`}`,
		`automation terse @trigger(event="a") => logic x`, // 20
		`seed brief first {`,                              // 21
		`  note: "automation notADecl {"`,
		`}`,
		// A declaration name may carry hyphens -- the seeded role and skill
		// catalogs are kebab-case -- and the name is all of it, not the
		// segment after the last hyphen.
		`seed agentRole row-crop-farmer {`, // 24
		`}`,
		`spec brief has-note = row => row.note != nil`,                       // 26
		`automation night-shift @trigger(schedule="0 0 * * * *") => logic x`, // 27
	}, "\n")
	type stmt struct {
		line          int
		keyword, name string
	}
	var got []stmt
	for _, s := range TopLevelStatements(src) {
		got = append(got, stmt{s.Line, s.Keyword, s.Name})
	}
	want := []stmt{
		{1, "use", ""},
		{4, "concept", "brief"},
		{7, "query", "briefsForOwner"},
		{10, "spec", "hasNote"},
		{11, "trait", "isOpen"},
		{13, "automation", "strict"},
		{17, "automation", "loose"},
		{20, "automation", "terse"},
		{21, "seed", "first"},
		{24, "seed", "row-crop-farmer"},
		{26, "spec", "has-note"},
		{27, "automation", "night-shift"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("TopLevelStatements =\n  %+v\nwant\n  %+v", got, want)
	}
}

// TestFindMisplacedUseLines (memql#5426): a `use` line below the file's first
// construct is found where the parser refuses it -- at the top level, after a
// non-`use` statement -- with its line and the module it names; one above
// every construct, one inside a construct's braces, and one in a comment or a
// string are not. The parser's own refusal of the line carries the same
// message and the same rule id, as its cause.
func TestFindMisplacedUseLines(t *testing.T) {
	src := `use shop.concepts.{ order }

// use shop.concepts.{ inComment }
concept a {
  b string  @description("use shop.concepts.{ inString }")
}

use shop.queries.{ openOrders }

use shop.shapes.{
  orderCard
}
`
	got := FindMisplacedUseLines(src)
	want := []MisplacedUse{
		{Line: 8, Path: "shop.queries", Message: misplacedUseMessage},
		{Line: 10, Path: "shop.shapes", Message: misplacedUseMessage},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("finding %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if !strings.HasSuffix(misplacedUseMessage, "["+CodeUseNotFileTop+"]") {
		t.Errorf("the message does not end with its rule id: %q", misplacedUseMessage)
	}

	_, err := ParseFile(src)
	var cause *MisplacedUse
	if err == nil || !errors.As(err, &cause) || cause.RuleCode() != CodeUseNotFileTop ||
		!strings.Contains(err.Error(), misplacedUseMessage) {
		t.Fatalf("the parser's refusal must carry the gate's message and code as its cause; got %v", err)
	}
	if clean := FindMisplacedUseLines("use shop.concepts.{ order }\n\nconcept a {\n  b string\n}\n"); len(clean) != 0 {
		t.Errorf("a file-top use line was reported: %+v", clean)
	}
}
