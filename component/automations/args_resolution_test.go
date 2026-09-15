package automations

// args_resolution_test.go -- what an automation's expressions may read beyond
// its statements (args_resolution.go): the trigger filter's and the
// preconditions' free names are roots, a statement body's names are the
// compiler's, and the G5 source scan (memql#2367).

import (
	"strings"
	"testing"
)

// preparedOuter prepares a Go-built automation the way the loader does, so
// the name rule reads its parsed nodes.
func preparedOuter(t *testing.T, a *Automation) *Automation {
	t.Helper()
	if err := PrepareExpressions(a); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	return a
}

// Every root a run binds before its first statement is a name a trigger
// filter and a precondition may read, and so is the filter's own parameter.
func TestValidateOuterExpressionNames_RootsAccepted(t *testing.T) {
	a := &Automation{
		Name: "good",
		Trigger: &TriggerConfig{
			Event:  "graph.node.created.v1:x:thing",
			Filter: `row => row.status == "open" && args.environment != "" && actor.userId != "" && config.demoMode != true && partition != nil && event.topic != "" && now != ""`,
		},
		Preconditions: []*Precondition{
			{ID: "envSet", Check: `args.environment != "" && config.demoMode != true`},
			{ID: "listed", Check: `args.list.any(v => v == now)`},
		},
	}
	if err := validateOuterExpressionNames(preparedOuter(t, a)); err != nil {
		t.Fatalf("a filter and preconditions over the roots were refused: %v", err)
	}
}

// A misspelled root is a load error rather than a filter that decides false,
// or a precondition that misses, on every fire.
func TestValidateOuterExpressionNames_UnknownNameRefused(t *testing.T) {
	for name, a := range map[string]*Automation{
		"a trigger filter": {Name: "bad", Trigger: &TriggerConfig{
			Event: "graph.node.created.v1:x:thing", Filter: `row => row.status == stauts`,
		}},
		"a precondition": {Name: "bad", Preconditions: []*Precondition{
			{ID: "envSet", Check: `arg.environment != ""`},
		}},
		"a bare args field": {Name: "bad", Preconditions: []*Precondition{
			{ID: "envSet", Check: `environment == "staging"`},
		}},
		"a lambda parameter outside its lambda": {Name: "bad", Preconditions: []*Precondition{
			{ID: "listed", Check: `args.list.any(v => v == "x") && v == "y"`},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateOuterExpressionNames(preparedOuter(t, a))
			if err == nil || !strings.Contains(err.Error(), "unknown name") {
				t.Fatalf("want an unknown-name refusal, got: %v", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Source-level end-to-end: the full compile pipeline accepts args read as
// args.<field> in every authored surface and rejects a typo'd bare name with
// the unknown-name error.
// ---------------------------------------------------------------------------

const argsReadSource = `@trigger(event="deploy.requested")
automation argsReadDeploy {
  args {
    environment     string   @required
    workdir         string   @required
    engineNodeTypes []string @required
  }
  gate := logic deployGateGreen(environment: args.environment, workdir: args.workdir)
  for nt in args.engineNodeTypes {
    logic buildOne(nodeType: nt, workdir: args.workdir)
  }
}`

func TestCompileMemQL_ArgsFieldsAccepted(t *testing.T) {
	loader := NewLoader(LoaderOptions{})
	auto, err := loader.compileMemQL(argsReadSource, "test:argsReadDeploy")
	if err != nil {
		t.Fatalf("compileMemQL rejected args read as args.<field>: %v", err)
	}
	if auto.Args == nil || len(auto.Args.Fields) != 3 {
		t.Fatalf("args schema not attached as expected")
	}
}

func TestCompileMemQL_TypoedBareNameRejected(t *testing.T) {
	loader := NewLoader(LoaderOptions{})
	// A typo'd bare name in a condition is an unknown name, refused at load
	// by the scope checker.
	src := strings.Replace(argsReadSource,
		"gate := logic deployGateGreen(environment: args.environment, workdir: args.workdir)",
		"if enviroment == \"development\" {\n    gate := logic deployGateGreen(environment: args.environment, workdir: args.workdir)\n  }", 1)
	if src == argsReadSource {
		t.Fatal("test setup: replacement did not apply")
	}
	_, err := loader.compileMemQL(src, "test:argsTypo")
	if err == nil || !strings.Contains(err.Error(), "`enviroment` is not a statement name") || !strings.Contains(err.Error(), "[body_unknown_name]") {
		t.Fatalf("typo'd bare name in a condition must fail compile, got: %v", err)
	}
}

// G5 (#2367): event.payload reads are retired in automation bodies -- the
// compile path rejects them with the migration hint; prose in comments and
// @description strings never trips the scan.
func TestCompileMemQL_EventPayloadReadRetired(t *testing.T) {
	loader := NewLoader(LoaderOptions{})

	// memql#3610: the scan was `event.payload.` ONLY, which made it blind to
	// the spelling authors actually reached for. `event.node.payload.X` reads
	// exactly like the shape of a graph-node event and resolves to NOTHING --
	// the CDC envelope has no `node` key -- so the filter decided false forever
	// and the automation never fired. That is how the computer-use kill switch
	// went inert. The narrow scan could not have caught it, because the broken
	// spelling was not the retired one; any dotted read off `event` is refused.
	for _, statement := range []string{
		`run := logic doThing(deploymentId: event.payload.deploymentId)`,
		`run := logic doThing(userId: event.node.id)`,
		`run := logic doThing(status: event.node.payload.status)`,
		`run := logic doThing(x: event.anythingElse)`,
	} {
		bad := `@trigger(event="node.updated", concept="v1:identity:user")
automation dottedEventRead {
  ` + statement + `
}`
		_, err := loader.compileMemQL(bad, "test:dottedEventRead")
		if err == nil || !strings.Contains(err.Error(), "reads are retired") {
			t.Errorf("%s must be rejected with the migration hint: a dotted read off `event` either is the "+
				"retired payload form or resolves to nothing at all, and BOTH are silent; got: %v", statement, err)
		}
	}

	// A bare `event` forwarded as a call argument carries no dot and stays
	// legal -- it is how a logic receives the whole envelope.
	okSrc := `@trigger(event="node.updated", concept="v1:identity:user")
automation forwardsEnvelope {
  run := logic doThing(event: event)
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
  run := logic doThing(deploymentId: args.deploymentId)
}`
	if _, err := loader.compileMemQL(prose, "test:proseOnly"); err != nil {
		t.Fatalf("prose-only event.payload mentions must not trip the retirement scan: %v", err)
	}
}
