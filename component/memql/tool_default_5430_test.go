package memql

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/ast"
)

// tool_default_5430_test.go -- memql#5430: a tool field's @default is a
// literal of the field's declared type, or the load refuses it.
//
// THE DEFECT. The registry holds @default on a tool field to one quoted
// string, and nothing held what is INSIDE the quotes to anything: `limit
// integer @default("twenty")` loaded, the schema told the model the integer
// field defaults to "twenty", and a call that left the field out handed the
// handler the string -- coerceSchemaDefault falls back to the raw text when it
// cannot coerce. memql#1673 was that bug on a boolean field.

func TestToolDefaultIsALiteralOfTheFieldType(t *testing.T) {
	type tc struct {
		typ   string
		enum  []string
		text  string
		want  any    // the value published, when it loads
		fault string // the reason, when it is refused
	}
	cases := []tc{
		// string: any text, the empty one included.
		{typ: "string", text: "open", want: "open"},
		{typ: "string", text: "", want: ""},
		{typ: "string", text: "20", want: "20"},
		// integer (and its alias int).
		{typ: "integer", text: "20", want: int64(20)},
		{typ: "integer", text: "0", want: int64(0)},
		{typ: "integer", text: "-5", want: int64(-5)},
		{typ: "int", text: "7", want: int64(7)},
		{typ: "integer", text: "twenty", fault: "is not an integer"},
		{typ: "integer", text: "2.5", fault: "is not an integer"},
		{typ: "integer", text: "1e3", fault: "is not an integer"},
		{typ: "integer", text: "007", fault: "is not an integer"},
		{typ: "integer", text: "+5", fault: "is not an integer"},
		{typ: "integer", text: " 5", fault: "is not an integer"},
		{typ: "integer", text: "", fault: "is not an integer"},
		{typ: "integer", text: "99999999999999999999", fault: "is outside the integers a field can hold"},
		// number (and its alias float).
		{typ: "number", text: "2.5", want: 2.5},
		{typ: "number", text: "20", want: float64(20)},
		{typ: "number", text: "-1.5e3", want: -1500.0},
		{typ: "float", text: "0.5", want: 0.5},
		{typ: "number", text: "high", fault: "is not a number"},
		{typ: "number", text: "NaN", fault: "is not a number"},
		{typ: "number", text: "Inf", fault: "is not a number"},
		{typ: "number", text: ".5", fault: "is not a number"},
		{typ: "number", text: "1e999", fault: "is outside the numbers a field can hold"},
		// boolean (and its alias bool).
		{typ: "boolean", text: "true", want: true},
		{typ: "bool", text: "false", want: false},
		{typ: "boolean", text: "yes", fault: "is not true or false"},
		{typ: "boolean", text: "TRUE", fault: "is not true or false"},
		{typ: "boolean", text: "1", fault: "is not true or false"},
		// array and object: JSON, since the value is published in a JSON Schema.
		{typ: "array", text: "[]", want: []any{}},
		{typ: "array", text: `["a", 1]`, want: []any{"a", float64(1)}},
		{typ: "array", text: "a,b", fault: "is not a JSON array"},
		{typ: "array", text: "{}", fault: "is not a JSON array"},
		{typ: "object", text: "{}", want: map[string]any{}},
		{typ: "object", text: `{"a": 1}`, want: map[string]any{"a": float64(1)}},
		{typ: "object", text: "[]", fault: "is not a JSON object"},
		{typ: "object", text: "{a: 1}", fault: "is not a JSON object"},
		// A field that declares its values takes one of them.
		{typ: "string", enum: []string{"open", "closed"}, text: "open", want: "open"},
		{typ: "string", enum: []string{"open", "closed"}, text: "pending", fault: "is not one of the field's values"},
		{typ: "string", enum: []string{"open", "closed"}, text: "", fault: "is not one of the field's values"},
		{typ: "integer", enum: []string{"1", "2"}, text: "2", want: int64(2)},
		{typ: "integer", enum: []string{"1", "2"}, text: "3", fault: "is not one of the field's values"},
	}
	for _, c := range cases {
		f := ast.ToolFieldDecl{Name: "f", Type: c.typ, EnumValues: c.enum, Default: c.text, HasDefault: true}
		decl := &ast.ToolDecl{Name: "probeDefaults", HandlerType: "function", HandlerName: "x", Fields: []ast.ToolFieldDecl{f}}
		tools, err := toolDeclToTool(decl, "unified:probe/tools.memql:probeDefaults")
		label := c.typ + " @default(" + `"` + c.text + `"` + ")"
		if c.fault != "" {
			var tde *ToolDefaultError
			if !errors.As(err, &tde) {
				t.Errorf("%s: loaded (err %v), want it refused: %s", label, err, c.fault)
				continue
			}
			msg := err.Error()
			for _, want := range []string{`tool "probeDefaults" field "f"`, c.fault, "[" + ToolDefaultCode + "]"} {
				if !strings.Contains(msg, want) {
					t.Errorf("%s: the refusal does not say %q:\n  %s", label, want, msg)
				}
			}
			if tde.RuleCode() != ToolDefaultCode {
				t.Errorf("%s: code = %q", label, tde.RuleCode())
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: refused, want it to load: %v", label, err)
			continue
		}
		var schema struct {
			Properties map[string]map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(tools[0].InputSchema, &schema); err != nil {
			t.Fatalf("%s: schema: %v", label, err)
		}
		got := schema.Properties["f"]["default"]
		want := c.want
		if n, ok := want.(int64); ok {
			want = float64(n) // what JSON decodes a published integer to
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: published default %#v, want %#v -- a default is published with its field's type", label, got, want)
		}
	}
}

// The handler receives the typed value, whether the schema publishes it typed
// (an authored tool, since memql#5430) or as a string (a schema built by hand):
// an integer arrives as the int64 it always did.
func TestToolSchemaDefaultsReadsTypedAndStringDefaults(t *testing.T) {
	for _, schema := range []string{
		`{"properties":{"limit":{"type":"integer","default":10},"flag":{"type":"boolean","default":false},"ratio":{"type":"number","default":0.5},"tags":{"type":"array","default":["a"]},"name":{"type":"string","default":""},"none":{"type":"string"}}}`,
		`{"properties":{"limit":{"type":"integer","default":"10"},"flag":{"type":"boolean","default":"false"},"ratio":{"type":"number","default":"0.5"},"tags":{"type":"array","default":["a"]},"name":{"type":"string","default":""},"none":{"type":"string","default":null}}}`,
	} {
		got := toolSchemaDefaults(&Tool{InputSchema: json.RawMessage(schema)})
		want := map[string]any{"limit": int64(10), "flag": false, "ratio": 0.5, "tags": []any{"a"}, "name": ""}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("schema %s:\n got %#v\nwant %#v", schema, got, want)
		}
	}
	// An integer beyond a float64's exact range is read from its token.
	big := toolSchemaDefaults(&Tool{InputSchema: json.RawMessage(`{"properties":{"n":{"type":"integer","default":9007199254740993}}}`)})
	if big["n"] != int64(9007199254740993) {
		t.Errorf("a large integer default was rounded: %#v", big["n"])
	}
}

// The whole path: a tool whose default is not a literal of its field's type is
// refused at load, by name, with the code; one whose default is, loads.
func TestToolDefaultRefusedAtLoad(t *testing.T) {
	diags := lint(t, sigTree(map[string]string{
		"tooldefault5430/concepts.memql": "/// A ticket.\nconcept tdTicket5430 {\n  title  string\n}\n",
		"tooldefault5430/queries.memql": `/// Every ticket.
query tdTicket5430 tdTickets5430 {
  sort "row.createdAt", "desc"
}
`,
		"tooldefault5430/tools.memql": `/// List the tickets, the default in words.
@handler(type="query", query="paginate(query tdTickets5430(), args.limit)")
tool tdListInWords5430 {
  limit  integer  @default("twenty")  @description("How many.")
}

/// List the tickets, twenty unless asked.
@handler(type="query", query="paginate(query tdTickets5430(), args.limit)")
tool tdListTwenty5430 {
  limit  integer  @default("20")  @description("How many.")
}
`,
	}))
	d := sigRefusal(t, diags, `"tdListInWords5430"`)
	if d.Code != ToolDefaultCode || !strings.Contains(d.Message, `tool "tdListInWords5430" field "limit" (integer): @default("twenty") is not an integer -- write one, as in @default("20")`) {
		t.Errorf("want the tool, the field, its type, the value and the code; got [%s] %s", d.Code, d.Message)
	}
	for _, other := range diags {
		if strings.Contains(other.Message, "tdListTwenty5430") {
			t.Errorf("a default that is an integer was refused:\n  %s", other.Message)
		}
	}
}
