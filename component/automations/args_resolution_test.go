package automations

// G2 (event-payload-binding ADR Decision 3, memql#2364) tests: bare-field
// resolution order at runtime, load-time shadowing + unknown-identifier
// rejection, and the args-less regression guarantee.

import (
	"strings"
	"testing"
)

func g2ArgsSchema(names ...string) *ArgsSchema {
	s := &ArgsSchema{}
	for _, n := range names {
		s.Fields = append(s.Fields, &ArgsField{Name: n, Type: "string", Optional: true})
	}
	return s
}

func g2Evaluator(bound map[string]any, declared map[string]bool) *Evaluator {
	e := NewEvaluator()
	if bound != nil {
		e.SetCustom("args", bound)
	}
	if declared != nil {
		e.SetCustom("argsDeclared", declared)
	}
	return e
}

// ---------------------------------------------------------------------------
// Runtime resolution order
// ---------------------------------------------------------------------------

// Tier 4: a bound args field resolves bare instead of falling through to the
// literal-string fallback.
func TestBareResolution_ArgsField(t *testing.T) {
	e := g2Evaluator(map[string]any{"environment": "staging"}, map[string]bool{"environment": true})
	got, err := e.EvaluateValue("environment")
	if err != nil {
		t.Fatalf("EvaluateValue: %v", err)
	}
	if got != "staging" {
		t.Fatalf("bare args field = %v, want staging", got)
	}
}

// A declared-but-absent optional field resolves to nil -- NEVER the literal
// text of the identifier (the silent-wrong-value class G2 kills).
func TestBareResolution_DeclaredAbsentIsNil(t *testing.T) {
	e := g2Evaluator(map[string]any{}, map[string]bool{"replicas": true})
	got, err := e.EvaluateValue("replicas")
	if err != nil {
		t.Fatalf("EvaluateValue: %v", err)
	}
	if got != nil {
		t.Fatalf("declared-but-absent optional = %v, want nil (not the literal)", got)
	}
}

// Tier 3 beats tier 4: a recorded step result wins over an args field of a
// DIFFERENT name; and load-time shadowing forbids same names, so runtime
// order only matters across distinct names.
func TestBareResolution_StepBeatsArgs(t *testing.T) {
	e := g2Evaluator(map[string]any{"environment": "staging"}, map[string]bool{"environment": true})
	e.SetStepResult("gate", &StepResult{StepId: "gate", Status: "success", Result: "green"})
	got, err := e.EvaluateValue("gate")
	if err != nil {
		t.Fatalf("EvaluateValue: %v", err)
	}
	if got != "green" {
		t.Fatalf("bare step ref = %v, want green", got)
	}
}

// Tier 2 beats tiers 3+4: the loop variable in scope wins.
func TestBareResolution_LoopVarBeatsArgs(t *testing.T) {
	e := g2Evaluator(map[string]any{"nt": "from-args"}, map[string]bool{"nt": true})
	e.SetItem(map[string]any{"x": 1}, "nt")
	got, err := e.EvaluateValue("nt")
	if err != nil {
		t.Fatalf("EvaluateValue: %v", err)
	}
	m, ok := got.(map[string]any)
	if !ok || m["x"] != 1 {
		t.Fatalf("bare loop var = %v, want the loop item", got)
	}
}

// Args-less regression: with NO args binding, a bare non-step identifier
// keeps the prior literal fallback and conditions keep their semantics.
func TestBareResolution_ArgsLessUnchanged(t *testing.T) {
	e := NewEvaluator()
	got, err := e.EvaluateValue("environment")
	if err != nil {
		t.Fatalf("EvaluateValue: %v", err)
	}
	if got != "environment" {
		t.Fatalf("args-less bare identifier = %v, want the literal fallback %q", got, "environment")
	}
}

// Conditions: a bare args-field operand compares against its bound value.
func TestBareResolution_ConditionOperand(t *testing.T) {
	e := g2Evaluator(map[string]any{"environment": "development"}, map[string]bool{"environment": true})
	ok, err := e.EvaluateCondition(`environment == "development"`)
	if err != nil {
		t.Fatalf("EvaluateCondition: %v", err)
	}
	if !ok {
		t.Fatal("condition on bare args field should be true")
	}
	ok, err = e.EvaluateCondition(`environment == "staging"`)
	if err != nil {
		t.Fatalf("EvaluateCondition: %v", err)
	}
	if ok {
		t.Fatal("condition on bare args field should be false for a non-matching value")
	}
}

// Explicit args.X keeps working as the disambiguator.
func TestBareResolution_ExplicitArgsStillWorks(t *testing.T) {
	e := g2Evaluator(map[string]any{"environment": "production"}, map[string]bool{"environment": true})
	ok, err := e.EvaluateCondition(`args.environment == "production"`)
	if err != nil {
		t.Fatalf("EvaluateCondition: %v", err)
	}
	if !ok {
		t.Fatal("explicit args.X must remain valid")
	}
}

// ---------------------------------------------------------------------------
// Load-time validation
// ---------------------------------------------------------------------------

func TestValidateArgsResolution_ReservedShadow(t *testing.T) {
	a := &Automation{Name: "bad", Args: g2ArgsSchema("event")}
	err := validateArgsResolution(a)
	if err == nil || !strings.Contains(err.Error(), "reserved engine name") {
		t.Fatalf("args field shadowing a reserved name must be rejected, got: %v", err)
	}
}

func TestValidateArgsResolution_StepShadow(t *testing.T) {
	a := &Automation{
		Name: "bad",
		Args: g2ArgsSchema("gate"),
		Steps: []*Step{
			{ID: "gate", Type: StepTypeFunction, Function: &FunctionStepConfig{Name: "f"}},
		},
	}
	err := validateArgsResolution(a)
	if err == nil || !strings.Contains(err.Error(), "shadows the args field") {
		t.Fatalf("step id shadowing an args field must be rejected, got: %v", err)
	}
}

func TestValidateArgsResolution_LoopVarShadow(t *testing.T) {
	a := &Automation{
		Name: "bad",
		Args: g2ArgsSchema("nt"),
		Steps: []*Step{
			{ID: "loop", Type: StepTypeForEach, ForEach: &ForEachStepConfig{
				Source: "$args.nodes", As: "nt",
				Do: []*Step{{ID: "inner", Type: StepTypeFunction, Function: &FunctionStepConfig{Name: "f"}}},
			}},
		},
	}
	err := validateArgsResolution(a)
	if err == nil || !strings.Contains(err.Error(), "loop variable") {
		t.Fatalf("forEach loop var shadowing an args field must be rejected, got: %v", err)
	}
}

func TestValidateArgsResolution_UnknownBareIdentifier(t *testing.T) {
	a := &Automation{
		Name: "bad",
		Args: g2ArgsSchema("environment"),
		Steps: []*Step{
			{ID: "gate", Type: StepTypeFunction,
				Condition: `enviroment == "staging"`, // typo'd field
				Function:  &FunctionStepConfig{Name: "f"}},
		},
	}
	err := validateArgsResolution(a)
	if err == nil || !strings.Contains(err.Error(), `unknown bare identifier "enviroment"`) {
		t.Fatalf("typo'd bare identifier must be a load error, got: %v", err)
	}
}

// Every resolvable form passes: reserved roots, loop vars in scope, step
// names, args fields, explicit forms, calls, quoted literals, $-refs.
func TestValidateArgsResolution_AllTiersAccepted(t *testing.T) {
	a := &Automation{
		Name: "good",
		Args: g2ArgsSchema("environment", "workdir"),
		Steps: []*Step{
			{ID: "gate", Type: StepTypeFunction,
				Condition: `environment == "development" && exists(event.payload.ref)`,
				Function: &FunctionStepConfig{Name: "f", Args: map[string]any{
					"env":     "environment",
					"dir":     "$args.workdir",
					"prev":    "gate",
					"literal": `"development"`,
					"nested":  map[string]any{"deep": "workdir"},
				}}},
			{ID: "fan", Type: StepTypeForEach, ForEach: &ForEachStepConfig{
				Source: "$event.payload.engineNodeTypes", As: "nt",
				Filter: `nt != "voice" && environment == "development"`,
				Do: []*Step{{ID: "per", Type: StepTypeFunction,
					Function: &FunctionStepConfig{Name: "f", Args: map[string]any{"nodeType": "nt"}}}},
			}},
			{ID: "pick", Type: StepTypeSwitch, Switch: &SwitchStepConfig{
				Expression: "environment",
				Cases: map[string]*SwitchCase{
					"development": {Steps: []*Step{{ID: "devCase", Type: StepTypeFunction,
						Function: &FunctionStepConfig{Name: "f"}}}},
				},
			}},
		},
	}
	if err := validateArgsResolution(a); err != nil {
		t.Fatalf("valid automation rejected: %v", err)
	}
}

// A loop variable is OUT of scope outside its forEach body.
func TestValidateArgsResolution_LoopVarScoped(t *testing.T) {
	a := &Automation{
		Name: "bad",
		Args: g2ArgsSchema("environment"),
		Steps: []*Step{
			{ID: "fan", Type: StepTypeForEach, ForEach: &ForEachStepConfig{
				Source: "$event.payload.list", As: "nt",
				Do: []*Step{{ID: "per", Type: StepTypeFunction, Function: &FunctionStepConfig{Name: "f"}}},
			}},
			{ID: "after", Type: StepTypeFunction,
				Condition: `nt == "voice"`, // loop var referenced outside its forEach
				Function:  &FunctionStepConfig{Name: "f"}},
		},
	}
	err := validateArgsResolution(a)
	if err == nil || !strings.Contains(err.Error(), `unknown bare identifier "nt"`) {
		t.Fatalf("loop var used outside its scope must be a load error, got: %v", err)
	}
}

// Args-less automations are fully exempt -- whatever their expressions
// contain, the G2 validator never fires (zero tree regression).
func TestValidateArgsResolution_ArgsLessExempt(t *testing.T) {
	a := &Automation{
		Name: "legacy",
		Steps: []*Step{
			{ID: "s", Type: StepTypeFunction,
				Condition: `anything == "goes" && unknownWord`,
				Function:  &FunctionStepConfig{Name: "f", Args: map[string]any{"x": "someBareWord"}}},
		},
	}
	if err := validateArgsResolution(a); err != nil {
		t.Fatalf("args-less automation must be exempt, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Source-level end-to-end: the full compile pipeline (rewriter -> compile ->
// validateArgsResolution) accepts bare fields in every authored surface and
// rejects a typo'd one with the unknown-identifier error.
// ---------------------------------------------------------------------------

const g2E2ESource = `@trigger(event="deploy.requested", concept="v1:cluster:deployment")
automation g2BareFieldsDeploy {
  args {
    environment     string   @required
    workdir         string   @required
    engineNodeTypes []string @required
  }
  step gate {
    logic deployGateGreen { environment: environment, workdir: workdir }
  }
  step fan {
    forEach nt in engineNodeTypes {
      logic buildOne { nodeType: nt, workdir: workdir }
    }
  }
}`

func TestCompileMemQL_BareArgsFieldsAccepted(t *testing.T) {
	loader := NewLoader(LoaderOptions{})
	auto, err := loader.compileMemQL(g2E2ESource, "test:g2BareFieldsDeploy")
	if err != nil {
		t.Fatalf("compileMemQL rejected valid bare args fields: %v", err)
	}
	if auto.Args == nil || len(auto.Args.Fields) != 3 {
		t.Fatalf("args schema not attached as expected")
	}
}

func TestCompileMemQL_TypoedBareFieldRejected(t *testing.T) {
	loader := NewLoader(LoaderOptions{})
	// Typos in CONDITIONS are strictly rejected (a condition token is always
	// an expression). Single-bare-word VALUES are pun-or-literal by design --
	// lowering strips quotes, so `status: "x"` and a bare ref are identical
	// strings and the runtime resolves ref-first, literal-fallback (see
	// checkValueIdentifiers).
	src := strings.Replace(g2E2ESource,
		"logic deployGateGreen { environment: environment, workdir: workdir }",
		"if enviroment == \"development\" {\n      logic deployGateGreen { environment: environment, workdir: workdir }\n    }", 1)
	if src == g2E2ESource {
		t.Fatal("test setup: replacement did not apply")
	}
	_, err := loader.compileMemQL(src, "test:g2Typo")
	if err == nil || !strings.Contains(err.Error(), `unknown bare identifier "enviroment"`) {
		t.Fatalf("typo'd bare field in a condition must fail compile, got: %v", err)
	}
}

// G5 (#2367): event.payload reads are retired in automation bodies -- the
// compile path rejects them with the migration hint; prose in comments and
// @description strings never trips the scan.
func TestCompileMemQL_EventPayloadReadRetired(t *testing.T) {
	loader := NewLoader(LoaderOptions{})
	src := `@trigger(event="deploy.requested", concept="v1:cluster:deployment")
automation legacyReader {
  step run {
    logic doThing(deploymentId: event.payload.deploymentId)
  }
}`
	_, err := loader.compileMemQL(src, "test:legacyReader")
	if err == nil || !strings.Contains(err.Error(), "event.payload.<field> reads are retired") {
		t.Fatalf("event.payload read must be rejected with the migration hint, got: %v", err)
	}

	// Prose mentions are fine: comment + @description string.
	prose := `// reads event.payload.id historically
@trigger(event="deploy.requested", concept="v1:cluster:deployment")
@description("binds args from event.payload.status transitions")
automation proseOnly {
  args {
    deploymentId any
  }
  step run {
    logic doThing(deploymentId)
  }
}`
	if _, err := loader.compileMemQL(prose, "test:proseOnly"); err != nil {
		t.Fatalf("prose-only event.payload mentions must not trip the retirement scan: %v", err)
	}
}

// G5 fallout fixes (#2367, caught by the DB-gated conformance suite in CI):

// ResolveArgsPath walks a dotted path whose head is an args field; a missing
// tail yields (nil, true) so coalesce defaults apply.
func TestResolveArgsPath(t *testing.T) {
	e := g2Evaluator(map[string]any{"node": map[string]any{"id": "n1"}}, map[string]bool{"node": true})
	v, ok := e.ResolveArgsPath("node.id")
	if !ok || v != "n1" {
		t.Fatalf("node.id = (%v,%v), want (n1,true)", v, ok)
	}
	v, ok = e.ResolveArgsPath("node.missing.deep")
	if !ok || v != nil {
		t.Fatalf("missing tail = (%v,%v), want (nil,true)", v, ok)
	}
	if _, ok := e.ResolveArgsPath("unknown.id"); ok {
		t.Fatal("unknown head must not resolve")
	}
	argsLess := NewEvaluator()
	if _, ok := argsLess.ResolveArgsPath("node.id"); ok {
		t.Fatal("args-less evaluator must not resolve paths")
	}
}
