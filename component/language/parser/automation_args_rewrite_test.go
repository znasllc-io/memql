package parser

import (
	"strings"
	"testing"
)

// G1 (event-payload-binding ADR, memql#2363): a full-form automation may
// declare an `args { }` block. The struct-form rewriter hoists it to the
// file-top position (like logic/query) so the parser attaches it to the
// automation's FunctionDef.ArgsSchema.
func TestNormaliseAutomationSource_HoistsArgsBlock(t *testing.T) {
	src := `@trigger(event="deploy.requested", concept="v1:cluster:deployment", partition="*")
automation deployWithArgs {
  args {
    environment string @required @enum("development", "staging", "production")
  }
  step gate {
    logic requireForwardDeploy { environment: args.environment }
  }
}`
	out, err := NormaliseAutomationSource(src)
	if err != nil {
		t.Fatalf("NormaliseAutomationSource error: %v", err)
	}
	// The args block must be emitted at file-top, BEFORE the procedural func
	// header, so parseDefinition binds it as the automation's args schema.
	argsIdx := strings.Index(out, "args {")
	funcIdx := strings.Index(out, "func (Automation) deployWithArgs")
	if argsIdx < 0 || funcIdx < 0 {
		t.Fatalf("expected both an args block and the automation func in the output:\n%s", out)
	}
	if argsIdx > funcIdx {
		t.Fatalf("args block must precede the func header; got:\n%s", out)
	}
	if !strings.Contains(out, "environment string @required") {
		t.Errorf("args field declaration lost in rewrite:\n%s", out)
	}
	// The step must still be present.
	if !strings.Contains(out, "gate := requireForwardDeploy(") {
		t.Errorf("step lost in rewrite:\n%s", out)
	}
}

// A full-form automation with an args block round-trips through the full
// NormaliseAll chain and parses into a FunctionDef carrying the args schema.
func TestAutomationArgsBlock_ParsesToArgsSchema(t *testing.T) {
	src := `@trigger(event="deploy.requested", concept="v1:cluster:deployment", partition="*")
automation deployWithArgs {
  args {
    environment string @required
    replicas    int
  }
  step gate {
    logic requireForwardDeploy { environment: args.environment }
  }
}`
	normalised, err := NormaliseAll(src)
	if err != nil {
		t.Fatalf("NormaliseAll error: %v", err)
	}
	toks, err := NewLexer(normalised).Tokenize()
	if err != nil {
		t.Fatalf("lex error: %v", err)
	}
	astNode, err := NewParser(toks).Parse()
	if err != nil {
		t.Fatalf("parse error: %v\nsource:\n%s", err, normalised)
	}
	file, ok := astNode.(*File)
	if !ok {
		t.Fatalf("expected *File, got %T", astNode)
	}
	var fn *FunctionDef
	for _, def := range file.Definitions {
		if f, ok := def.(*FunctionDef); ok && f.Name == "deployWithArgs" {
			fn = f
		}
	}
	if fn == nil {
		t.Fatalf("automation FunctionDef not found")
	}
	if fn.ArgsSchema == nil || len(fn.ArgsSchema.Fields) != 2 {
		t.Fatalf("expected 2 args fields on the automation, got %+v", fn.ArgsSchema)
	}
	if _, ok := fn.Body.(*AutomationDef); !ok {
		t.Fatalf("expected AutomationDef body, got %T", fn.Body)
	}
}

// The terse single-step form (`=> logic X`) declares NO args block. A file-top
// args block preceding a terse automation is rejected with a graduate-to-full-
// form hint (event-payload-binding ADR Decision 6).
func TestTerseAutomation_RejectsPrecedingArgsBlock(t *testing.T) {
	src := `args {
  environment string @required
}
@trigger(event="deploy.requested", concept="v1:cluster:deployment", partition="*")
automation deployTerse @trigger(event="deploy.requested") => logic handleDeploy`

	if _, err := NormaliseTerseAutomationSource(src); err == nil {
		t.Fatalf("expected a rejection for an args block preceding a terse automation")
	} else if !strings.Contains(err.Error(), "terse") {
		t.Errorf("error should explain the terse-form restriction, got: %v", err)
	}

	// It must also fail through the full chain.
	if _, err := NormaliseAll(src); err == nil {
		t.Fatalf("expected NormaliseAll to reject terse-form + args block")
	}
}

// A file-top args block preceding a FULL-form automation is legal (it binds
// normally) -- the rejection is terse-specific.
func TestFullAutomation_AllowsPrecedingArgsBlock(t *testing.T) {
	src := `args {
  environment string @required
}
@trigger(event="deploy.requested", concept="v1:cluster:deployment", partition="*")
automation deployFull {
  step gate { logic requireForwardDeploy { environment: args.environment } }
}`
	if _, err := NormaliseTerseAutomationSource(src); err != nil {
		t.Fatalf("full-form automation preceded by an args block must NOT be rejected: %v", err)
	}
	if _, err := NormaliseAll(src); err != nil {
		t.Fatalf("NormaliseAll on full-form automation + args block: %v", err)
	}
}
