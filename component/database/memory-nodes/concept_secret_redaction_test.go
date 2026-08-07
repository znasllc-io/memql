package memoryNodes

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// concept_secret_redaction_test.go covers memql#3184: the concept-payload
// JSON-schema validation surface.
//
// This is the surface where @minimum / @maximum / @format are ENFORCED --
// they are declared on the CONCEPT and checked nowhere else, so unlike the
// memql function-args validator (memql#3097) this path needs no automation and
// no matching argument name to be reached. Every one of these tests drives the
// real Concept.Create.

const secretRedactionFixture = `
@description("A credential record.")
concept Credential {
  secretPin    int     @required  @secret  @minimum(100000)  @maximum(999999)
  publicPin    int     @required            @minimum(100000)  @maximum(999999)
  secretToken  string  @secret
  publicToken  string
  rotatedAt    datetime
  secretStamp  datetime @secret
}
`

// redactionStore is the whole Store surface Concept.Create touches. Named
// distinctly so it cannot collide with the other fakes in this package.
type redactionStore struct{ written *MemoryNode }

func (s *redactionStore) InsertMemoryNode(_ context.Context, node *MemoryNode) error {
	s.written = node
	return nil
}

func (s *redactionStore) QueryMemoryNodes(_ context.Context, _ QueryParams) ([]MemoryNode, error) {
	return nil, nil
}

func secretRedactionConcept(t *testing.T) *Concept {
	t.Helper()
	c, err := ParseConceptMemQL([]byte(secretRedactionFixture), "v1/test/credential")
	if err != nil {
		t.Fatalf("ParseConceptMemQL failed: %v", err)
	}
	return c
}

// createErr drives the real Concept.Create and returns the error string.
func createErr(t *testing.T, c *Concept, payload map[string]any) string {
	t.Helper()
	_, err := c.Create(context.Background(), &redactionStore{}, CreateParams{
		Actor:   "tester",
		ID:      "probe1",
		Payload: payload,
	})
	if err == nil {
		t.Fatal("control broken: Create must have failed validation, so this measures nothing")
	}
	return err.Error()
}

// unredactedErr reproduces exactly what Create's message was before memql#3184
// by calling the same validator with no redaction and re-applying Create's
// wrapper. It is the reference for byte-identity.
func unredactedErr(t *testing.T, c *Concept, payload map[string]any) string {
	t.Helper()
	err := c.validate(definitionSchemaKey, StripReservedPayloadFields(clonePayload(payload)))
	if err == nil {
		t.Fatal("control broken: raw validation must have failed")
	}
	return "concept payload validation failed: " + err.Error()
}

// The headline test: a @secret numeric field violating @minimum, driven
// through Concept.Create.
//
// PROVEN TO BITE. Delete the c.redactSecretValidationError(...) call from
// Concept.Create (concept.go, the definitionSchemaKey validation site) and this
// fails with:
//
//	secret value 4242 leaked into Concept.Create's error:
//	concept payload validation failed: jsonschema: '/secretPin' does not
//	validate with .../properties/secretPin/minimum: must be >= 100000 but found 4242
func TestConceptCreate_SecretMinimumViolationIsRedacted(t *testing.T) {
	c := secretRedactionConcept(t)
	got := createErr(t, c, map[string]any{"secretPin": 4242, "publicPin": 123456})

	if strings.Contains(got, "4242") {
		t.Fatalf("secret value 4242 leaked into Concept.Create's error:\n%s", got)
	}
	if !strings.Contains(got, redactedSecretValue) {
		t.Fatalf("expected %q in the message, got:\n%s", redactedSecretValue, got)
	}
	// The constraint half must survive -- redaction removes the value, not the
	// operator's ability to see which rule was broken.
	if !strings.Contains(got, "must be >= 100000 but found "+redactedSecretValue) {
		t.Fatalf("constraint half of the message was lost:\n%s", got)
	}
	if !strings.Contains(got, "concept payload validation failed: ") {
		t.Fatalf("Create's wrapper was lost:\n%s", got)
	}
}

// @maximum is the same keyword family and must behave identically.
func TestConceptCreate_SecretMaximumViolationIsRedacted(t *testing.T) {
	c := secretRedactionConcept(t)
	got := createErr(t, c, map[string]any{"secretPin": 7777777, "publicPin": 123456})

	if strings.Contains(got, "7777777") {
		t.Fatalf("secret value leaked:\n%s", got)
	}
	if !strings.Contains(got, "must be <= 999999 but found "+redactedSecretValue) {
		t.Fatalf("maximum message not redacted as expected:\n%s", got)
	}
}

// The NESTED case, and the reason this file resolves the schema itself instead
// of calling Concept.SecretFields(): an optional datetime is emitted as a
// oneOf, so its format failure is a LEAF one level down from the root error.
// ValidationError.Error() renders that leaf. A redaction applied at the root
// would miss it entirely.
func TestConceptCreate_SecretFormatViolationOnNestedLeafIsRedacted(t *testing.T) {
	c := secretRedactionConcept(t)
	got := createErr(t, c, map[string]any{
		"secretPin":   123456,
		"publicPin":   123456,
		"secretStamp": "sk-live-SUPERSECRET",
	})

	if strings.Contains(got, "SUPERSECRET") {
		t.Fatalf("secret value leaked from a nested oneOf leaf:\n%s", got)
	}
	if !strings.Contains(got, redactedSecretValue+" is not valid ") {
		t.Fatalf("format message not redacted as expected:\n%s", got)
	}
}

// The other half of the acceptance criteria, and the one that constrains the
// implementation hardest: a NON-secret field's message must be byte-for-byte
// what it was before this change. The reference is the unredacted validator
// output plus Create's own wrapper.
func TestConceptCreate_NonSecretMessagesAreByteIdentical(t *testing.T) {
	c := secretRedactionConcept(t)

	for _, tc := range []struct {
		name    string
		payload map[string]any
	}{
		{"minimum", map[string]any{"secretPin": 123456, "publicPin": 4242}},
		{"maximum", map[string]any{"secretPin": 123456, "publicPin": 7777777}},
		{"format", map[string]any{"secretPin": 123456, "publicPin": 123456, "rotatedAt": "not-a-date"}},
		{"type", map[string]any{"secretPin": 123456, "publicPin": 123456, "publicToken": 17}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := unredactedErr(t, c, tc.payload)
			got := createErr(t, c, tc.payload)
			if got != want {
				t.Fatalf("non-secret message changed.\n got: %s\nwant: %s", got, want)
			}
		})
	}
}

// A concept with no @secret field must be untouched everywhere, including on
// the keywords redaction knows how to rewrite. Guards against a schema-walk
// slip that treats an unresolvable pointer as secret.
func TestConceptCreate_ConceptWithoutSecretsIsUntouched(t *testing.T) {
	c, err := ParseConceptMemQL([]byte(`
concept Plain {
  pin  int  @required  @minimum(100000)
}
`), "v1/test/plain")
	if err != nil {
		t.Fatalf("ParseConceptMemQL failed: %v", err)
	}
	payload := map[string]any{"pin": 4242}
	want := unredactedErr(t, c, payload)
	got := createErr(t, c, payload)
	if got != want {
		t.Fatalf("message changed on a concept with no @secret field.\n got: %s\nwant: %s", got, want)
	}
	if !strings.Contains(got, "4242") {
		t.Fatalf("control broken: the unredacted message should still quote the value:\n%s", got)
	}
}

// When one payload breaks a secret rule AND a non-secret rule at once, the
// tree carries several leaves. Every secret leaf must be rewritten and every
// non-secret leaf left alone -- rewriting only the first leaf that Error()
// happens to render would pass a single-violation test and leak here.
func TestConceptCreate_MixedViolationsRedactOnlyTheSecretLeaves(t *testing.T) {
	c := secretRedactionConcept(t)
	_, err := c.Create(context.Background(), &redactionStore{}, CreateParams{
		Actor: "tester", ID: "probe1",
		Payload: map[string]any{"secretPin": 4242, "publicPin": 1111},
	})
	if err == nil {
		t.Fatal("control broken: Create must have failed validation")
	}

	// Walk every message in the tree, not just the one Error() renders.
	messages := allMessages(t, err)
	joined := strings.Join(messages, "\n")
	if strings.Contains(joined, "4242") {
		t.Fatalf("secret value survived somewhere in the error tree:\n%s", joined)
	}
	if !strings.Contains(joined, "1111") {
		t.Fatalf("the NON-secret value must still be reported; the tree was over-redacted:\n%s", joined)
	}
}

// Nested support is a DELIBERATE, explicit decision (memql#3184): @secret is
// expressible at any depth because propertyToJSONSchema recurses, and x-secret
// is inherited by everything beneath it. Concept.SecretFields() is top-level
// only and is intentionally NOT the accessor used here.
func TestConceptCreate_NestedSecretIsSupportedAndInherited(t *testing.T) {
	c, err := ParseConceptMemQL([]byte(`
concept Vault {
  creds {
    pin       int  @secret  @minimum(100000)
    publicPin int            @minimum(100000)
  }
  pins []int @secret @minimum(100000)
}
`), "v1/test/vault")
	if err != nil {
		t.Fatalf("ParseConceptMemQL failed: %v", err)
	}

	// A @secret field ONE LEVEL DOWN. Concept.SecretFields() cannot see this
	// one at all -- redaction resolves the instance pointer through the schema
	// instead, which is the whole reason it does not use that accessor.
	got := createErr(t, c, map[string]any{"creds": map[string]any{"pin": 4242}})
	if strings.Contains(got, "4242") {
		t.Fatalf("a nested @secret field leaked its value:\n%s", got)
	}
	if !strings.Contains(got, redactedSecretValue) {
		t.Fatalf("expected %q in the nested message:\n%s", redactedSecretValue, got)
	}

	// Its non-secret sibling, at the same depth, is untouched.
	payload := map[string]any{"creds": map[string]any{"publicPin": 4242}}
	want := unredactedErr(t, c, payload)
	if got := createErr(t, c, payload); got != want {
		t.Fatalf("non-secret nested message changed.\n got: %s\nwant: %s", got, want)
	}

	// INHERITANCE is load-bearing, not a nicety: propertyDeclToParsed moves
	// @minimum down onto the array ELEMENT while @secret stays on the wrapper,
	// so the failing location is /pins/0 and the x-secret mark is one level
	// ABOVE it. Without downward inheritance this leaks.
	got = createErr(t, c, map[string]any{"pins": []any{4242}})
	if strings.Contains(got, "4242") {
		t.Fatalf("x-secret was not inherited onto the array element:\n%s", got)
	}

	// SecretFields() stays top-level-only; nested support here does not and
	// must not change it. `pins` is top-level and visible; `creds.pin` is not.
	fields := c.SecretFields()
	if len(fields) != 1 || fields[0] != "pins" {
		t.Fatalf("SecretFields() = %v, want exactly [pins] -- it is top-level only by design, "+
			"and creds.pin must NOT appear in it", fields)
	}
}

// The enumeration is derived from jsonschema v5's message-formatting code, not
// from the three keywords the issue happened to name. Pinned so a future edit
// that drops one has to argue with a test.
func TestValueInterpolatingKeywordsEnumeration(t *testing.T) {
	want := map[string]bool{
		"format": true, "minimum": true, "exclusiveMinimum": true,
		"maximum": true, "exclusiveMaximum": true, "multipleOf": true,
	}
	if len(valueInterpolatingKeywords) != len(want) {
		t.Fatalf("enumeration = %v, want the six derived from schema.go", valueInterpolatingKeywords)
	}
	for keyword := range want {
		if _, ok := valueInterpolatingKeywords[keyword]; !ok {
			t.Errorf("keyword %q dropped from the enumeration -- jsonschema/v5 schema.go interpolates the instance value for it", keyword)
		}
	}
	// Keywords deliberately excluded because their message carries a count, a
	// name or a schema-side value -- never the instance.
	for _, keyword := range []string{
		"minLength", "maxLength", "pattern", "enum", "const", "type",
		"required", "minItems", "maxItems", "uniqueItems", "additionalProperties",
	} {
		if _, ok := valueInterpolatingKeywords[keyword]; ok {
			t.Errorf("keyword %q must NOT be redacted: its message quotes no instance value, so rewriting it would change non-secret output", keyword)
		}
	}
}

// applyKeywordRedaction must fail CLOSED. If a library upgrade rewords a
// message so the expected separator is gone, the value must not survive by
// default.
func TestApplyKeywordRedaction_FailsClosed(t *testing.T) {
	got := applyKeywordRedaction("minimum", valueInterpolatingKeywords["minimum"], "reworded upstream: 4242 is too small")
	if strings.Contains(got, "4242") {
		t.Fatalf("redaction failed OPEN on an unrecognised message: %q", got)
	}
	if !strings.Contains(got, redactedSecretValue) {
		t.Fatalf("fallback message must still name the placeholder, got %q", got)
	}
}

// allMessages flattens every message in the validation-error tree, so a test
// can assert about leaves Error() never renders.
func allMessages(t *testing.T, err error) []string {
	t.Helper()
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected a *jsonschema.ValidationError, got %T: %v", err, err)
	}
	var walk func(*jsonschema.ValidationError)
	var out []string
	walk = func(node *jsonschema.ValidationError) {
		out = append(out, node.Message)
		for _, cause := range node.Causes {
			walk(cause)
		}
	}
	walk(ve)
	return out
}

func TestParseJSONPointer(t *testing.T) {
	for _, tc := range []struct {
		pointer string
		want    []string
	}{
		{"", nil},
		{"/secretPin", []string{"secretPin"}},
		{"/creds/apiKey", []string{"creds", "apiKey"}},
		{"/items/0/key", []string{"items", "0", "key"}},
		{"/a~1b/c~0d", []string{"a/b", "c~d"}},
	} {
		got := parseJSONPointer(tc.pointer)
		if len(got) != len(tc.want) {
			t.Fatalf("parseJSONPointer(%q) = %v, want %v", tc.pointer, got, tc.want)
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Fatalf("parseJSONPointer(%q) = %v, want %v", tc.pointer, got, tc.want)
			}
		}
	}
}
