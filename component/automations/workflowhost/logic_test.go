package workflowhost

import (
	"context"
	"fmt"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

func logicFixture(t *testing.T, sources ...string) func(string) (*memql.Function, error) {
	t.Helper()
	all := map[string]*memql.Function{}
	for _, source := range sources {
		for _, slice := range memql.ExtractFunctionSlices(source) {
			fn, err := memql.BuildFunctionConstruct(source, slice.Name, "unified:scope/logic.memql", nil)
			if err != nil {
				t.Fatal(err)
			}
			all[fn.Name] = fn
		}
	}
	return func(name string) (*memql.Function, error) {
		if fn := all[name]; fn != nil {
			return fn, nil
		}
		return nil, fmt.Errorf("missing logic %s", name)
	}
}

func TestScopePureLogicComposesAndValidatesArguments(t *testing.T) {
	load := fixture(t, `@template automation decideScope {
 args { trigger string! }
 decision := logic outerDecision(trigger: args.trigger)
 if decision { return builtin scopeProbe() }
 return "no work"
}`)
	logics := logicFixture(t, `logic outerDecision { args { trigger string! }
 return logic innerDecision(trigger: args.trigger)
}`, `logic innerDecision { args { trigger string! }
 return args.trigger == "reactive" || args.trigger == "recurring"
}`)
	calls := 0
	opts := Options{Load: load, LoadLogic: logics, Operations: map[string]Operation{
		"scopeProbe": func(context.Context, map[string]any) (any, error) { calls++; return "opened", nil },
	}}
	for _, trigger := range []string{"reactive", "recurring", "standing"} {
		got, err := Run(context.Background(), "decideScope", map[string]any{"trigger": trigger}, opts)
		want := "opened"
		if trigger == "standing" {
			want = "no work"
		}
		if err != nil || got != want {
			t.Fatalf("%s: %v %v", trigger, got, err)
		}
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
	fn, _ := logics("innerDecision")
	fn.ArgsSchema.Fields[0].Enum = []any{"standing"}
	if _, err := Run(context.Background(), "decideScope", map[string]any{"trigger": "reactive"}, opts); err == nil || calls != 2 {
		t.Fatalf("logic argument contract was not enforced: err=%v calls=%d", err, calls)
	}
}

func TestScopeRejectsImpureOrGatedLogicBeforeAnyEffect(t *testing.T) {
	load := fixture(t, `@template automation decideScope {
 builtin scopeProbe()
 return logic decision()
}`)
	for _, source := range []string{
		`logic decision { return query privateRows() }`,
		`logic decision { return builtin scopeProbe() }`,
		`logic decision { return logic decision() }`,
		`@serverOnly logic decision { return true }`,
		`@requiresOwner logic decision { return true }`,
		`@disabled logic decision { return true }`,
	} {
		t.Run(source, func(t *testing.T) {
			calls := 0
			_, err := Run(context.Background(), "decideScope", nil, Options{Load: load, LoadLogic: logicFixture(t, source), Operations: map[string]Operation{
				"scopeProbe": func(context.Context, map[string]any) (any, error) { calls++; return nil, nil },
			}})
			if err == nil || calls != 0 {
				t.Fatalf("preflight err=%v calls=%d", err, calls)
			}
		})
	}
}

func TestScopeExecutesThePureBodyAndSchemaItPreflighted(t *testing.T) {
	for _, mutateInPlace := range []bool{false, true} {
		t.Run(fmt.Sprintf("in-place=%t", mutateInPlace), func(t *testing.T) {
			load := fixture(t, `@template automation frozenScope {
 builtin replaceDefinition()
 return logic decision(trigger: "allowed")
}`)
			logics := logicFixture(t, `logic decision { args { trigger string! } return "original" }`)
			fn, _ := logics("decision")
			fn.ArgsSchema.Fields[0].Enum = []any{"allowed"}
			replacement := logicFixture(t, `logic decision { return builtin escapedEffect() }`)
			replacementFn, _ := replacement("decision")
			escaped := 0
			got, err := Run(context.Background(), "frozenScope", nil, Options{Load: load, LoadLogic: logics, Operations: map[string]Operation{
				"replaceDefinition": func(context.Context, map[string]any) (any, error) {
					if mutateInPlace {
						fn.LogicBody[0] = replacementFn.LogicBody[0]
					} else {
						fn.LogicBody = replacementFn.LogicBody
					}
					fn.ArgsSchema.Fields[0].Enum[0] = "changed"
					return nil, nil
				},
				"escapedEffect": func(context.Context, map[string]any) (any, error) { escaped++; return "escaped", nil },
			}})
			if err != nil || got != "original" || escaped != 0 {
				t.Fatalf("preflighted definition changed: result=%v err=%v escaped=%d", got, err, escaped)
			}
		})
	}
}

func TestScopeFreezesTemplatesAndOperationBindings(t *testing.T) {
	load := fixture(t, `@template automation frozenScope {
 builtin replaceDefinition()
 return automation childScope()
}`, `@template automation childScope { return builtin originalEffect() }`)
	child, _ := load("childScope")
	original, replaced := 0, 0
	ops := map[string]Operation{}
	ops["originalEffect"] = func(context.Context, map[string]any) (any, error) { original++; return "original", nil }
	ops["replacedEffect"] = func(context.Context, map[string]any) (any, error) { replaced++; return "replaced", nil }
	ops["replaceDefinition"] = func(context.Context, map[string]any) (any, error) {
		child.Steps[0].Function.Name = "replacedEffect"
		ops["originalEffect"] = ops["replacedEffect"]
		return nil, nil
	}
	got, err := Run(context.Background(), "frozenScope", nil, Options{Load: load, Operations: ops})
	if err != nil || got != "original" || original != 1 || replaced != 0 {
		t.Fatalf("preflighted template changed: result=%v err=%v calls=%d/%d", got, err, original, replaced)
	}
}
