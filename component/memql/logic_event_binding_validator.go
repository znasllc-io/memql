package memql

import (
	"fmt"
	"regexp"

	languageParser "github.com/znasllc-io/memql/component/language/parser"
)

// validateLogicEventBinding enforces first-class event-context binding for
// Logic functions (memql#1706).
//
// An event-triggered Logic reads the triggering event through its declared
// `event` input -- e.g. `args.event.payload.partitionId`. The LogicRunner seeds
// that input into the per-step argument-resolution scope ONLY when the caller
// passes an `event` arg, which in turn only happens reliably when the Logic
// DECLARES `event` in its `args { }` block (the automation step that fires it
// passes `logic name { event: event }`, and the engine validates the call
// against the declared args schema).
//
// When a Logic body references the event WITHOUT declaring `event` as an
// input, nothing threads the triggering event into scope: every
// `args.event.*` reference resolves to empty/undefined inside the nested
// steps, and the first step that feeds one of those references to a
// @required argument fails at RUNTIME with "required argument <x> is
// missing" (the memql#1706 failure shape across logicAutoJoinAI,
// conflictDetection, generateResponse,
// logicEnsureDailySpaceOnAuthSession, voiceMigrationOnSecondHuman).
//
// This validator turns that latent runtime failure into a COMPILE-TIME
// rejection: a Logic whose body references the triggering event MUST declare
// a typed `event` field in its args block. The declared `event` field is the
// per-logic event schema -- the in-scope, validated contract the runner binds
// from. It is the inverse of the declared-usage validator's Logic exemption
// (declared-but-unused `event` is allowed for cron logics; used-but-undeclared
// `event` is forbidden for every logic).
//
// Scope: Logic functions only. Runs on the RAW source (pre-path-translation)
// so author-written `args.event.*` / `event.*` references pass through
// unchanged. Zero false-rejects across the live DSL tree -- every
// event-referencing logic already declares `event`.
func validateLogicEventBinding(rawSource string, funcDef *languageParser.FunctionDef) error {
	if funcDef == nil || funcDef.Type != languageParser.FunctionTypeLogic {
		return nil
	}

	body := extractFunctionBody(rawSource)
	if body == "" {
		return nil
	}
	// Strip annotation lines so an `@description("... event ...")` doc string
	// can't be mistaken for a body reference to the event.
	bodyNoAnnotations := stripAttrLines(body)

	if !referencesTriggeringEvent(bodyNoAnnotations) {
		return nil
	}

	if logicDeclaresEventInput(funcDef) {
		return nil
	}

	return fmt.Errorf(
		"logic %q references the triggering event (args.event.* / event.*) but does not declare an `event` input in its args block; "+
			"add `args { event object @required }` so the event is a first-class, in-scope value the runner threads into every step's argument resolution (memql#1706)",
		funcDef.Name,
	)
}

// triggeringEventRefPattern matches a body reference that READS the triggering
// event: the canonical `args.event.<field>` form authors write, or the bare
// `event.<field>` form (both lift to the runner-seeded `event` binding at
// compile time). Anchored on a following `.` so a step variable that merely
// ends in `event` (e.g. `lastEvent`) or a bare `event` used as nothing does
// not trip the check; `\bargs\.event\b` additionally accepts `args.event`
// passed as a whole object (e.g. `foo({ payload: args.event })`).
var triggeringEventRefPattern = regexp.MustCompile(`\bargs\.event\b|\bevent\.`)

// referencesTriggeringEvent reports whether body reads the triggering event.
func referencesTriggeringEvent(body string) bool {
	return triggeringEventRefPattern.MatchString(body)
}

// logicDeclaresEventInput reports whether the function declares a field named
// `event` in its args block.
func logicDeclaresEventInput(funcDef *languageParser.FunctionDef) bool {
	if funcDef.ArgsSchema == nil {
		return false
	}
	for _, field := range funcDef.ArgsSchema.Fields {
		if field != nil && field.Name == "event" {
			return true
		}
	}
	return false
}
