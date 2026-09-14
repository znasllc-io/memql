package parser

import (
	"reflect"
	"testing"

	"github.com/znasllc-io/memql/component/language/annotations"
)

// Two readings classify an annotation's arguments for the registry check:
// AnnotationUse, over the attribute parseAttribute built (every construct and
// every field list but one), and peekAnnotationUse, over the tokens of an args
// block before the block parses them itself (memql#5359). A difference between
// them is a form one gate accepts and the other refuses.

// attributeUse parses text as one annotation with parseAttribute and converts
// it with AnnotationUse.
func attributeUse(t *testing.T, text string) (annotations.Use, error) {
	t.Helper()
	tokens, err := NewLexer(text).Tokenize()
	if err != nil {
		t.Fatalf("lex %q: %v", text, err)
	}
	attr, err := NewParser(tokens).parseAttribute()
	if err != nil {
		return annotations.Use{}, err
	}
	return AnnotationUse(attr), nil
}

// argsBlockUse classifies text the way the args block does: positioned after
// the `@name`, before the arguments are read.
func argsBlockUse(t *testing.T, text string) annotations.Use {
	t.Helper()
	tokens, err := NewLexer(text).Tokenize()
	if err != nil {
		t.Fatalf("lex %q: %v", text, err)
	}
	p := NewParser(tokens)
	p.advance() // `@`
	name := p.current.Literal
	p.advance()
	return p.peekAnnotationUse(name)
}

// TestArgsBlockAndAttributeClassifiersAgree: over every argument form
// parseAttribute produces, both readings make the same Use -- the form, and
// for keyword arguments the keys in written order with their shapes.
func TestArgsBlockAndAttributeClassifiersAgree(t *testing.T) {
	valued := func(name string) annotations.WrittenKey { return annotations.WrittenKey{Name: name} }
	bare := func(name string) annotations.WrittenKey { return annotations.WrittenKey{Name: name, Bare: true} }
	cases := []struct {
		text string
		want annotations.Use
	}{
		{"@x", annotations.Use{Name: "x", Form: annotations.FormFlag}},
		{"@x()", annotations.Use{Name: "x", Form: annotations.FormEmpty}},
		{`@x("a")`, annotations.Use{Name: "x", Form: annotations.FormString}},
		{`@x("a", "b")`, annotations.Use{Name: "x", Form: annotations.FormStrings}},
		{"@x(5)", annotations.Use{Name: "x", Form: annotations.FormNumber}},
		{"@x(2.5)", annotations.Use{Name: "x", Form: annotations.FormNumber}},
		// The lexer folds a `-` written against the digits into the number.
		{"@minimum(-1)", annotations.Use{Name: "minimum", Form: annotations.FormNumber}},
		{"@x(-2.5)", annotations.Use{Name: "x", Form: annotations.FormNumber}},
		{"@x(true)", annotations.Use{Name: "x", Form: annotations.FormBool}},
		{"@x(false)", annotations.Use{Name: "x", Form: annotations.FormBool}},
		{`@x({ "k": 1 })`, annotations.Use{Name: "x", Form: annotations.FormObject}},
		{`@x(!"a")`, annotations.Use{Name: "x", Form: annotations.FormExclude}},
		{`@x(!"a", !"b")`, annotations.Use{Name: "x", Form: annotations.FormExclude}},
		{"@filter(row => row.a == 1)", annotations.Use{Name: "filter", Form: annotations.FormExpression}},
		{`@filter(row => row.a == 1)`, annotations.Use{Name: "filter", Form: annotations.FormString}},
		{`@x(k="v")`, annotations.Use{Name: "x", Form: annotations.FormKeywords, Keys: []annotations.WrittenKey{valued("k")}}},
		{"@x(k)", annotations.Use{Name: "x", Form: annotations.FormKeywords, Keys: []annotations.WrittenKey{bare("k")}}},
		{`@x(zz="v", aa, mm=3)`, annotations.Use{Name: "x", Form: annotations.FormKeywords, Keys: []annotations.WrittenKey{valued("zz"), bare("aa"), valued("mm")}}},
		{`@x(k=[1, 2], j={ "a": 1 })`, annotations.Use{Name: "x", Form: annotations.FormKeywords, Keys: []annotations.WrittenKey{valued("k"), valued("j")}}},
		{`@x(k="a, b", j)`, annotations.Use{Name: "x", Form: annotations.FormKeywords, Keys: []annotations.WrittenKey{valued("k"), bare("j")}}},
		{`@x(as="y")`, annotations.Use{Name: "x", Form: annotations.FormKeywords, Keys: []annotations.WrittenKey{valued("as")}}},
	}
	for _, tc := range cases {
		fromAttribute, err := attributeUse(t, tc.text)
		if err != nil {
			t.Errorf("%s: parseAttribute refused a form this table says it produces: %v", tc.text, err)
			continue
		}
		fromArgsBlock := argsBlockUse(t, tc.text)
		if !reflect.DeepEqual(fromAttribute, tc.want) {
			t.Errorf("%s: AnnotationUse = %+v, want %+v", tc.text, fromAttribute, tc.want)
		}
		if !reflect.DeepEqual(fromArgsBlock, tc.want) {
			t.Errorf("%s: peekAnnotationUse = %+v, want %+v", tc.text, fromArgsBlock, tc.want)
		}
	}

	// The one spelling only the args block reads: a negative number whose
	// minus is SEPARATED from its digits. `- 1` lexes as the `-` operator and
	// the magnitude (only a minus written against the digits folds into the
	// number); parseNumericArgsAnnotation reads both tokens and parseAttribute
	// does not. Both halves are pinned, so teaching either reading the
	// spelling fails here until the other agrees.
	if u := argsBlockUse(t, "@minimum(- 1)"); u.Form != annotations.FormNumber {
		t.Errorf("@minimum(- 1): peekAnnotationUse = %+v, want a number", u)
	}
	if _, err := attributeUse(t, "@minimum(- 1)"); err == nil {
		t.Error("@minimum(- 1): parseAttribute now reads a separated minus -- AnnotationUse must classify it as the args block does (a number), and this pin moves into the table above")
	}
}
