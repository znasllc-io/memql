package memql

import (
	"context"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// @requiresCapability ON A BUILTIN (epic memql#5288, task memql#5301).
//
// The Deployables parts live mostly on builtins -- packageDeploy, siteArchive,
// customDomainAdd -- and a builtin's execution never passes the mutation or
// logic entry points where the gate already sat: a top-level builtin call
// returns from executeWith's own branch before the plan-level refusal runs.
// These pin the seam that closes that: the builtin executor asks the same
// question, refuses the same way, and passes internal origin the same way.

const gatedProbeBuiltin = "gatedDeployProbe"

// installBuiltinGateProbe registers one gated builtin on a bare engine with a
// fake executor that records whether it was reached.
func installBuiltinGateProbe(t *testing.T) (*MemQLEngine, *int) {
	t.Helper()
	reached := 0
	e := &MemQLEngine{functions: newFunctionRegistry()}
	_ = e.functions.Upsert(&Function{
		Name:               gatedProbeBuiltin,
		Type:               FunctionTypeBuiltin,
		FunctionKind:       FunctionTypeBuiltin,
		Executor:           "integration.probe.deploy",
		Enabled:            true,
		RequiresCapability: CapabilityRequirement{Verb: auth.VerbExecute, Resource: "app:deployables/deploy"},
	})
	e.builtinExecutorHandlers = map[string]builtinExecutorHandler{
		"integration.probe.deploy": func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error) {
			reached++
			return nil, nil
		},
	}
	return e, &reached
}

func installDeployPartFake(t *testing.T) {
	t.Helper()
	deploy := auth.VerbResource{Verb: auth.VerbExecute, Resource: "app:deployables/deploy"}
	auth.SetCapabilityCatalog(&capabilityFake{
		ranks: map[string]int{"owner": 400, "developer": 300, "admin": 200, "user": 100, "writer": 100, "viewer": 50},
		grants: map[string]map[auth.VerbResource]bool{
			"owner":     {deploy: true},
			"developer": {deploy: true},
			"admin":     {},
			"user":      {},
			"writer":    {},
			"viewer":    {},
		},
	})
	t.Cleanup(func() { auth.SetCapabilityCatalog(nil) })
}

func TestABuiltinDeclaringAPartRefusesARoleWithoutItAndAdmitsOneWithIt(t *testing.T) {
	installDeployPartFake(t)
	e, reached := installBuiltinGateProbe(t)
	call := &BuiltinFunctionExpression{Name: gatedProbeBuiltin, Executor: "integration.probe.deploy"}

	_, err := e.evaluateBuiltinFunctionExpression(asCaller("writer"), call, 0)
	if err == nil {
		t.Fatal("a writer was admitted to a builtin requiring execute on app:deployables/deploy")
	}
	if !strings.Contains(err.Error(), "app:deployables/deploy") || !strings.Contains(err.Error(), gatedProbeBuiltin) {
		t.Errorf("the refusal must name the builtin and the requirement: %v", err)
	}
	if *reached != 0 {
		t.Fatal("the executor ran for a refused caller; the gate sits after the handler")
	}

	if _, err := e.evaluateBuiltinFunctionExpression(asCaller("developer"), call, 0); err != nil {
		t.Fatalf("a developer holding the part was refused: %v", err)
	}
	if *reached != 1 {
		t.Fatalf("the executor did not run for an admitted caller (reached=%d)", *reached)
	}
}

func TestInternalOriginPassesTheBuiltinGate(t *testing.T) {
	installDeployPartFake(t)
	e, reached := installBuiltinGateProbe(t)
	call := &BuiltinFunctionExpression{Name: gatedProbeBuiltin, Executor: "integration.probe.deploy"}

	// An automation driving the builtin on a person's behalf: trusted Go, not
	// a principal, exactly as the mutation and logic gates treat it.
	ctx := auth.ContextWithInternalOrigin(asCaller("writer"))
	if _, err := e.evaluateBuiltinFunctionExpression(ctx, call, 0); err != nil {
		t.Fatalf("internal origin was refused the gated builtin: %v", err)
	}
	if *reached != 1 {
		t.Fatal("the executor did not run under internal origin")
	}
}

func TestABuiltinWithNoIdentityIsRefusedThePart(t *testing.T) {
	installDeployPartFake(t)
	e, reached := installBuiltinGateProbe(t)
	call := &BuiltinFunctionExpression{Name: gatedProbeBuiltin, Executor: "integration.probe.deploy"}
	if _, err := e.evaluateBuiltinFunctionExpression(context.Background(), call, 0); err == nil {
		t.Fatal("a call with no caller identity was admitted to a gated builtin")
	}
	if *reached != 0 {
		t.Fatal("the executor ran for an anonymous caller")
	}
}

// TestAMetaCommandIsNotGated. `concepts`, `functions` and `help` dispatch
// through the same expression type under names no registry holds. They
// carry no requirement and must keep answering, or refusing them would take
// the engine's own introspection dark.
func TestAMetaCommandIsNotGated(t *testing.T) {
	installDeployPartFake(t)
	e, _ := installBuiltinGateProbe(t)
	reached := 0
	e.builtinExecutorHandlers["meta.probe"] = func(context.Context, map[string]any, int) ([]memorynodes.MemoryNode, error) {
		reached++
		return nil, nil
	}
	call := &BuiltinFunctionExpression{Name: "notInAnyRegistry", Executor: "meta.probe"}
	if _, err := e.evaluateBuiltinFunctionExpression(asCaller("viewer"), call, 0); err != nil {
		t.Fatalf("an unregistered (meta) builtin was refused: %v", err)
	}
	if reached != 1 {
		t.Fatal("the meta executor did not run")
	}
}

// TestABuiltinParsedFromSourceCarriesItsRequirement is the converter half:
// the annotation is accepted on a builtin and lands on the Function, and an
// annotation the builtin set still does not accept is refused by name.
func TestABuiltinParsedFromSourceCarriesItsRequirement(t *testing.T) {
	fn := parseBuiltinForTest(t, `@sdk
@executor("integration.packages.deploy")
@args(profile="object")
@requiresCapability("execute", "app:deployables/deploy")
builtin parsedProbe {
  packageId  string!  @description("The package.")
}
`)
	if fn.RequiresCapability != (CapabilityRequirement{Verb: auth.VerbExecute, Resource: "app:deployables/deploy"}) {
		t.Fatalf("the builtin's @requiresCapability did not reach the Function: %+v", fn.RequiresCapability)
	}

	// The ONE-argument form is what a typo produces; it must arrive as a
	// half-requirement so the load-time validator refuses it by name rather
	// than as no requirement.
	half := parseBuiltinForTest(t, `@sdk
@executor("integration.packages.deploy")
@args(profile="object")
@requiresCapability("execute")
builtin halfProbe {
  packageId  string!  @description("The package.")
}
`)
	if half.RequiresCapability.complete() || !half.RequiresCapability.declared() {
		t.Fatalf("a one-argument @requiresCapability on a builtin must be a half-requirement, got %+v", half.RequiresCapability)
	}
}
