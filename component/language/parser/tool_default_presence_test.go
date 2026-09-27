package parser

import "testing"

// tool_default_presence_test.go -- memql#5430. `@default("")` is written and
// empty, which the text alone cannot say; the loader holds a written default
// to a literal of the field's type, and an empty text is one only for a string
// field.
func TestToolFieldRecordsThatADefaultWasWritten(t *testing.T) {
	decl, err := ParseToolDecl(`@handler(type="query", query="paginate(query openTickets(), args.limit)")
tool listTickets {
  limit   integer  @default("")
  title   string   @default("untitled")
  status  string
}
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := map[string][2]any{}
	for _, f := range decl.Fields {
		got[f.Name] = [2]any{f.HasDefault, f.Default}
	}
	for name, want := range map[string][2]any{
		"limit":  {true, ""},
		"title":  {true, "untitled"},
		"status": {false, ""},
	} {
		if got[name] != want {
			t.Errorf("field %q: (HasDefault, Default) = %v, want %v", name, got[name], want)
		}
	}
}
