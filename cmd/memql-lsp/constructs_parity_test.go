package main

// constructs_parity_test.go pins the LANGUAGE SERVER's argument wire shape to
// the construct catalog's (memql#3750).
//
// The extension generates one argument form from two sources: this server's
// `memql/runnableConstructs` when the .memql file is open, and
// `ListConstructs` when browsing a cluster where it is not. `runnable.go`'s
// header states the rule the whole run path rests on -- the language server
// owns all .memql parsing, because two parsers let the generated form silently
// disagree with the compiler about what a construct accepts. A second wire
// shape for the same information reintroduces exactly that risk at the
// transport layer instead of the parser, and the failure looks identical: the
// developer finds out by running the wrong thing.
//
// So the two are pinned field for field, in the direction that catches drift
// on either side.

import (
	"reflect"
	"strings"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/memql/sense"
)

// argFieldWireNames maps each LSP JSON field name to the catalog's. The one
// rename is deliberate and documented on the proto field: `enum` is a reserved
// word in the proto language.
var argFieldWireNames = map[string]string{
	"name":         "name",
	"type":         "type",
	"required":     "required",
	"enum":         "enumValues",
	"description":  "description",
	"autoInjected": "autoInjected",
}

// TestRunnableArgMatchesConstructArg: every field the language server reports
// for an argument has a catalog counterpart with the same Go type, and neither
// side carries a field the other does not.
func TestRunnableArgMatchesConstructArg(t *testing.T) {
	lsp := reflect.TypeOf(runnableArg{})
	wire := reflect.TypeOf(memqlv1.ConstructArg{})

	lspByJSON := map[string]reflect.StructField{}
	for i := 0; i < lsp.NumField(); i++ {
		f := lsp.Field(i)
		lspByJSON[jsonName(f)] = f
	}
	wireByJSON := map[string]reflect.StructField{}
	for i := 0; i < wire.NumField(); i++ {
		f := wire.Field(i)
		if name := protoJSONName(f); name != "" {
			wireByJSON[name] = f
		}
	}

	for lspName, wireName := range argFieldWireNames {
		lf, ok := lspByJSON[lspName]
		if !ok {
			t.Errorf("the language server no longer reports %q; either it was renamed (update argFieldWireNames and ConstructArg) or the two argument forms have diverged", lspName)
			continue
		}
		wf, ok := wireByJSON[wireName]
		if !ok {
			t.Errorf("ConstructArg no longer carries %q, the catalog spelling of the language server's %q", wireName, lspName)
			continue
		}
		if lf.Type != wf.Type {
			t.Errorf("field %q: language server has %s, catalog has %s", lspName, lf.Type, wf.Type)
		}
	}

	for name := range lspByJSON {
		if _, mapped := argFieldWireNames[name]; !mapped {
			t.Errorf("the language server gained argument field %q with no catalog counterpart", name)
		}
	}
	mapped := map[string]bool{}
	for _, wireName := range argFieldWireNames {
		mapped[wireName] = true
	}
	for name := range wireByJSON {
		if !mapped[name] {
			t.Errorf("ConstructArg gained field %q that the language server does not report", name)
		}
	}
}

// triggerFieldWireNames maps each LSP JSON field name for a trigger to the
// catalog's. No renames: unlike `enum`, none of the three collides with a proto
// reserved word.
var triggerFieldWireNames = map[string]string{
	"concept":  "concept",
	"event":    "event",
	"schedule": "schedule",
}

// TestRunnableTriggerMatchesConstructTrigger: the same gate as the argument one
// above, for the field that decides an AUTOMATION's form (memql#3805).
//
// It matters for the same reason and one more. The same reason: the extension
// builds ONE automation run form, from `memql/runnableConstructs` when the
// .memql file is open and from `ListConstructs` when browsing a cluster where
// it is not, so two shapes for one form is how the two come to disagree. The
// one more: an argument mismatch shows up as a missing input box, while a
// trigger mismatch shows up as a form that FIRES A REAL EVENT on a real cluster
// with a payload nobody chose -- the failure the Constructs view withheld the
// affordance over until this field existed.
func TestRunnableTriggerMatchesConstructTrigger(t *testing.T) {
	lsp := reflect.TypeOf(runnableTrigger{})
	wire := reflect.TypeOf(memqlv1.ConstructTrigger{})

	lspByJSON := map[string]reflect.StructField{}
	for i := 0; i < lsp.NumField(); i++ {
		f := lsp.Field(i)
		lspByJSON[jsonName(f)] = f
	}
	wireByJSON := map[string]reflect.StructField{}
	for i := 0; i < wire.NumField(); i++ {
		f := wire.Field(i)
		if name := protoJSONName(f); name != "" {
			wireByJSON[name] = f
		}
	}

	for lspName, wireName := range triggerFieldWireNames {
		lf, ok := lspByJSON[lspName]
		if !ok {
			t.Errorf("the language server no longer reports trigger field %q; either it was renamed (update triggerFieldWireNames and ConstructTrigger) or the two automation forms have diverged", lspName)
			continue
		}
		wf, ok := wireByJSON[wireName]
		if !ok {
			t.Errorf("ConstructTrigger no longer carries %q, the catalog spelling of the language server's %q", wireName, lspName)
			continue
		}
		if lf.Type != wf.Type {
			t.Errorf("trigger field %q: language server has %s, catalog has %s", lspName, lf.Type, wf.Type)
		}
	}

	// Both directions, as the argument gate does: a field gained on either side
	// with no counterpart is drift, and the one that is caught late is the one
	// the form silently stops rendering.
	for name := range lspByJSON {
		if _, mapped := triggerFieldWireNames[name]; !mapped {
			t.Errorf("the language server gained trigger field %q with no catalog counterpart", name)
		}
	}
	mapped := map[string]bool{}
	for _, wireName := range triggerFieldWireNames {
		mapped[wireName] = true
	}
	for name := range wireByJSON {
		if !mapped[name] {
			t.Errorf("ConstructTrigger gained field %q that the language server does not report", name)
		}
	}
}

// TestArgTypeSetIsClosedAndShared: the `type` value set is six values, and it
// is the language server's own normalization that produces them. A seventh
// would reach a form generator with no widget for it.
func TestArgTypeSetIsClosedAndShared(t *testing.T) {
	want := map[string]bool{
		"string": true, "number": true, "boolean": true,
		"object": true, "array": true, "any": true,
	}
	got := map[string]bool{}
	for _, rc := range sense.AnalyzeRunnable(argTypeFixture) {
		for _, a := range rc.Args {
			got[a.Type] = true
		}
	}
	for typ := range got {
		if !want[typ] {
			t.Errorf("argument type %q is outside the closed set the argument form knows how to render", typ)
		}
	}
	// And the fixture must reach ALL SIX, or the assertion above passes
	// vacuously for whichever it stopped covering -- `any` in particular,
	// which is the degrade path for a DSL type the form has no widget for.
	for typ := range want {
		if !got[typ] {
			t.Errorf("the fixture no longer produces %q; either the normalization changed or the fixture stopped parsing", typ)
		}
	}
}

// argTypeFixture declares one arg per DSL type alias the normalization maps,
// so the assertion above is over real parser output rather than a restatement
// of the mapping table.
const argTypeFixture = `logic parityFixture {
  args {
    a string
    b number
    c bool
    d object
    e array
    f datetime
    g integer
    h blob
  }
  body {
    return { a: args.a, b: args.b, c: args.c, d: args.d, e: args.e, f: args.f, g: args.g, h: args.h }
  }
}`

// jsonName returns a struct field's JSON name, ignoring options.
func jsonName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "" {
		return f.Name
	}
	return strings.Split(tag, ",")[0]
}

// protoJSONName returns a generated field's protojson name, or "" for the
// generated bookkeeping fields, which are not part of the message's shape.
func protoJSONName(f reflect.StructField) string {
	tag := f.Tag.Get("protobuf")
	if tag == "" {
		return ""
	}
	for _, part := range strings.Split(tag, ",") {
		if strings.HasPrefix(part, "json=") {
			return strings.TrimPrefix(part, "json=")
		}
	}
	// No `json=` part means the protojson name equals the proto field name,
	// which is the last `name=` component.
	for _, part := range strings.Split(tag, ",") {
		if strings.HasPrefix(part, "name=") {
			return strings.TrimPrefix(part, "name=")
		}
	}
	return ""
}
