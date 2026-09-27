package memql

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// prompt_default_5430_test.go -- memql#5430, the prompt half: a prompt field's
// @default is held to a literal of the field's type, as a tool field's is.
// Its placement also admits an unquoted number, which is a literal of a
// numeric field only.

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
		{typ: "number", text: "2", want: float64(2)},
		{typ: "boolean", text: "true", want: true},
		{typ: "array", elem: "object", text: "[]", want: []any{}},
		{typ: "object", text: `{"a": 1}`, want: map[string]any{"a": float64(1)}},
		// A type the schema publishes as a string takes any text.
		{typ: "datetime", text: "2026-01-02T03:04:05Z", want: "2026-01-02T03:04:05Z"},
		{typ: "string", enum: []string{"short", "long"}, text: "long", want: "long"},

		{typ: "int", text: "three", fault: "is not an integer"},
		{typ: "int", text: "2.5", number: true, fault: "is not an integer"},
		{typ: "string", text: "5", number: true, fault: "is a number, and the field is a string -- write it quoted, as in @default(\"5\")"},
		{typ: "boolean", text: "1", number: true, fault: "is a number, and the field is a boolean"},
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
			prompt("pdNumberOnString5430", `language  string  @default(5)  @description("Language.")`) + "\n" +
			prompt("pdThreeSentences5430", `length  int  @default(3)  @description("Sentences.")`),
		"promptdefault5430/digest.tmpl": "{{ .title }}",
	}))
	for construct, want := range map[string]string{
		`"pdWordLength5430"`:     `prompt "pdWordLength5430" field "length" (int): @default("three") is not an integer`,
		`"pdNumberOnString5430"`: `prompt "pdNumberOnString5430" field "language" (string): @default(5) is a number, and the field is a string`,
	} {
		d := sigRefusal(t, diags, construct)
		if d.Code != PromptDefaultCode || !strings.Contains(d.Message, want) {
			t.Errorf("%s: want [%s] %q, got [%s] %s", construct, PromptDefaultCode, want, d.Code, d.Message)
		}
	}
	for _, d := range diags {
		if strings.Contains(d.Message, "pdThreeSentences5430") {
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
