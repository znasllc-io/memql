package memql

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// prompt_default_5430_test.go -- memql#5430, the prompt half: a prompt field's
// @default converts to the field's type by the rule a tool field's does. Its
// placement also admits an unquoted number, which is read as the text it was
// written as, so `@default(3)` and `@default("3")` are one default.

func TestPromptDefaultIsALiteralOfTheFieldType(t *testing.T) {
	type tc struct {
		typ, elem string
		enum      []string
		text      string
		number    bool // written unquoted
		want      any
		fault     string
	}
	cases := []tc{
		{typ: "string", text: "en", want: "en"},
		{typ: "string", text: "", want: ""},
		{typ: "int", text: "3", want: int64(3)},
		{typ: "int", text: "3", number: true, want: int64(3)},
		{typ: "float", text: "0.5", number: true, want: 0.5},
		{typ: "number", text: "2", want: int64(2)},
		{typ: "float", text: "9007199254740993", number: true, want: int64(9007199254740993)},
		{typ: "int", text: "9007199254740993", number: true, want: int64(9007199254740993)},
		// An unquoted number is the text it spells, on any field.
		{typ: "string", text: "5", number: true, want: "5"},
		{typ: "boolean", text: "1", number: true, want: true},
		{typ: "boolean", text: "true", want: true},
		{typ: "array", elem: "object", text: "[]", want: []any{}},
		{typ: "object", text: `{"a": 1}`, want: map[string]any{"a": float64(1)}},
		// A type the schema publishes as a string takes any text.
		{typ: "datetime", text: "2026-01-02T03:04:05Z", want: "2026-01-02T03:04:05Z"},
		{typ: "string", enum: []string{"short", "long"}, text: "long", want: "long"},

		{typ: "int", text: "three", fault: "is not an integer"},
		{typ: "int", text: "2.5", number: true, fault: "@default(2.5) is not an integer"},
		// 1e3 is 1000 once parsed, but it is not what an integer field reads:
		// unquoted, it is refused as its quoted spelling is.
		{typ: "int", text: "1e3", number: true, fault: "@default(1e3) is not an integer"},
		{typ: "int", text: "1e3", fault: "@default(\"1e3\") is not an integer"},
		{typ: "boolean", text: "yes", fault: "is not true or false"},
		{typ: "array", elem: "object", text: "none", fault: "is not a JSON array"},
		{typ: "string", enum: []string{"short", "long"}, text: "medium", fault: "is not one of the field's values"},
	}
	for _, c := range cases {
		f := toolField{name: "f", typeName: c.typ, elementType: c.elem, enumValues: c.enum,
			defaultVal: c.text, defaultSet: true, defaultNumber: c.number}
		if c.elem != "" {
			f.typeName = "array"
		}
		label := c.typ + " @default(" + c.text + ")"
		got, err := promptDefaultValue("probeDigest", f)
		if c.fault != "" {
			var fde *FieldDefaultError
			if !errors.As(err, &fde) || fde.RuleCode() != PromptDefaultCode {
				t.Errorf("%s: got %v, want a %s refusal", label, err, PromptDefaultCode)
				continue
			}
			for _, want := range []string{`prompt "probeDigest" field "f"`, c.fault, "[" + PromptDefaultCode + "]"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s: the refusal does not say %q:\n  %s", label, want, err)
				}
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: refused, want it to load: %v", label, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: value %#v, want %#v", label, got, c.want)
		}
	}
}

// The whole path: a prompt whose default is not a literal of its field's type
// is refused at load, with the code; one whose default is loads, and its input
// schema publishes the default with the field's type.
func TestPromptDefaultRefusedAtLoadAndPublishedTyped(t *testing.T) {
	prompt := func(name, field string) string {
		return "/// A one-line digest.\n@level(\"fast\")\n@templateFile(\"digest.tmpl\")\nprompt " + name + " {\n  title  string!  @description(\"The title.\")\n  " + field + "\n}\n"
	}
	diags := lint(t, sigTree(map[string]string{
		"promptdefault5430/prompts.memql": prompt("pdWordLength5430", `length  int  @default("three")  @description("Sentences.")`) + "\n" +
			prompt("pdExponentLength5430", `length  int  @default(1e3)  @description("Sentences.")`) + "\n" +
			prompt("pdExactCount5430", `count  int  @default(9007199254740993)  @description("A count.")`) + "\n" +
			prompt("pdThreeSentences5430", `length  int  @default(3)  @description("Sentences.")`),
		"promptdefault5430/digest.tmpl": "{{ .title }}",
	}))
	for construct, want := range map[string]string{
		`"pdWordLength5430"`:     `prompt "pdWordLength5430" field "length" (int): @default("three") is not an integer`,
		`"pdExponentLength5430"`: `prompt "pdExponentLength5430" field "length" (int): @default(1e3) is not an integer`,
	} {
		d := sigRefusal(t, diags, construct)
		if d.Code != PromptDefaultCode || !strings.Contains(d.Message, want) {
			t.Errorf("%s: want [%s] %q, got [%s] %s", construct, PromptDefaultCode, want, d.Code, d.Message)
		}
	}
	for _, d := range diags {
		if strings.Contains(d.Message, "pdThreeSentences5430") || strings.Contains(d.Message, "pdExactCount5430") {
			t.Errorf("a whole-number default on an int field was refused:\n  %s", d.Message)
		}
	}

	// The schema a loaded prompt publishes carries the typed default.
	decl := &promptDecl{name: "p", fields: []toolField{{name: "length", typeName: "int", defaultVal: "3", defaultSet: true, defaultValue: int64(3)}}}
	schema, err := decl.toInputSchema()
	if err != nil {
		t.Fatal(err)
	}
	prop := schema["properties"].(map[string]any)["length"].(map[string]any)
	if prop["default"] != int64(3) || prop["type"] != "integer" {
		t.Errorf("published %#v, want an integer default 3", prop)
	}
}

// An unquoted integer is read from the digits written, not from its parsed
// float64: the published schema keeps every one.
func TestPromptDefaultKeepsAnUnquotedIntegerExact(t *testing.T) {
	for _, typ := range []string{"int", "float"} {
		ast, err := languageParser.ParsePromptDecl("@level(\"fast\")\n@templateFile(\"x.tmpl\")\nprompt p {\n  n  " + typ + "  @default(9007199254740993)\n}\n")
		if err != nil {
			t.Fatal(err)
		}
		decl, err := promptDeclToPromptDecl(ast, "unified:x/prompts.memql:p")
		if err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		schema, err := decl.toInputSchema()
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(schema)
		if !strings.Contains(string(b), `"default":9007199254740993`) {
			t.Errorf("%s field: published %s, want the default's every digit", typ, b)
		}
	}
}
