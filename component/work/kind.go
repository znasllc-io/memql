package work

// kind.go -- a step's kind is DERIVED, not declared (design record
// docs/superpowers/specs/2026-09-05-work-spine-design.md, section B
// "Kind is derived, not declared").
//
// A call into a query, mutation, logic or builtin is deterministic UNLESS
// it transitively reaches a prompt, in which case it is reasoning. A spec
// call is a decision. The approval, feedback and wait step forms park the
// run. A sub-automation call opens a child run.
//
// WHY THE WALK IS TRANSITIVE. A logic function that calls another logic
// function that renders a prompt is a reasoning step, and nothing at the
// call site says so. Deriving from the step type alone would let a
// template advertise a replayable step that reaches a model on every run,
// which is the one claim the whole spine rests on.
//
// THE LOADER RULE IS THE ENFORCEMENT (ValidateDeclaredKind): a step
// ANNOTATED deterministic whose derivation says reasoning is REFUSED at
// load, and the refusal names the prompt, because "somewhere under here
// there is a model call" is not an error a person can act on.

import (
	"errors"
	"fmt"
	"strings"
)

// Kind is the derived step kind. The values are the closed enum
// v1:work:step.kind declares.
type Kind string

const (
	// KindUnset means "not derived yet"; the enum's empty member.
	KindUnset Kind = ""
	// KindDeterministic is a step that reaches no prompt. These are the reuse.
	KindDeterministic Kind = "deterministic"
	// KindReasoning is a step that reaches a prompt.
	KindReasoning Kind = "reasoning"
	// KindDecision is a spec call.
	KindDecision Kind = "decision"
	// KindHuman is a step that parks the run on a person or a wait.
	KindHuman Kind = "human"
	// KindLoop is a bounded agent loop under its own budget.
	KindLoop Kind = "loop"
	// KindSubrun is a step that opens a child run and parks.
	KindSubrun Kind = "subrun"
)

// Construct kinds the registry distinguishes.
const (
	ConstructQuery    = "query"
	ConstructMutation = "mutation"
	ConstructShape    = "shape"
	ConstructSpec     = "spec"
	ConstructLogic    = "logic"
	ConstructBuiltin  = "builtin"
	ConstructPrompt   = "prompt"
)

// Target is one construct in the call graph.
type Target struct {
	// ConstructKind is one of the Construct* constants.
	ConstructKind string
	// Calls names the constructs this one invokes, by name. The walk is
	// over names rather than pointers so a registry can be built from the
	// loaded DSL or from a test literal with equal ease.
	Calls []string
	// Concept is the concept a mutation writes -- its whole effect, and
	// the source of its free postcondition.
	Concept string
	// Effects is what a builtin declares with @effects. Meaningless on a
	// query or a mutation, whose effects are structural.
	Effects Footprint
}

// Registry is the closed lookup the derivation walks. A name that is
// absent is treated as reaching no prompt: an unresolvable call is a
// different error, raised by the loader's own resolution pass, and
// guessing "reasoning" here would refuse honest deterministic steps.
type Registry map[string]Target

// ErrDeterministicReachesPrompt is the loader's refusal.
var ErrDeterministicReachesPrompt = errors.New("a step declared deterministic reaches a prompt")

// ReachesPrompt reports whether name transitively reaches a prompt, and
// returns the path that got there so an error can name it. Cycle-safe:
// a construct already on the current path is not re-entered, so a mutual
// recursion terminates and is not mistaken for a prompt.
func ReachesPrompt(name string, reg Registry) (bool, []string) {
	seen := map[string]bool{}
	var walk func(string, []string) (bool, []string)
	walk = func(n string, path []string) (bool, []string) {
		if seen[n] {
			return false, nil
		}
		seen[n] = true
		t, ok := reg[n]
		if !ok {
			return false, nil
		}
		path = append(path, n)
		if t.ConstructKind == ConstructPrompt {
			return true, path
		}
		for _, c := range t.Calls {
			if hit, p := walk(c, path); hit {
				return true, p
			}
		}
		return false, nil
	}
	return walk(name, nil)
}

// DeriveKind answers the kind for one step. stepType is the automation
// step type; target is the construct it calls, empty for the structural
// types that call nothing by name.
func DeriveKind(stepType, target string, reg Registry) (Kind, error) {
	switch stepType {
	case "approval", "feedback", "wait":
		// All three park the run and resume on a row rather than a
		// process being held open.
		return KindHuman, nil
	case "automation":
		return KindSubrun, nil
	case "loop":
		return KindLoop, nil
	case "spec":
		return KindDecision, nil
	}
	if target != "" {
		if t, ok := reg[target]; ok && t.ConstructKind == ConstructSpec {
			return KindDecision, nil
		}
		if reached, _ := ReachesPrompt(target, reg); reached {
			return KindReasoning, nil
		}
	}
	switch stepType {
	case "query", "mutation", "shape", "function", "action", "forEach", "parallel", "switch",
		"webhook", "event", "detectLeadSignal", "emitConceptCard":
		return KindDeterministic, nil
	}
	return KindUnset, fmt.Errorf("work: unknown step type %q", stepType)
}

// ValidateDeclaredKind is the loader rule. An UNDECLARED kind is derived
// and never refused; a declared one must agree with the derivation in the
// direction that matters -- claiming deterministic while reaching a
// prompt is the lie the spine cannot tolerate.
func ValidateDeclaredKind(declared, derived Kind, target string, promptPath []string) error {
	if declared == KindUnset || declared == derived {
		return nil
	}
	if declared == KindDeterministic && derived == KindReasoning {
		via := ""
		if len(promptPath) > 0 {
			via = " via " + strings.Join(promptPath, " -> ")
		}
		return fmt.Errorf("%w: %q%s -- a deterministic step is the reuse, and a step that calls a model on every run is not reuse; declare it reasoning or move the prompt out of its call graph", ErrDeterministicReachesPrompt, target, via)
	}
	return fmt.Errorf("work: step %q declares kind %q but derives %q", target, declared, derived)
}
