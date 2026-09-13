package automations

// failure_path.go -- what happens to a run that failed for a reason other than
// a shut inference door (epic memql#5127, design D12).
//
// # The order is the whole design
//
// DETERMINISTIC RULES RUN FIRST. component/work's symptom table is a pure
// function over a Signal, and it is consulted before anything else. Only when
// it has NO OPINION does this path make one cheap model call, at level `fast`,
// to the classifySymptom prompt. So a failure the rules classify costs ZERO
// provider calls, which is the headline property the work spine's design
// claimed and nothing had ever exercised: ClassifyByRules, ActFor,
// ApprovalKindFor, the classifySymptom prompt, the replan prompt and the
// healer were all built, all tested in isolation, and none of them had a
// production caller.
//
// # Why the act is RECORDED rather than performed here
//
// The obvious implementation performs the act inline: retry the step, invoke
// replanGap, call the repair loop. That implementation is single-node. The
// executor runs on an agent replica; compile, replan and the sweeps run on the
// planner (design record section H). A replan performed here would be a call
// into machinery this node does not have, on a run another node owns.
//
// So the act lands on the RUN, as a `waiting` state naming what it waits for,
// and the sweep that already re-dispatches stale runs picks it up on the node
// that can serve it. That is the same shape parkOnInference already uses, and
// it is the shape that survives two replicas.
//
// # What an unwired classifier does: nothing
//
// With no SymptomClassifier installed, a table miss is not a classification,
// so no act is selected and the run FAILS exactly as it did before any of this
// existed. That direction is what makes the wiring safe to land: a node that
// cannot reach a model behaves identically to the tree before this epic, and
// installing a classifier can only REDUCE the number of failures that end as a
// bare `failed` row. The inverse -- treating "unclassified" as "ask a person"
// -- would turn every ordinary failure on such a node into a run parked on a
// question nobody was told about.

import (
	"context"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/airoute"
	"github.com/znasllc-io/memql/core/id"
)

// ClassifySymptomInput is what the one model call is given. It mirrors the
// classifySymptom prompt's declared field list.
type ClassifySymptomInput struct {
	StepKey      string
	StepType     string
	ErrorCode    string
	ErrorMessage string
	Attempt      int
	Trace        []map[string]any
}

// SymptomClassifier is the ONE model call this path may make. It is reached
// only when component/work's deterministic table has no opinion.
//
// The interface exists rather than a direct engine call because
// component/automations' journal holds a query executor and nothing else, and
// because a test must be able to COUNT the calls: "a rules-classified failure
// makes zero provider calls" is only checkable if the seam can be counted.
type SymptomClassifier interface {
	// Level is the level the implementation resolves the call at. It is on
	// the interface so a test can assert it is `fast` without reaching into
	// the implementation: the cheapest tier is a property of the design, not
	// an implementation detail.
	Level() airoute.Level
	Classify(ctx context.Context, in ClassifySymptomInput) (work.Symptom, work.Evidence, error)
}

// SetSymptomClassifier installs the classifier. Called once from app/ on a
// node that can reach a model. It lands on the journal, which is the only
// thing on this path that outlives one execution.
func (e *Executor) SetSymptomClassifier(c SymptomClassifier) {
	if e == nil || e.journal == nil {
		return
	}
	e.journal.classifier = c
}

// failureWaitKinds are the `waitingOn.kind` values this path writes. They are
// constants because the sweep on the other side matches them, and a typo would
// leave a run waiting for something no sweep looks for.
const (
	// WaitKindApproval is an existing kind: a person decides.
	WaitKindApproval = "approval"
	// WaitKindRetry is a run the sweep should re-dispatch unchanged. The
	// symptom said the failure was a blip.
	WaitKindRetry = "retry"
	// WaitKindReplan is a run whose remaining plan is wrong from the failed
	// step on. The planner's sweep re-plans the gap, keeping the prefix.
	WaitKindReplan = "replan"
	// WaitKindRepair is a run whose step did something other than what it
	// promised. The failed step re-runs with the violation as guidance.
	WaitKindRepair = "repair"
)

// retryBackoff is how long a `retry` wait sits before the sweep re-dispatches.
// Short, because a transient failure is transient; the retry BUDGET rather
// than this interval is what stops a loop.
const retryBackoff = 30 * time.Second

// classifyAndAct is closeRun's non-inference failure branch. It returns true
// when it wrote a terminal-or-waiting state itself, so closeRun does not also
// write `failed` over the top of it.
func (j *workJournal) classifyAndAct(ctx context.Context, exec *AutomationExecution, chainHead string) bool {
	if j == nil || exec == nil {
		return false
	}
	sig := signalFor(exec)
	stepKey := lastStepKey(exec)

	// A FAILURE THAT IS ALREADY OVER IS NOT A SYMPTOM (component/work's
	// terminal.go). The five symptoms are all answers to "what should the
	// loop do about this?", and these have no act: the composition wrote its
	// own terminal row, or a clock this system set for itself ran out and the
	// next attempt gets the same clock. Asking the table anyway is how the
	// bug shipped -- it read "deadline exceeded", called it a blip and parked
	// the run on a retry, while the composition row said `failed`. So the
	// check is here, ABOVE the table, and the run goes terminal with the
	// reason on it.
	if code, ok := work.TerminalFailureCode(sig.ErrorMessage); ok {
		j.failTerminally(ctx, exec, chainHead, stepKey, code)
		return true
	}

	symptom, evidence, ok := work.ClassifyByRules(sig)
	if !ok {
		// THE ONE MODEL CALL, and only here.
		symptom, evidence, ok = j.classifyByModel(ctx, exec, sig, stepKey)
	}
	if !ok {
		// NOTHING CLASSIFIED IT, so nothing acts on it: the run FAILS exactly
		// as it did before any of this was wired.
		//
		// This is the direction that makes the wiring safe to land. The
		// tempting alternative -- treat "unclassified" as ActAsk and park on a
		// feedback approval -- turns every ordinary failure into a run waiting
		// on a person who was never told there was a question, and on a
		// cluster with no classifier installed that is EVERY failure. Wiring a
		// classifier can then only REDUCE the number of times a person is
		// asked, which is the property design D12 claims; parking here would
		// have inverted it.
		return false
	}

	act := work.ActFor(symptom, sig.Attempt, sig.MaxRetries)
	j.recordSymptom(ctx, exec, stepKey, symptom)

	now := time.Now().UTC()
	switch act {
	case work.ActRetry:
		j.waitFor(ctx, exec, chainHead, map[string]any{
			"kind":     WaitKindRetry,
			"subject":  stepKey,
			"since":    rfc3339(now),
			"resumeAt": rfc3339(now.Add(retryBackoff)),
			"reason":   evidence.Reason,
			"ruleId":   evidence.RuleId,
		})
	case work.ActReplan:
		j.waitFor(ctx, exec, chainHead, map[string]any{
			"kind":    WaitKindReplan,
			"subject": stepKey,
			"since":   rfc3339(now),
			"reason":  evidence.Reason,
			"ruleId":  evidence.RuleId,
		})
	case work.ActRepair:
		j.waitFor(ctx, exec, chainHead, map[string]any{
			"kind":    WaitKindRepair,
			"subject": stepKey,
			"since":   rfc3339(now),
			"reason":  evidence.Reason,
			"ruleId":  evidence.RuleId,
		})
	case work.ActHeal, work.ActAsk:
		// The KIND is chosen from the verdict rather than the act alone: an
		// exhausted budget and "the system cannot decide this" are both
		// ActAsk and are different questions, and the person deciding reads
		// the kind's sentence, not the act's.
		kind := work.ApprovalKindForVerdict(act, evidence)
		if kind == "" {
			return false
		}
		j.waitOnApproval(ctx, exec, chainHead, stepKey, kind, symptom, evidence, now)
	default:
		return false
	}

	if j.logger != nil {
		j.logger.Info("work journal: a failed run was classified and is waiting on its act",
			"component", ComponentName, "run", exec.ID, "step", stepKey,
			"symptom", string(symptom), "act", string(act),
			"source", evidence.Source, "ruleId", evidence.RuleId)
	}
	return true
}

// failTerminally closes the run as `failed` with the code naming why another
// attempt is not the answer.
//
// IT WRITES THE TERMINAL ROW ITSELF rather than returning false and letting
// closeRun do it, for the one thing closeRun cannot say: the errorCode. Nexus
// keys its run notices on that field, so a code is the difference between a
// reader seeing "the composition recorded its own failure, so the document
// does not exist" and seeing the generic "this run failed -- the step it
// stopped at is marked below, with what the classifier made of it", which
// would point them at a classifier that deliberately never ran.
//
// NO SYMPTOM IS RECORDED ON THE STEP. The five are a closed enum and none of
// them is true here: `transient` promises a retry inside the budget,
// `contract` promises a repair from the failed step, `human` promises the run
// parked and asked -- and this run did none of those. Nexus renders each of
// those promises as a sentence, so writing one to fill the field would put a
// false sentence on the row. An empty symptom reads as "this build cannot say
// which of the five", which is exactly right: it is none of them.
func (j *workJournal) failTerminally(ctx context.Context, exec *AutomationExecution, chainHead, stepKey, code string) {
	if stepKey != "" {
		j.call(ctx, "updateWorkStep", map[string]any{
			"stepId":    workStepId(exec.ID, stepKey),
			"errorCode": code,
		})
	}
	finished := exec.CompletedAt
	if finished.IsZero() {
		finished = time.Now()
	}
	args := map[string]any{
		"runId":      exec.ID,
		"status":     "failed",
		"finishedAt": rfc3339(finished),
		"chainHead":  chainHead,
		"stepOrder":  exec.StepOrder,
		"errorCode":  code,
		"outcome": map[string]any{
			"executorStatus": exec.Status,
			"terminalReason": work.TerminalReason(code),
		},
	}
	if exec.Error != "" {
		args["errorMessage"] = exec.Error
	}
	j.call(ctx, "updateWorkRun", args)
	if j.logger != nil {
		j.logger.Info("work journal: a failure that cannot end differently was recorded as terminal rather than parked",
			"component", ComponentName, "run", exec.ID, "step", stepKey, "errorCode", code)
	}
}

// classifyByModel makes the single classifySymptom call. It is a separate
// method so the "zero calls on a rules hit" property is visible as a branch a
// test can assert rather than as an early return buried in a longer function.
// It returns ok=false for every outcome that is not a classification -- no
// classifier on this node, a call that failed, an answer outside the enum --
// and the caller then leaves the run to fail as it always did.
func (j *workJournal) classifyByModel(ctx context.Context, exec *AutomationExecution, sig work.Signal, stepKey string) (work.Symptom, work.Evidence, bool) {
	if j.classifier == nil {
		// No classifier on this node: not a verdict, and not a reason to act.
		return work.SymptomNone, work.Evidence{}, false
	}
	symptom, evidence, err := j.classifier.Classify(ctx, ClassifySymptomInput{
		StepKey:      stepKey,
		StepType:     sig.StepType,
		ErrorCode:    sig.ErrorCode,
		ErrorMessage: sig.ErrorMessage,
		Attempt:      sig.Attempt,
		Trace:        traceFor(exec),
	})
	if err != nil {
		// A classifier that could not answer is not a classification. Asking a
		// person is the only act of the five that cannot make things worse.
		if j.logger != nil {
			j.logger.Warn("work journal: the symptom classifier could not answer, so the run fails as it would have without one",
				"component", ComponentName, "run", exec.ID, "error", err)
		}
		return work.SymptomNone, work.Evidence{}, false
	}
	if !symptom.Valid() || symptom == work.SymptomNone {
		return work.SymptomNone, work.Evidence{}, false
	}
	return symptom, evidence, true
}

// recordSymptom writes the classifier's verdict onto the failed step, so the
// decision is on the row a person reads rather than only in a log line.
func (j *workJournal) recordSymptom(ctx context.Context, exec *AutomationExecution, stepKey string, symptom work.Symptom) {
	if stepKey == "" {
		return
	}
	// v1:work:step carries `symptom` and no `evidence`: the reasoned half
	// lives on the approval this act may raise, and on waitingOn for the acts
	// that raise none. Writing it twice would give two rows that disagree the
	// first time one of them is updated.
	j.call(ctx, "updateWorkStep", map[string]any{
		"stepId":  workStepId(exec.ID, stepKey),
		"symptom": string(symptom),
	})
}

// waitFor parks the run on a non-approval wait. It writes NEITHER finishedAt
// NOR an errorCode, for parkOnInference's reason: the run has not finished and
// has not failed, and writing either would make every terminal-run reader
// treat a waiting run as done.
func (j *workJournal) waitFor(ctx context.Context, exec *AutomationExecution, chainHead string, waiting map[string]any) {
	j.call(ctx, "updateWorkRun", map[string]any{
		"runId":        exec.ID,
		"status":       "waiting",
		"chainHead":    chainHead,
		"stepOrder":    exec.StepOrder,
		"waitingOn":    waiting,
		"errorMessage": exec.Error,
	})
}

// waitOnApproval raises the approval and then parks on it. THE ORDER IS
// LOAD-BEARING, exactly as it is in parkOnInference: a run parked on an
// approval id that does not exist waits on nothing, which no person can decide
// and no sweep can resolve.
func (j *workJournal) waitOnApproval(ctx context.Context, exec *AutomationExecution, chainHead, stepKey, kind string, symptom work.Symptom, evidence work.Evidence, now time.Time) {
	approvalId := "v1:work:approval:" + id.NewShortId()
	question := "This run failed and the system does not know how to proceed."
	if evidence.Reason != "" {
		question = evidence.Reason
	}
	j.call(ctx, "createWorkApproval", map[string]any{
		"approvalId": approvalId,
		"runId":      exec.ID,
		"stepKey":    stepKey,
		"kind":       kind,
		"subject": map[string]any{
			"symptom":      string(symptom),
			"stepKey":      stepKey,
			"errorMessage": exec.Error,
		},
		// The hash covers the FAILURE, not a proposed change: an approval is a
		// decision about one specific situation, and resume compares the hash
		// so a decision cannot carry to a different failure of the same step.
		"artifactHash": work.ArtifactHash(map[string]any{
			"runId":   exec.ID,
			"stepKey": stepKey,
			"error":   exec.Error,
			"symptom": string(symptom),
		}),
		"question": question,
		"options":  []any{"retry", "abandon"},
		"evidence": map[string]any{
			"tier":   evidence.Tier,
			"reason": evidence.Reason,
			"ruleId": evidence.RuleId,
			"source": evidence.Source,
		},
		"requestedAt": rfc3339(now),
		"expiresAt":   rfc3339(now.Add(workApprovalTTL)),
	})
	j.waitFor(ctx, exec, chainHead, map[string]any{
		"kind":         WaitKindApproval,
		"subject":      approvalId,
		"approvalKind": kind,
		"since":        rfc3339(now),
	})
}

// signalFor builds the rules table's input from what the executor actually
// knows.
//
// TWO FIELDS ARE DELIBERATELY LEFT FALSE, and saying so here is the point.
// PostconditionFailed has NO PRODUCER in this tree: component/work derives and
// requires postconditions, and the automations executor does not evaluate
// them, so the `contract.postcondition` rule is unreachable from this path.
// The ActRepair arm above is still implemented, so a caller that does produce
// the signal is served -- but do not read the presence of that arm as evidence
// that contract misses are being caught today. PreconditionFailed IS produced,
// by emitPreconditionMiss, and is carried here through exec.PreconditionMissed.
func signalFor(exec *AutomationExecution) work.Signal {
	if exec == nil {
		return work.Signal{}
	}
	sig := work.Signal{
		ErrorMessage:       exec.Error,
		StepType:           exec.FailedStepType,
		Attempt:            exec.FailedStepAttempt,
		MaxRetries:         exec.FailedStepRetries,
		PreconditionFailed: exec.PreconditionMissed,
	}
	if sig.Attempt < 1 {
		sig.Attempt = 1
	}
	if step := failedStep(exec); step != nil && strings.TrimSpace(step.Error) != "" {
		sig.ErrorMessage = step.Error
	}
	// A step that used every retry it was given and failed the same way each
	// time IS the stall signal. The rules table puts that rule above every
	// transient matcher precisely so a repeated action escalates rather than
	// retrying forever -- and the executor is the only place that knows the
	// difference, because the budget is spent inside its loop.
	sig.RepeatedAction = sig.MaxRetries > 0 && sig.Attempt > sig.MaxRetries
	return sig
}

// failedStep returns the last step that failed, which is the one the act is
// about.
func failedStep(exec *AutomationExecution) *StepResult {
	if exec == nil {
		return nil
	}
	for i := len(exec.StepOrder) - 1; i >= 0; i-- {
		if s := exec.Steps[exec.StepOrder[i]]; s != nil && s.Status == "failed" {
			return s
		}
	}
	return nil
}

// lastStepKey prefers the step the executor RECORDED as failed over the one
// derived from the results map. The two agree in an ordinary run, and when
// they do not it is because the recorded one is a fact the executor stated and
// the derived one is a guess from a map somebody else may have written to.
func lastStepKey(exec *AutomationExecution) string {
	if exec == nil {
		return ""
	}
	if exec.FailedStepId != "" {
		return exec.FailedStepId
	}
	if s := failedStep(exec); s != nil && s.StepId != "" {
		return s.StepId
	}
	if n := len(exec.StepOrder); n > 0 {
		return exec.StepOrder[n-1]
	}
	return ""
}

// traceFor is what the classifier reads to see whether the run is going in
// circles: the recent steps, oldest first, with their status and error and
// nothing else. Results are deliberately absent -- a step's output may carry
// resolved secrets, and the classifier's answer is one of five words.
func traceFor(exec *AutomationExecution) []map[string]any {
	if exec == nil {
		return nil
	}
	const maxTrace = 12
	start := 0
	if n := len(exec.StepOrder); n > maxTrace {
		start = n - maxTrace
	}
	out := make([]map[string]any, 0, len(exec.StepOrder)-start)
	for _, key := range exec.StepOrder[start:] {
		step := exec.Steps[key]
		if step == nil {
			continue
		}
		out = append(out, map[string]any{
			"key":    key,
			"status": step.Status,
			"error":  step.Error,
		})
	}
	return out
}
