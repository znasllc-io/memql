package memql

import (
	"strings"
	"testing"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// #2621: used-requires-declared for the auth envelope, across all four
// function kinds; declared-but-unused stays legal.
func TestValidateActorBinding(t *testing.T) {
	fd := func(kind languageParser.FunctionType, name string) *languageParser.FunctionDef {
		return &languageParser.FunctionDef{Name: name, Type: kind}
	}

	undeclared := `@description("owned")
func (Query) todos(_ any) {
  return concept==v1:todos:todo AND ownerUserId == actor.userId, nil
}`
	if err := validateActorBinding(undeclared, fd(languageParser.FunctionTypeQuery, "todos")); err == nil {
		t.Error("query reading actor.* without @actor must fail load")
	} else if !strings.Contains(err.Error(), "@actor") || !strings.Contains(err.Error(), "todos") {
		t.Errorf("error must name the construct and the fix: %v", err)
	}

	declared := "@actor\n" + undeclared
	if err := validateActorBinding(declared, fd(languageParser.FunctionTypeQuery, "todos")); err != nil {
		t.Errorf("declared @actor must load: %v", err)
	}

	unused := `@actor
func (Query) allTodos(_ any) {
  return concept==v1:todos:todo, nil
}`
	if err := validateActorBinding(unused, fd(languageParser.FunctionTypeQuery, "allTodos")); err != nil {
		t.Errorf("declared-but-unused must load: %v", err)
	}

	proseOnly := `func (Query) ranked(_ any) {
  // ordered by actor.rank
  return concept==v1:todos:todo, nil
}`
	if err := validateActorBinding(proseOnly, fd(languageParser.FunctionTypeQuery, "ranked")); err != nil {
		t.Errorf("comment prose must not count as a read: %v", err)
	}

	eventEnvelope := `func (Logic) onThing(_ any) {
  check := cond(event.actor.id == "x", 1, 2)
  return check, nil
}`
	if err := validateActorBinding(eventEnvelope, fd(languageParser.FunctionTypeLogic, "onThing")); err != nil {
		t.Errorf("event.actor.* is the event envelope, not the auth envelope: %v", err)
	}

	for _, kind := range []languageParser.FunctionType{
		languageParser.FunctionTypeMutation,
		languageParser.FunctionTypeLogic,
		languageParser.FunctionTypeAutomation,
	} {
		src := `func (X) probe(_ any) {
  owner := actor.userId
  return owner, nil
}`
		if err := validateActorBinding(src, fd(kind, "probe")); err == nil {
			t.Errorf("%v reading actor.* without @actor must fail load", kind)
		}
	}
}
