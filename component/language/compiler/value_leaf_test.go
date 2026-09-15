package compiler

import (
	"encoding/json"
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/parser"
)

// TestEncodeValueLeafRule pins the one encoding rule, case by case.
func TestEncodeValueLeafRule(t *testing.T) {
	parse := func(src string) ast.ExpressionNode {
		n, err := parser.ParseV1Expression(src)
		if err != nil {
			t.Fatalf("parse %s: %v", src, err)
		}
		return n
	}
	for src, want := range map[string]string{
		`"s"`:                     `"s"`,
		`12`:                      `12`,
		`1.5`:                     `1.5`,
		`true`:                    `true`,
		`nil`:                     `null`,
		`("paren")`:               `"paren"`,
		`args.x`:                  `{"$expr":"args.x"}`,
		`(args.a + args.b)`:       `{"$expr":"args.a + args.b"}`,
		`args.x ?? "d"`:           `{"$expr":"args.x ?? \"d\""}`,
		`[1, args.x]`:             `[1,{"$expr":"args.x"}]`,
		`{a: 1, b: args.x}`:       `{"a":1,"b":{"$expr":"args.x"}}`,
		`{a: {b: [nil, args.y]}}`: `{"a":{"b":[null,{"$expr":"args.y"}]}}`,
		`{}`:                      `{}`,
		`[]`:                      `[]`,
	} {
		b, err := json.Marshal(EncodeValueLeaf(parse(src)))
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != want {
			t.Errorf("EncodeValueLeaf(%s) = %s, want %s", src, b, want)
		}
	}
	if EncodeValueLeaf(nil) != nil {
		t.Error("EncodeValueLeaf(nil) is not null")
	}
}
