package memql

import (
	"encoding/json"
	"strings"
	"testing"
)

// memql#3117: validateToolArgs is a FOURTH args validator, compiled from the
// same ArgsSchema that carries the Secret flag memql#3036 added -- and it did
// no redaction at all.
//
// It is the worst of the uncovered surfaces on three counts:
//
//   - it runs FIRST on the agent path, before the validator #3036 redacts, so a
//     violation on a secret arg was rejected and logged before that redaction
//     was ever reached;
//   - it serialises the ENTIRE args map into a WARN log, not just the rejected
//     field, so arguments that passed validation were logged too;
//   - its message is returned with IsError, and the streaming tool loop hands
//     that text to the model.
//
// Deliberately NOT restated here: the claim that the generator registers a tool
// for "every enabled query and mutation". memql#3143 measured that as false in
// two ways (a name already taken by an authored tool is skipped, and
// ValidateTool requires a description). The defect below does not depend on how
// many functions get a generated twin, so this test does not rest on it.

// secretArgTool builds the tool a function with one @secret arg generates,
// through the REAL emitter -- so the test breaks if x-secret stops being
// carried onto the tool schema, which is the wiring the redaction depends on.
func secretArgTool(t *testing.T) *Tool {
	t.Helper()
	min := 100000.0
	// All OPTIONAL on purpose. With required fields the validator fails on
	// "missing properties" before it ever evaluates `minimum`, so the test would
	// measure requiredness rather than the value-interpolating keyword this
	// issue is about.
	args := &ArgsSchemaConfig{Fields: []*FunctionArgsField{
		{Name: "label", Type: "string", Optional: true},
		{Name: "secretPin", Type: "integer", Minimum: &min, Secret: true, Optional: true},
		{Name: "publicPin", Type: "integer", Minimum: &min, Optional: true},
	}}
	raw, err := toolInputSchemaFromArgs(args)
	if err != nil {
		t.Fatalf("toolInputSchemaFromArgs: %v", err)
	}
	return &Tool{Name: "probeTool", Description: "probe", InputSchema: raw}
}

func TestToolInputSchemaCarriesTheSecretMarking(t *testing.T) {
	tool := secretArgTool(t)

	var doc struct {
		Properties map[string]map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(tool.InputSchema, &doc); err != nil {
		t.Fatalf("unmarshal tool schema: %v", err)
	}
	if doc.Properties["secretPin"]["x-secret"] != true {
		t.Errorf("the generated tool schema does not carry x-secret for a @secret arg.\n"+
			"  got: %v\n\nThe flag stopping at the function is exactly memql#3117: the tool is "+
			"compiled from the same ArgsSchema, so a function whose args #3036 redacts had a tool "+
			"twin whose args were not.", doc.Properties["secretPin"])
	}
	if _, present := doc.Properties["publicPin"]["x-secret"]; present {
		t.Errorf("a non-secret arg gained x-secret: %v", doc.Properties["publicPin"])
	}
	// The marking must not carry a VALUE anywhere -- only the fact.
	if strings.Contains(string(tool.InputSchema), "redacted") {
		t.Errorf("the tool schema mentions redaction; it should carry the flag only:\n%s", tool.InputSchema)
	}
}

func TestValidateToolArgsRedactsASecretArgFromTheReturnedMessage(t *testing.T) {
	e := &MemQLEngine{}
	tool := secretArgTool(t)

	err := e.validateToolArgs(tool, map[string]any{"label": "x", "secretPin": float64(4242)})
	if err == nil {
		t.Fatal("a below-minimum value was accepted, so there is no message to redact")
	}
	msg := err.Error()

	if strings.Contains(msg, "4242") {
		t.Errorf("the rejected value of a @secret arg appears verbatim in the message returned to "+
			"the caller.\n  %s\n\nThis message is returned with IsError and handed to the model by "+
			"the streaming tool loop (memql#3117).", msg)
	}
	if !strings.Contains(msg, "redacted") {
		t.Errorf("the message does not say the value was withheld, so a reader cannot tell a "+
			"redaction from a message that never carried a value.\n  %s", msg)
	}
}

// The converse, and the assertion that fails if redaction leaks across args.
func TestValidateToolArgsLeavesNonSecretArgsAlone(t *testing.T) {
	e := &MemQLEngine{}
	tool := secretArgTool(t)

	err := e.validateToolArgs(tool, map[string]any{"label": "x", "publicPin": float64(4242)})
	if err == nil {
		t.Fatal("a below-minimum value was accepted on the non-secret arg")
	}
	if !strings.Contains(err.Error(), "4242") {
		t.Errorf("a NON-secret arg's value was redacted. Redaction must be scoped to args declared "+
			"@secret, or every tool-validation error becomes undebuggable.\n  %s", err.Error())
	}
}

// The WARN log is the count that made this the worst surface: it serialises the
// WHOLE args map, so a secret argument was logged even when a DIFFERENT
// argument was the one that failed validation.
func TestRedactSecretArgValuesScrubsTheWholeArgsMap(t *testing.T) {
	secret := map[string]bool{"secretPin": true}
	args := map[string]any{"label": "x", "secretPin": "sk-live-SUPERSECRET", "publicPin": 7}

	got := redactSecretArgValues(args, secret)

	blob, _ := json.Marshal(got)
	if strings.Contains(string(blob), "sk-live-SUPERSECRET") {
		t.Errorf("a secret argument's value survives into the WARN log payload:\n  %s", blob)
	}
	if !strings.Contains(string(blob), "label") || !strings.Contains(string(blob), "7") {
		t.Errorf("non-secret arguments were lost; the log exists to diagnose a shape mismatch and "+
			"still needs them:\n  %s", blob)
	}

	// The caller's map is the one ExecuteTool dispatches with. Redacting it in
	// place would destroy the real argument before dispatch -- a log-hygiene fix
	// turned into a functional bug.
	if args["secretPin"] != "sk-live-SUPERSECRET" {
		t.Errorf("the caller's args map was mutated: %v", args["secretPin"])
	}
}

func TestRedactSecretArgValuesIsANoOpWithoutSecrets(t *testing.T) {
	args := map[string]any{"a": 1}
	if got := redactSecretArgValues(args, nil); len(got) != 1 || got["a"] != 1 {
		t.Errorf("redaction altered args when nothing is secret: %v", got)
	}
}

func TestSecretToolArgFieldsReadsTheToolSchema(t *testing.T) {
	tool := secretArgTool(t)
	got := secretToolArgFields(tool)
	if !got["secretPin"] {
		t.Errorf("secretToolArgFields did not find the @secret arg: %v", got)
	}
	if got["publicPin"] || got["label"] {
		t.Errorf("secretToolArgFields marked a non-secret arg: %v", got)
	}
	if secretToolArgFields(nil) != nil {
		t.Error("a nil tool should yield no secret fields")
	}
	if secretToolArgFields(&Tool{Name: "empty"}) != nil {
		t.Error("a tool with no input schema should yield no secret fields")
	}
}
