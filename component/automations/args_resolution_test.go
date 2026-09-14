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

// A bound args field resolves bare (the G2 tier, run_scope.go).
func TestBareResolution_ArgsField(t *testing.T) {
	e := g2Evaluator(map[string]any{"environment": "staging"}, map[string]bool{"environment": true})
	got, err := evalV1(e, "environment")
	if err != nil {
		t.Fatalf("environment: %v", err)
	}
	if got != "staging" {
		t.Fatalf("bare args field = %v, want staging", got)
	}
}

// A declared-but-absent optional field resolves to nil -- NEVER the literal
// text of the identifier (the silent-wrong-value class G2 kills).
func TestBareResolution_DeclaredAbsentIsNil(t *testing.T) {
	e := g2Evaluator(map[string]any{}, map[string]bool{"replicas": true})
	got, err := evalV1(e, "replicas")
	if err != nil {
		t.Fatalf("replicas: %v", err)
	}
	if got != nil {
		t.Fatalf("declared-but-absent optional = %v, want nil (not the literal)", got)
	}
}

// A recorded step result wins over an args field of a DIFFERENT name; and
// load-time shadowing forbids same names, so runtime order only matters
// across distinct names.
func TestBareResolution_StepBeatsArgs(t *testing.T) {
	e := g2Evaluator(map[string]any{"environment": "staging"}, map[string]bool{"environment": true})
	e.SetStepResult("gate", &StepResult{StepId: "gate", Status: "success", Result: "green"})
	got, err := evalV1(e, "gate")
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	if got != "green" {
		t.Fatalf("bare step ref = %v, want green", got)
	}
}

// The loop variable in scope wins over an args field of the same name.
func TestBareResolution_LoopVarBeatsArgs(t *testing.T) {
	e := g2Evaluator(map[string]any{"nt": "from-args"}, map[string]bool{"nt": true})
	e.SetItem(map[string]any{"x": 1}, "nt")
	got, err := evalV1(e, "nt")
	if err != nil {
		t.Fatalf("nt: %v", err)
	}
	m, ok := got.(map[string]any)
	if !ok || m["x"] != 1 {
		t.Fatalf("bare loop var = %v, want the loop item", got)
	}
}

// With NO args binding, a bare name that is no root, step or loop variable is
// an unknown name, refused -- the string evaluator read it as its own text.
func TestBareResolution_ArgsLessUnknownNameRefused(t *testing.T) {
	e := NewEvaluator()
	got, err := evalV1(e, "environment")
	if err == nil {
		t.Fatalf("args-less bare identifier = %#v, want an unknown-name refusal (never the literal text)", got)
	}
}

// Conditions: a bare args-field operand compares against its bound value.
func TestBareResolution_ConditionOperand(t *testing.T) {
	e := g2Evaluator(map[string]any{"environment": "development"}, map[string]bool{"environment": true})
	ok, err := evalV1Cond(t, e, `environment == "development"`)
	if err != nil {
		t.Fatalf("condition: %v", err)
	}
	if !ok {
		t.Fatal("condition on bare args field should be true")
	}
	ok, err = evalV1Cond(t, e, `environment == "staging"`)
	if err != nil {
		t.Fatalf("condition: %v", err)
	}
	if ok {
		t.Fatal("condition on bare args field should be false for a non-matching value")
	}
}

// Explicit args.X keeps working as the disambiguator.
func TestBareResolution_ExplicitArgsStillWorks(t *testing.T) {
	e := g2Evaluator(map[string]any{"environment": "production"}, map[string]bool{"environment": true})
	ok, err := evalV1Cond(t, e, `args.environment == "production"`)
	if err != nil {
		t.Fatalf("condition: %v", err)
	}
	if !ok {
		t.Fatal("explicit args.X must remain valid")
	}
}

// ---------------------------------------------------------------------------
// Load-time validation
// ---------------------------------------------------------------------------

// preparedForG2 prepares a Go-built automation the way the loader does, so
// the name rules read its parsed nodes.
func preparedForG2(t *testing.T, a *Automation) *Automation {
	t.Helper()
	if err := PrepareExpressions(a); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	return a
}

// expr is a compiled value leaf holding an expression.
func expr(src string) map[string]any { return map[string]any{"$expr": src} }

func TestValidateArgsResolution_ReservedShadow(t *testing.T) {
	a := &Automation{Name: "bad", Args: g2ArgsSchema("event")}
	err := validateArgsResolution(preparedForG2(t, a))
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
	err := validateArgsResolution(preparedForG2(t, a))
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
				Source: "args.nt", As: "nt",
				Do: []*Step{{ID: "inner", Type: StepTypeFunction, Function: &FunctionStepConfig{Name: "f"}}},
			}},
		},
	}
	err := validateArgsResolution(preparedForG2(t, a))
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
	err := validateArgsResolution(preparedForG2(t, a))
	if err == nil || !strings.Contains(err.Error(), `unknown name "enviroment"`) {
		t.Fatalf("typo'd bare identifier must be a load error, got: %v", err)
	}
}

// Every resolvable form passes: reserved roots, loop vars in scope, step
// names, args fields, explicit forms and calls. A string leaf is a literal,
// whatever it spells.
func TestValidateArgsResolution_AllTiersAccepted(t *testing.T) {
	a := &Automation{
		Name: "good",
		Args: g2ArgsSchema("environment", "workdir"),
		Steps: []*Step{
			{ID: "gate", Type: StepTypeFunction,
				Condition: `environment == "development" && event.payload.ref != nil`,
				Function: &FunctionStepConfig{Name: "f", Args: map[string]any{
					"env":     expr("environment"),
					"dir":     expr("args.workdir"),
					"literal": "development",
					"nested":  map[string]any{"deep": expr("workdir")},
				}}},
			{ID: "after", Type: StepTypeFunction,
				Function: &FunctionStepConfig{Name: "f", Args: map[string]any{"prev": expr("gate")}}},
			{ID: "fan", Type: StepTypeForEach, ForEach: &ForEachStepConfig{
				Source: "event.payload.engineNodeTypes", As: "nt",
				Filter: `nt != "voice" && environment == "development"`,
				Do: []*Step{{ID: "per", Type: StepTypeFunction,
					Function: &FunctionStepConfig{Name: "f", Args: map[string]any{"nodeType": expr("nt")}}}},
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
	if err := validateArgsResolution(preparedForG2(t, a)); err != nil {
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
				Source: "event.payload.list", As: "nt",
				Do: []*Step{{ID: "per", Type: StepTypeFunction, Function: &FunctionStepConfig{Name: "f"}}},
			}},
			{ID: "after", Type: StepTypeFunction,
				Condition: `nt == "voice"`, // loop var referenced outside its forEach
				Function:  &FunctionStepConfig{Name: "f"}},
		},
	}
	err := validateArgsResolution(preparedForG2(t, a))
	if err == nil || !strings.Contains(err.Error(), `unknown name "nt"`) {
		t.Fatalf("loop var used outside its scope must be a load error, got: %v", err)
	}
}

// Args-less automations are exempt from the free-name check -- whatever their
// expressions contain, the G2 validator never fires (zero tree regression).
func TestValidateArgsResolution_ArgsLessExempt(t *testing.T) {
	a := &Automation{
		Name: "argsLess",
		Steps: []*Step{
			{ID: "s", Type: StepTypeFunction,
				Condition: `anything == "goes" && unknownWord`,
				Function:  &FunctionStepConfig{Name: "f", Args: map[string]any{"x": expr("someBareWord")}}},
		},
	}
	if err := validateArgsResolution(preparedForG2(t, a)); err != nil {
		t.Fatalf("args-less automation must be exempt, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Source-level end-to-end: the full compile pipeline (rewriter -> compile ->
// validateArgsResolution) accepts bare fields in every authored surface and
// rejects a typo'd one with the unknown-identifier error.
// ---------------------------------------------------------------------------

const g2E2ESource = `@trigger(event="deploy.requested")
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
	// A typo'd bare name in a condition is an unknown name, refused at load.
	src := strings.Replace(g2E2ESource,
		"logic deployGateGreen { environment: environment, workdir: workdir }",
		"if enviroment == \"development\" {\n      logic deployGateGreen { environment: environment, workdir: workdir }\n    }", 1)
	if src == g2E2ESource {
		t.Fatal("test setup: replacement did not apply")
	}
	_, err := loader.compileMemQL(src, "test:g2Typo")
	if err == nil || !strings.Contains(err.Error(), `unknown name "enviroment"`) {
		t.Fatalf("typo'd bare field in a condition must fail compile, got: %v", err)
	}
}

// G5 (#2367): event.payload reads are retired in automation bodies -- the
// compile path rejects them with the migration hint; prose in comments and
// @description strings never trips the scan.
func TestCompileMemQL_EventPayloadReadRetired(t *testing.T) {
	loader := NewLoader(LoaderOptions{})
	src := `@trigger(event="deploy.requested")
automation legacyReader {
  step run {
    logic doThing(deploymentId: event.payload.deploymentId)
  }
}`
	_, err := loader.compileMemQL(src, "test:legacyReader")
	if err == nil || !strings.Contains(err.Error(), "reads are retired") {
		t.Fatalf("event.payload read must be rejected with the migration hint, got: %v", err)
	}

	// memql#3610: the scan was `event.payload.` ONLY, which made it blind to
	// the spelling authors actually reached for. `event.node.payload.X` reads
	// exactly like the shape of a graph-node event and resolves to NOTHING --
	// the CDC envelope has no `node` key -- so the filter decided false forever
	// and the automation never fired. That is how the computer-use kill switch
	// went inert. The narrow scan could not have caught it, because the broken
	// spelling was not the retired one; any dotted read off `event` is refused.
	for _, body := range []string{
		`logic doThing(userId: event.node.id)`,
		`logic doThing(status: event.node.payload.status)`,
		`logic doThing(x: event.anythingElse)`,
	} {
		bad := `@trigger(event="node.updated", concept="v1:identity:user")
automation dottedEventRead {
  step run {
    ` + body + `
  }
}`
		if _, err := loader.compileMemQL(bad, "test:dottedEventRead"); err == nil {
			t.Errorf("%s must be rejected: a dotted read off `event` either is the "+
				"retired payload form or resolves to nothing at all, and BOTH are silent", body)
		}
	}

	// A bare `event` forwarded as a step argument carries no dot and stays legal
	// -- it is how a logic body receives the whole envelope.
	okSrc := `@trigger(event="node.updated", concept="v1:identity:user")
automation forwardsEnvelope {
  step run {
    logic doThing ( event: event )
  }
}`
	if _, err := loader.compileMemQL(okSrc, "test:forwardsEnvelope"); err != nil {
		t.Errorf("forwarding the bare envelope must stay legal, got: %v", err)
	}

	// Prose mentions are fine: comment + @description string.
	prose := `// reads event.payload.id historically
@trigger(event="deploy.requested")
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
