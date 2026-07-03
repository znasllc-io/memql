package automations

// G1 (event-payload-binding ADR, memql#2363) tests: args-block parse/compile,
// the bind/validate core, evaluator visibility of `args`, and the fire-time
// refusal path (trigger + invoke-by-reference).

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/events"
)

// argsAutomationSource is a full-form automation declaring a typed args
// contract. It compiles through the real loader pipeline (rewriter -> parser
// -> compiler) so the schema round-trips onto Automation.Args.
const argsAutomationSource = `@enabled
@trigger(event="deploy.requested", concept="v1:cluster:deployment", partition="*")
@description("deploy with a typed args contract")
automation deployWithArgs {
  args {
    environment     string   @required @enum("development", "staging", "production")
    engineNodeTypes []string @required
    replicas        int
  }
  step gate {
    logic requireForwardDeploy { environment: args.environment }
  }
}`

// --- parse / compile: args block accepted, schema round-trips -------------

func TestCompileMemQL_AttachesArgsSchema(t *testing.T) {
	loader := NewLoader(LoaderOptions{})
	auto, err := loader.compileMemQL(argsAutomationSource, "test:deployWithArgs")
	if err != nil {
		t.Fatalf("compileMemQL: %v", err)
	}
	if auto.Args == nil {
		t.Fatalf("expected Automation.Args to be populated from the args block")
	}
	if len(auto.Args.Fields) != 3 {
		t.Fatalf("want 3 declared args fields, got %d", len(auto.Args.Fields))
	}
	byName := map[string]*ArgsField{}
	for _, f := range auto.Args.Fields {
		byName[f.Name] = f
	}

	env := byName["environment"]
	if env == nil {
		t.Fatalf("missing 'environment' field")
	}
	if env.Type != "string" || env.Optional {
		t.Errorf("environment = {type:%q optional:%v}, want {string false}", env.Type, env.Optional)
	}
	if len(env.Enum) != 3 {
		t.Errorf("environment enum = %v, want 3 values", env.Enum)
	}

	nt := byName["engineNodeTypes"]
	if nt == nil || nt.Type != "array" || nt.Optional {
		t.Errorf("engineNodeTypes = %+v, want {type:array required}", nt)
	}
	if nt != nil && (nt.Items == nil || nt.Items.Type != "string") {
		t.Errorf("engineNodeTypes items = %+v, want string element type", nt.Items)
	}

	rep := byName["replicas"]
	if rep == nil || rep.Type != "int" || !rep.Optional {
		t.Errorf("replicas = %+v, want {type:int optional}", rep)
	}
}

// @default on an args field is retired (#991); it must be rejected at compile
// so the contract stays honest (defaults belong in the body via coalesce).
func TestCompileMemQL_RejectsDefaultOnArgsField(t *testing.T) {
	loader := NewLoader(LoaderOptions{})
	src := `@trigger(event="deploy.requested", concept="v1:cluster:deployment", partition="*")
automation badDefault {
  args {
    environment string @default("staging")
  }
  step gate { logic noop { event: event } }
}`
	if _, err := loader.compileMemQL(src, "test:badDefault"); err == nil {
		t.Fatalf("expected compile to reject @default on an args field")
	} else if !strings.Contains(err.Error(), "default") {
		t.Errorf("error should mention @default, got: %v", err)
	}
}

// An automation with NO args block still compiles and carries a nil Args --
// the backward-compatible untyped-event path (zero regression).
func TestCompileMemQL_NoArgsBlock_NilArgs(t *testing.T) {
	loader := NewLoader(LoaderOptions{})
	src := `@trigger(event="node.created", concept="v1:cognition:space", partition="*")
automation legacyUntyped {
  step greet { publishEvent { topic: "demo.greeted" } }
}`
	auto, err := loader.compileMemQL(src, "test:legacyUntyped")
	if err != nil {
		t.Fatalf("compileMemQL: %v", err)
	}
	if auto.Args != nil {
		t.Fatalf("expected nil Args for an automation with no args block, got %+v", auto.Args)
	}
}

// --- bind / validate core -------------------------------------------------

func schema(fields ...*ArgsField) *ArgsSchema { return &ArgsSchema{Fields: fields} }

func req(name, typ string) *ArgsField { return &ArgsField{Name: name, Type: typ, Optional: false} }
func opt(name, typ string) *ArgsField { return &ArgsField{Name: name, Type: typ, Optional: true} }

func eventWith(payload map[string]any) *events.Event {
	return &events.Event{Topic: "deploy.requested", Kind: events.KindNodeCreated, Payload: payload}
}

func TestBindEventArgs_HappyPath(t *testing.T) {
	auto := &Automation{Name: "a", Args: schema(req("environment", "string"), opt("replicas", "int"))}
	bound, extras, err := bindEventArgs(auto, eventWith(map[string]any{"environment": "staging", "replicas": 2}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(extras) != 0 {
		t.Errorf("extras = %v, want none", extras)
	}
	if bound["environment"] != "staging" || bound["replicas"] != 2 {
		t.Errorf("bound = %v, want environment=staging replicas=2", bound)
	}
}

func TestBindEventArgs_TolerantExtras(t *testing.T) {
	auto := &Automation{Name: "a", Args: schema(req("environment", "string"))}
	bound, extras, err := bindEventArgs(auto, eventWith(map[string]any{
		"environment": "staging",
		"triggeredBy": "alice", // undeclared -> tolerated, not bound
		"note":        "x",
	}))
	if err != nil {
		t.Fatalf("unexpected error on extra fields (tolerant-reader): %v", err)
	}
	if _, bound2 := bound["triggeredBy"]; bound2 {
		t.Errorf("undeclared field 'triggeredBy' must NOT be bound; bound=%v", bound)
	}
	if len(extras) != 2 || extras[0] != "note" || extras[1] != "triggeredBy" {
		t.Errorf("extras = %v, want sorted [note triggeredBy]", extras)
	}
}

func TestBindEventArgs_MissingRequired(t *testing.T) {
	auto := &Automation{Name: "a", Args: schema(req("environment", "string"))}
	_, _, err := bindEventArgs(auto, eventWith(map[string]any{"replicas": 2}))
	if err == nil {
		t.Fatalf("expected a missing-required error")
	}
	abe, ok := err.(*argBindError)
	if !ok || abe.Field != "environment" {
		t.Errorf("error = %v, want *argBindError for field 'environment'", err)
	}
}

func TestBindEventArgs_WrongType(t *testing.T) {
	auto := &Automation{Name: "a", Args: schema(req("replicas", "int"))}
	_, _, err := bindEventArgs(auto, eventWith(map[string]any{"replicas": "two"}))
	if err == nil {
		t.Fatalf("expected a type-mismatch error")
	}
	if abe, ok := err.(*argBindError); !ok || abe.Field != "replicas" {
		t.Errorf("error = %v, want *argBindError for field 'replicas'", err)
	}
}

func TestBindEventArgs_EnumViolationAndMatch(t *testing.T) {
	field := &ArgsField{Name: "environment", Type: "string", Enum: []any{"development", "staging", "production"}}
	auto := &Automation{Name: "a", Args: schema(field)}

	if _, _, err := bindEventArgs(auto, eventWith(map[string]any{"environment": "qa"})); err == nil {
		t.Fatalf("expected an enum-violation error for 'qa'")
	}
	if _, _, err := bindEventArgs(auto, eventWith(map[string]any{"environment": "staging"})); err != nil {
		t.Fatalf("unexpected error for a valid enum value: %v", err)
	}
}

func TestBindEventArgs_MaxLengthAndPattern(t *testing.T) {
	auto := &Automation{Name: "a", Args: schema(
		&ArgsField{Name: "code", Type: "string", MaxLength: 3},
		&ArgsField{Name: "id", Type: "string", Pattern: "^v1:"},
	)}
	if _, _, err := bindEventArgs(auto, eventWith(map[string]any{"code": "abcd", "id": "v1:x"})); err == nil {
		t.Fatalf("expected a maxLength violation for 'abcd'")
	}
	if _, _, err := bindEventArgs(auto, eventWith(map[string]any{"code": "abc", "id": "nope"})); err == nil {
		t.Fatalf("expected a pattern violation for 'nope'")
	}
	if _, _, err := bindEventArgs(auto, eventWith(map[string]any{"code": "abc", "id": "v1:ok"})); err != nil {
		t.Fatalf("unexpected error for valid maxLength+pattern: %v", err)
	}
}

func TestBindEventArgs_OptionalAbsent(t *testing.T) {
	auto := &Automation{Name: "a", Args: schema(req("environment", "string"), opt("replicas", "int"))}
	bound, _, err := bindEventArgs(auto, eventWith(map[string]any{"environment": "staging"}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, present := bound["replicas"]; present {
		t.Errorf("absent optional field must not be bound; bound=%v", bound)
	}
}

func TestBindEventArgs_NoArgsBlockIsNoOp(t *testing.T) {
	auto := &Automation{Name: "a"} // no Args
	bound, extras, err := bindEventArgs(auto, eventWith(map[string]any{"anything": 1}))
	if err != nil || bound != nil || extras != nil {
		t.Fatalf("no args block must be a no-op, got bound=%v extras=%v err=%v", bound, extras, err)
	}
}

// --- evaluator visibility: bodies may read args.X (G1) --------------------

func TestEvaluatorSeesArgs(t *testing.T) {
	auto := &Automation{Name: "a", Args: schema(req("environment", "string"))}
	bound, _, err := bindEventArgs(auto, eventWith(map[string]any{"environment": "staging"}))
	if err != nil {
		t.Fatalf("bind: %v", err)
	}

	eval := NewEvaluator()
	eval.SetCustom("args", bound)

	val, err := eval.EvaluateFilterValue("args.environment")
	if err != nil {
		t.Fatalf("resolve args.environment: %v", err)
	}
	if val != "staging" {
		t.Errorf("args.environment = %v, want staging", val)
	}

	ok, err := eval.EvaluateCondition(`args.environment == "staging"`)
	if err != nil {
		t.Fatalf("evaluate condition: %v", err)
	}
	if !ok {
		t.Errorf("expected `args.environment == \"staging\"` to be true")
	}
}

// --- refusal counter ------------------------------------------------------

func TestFireRefusalCounter(t *testing.T) {
	c := &fireRefusalCounter{}
	c.record("x")
	c.record("x")
	c.record("y")
	if c.Count() != 3 {
		t.Errorf("total = %d, want 3", c.Count())
	}
	if c.CountFor("x") != 2 || c.CountFor("y") != 1 || c.CountFor("z") != 0 {
		t.Errorf("per-name counts wrong: x=%d y=%d z=%d", c.CountFor("x"), c.CountFor("y"), c.CountFor("z"))
	}
}

// capturingLogger returns a logger writing to buf so a test can assert the
// loud Warn fired.
func capturingLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
