package memql

// memql#2909: a concept property typed with a spelling the schema builder does
// not know (`boolean` for `bool`) makes BuildConceptFromDecl fail, and the
// unified loader responds by WARNING and dropping the whole concept. Not the
// property -- the concept, and with it every query, mutation and shape bound
// to it.
//
// The warning goes to a logger. LintUnifiedTree collects eng.loadReport.Skipped,
// which covers construct-phase skips, so a concept that never reached the
// registry leaves no diagnostic behind. memqllint is the only pre-boot gate a
// product bundle has, so the bundle lints clean and loses a concept at boot.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/santhosh-tekuri/jsonschema/v5"

	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// TestLintParity_ConceptWithUnknownPropertyType is the witness. `boolean` is
// accepted for the same author-facing value everywhere else in the tree --
// args binding (component/automations/args_binding.go), value type-checking
// (component/memql/parser.go) -- so it is a predictable thing to type, and the
// concept schema builder is the outlier that rejects it.
func TestLintParity_ConceptWithUnknownPropertyType(t *testing.T) {
	root := fstest.MapFS{
		"lintbadtype/concepts.memql": {Data: []byte(`@version("1.0.0")
@namespace("lintbadtype")
@description("A widget with a mistyped property type.")
concept widget {
  label    string   @required  @description("Widget label.")
  enabled  boolean  @description("Whether the widget is enabled.")
}
`)},
	}
	diags := lint(t, root)
	if len(diags) == 0 {
		t.Fatal("a concept whose schema fails to build must produce a diagnostic: the unified " +
			"loader drops the WHOLE concept, and memqllint is the only pre-boot gate a product " +
			"bundle has (memql#2909)")
	}
	if !diagsContain(diags, "widget") {
		t.Errorf("the diagnostic must name the dropped concept, or an author cannot find it.\n  got: %+v", diags)
	}
	if !diagsContain(diags, "enabled") {
		t.Errorf("the diagnostic must name the offending property.\n  got: %+v", diags)
	}
}

// TestLintParity_ConceptWithUnknownElementType is the wrapped-position twin of
// the test above, and it is the one that pins memql#2951's headline repro:
//
//	$ go run ./cmd/memqllint <bundle declaring `flags []boolean`>
//	OK: 1 file(s) loaded, no diagnostics.
//
// That was the issue's opening complaint, and every OTHER element assertion in
// this file calls BuildConceptFromDecl directly -- which proves the builder
// rejects it, not that the LINT GATE ever sees it. memql#2909's whole lesson
// was that those are different claims: the loader can drop a concept while the
// pre-boot gate a product bundle actually runs stays silent. Added in the
// memql#2951 review, where the gap was that the file named `lint_parity_*`
// had lint parity for scalar bad types and none for wrapped ones.
func TestLintParity_ConceptWithUnknownElementType(t *testing.T) {
	root := fstest.MapFS{
		"lintbadelem/concepts.memql": {Data: []byte(`@version("1.0.0")
@namespace("lintbadelem")
@description("A widget whose ELEMENT type is mistyped.")
concept widget {
  label  string     @required  @description("Widget label.")
  flags  []boolean  @description("Element type is not a real type.")
}
`)},
	}
	diags := lint(t, root)
	if len(diags) == 0 {
		t.Fatal("a concept whose ELEMENT type fails to build must produce a diagnostic. " +
			"`[]boolean` lowering silently to `[]string` with no diagnostic is memql#2951's " +
			"opening repro, and memqllint is the only pre-boot gate a product bundle has")
	}
	if !diagsContain(diags, "widget") {
		t.Errorf("the diagnostic must name the dropped concept.\n  got: %+v", diags)
	}
	if !diagsContain(diags, "flags") {
		t.Errorf("the diagnostic must name the offending property.\n  got: %+v", diags)
	}
	// The correction is the difference between a dead end and a fix: an author
	// who wrote `boolean` needs to be told the spelling is `bool`.
	if !diagsContain(diags, "bool") {
		t.Errorf("the diagnostic must carry the `did you mean` correction for the ELEMENT "+
			"type, not just report a failure.\n  got: %+v", diags)
	}
}

// TestLintParity_UnknownPropertyTypeNamesItsFile pins the attribution. A
// bundle can hold hundreds of concepts; a diagnostic that cannot say which
// file to open costs more than it saves.
func TestLintParity_UnknownPropertyTypeNamesItsFile(t *testing.T) {
	root := fstest.MapFS{
		"lintbadfile/concepts.memql": {Data: []byte(`@version("1.0.0")
@namespace("lintbadfile")
@description("A gadget with a mistyped property type.")
concept gadget {
  label  string     @required  @description("Gadget label.")
  size   quantities @description("Bogus type.")
}
`)},
	}
	diags, _, err := LintUnifiedTree(nil, root)
	if err != nil {
		t.Fatalf("LintUnifiedTree: %v", err)
	}
	var found bool
	for _, d := range diags {
		if strings.Contains(d.Message, "gadget") && strings.Contains(d.File, "lintbadfile/concepts.memql") {
			found = true
		}
	}
	if !found {
		t.Errorf("the diagnostic must carry the origin file of the dropped concept (memql#2909).\n  got: %+v", diags)
	}
}

// TestConceptPropertyTypes_AcceptedAndRejectedSets is memql#2909 item 3 -- the
// audit, executable. It drives BuildConceptFromDecl (the function the loader
// itself calls) once per spelling, so the two sets are measured rather than
// asserted.
//
// The rejected column is the point: every one of these silently dropped a
// whole concept before this change, and each is a spelling an author has a
// reason to reach for -- the JSON Schema names because the builder EMITS them,
// the rest because every neighbouring language accepts them.
func TestConceptPropertyTypes_AcceptedAndRejectedSets(t *testing.T) {
	build := func(t *testing.T, ty string) error {
		t.Helper()
		src := "@version(\"1.0.0\")\n@namespace(\"aud\")\n@description(\"d\")\n" +
			"concept probe {\n  label string @required @description(\"l\")\n  f " + ty + " @description(\"x\")\n}\n"
		decls := ExtractConceptDecls(src)
		if len(decls) == 0 {
			t.Fatalf("fixture for %q did not parse into a concept decl, so it measures nothing", ty)
		}
		_, err := concept.BuildConceptFromDecl(decls[0], "v1:aud:probe")
		return err
	}

	t.Run("accepted", func(t *testing.T) {
		// Includes the PARAMETERISED forms the docs now claim are writable.
		// Review found the first version of those docs wrong precisely
		// because the vocabulary was read off buildPropertySchema's case
		// labels instead of measured: `map` and `enum` are cases there but
		// are reachable only as `map[string]<type>` and `enum("a", "b")`.
		for _, ty := range []string{
			"string", "bool", "int", "float", "datetime", "array", "object", "any",
			"[]string", "[]object", `enum("a", "b")`, "map[string]string", "map[string]int",
		} {
			if err := build(t, ty); err != nil {
				t.Errorf("%q must be an accepted property type; the builder rejected it: %v", ty, err)
			}
		}
	})

	t.Run("rejected with a correction", func(t *testing.T) {
		// wrong -> the spelling the author wanted.
		// All SIXTEEN. Review round 14 found this map had twelve while the
		// table it independently checks had sixteen, so long/decimal/str/time
		// were corrected in the error an author sees and driven by no test.
		for wrong, want := range map[string]string{
			"boolean": "bool",
			"integer": "int", "int64": "int", "long": "int",
			"number": "float", "double": "float", "decimal": "float",
			"text": "string", "str": "string", "uuid": "string",
			"date": "datetime", "time": "datetime", "timestamp": "datetime",
			"list": "array",
			"dict": "object", "json": "object",
		} {
			err := build(t, wrong)
			if err == nil {
				t.Errorf("%q is accepted by the builder but the vocabulary table lists it as a "+
					"misspelling of %q -- one of the two is wrong", wrong, want)
				continue
			}
			if !strings.Contains(err.Error(), "did you mean") || !strings.Contains(err.Error(), want) {
				t.Errorf("rejecting %q must point at %q, or the author is left guessing "+
					"why a whole concept vanished.\n  got: %v", wrong, want, err)
			}
		}
	})
}

// TestConceptPropertyTypes_NestedBlockPropertiesAreValidated pins the boundary
// the docs state. Review round 2 caught that boundary written as "top-level
// property position", which is wrong: propertyToJSONSchema recurses through a
// block property's `case "object"` and validates nested properties to the same
// standard, correction message included.
//
// The real line is property position (at ANY depth) versus element-type
// position inside []<type> / map[string]<type>. Getting it wrong told authors a
// nested `boolean` was in the harmless zone when it drops the whole concept --
// the exact failure this issue exists to prevent.
func TestConceptPropertyTypes_NestedBlockPropertiesAreValidated(t *testing.T) {
	src := "@version(\"1.0.0\")\n@namespace(\"aud\")\n@description(\"d\")\n" +
		"concept probe {\n  label string @required @description(\"l\")\n" +
		"  cfg {\n    inner boolean @description(\"x\")\n  }\n}\n"
	decls := ExtractConceptDecls(src)
	if len(decls) == 0 {
		t.Fatal("fixture did not parse into a concept decl, so it measures nothing")
	}
	_, err := concept.BuildConceptFromDecl(decls[0], "v1:aud:probe")
	if err == nil {
		t.Fatal("a nested block property typed `boolean` must be rejected. The docs say " +
			"validation reaches any nesting depth in property position; if that stops being " +
			"true, an author following them loses a whole concept (memql#2909)")
	}
	if !strings.Contains(err.Error(), "did you mean") || !strings.Contains(err.Error(), `"bool"`) {
		t.Errorf("a nested property must get the same correction a top-level one does.\n  got: %v", err)
	}
	if !strings.Contains(err.Error(), "inner") {
		t.Errorf("the diagnostic must name the nested property.\n  got: %v", err)
	}
}

// TestConceptPropertyTypes_ElementTypesAreValidated is the memql#2951 fix, and
// it is the inverse of the test that stood here: this file used to pin the gap
// as CURRENT behaviour so the docs and the code could not drift apart, with a
// note saying whoever closed it must delete the caveat. Both happened together.
//
// The gap was a second lowering. memqlTypeToJSONType ended in
// `default: return "string"` and never consulted suggestPropertyType, so inside
// `[]<type>` and `map[string]<type>` an unrecognised element type was neither
// rejected nor corrected -- `[]boolean` built a field validating as `[]string`.
// The element now goes through propertyToJSONSchema itself, so there is no
// second answer to diverge.
//
// Both halves are asserted, because each fails differently:
//   - REJECTED with the same `did you mean` correction property position gets.
//   - ACCEPTED, emitting the exact element schema the declaration describes --
//     asserted whole rather than by type, because the case this most needs to
//     catch is `[]datetime` silently losing `format` while still lowering to
//     `"type":"string"`.
func TestConceptPropertyTypes_ElementTypesAreValidated(t *testing.T) {
	buildField := func(t *testing.T, ty string) (map[string]any, error) {
		t.Helper()
		src := "@version(\"1.0.0\")\n@namespace(\"aud\")\n@description(\"d\")\n" +
			"concept probe {\n  label string @required @description(\"l\")\n  f " + ty + " @description(\"x\")\n}\n"
		decls := ExtractConceptDecls(src)
		if len(decls) == 0 {
			t.Fatalf("fixture for %q did not parse, so it measures nothing", ty)
		}
		c, err := concept.BuildConceptFromDecl(decls[0], "v1:aud:probe")
		if err != nil {
			return nil, err
		}
		raw, serr := c.DefinitionSchema()
		if serr != nil {
			return nil, serr
		}
		var doc struct {
			Properties map[string]map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("unmarshal %q: %v", ty, err)
		}
		return doc.Properties["f"], nil
	}

	// An unrecognised element type is refused, and the message carries the same
	// correction. Asserting the CORRECTION and not merely the refusal is the
	// point: rejecting `[]boolean` with "unknown type" alone would leave the
	// author exactly where memql#2909 found them in property position.
	t.Run("unrecognised element types are rejected with a correction", func(t *testing.T) {
		for _, c := range []struct{ ty, want string }{
			{"[]boolean", `did you mean "bool"?`},
			{"map[string]boolean", `did you mean "bool"?`},
			{"[]integer", `did you mean "int"?`},
			{"map[string]number", `did you mean "float"?`},
			// No suggestion exists, so the accepted set is listed instead.
			{"[]frobnicate", "accepted: string, bool, int, float, datetime"},
			// Two levels in: the inner type is REACHED, not discarded. Before
			// the fix `[][]frobnicate` built happily as `items:{type:array}`.
			{"[][]frobnicate", "accepted: string, bool, int, float, datetime"},
		} {
			t.Run(c.ty, func(t *testing.T) {
				_, err := buildField(t, c.ty)
				if err == nil {
					t.Fatalf("`%s` still builds. An unrecognised element type must be refused the "+
						"same way property position refuses it (memql#2951).", c.ty)
				}
				if !strings.Contains(err.Error(), c.want) {
					t.Errorf("`%s` is refused, but without the correction an author acts on.\n"+
						"  want the message to contain: %s\n  got: %v", c.ty, c.want, err)
				}
			})
		}
	})

	// Recognised element types emit the schema the declaration describes,
	// asserted WHOLE. A type-only check passes for `[]datetime` -- which does
	// lower to `"type":"string"` -- while the RFC3339 check that is the entire
	// reason to write `datetime` is missing.
	t.Run("recognised element types emit their real schema", func(t *testing.T) {
		str := map[string]any{"type": "string"}
		dt := map[string]any{"type": "string", "format": "date-time"}
		obj := map[string]any{"type": "object"}
		strMap := map[string]any{"type": "object", "additionalProperties": str}
		for _, c := range []struct {
			ty      string
			elemKey string
			want    map[string]any
		}{
			{"[]string", "items", str},
			{"[]bool", "items", map[string]any{"type": "boolean"}},
			{"[]int", "items", map[string]any{"type": "integer"}},
			{"[]float", "items", map[string]any{"type": "number"}},
			{"[]object", "items", obj},
			// `any` means "unconstrained" as a property, and now means the same
			// one level in. It used to mean "must be a string", which is the
			// exact opposite of what the author wrote.
			{"[]any", "items", map[string]any{}},
			{"map[string]any", "additionalProperties", map[string]any{}},
			// The defect that motivated asserting whole schemas.
			{"[]datetime", "items", dt},
			{"map[string]datetime", "additionalProperties", dt},
			// enum is the same class as datetime -- a VALUE constraint that the
			// old lowering dropped one level in, leaving `{"type":"string"}`
			// with the permitted set silently gone. Caught by the memql#2951
			// review: the fix ships this behaviour and nothing pinned it.
			{"[]enum(\"a\", \"b\")", "items", map[string]any{
				"type": "string", "enum": []any{"a", "b"}}},
			{"map[string]enum(\"a\", \"b\")", "additionalProperties", map[string]any{
				"type": "string", "enum": []any{"a", "b"}}},
			// Composites keep their inner type instead of collapsing.
			{"map[string]map[string]string", "additionalProperties", strMap},
			{"[]map[string]string", "items", strMap},
			{"[][]string", "items", map[string]any{"type": "array", "items": str}},
			{"map[string][]int", "additionalProperties", map[string]any{
				"type": "array", "items": map[string]any{"type": "integer"}}},
		} {
			t.Run(c.ty, func(t *testing.T) {
				f, err := buildField(t, c.ty)
				if err != nil {
					t.Fatalf("`%s` no longer builds: %v", c.ty, err)
				}
				inner, ok := f[c.elemKey].(map[string]any)
				if !ok {
					t.Fatalf("`%s` emitted no %s schema at all: %v", c.ty, c.elemKey, f)
				}
				if !reflect.DeepEqual(inner, c.want) {
					t.Errorf("`%s` must emit exactly %v.\n  got: %v", c.ty, c.want, inner)
				}
			})
		}
	})
}

// TestConceptPropertyTypes_ElementSafeListCarriesTheSameConstraints pins which
// element types are dependable inside []<type> and map[string]<type>. That
// list lived in docs/public/language/memql.md until memql#2909 scoped its docs
// back to the type gate; it is memql#2951's to state now, and this keeps it
// measured either way.
//
// The invariant is not "it lowers to the right JSON type" -- that is too weak,
// and I first wrote it that way and it passed for `datetime`, which is exactly
// the type it needed to catch. `datetime` DOES lower to `"type":"string"`; what
// it loses is `"format":"date-time"`, so `[]datetime` is byte-identical to
// `[]string` and an author who writes `auditTimes []datetime` for the RFC3339
// check gets an array accepting ["hello"].
//
// So the invariant is: THE ELEMENT SCHEMA CARRIES EVERY CONSTRAINT THE SCALAR
// SCHEMA CARRIES. A type belongs on the dependable list only if wrapping it
// costs nothing.
func TestConceptPropertyTypes_ElementSafeListCarriesTheSameConstraints(t *testing.T) {
	build := func(t *testing.T, decl string) map[string]any {
		t.Helper()
		src := "@version(\"1.0.0\")\n@namespace(\"aud\")\n@description(\"d\")\n" +
			"concept probe {\n  label string @required @description(\"l\")\n  f " + decl + " @description(\"x\")\n}\n"
		decls := ExtractConceptDecls(src)
		if len(decls) == 0 {
			t.Fatalf("fixture for %q did not parse, so it measures nothing", decl)
		}
		c, err := concept.BuildConceptFromDecl(decls[0], "v1:aud:probe")
		if err != nil {
			t.Fatalf("%q does not build: %v", decl, err)
		}
		raw, serr := c.DefinitionSchema()
		if serr != nil {
			t.Fatalf("schema for %q: %v", decl, serr)
		}
		var doc struct {
			Properties map[string]map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("unmarshal schema for %q: %v", decl, err)
		}
		return doc.Properties["f"]
	}

	// constraints strips the keys that annotate rather than constrain, so
	// what remains is comparable between a scalar and an element.
	constraints := func(m map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range m {
			switch k {
			case "description":
			default:
				out[k] = v
			}
		}
		return out
	}
	// Compared by VALUE, not by key set. Review round 4 found the key-set
	// version too weak: every safe-list entry collapses to the single key
	// "type", so mutating bool's lowering to emit `items:{"type":"string"}`
	// left this test -- and all 127 packages -- green. The keys matched; the
	// guarantee did not.
	same := func(a, b map[string]any) bool { return reflect.DeepEqual(a, b) }
	keysOf := func(m map[string]any) []string {
		var out []string
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}

	// `any` joins the list with memql#2951 -- it used to mean "unconstrained"
	// as a property and "must be a string" as an element. `datetime` is the one
	// type this equality cannot express, and it gets its own subtest below.
	for _, base := range []string{"string", "bool", "int", "float", "object", "any"} {
		t.Run(base, func(t *testing.T) {
			scalar := constraints(build(t, base))

			arr := build(t, "[]"+base)
			items, _ := arr["items"].(map[string]any)
			if items == nil {
				t.Fatalf("[]%s emitted no items schema: %+v", base, arr)
			}
			if got := constraints(items); !same(got, scalar) {
				t.Errorf("[]%s is on the docs' dependable list, but its element does not carry what a "+
					"scalar %s carries -- the docs promise a guarantee the schema does not make.\n"+
					"  element: %v (keys %v)\n  scalar:  %v (keys %v)",
					base, base, got, keysOf(got), scalar, keysOf(scalar))
			}

			m := build(t, "map[string]"+base)
			vals, _ := m["additionalProperties"].(map[string]any)
			if vals == nil {
				t.Fatalf("map[string]%s emitted no additionalProperties schema: %+v", base, m)
			}
			if got := constraints(vals); !same(got, scalar) {
				t.Errorf("map[string]%s is on the docs' dependable list, but its value does not carry "+
					"what a scalar %s carries.\n  value:  %v (keys %v)\n  scalar: %v (keys %v)",
					base, base, got, keysOf(got), scalar, keysOf(scalar))
			}
		})
	}

	// datetime is the one type where element and scalar are DELIBERATELY not
	// equal, so it cannot ride the loop above -- and stating that here is the
	// point, because "not equal" is also what the memql#2951 defect looked like.
	//
	// An OPTIONAL scalar datetime accepts the unset sentinels "" and null
	// (memql#1629), because an optional field has to be clearable. An ELEMENT
	// is never unset -- you omit it from the array instead -- so the sentinels
	// have no meaning one level in and the element is the strict RFC3339 form.
	// It is therefore at least as strict as the scalar, which is the invariant
	// that actually matters; what memql#2951 fixed was the element being
	// strictly WEAKER (a bare string, so `["hello"]` was accepted).
	t.Run("datetime elements are strict RFC3339", func(t *testing.T) {
		want := map[string]any{"type": "string", "format": "date-time"}
		for _, c := range []struct{ decl, elemKey string }{
			{"[]datetime", "items"},
			{"map[string]datetime", "additionalProperties"},
		} {
			got, _ := build(t, c.decl)[c.elemKey].(map[string]any)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("`%s` must constrain each entry to RFC3339 -- that check is the entire "+
					"reason to write datetime rather than string.\n  want: %v\n  got:  %v",
					c.decl, want, got)
			}
		}
		// The consequence, driven rather than inspected: a garbage entry is
		// refused. Before memql#2951 this array accepted ["hello"].
		mustReject(t, "f []datetime", []any{"hello"}, []any{"2026-01-02T03:04:05Z"})
	})
}

// mustReject compiles the concept schema for a single-field declaration exactly
// as the engine compiles it, and asserts the bad payload is refused BY THE
// ELEMENT SCHEMA while the good one is accepted.
//
// The positive control is not decoration: without it a declaration that refuses
// everything -- or a fixture that never parsed the field at all -- passes the
// rejection half and proves nothing.
func mustReject(t *testing.T, decl string, bad, good any) {
	t.Helper()
	id := "v1:aud:probeReject"
	src := "@version(\"1.0.0\")\n@namespace(\"aud\")\n@description(\"d\")\n" +
		"concept probeReject {\n  label string @required @description(\"l\")\n  " + decl + "\n}\n"
	decls := ExtractConceptDecls(src)
	if len(decls) == 0 {
		t.Fatalf("fixture %q did not parse, so it measures nothing", decl)
	}
	cc, err := concept.BuildConceptFromDecl(decls[0], id)
	if err != nil {
		t.Fatalf("fixture %q does not build: %v", decl, err)
	}
	raw, serr := cc.DefinitionSchema()
	if serr != nil {
		t.Fatalf("schema for %q: %v", decl, serr)
	}
	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2019
	if err := compiler.AddResource(id, bytes.NewReader(raw)); err != nil {
		t.Fatalf("register schema for %q: %v", decl, err)
	}
	schema, err := compiler.Compile(id)
	if err != nil {
		t.Fatalf("compile schema for %q: %v", decl, err)
	}
	if err := schema.Validate(map[string]any{"label": "l", "f": bad}); err == nil {
		t.Errorf("`%s` ACCEPTS %v, which does not conform to the declaration.\n  schema: %s",
			decl, bad, string(raw))
	} else if loc := err.Error(); !strings.Contains(loc, "/properties/f/items") &&
		!strings.Contains(loc, "/properties/f/additionalProperties") {
		t.Errorf("`%s` refused %v, but not by its element schema, so this proves nothing.\n  error: %v",
			decl, bad, err)
	}
	if err := schema.Validate(map[string]any{"label": "l", "f": good}); err != nil {
		t.Errorf("control for `%s` must be accepted -- %v conforms to the declaration. If this "+
			"fails the fixture is broken and the rejection above passes for the wrong reason.\n"+
			"  error: %v", decl, good, err)
	}
}

// TestConceptPropertyTypes_AnnotationsSplitIntoValueConstraintsAndFieldMarkers
// pins the WHOLE annotation matrix the docs state, in both directions.
//
// Review round 4 got me to write "no constraining annotation", and round 5
// showed that is over-broad in a way that matters: `@required` on `[]string!`
// works exactly as it does on a scalar, and shipped concept fields in dsl/ use
// that form (dsl/worker/concepts.memql:55, dsl/actions/concepts.memql:36,:80,
// :126, dsl/harness/concepts.memql:146). Telling authors to hand-check them is
// its own defect -- the same shape as round 1, where I called `map` unwritable.
//
// The real split is what the annotation is ABOUT:
//
//	VALUE constraints  describe the value, so landing on the array/map makes
//	                   them inert -- JSON Schema does not apply `pattern` or
//	                   `minLength` to an array, and `@variant` is dropped.
//	FIELD markers      describe the field, so the wrapper is the correct
//	                   place and nothing is lost.
//
// Both lists are asserted, because getting either wrong costs an author
// something: a false negative loses validation, a false positive loses a
// working construct.
//
// One caveat the docs no longer make and neither does this test: several
// markers (@unique, @immutable, @secret, @default) lower to keys NOTHING in
// the tree reads today -- `x-unique`, `x-immutable`, `x-secret` each have
// exactly one occurrence, the emit site. So "survives wrapping" here means the
// emitted schema is unchanged, NOT that the annotation is enforced. @pii is
// the exception that is genuinely live, and it has TWO consumers: the
// @scrubPii mutation path (concept.go PIIFields -> scrubPIIFields) and the
// projection authorization gate in dsl/pii_projection_test.go (memql#2883).
// Filed as memql#2960.
func TestConceptPropertyTypes_AnnotationsSplitIntoValueConstraintsAndFieldMarkers(t *testing.T) {
	fieldOf := func(t *testing.T, decl string) map[string]any {
		t.Helper()
		src := "@version(\"1.0.0\")\n@namespace(\"aud\")\n@description(\"d\")\n" +
			"concept probe {\n  label string @required @description(\"l\")\n  " + decl + "\n}\n"
		decls := ExtractConceptDecls(src)
		if len(decls) == 0 {
			t.Fatalf("fixture %q did not parse, so it measures nothing", decl)
		}
		c, err := concept.BuildConceptFromDecl(decls[0], "v1:aud:probe")
		if err != nil {
			t.Fatalf("fixture %q does not build: %v", decl, err)
		}
		raw, serr := c.DefinitionSchema()
		if serr != nil {
			t.Fatalf("schema for %q: %v", decl, serr)
		}
		var doc struct {
			Properties map[string]map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("unmarshal %q: %v", decl, err)
		}
		return doc.Properties["f"]
	}

	// VALUE CONSTRAINTS: the key must land on the ELEMENT and NOT on the
	// wrapper. On the wrapper JSON Schema ignores it -- `pattern` does nothing
	// to an array -- which was the memql#2951 defect. Both directions are
	// asserted: leaving it on the wrapper is the old bug, emitting it in BOTH
	// places would make the array's own validation depend on a keyword meant
	// for its items.
	t.Run("value constraints apply to the element when wrapped", func(t *testing.T) {
		for _, c := range []struct{ base, ann, key string }{
			{"string", `@pattern("^[a-z]+$")`, "pattern"},
			{"string", "@minLength(3)", "minLength"},
			{"string", "@maxLength(9)", "maxLength"},
			{"int", "@minimum(10)", "minimum"},
			{"int", "@maximum(99)", "maximum"},
		} {
			t.Run(c.key, func(t *testing.T) {
				scalar := fieldOf(t, "f "+c.base+" "+c.ann+` @description("x")`)
				if _, ok := scalar[c.key]; !ok {
					t.Fatalf("control broken: a scalar %s must carry %q.\n  got: %v", c.base, c.key, scalar)
				}
				// BOTH wrapped forms. Review round 6 of the original found the
				// map half of this claim untested, and it stays gated here.
				for _, w := range []struct{ form, elemKey string }{
					{"f []" + c.base + " " + c.ann + ` @description("x")`, "items"},
					{"f map[string]" + c.base + " " + c.ann + ` @description("x")`, "additionalProperties"},
				} {
					wrapped := fieldOf(t, w.form)
					elems, _ := wrapped[w.elemKey].(map[string]any)
					if got, onElems := elems[c.key]; !onElems {
						t.Errorf("%s does not constrain the elements of `%s`, so the annotation is "+
							"inert -- JSON Schema does not apply it to the wrapper.\n  element: %v",
							c.key, w.form, elems)
					} else if want := scalar[c.key]; !reflect.DeepEqual(got, want) {
						t.Errorf("%s reaches the elements of `%s` with a different value than the "+
							"scalar carries.\n  want: %v\n  got:  %v", c.key, w.form, want, got)
					}
					if _, onWrapper := wrapped[c.key]; onWrapper {
						t.Errorf("%s is emitted on the WRAPPER of `%s` as well. It belongs only on the "+
							"element; on an array it constrains nothing and reads as though it "+
							"did.\n  got: %v", c.key, w.form, wrapped)
					}
				}
			})
		}

		// Driven end to end, not merely inspected. The schema-shape assertions
		// above would all pass on a `pattern` the validator never reaches.
		mustReject(t, `f []string @pattern("^[a-z]+$")`, []any{"ZZZ"}, []any{"abc"})
		mustReject(t, `f map[string]string @pattern("^[a-z]+$")`,
			map[string]any{"k": "ZZZ"}, map[string]any{"k": "abc"})
		mustReject(t, "f []int @minimum(10)", []any{3}, []any{11})
	})

	// FIELD MARKERS: scalar and wrapped must be INDISTINGUISHABLE apart from
	// the type itself. These are the ones the docs promise still work.
	t.Run("field markers survive wrapping", func(t *testing.T) {
		for _, c := range []struct{ name, ann string }{
			{"description", `@description("x")`},
			{"default", `@description("x") @default("z")`},
			{"unique", `@description("x") @unique`},
			{"immutable", `@description("x") @immutable`},
			{"secret", `@description("x") @secret`},
			{"pii", `@description("x") @pii`},
			{"internal", `@description("x") @internal`},
			{"serverSet", `@description("x") @serverSet`},
		} {
			t.Run(c.name, func(t *testing.T) {
				scalar := fieldOf(t, "f string "+c.ann)
				delete(scalar, "type")
				for _, w := range []struct{ form, elemKey string }{
					{"f []string " + c.ann, "items"},
					{"f map[string]string " + c.ann, "additionalProperties"},
				} {
					wrapped := fieldOf(t, w.form)
					delete(wrapped, "type")
					delete(wrapped, w.elemKey)
					if !reflect.DeepEqual(scalar, wrapped) {
						t.Errorf("%s is emitted identically on a scalar and on `%s` today; if that "+
							"changes, memql#2951's note about which annotations survive wrapping "+
							"needs updating with it.\n  scalar:  %v\n  wrapped: %v",
							c.name, w.form, scalar, wrapped)
					}
				}
			})
		}
	})

	// @required is separate: it does not live on the field schema at all, it
	// enters the concept-level `required` list. Wrapping must not change that.
	t.Run("required survives wrapping", func(t *testing.T) {
		requiredOf := func(decl string) []any {
			src := "@version(\"1.0.0\")\n@namespace(\"aud\")\n@description(\"d\")\n" +
				"concept probe {\n  label string @required @description(\"l\")\n  " + decl + "\n}\n"
			decls := ExtractConceptDecls(src)
			if len(decls) == 0 {
				t.Fatalf("fixture %q did not parse", decl)
			}
			c, err := concept.BuildConceptFromDecl(decls[0], "v1:aud:probe")
			if err != nil {
				t.Fatalf("fixture %q does not build: %v", decl, err)
			}
			raw, _ := c.DefinitionSchema()
			var doc struct {
				Required []any `json:"required"`
			}
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatalf("unmarshal %q: %v", decl, err)
			}
			sort.Slice(doc.Required, func(i, j int) bool {
				return fmt.Sprint(doc.Required[i]) < fmt.Sprint(doc.Required[j])
			})
			return doc.Required
		}
		scalar := requiredOf(`f string @required @description("x")`)
		for _, form := range []string{
			`f []string @required @description("x")`,
			`f []string! @description("x")`,
			`f map[string]string @required @description("x")`,
		} {
			if got := requiredOf(form); !reflect.DeepEqual(got, scalar) {
				t.Errorf("`%s` must be as required as a scalar -- shipped concept fields in dsl/ "+
					"use the []T! form.\n  got: %v\n  scalar: %v", form, got, scalar)
			}
		}
	})
}

// TestConceptPropertyTypes_ValueAnnotationsAreCarriedIntoElementPosition pins
// the half of the element gap that was not about TYPES at all.
//
// A type-only safe-list could not express it: `object` wraps correctly, and yet
// `[]object @variant(...)` dropped the entire discriminated union. What was
// lost is the ANNOTATION that gives the field its structure --
//
//	object @variant     -> oneOf + x-discriminator     []object @variant  -> items:{type:object}
//	string @pattern     -> pattern + type              []string @pattern  -> pattern on the ARRAY,
//	                                                                        where JSON Schema
//	                                                                        ignores it for strings
//
// -- so a bundle modelling "a post is a list of text|image blocks" shipped with
// no validation at all and linted clean. Both now land on the element
// (memql#2951), which is where JSON Schema actually reads them.
func TestConceptPropertyTypes_ValueAnnotationsAreCarriedIntoElementPosition(t *testing.T) {
	schemaFor := func(t *testing.T, decl string) map[string]any {
		t.Helper()
		src := "@version(\"1.0.0\")\n@namespace(\"aud\")\n@description(\"d\")\n" +
			"concept probe {\n  label string @required @description(\"l\")\n  " + decl + "\n}\n"
		decls := ExtractConceptDecls(src)
		if len(decls) == 0 {
			t.Fatalf("fixture %q did not parse, so it measures nothing", decl)
		}
		c, err := concept.BuildConceptFromDecl(decls[0], "v1:aud:probe")
		if err != nil {
			t.Fatalf("fixture %q does not build: %v", decl, err)
		}
		raw, serr := c.DefinitionSchema()
		if serr != nil {
			t.Fatalf("schema for %q: %v", decl, serr)
		}
		var doc struct {
			Properties map[string]map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("unmarshal %q: %v", decl, err)
		}
		return doc.Properties["f"]
	}

	const variantBody = " @variant(discriminator=\"kind\") @description(\"x\") {\n" +
		"    text { body string }\n    image { url string }\n  }"

	t.Run("variant survives on a scalar", func(t *testing.T) {
		got := schemaFor(t, "f object"+variantBody)
		if _, ok := got["oneOf"]; !ok {
			t.Fatalf("the control is broken: a scalar `object @variant` must emit oneOf.\n  got: %v", got)
		}
		// memql#2951 records BOTH halves, so both are asserted.
		if _, ok := got["x-discriminator"]; !ok {
			t.Errorf("memql#2951 records a scalar `object @variant` as emitting oneOf AND "+
				"x-discriminator; "+
				"the discriminator is missing.\n  got: %v", got)
		}
	})

	// BOTH wrapped forms: the docs claim covers []T and map[string]T alike,
	// and review round 7 caught the map half ungated -- carrying @variant into
	// map values left the whole suite green while the claim stayed broad.
	t.Run("variant applies to each element when wrapped", func(t *testing.T) {
		for _, w := range []struct{ form, elemKey string }{
			{"f []object" + variantBody, "items"},
			{"f map[string]object" + variantBody, "additionalProperties"},
		} {
			elems, _ := schemaFor(t, w.form)[w.elemKey].(map[string]any)
			if _, ok := elems["oneOf"]; !ok {
				t.Errorf("`%s` drops its discriminated union, so a concept modelling \"a post is a "+
					"list of text|image blocks\" validates nothing.\n  element: %v", w.form, elems)
			}
			// BOTH halves, as on the scalar: the discriminator is what tells a
			// form generator or Sense hover which sibling picks the branch.
			if _, ok := elems["x-discriminator"]; !ok {
				t.Errorf("`%s` carries oneOf but not x-discriminator; a scalar `object @variant` "+
					"emits both.\n  element: %v", w.form, elems)
			}
			// And it must not ALSO sit on the wrapper, where it means nothing.
			if _, onWrapper := schemaFor(t, w.form)["oneOf"]; onWrapper {
				t.Errorf("`%s` emits oneOf on the wrapper as well as the element.", w.form)
			}
		}

		// The claim with teeth: an element that matches no variant is refused,
		// and one that matches a variant is accepted.
		mustReject(t, "f []object"+variantBody,
			[]any{map[string]any{"nonsense": 1}},
			[]any{map[string]any{"body": "hello"}})
	})
}

// TestConceptPropertyTypes_WrappedElementsAcceptConformingData is the inverse
// of the test that stood here, and it is the claim with teeth: a payload that
// conforms to the DECLARATION is now accepted, and one that does not is refused
// by the element schema.
//
// It drives the compiled schema rather than inspecting the emitted one, because
// the emitted-shape assertions elsewhere in this file would all pass on an
// element schema the validator never reaches.
//
// NOT every row proves the fix, and the distinction matters when reading a
// failure. `[]bool`, `[]int` and `map[string]int` lowered CORRECTLY before
// memql#2951 -- the old `memqlTypeToJSONType` mapped bool->boolean and
// int->integer -- so those three pass against the pre-change parser too and are
// regression guards, not evidence. The rows that were genuinely backwards
// before the change, i.e. the ones whose conforming payload was REJECTED
// because the element had been flattened to `string`, are the datetime, the
// composite and the `any` rows. (An earlier version of this comment claimed all
// of them; corrected in the memql#2951 review, which measured it.)
func TestConceptPropertyTypes_WrappedElementsAcceptConformingData(t *testing.T) {
	for _, c := range []struct {
		decl string
		// good CONFORMS to the declaration and must be accepted
		good any
		// bad does NOT conform and must be refused by the element schema
		bad any
	}{
		{"f []bool", []any{true}, []any{"s"}},
		{"f []int", []any{3}, []any{"s"}},
		{"f []datetime", []any{"2026-01-02T03:04:05Z"}, []any{"hello"}},
		{"f map[string]int", map[string]any{"count": 3}, map[string]any{"count": "s"}},
		{"f map[string]datetime", map[string]any{"at": "2026-01-02T03:04:05Z"},
			map[string]any{"at": "hello"}},
		// Composites: the inner type is reached, not collapsed to the outer one.
		{"f []map[string]string", []any{map[string]any{"a": "b"}}, []any{"s"}},
		{"f map[string]map[string]string", map[string]any{"a": map[string]any{"b": "c"}},
			map[string]any{"a": "s"}},
		{"f [][]int", []any{[]any{1, 2}}, []any{[]any{"s"}}},
		// `any` is unconstrained one level in, as it is in property position.
		// Only the accept half is meaningful here -- nothing is refusable --
		// so it is asserted on its own below rather than through the pair.
	} {
		t.Run(c.decl, func(t *testing.T) {
			mustReject(t, c.decl, c.bad, c.good)
		})
	}

	// `any` used to mean "must be a string" in element position, which is the
	// exact opposite of what it means as a property. Every JSON value conforms
	// now, so this asserts acceptance only.
	t.Run("any accepts every JSON value", func(t *testing.T) {
		for _, decl := range []struct {
			decl    string
			payload any
		}{
			{"f []any", []any{3, "s", true, nil, map[string]any{"k": "v"}}},
			{"f map[string]any", map[string]any{"n": 3, "s": "s", "b": true, "o": map[string]any{}}},
		} {
			mustAccept(t, decl.decl, decl.payload)
		}
	})
}

// mustAccept is mustReject's other half, for declarations with nothing
// refusable to pair against.
func mustAccept(t *testing.T, decl string, payload any) {
	t.Helper()
	id := "v1:aud:probeAccept"
	src := "@version(\"1.0.0\")\n@namespace(\"aud\")\n@description(\"d\")\n" +
		"concept probeAccept {\n  label string @required @description(\"l\")\n  " + decl + "\n}\n"
	decls := ExtractConceptDecls(src)
	if len(decls) == 0 {
		t.Fatalf("fixture %q did not parse, so it measures nothing", decl)
	}
	cc, err := concept.BuildConceptFromDecl(decls[0], id)
	if err != nil {
		t.Fatalf("fixture %q does not build: %v", decl, err)
	}
	raw, serr := cc.DefinitionSchema()
	if serr != nil {
		t.Fatalf("schema for %q: %v", decl, serr)
	}
	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2019
	if err := compiler.AddResource(id, bytes.NewReader(raw)); err != nil {
		t.Fatalf("register schema for %q: %v", decl, err)
	}
	schema, err := compiler.Compile(id)
	if err != nil {
		t.Fatalf("compile schema for %q: %v", decl, err)
	}
	if err := schema.Validate(map[string]any{"label": "l", "f": payload}); err != nil {
		t.Errorf("`%s` REFUSES %v, which conforms to the declaration.\n  schema: %s\n  error: %v",
			decl, payload, string(raw), err)
	}
}

// TestLintParity_SkipsSurviveAHardLoadError closes the last ungated path in
// memql#2909 (review round 15): LintUnifiedTree returns the concept skips it
// already gathered alongside a fatal load error, instead of discarding them.
//
// Without it a tree holding both problems reports only the second, so an
// author fixes the id error, re-runs, and only then learns about the dropped
// concept. Additive, but it is the difference between one lint round-trip and
// two -- and deleting the block left every test in this package green, which
// is why it needed one of its own.
func TestLintParity_SkipsSurviveAHardLoadError(t *testing.T) {
	root := fstest.MapFS{
		// Builds, but its schema fails -- a recoverable skip.
		"zzaaa/concepts.memql": {Data: []byte(`@version("1.0.0")
@namespace("zzaaa")
@description("A widget with a mistyped property type.")
concept widget {
  label    string   @required  @description("Widget label.")
  enabled  boolean             @description("Mistyped.")
}
`)},
		// @namespace disagreeing with the directory is a hard load ERROR
		// (the moved-file guard, #2614), not a warn-skip.
		"zzzzz/concepts.memql": {Data: []byte(`@version("1.0.0")
@namespace("somewhereelse")
@description("A gadget in the wrong place.")
concept gadget {
  label string @required @description("Gadget label.")
}
`)},
	}
	diags, _, err := LintUnifiedTree(nil, root)
	if err == nil {
		t.Fatal("the fixture must still produce a hard load error, or this proves nothing")
	}
	if len(diags) == 0 {
		t.Fatalf("the skip gathered before the hard error was discarded, so an author sees only "+
			"the id error and needs a second lint round to find the dropped concept "+
			"(memql#2909).\n  error: %v", err)
	}
	if !diagsContain(diags, "widget") {
		t.Errorf("the surviving diagnostic must name the dropped concept.\n  got: %+v", diags)
	}
}
