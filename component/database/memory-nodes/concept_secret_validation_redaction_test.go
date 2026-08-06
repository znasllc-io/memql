package memoryNodes

import (
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/language/parser"
)

// memql#3112: concept payload validation interpolated the offending INSTANCE
// VALUE into its message, and Concept.Create wrapped that verbatim as
// "concept payload validation failed: %w".
//
// memql#3036 redacted the function-args validator, which covers only the
// constraints a mutation's ARGS BLOCK declares. @minimum / @maximum / @format /
// @minLength declared on the CONCEPT are enforced only here -- so any concept
// constraint the args block does not mirror bypassed that redaction entirely,
// with no automation and no matching arg name required. That made this the
// largest of the surfaces #3036 left open.
//
// The value is a credential by declaration: the field says @secret.

// secretConceptFixture builds a concept with one @secret field carrying a
// value-interpolating constraint, and one ordinary field carrying the same
// constraint, so both directions are measurable from one fixture.
func secretConceptFixture(t *testing.T) *Concept {
	t.Helper()
	src := "@version(\"1.0.0\")\n@namespace(\"aud\")\n@description(\"d\")\n" +
		"concept probe {\n" +
		"  label      string  @required @description(\"l\")\n" +
		"  secretPin  int     @secret @minimum(100000) @description(\"s\")\n" +
		"  publicPin  int     @minimum(100000) @description(\"p\")\n" +
		"}\n"
	return buildConceptWithId(t, src, "v1:aud:secretprobe")
}

func TestConceptPayloadValidationRedactsASecretField(t *testing.T) {
	c := secretConceptFixture(t)

	// Sanity: the fixture actually declares the field secret. Without this a
	// broken annotation would make every assertion below pass vacuously.
	secret := c.SecretFields()
	found := false
	for _, f := range secret {
		if f == "secretPin" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the fixture's @secret field did not reach SecretFields() (%v) -- this test would "+
			"pass for the wrong reason", secret)
	}

	const pin = 4242
	err := c.validate(definitionSchemaKey, map[string]any{"label": "x", "secretPin": pin})
	if err == nil {
		t.Fatal("a below-minimum value was accepted, so there is no message to redact")
	}
	msg := err.Error()

	if strings.Contains(msg, "4242") {
		t.Errorf("the rejected value of a @secret field appears verbatim in the validation error.\n"+
			"  %s\n\nConcept.Create wraps this as \"concept payload validation failed: %%w\", so the "+
			"value travels into the caller's error and any log that records it (memql#3112).", msg)
	}
	if !strings.Contains(msg, "redacted") {
		t.Errorf("the message does not say the value was withheld, so a reader cannot tell a "+
			"redaction from a message that never carried a value.\n  %s", msg)
	}
	// The constraint must still be identifiable, or the redaction has removed
	// the diagnostic rather than the secret.
	if !strings.Contains(msg, "minimum") {
		t.Errorf("the redacted message no longer identifies which constraint failed.\n  %s", msg)
	}
}

func TestConceptPayloadValidationLeavesNonSecretFieldsByteIdentical(t *testing.T) {
	c := secretConceptFixture(t)

	err := c.validate(definitionSchemaKey, map[string]any{"label": "x", "publicPin": 4242})
	if err == nil {
		t.Fatal("a below-minimum value was accepted on the non-secret field")
	}
	msg := err.Error()

	// The acceptance criterion is explicit: non-secret messages are unchanged.
	// The value is what proves it -- if redaction leaked across fields this is
	// the assertion that fails.
	if !strings.Contains(msg, "4242") {
		t.Errorf("a NON-secret field's value was redacted. Redaction must be scoped to fields "+
			"declared @secret, or every validation error becomes undebuggable.\n  %s", msg)
	}
	if strings.Contains(msg, "redacted") {
		t.Errorf("a non-secret field's message was rewritten.\n  %s", msg)
	}
}

// A concept declaring NO secret fields must be completely untouched -- the
// common case, and the one where a regression would be widest.
func TestConceptPayloadValidationUntouchedWhenNothingIsSecret(t *testing.T) {
	src := "@version(\"1.0.0\")\n@namespace(\"aud\")\n@description(\"d\")\n" +
		"concept plain {\n  label string @required @description(\"l\")\n" +
		"  pin int @minimum(100000) @description(\"p\")\n}\n"
	c := buildConceptWithId(t, src, "v1:aud:plainprobe")
	verr := c.validate(definitionSchemaKey, map[string]any{"label": "x", "pin": 7})
	if verr == nil {
		t.Fatal("a below-minimum value was accepted")
	}
	if !strings.Contains(verr.Error(), "7") {
		t.Errorf("a concept with no @secret field had its message rewritten.\n  %s", verr.Error())
	}
}

// topLevelInstanceField is the matcher the redaction turns on, and its edge
// cases decide whether a value leaks. Pinned directly.
func TestTopLevelInstanceField(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"/apiKey", "apiKey"},
		{"/apiKey/inner", "apiKey"},  // nested under a secret field is still that field
		{"/apiKey/0/deep", "apiKey"}, // array index under a secret field
		{"", ""},                     // the root instance names no field
		{"/", ""},                    // degenerate
		{"/a~1b", "a/b"},             // JSON-pointer escape for '/'
		{"/a~0b", "a~b"},             // JSON-pointer escape for '~'
		{"apiKey", "apiKey"},         // tolerate a missing leading slash
	} {
		if got := topLevelInstanceField(c.in); got != c.want {
			t.Errorf("topLevelInstanceField(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// buildConceptWithId parses a single-concept fixture and builds it under a
// CALLER-SUPPLIED id.
//
// The package's buildFixtureConcept hardcodes "v1:ref:probe", and compileSchema
// caches the compiled schema by that id -- so two fixtures built through it in
// one package run share a compiled schema, and the second silently validates
// against the first's properties. That is not hypothetical: the
// nothing-is-secret test below passed in isolation and failed in the suite for
// exactly this reason before it got its own id.
//
// It picks the decl out of File.Definitions rather than calling
// component/memql.ExtractConceptDecls, which would be an import cycle from here.
func buildConceptWithId(t *testing.T, src, conceptId string) *Concept {
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
		c, err := BuildConceptFromDecl(decl, conceptId)
		if err != nil {
			t.Fatalf("fixture does not build: %v", err)
		}
		return c
	}
	t.Fatal("fixture declared no concept, so it measures nothing")
	return nil
}
