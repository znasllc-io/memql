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
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

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
// reason to reach for -- the JSON Schema names because memqlTypeToJSONType
// emits them, the rest because every neighbouring language accepts them.
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
		for wrong, want := range map[string]string{
			"boolean": "bool", "integer": "int", "int64": "int",
			"number": "float", "double": "float",
			"text": "string", "uuid": "string",
			"date": "datetime", "timestamp": "datetime",
			"list": "array", "dict": "object", "json": "object",
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

// TestConceptPropertyTypes_ElementTypesAreNotValidated pins the KNOWN GAP that
// docs/public/language/memql.md now warns about, so the warning and the code
// cannot drift apart.
//
// memqlTypeToJSONType ends in `default: return "string"`, and the array/map
// branches never consult suggestPropertyType -- so inside `[]<type>` and
// `map[string]<type>` an unrecognised element type is neither rejected nor
// corrected, it silently becomes `string`. `[]boolean` therefore builds a
// field that validates as `[]string`.
//
// Deliberately NOT fixed in memql#2909: that issue is about types the loader
// REJECTS (which drop a concept), and these are types it ACCEPTS. Closing the
// gap would reject DSL that loads today, which is a behaviour change for
// existing bundles rather than a lint fix. Filed as memql#2951.
//
// This test asserts the CURRENT behaviour. When the gap is closed it will
// fail, which is the intended signal: whoever closes it must also delete the
// caveat from the docs.
func TestConceptPropertyTypes_ElementTypesAreNotValidated(t *testing.T) {
	// The exact element schema each row claims. Asserted whole, so a change to
	// the lowering breaks the test rather than the documentation.
	str := map[string]any{"type": "string"}
	wantElementSchema := map[string]map[string]any{
		"[]boolean": str, "[]frobnicate": str, "map[string]boolean": str,
		"map[string]any": str, "map[string]map[string]string": str,
		"[]map[string]string": str,
		// inner type discarded entirely, not coerced
		"[][]frobnicate": {"type": "array"},
		// byte-identical to []string: no `format`, which is the whole defect
		"[]datetime": str, "map[string]datetime": str,
	}

	// Includes RECOGNISED types, not just unrecognised ones: review round 2
	// found `map[string]any` produces a string constraint, so `any` means
	// "unconstrained" as a property and "must be a string" as an element, and
	// composite element types collapse entirely. The caveat in the docs had to
	// widen to match, and so does this.
	for _, ty := range []string{
		"[]boolean", "[]frobnicate", "map[string]boolean",
		"map[string]any", "map[string]map[string]string", "[]map[string]string", "[][]frobnicate",
		// datetime is RECOGNISED and lowers to a bare string with no
		// `format`, so the RFC3339 check the scalar position enforces is
		// silently dropped. Review round 3 found it on the docs' safe-list,
		// which is the one sentence an author acts on.
		"[]datetime", "map[string]datetime",
	} {
		src := "@version(\"1.0.0\")\n@namespace(\"aud\")\n@description(\"d\")\n" +
			"concept probe {\n  label string @required @description(\"l\")\n  f " + ty + " @description(\"x\")\n}\n"
		decls := ExtractConceptDecls(src)
		if len(decls) == 0 {
			t.Fatalf("fixture for %q did not parse, so it measures nothing", ty)
		}
		c, err := concept.BuildConceptFromDecl(decls[0], "v1:aud:probe")
		if err != nil {
			t.Errorf("%q is now REJECTED. That is an improvement, but the caveat in "+
				"docs/public/language/memql.md still tells authors element types are "+
				"unvalidated -- delete it in the same change.\n  got: %v", ty, err)
			continue
		}
		// Assert what it EMITS, not merely that it builds. Review round 5
		// found the build-only check too weak: reverting the datetime fix, or
		// lowering unknown elements to `object` instead of `string`, both left
		// this green while contradicting the docs table row for row.
		raw, serr := c.DefinitionSchema()
		if serr != nil {
			t.Fatalf("schema for %q: %v", ty, serr)
		}
		var doc struct {
			Properties map[string]map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("unmarshal %q: %v", ty, err)
		}
		f := doc.Properties["f"]
		var inner map[string]any
		if items, ok := f["items"].(map[string]any); ok {
			inner = items
		} else if vals, ok := f["additionalProperties"].(map[string]any); ok {
			inner = vals
		}
		if inner == nil {
			t.Errorf("%q emitted no element schema at all: %v", ty, f)
			continue
		}
		// The WHOLE element schema, not just its type. Review round 6 showed the
		// type-only check still missed the case it was added for: actually
		// FIXING datetime -- attaching `format: date-time` to elements -- left
		// the suite green while the docs still said []datetime is byte-identical
		// to []string.
		if want := wantElementSchema[ty]; !reflect.DeepEqual(inner, want) {
			t.Errorf("%q must emit exactly %v -- that is what the docs and memql#2951 state. "+
				"Emitting %v means one of them is now wrong.", ty, want, inner)
		}
	}
}

// TestConceptPropertyTypes_ElementSafeListCarriesTheSameConstraints gates the
// one sentence in docs/public/language/memql.md an author will act on: which
// element types are dependable inside []<type> and map[string]<type>.
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

	for _, base := range []string{"string", "bool", "int", "float", "object"} {
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
// the exception that is genuinely live (concept.go PIIFields -> scrubPIIFields).
// Filed as memql#2959.
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

	// VALUE CONSTRAINTS: the key must land on the WRAPPER and NOT on the
	// elements. That IS the defect -- on the wrapper it is inert.
	t.Run("value constraints are inert when wrapped", func(t *testing.T) {
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
				// BOTH wrapped forms. Review round 6 found the map half of this
				// claim untested: making these constrain map VALUES left the
				// whole suite green while the docs said they were inert there.
				for _, w := range []struct{ form, elemKey string }{
					{"f []" + c.base + " " + c.ann + ` @description("x")`, "items"},
					{"f map[string]" + c.base + " " + c.ann + ` @description("x")`, "additionalProperties"},
				} {
					wrapped := fieldOf(t, w.form)
					elems, _ := wrapped[w.elemKey].(map[string]any)
					if _, onElems := elems[c.key]; onElems {
						t.Errorf("%s now constrains the elements of `%s`. That is the memql#2951 fix "+
							"-- remove this annotation from the inert list in "+
							"docs/public/language/memql.md and dsl/_reference/_concept.memql in the "+
							"same change.", c.key, w.form)
					}
					if _, onWrapper := wrapped[c.key]; !onWrapper {
						t.Errorf("%s is no longer emitted on the wrapper of `%s` either. The docs say "+
							"it lands there and is ignored; if it is now dropped outright, say so "+
							"instead.\n  got: %v", c.key, w.form, wrapped)
					}
				}
			})
		}
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

// TestConceptPropertyTypes_FieldAnnotationsAreNotCarriedIntoElementPosition
// pins the half of the element gap that is not about TYPES at all.
//
// Review round 4 found `object` on the docs' dependable list while
// `[]object @variant(...)` silently drops the entire discriminated union. The
// type is fine -- a bare `object` wraps correctly -- so a type-only safe-list
// cannot express the problem. What is dropped is the ANNOTATION that gives the
// field its structure, and the same is true of `@pattern`:
//
//	object @variant     -> oneOf + x-discriminator     []object @variant  -> items:{type:object}
//	string @pattern     -> pattern + type              []string @pattern  -> pattern on the ARRAY,
//	                                                                        where JSON Schema
//	                                                                        ignores it for strings
//
// So a bundle modelling "a post is a list of text|image blocks" ships with no
// validation at all and lints clean. Asserted as CURRENT behaviour; this fails
// when memql#2951 lands, which is the reminder to update the docs with it.
func TestConceptPropertyTypes_FieldAnnotationsAreNotCarriedIntoElementPosition(t *testing.T) {
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
		// The docs row claims BOTH halves, so both are asserted.
		if _, ok := got["x-discriminator"]; !ok {
			t.Errorf("the docs say a scalar `object @variant` emits oneOf AND x-discriminator; "+
				"the discriminator is missing.\n  got: %v", got)
		}
	})

	t.Run("variant is DROPPED on an array", func(t *testing.T) {
		got := schemaFor(t, "f []object"+variantBody)
		items, _ := got["items"].(map[string]any)
		if _, ok := items["oneOf"]; ok {
			t.Errorf("`[]object @variant` now carries its discriminated union. That is the fix " +
				"memql#2951 wants -- delete the annotation caveat from " +
				"docs/public/language/memql.md and dsl/_reference/_concept.memql in the same change.")
		}
	})

	t.Run("pattern lands on the array, not its items", func(t *testing.T) {
		got := schemaFor(t, `f []string @pattern("^[a-z]+$") @description("x")`)
		items, _ := got["items"].(map[string]any)
		if _, onItems := items["pattern"]; onItems {
			t.Errorf("`[]string @pattern` now constrains its items. Same as above -- the docs " +
				"caveat has to go with it.")
		}
		if _, onArray := got["pattern"]; !onArray {
			t.Errorf("the docs say @pattern lands on the ARRAY and is ignored there. It is now on "+
				"neither the array nor its items, so the documented behaviour is wrong either "+
				"way.\n  got: %v", got)
		}
	})
}
