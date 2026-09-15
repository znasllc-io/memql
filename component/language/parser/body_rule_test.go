package parser

import (
	"strings"
	"testing"
)

// Story 6 / memql#2327 -- enforcement of the body rule (construct-invocation
// ADR Decision 5): `body { }` is FORBIDDEN on every construct. On a logic and
// an automation it is the wrapper epic memql#5370 retired -- their statements
// follow the args block directly -- and the refusal names it. These tests pin
// the parser half of that enforcement (the whole-tree gate half lives in
// component/memql/callgraph).

// rewriteAndParse runs the struct-form rewriter (NormaliseAll) and then parses
// the result, mirroring the real loader pipeline. A query and a mutation reach
// the parser only after the rewriter expands their struct form, so a test that
// exercises the full path must rewrite first.
func rewriteAndParse(t *testing.T, src string) (*File, error) {
	t.Helper()
	rewritten, err := NormaliseAll(src)
	if err != nil {
		return nil, err
	}
	return ParseFile(rewritten)
}

// A logic's statements follow its args block directly (epic memql#5370; the
// owner's answer of 2026-09-13 retired the `body { }` wrapper, and with it the
// logic half of ADR Decision 5). The parser reads them as written. `body { }`
// is forbidden on every construct; on a logic and an automation it is the
// retired form, refused by name (body_block_retired).
func TestBodyRule_LogicWithoutBodyIsTheStatementForm(t *testing.T) {
	src := `/// no wrapper
logic decideThing {
  args {
    x string!
  }
  return args.x
}`
	file, err := rewriteAndParse(t, src)
	if err != nil {
		t.Fatalf("a statement-form logic should parse cleanly, got: %v", err)
	}
	fn, ok := file.Definitions[0].(*FunctionDef)
	if !ok || fn.Type != FunctionTypeLogic {
		t.Fatalf("definition = %#v, want a logic FunctionDef", file.Definitions[0])
	}
	auto, ok := fn.Body.(*AutomationDef)
	if !ok || auto.Body == nil || len(auto.Body.Statements) != 1 {
		t.Fatalf("the logic's body was not read as statements: %#v", fn.Body)
	}
}

// A spec carrying a `body { }` block is refused. Edition 2026 has no braced
// spec at all -- a spec is `spec <bound> <name> = row => <predicate>` -- so the
// refusal is the retired braced form's, at the `{` the author wrote, naming
// the spelling that replaces it and the migrator that writes it.
func TestBodyRule_SpecWithBody_Rejected(t *testing.T) {
	// memqlmigrate:keep -- the braced body is the case.
	src := `use cognition.concepts.{ participant }
@description("bad spec")
spec participant specIsHuman {
  body {
    return participantType == "human"
  }
}`
	_, err := ParseFile(src)
	wantRetiredAt(t, err, ruleSpecReturnBody, src, "{\n  body", 1)
}

// A trait carrying a `body { }` block is refused for the same reason.
func TestBodyRule_TraitWithBody_Rejected(t *testing.T) {
	// memqlmigrate:keep -- the braced body is the case.
	src := `@description("bad trait")
trait isActiveRecord {
  body {
    return active == true
  }
}`
	_, err := ParseFile(src)
	wantRetiredAt(t, err, ruleTraitReturnBody, src, "{\n  body", 1)
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
mutation space mutateSpace {
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

// A logic or an automation carrying a `body { }` block is refused by name:
// its statements follow its args block directly.
func TestBodyRule_LogicAndAutomationWithBody_Rejected(t *testing.T) {
	for _, src := range []string{
		// memqlmigrate:keep -- the retired body wrapper is the case.
		"logic decideThing {\n  args {\n    x string!\n  }\n  body {\n    return args.x\n  }\n}",
		// memqlmigrate:keep -- the retired body wrapper is the case.
		"@trigger(event=\"system.startup\")\nautomation onStartup {\n  body {\n    run := logic doThing(event: event)\n  }\n}",
	} {
		_, err := rewriteAndParse(t, src)
		if err == nil || !strings.Contains(err.Error(), "body_block_retired") {
			t.Errorf("want the body_block_retired refusal, got %v:\n%s", err, src)
		}
	}
}
