package memql

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/santhosh-tekuri/jsonschema/v5"
	"log/slog"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// tool_execution_secret_redaction_test.go is memql#3182's proof, and it is
// deliberately built the way function_secret_loader_test.go is: nothing here
// hand-sets FunctionArgsField.Secret and nothing here hand-builds the Tool.
//
// The issue's point is that MemQLEngine.validateToolArgs is a FOURTH args
// validator compiled from the SAME ArgsSchema memql#3036 redacts, so the only
// test that means anything drives the real chain end to end:
//
//	DSL source + concept registry
//	  -> tryParseNewFunctionSyntax    (stamps Secret from the concept's @secret)
//	  -> registerFunctionTools        (compiles the tool twin's inputSchema)
//	  -> MemQLEngine.validateToolArgs (the message the model gets + the WARN)
//
// A fixture-built Tool would stay green with the whole chain broken.

// secretCodeConcept marks `code` @secret, the way the concept parser emits it.
func secretCodeConcept(t *testing.T, id string) *memoryNodes.Concept {
	t.Helper()
	c, err := memoryNodes.ParseConceptMemQL([]byte(`
concept authCode {
  code   int     @required  @secret
  label  int
}
`), "v1/identity/authCode")
	if err != nil {
		t.Fatalf("parse concept: %v", err)
	}
	c.Name = id
	return c
}

const secretCodeSource = `use identity.concepts.{ authCode }
@description("Store an auth code.")
mutate authCode storeAuthCode {
	args {
		code   int  @required
		label  int
	}
	insert {
		id: args.code
		code: args.code
		label: args.label
	}
}`

// secretToolFixture loads the mutation from DSL, auto-registers its function
// tool, and returns the tool plus an engine whose logger writes to the
// returned buffer.
//
// withNumericBounds stamps @minimum / @maximum onto the LOADED args fields
// (both of them, so the @secret annotation is the only difference between the
// two arguments). It is stamped rather than authored because the args-block
// parser does not accept @minimum yet (args_block_parser.go:239 -- @required /
// @enum / @maxLength / @pattern only), while
// function_tools.go:jsonSchemaForArgsFieldBase ALREADY carries Minimum /
// Maximum / Format into the generated tool schema. That asymmetry is the whole
// reason the returned-message half of memql#3182 is latent rather than live:
// the two jsonschema keywords that interpolate the instance value into their
// message ("must be >= %v but found %v", "%v is not valid %s") are exactly the
// ones the translation already carries and the authoring surface cannot yet
// reach. Everything else in the chain -- the Secret stamp, the schema compile,
// the validation, the message formatting -- is the real path.
func secretToolFixture(t *testing.T, withNumericBounds bool) (*MemQLEngine, *Tool, *bytes.Buffer) {
	t.Helper()

	const conceptID = "v1:identity:authCode"
	registry := newMemoryRegistry(map[string]*memoryNodes.Concept{
		conceptID: secretCodeConcept(t, conceptID),
	})

	fn, err := tryParseNewFunctionSyntax("storeAuthCode", "mutation", secretCodeSource, "dsl/identity/mutations.memql", registry)
	if err != nil {
		t.Fatalf("load mutation: %v", err)
	}
	// Guard the premise: if the loader stopped stamping Secret, every
	// assertion below would still pass, for the wrong reason.
	if !fieldByName(t, fn, "code").Secret {
		t.Fatal("premise broken: the loader did not stamp Secret on the @secret arg")
	}
	if fieldByName(t, fn, "label").Secret {
		t.Fatal("premise broken: a non-@secret arg was stamped Secret")
	}
	if withNumericBounds {
		min, max := 100000.0, 999999.0
		for _, name := range []string{"code", "label"} {
			field := fieldByName(t, fn, name)
			field.Minimum = &min
			field.Maximum = &max
		}
	}

	functions := newFunctionRegistry()
	if err := functions.add(fn); err != nil {
		t.Fatalf("register function: %v", err)
	}
	tools := newToolRegistry()
	registerFunctionTools(slog.Default(), functions, tools)

	tool, err := tools.Get("storeAuthCode")
	if err != nil || tool == nil {
		t.Fatalf("the auto-registered function tool is missing: %v", err)
	}
	if tool.Handler == nil || !strings.EqualFold(tool.Handler.Type, "function") {
		t.Fatalf("unexpected generated handler: %+v", tool.Handler)
	}

	buf := &bytes.Buffer{}
	engine := &MemQLEngine{
		Component: &component.Component{Logger: slog.New(slog.NewTextHandler(buf, nil))},
		functions: functions,
	}
	// The compiled-schema cache is keyed by tool NAME and lives for the
	// process, so without this a later test compiling a different schema under
	// the same name would read this one back.
	toolSchemaCache.Delete(tool.Name)
	t.Cleanup(func() { toolSchemaCache.Delete(tool.Name) })
	return engine, tool, buf
}

// The value of a @secret argument must not reach the model. ExecuteTool hands
// this message straight back as the tool-result text with IsError: true, which
// is what the streaming tool loop feeds the LLM.
func TestToolArgsValidation_RedactsSecretFromLLMFacingMessage(t *testing.T) {
	engine, tool, _ := secretToolFixture(t, true)

	// jsonschema interpolates the instance value with %v, so the assertion
	// below is written against %v of the SAME value rather than against a
	// hand-typed literal -- float64(4242424242) renders as "4.242424242e+09",
	// and a literal-digits assertion would have missed the leak entirely.
	secretCode := float64(4242)
	err := engine.validateToolArgs(tool, map[string]any{
		"code":  secretCode,
		"label": float64(123456), // in range -- the failure is on code
	})
	if err == nil {
		t.Fatal("expected the out-of-range secret argument to be rejected")
	}
	if strings.Contains(err.Error(), fmt.Sprintf("%v", secretCode)) {
		t.Fatalf("the @secret argument's value leaked into the message the LLM receives:\n  %s", err.Error())
	}
	if !strings.Contains(err.Error(), redactedArgValue) {
		t.Fatalf("expected the redaction placeholder %q in the message, got:\n  %s", redactedArgValue, err.Error())
	}
	// The declared constraint comes from the schema, never from caller data,
	// so it survives -- that is what keeps the message actionable enough for
	// the model to retry with a corrected call.
	if !strings.Contains(err.Error(), "100000") {
		t.Errorf("the declared constraint must survive redaction, got:\n  %s", err.Error())
	}
}

// Control, and the reason the redaction is targeted rather than blanket: the
// SAME declaration on a non-@secret argument still reports its value. The two
// args differ only in the concept's @secret annotation.
func TestToolArgsValidation_NonSecretValueStillReported(t *testing.T) {
	engine, tool, buf := secretToolFixture(t, true)

	err := engine.validateToolArgs(tool, map[string]any{
		"code":  float64(123456), // in range -- the failure is on label
		"label": float64(7),
	})
	if err == nil {
		t.Fatal("expected the out-of-range non-secret argument to be rejected")
	}
	if !strings.Contains(err.Error(), "but found 7") {
		t.Errorf("a non-secret value must still appear in the message, got:\n  %s", err.Error())
	}
	// (the slog text handler escapes the JSON attribute's quotes)
	if !strings.Contains(buf.String(), `label\":7`) {
		t.Errorf("a non-secret value must still appear in the WARN, got:\n  %s", buf.String())
	}
	// ... and the secret argument is still redacted from the log even though
	// it was not the argument that failed. The WARN serialized the WHOLE args
	// map, so a value that PASSED validation was logged too.
	if strings.Contains(buf.String(), "123456") {
		t.Errorf("the @secret argument's value leaked into the WARN log:\n  %s", buf.String())
	}
}

// The WARN is the live half of memql#3182 and needs no injected annotation:
// it dumps the entire args map on ANY validation failure, whatever the failing
// keyword and whatever the argument's type. Here the failure is a plain type
// mismatch on a different argument, authored entirely in DSL.
func TestToolArgsValidation_WarnRedactsSecretPerKey(t *testing.T) {
	engine, tool, buf := secretToolFixture(t, false)

	if err := engine.validateToolArgs(tool, map[string]any{
		"code":  float64(4242424242),
		"label": "not-an-integer",
	}); err == nil {
		t.Fatal("expected the type mismatch to be rejected")
	}

	logged := buf.String()
	if logged == "" {
		t.Fatal("expected a WARN to be logged")
	}
	if strings.Contains(logged, "4242424242") {
		t.Fatalf("the @secret argument's value leaked into the WARN log:\n  %s", logged)
	}
	if !strings.Contains(logged, redactedArgValue) {
		t.Fatalf("expected the WARN's args attribute to carry %q, got:\n  %s", redactedArgValue, logged)
	}
	// Per-key redaction, not a dropped attribute: the non-secret argument and
	// its value are still there, which is the reason the attribute is kept.
	if !strings.Contains(logged, "not-an-integer") {
		t.Errorf("the WARN must keep the non-secret args, got:\n  %s", logged)
	}
}

// A tool whose arguments cannot be classified -- no function handler, so no
// ArgsSchema to read Secret off -- logs the KEY LIST instead of the values. A
// log line is durable and ships to an aggregator; an args map nothing has
// vouched for does not go into one.
func TestToolArgsValidation_UnclassifiableToolLogsKeysOnly(t *testing.T) {
	buf := &bytes.Buffer{}
	engine := &MemQLEngine{
		Component: &component.Component{Logger: slog.New(slog.NewTextHandler(buf, nil))},
		functions: newFunctionRegistry(),
	}
	tool := &Tool{
		Name: "webhookToolWithNoFunctionBehindIt",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {"token": {"type": "number", "minimum": 100}},
			"required": ["token"]
		}`),
		Handler: &ToolHandler{Type: "webhook", URL: "https://example.com/hook"},
	}
	toolSchemaCache.Delete(tool.Name)
	t.Cleanup(func() { toolSchemaCache.Delete(tool.Name) })

	if err := engine.validateToolArgs(tool, map[string]any{"token": float64(7)}); err == nil {
		t.Fatal("expected a validation error")
	}
	logged := buf.String()
	if !strings.Contains(logged, "argKeys") {
		t.Errorf("an unclassifiable tool must log the key list, got:\n  %s", logged)
	}
	if strings.Contains(logged, "args=") {
		t.Errorf("an unclassifiable tool must not log the args map, got:\n  %s", logged)
	}
	if !strings.Contains(logged, "token") {
		t.Errorf("the key list must still name the supplied arguments, got:\n  %s", logged)
	}
}

// A tool with no @secret argument at all keeps its message and its log exactly
// as they were -- the common case, and the guard against the redaction
// spreading across every tool.
func TestToolArgsValidation_NoSecretArgsIsUnchanged(t *testing.T) {
	const conceptID = "v1:identity:plainCode"
	plain, err := memoryNodes.ParseConceptMemQL([]byte(`
concept plainCode {
  code  int  @required
}
`), "v1/identity/plainCode")
	if err != nil {
		t.Fatalf("parse concept: %v", err)
	}
	plain.Name = conceptID
	registry := newMemoryRegistry(map[string]*memoryNodes.Concept{conceptID: plain})

	src := `use identity.concepts.{ plainCode }
@description("Store a plain code.")
mutate plainCode storePlainCode {
	args {
		code  int  @required
	}
	insert {
		id: args.code
		code: args.code
	}
}`
	fn, err := tryParseNewFunctionSyntax("storePlainCode", "mutation", src, "dsl/identity/mutations.memql", registry)
	if err != nil {
		t.Fatalf("load mutation: %v", err)
	}
	min := 100000.0
	fieldByName(t, fn, "code").Minimum = &min

	functions := newFunctionRegistry()
	if err := functions.add(fn); err != nil {
		t.Fatalf("register function: %v", err)
	}
	tools := newToolRegistry()
	registerFunctionTools(slog.Default(), functions, tools)
	tool, err := tools.Get("storePlainCode")
	if err != nil {
		t.Fatalf("generated tool missing: %v", err)
	}
	toolSchemaCache.Delete(tool.Name)
	t.Cleanup(func() { toolSchemaCache.Delete(tool.Name) })

	buf := &bytes.Buffer{}
	engine := &MemQLEngine{
		Component: &component.Component{Logger: slog.New(slog.NewTextHandler(buf, nil))},
		functions: functions,
	}
	err = engine.validateToolArgs(tool, map[string]any{"code": float64(42)})
	if err == nil {
		t.Fatal("expected a validation error")
	}
	if !strings.Contains(err.Error(), "but found 42") {
		t.Errorf("a tool with no @secret args must report values as before, got:\n  %s", err.Error())
	}
	if strings.Contains(err.Error(), redactedArgValue) {
		t.Errorf("nothing should be redacted for a tool with no @secret args, got:\n  %s", err.Error())
	}
	if !strings.Contains(buf.String(), "args=") {
		t.Errorf("the WARN must keep its args attribute for a classified tool, got:\n  %s", buf.String())
	}
}

// TestJSONSchemaSingleQuotedMatchesLibrary pins jsonschemaSingleQuoted against
// a message the LIBRARY produced, not against a hand-written expectation.
//
// The needle it builds only works if it is byte-identical to what
// jsonschema/v5 printed. It previously was not: the code concatenated
// `"'" + s + "'"`, while the library %q-escapes the string and then escapes
// single quotes as \'. For a secret containing a quote, a backslash, or a
// newline the needle never matched and the value was NOT scrubbed -- a
// redaction gap wearing a CodeQL go/unsafe-quoting label.
//
// This drives a real `format` violation through the real compiler so the
// expectation comes from the library's own output.
func TestJSONSchemaSingleQuotedMatchesLibrary(t *testing.T) {
	// Each secret carries the distinctive marker SUPERSECRET. That is the
	// oracle: the library's ESCAPED rendering (`'has\'quote-SUPERSECRET'`)
	// still contains the marker verbatim, so asserting the marker is gone
	// catches a failed scrub no matter how the value was escaped. Asserting on
	// the raw secret would NOT -- `has'quote` is not a substring of
	// `has\'quote`, so a totally failed scrub would look clean.
	for _, secret := range []string{
		"plain-SUPERSECRET",
		`has'quote-SUPERSECRET`,
		`has\backslash-SUPERSECRET`,
		"has\nnewline-SUPERSECRET",
		`all'three\and` + "\n" + "-SUPERSECRET",
	} {
		t.Run(secret, func(t *testing.T) {
			compiler := jsonschema.NewCompiler()
			compiler.Draft = jsonschema.Draft2019
			compiler.AssertFormat = true
			if err := compiler.AddResource("pin://s", strings.NewReader(
				`{"type":"object","properties":{"f":{"type":"string","format":"date-time"}}}`,
			)); err != nil {
				t.Fatalf("add resource: %v", err)
			}
			schema, err := compiler.Compile("pin://s")
			if err != nil {
				t.Fatalf("compile: %v", err)
			}

			err = schema.Validate(map[string]any{"f": secret})
			if err == nil {
				t.Fatal("expected a format violation")
			}
			libraryMessage := err.Error()

			needle := jsonschemaSingleQuoted(secret)
			if !strings.Contains(libraryMessage, needle) {
				t.Fatalf("the needle does not match what the library printed, so a "+
					"secret containing these characters would NOT be scrubbed.\n"+
					"  needle:  %s\n  library: %s", needle, libraryMessage)
			}

			// And the scrub built on it actually removes the value. Assert on
			// the MARKER, not the raw secret -- see the comment above.
			if !strings.Contains(libraryMessage, "SUPERSECRET") {
				t.Fatalf("premise broken: the library message does not carry the "+
					"marker at all, so this assertion proves nothing:\n  %s", libraryMessage)
			}
			redacted := redactSecretArgValues(libraryMessage,
				map[string]struct{}{"f": {}}, map[string]any{"f": secret})
			if strings.Contains(redacted, "SUPERSECRET") {
				t.Errorf("the secret survived redaction -- the needle did not match "+
					"what the library printed:\n  %s", redacted)
			}
		})
	}
}
