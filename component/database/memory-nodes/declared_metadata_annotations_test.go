package memoryNodes

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	goparser "go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/provenance"
)

// declared_metadata_annotations_test.go -- memql#2960.
//
// Four field annotations are DECLARED METADATA: @unique, @immutable, @secret
// and @default parse, reach the emitted schema, and nothing in the engine acts
// on them. The reference (dsl/_reference/_concept.memql section 8),
// docs/public/language/attribute-matrix.md and reserved.md all now say so
// plainly, having previously promised enforcement that does not exist.
//
// A documentation-only fix would repeat the very failure memql#2960 is an
// instance of -- a rule recorded somewhere and enforced nowhere. So the split
// is gated from both sides:
//
//   - the keys are still EMITTED (an author's declaration is not silently
//     dropped, and @pii's history shows a marker can become enforced later);
//   - the keys are still read by NOTHING, which is what makes the docs true.
//
// When enforcement lands for any of them, the second half fails and names the
// documents that have to change in the same commit. That is the point: the
// test is a tripwire on the docs, not an endorsement of the gap.

// buildFixtureConcept parses a single-concept fixture and builds it. The
// concept is picked out of File.Definitions rather than via
// component/memql.ExtractConceptDecls, which would be an import cycle from
// here.
func buildFixtureConcept(t *testing.T, src string) *Concept {
	t.Helper()
	file, err := parser.ParseFile(src)
	if err != nil {
		t.Fatalf("fixture did not parse: %v", err)
	}
	for _, def := range file.Definitions {
		decl, ok := def.(*parser.ConceptDecl)
		if !ok {
			continue
		}
		c, err := BuildConceptFromDecl(decl, "v1:ref:probe")
		if err != nil {
			t.Fatalf("fixture does not build: %v", err)
		}
		return c
	}
	t.Fatal("fixture declared no concept, so it measures nothing")
	return nil
}

// helper: build a concept from source and return its property schemas.
func propertySchemas(t *testing.T, src string) map[string]map[string]any {
	t.Helper()
	c := buildFixtureConcept(t, src)
	raw, err := c.DefinitionSchema()
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	var doc struct {
		Properties map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return doc.Properties
}

const declaredMetadataFixture = `@version("1.0.0")
@namespace("ref")
@description("d")
concept probe {
  label      string  @required @description("l")
  uniqueKey  string  @unique @description("u")
  externalId string  @immutable @description("i")
  apiKey     string  @secret @description("s")
  tier       string  @default("bronze") @description("t")
}
`

// The emission half. An author's declaration must survive into the schema --
// dropping it would make the annotation unrecoverable when enforcement lands,
// and @pii is the precedent for a marker graduating from declared to enforced
// without a schema change.
func TestDeclaredMetadataAnnotationsAreStillEmitted(t *testing.T) {
	props := propertySchemas(t, declaredMetadataFixture)
	for _, tc := range []struct{ field, key string }{
		{"uniqueKey", "x-unique"},
		{"externalId", "x-immutable"},
		{"apiKey", "x-secret"},
		{"tier", "default"},
	} {
		if _, ok := props[tc.field][tc.key]; !ok {
			t.Errorf("%s no longer emits %q. If the annotation was retired rather than left as "+
				"declared metadata, section 8 of dsl/_reference/_concept.memql still lists it "+
				"as accepted -- update it in the same change.\n  got: %v",
				tc.field, tc.key, props[tc.field])
		}
	}
}

// The reference sheet this issue rewrites is the page an author copies from,
// and NOTHING was building it. dsl/_reference/ is absent from dsl/embed.go's
// embed list and underscore-skipped by dslfs.WalkMemqlFiles, so every corpus
// gate structurally excludes it; the one test that does read it
// (component/memql/sense.TestDiagnose_ReferenceFiles_NoErrors) gates lex, parse
// and rewrite with a registry-less service and never reaches concept build.
//
// So a worked example could declare a reserved field, or misspell an
// annotation, and ship green -- teaching the author something the engine
// rejects. That is the same defect class memql#2960 exists to delete, one level
// up: the reference asserting behaviour that is not behaviour.
//
// This gate closes it by putting every concept in the sheet through
// BuildConceptFromDecl, which is the check that runs on a real domain.
func TestReferenceConceptSheetActuallyBuilds(t *testing.T) {
	path := filepath.Join(repoRoot(t), "dsl", "_reference", "_concept.memql")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read reference sheet: %v", err)
	}
	file, err := parser.ParseFile(string(src))
	if err != nil {
		t.Fatalf("dsl/_reference/_concept.memql did not parse: %v", err)
	}

	built := 0
	for _, def := range file.Definitions {
		decl, ok := def.(*parser.ConceptDecl)
		if !ok {
			continue
		}
		built++
		if _, err := BuildConceptFromDecl(decl, "v1:ref:"+decl.Name); err != nil {
			t.Errorf("concept %q in dsl/_reference/_concept.memql does not build: %v\n\n"+
				"The reference sheet is what an author copies from, so an example the engine "+
				"refuses teaches a rule that does not exist. Common causes: declaring a "+
				"reserved row intrinsic (id / createdAt / createdBy / concept / type / "+
				"partition / payload / schema -- see section 12) as a payload field, or an "+
				"annotation the parser does not accept.", decl.Name, err)
		}
	}
	if built == 0 {
		t.Fatal("no concepts found in the reference sheet, so this gate measures nothing")
	}
}

// emitSite is the one non-test Go location allowed to spell a declared-metadata
// key: the emitter that puts it into the schema.
const emitSite = "component/database/memory-nodes/concept_parser.go"

// The half that makes the documentation true, and the tripwire on it.
//
// The assertion is not "the key appears once". It is the stronger, and actually
// load-bearing, "the key appears once AND that one appearance is a WRITE" --
// the literal used as the index of an assignment target, `schema[key] = ...`.
// That is what "emitted, read by nothing" means, stated as something a machine
// can check.
//
// It is deliberately an AST scan rather than a grep, because a textual count is
// the wrong instrument for this question and can be defeated three ways, each
// of which was demonstrated against an earlier draft of this test:
//
//   - hoist the key into a named constant and point the emitter at the
//     identifier. The literal count stays at exactly 1 -- it is now the const
//     declaration -- while any number of readers reference the name. Declaring
//     that constant in THIS file also defeats a check that merely pins the
//     path, and this file is precisely where anyone would put it.
//   - build the key by concatenation (`"x-" + "secret"`). No literal matches at
//     all, so a count-based gate reads 0 and reports the emitter missing rather
//     than the reader added.
//   - mention the key in a comment. A grep counts prose as code and fails,
//     telling its author to go and edit three documents that are correct -- and
//     the likeliest author of such a comment is someone documenting the very
//     non-enforcement this test exists to pin.
//
// Walking the AST answers all three: a const declaration is a ValueSpec and not
// an assignment index, so it fails wherever it is written; a concatenation
// yields no matching literal, so the write disappears and the count fails; and
// comments are not AST expressions at all, so they are structurally invisible
// instead of specially-cased.
// x-secret LEFT this list in memql#3036. It is now read by
// Concept.SecretFields() through a `json:"x-secret"` struct tag -- the same
// shape as the x-pii reader the positive control below pins -- and drives
// redaction of a rejected value out of validation error messages. Its
// enforcement is PARTIAL by ruling (error messages only; NOT query results,
// NOT structured logs), and the three documents this test used to point at now
// name the covered surface and the uncovered ones explicitly.
//
// It stays pinned by TestSecretEnforcementIsRealAndScoped below, so this is a
// stronger assertion replacing a weaker one rather than a key dropping out of
// the suite.
func TestDeclaredMetadataKeysAreReadByNothing(t *testing.T) {
	root := repoRoot(t)
	for _, key := range []string{"x-unique", "x-immutable"} {
		t.Run(key, func(t *testing.T) {
			const remedy = "\n\nIf enforcement landed, that is good news and three documents are now WRONG. " +
				"Update all of them in the same change:\n" +
				"  - dsl/_reference/_concept.memql section 8 (move it out of DECLARED METADATA, " +
				"and fix the @description on its worked example -- that string ships in the schema)\n" +
				"  - docs/public/language/attribute-matrix.md\n" +
				"  - docs/public/language/reserved.md"

			hits := keyLiteralsInNonTestGo(t, root, key)
			if len(hits) != 1 {
				t.Errorf("%q appears as a literal in %d non-test Go locations, not 1 (the emit site).\n  %s%s",
					key, len(hits), joinHits(hits), remedy)
				return
			}
			hit := hits[0]
			if !hit.isWriteIndex {
				t.Errorf("the sole literal %q at %s is NOT a write (`schema[%q] = ...`).\n\n"+
					"That means the key is no longer merely emitted. The usual cause is hoisting it "+
					"into a named constant -- the literal count stays at 1 while readers reference "+
					"the identifier, which is enforcement.%s",
					key, hit, key, remedy)
				return
			}
			if !strings.HasPrefix(hit.path, emitSite) {
				t.Errorf("the emit of %q moved off %s to %s. If the emitter genuinely moved, update "+
					"emitSite in this test; if a second writer appeared, the docs describe one.%s",
					key, emitSite, hit, remedy)
			}
		})
	}
}

// The positive control on the scan itself, and the reason it exists is that the
// gate above can only ever report an ABSENCE. "No reader found" is the same
// output whether there is genuinely no reader or the scan cannot see the kind
// of reader that is there -- so a gate with no positive control is a gate that
// cannot distinguish working from broken.
//
// x-pii is the right control precisely because it is NOT declared metadata: it
// is enforced today, read by Concept.PIIFields through a struct tag. So the
// scan run against it MUST find that reader. If this test starts passing
// vacuously -- or if the tag arm is simplified away -- the three keys above go
// on reporting "read by nothing" whether or not it is still true.
//
// This is the acceptance criterion this PR was parked on, written as a test
// rather than as a claim.
func TestTheScanSeesAStructTagReader(t *testing.T) {
	const (
		enforcedKey = "x-pii"
		readerFile  = "component/database/memory-nodes/concept.go"
	)

	hits := keyLiteralsInNonTestGo(t, repoRoot(t), enforcedKey)

	var tagReader *keyHit
	for i := range hits {
		if !hits[i].isWriteIndex && strings.HasPrefix(hits[i].path, readerFile) {
			tagReader = &hits[i]
			break
		}
	}
	if tagReader == nil {
		t.Fatalf("the scan found no struct-tag reader of %q in %s, but Concept.PIIFields reads it "+
			"there via `json:\"%s\"`.\n  found: %s\n\n"+
			"The scan is therefore blind to the struct-tag form -- which is the idiom this repo "+
			"reads these keys with, and the one memql#3036 will use when it implements @secret "+
			"redaction by copying PIIFields. While that is true, the gate above reports "+
			"\"read by nothing\" for x-unique / x-immutable / x-secret whether or not it is "+
			"still true, and its first real exercise would be the case it cannot see.",
			enforcedKey, readerFile, enforcedKey, joinHits(hits))
	}

	// And the other half: x-pii must NOT pass the "emitted, read by nothing"
	// shape the gate above asserts. If it did, that shape would be satisfiable
	// by an enforced key, and satisfying it would mean nothing.
	if len(hits) == 1 && hits[0].isWriteIndex {
		t.Errorf("%q looks emitted-and-never-read, but it is enforced. The assertion the gate "+
			"above makes is not distinguishing enforced keys from unenforced ones.", enforcedKey)
	}
}

// @default's gap is different in kind: the key IS emitted into the schema, and
// the schema is a *validation* document, so nothing applies it on insert.
// Pinned separately because the fix is separate -- applying it means touching
// the write path, not adding a reader for an x- key.
//
// This is the behaviour CLAUDE.md already states ("a concept-field @default is
// NOT a substitute -- it is never applied on insert either (memql#2960), so ??
// is the only mechanism"), so if it ever changes, CLAUDE.md changes with it.
//
// memql#3038 RULED on what to do about that, and the answer was neither "apply
// it" nor "retire it" -- it stays documentation, and the mistake of writing one
// and getting nothing is caught at AUTHORING time instead of never. The gate is
// dsl's TestDefaultIsCoalescedOrStamped, which fails when an optional TOP-LEVEL
// concept field carries @default and no bound mutation stamps it. So this test
// keeps its exact meaning: it pins that the engine does not apply the value,
// which is precisely the premise the gate rests on. If this test ever has to
// change because Create starts filling defaults, that gate becomes wrong in the
// same commit.
//
// The gate's scope is narrower than this test's subject, deliberately, and
// stated so a reader does not infer tree-wide coverage: it scans the IN-REPO
// tree only (a MEMQL_DSL_PATH bundle is never scanned), and it skips NESTED
// object leaves (no write form stamps a single leaf). Nothing is applied at any
// depth, in-repo or mounted -- that part is universal, and it is what this test
// pins.
func TestDefaultIsEmittedButNeverApplied(t *testing.T) {
	props := propertySchemas(t, declaredMetadataFixture)
	if got := props["tier"]["default"]; got != "bronze" {
		t.Fatalf("control broken: @default must reach the schema, got %v", got)
	}
	// A payload omitting the defaulted field validates -- which is the whole
	// point: the schema does not require it and does not fill it. If a future
	// change makes the field required, or makes validation supply the value,
	// this is where it surfaces.
	if _, isRequired := requiredSet(t, declaredMetadataFixture)["tier"]; isRequired {
		t.Error("a @default field became required, which is a behaviour change the reference " +
			"does not describe")
	}

	// The write path itself, because the two assertions above are exactly the
	// schema-inspection failure memql#2960 names ("asserting x-unique is present
	// is what makes this look covered today"). Neither of them changes if
	// Concept.Create starts filling defaults tomorrow, so on their own they are a
	// false PASS on the one behaviour this test is named for.
	assertDefaultNotAppliedOnInsert(t)

	// The shape-guessing lowering, which memql#3248 demoted to a FALLBACK: it
	// is reached only for object / array / map / any now, where the declared
	// type does not narrow the literal to one reading. Its behaviour is
	// unchanged and still pinned, because those types still go through it --
	// but it is no longer what a string / bool / int / float / datetime field
	// gets. That is parseTypedDefaultValue, covered by
	// TestDefaultIsLoweredByItsDeclaredType below.
	for _, tc := range []struct {
		name string
		want any
	}{
		{"true", true},
		{"7", int64(7)},
		{"1.5", 1.5},
		{"2026-01-02T03:04:05Z", "2026-01-02T03:04:05Z"}, // left as a string
	} {
		if got := parseDefaultValue(tc.name); got != tc.want {
			t.Errorf("parseDefaultValue(%q) = %#v, want %#v -- this is the untyped fallback for "+
				"object/array/map/any and its behaviour is deliberately unchanged", tc.name, got, tc.want)
		}
	}
}

// TestDefaultIsLoweredByItsDeclaredType covers the half memql#3248 fixed: a
// @default is lowered against the field's DECLARED type, so one that could
// never be a value of that field is refused at load rather than emitted.
//
// This does NOT change the settled rule that @default is never applied on
// insert at any depth -- TestDefaultIsEmittedButNeverApplied above still pins
// that, and the nested-leaf carve-out stands. It closes the narrower hole the
// issue named: the emitted `default` keyword is read by the SDK, sense hover
// and the preferences form generators, so a wrong one is wrong documentation
// shipped to three consumers rather than an inert annotation.
func TestDefaultIsLoweredByItsDeclaredType(t *testing.T) {
	// ACCEPTED -- the literal matches the declaration.
	for _, tc := range []struct {
		typeName string
		text     string
		want     any
	}{
		{"bool", "true", true},
		{"bool", "false", false},
		{"int", "7", int64(7)},
		{"int", "-1", int64(-1)},
		{"float", "1.5", 1.5},
		{"float", "2", float64(2)}, // an integer literal is a valid float
		{"enum", "active", "active"},
		{"datetime", "2026-01-02T03:04:05Z", "2026-01-02T03:04:05Z"},
		{"datetime", "", ""}, // the unset sentinel an optional datetime accepts (memql#1629)

		// The string rows are the second defect memql#3248 names. Shape
		// guessing turned these into a bool and two numbers, so the emitted
		// `default` carried the wrong JSON TYPE for the field it annotates. A
		// string field's default is the string, always.
		{"string", "0", "0"},
		{"string", "true", "true"},
		{"string", "1.5", "1.5"},
	} {
		got, err := parseTypedDefaultValue(tc.typeName, tc.text)
		if err != nil {
			t.Errorf("parseTypedDefaultValue(%q, %q) errored: %v -- this literal is valid for the type",
				tc.typeName, tc.text, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseTypedDefaultValue(%q, %q) = %#v (%T), want %#v (%T)",
				tc.typeName, tc.text, got, got, tc.want, tc.want)
		}
	}

	// REFUSED -- the literal could not be a value of the declared type. The
	// datetime rows are the headline: before memql#3248 every one of them was
	// accepted, and `whenField datetime @default("true")` emitted the BOOL true
	// as the default of a field whose schema says format: date-time.
	for _, tc := range []struct {
		typeName string
		text     string
	}{
		{"datetime", "true"},
		{"datetime", "7"},
		{"datetime", "1.5"},
		{"datetime", "not-a-timestamp"},
		{"datetime", "2026-01-02"}, // a bare date is not RFC3339
		{"bool", "yes"},
		{"bool", "1"},
		{"bool", "True"}, // the grammar's spelling is exactly `true`
		{"int", "1.5"},
		{"int", "abc"},
		{"int", "true"},
		{"float", "abc"},
		{"float", "true"},
	} {
		got, err := parseTypedDefaultValue(tc.typeName, tc.text)
		if err == nil {
			t.Errorf("parseTypedDefaultValue(%q, %q) = %#v, want an error -- a %s field cannot "+
				"have that default, and accepting it emits a `default` of the wrong type into "+
				"the schema the SDK and sense hover read", tc.typeName, tc.text, got, tc.typeName)
		}
	}

	// Types with no single correct reading keep the untyped fallback. Refusing
	// what cannot be judged would reject valid declarations to catch nothing.
	for _, typeName := range []string{"object", "array", "map", "any", "somethingUnrecognised"} {
		if _, err := parseTypedDefaultValue(typeName, "whatever"); err != nil {
			t.Errorf("parseTypedDefaultValue(%q, ...) errored: %v -- these types must fall back "+
				"to the untyped lowering, not be refused", typeName, err)
		}
	}
}

// buildConceptErr is buildFixtureConcept's counterpart for sources that are
// SUPPOSED to fail: it returns the build error instead of failing the test, so
// a refusal can be asserted rather than merely survived.
func buildConceptErr(t *testing.T, src string) error {
	t.Helper()
	file, err := parser.ParseFile(src)
	if err != nil {
		return err
	}
	for _, def := range file.Definitions {
		decl, ok := def.(*parser.ConceptDecl)
		if !ok {
			continue
		}
		_, err := BuildConceptFromDecl(decl, "v1:ref:probe")
		return err
	}
	t.Fatal("fixture declared no concept, so it measures nothing")
	return nil
}

// TestBadDefaultRefusesToLoad drives the REAL load path, because
// TestDefaultIsLoweredByItsDeclaredType calls the lowering helper directly and
// would keep passing if the call site were reverted to the shape-guessing one.
// A unit test on a helper nothing calls proves nothing (memql#3248).
func TestBadDefaultRefusesToLoad(t *testing.T) {
	const tmpl = `@version("1.0.0")
@namespace("ref")
@description("d")
concept probe {
  probeField %s @default(%q) @description("p")
}
`

	// The exact case the issue named. Before memql#3248 this built without
	// complaint and emitted `default: true` on a field declared
	// format: date-time.
	for _, tc := range []struct{ typeName, text string }{
		{"datetime", "true"},
		{"datetime", "7"},
		{"datetime", "nonsense"},
		{"bool", "yes"},
		{"int", "1.5"},
		{"float", "abc"},
	} {
		src := fmt.Sprintf(tmpl, tc.typeName, tc.text)
		if err := buildConceptErr(t, src); err == nil {
			t.Errorf("`probeField %s @default(%q)` built successfully, want a refusal -- the "+
				"emitted schema would carry a default that field can never hold",
				tc.typeName, tc.text)
		}
	}

	// Positive control. If this stops building, the check above is refusing
	// valid declarations and the failures are meaningless.
	for _, tc := range []struct{ typeName, text string }{
		{"datetime", "2026-01-02T03:04:05Z"},
		{"datetime", ""},
		{"bool", "true"},
		{"int", "7"},
		{"float", "1.5"},
		{"string", "0"},
	} {
		src := fmt.Sprintf(tmpl, tc.typeName, tc.text)
		if err := buildConceptErr(t, src); err != nil {
			t.Errorf("`probeField %s @default(%q)` failed to build: %v -- this declaration is valid",
				tc.typeName, tc.text, err)
		}
	}
}

// TestBareDefaultLiteralIsRead covers the defect the type-directed lowering
// EXPOSED rather than caused (memql#3248).
//
// A bare argument is not stored where a quoted one is. The parser has no value
// to bind an unquoted token to, so it records the token as an Args KEY:
//
//	@default(false)     Value=<nil>    Args=map["false":true]
//	@default("false")   Value="false"  Args=map[]
//
// attrString only type-asserts Value to string, so it reported "" for the bare
// spelling. `isGroupGA bool @default(false)` in dsl/cognition/concepts.memql --
// the live spelling -- therefore emitted `"default": ""` on a field declared
// `"type": "boolean"`, and had done since the line was written.
//
// The two spellings mean the same thing and must not diverge. This pins that,
// because nothing else did: the old lowering accepted "" for every type, so the
// bug produced no error to notice.
func TestBareDefaultLiteralIsRead(t *testing.T) {
	const src = `@version("1.0.0")
@namespace("ref")
@description("d")
concept probe {
  bareBool    bool  @default(false)    @description("b")
  quotedBool  bool  @default("false")  @description("q")
  bareInt     int   @default(7)        @description("i")
}
`
	props := propertySchemas(t, src)

	for _, field := range []string{"bareBool", "quotedBool"} {
		got, ok := props[field]["default"]
		if !ok {
			t.Errorf("%s emitted no default at all", field)
			continue
		}
		if got != false {
			t.Errorf("%s default = %#v (%T), want the BOOL false. An empty string here is the "+
				"pre-memql#3248 bug: a bare argument read as \"\" and emitted on a boolean field",
				field, got, got)
		}
	}

	// The bare spelling is not bool-specific -- any unquoted token takes the
	// same path, so a bare int must lower as an int rather than as "".
	if got := props["bareInt"]["default"]; got != float64(7) && got != int64(7) {
		t.Errorf("bareInt default = %#v (%T), want 7 -- a bare numeric argument reads through the "+
			"same Args-key path as a bare bool", got, got)
	}
}

// capturingStore is the whole Store surface Concept.Create touches: it keeps
// the node that was actually written so the test can read the payload the
// engine produced rather than the schema it validated against.
type capturingStore struct{ written *MemoryNode }

func (s *capturingStore) InsertMemoryNode(_ context.Context, node *MemoryNode) error {
	s.written = node
	return nil
}

func (s *capturingStore) QueryMemoryNodes(_ context.Context, _ QueryParams) ([]MemoryNode, error) {
	return nil, nil
}

// assertDefaultNotAppliedOnInsert drives the real write path and asserts the
// defaulted field is absent from the stored payload.
//
// This is the assertion the reference sentence actually rests on -- "@default
// ... is NEVER APPLIED on insert -- Concept.Create validates and marshals the
// payload verbatim" -- and the one CLAUDE.md's "?? is the only mechanism that
// fills a value" depends on. Patch Concept.Create to copy schema defaults into
// the payload and this fails; without it, that patch is invisible.
func assertDefaultNotAppliedOnInsert(t *testing.T) {
	t.Helper()

	c := buildFixtureConcept(t, declaredMetadataFixture)
	store := &capturingStore{}
	ctx := provenance.ContextWithProvenance(
		context.Background(),
		provenance.Provenance{Kind: provenance.KindDirect, Name: "declared-metadata-test"},
	)

	// `tier` carries @default("bronze") and is deliberately omitted.
	if _, err := c.Create(ctx, store, CreateParams{
		Actor:   "tester",
		ID:      "probe1",
		Payload: map[string]any{"label": "x"},
	}); err != nil {
		t.Fatalf("control broken: insert omitting the defaulted field must succeed, got %v", err)
	}
	if store.written == nil {
		t.Fatal("control broken: nothing was written, so this measures nothing")
	}

	var stored map[string]any
	if err := json.Unmarshal(store.written.Payload, &stored); err != nil {
		t.Fatalf("unmarshal stored payload: %v", err)
	}
	if got, present := stored["tier"]; present {
		t.Errorf("@default WAS applied on insert: stored payload carries tier=%v.\n\n"+
			"That is enforcement, and it makes two documents WRONG. Update both in the same change:\n"+
			"  - dsl/_reference/_concept.memql section 8 (move @default out of DECLARED METADATA, "+
			"and fix the @description on its three worked examples -- those strings ship in the schema)\n"+
			"  - CLAUDE.md, which states that `??` is the only mechanism that fills a value", got)
	}
}

func requiredSet(t *testing.T, src string) map[string]struct{} {
	t.Helper()
	c := buildFixtureConcept(t, src)
	raw, err := c.DefinitionSchema()
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	var doc struct {
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out := map[string]struct{}{}
	for _, r := range doc.Required {
		out[r] = struct{}{}
	}
	return out
}

// repoRoot walks up from this package to the REPOSITORY root.
//
// It looks for go.work, not go.mod. This scan reads repo-relative paths
// (dsl/_reference/..., docs/...) and reports repo-relative emit sites, and
// since memql#3228 the tree is many modules -- component/database is one of
// them, so a go.mod walk stopped there and the scan looked for
// component/database/dsl/_reference/ and reported emit sites shorn of their
// component/database/ prefix (memql#3242). go.work exists exactly once, at the
// repository root, which is the thing every caller here means.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 12; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not locate the repository root (go.work), so this scan measures nothing")
	return ""
}

// keyHit is one string-literal occurrence of a schema key in non-test Go.
type keyHit struct {
	path string // repo-relative, slash-separated
	line int
	// isWriteIndex is true when the literal is the index of an assignment
	// TARGET -- `schema[key] = ...` -- i.e. the emit, as opposed to any read.
	isWriteIndex bool
}

func (h keyHit) String() string { return h.path + ":" + strconv.Itoa(h.line) }

func joinHits(hits []keyHit) string {
	parts := make([]string, 0, len(hits))
	for _, h := range hits {
		s := h.String()
		if h.isWriteIndex {
			s += " (write)"
		} else {
			s += " (READ or declaration)"
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "\n  ")
}

// keyLiteralsInNonTestGo returns every string-literal occurrence of key in a
// non-test .go file under root, each flagged with whether it is the emit write.
//
// Comments are invisible here by construction: they are not expressions, so
// ast.Inspect never yields one. That is the point -- documenting a key is not
// reading it, and a scan that cannot tell those apart punishes the person
// writing the documentation this test exists to protect.
//
// FOUR limits of scope, listed in full because the failure messages above
// assert three documents are wrong on the strength of this scan, and because
// this list said "two" while a third and fourth existed -- which is the same
// shape of defect the scan is built to catch:
//
//   - Go only. A consumer of these keys in sdk/ts or the cockpit would pass
//     unnoticed. The definition schema does reach clients, so the docs' claim is
//     broader than what is mechanically enforced here.
//   - A file that does not parse is skipped rather than failing the run (there
//     is deliberately-invalid Go under testdata trees). A live reader lives in
//     compiling code, so this cannot hide one -- but the count of parsed files
//     is asserted non-zero below, so a scan that silently reached nothing fails
//     instead of passing.
//   - A READER that builds the key by concatenation (`"x-" + "secret"`) adds no
//     matching literal and is invisible. Note the asymmetry with the emit: a
//     concatenated EMIT is caught, because the literal count drops to zero and
//     the assertion is on an exact count. A concatenated reader simply adds
//     nothing to count.
//   - A reader that never names the key at all -- a generic scan such as
//     `strings.HasPrefix(k, "x-")` over the schema's properties -- is likewise
//     invisible.
//
// The last two are inherent to any key-literal scan and are the price of
// choosing an exact matcher over a heuristic one. They are recorded rather than
// closed; a gate whose limits are written down can be trusted at its edges,
// which is the whole argument this file makes about documentation.
func keyLiteralsInNonTestGo(t *testing.T, root, key string) []keyHit {
	t.Helper()

	quoted := strconv.Quote(key)
	var hits []keyHit
	parsed := 0

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // an unreadable path is not this test's business
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		fset := token.NewFileSet()
		file, parseErr := goparser.ParseFile(fset, path, nil, goparser.SkipObjectResolution)
		if parseErr != nil {
			return nil // not valid Go; cannot contain a live reader
		}
		parsed++

		// First pass: every literal used as the index of an assignment target.
		writes := map[*ast.BasicLit]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok {
				return true
			}
			for _, lhs := range assign.Lhs {
				idx, ok := lhs.(*ast.IndexExpr)
				if !ok {
					continue
				}
				if lit, ok := idx.Index.(*ast.BasicLit); ok && lit.Kind == token.STRING && lit.Value == quoted {
					writes[lit] = true
				}
			}
			return true
		})

		// Second pass: every occurrence of the literal, anywhere.
		rel, _ := filepath.Rel(root, path)
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || lit.Value != quoted {
				return true
			}
			hits = append(hits, keyHit{
				path:         filepath.ToSlash(rel),
				line:         fset.Position(lit.Pos()).Line,
				isWriteIndex: writes[lit],
			})
			return true
		})

		// Third pass: STRUCT TAGS, which the pass above cannot see and which
		// are the idiom this repo actually reads these keys with.
		//
		// A tag is a *ast.BasicLit too, but its value is `json:"x-pii"` --
		// backquoted, with the key nested inside. It never equals `"x-pii"`,
		// so an equality test skips it entirely. That is not a hypothetical
		// gap: Concept.PIIFields() reads x-pii exactly this way
		// (concept.go:526), and memql#3036 -- approved, and blocked on this
		// PR -- will implement @secret redaction by copying that function.
		// The tripwire's first real exercise was the one case it could not
		// see, which is the worst possible place for a blind spot.
		//
		// Matched by walking *ast.Field.Tag specifically rather than by
		// searching every backquoted string, so an unrelated raw string that
		// happens to contain the key is not counted as a reader.
		ast.Inspect(file, func(n ast.Node) bool {
			field, ok := n.(*ast.Field)
			if !ok || field.Tag == nil {
				return true
			}
			// Both the bare key and the key carrying tag OPTIONS. Matching
			// only `"x-secret"` requires the closing quote immediately after
			// the name, so `json:"x-secret,omitempty"` slips through -- and
			// on a bool field `,omitempty` is the reflexive Go spelling, so
			// that is the MORE likely form of the reader this arm exists to
			// catch, not a contrived one. Missing it would have reproduced
			// the defect that parked this PR, one grammar level down.
			//
			// The trailing quote/comma is what keeps it precise:
			// `json:"x-secret-other"` matches neither and must not fire.
			if !strings.Contains(field.Tag.Value, quoted) &&
				!strings.Contains(field.Tag.Value, `"`+key+`,`) {
				return true
			}
			hits = append(hits, keyHit{
				path: filepath.ToSlash(rel),
				line: fset.Position(field.Tag.Pos()).Line,
				// A tag is a decode instruction: it exists to READ the key
				// out of a document. It is never the emit.
				isWriteIndex: false,
			})
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if parsed == 0 {
		t.Fatal("no non-test Go files parsed, so this scan measures nothing")
	}
	return hits
}

// TestSecretEnforcementIsRealAndScoped replaces x-secret's row in the
// read-by-nothing gate above (memql#3036). It asserts the two halves that make
// the documentation true, and it fails from BOTH directions:
//
//   - x-secret must still have a real reader. If SecretFields() is deleted or
//     refactored away, the annotation silently returns to declared metadata
//     while three documents go on claiming it redacts -- the exact
//     over-promise this issue exists to remove.
//   - the documentation must still name EVERY surface and get its state
//     right. Enforcement is only honest if a reader can neither infer blanket
//     redaction from the word "enforced" (query results and length are still
//     uncovered) nor be told a surface leaks when it no longer does. Both
//     directions are checked: a missing surface fails, and so does a stale
//     "not redacted by ..." for one that now redacts.
func TestSecretEnforcementIsRealAndScoped(t *testing.T) {
	root := repoRoot(t)

	t.Run("has a reader", func(t *testing.T) {
		const readerFile = "component/database/memory-nodes/concept.go"
		hits := keyLiteralsInNonTestGo(t, root, "x-secret")

		var reader *keyHit
		for i := range hits {
			if !hits[i].isWriteIndex && strings.HasPrefix(hits[i].path, readerFile) {
				reader = &hits[i]
				break
			}
		}
		if reader == nil {
			t.Fatalf("x-secret has no reader in %s, but Concept.SecretFields must read it there "+
				"via `json:\"x-secret\"`.\n  found: %s\n\n"+
				"If the redaction was deliberately reverted, @secret is declared metadata again "+
				"and THREE documents now over-promise. Put x-secret back in "+
				"TestDeclaredMetadataKeysAreReadByNothing and correct:\n"+
				"  - dsl/_reference/_concept.memql section 8\n"+
				"  - docs/public/language/attribute-matrix.md\n"+
				"  - docs/public/language/reserved.md",
				readerFile, joinHits(hits))
		}
	})

	// Each document must name EVERY unenforced surface. A document that says
	// "enforced" without them lets a reader assume a credential is safe
	// everywhere.
	//
	// The phrases are DISTINCTIVE on purpose. The first version of this gate
	// searched the whole lowercased file for "query result" and "log", and both
	// are satisfied by text that has nothing to do with @secret: "log" is a
	// substring of "logic"/"logical", which all three documents use for
	// unrelated reasons, and attribute-matrix.md documents @cacheSeconds as
	// "Cache query results". Measured: with that gate, deleting the entire
	// @secret caveat from _concept.memql and attribute-matrix.md left it GREEN,
	// and it could not even distinguish these documents from their pre-memql#3036
	// text, which asserted the OPPOSITE ("NOTHING IS REDACTED"). Only
	// reserved.md failed, which is why the original verification -- done on that
	// one file -- read as proof for all three.
	//
	// So each phrase below must be one a reader could only write while
	// describing @secret's scope.
	for _, doc := range []string{
		"dsl/_reference/_concept.memql",
		"docs/public/language/attribute-matrix.md",
		"docs/public/language/reserved.md",
	} {
		t.Run(doc, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(doc)))
			if err != nil {
				t.Fatalf("read %s: %v", doc, err)
			}
			text := strings.ToLower(string(raw))
			if !strings.Contains(text, "@secret") && !strings.Contains(text, "x-secret") {
				t.Fatalf("%s does not mention @secret at all, so it cannot describe its scope", doc)
			}
			// surface -> the alternative spellings that count as naming it,
			// plus (for surfaces that are now ENFORCED) the stale spellings
			// that would mean the document still calls them uncovered.
			//
			// The staleGoneAfterEnforcement half is the half that bites now.
			// Before memql#3182-#3184 these documents had to NAME each
			// uncovered surface so a reader could not infer blanket coverage.
			// Now that four of them redact, the failure mode has INVERTED: a
			// document that still says "not redacted by the tool-args
			// validator" under-promises, and an author reading it will write
			// a workaround for a leak that no longer exists -- or, worse,
			// conclude the annotation is not worth using. Naming alone can no
			// longer distinguish the two states, so the state is asserted.
			for _, surface := range []struct {
				name  string
				anyOf []string
				// staleGoneAfterEnforcement: phrases that must NOT appear,
				// because they assert this surface is still uncovered. Empty
				// for surfaces that genuinely remain uncovered.
				staleGoneAfterEnforcement []string
			}{
				{
					name:  "query results",
					anyOf: []string{"redacted from **query results**", "not redacted from query results", "from **query results**"},
					// Still genuinely uncovered (memql#2803) -- nothing stale.
				},
				{
					name:                      "the automation args binder",
					anyOf:                     []string{"automation args binder", "args_binding.go"},
					staleGoneAfterEnforcement: []string{"not redacted by the automation", "not** redacted by the **automation"},
				},
				{
					name:                      "concept payload validation",
					anyOf:                     []string{"concept payload validation", "by concept payload validation"},
					staleGoneAfterEnforcement: []string{"not redacted by concept payload validation", "not** redacted by **concept payload validation"},
				},
				{
					name:                      "the tool-args validator",
					anyOf:                     []string{"tool-args validator", "validatetoolargs", "tool_execution.go"},
					staleGoneAfterEnforcement: []string{"not redacted by the tool-args validator", "not** redacted by the **tool-args validator"},
				},
				{
					name:  "the by-name matching rule",
					anyOf: []string{"by argument name", "by arg name", "matching is by argument name", "matches by argument name", "name, not by write target", "not by write target"},
				},
				{
					name:  "the length disclosure",
					anyOf: []string{"length is never redacted", "value too long", "rune count for a secret"},
				},
				{
					// Found by the exhaustive enumeration memql#3182's DoD
					// required, and absent from the epic's own four-surface
					// model -- so the documents must name it too, or the next
					// reader re-derives it.
					name:  "the validate/preflight builtins",
					anyOf: []string{"preflight", "executor_builtin.go"},
				},
			} {
				for _, stale := range surface.staleGoneAfterEnforcement {
					if strings.Contains(text, stale) {
						t.Errorf("%s still describes %s as NOT redacted (%q), but it is "+
							"now enforced.\n\nAn under-promising scope document is a real "+
							"failure, not a harmless one: an author reading it writes a "+
							"workaround for a leak that no longer exists, or concludes "+
							"@secret is not worth using. Update the paragraph.",
							doc, surface.name, stale)
					}
				}
				found := false
				for _, phrase := range surface.anyOf {
					if strings.Contains(text, phrase) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("%s must name %s when describing @secret's scope.\n\n"+
						"@secret now redacts on every validation surface that quotes a "+
						"rejected value -- the function-args validator (memql#3036), the "+
						"tool-args validator (#3182), the automation args binder (#3183), "+
						"concept payload validation (#3184), and the validate/preflight "+
						"builtins -- while query results and length remain uncovered, and "+
						"the two args-validator surfaces match by argument NAME rather than "+
						"by write target.\n\n"+
						"Every one of those has to be NAMED, covered or not. The list comes "+
						"from an exhaustive enumeration of validators, never from appending "+
						"one entry at a time: three successive incremental passes each "+
						"shipped a paragraph that walked past a surface the next pass found, "+
						"and the exhaustive sweep turned up two the epic's own four-surface "+
						"model did not contain. A document that says @secret is enforced "+
						"without naming what it does NOT cover recreates the over-promise "+
						"memql#2960 corrected -- one rung higher, and harder to spot now "+
						"that it is mostly true.\n\n"+
						"Accepted spellings: %v", doc, surface.name, surface.anyOf)
				}
			}
		})
	}
}
