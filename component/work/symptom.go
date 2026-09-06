package work

// symptom.go -- the symptom classifier (design record
// docs/superpowers/specs/2026-09-05-work-spine-design.md, section E "Miss").
//
// On a failure, a precondition miss, a postcondition failure or a stalled
// loop, DETERMINISTIC RULES RUN FIRST and one cheap model call
// (classifySymptom) runs only when they have no opinion. That order is the
// whole point: the spec's headline test is that a symptom the rules
// classified makes ZERO provider calls, so ClassifyByRules returning
// ok=false is the ONLY thing that costs money here.
//
// The rules table is deliberately conservative. A rule that guesses is
// worse than no rule: an unclassifiable failure falls through to a model
// that reads the trace, whereas a WRONG rule acts confidently -- retrying
// a contract violation forever, or asking a person about a blip. So every
// matcher below is a signal that means one thing, and anything ambiguous
// is left to the model.
//
// Repair beats resample (SymTrace, section "What the research says"):
// the act for a contract miss is repair FROM THE FAILED STEP with the
// prefix kept, never a rerun from the start.

import "strings"

// Symptom is the classifier's verdict. The values are the closed enum
// v1:work:step.symptom declares; a sixth would need the concept changed.
type Symptom string

const (
	// SymptomNone is "not classified", the enum's empty member.
	SymptomNone Symptom = ""
	// SymptomTransient is a blip: the same call would probably work again.
	SymptomTransient Symptom = "transient"
	// SymptomEnvironment is the world disagreeing with the template: a
	// permission, a missing thing, a literal that does not hold here.
	SymptomEnvironment Symptom = "environment"
	// SymptomContract is a violated postcondition -- the step did
	// something, and it was not what it promised.
	SymptomContract Symptom = "contract"
	// SymptomPlan is the plan being wrong from here on.
	SymptomPlan Symptom = "plan"
	// SymptomHuman is a decision the system must not take alone.
	SymptomHuman Symptom = "human"
)

// AllSymptoms returns the closed set, in the concept's declaration order.
func AllSymptoms() []Symptom {
	return []Symptom{SymptomNone, SymptomTransient, SymptomEnvironment, SymptomContract, SymptomPlan, SymptomHuman}
}

// Valid reports whether s is a member of the closed set.
func (s Symptom) Valid() bool {
	for _, k := range AllSymptoms() {
		if k == s {
			return true
		}
	}
	return false
}

// Act is what the loop does about a symptom (spec section E, the
// five-row table). One act per symptom, with the single budget-dependent
// branch on transient.
type Act string

const (
	// ActRetry re-runs the step with backoff, inside the run-wide budget.
	ActRetry Act = "retry"
	// ActHeal hands the step to the healing loop, whose typed patches
	// reach a person as a planReview approval. Never a silent edit (D5).
	ActHeal Act = "heal"
	// ActRepair re-runs the failed step's reasoning with the violation as
	// guidance, bounded, keeping the prefix.
	ActRepair Act = "repair"
	// ActReplan emits the remaining steps as a new template version from
	// this step on (replanGap), keeping the prefix.
	ActReplan Act = "replan"
	// ActAsk parks the run on an approval and waits for a person.
	ActAsk Act = "ask"
)

// Approval kinds this file decides. The full set lives on
// v1:work:approval.kind; these are the two the miss path raises.
const (
	ApprovalKindPlanReview = "planReview"
	ApprovalKindFeedback   = "feedback"
)

// Evidence sources. A verdict always says where it came from, because
// "the rules decided" and "a model decided" have different costs and
// different trust, and a reader of the row must be able to tell.
const (
	EvidenceSourceRules = "rules"
	EvidenceSourceModel = "model"
)

// Evidence is the reasoned half of a verdict; it lands on
// v1:work:approval.evidence and in the step's log line.
type Evidence struct {
	// Tier is the severity band a governance reader keys on.
	Tier string
	// Reason is the verdict in words a person can read.
	Reason string
	// RuleId names the rule that fired, or is empty for a model verdict.
	RuleId string
	// Source is EvidenceSourceRules or EvidenceSourceModel.
	Source string
}

// Signal is everything the rules may read. It is deliberately a value:
// the classifier must be callable from a test with no engine, which is
// what makes "a rules-classified symptom makes zero provider calls"
// provable without a database.
type Signal struct {
	// ErrorCode is the catalogued code, when the failure had one.
	ErrorCode string
	// ErrorMessage is the failure in words.
	ErrorMessage string
	// StepType is the automation step type that failed.
	StepType string
	// PostconditionFailed is set when the step ran and its postcondition
	// did not hold.
	PostconditionFailed bool
	// PreconditionFailed is set when the step did not run because a
	// declared precondition missed.
	PreconditionFailed bool
	// RepeatedAction is set when this step has already been attempted
	// with the same input and the same result -- the stall signal.
	RepeatedAction bool
	// Attempt is 1 on the first execution.
	Attempt int
	// MaxRetries is the run-wide retry budget.
	MaxRetries int
}

// rule is one row of the table.
type rule struct {
	id      string
	tier    string
	symptom Symptom
	reason  string
	match   func(Signal) bool
}

// anyOf reports whether haystack (lower-cased once by the caller)
// contains any needle.
func anyOf(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// rules is evaluated in order, first match wins. ORDER IS LOAD-BEARING:
// the stall rule sits at the top because a repeated action that also
// looks transient must escalate rather than retry forever, and the
// contract rule sits above the message matchers because a postcondition
// failure is a fact about the step rather than a guess about its text.
var rules = []rule{
	{
		id: "human.stalled", tier: "escalate", symptom: SymptomHuman,
		reason: "the same action repeated with the same result; retrying it again is the loop the budget exists to stop",
		match:  func(s Signal) bool { return s.RepeatedAction },
	},
	{
		id: "contract.postcondition", tier: "contract", symptom: SymptomContract,
		reason: "the step ran and its postcondition did not hold",
		match:  func(s Signal) bool { return s.PostconditionFailed },
	},
	{
		id: "transient.rateLimit", tier: "retryable", symptom: SymptomTransient,
		reason: "the far side rate-limited the call",
		match: func(s Signal) bool {
			return s.ErrorCode == "rate_limited" || anyOf(lower(s.ErrorMessage), "rate limit", "too many requests", "429")
		},
	},
	{
		id: "transient.timeout", tier: "retryable", symptom: SymptomTransient,
		reason: "the call did not answer in time",
		match: func(s Signal) bool {
			return s.ErrorCode == "timeout" || anyOf(lower(s.ErrorMessage), "deadline exceeded", "timeout", "timed out", "i/o timeout")
		},
	},
	{
		id: "transient.unavailable", tier: "retryable", symptom: SymptomTransient,
		reason: "the far side reported itself temporarily unavailable",
		match: func(s Signal) bool {
			return anyOf(lower(s.ErrorMessage), "service unavailable", "503", "502 bad gateway", "temporarily unavailable")
		},
	},
	{
		id: "transient.network", tier: "retryable", symptom: SymptomTransient,
		reason: "the connection failed below the application",
		match: func(s Signal) bool {
			return anyOf(lower(s.ErrorMessage), "connection refused", "connection reset", "no such host", "broken pipe", "eof", "network is unreachable")
		},
	},
	{
		id: "environment.permission", tier: "environment", symptom: SymptomEnvironment,
		reason: "the actor is not allowed to do this here",
		match: func(s Signal) bool {
			return s.ErrorCode == "forbidden" || s.ErrorCode == "permission_denied" ||
				anyOf(lower(s.ErrorMessage), "permission denied", "forbidden", "not authorized", "unauthorized", "access denied", "403")
		},
	},
	{
		id: "environment.notFound", tier: "environment", symptom: SymptomEnvironment,
		reason: "something the template names is not here",
		match: func(s Signal) bool {
			return s.ErrorCode == "not_found" ||
				anyOf(lower(s.ErrorMessage), "no such file or directory", "not found", "404", "does not exist")
		},
	},
	{
		id: "environment.literal", tier: "environment", symptom: SymptomEnvironment,
		reason: "a declared precondition did not hold on this machine",
		match:  func(s Signal) bool { return s.PreconditionFailed },
	},
}

func lower(s string) string { return strings.ToLower(s) }

// ClassifyByRules runs the deterministic table. ok=false means the rules
// have no opinion and the caller should make the one cheap classifySymptom
// call -- it is the ONLY path here that reaches a provider.
func ClassifyByRules(s Signal) (Symptom, Evidence, bool) {
	for _, r := range rules {
		if r.match(s) {
			return r.symptom, Evidence{
				Tier:   r.tier,
				Reason: r.reason,
				RuleId: r.id,
				Source: EvidenceSourceRules,
			}, true
		}
	}
	return SymptomNone, Evidence{}, false
}

// ActFor maps a symptom onto its act (spec section E). The one branch is
// transient: it retries INSIDE the run-wide retry budget and becomes a
// question for a person past it, because a blip that will not stop being
// a blip is not a blip.
func ActFor(sym Symptom, attempt, maxRetries int) Act {
	switch sym {
	case SymptomTransient:
		if attempt < maxRetries {
			return ActRetry
		}
		return ActAsk
	case SymptomEnvironment:
		return ActHeal
	case SymptomContract:
		return ActRepair
	case SymptomPlan:
		return ActReplan
	default:
		// SymptomHuman, and anything unrecognised. An unknown symptom
		// asks a person rather than acting on a guess: of the five acts
		// this is the only one that cannot make things worse.
		return ActAsk
	}
}

// ApprovalKindFor names the v1:work:approval kind an act raises, or ""
// for an act that asks nobody. Healing is planReview because D5 forbids a
// silent edit even to the run's own draft template.
func ApprovalKindFor(a Act) string {
	switch a {
	case ActHeal:
		return ApprovalKindPlanReview
	case ActAsk:
		return ApprovalKindFeedback
	default:
		return ""
	}
}
