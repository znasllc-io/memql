package work

// inference.go -- what a run does when no door to a model is open (epic
// memql#5096, task memql#5101, design D4 / D9).
//
// The local-models epic parked such a run as a `v1:planner:plan` with
// `feedbackReason = no_local_model_available`. Plans were retired (memql#5000)
// and that park lost its home. This is the replacement, on the spine that
// exists: one `v1:work:approval` of kind `inferenceUnavailable` on the run,
// naming every door considered and why each is shut.
//
// WHY IT IS NOT A FAILURE. A run that failed is a run somebody has to start
// again; a run that parked resumes where it stopped when the condition
// changes -- and the condition here changes on a human timescale for reasons
// that have nothing to do with the work: a laptop opens, somebody signs into
// Claude Code, a ceiling is raised. Failing it would throw away a compiled
// plan and a journal because a lid was shut.
//
// WHY IT IS NOT A RETRY EITHER. `transient` retries inside the step's budget,
// which is measured in seconds and attempts; a shut door is neither, and
// burning the retry budget against it means the run fails anyway, later, with
// a symptom that names the wrong thing.
//
// This file is PURE, like the rest of this package: values in, values out, no
// engine and no database. What writes the row is component/automations'
// journal, and what re-checks it is integrations/work's sweep.

import (
	"errors"
	"sort"
	"strings"
	"time"
)

// ApprovalKindInferenceUnavailable is the v1:work:approval kind for a run
// parked because no door to a model is open.
const ApprovalKindInferenceUnavailable = "inferenceUnavailable"

// The refusal codes an inference call can report. They are a CLOSED SET and
// they are STABLE STRINGS: the router puts one at the head of the error it
// returns, this package matches on it, and an operator reads the same word in
// a log line, on the approval row and in the park card. Three spellings of one
// condition is what this constant set exists to prevent.
const (
	// RefusalNoLocalModel is the fleet's: no eligible machine for the model
	// a policy named.
	RefusalNoLocalModel = "no_local_model_available"
	// RefusalNoApp is the app door's: no machine has the app allowed,
	// signed in and online on a stream this node holds or can forward to.
	RefusalNoApp = "no_app_available"
	// RefusalEveryDoorShut is the chain's: every entry was tried and none
	// could serve the call. This is the one that parks in the ordinary case.
	RefusalEveryDoorShut = "every_door_shut"
	// RefusalCeilingReached is the federation hop refused by the cost
	// ceiling. It is a REFUSAL rather than a park in the router, and it
	// parks here for a different reason from the others: the fix is a
	// person raising a limit, not a machine waking up.
	RefusalCeilingReached = "ceiling_reached"
)

// inferenceRefusalCodes is the matcher's set, longest first so
// `no_local_model_available` is not shadowed by a shorter prefix if one is
// ever added.
var inferenceRefusalCodes = []string{
	RefusalNoLocalModel,
	RefusalEveryDoorShut,
	RefusalCeilingReached,
	RefusalNoApp,
}

// InferenceRefusalCode reports which refusal an error message carries, if any.
//
// It matches on the MESSAGE rather than on a typed error because the two ends
// are in different modules: the router raises the refusal, and the executor
// that has to decide what the run does about it sees only the string the
// executor recorded. The codes are stable and are asserted at the raising end
// (component/router's tests pin that each refusal's Error() leads with its
// code), so this is a contract rather than a guess about wording.
//
// It is deliberately a CONTAINS rather than a prefix match: the message
// travels through wrapping ("automation step 3: ...") on the way here, and a
// prefix match would silently stop recognising the condition the first time a
// caller added context.
func InferenceRefusalCode(errorMessage string) (string, bool) {
	msg := strings.ToLower(errorMessage)
	if msg == "" {
		return "", false
	}
	for _, code := range inferenceRefusalCodes {
		if strings.Contains(msg, code) {
			return code, true
		}
	}
	return "", false
}

// InferenceRetryInterval is how long a parked run waits before the sweep hands
// it back to the cluster to try again.
//
// FIVE MINUTES IS A GUESS ABOUT PEOPLE, not about machines, and it is the
// honest one: the conditions that open a door -- a lid opening, a sign-in, a
// model finishing a pull -- happen on that scale. Polling faster would burn
// dispatches against a condition that has not moved; slower would leave
// somebody who just woke their laptop staring at a parked run.
//
// It is a POLL because the event that would replace it does not exist yet: the
// module-readiness feed (`v1:platform:moduleReadiness`) is epic 1's, and
// subscribing to a concept this tree does not declare would be a resume path
// that never fires. When that feed lands, this becomes its fallback rather
// than its mechanism.
const InferenceRetryInterval = 5 * time.Minute

// DoorReport is one door the chain considered and why it did not open. It is a
// plain value so the router can build it without importing this package's
// approval machinery and the approval can carry it without importing the
// router's.
type DoorReport struct {
	// Door is `local`, `app`, `federation` or `apiKey` -- the same words
	// v1:platform:inferenceStatus.doorsOpen uses, so a person reading a park
	// card and a person reading the readiness line see one vocabulary.
	Door string
	// Name is the provider reference the policy actually wrote
	// (`fleet:*`, `app:claude-code`, `streamClaudeSonnet`).
	Name string
	// Reason is why this door did not serve the call.
	Reason string
	// Considered is the door's own report, one entry per thing it looked at
	// and why that thing was ruled out: a machine for the local door, an app
	// for the app door.
	//
	// It is kept because the machine-level detail is the part somebody can
	// ACT on -- "your fleet is unavailable" sends a person nowhere, while
	// "laptop: offline; desktop: does not offer llama3.1:8b" names the
	// laptop to open and the model to pull. The door line says which door;
	// this says why that door could not open.
	Considered map[string]string
}

// AsMap renders one door for the approval subject. `considered` is a SLICE so
// the order a person reads is the order every reader gets -- a map would
// render in whatever order the encoder chose that day.
func (d DoorReport) AsMap() map[string]any {
	out := map[string]any{"door": d.Door, "name": d.Name, "reason": d.Reason}
	if len(d.Considered) == 0 {
		return out
	}
	keys := make([]string, 0, len(d.Considered))
	for k := range d.Considered {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	considered := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		considered = append(considered, map[string]any{"subject": k, "reason": d.Considered[k]})
	}
	out["considered"] = considered
	return out
}

// InferenceUnavailableApproval builds the park.
//
// The subject carries every door and its reason because "no model available"
// with nothing else is not a decision anybody can make: the fixes are in four
// different places (wake a machine, sign into an app, add a key, raise a
// ceiling) and which one applies is exactly what the door list says.
//
// The ARTIFACT HASH is over that subject, which gives the resume gate the
// right meaning here: approving "these four doors are shut, spend money
// anyway" must not carry to a later moment when a different set is shut. A
// person raising the ceiling is deciding about the situation they were shown.
func InferenceUnavailableApproval(runId, stepKey, code string, doors []DoorReport, now time.Time, ttl time.Duration) ApprovalRequest {
	rendered := make([]any, 0, len(doors))
	for _, d := range doors {
		rendered = append(rendered, d.AsMap())
	}
	subject := map[string]any{
		"code":  code,
		"doors": rendered,
	}
	ev := Evidence{
		Tier:   "environment",
		Reason: inferenceReason(code),
		RuleId: "inference." + code,
		Source: EvidenceSourceRules,
	}
	a := newApproval(ApprovalKindInferenceUnavailable, runId, stepKey, subject, ev, now, ttl)
	a.Question = inferenceQuestion(code)
	a.Options = []map[string]any{
		{"label": "Use a paid provider for this run", "value": "approved"},
		{"label": "Stop this run", "value": "rejected"},
	}
	return a
}

// inferenceReason is the verdict in words. It says what is true rather than
// what to do, because what to do depends on which door the reader owns.
func inferenceReason(code string) string {
	switch code {
	case RefusalCeilingReached:
		return "a paid provider could have served this call, and the cost ceiling for this process has been reached"
	case RefusalNoLocalModel:
		return "the policy named a local model and no machine can serve it"
	case RefusalNoApp:
		return "the policy named a subscription app and no machine has one allowed, signed in and online"
	default:
		return "every door to a model is shut: no local model, no signed-in app, and no paid provider this run may reach"
	}
}

// inferenceQuestion is what a person is actually being asked. The two cases
// are genuinely different asks and collapsing them would put the wrong
// question in front of somebody: one is "spend money", the other is "spend
// MORE money".
func inferenceQuestion(code string) string {
	if code == RefusalCeilingReached {
		return "Raise the cost ceiling so this run can use a paid provider?"
	}
	return "No model is reachable. Use a paid provider for this run?"
}

// DoorReporter is implemented by a refusal that can describe the doors it
// tried. component/router's InferenceUnavailable is the only implementation.
//
// IT IS AN INTERFACE BECAUSE THE MODULES CANNOT SEE EACH OTHER. The refusal is
// raised in component/router and the decision about what the run does with it
// is made in component/automations, and neither imports the other. Both import
// this package, so this is where the shape of the answer belongs.
//
// The alternative -- reading the door list back out of the rendered error
// message -- would be a parser of our own output: a second opinion about a
// decision already made, drifting the first time the message changed wording.
type DoorReporter interface {
	// RefusalCode is one of the Refusal* constants above.
	RefusalCode() string
	// ReportedDoors is every door the chain tried and why each did not open.
	ReportedDoors() []DoorReport
}

// DoorsFrom pulls the structured refusal out of an error chain.
//
// It answers (code, doors, true) when the error carries a door report, and
// falls back to the MESSAGE match when it does not -- so a refusal that
// travelled as a string still parks the run, with an empty door list rather
// than an invented one. An empty list is honest: it says the structure did not
// reach here, where a single synthetic entry would claim a door that was never
// named.
func DoorsFrom(err error) (code string, doors []DoorReport, ok bool) {
	if err == nil {
		return "", nil, false
	}
	var reporter DoorReporter
	if errorsAs(err, &reporter) {
		return reporter.RefusalCode(), reporter.ReportedDoors(), true
	}
	if code, matched := InferenceRefusalCode(err.Error()); matched {
		return code, nil, true
	}
	return "", nil, false
}

// errorsAs is errors.As, named locally so the one place this package reaches
// into an error chain is greppable.
func errorsAs(err error, target any) bool { return errors.As(err, target) }
