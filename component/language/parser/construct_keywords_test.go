package parser

import (
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
