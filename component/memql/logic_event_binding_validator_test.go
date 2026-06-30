package memql

import (
	"testing"

	"github.com/stretchr/testify/require"

	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// Tests for the first-class event-context binding validator (memql#1706).
//
// An event-triggered Logic that reads the triggering event MUST declare an
// `event` input in its args block, so the LogicRunner threads the event into
// every nested step's argument-resolution scope. A Logic that references the
// event WITHOUT declaring it is the structural root of the #1706 runtime
// failure ("required argument <x> is missing" at the first step that feeds an
// `args.event.*` reference): nothing seeds the event, so the references
// resolve to empty inside the steps. This validator rejects that shape at
// LOAD time instead of letting it fail at runtime.

func eventBindingRegistry() memoryNodes.Registry {
	return newMemoryRegistry(map[string]*memoryNodes.Concept{
		"v1:cognition:space": {Name: "v1:cognition:space"},
	})
}

// FAIL-before / PASS-after: a Logic that reads args.event.* but declares no
// `event` input is rejected at load time. Before #1706 this loaded cleanly and
// failed only at runtime inside the nested step.
func TestLogicEventBinding_RejectsUsedButUndeclaredEvent(t *testing.T) {
	src := `@description("reads the event but never declares it")
logic logicUsesEventNoDecl {
  body {
    return ensureDailySpaceForUser( userId: args.event.payload.id )
  }
}`
	_, err := tryParseNewFunctionSyntax("logicUsesEventNoDecl", "logic", src, "test.memql", eventBindingRegistry())
	require.Error(t, err)
	require.Contains(t, err.Error(), "logicUsesEventNoDecl")
	require.Contains(t, err.Error(), "event")
	require.Contains(t, err.Error(), "1706")
}

// The bare `event.<field>` form (not prefixed with `args.`) is caught too.
func TestLogicEventBinding_RejectsBareEventRefWithoutDecl(t *testing.T) {
	src := `@description("bare event.* form, still undeclared")
logic logicBareEventNoDecl {
  body {
    return ensureDailySpaceForUser( userId: event.payload.id )
  }
}`
	_, err := tryParseNewFunctionSyntax("logicBareEventNoDecl", "logic", src, "test.memql", eventBindingRegistry())
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not declare an `event` input")
}

// The same body WITH a declared `event` input loads cleanly -- this is the
// shape every live event-trigger logic uses.
func TestLogicEventBinding_AcceptsDeclaredEvent(t *testing.T) {
	src := `@description("reads the event and declares it")
logic logicUsesEventDecl {
  args {
    event object @required
  }
  body {
    return ensureDailySpaceForUser( userId: args.event.payload.id )
  }
}`
	_, err := tryParseNewFunctionSyntax("logicUsesEventDecl", "logic", src, "test.memql", eventBindingRegistry())
	require.NoError(t, err)
}

// A cron/schedule Logic that declares `event` (the uniform convention) but
// never reads it stays valid -- the declared-but-unused Logic exemption. The
// validator only forbids the inverse (used-but-undeclared).
func TestLogicEventBinding_AcceptsDeclaredButUnusedEvent(t *testing.T) {
	src := `@description("cron logic: declares event, ignores it")
logic logicCronNoEventRead {
  args {
    event object @required
  }
  body {
    return 1
  }
}`
	_, err := tryParseNewFunctionSyntax("logicCronNoEventRead", "logic", src, "test.memql", eventBindingRegistry())
	require.NoError(t, err)
}

// A Logic that neither declares nor reads the event is unaffected.
func TestLogicEventBinding_AcceptsNoEventAtAll(t *testing.T) {
	src := `@description("interactive logic: no event involved")
logic logicNoEventAtAll {
  body {
    return ensureDailySpaceForCaller()
  }
}`
	_, err := tryParseNewFunctionSyntax("logicNoEventAtAll", "logic", src, "test.memql", eventBindingRegistry())
	require.NoError(t, err)
}
