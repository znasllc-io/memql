package router

import (
	"sort"
	"strings"
)

// Simulating a described rule before it is activated (epic memql#5137, D8).
//
// ===========================================================================
// THIS IS WHAT MAKES A DESCRIBED RULE TRUSTWORTHY
// ===========================================================================
// A person types a sentence and a model turns it into a rule. The compiled form
// is shown beside the sentence, which helps somebody who reads DSL and does
// nothing at all for somebody who does not. The simulation is for them: it
// replays the rule over calls the cluster ACTUALLY MADE and says what would
// have been different.
//
// A rule that matches nothing is the common wrong answer -- the compiler
// produced something narrower than the person meant -- and it is invisible in
// the compiled form and obvious here.
//
// ===========================================================================
// OVER REAL RECORDS, NOT SYNTHETIC ONES
// ===========================================================================
// The last two hundred `v1:router:call` rows. Two hundred rather than all of
// them because a simulation nobody waits for is a simulation nobody runs, and
// because the recent ones are the ones whose traffic the person recognises.
//
// A cluster with no decision records yet gets an honest empty answer rather
// than a reassuring one: "this rule would have changed nothing" and "there was
// nothing to change" are different sentences, and only the second is true.

// DecisionRecord is the part of a `v1:router:call` row a simulation reads.
//
// A PROJECTION RATHER THAN THE ROW, so the simulator is a pure function over
// values and testable on a fixture. It also means this file names no concept
// and imports no engine: the thing being tested is the matching, not the read.
type DecisionRecord struct {
	CallId string
	// Level the call declared.
	Level string
	// Modality derived from the call.
	Modality string
	// PromptName, empty for a free-form request.
	PromptName string
	// Role of the acting agent, when there was one.
	Role string
	// ActorRole is the role of the person the call was made for.
	ActorRole string
	// Tags the call carried.
	Tags []string
	// Touches names the concepts the call's data came from.
	Touches []string
	// Rule that actually decided, empty before epic memql#5127 filled it.
	Rule string
	// Policy that actually decided.
	Policy string
	// Door the call went through: local | app | federation.
	Door string
	// Model that answered.
	Model string
}

// SimulationChange is one call the rule would have decided differently.
type SimulationChange struct {
	CallId string
	// WasPolicy and WouldBePolicy are the before and after.
	WasPolicy     string
	WouldBePolicy string
	// WasDoor is where the call actually went. There is deliberately no
	// "WouldBeDoor": which door a policy resolves to depends on which machines
	// are awake, and claiming to know that for a call made yesterday would be
	// the most confident thing on the screen and the least true.
	WasDoor string
}

// SimulationResult is what a person sees before confirming a rule.
type SimulationResult struct {
	// Considered is how many records were replayed. ZERO IS A REAL ANSWER and
	// the surface must say so rather than rendering "no changes".
	Considered int
	// Matched is how many the rule's conditions selected.
	Matched int
	// Changes are the calls whose deciding policy would differ. A subset of
	// Matched: a rule can match a call that was already going where it says.
	Changes []SimulationChange
	// AlreadyAgreed is Matched minus len(Changes), named rather than left to
	// arithmetic because it is the reassuring half of the answer -- "this rule
	// agrees with what you were already doing forty times" is worth saying.
	AlreadyAgreed int
}

// matchesConditions reports whether a compiled rule's `@when` conditions select
// a record.
//
// EVERY PRESENT KEY MUST MATCH; an absent key is no condition. That is epic
// memql#5127's rule and it is restated here rather than imported because the
// simulation must agree with the router exactly -- a simulation that matched
// more liberally than the runtime would promise changes that never happen, and
// one that matched more strictly would hide them.
//
// AN EMPTY-STRING VALUE IS A CONDITION, not an absence: it matches only a
// record whose field is empty. That distinction is what lets a rule say
// "free-form requests" (prompt="") rather than "any request".
func matchesConditions(conditions map[string]string, rec DecisionRecord) bool {
	for key, want := range conditions {
		var got string
		switch key {
		case "level":
			got = rec.Level
		case "modality":
			got = rec.Modality
		case "prompt":
			got = rec.PromptName
		case "role":
			got = rec.Role
		case "actorRole":
			got = rec.ActorRole
		case "tag":
			if !containsString(rec.Tags, want) {
				return false
			}
			continue
		case "touches":
			if !containsString(rec.Touches, want) {
				return false
			}
			continue
		default:
			// AN UNKNOWN KEY MATCHES NOTHING, deliberately. The `@when`
			// vocabulary is closed and the compiler is told what it is, so a key
			// outside it means the compiler invented one -- and a simulation
			// that ignored the key would show the person a rule matching
			// everything while the loader was about to refuse it.
			return false
		}
		if got != want {
			return false
		}
	}
	return true
}

func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// Simulate replays a compiled rule over decision records.
func Simulate(rule CompiledRule, records []DecisionRecord) SimulationResult {
	out := SimulationResult{Considered: len(records)}
	for _, rec := range records {
		if !matchesConditions(rule.Conditions, rec) {
			continue
		}
		out.Matched++
		if strings.TrimSpace(rec.Policy) == strings.TrimSpace(rule.Policy) {
			out.AlreadyAgreed++
			continue
		}
		out.Changes = append(out.Changes, SimulationChange{
			CallId:        rec.CallId,
			WasPolicy:     rec.Policy,
			WouldBePolicy: rule.Policy,
			WasDoor:       rec.Door,
		})
	}
	// Stable order so two runs over one fixture read the same, and so a person
	// comparing two simulations is comparing the rules rather than the sort.
	sort.SliceStable(out.Changes, func(i, j int) bool { return out.Changes[i].CallId < out.Changes[j].CallId })
	return out
}

// SimulationSentence is the one-line summary shown beside the compiled rule.
//
// IT NAMES THE EMPTY CASES SEPARATELY, because they are the two ways a
// described rule goes wrong and they look identical in a count:
//
//   - nothing to replay: the cluster has made no decisions yet, so the
//     simulation says nothing about the rule at all.
//   - matched nothing: the rule is real and selects none of the traffic, which
//     usually means the compiler produced something narrower than was meant.
func SimulationSentence(r SimulationResult) string {
	switch {
	case r.Considered == 0:
		return "No calls to replay yet, so this cannot be checked against anything."
	case r.Matched == 0:
		return "This rule matches none of the last " + itoa(r.Considered) + " calls. It may be narrower than you meant."
	case len(r.Changes) == 0:
		return "This rule matches " + itoa(r.Matched) + " of the last " + itoa(r.Considered) + " calls and agrees with all of them."
	case len(r.Changes) == r.Matched:
		return "This rule would have changed all " + itoa(r.Matched) + " of the calls it matches."
	default:
		return "This rule would have changed " + itoa(len(r.Changes)) + " of the " + itoa(r.Matched) + " calls it matches."
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
