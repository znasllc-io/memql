package memql

import (
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// platform_secret_annotation_test.go -- memql#3113.
//
// memql#3036 shipped @secret enforcement; memql#3113 observed that NOTHING in
// the DSL tree carried the annotation, so the enforcement protected zero
// fields. This file gates the annotation that fixes that, and it drives the
// REAL tree rather than a fixture: a synthetic concept proves the mechanism
// (function_secret_loader_test.go already does) but says nothing about whether
// the shipped DSL actually uses it. That gap is the whole issue.
//
// The chain under test, end to end:
//
//	dsl/platform/concepts.memql  @secret
//	  -> concept parser           x-secret in the definition schema
//	  -> Concept.SecretFields()   ["encryptedValue"]
//	  -> markSecretArgsFields     FunctionArgsField.Secret on the bound mutation
//
// Every link is the production one. Nothing here is constructed by the test
// except the registries the loaders fill.

// loadRealTreeFunctions loads the embedded concepts and functions the way
// bootstrap does, and returns the function registry plus the concept registry
// they were bound against.
func loadRealTreeFunctions(t *testing.T) (*FunctionRegistry, memoryNodes.Registry) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	if _, err := LoadUnifiedConcepts(logger); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
	concepts := memoryNodes.DefaultRegistry()

	functions, err := loadEmbeddedFunctions(logger, concepts)
	if err != nil {
		t.Fatalf("loadEmbeddedFunctions: %v", err)
	}
	if _, _, err := LoadUnifiedFunctions(logger, functions, concepts); err != nil {
		t.Fatalf("LoadUnifiedFunctions: %v", err)
	}
	return functions, concepts
}

// argsFieldOf returns a named args field of a named loaded function.
func argsFieldOf(t *testing.T, registry *FunctionRegistry, fnName, field string) *FunctionArgsField {
	t.Helper()
	fn, err := registry.Get(fnName)
	if err != nil || fn == nil {
		t.Fatalf("function %q not found in the loaded tree: %v", fnName, err)
	}
	if fn.ArgsSchema == nil {
		t.Fatalf("function %q has no args schema", fnName)
	}
	for _, f := range fn.ArgsSchema.Fields {
		if f != nil && f.Name == field {
			return f
		}
	}
	t.Fatalf("function %q has no args field %q", fnName, field)
	return nil
}

// secretBearingMutations pairs each platform secret concept with the mutation
// that writes it. Both halves matter: the annotation lives on the concept, but
// the stamp lands on the mutation's args field, and only because the names
// coincide (matching is BY ARGUMENT NAME -- see Concept.SecretFields).
var secretBearingMutations = []struct {
	concept  string
	mutation string
}{
	{"v1:platform:globalSecret", "setGlobalSecret"},
	{"v1:platform:partitionSecret", "setPartitionSecret"},
}

// The annotation reaches the concept registry as x-secret, on the real tree.
func TestPlatformSecretConceptsDeclareEncryptedValueSecret(t *testing.T) {
	_, concepts := loadRealTreeFunctions(t)

	for _, tc := range secretBearingMutations {
		concept, err := concepts.Get(tc.concept)
		if err != nil {
			t.Fatalf("concept %q: %v", tc.concept, err)
		}
		fields := concept.SecretFields()

		var hasEncrypted, hasFingerprint bool
		for _, f := range fields {
			switch f {
			case "encryptedValue":
				hasEncrypted = true
			case "fingerprint":
				hasFingerprint = true
			}
		}
		if !hasEncrypted {
			t.Errorf("%s: SecretFields()=%v, want encryptedValue.\n"+
				"The @secret annotation on dsl/platform/concepts.memql is missing or "+
				"stopped reaching the definition schema (memql#3113).", tc.concept, fields)
		}
		if hasFingerprint {
			t.Errorf("%s: fingerprint is marked @secret, and it must not be.\n"+
				"fingerprint exists to be SHOWN -- it is the last 4 chars of the cleartext, "+
				"kept so operators can tell rotated secrets apart. Redacting it deletes the "+
				"field's only purpose (memql#3113).", tc.concept)
		}
	}
}

// The stamp lands on the real mutation's args field -- the link that makes the
// annotation do anything at all.
func TestPlatformSecretArgsAreStampedOnTheRealMutations(t *testing.T) {
	registry, _ := loadRealTreeFunctions(t)

	for _, tc := range secretBearingMutations {
		if got := argsFieldOf(t, registry, tc.mutation, "encryptedValue"); !got.Secret {
			t.Errorf("%s: args field encryptedValue has Secret=false, want true.\n"+
				"The concept declares it @secret, so markSecretArgsFields should have "+
				"stamped the bound mutation (memql#3113).", tc.mutation)
		}
	}
}

// Control. Without this, a loader that stamped every field would pass the two
// tests above and the suite would be asserting nothing.
func TestNonSecretPlatformArgsAreNotStamped(t *testing.T) {
	registry, _ := loadRealTreeFunctions(t)

	for _, tc := range secretBearingMutations {
		for _, field := range []string{"name", "fingerprint", "kind", "description"} {
			if got := argsFieldOf(t, registry, tc.mutation, field); got.Secret {
				t.Errorf("%s: args field %q has Secret=true, want false.\n"+
					"Only encryptedValue is @secret on these concepts; a stamp here means "+
					"markSecretArgsFields is over-matching (memql#3113).", tc.mutation, field)
			}
		}
	}
}

// The honest scope limit, asserted rather than written in a comment nobody
// reads.
//
// Every site that redacts a rejected value first needs a CONSTRAINT to reject
// against: @enum, @minimum/@maximum, @format("date-time"), @pattern. Both
// encryptedValue args are unconstrained `string!`, so no validation error can
// quote them and the stamp changes no message today. The annotation is correct
// and the wiring is live -- but the protection is LATENT, and a reader who
// assumes otherwise will believe a ciphertext is being kept out of diagnostics
// that were never going to carry it.
//
// If someone later adds a constraint to these args, this test fails and points
// at the redaction that has just become real. That is the intended outcome, not
// a regression: delete the arm for the field that gained the constraint.
func TestPlatformSecretRedactionIsLatentUntilAConstraintExists(t *testing.T) {
	registry, _ := loadRealTreeFunctions(t)

	for _, tc := range secretBearingMutations {
		f := argsFieldOf(t, registry, tc.mutation, "encryptedValue")
		switch {
		case len(f.Enum) > 0:
			t.Errorf("%s.encryptedValue gained @enum -- redaction is now live; update memql#3113's scope note", tc.mutation)
		case f.Minimum != nil || f.Maximum != nil:
			t.Errorf("%s.encryptedValue gained bounds -- redaction is now live; update memql#3113's scope note", tc.mutation)
		case f.Pattern != "":
			t.Errorf("%s.encryptedValue gained @pattern -- redaction is now live; update memql#3113's scope note", tc.mutation)
		case f.Format != "":
			t.Errorf("%s.encryptedValue gained @format -- redaction is now live; update memql#3113's scope note", tc.mutation)
		}
	}
}

// ---------------------------------------------------------------------------
// The corpus gate.
//
// Annotating three fields answers memql#3113 once. It does not stop the fourth
// credential-bearing field from landing unannotated next month, which is the
// same shape of failure the issue reported: enforcement that protects nothing
// because nobody wired the declaration to it.
//
// So: a concept field whose OWN description says the value must not be shown to
// humans has to carry @secret. The description is the author stating the
// requirement; the annotation is the machine-readable form of the same
// sentence. Letting them disagree is how the tree ended up with a
// "never rendered to humans" field that nothing treated as secret.
//
// LIMIT, stated plainly: this matches declared prose, so a credential field
// whose description simply does not say any of this escapes the gate. It is a
// floor, not a proof. Widening it by field NAME was tried and rejected --
// safety.IsSecretKey matches any name containing "TOKEN", which on this tree
// hits v1:planner:plan.tokenBudget and tokenSpent, and a gate that cries wolf
// on integers gets suppressed rather than fixed.
// ---------------------------------------------------------------------------

// mustNotBeShownPhrases are the declarations that oblige a field to be @secret.
// Deliberately narrow: "never rendered in participant-facing UI" (on
// v1:cognition:participant) is a display rule for a boolean flag, not a
// credential, and must NOT match.
var mustNotBeShownPhrases = []string{
	"never rendered to humans",
	"never logged",
	"never surfaced in audit",
}

func declaresMustNotBeShown(description string) bool {
	lower := strings.ToLower(description)
	for _, phrase := range mustNotBeShownPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

// conceptProperty is the slice of a definition schema this gate reads.
type conceptProperty struct {
	Description string `json:"description"`
	Secret      bool   `json:"x-secret"`
}

func definitionProperties(t *testing.T, c *memoryNodes.Concept) map[string]conceptProperty {
	t.Helper()
	raw, ok := c.Schemas["definition"]
	if !ok || len(raw) == 0 {
		return nil
	}
	var schema struct {
		Properties map[string]conceptProperty `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("concept %q: unmarshal definition schema: %v", c.Name, err)
	}
	return schema.Properties
}

func TestFieldsDeclaredUnshowableCarrySecret(t *testing.T) {
	_, concepts := loadRealTreeFunctions(t)

	var scannedConcepts, scannedFields, matched int
	for _, concept := range concepts.List() {
		if concept == nil {
			continue
		}
		scannedConcepts++
		for name, prop := range definitionProperties(t, concept) {
			scannedFields++
			if !declaresMustNotBeShown(prop.Description) {
				continue
			}
			matched++
			if !prop.Secret {
				t.Errorf("%s.%s declares it must not be shown to humans but is not @secret.\n"+
					"  description: %s\n"+
					"Add @secret, or reword the description if the value is genuinely "+
					"displayable (memql#3113).", concept.Name, name, prop.Description)
			}
		}
	}

	// Vacuity guards. Without these the gate passes just as happily against a
	// broken schema read, an empty registry, or a phrase list that matches
	// nothing -- which is the exact failure mode memql#3113 reported one layer
	// up.
	if scannedConcepts < 50 {
		t.Fatalf("scanned only %d concepts; the registry did not load and this gate proved nothing", scannedConcepts)
	}
	if scannedFields < 500 {
		t.Fatalf("scanned only %d fields across %d concepts; definition schemas are not being read", scannedFields, scannedConcepts)
	}
	if matched < 3 {
		t.Fatalf("matched only %d fields, want at least the 3 known ones "+
			"(globalSecret.encryptedValue, partitionSecret.encryptedValue, authCode.code). "+
			"The phrase list or the schema read has broken.", matched)
	}
}

// Negative control for the matcher. An ordinary description must not oblige a
// field to be @secret, and the participant display rule specifically must not:
// it is the nearest miss in the tree.
func TestUnshowableMatcherDoesNotOverMatch(t *testing.T) {
	shouldNotMatch := []string{
		"Human-readable description of the secret's purpose.",
		"When true, the participant is present in the space for routing purposes but is never rendered in participant-facing UI (PresencePanel, invite lists, etc).",
		"Last 4 chars of the cleartext, prefixed with '...'. For UI display only; lets operators tell rotated secrets apart without leaking the value.",
		"SHA-256 hex digest of the plaintext code. Primary lookup key in the token-exchange path.",
		"",
	}
	for _, description := range shouldNotMatch {
		if declaresMustNotBeShown(description) {
			t.Errorf("matcher fired on a description that carries no such declaration: %q", description)
		}
	}

	shouldMatch := []string{
		"Base64-encoded nonce || NaCl-secretbox ciphertext. Opaque -- never rendered to humans.",
		"Plaintext one-time auth code; never logged, never surfaced in audit detail.",
	}
	for _, description := range shouldMatch {
		if !declaresMustNotBeShown(description) {
			t.Errorf("matcher missed a declaration it must catch: %q", description)
		}
	}
}
