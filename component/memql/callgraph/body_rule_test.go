package callgraph

import "testing"

// Story 6 / memql#2327 -- the whole-tree gate's half of the body rule
// (construct-invocation ADR Decision 5): `body { }` is FORBIDDEN on every
// (procedural) construct but logic. ConstructFindings mirrors the parser's
// enforcement so the conformance gate + authoring-sandbox cross-reference pass
// flag the same violation.

// A logic WITHOUT a `body { }` block is the edition-2026 statement form (epic
// memql#5370 retired the wrapper), and produces no body-rule finding.
func TestBodyRule_LogicWithoutBodyIsTheStatementForm(t *testing.T) {
	src := `logic decideThing {
  args { x string! }
  return args.x
}`
	fs := CheckFile("dsl/cluster/logic.memql", src, nil)
	if has(fs, "body-rule") {
		t.Fatalf("a statement-form logic must not be flagged; got %v", rules(fs))
	}
}

// A non-logic procedural construct WITH a `body { }` block is flagged. The
// whole-tree gate restricts to {logic, query, mutation, action}; a query is a
// representative non-logic procedural kind.
func TestBodyRule_QueryWithBodyFlagged(t *testing.T) {
	// memqlmigrate:keep -- the forbidden body block is the case.
	src := `query participant queryParticipants {
  args { spaceId string @required }
  body {
    return spaceId
  }
}`
	fs := CheckFile("dsl/cognition/queries.memql", src, nil)
	if !has(fs, "body-rule") {
		t.Fatalf("expected body-rule finding (query with a forbidden body block); got %v", rules(fs))
	}
}

// ConstructFindings is called directly for a spec (a non-restricted kind that
// flows through the authoring-sandbox cross-reference pass): a spec carrying a
// `body { }` block is flagged.
func TestBodyRule_SpecWithBodyFlagged(t *testing.T) {
	// memqlmigrate:keep -- the forbidden body block is the case.
	src := `spec participant specIsHuman {
  body {
    return participantType == "human"
  }
}`
	fs := ConstructFindings("spec", "specIsHuman", src, nil, nil)
	if !has(fs, "body-rule") {
		t.Fatalf("expected body-rule finding (spec with a forbidden body block); got %v", rules(fs))
	}
}

// A DECLARATIVE construct (concept) carrying a nested object field named
// `body` is NOT flagged -- a field named body is not the procedural marker.
func TestBodyRule_DeclarativeBodyFieldNotFlagged(t *testing.T) {
	// memqlmigrate:keep -- a concept field named body is the case, not a body block.
	src := `concept message {
  body {
    text string @required
  }
}`
	fs := ConstructFindings("concept", "message", src, nil, nil)
	if has(fs, "body-rule") {
		t.Fatalf("a declarative concept field named `body` must not be flagged; got %v", rules(fs))
	}
}
