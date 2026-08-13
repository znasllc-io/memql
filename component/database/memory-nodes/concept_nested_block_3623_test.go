package memoryNodes

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/parser"
)

// concept_nested_block_3623_test.go -- memql#3623.
//
// Three defects in concept nested / @variant blocks, all silent. Each half of
// this file drives the REAL emitter (ParseConceptMemQL -> Concept.validate),
// never a hand-written copy of a schema, so a change to what the builder emits
// shows up here rather than drifting away from it.
//
//  1. `!` / `@required` inside a nested object block was never emitted, and
//     neither was `additionalProperties: false` -- so a nested block enforced
//     nothing at all while the identical declaration one level up (and inside a
//     @variant branch) enforced both. The `required` half is fixed here. The
//     closed-object half is NOT, and TestNestedBlock_UndeclaredKeysStillAccepted
//     is where that is recorded, measured, and left ready to flip.
//  2. An annotation written after a nested / @variant block's closing brace was
//     left unconsumed by parsePropertyDecl and re-read by parseConceptDecl as a
//     PREFIX attribute of the next property -- exactly inverting @secret /
//     @pii / @internal.
//  3. @variant's discriminator was metadata only: `oneOf` asks that the
//     credentials object match exactly one branch, and nothing tied the branch
//     it matched to the sibling field that names it.

// nestedRequiredConcept declares one required and one optional field inside a
// nested block, plus the same pair at the top level as the oracle. The top
// level has always enforced `mustHave`; the nested block is the defect.
func nestedRequiredConcept(t *testing.T) *Concept {
	t.Helper()
	c, err := ParseConceptMemQL([]byte(`
@description("A widget with a nested block.")
concept widget {
  topMustHave  string!  @description("Enforced before memql#3623 and after it.")
  outer {
    mustHave  string!  @description("The same annotation, one level down.")
    optional  string   @description("Not required.")
  }
}
`), "v1/test/widget")
	if err != nil {
		t.Fatalf("ParseConceptMemQL: %v", err)
	}
	return c
}

// Defect 1a: `!` inside a nested block must reach the emitted schema.
func TestNestedBlock_RequiredIsEnforced(t *testing.T) {
	c := nestedRequiredConcept(t)

	// The oracle: the identical annotation at the top level.
	if err := c.validate("definition", map[string]any{
		"outer": map[string]any{"mustHave": "x"},
	}); err == nil {
		t.Fatal("top-level `topMustHave string!` was not enforced -- the oracle this test " +
			"measures the nested case against is itself broken")
	}

	err := c.validate("definition", map[string]any{
		"topMustHave": "x",
		"outer":       map[string]any{"optional": "x"},
	})
	if err == nil {
		t.Fatal("`mustHave string!` inside a nested block was NOT enforced: a payload omitting " +
			"it validated. The annotation is accepted by the parser and then dropped by the " +
			"schema builder, so the author gets no error at load and no error at insert.")
	}
	if !strings.Contains(err.Error(), "mustHave") {
		t.Errorf("rejection does not name the missing nested field: %v", err)
	}

	// The negative control: a complete nested object still validates.
	if err := c.validate("definition", map[string]any{
		"topMustHave": "x",
		"outer":       map[string]any{"mustHave": "x", "optional": "y"},
	}); err != nil {
		t.Errorf("a complete nested object was rejected: %v", err)
	}

	// An ABSENT optional nested block stays absent-legal: `required` inside an
	// object applies only when that object is present.
	if err := c.validate("definition", map[string]any{"topMustHave": "x"}); err != nil {
		t.Errorf("omitting the whole optional nested block was rejected: %v", err)
	}
}

// Defect 1b, NOT FIXED, and this is the record of why.
//
// A nested block is still OPEN: an undeclared key inside one is accepted, while
// the identical key at the top level is refused. That is the memql#3623
// exposure verbatim -- a typo'd write to v1:identity:user.preferences lands
// beside the real field and the computer-use kill switch keeps its old value.
//
// Closing it is a one-line change to the emitter, and the tree does not survive
// it. Measured before deciding, not assumed:
//
//   - [FIXED, memql#3641] dsl/agents/plannerAgent.memql and trainerAgent.memql
//     seeded `capabilities.domains` and `capabilities.tools`, the pre-#158
//     surface skillIds replaced. Both wrote empty arrays and nothing but the
//     legacy-row migration consumes that shape, so the seeds were deleted.
//   - [FIXED, memql#3641] v1:cognition:utterance.source took seven undeclared
//     keys from live writers, one load-bearing: `transcriptOnly`, written by
//     sendRealtimeTranscriptUtterance and read back by
//     cognition_utterance_auth_validation.go and cognition_handler.go. All
//     seven are now declared, and
//     test/dslconformance/nested_block_writes_3641_test.go fails on the next
//     undeclared key written into either block.
//   - insertSystemActionUtterance merges an ARBITRARY caller-supplied map into
//     the utterance source. Its keys are all declared today, but the writer is
//     open by construction, so closing the block moves the next new key from a
//     silent store to a failed insert on a path whose callers swallow the error.
//   - assistant_skill_reconcile.go and integrations/agents/factory.go read an
//     agent's capabilities object out of the DB and write it back wholesale, so
//     every EXISTING row carrying a legacy key fails on write-back. A data
//     migration, not an edit.
//   - and it does not stop at this repo: product bundles mount their own nested
//     blocks at runtime through MEMQL_DSL_PATH, and mutations such as
//     addAgentToSpace / updateSessionDevices / updateParticipantPresence splat a
//     client-supplied `object` into one. Closing is a wire-contract change for
//     every bundle and every client.
//
// So this test asserts the CURRENT behaviour deliberately. When the writers
// above are fixed, emit `additionalProperties: false` next to the `required`
// this issue did ship and invert this test -- it is the flip's checklist, not a
// claim that open is correct.
//
// Note what the three remaining blockers have in common: each is a block a
// CALLER or a STORED ROW populates, and none of them is `preferences` -- the
// block memql#3623 named as the risk, whose writers are all in-repo. That is
// the argument for a per-concept flip landing before a tree-wide one.
func TestNestedBlock_UndeclaredKeysStillAccepted(t *testing.T) {
	c := nestedRequiredConcept(t)

	// The top level is closed, which is the asymmetry that makes the nested
	// case surprising rather than merely permissive.
	if err := c.validate("definition", map[string]any{
		"topMustHave": "x",
		"typoAtTop":   "x",
		"outer":       map[string]any{"mustHave": "x"},
	}); err == nil {
		t.Fatal("an undeclared TOP-LEVEL key was accepted -- the asymmetry this test " +
			"documents has changed and its reasoning no longer holds")
	}

	if err := c.validate("definition", map[string]any{
		"topMustHave": "x",
		"outer":       map[string]any{"mustHave": "x", "computerUseEnabld": true},
	}); err != nil {
		t.Fatalf("an undeclared key inside a nested block is now REFUSED: %v\n\n"+
			"If that was deliberate, this test is the checklist to work through first: the "+
			"perUser agent seeds, utterance.source's seven undeclared keys, the two "+
			"read-modify-write paths over existing rows, and the out-of-repo bundles and "+
			"clients that splat client-supplied objects into a nested block.", err)
	}
}

// The emitted shape, asserted separately from the behaviour so a change that
// alters the schema but not the outcome is still visible.
func TestNestedBlock_EmitsRequired(t *testing.T) {
	raw, err := nestedRequiredConcept(t).DefinitionSchema()
	if err != nil {
		t.Fatalf("DefinitionSchema: %v", err)
	}
	var doc struct {
		Properties map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	outer := doc.Properties["outer"]
	req, _ := outer["required"].([]any)
	if len(req) != 1 || req[0] != "mustHave" {
		t.Errorf("nested block emits required=%v, want [mustHave]", outer["required"])
	}
}

// Defect 2: an annotation on the same line as the closing brace of a nested or
// @variant block is REFUSED rather than silently re-bound to the next property.
//
// Refusal rather than consumption, because the lexer strips newlines: `} @x`
// and `}` then `@x` on the next line are the same token stream apart from line
// numbers, and the second spelling ALREADY means "prefix attribute of the
// following property". Consuming the annotation onto the block would have made
// a reflow -- a formatter moving `@internal` off the closing-brace line, or
// onto it -- silently change which property is hidden. There is no such thing
// as a safe silent answer here, so the ambiguous spelling gets an error naming
// both intents.
func TestNestedBlock_TrailingAnnotationIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, prop, src string }{
		{
			name: "after a nested block",
			prop: "outer",
			src: `
@description("A widget.")
concept widget {
  outer { a string } @internal
  publicField string
}
`,
		},
		{
			name: "after a nested block, closing brace on its own line",
			prop: "outer",
			src: `
@description("A widget.")
concept widget {
  outer {
    a string
  } @internal
  publicField string
}
`,
		},
		{
			name: "after a @variant block",
			prop: "body",
			src: `
@description("A widget.")
concept widget {
  kind  enum("a", "b")!
  body  object!  @variant(discriminator="kind") {
    a { x string! }
    b { y string! }
  } @internal
  publicField string
}
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConceptMemQL([]byte(tc.src), "v1/test/widget")
			if err == nil {
				t.Fatal("the trailing annotation was accepted. Before memql#3623 it was " +
					"re-read as a PREFIX attribute of `publicField`, so the field the author " +
					"wrote it on stayed exposed and the next one was hidden -- exactly " +
					"inverted, and silent.")
			}
			// The diagnostic has to name the annotation, the property it was
			// written against, and the placement rule -- an author who hits
			// this needs to know which of the two intents to spell out.
			for _, want := range []string{"@internal", tc.prop, "line"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("diagnostic does not mention %q: %v", want, err)
				}
			}
		})
	}
}

// The negative control the refusal must not swallow: an annotation on a LATER
// line is the prefix form, it binds to the property that follows, and the tree
// depends on that reading everywhere a nested block is not involved.
func TestNestedBlock_PrefixAnnotationOnALaterLineStillBindsForward(t *testing.T) {
	c, err := ParseConceptMemQL([]byte(`
@description("A widget.")
concept widget {
  outer {
    a string
  }
  @internal
  publicField string
}
`), "v1/test/widget")
	if err != nil {
		t.Fatalf("the prefix form after a nested block must keep parsing: %v", err)
	}
	raw, err := c.DefinitionSchema()
	if err != nil {
		t.Fatalf("DefinitionSchema: %v", err)
	}
	var doc struct {
		Properties map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc.Properties["publicField"]["x-internal"] != true {
		t.Errorf("@internal on its own line no longer binds to the following property: %v",
			doc.Properties["publicField"])
	}
	if _, ok := doc.Properties["outer"]["x-internal"]; ok {
		t.Errorf("@internal on its own line was consumed by the preceding nested block: %v",
			doc.Properties["outer"])
	}
}

// realIdentityConcept builds v1:identity:identity from the tree, because
// defect 3 is about THE authentication credential schema and a synthetic
// stand-in would not prove the ten shipped variants survive the change.
func realIdentityConcept(t *testing.T) *Concept {
	t.Helper()
	path := filepath.Join(repoRoot(t), "dsl", "identity", "concepts.memql")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	file, err := parser.ParseFile(string(src))
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, def := range file.Definitions {
		decl, ok := def.(*parser.ConceptDecl)
		if !ok || decl.Name != "identity" {
			continue
		}
		c, err := BuildConceptFromDecl(decl, "v1:identity:identity")
		if err != nil {
			t.Fatalf("build v1:identity:identity: %v", err)
		}
		return c
	}
	t.Fatal("dsl/identity/concepts.memql declares no `identity` concept")
	return nil
}

// variantBranches reads the emitted oneOf back out, so the payloads below are
// derived from the shipped declaration rather than copied from it. A branch
// added to dsl/identity/concepts.memql is covered the day it lands.
func variantBranches(t *testing.T, c *Concept) map[string]map[string]any {
	t.Helper()
	raw, err := c.DefinitionSchema()
	if err != nil {
		t.Fatalf("DefinitionSchema: %v", err)
	}
	var doc struct {
		Properties map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	oneOf, ok := doc.Properties["credentials"]["oneOf"].([]any)
	if !ok || len(oneOf) == 0 {
		t.Fatalf("credentials carries no oneOf; got %v", doc.Properties["credentials"])
	}
	out := make(map[string]map[string]any, len(oneOf))
	for _, member := range oneOf {
		branch, _ := member.(map[string]any)
		title, _ := branch["title"].(string)
		if title == "" {
			t.Fatalf("a oneOf member carries no title naming its variant: %v", branch)
		}
		out[title] = branch
	}
	return out
}

// sampleFor synthesizes a conforming value for a leaf property schema.
func sampleFor(t *testing.T, name string, prop map[string]any) any {
	t.Helper()
	if _, ok := prop["oneOf"]; ok {
		// An optional datetime lowers to the sentinel union (memql#1629).
		return "2026-08-13T12:00:00Z"
	}
	switch prop["type"] {
	case "string":
		if prop["format"] == "date-time" {
			return "2026-08-13T12:00:00Z"
		}
		if values, ok := prop["enum"].([]any); ok && len(values) > 0 {
			return values[0]
		}
		return "x"
	case "boolean":
		return true
	case "integer":
		return 1
	case "number":
		return 1.5
	case "array":
		return []any{"x"}
	case "object":
		return map[string]any{}
	default:
		t.Fatalf("no sample value for property %q of shape %v", name, prop)
		return nil
	}
}

// branchPayload builds a full credentials object for one variant: every
// declared field, not only the required ones, so the closed branch schema is
// exercised as well as its required list.
func branchPayload(t *testing.T, branch map[string]any) map[string]any {
	t.Helper()
	props, _ := branch["properties"].(map[string]any)
	if len(props) == 0 {
		t.Fatalf("variant branch %v declares no properties", branch["title"])
	}
	out := make(map[string]any, len(props))
	for name, raw := range props {
		prop, _ := raw.(map[string]any)
		out[name] = sampleFor(t, name, prop)
	}
	return out
}

// Defect 3, first half: every shipped variant still validates against its own
// identityType. This is the authentication credential schema, so the change
// that ties the discriminator to its branch has to leave all ten alone.
func TestIdentityVariant_EveryShippedBranchStillValidates(t *testing.T) {
	c := realIdentityConcept(t)
	branches := variantBranches(t, c)
	if len(branches) < 10 {
		t.Fatalf("expected the ten shipped credential variants, found %d: %v",
			len(branches), branches)
	}
	for name, branch := range branches {
		t.Run(name, func(t *testing.T) {
			if err := c.validate("definition", map[string]any{
				"userId":       "user-1",
				"identityType": name,
				"credentials":  branchPayload(t, branch),
			}); err != nil {
				t.Errorf("a conforming %s credential was REJECTED: %v", name, err)
			}
		})
	}
}

// Defect 3, second half: the discriminator and the credential material can no
// longer disagree. Both pairs below were ACCEPTED before the fix -- a Go reader
// that branches on identityType and then type-asserts into credentials was
// reading a shape the schema permitted.
func TestIdentityVariant_DiscriminatorAndCredentialsCannotDisagree(t *testing.T) {
	c := realIdentityConcept(t)
	branches := variantBranches(t, c)

	for _, tc := range []struct{ declared, material string }{
		{declared: "passkey", material: "api_key"},
		{declared: "account_token", material: "service_account"},
		{declared: "magic_link", material: "api_key"},
		{declared: "node_token", material: "api_key"},
	} {
		t.Run(tc.declared+"/"+tc.material, func(t *testing.T) {
			branch, ok := branches[tc.material]
			if !ok {
				t.Fatalf("variant %q is gone from the tree; update this table", tc.material)
			}
			err := c.validate("definition", map[string]any{
				"userId":       "user-1",
				"identityType": tc.declared,
				"credentials":  branchPayload(t, branch),
			})
			if err == nil {
				t.Fatalf("identityType=%q carrying %q credential material was ACCEPTED. "+
					"x-discriminator is emitted and enforced by nothing, so the two halves "+
					"of the credential row are free to disagree.", tc.declared, tc.material)
			}
		})
	}
}

// A row whose discriminator names no branch at all is not the case the if/then
// pairs constrain, so the enum on identityType stays load-bearing. Pinned
// because it is the one hole a discriminator-only design would leave open.
func TestIdentityVariant_UnknownDiscriminatorStillRefused(t *testing.T) {
	c := realIdentityConcept(t)
	if err := c.validate("definition", map[string]any{
		"userId":       "user-1",
		"identityType": "not_a_credential_family",
		"credentials":  map[string]any{"keyHash": "h"},
	}); err == nil {
		t.Fatal("an identityType outside the declared enum was accepted")
	}
}
