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
