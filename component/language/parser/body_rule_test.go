package parser

import (
	"strings"
	"testing"
)

// Story 6 / memql#2327 -- enforcement of the body rule (construct-invocation
// ADR Decision 5): `body { }` is the procedural marker, MANDATORY on `logic`
// and FORBIDDEN on every other construct. These tests pin the parser half of
// that enforcement (the whole-tree gate half lives in
// component/memql/callgraph).

// rewriteAndParse runs the struct-form rewriter (NormaliseAll) and then parses
// the result, mirroring the real loader pipeline. Logic/query/mutation/
// automation reach the parser only after the rewriter expands their struct
// form, so a test that exercises the full path must rewrite first.
func rewriteAndParse(t *testing.T, src string) (*File, error) {
	t.Helper()
	rewritten, err := NormaliseAll(src)
	if err != nil {
		return nil, err
	}
	return ParseFile(rewritten)
}

// A logic WITHOUT a `body { }` block is rejected -- body is mandatory on logic.
func TestBodyRule_LogicWithoutBody_Rejected(t *testing.T) {
	src := `@description("missing body")
logic decideThing {
  args {
    x string @required
  }
  return x
}`
	_, err := NormaliseLogicSource(src)
	if err == nil {
		t.Fatal("expected a parse error for a logic without a `body { }` block, got nil")
	}
	if !strings.Contains(err.Error(), "body") || !strings.Contains(err.Error(), "Decision 5") {
		t.Errorf("error %q should name the missing `body` block and ADR Decision 5", err.Error())
	}
}

// A logic WITH a `body { }` block (even a one-liner) is accepted and parses to
// a FunctionDef of logic kind.
func TestBodyRule_LogicWithBody_Accepted(t *testing.T) {
	src := `@description("has body")
logic decideThing {
  args {
    x string @required
  }
  body {
    return x
  }
}`
	file, err := rewriteAndParse(t, src)
	if err != nil {
		t.Fatalf("logic with a body block should parse cleanly, got: %v", err)
	}
	if len(file.Definitions) != 1 {
		t.Fatalf("expected 1 definition, got %d", len(file.Definitions))
	}
	fn, ok := file.Definitions[0].(*FunctionDef)
	if !ok {
		t.Fatalf("expected *FunctionDef, got %T", file.Definitions[0])
	}
	if fn.Type != FunctionTypeLogic {
		t.Errorf("Type = %v, want logic", fn.Type)
	}
	if fn.Name != "decideThing" {
		t.Errorf("Name = %q, want decideThing", fn.Name)
	}
}

// A spec carrying a `body { }` block is rejected: a spec is a bare
// `return <expr>`, never a procedural body.
func TestBodyRule_SpecWithBody_Rejected(t *testing.T) {
	src := `use cognition.concepts.{ participant }
@description("bad spec")
spec participant specIsHuman {
  body {
    return participantType == "human"
  }
}`
	_, err := ParseFile(src)
	if err == nil {
		t.Fatal("expected a parse error for a spec with a `body { }` block, got nil")
	}
	if !strings.Contains(err.Error(), "body") {
		t.Errorf("error %q should mention the rejected `body` block", err.Error())
	}
}

// A trait carrying a `body { }` block is rejected for the same reason.
func TestBodyRule_TraitWithBody_Rejected(t *testing.T) {
	src := `@description("bad trait")
trait isActiveRecord {
  body {
    return active == true
  }
}`
	_, err := ParseFile(src)
	if err == nil {
		t.Fatal("expected a parse error for a trait with a `body { }` block, got nil")
	}
	if !strings.Contains(err.Error(), "body") {
		t.Errorf("error %q should mention the rejected `body` block", err.Error())
	}
}

// A query carrying a `body { }` block is rejected: a query is declarative
// clauses, never a procedural body.
func TestBodyRule_QueryWithBody_Rejected(t *testing.T) {
	src := `use cognition.concepts.{ participant }
@description("bad query")
query participant queryParticipants {
  args {
    spaceId string @required
  }
  body {
    return spaceId
  }
}`
	_, err := NormaliseQuerySource(src)
	if err == nil {
		t.Fatal("expected a rewrite error for a query with a `body { }` block, got nil")
	}
	if !strings.Contains(err.Error(), "body") {
		t.Errorf("error %q should mention the rejected `body` block", err.Error())
	}
}

// A mutation carrying a `body { }` block is rejected: a mutation is a
// declarative insert/update block, never a procedural body.
func TestBodyRule_MutationWithBody_Rejected(t *testing.T) {
	src := `use cognition.concepts.{ space }
@description("bad mutation")
mutate space mutateSpace {
  args {
    spaceId string @required
  }
  body {
    insert { id: args.spaceId }
  }
}`
	_, err := NormaliseMutationSource(src)
	if err == nil {
		t.Fatal("expected a rewrite error for a mutation with a `body { }` block, got nil")
	}
	if !strings.Contains(err.Error(), "body") {
		t.Errorf("error %q should mention the rejected `body` block", err.Error())
	}
}

// An action carrying a `body { }` block is rejected: an action is a single
// `capability ...(...)` call, never a procedural body.
func TestBodyRule_ActionWithBody_Rejected(t *testing.T) {
	src := `@description("bad action")
action tagRelease {
  body {
    return 1
  }
}`
	_, err := ParseActionDecl(src)
	if err == nil {
		t.Fatal("expected a parse error for an action with a `body { }` block, got nil")
	}
}

// An automation carrying a `body { }` block is rejected: an automation is
// `step ...` blocks, never a procedural body.
func TestBodyRule_AutomationWithBody_Rejected(t *testing.T) {
	src := `@enabled
@trigger(event="system.startup")
automation onStartup {
  body {
    step run { logic doThing { event: event } }
  }
}`
	_, err := rewriteAndParse(t, src)
	if err == nil {
		t.Fatal("expected an error for an automation with a `body { }` block, got nil")
	}
}
