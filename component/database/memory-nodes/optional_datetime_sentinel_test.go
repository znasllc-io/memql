package memoryNodes

import (
	"encoding/json"
	"testing"
)

// optional_datetime_sentinel_test.go -- memql#3051.
//
// # The reported defect does not reproduce, and the reason is worth pinning
//
// An OPTIONAL scalar `datetime` lowers to a sentinel union (memql#1629) so a
// caller can leave the field unset:
//
//	oneOf: [ {type: string, format: date-time}
//	       , {type: string, maxLength: 0}
//	       , {type: null} ]
//
// memql#3051 read this as broken: if `format` is not asserted then "" matches
// BOTH the date-time member and the maxLength:0 member, and `oneOf` demands
// exactly one -- so the empty string would be rejected by the very sentinel
// that exists to permit it.
//
// The library rule it cites is right (v5.3.1 compiler.go: format is asserted
// when `draft.version < 2019 || AssertFormat || the format-assertion vocabulary
// is present`), and it is right that nothing here sets AssertFormat. What it
// missed is WHICH DRAFT APPLIES. `compiler.Draft = Draft2019` in
// concept_schema.go is only the DEFAULT for a resource that declares no
// `$schema`; our emitted schema declares one, and it is **draft-07**
// (concept_parser.go). The resource's own `$schema` wins, `7 < 2019`, so
// format IS asserted -- "" fails the date-time member, exactly one member
// matches, and the empty string is accepted.
//
// # But the tree is one `$schema` bump away from the bug
//
// That makes the draft-07 declaration load-bearing for validation behaviour,
// which is not obvious from reading it. It was not UNguarded -- an earlier
// version of this comment said so and that was wrong. concept_datetime_
// nullable_test.go has run the same four value cases through the production
// path since memql#1629, and it does go red if the emitted `$schema` is
// modernised. What was missing was not coverage of the behaviour but any
// statement of WHY the behaviour holds, which is what sent memql#3051 chasing a
// defect that is not there. Measured both ways against the real compiler:
//
//	$schema                                       ""          "not-a-date"
//	http://json-schema.org/draft-07/schema#       ACCEPTED    REJECTED
//	https://json-schema.org/draft/2019-09/schema  REJECTED    ACCEPTED
//
// So "modernising" the emitted `$schema` would silently break BOTH directions
// at once: it would introduce exactly the #3051 defect, and it would ALSO stop
// rejecting garbage in a field declared `datetime`.
//
// WHICH TEST CATCHES THAT, precisely, because an earlier version of this
// comment named the wrong one. TestOptionalDatetime_SentinelBehaviour is the
// guard: it drives the emitted schema, so bumping the `$schema` in
// concept_parser.go turns it red on both the empty-string and the garbage case.
// TestOptionalDatetime_EmitsSentinelUnion pins the declaration itself.
// TestOptionalDatetime_FormatAssertionDependsOnTheDeclaredDraft is a
// CHARACTERISATION of the library rule -- it explains why the guard holds and
// would fail if a jsonschema upgrade changed the rule, but it is built from the
// emitted schema rather than a literal so it cannot drift from what the emitter
// produces.

// optionalDatetimeConcept builds a concept with one optional scalar datetime
// field through the real parser, so the schema under test is the one the
// engine actually emits.
func optionalDatetimeConcept(t *testing.T) *Concept {
	t.Helper()
	c, err := ParseConceptMemQL([]byte(`
@description("A task with an optional deadline.")
concept todo {
  title  string    @required  @description("What to do.")
  dueAt  datetime  @description("Optional deadline; unset means no deadline.")
}
`), "v1/test/todo")
	if err != nil {
		t.Fatalf("ParseConceptMemQL: %v", err)
	}
	return c
}

// The union really is emitted, asserted separately from the behaviour so a
// change that alters the shape but not the outcome is still visible.
func TestOptionalDatetime_EmitsSentinelUnion(t *testing.T) {
	c := optionalDatetimeConcept(t)
	raw, err := c.DefinitionSchema()
	if err != nil {
		t.Fatalf("DefinitionSchema: %v", err)
	}
	var doc struct {
		Schema     string                    `json:"$schema"`
		Properties map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// The declared draft is the whole reason the union is sound, and this
	// struct already parses it -- an earlier version read it into a field and
	// then never asserted on it, which is the one line that pins the claim.
	const draft07 = "http://json-schema.org/draft-07/schema#"
	if doc.Schema != draft07 {
		t.Fatalf("emitted $schema = %q, want %q. Format assertion -- and therefore the "+
			"soundness of the sentinel union below -- depends on the declared draft being "+
			"< 2019 (memql#3051).", doc.Schema, draft07)
	}

	oneOf, ok := doc.Properties["dueAt"]["oneOf"].([]any)
	if !ok {
		t.Fatalf("optional datetime `dueAt` carries no oneOf; got %v", doc.Properties["dueAt"])
	}
	if len(oneOf) != 3 {
		t.Fatalf("expected 3 sentinel members, got %d: %v", len(oneOf), oneOf)
	}
}

// The reproduction memql#3051 asked for, driven through the real validator.
// Every case passes today: the reported defect does not exist.
func TestOptionalDatetime_SentinelBehaviour(t *testing.T) {
	c := optionalDatetimeConcept(t)

	for _, tc := range []struct {
		name    string
		value   any
		present bool
		wantOK  bool
		why     string
	}{
		{
			name: "empty string -- the clear-a-field sentinel", value: "", present: true, wantOK: true,
			why: "the reported defect. \"\" is the coalesce(x,\"\") convention memql#1629 added " +
				"the union FOR. It is accepted because format IS asserted (draft-07), so it " +
				"fails the date-time member and matches only maxLength:0.",
		},
		{name: "JSON null", value: nil, present: true, wantOK: true, why: "matches only {type: null}."},
		{name: "field absent entirely", present: false, wantOK: true, why: "an optional field may be omitted."},
		{
			name: "a real RFC3339 value", value: "2026-08-05T12:00:00Z", present: true, wantOK: true,
			why: "matches the date-time member only.",
		},
		{
			name: "garbage string", value: "not-a-date", present: true, wantOK: false,
			why: "the union's own comment promises a non-empty value must still be valid " +
				"RFC3339. That promise HOLDS -- and it holds for the same reason the empty " +
				"string is accepted, so the two cannot be separated.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{"title": "t"}
			if tc.present {
				payload["dueAt"] = tc.value
			}
			err := c.validate("definition", payload)
			switch {
			case tc.wantOK && err != nil:
				t.Errorf("payload %v was REJECTED, must be accepted.\n  why: %s\n  error: %v",
					payload, tc.why, err)
			case !tc.wantOK && err == nil:
				t.Errorf("payload %v was ACCEPTED, must be rejected.\n  why: %s", payload, tc.why)
			}
		})
	}
}

// The guard this issue actually earns.
//
// The emitted `$schema` is draft-07, and that is what makes `format` asserted
// (v5.3.1 asserts format when the resource draft is < 2019). Nothing else in
// the repo sets AssertFormat. So the draft declaration is load-bearing for
// VALIDATION, not just a compatibility label -- and swapping it for a newer
// draft silently flips two behaviours in opposite directions:
//
//   - "" starts being REJECTED (memql#3051's defect, made real), and
//   - garbage starts being ACCEPTED in a field declared `datetime`.
//
// Asserted against the real compiler rather than by reading the library, so
// this stays true across a jsonschema upgrade that changes the rule.
func TestOptionalDatetime_FormatAssertionDependsOnTheDeclaredDraft(t *testing.T) {
	// The schema under test is the EMITTED one with only its $schema swapped --
	// never a hand-written copy of the union. A literal copy silently keeps
	// asserting a shape the emitter has moved on from: add `minLength: 1` to the
	// date-time member (one of the fixes memql#3051 itself proposed) and a
	// hand-rolled copy stays green while describing something that no longer
	// exists. Deriving it means this test fails loudly instead.
	build := func(t *testing.T, key, draft string) func(any) error {
		t.Helper()
		emitted, err := optionalDatetimeConcept(t).DefinitionSchema()
		if err != nil {
			t.Fatalf("DefinitionSchema: %v", err)
		}
		var doc struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(emitted, &doc); err != nil {
			t.Fatalf("unmarshal emitted schema: %v", err)
		}
		dueAt, ok := doc.Properties["dueAt"]
		if !ok {
			t.Fatal("the emitted schema has no dueAt property to characterise")
		}
		// Only the dueAt member is lifted, wrapped in a minimal document with
		// the draft under test. Taking the whole emitted document would drag in
		// `required: [title]` and reject the bare payloads below for the wrong
		// reason -- but the UNION ITSELF is the emitter's, so the drift this
		// derivation exists to prevent is still prevented.
		raw, err := json.Marshal(map[string]any{
			"$schema":    draft,
			"type":       "object",
			"properties": map[string]any{"dueAt": json.RawMessage(dueAt)},
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		s, err := compileSchema(key, raw)
		if err != nil {
			t.Fatalf("compile %s: %v", draft, err)
		}
		return func(v any) error { return s.Validate(map[string]any{"dueAt": v}) }
	}

	// The draft we actually emit: the sentinel works AND garbage is caught.
	emitted := build(t, "memql3051-draft07", "http://json-schema.org/draft-07/schema#")
	if err := emitted(""); err != nil {
		t.Errorf("under the emitted draft-07 schema the empty-string sentinel must be accepted, got: %v", err)
	}
	if emitted("not-a-date") == nil {
		t.Error("under the emitted draft-07 schema a garbage datetime must be rejected")
	}

	// A newer draft turns format assertion OFF, and both behaviours invert.
	// This is asserted, not merely noted, so that anyone modernising the
	// emitted `$schema` is told exactly what it costs.
	newer := build(t, "memql3051-draft2019", "https://json-schema.org/draft/2019-09/schema")
	if newer("") == nil {
		t.Error("control broken: under draft 2019-09 format is NOT asserted, so \"\" should " +
			"match two members and be rejected. If this now passes, the library's " +
			"format-assertion rule changed and the emitted draft is no longer what makes " +
			"the sentinel union sound -- re-derive memql#3051 before relying on it.")
	}
	if err := newer("not-a-date"); err != nil {
		t.Errorf("control broken: under draft 2019-09 garbage should be ACCEPTED "+
			"(format unasserted), got: %v", err)
	}
}

// TestArrayDatetimeElementRejectsEmptyAndNull pins what `required` on an array
// element actually buys, which concept_parser.go's elementFromTypeRef comment
// now claims is CORRECTNESS rather than clarity.
//
// It is the assertion behind that claim, and it exists because the claim it
// replaced -- that `required` there "buys clarity, not correctness" -- was
// shipped without one. An element marked required emits a bare
// {"type":"string","format":"date-time"}; drop the required and it carries the
// same sentinel union a scalar does, at which point an empty or null ENTRY
// inside a []datetime becomes legal. That is a difference in the accepted value
// set, and nothing was holding it.
func TestArrayDatetimeElementRejectsEmptyAndNull(t *testing.T) {
	c, err := ParseConceptMemQL([]byte(`
@description("A thing with a list of timestamps.")
concept stamped {
  title   string      @required  @description("What it is.")
  stamps  []datetime             @description("When things happened.")
}
`), "v1/test/stamped")
	if err != nil {
		t.Fatalf("parse concept: %v", err)
	}

	for _, tc := range []struct {
		name    string
		payload map[string]any
		accept  bool
	}{
		{"a valid RFC3339 entry", map[string]any{"title": "t", "stamps": []any{"2026-08-05T12:00:00Z"}}, true},
		{"an empty list", map[string]any{"title": "t", "stamps": []any{}}, true},
		{"an EMPTY-STRING entry", map[string]any{"title": "t", "stamps": []any{""}}, false},
		{"a NULL entry", map[string]any{"title": "t", "stamps": []any{nil}}, false},
		{"a garbage entry", map[string]any{"title": "t", "stamps": []any{"not-a-date"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := c.validate("definition", tc.payload)
			if tc.accept && err != nil {
				t.Errorf("%v was REJECTED and should not be: %v", tc.payload, err)
			}
			if !tc.accept && err == nil {
				t.Errorf("%v was ACCEPTED. An array element is `required` precisely so an "+
					"empty or null entry is illegal one level in -- if that stops holding, "+
					"elementFromTypeRef's comment about what `required` buys is wrong again "+
					"(memql#3051).", tc.payload)
			}
		})
	}
}
