package automations

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/airoute"
)

// countingClassifier is the whole point of the SymptomClassifier interface:
// the design's headline claim is about how many provider calls a failure
// costs, and a claim about a COUNT is only checkable if the seam can be
// counted. A classifier reached through the engine directly could not be.
type countingClassifier struct {
	calls   int
	symptom work.Symptom
	err     error
}

func (c *countingClassifier) Level() airoute.Level { return airoute.LevelFast }

func (c *countingClassifier) Classify(context.Context, ClassifySymptomInput) (work.Symptom, work.Evidence, error) {
	c.calls++
	if c.err != nil {
		return work.SymptomNone, work.Evidence{}, c.err
	}
	return c.symptom, work.Evidence{
		Tier:   "classified",
		Reason: "the model read the trace",
		Source: work.EvidenceSourceModel,
	}, nil
}

// failedRun builds an execution that failed at one step, the way the executor
// leaves it: the message on the run, and the step facts recorded at the point
// of failure.
func failedRun(id, message string, attempt, retries int) *AutomationExecution {
	run := &AutomationExecution{ID: id}
	run.RecordFailedStep(&Step{ID: "run", Type: StepType("function"), RetryCount: retries}, attempt)
	run.Fail(errors.New(message))
	return run
}

func closeWith(t *testing.T, c SymptomClassifier, run *AutomationExecution) []string {
	t.Helper()
	exec := &recordingJournalExecutor{}
	j := newWorkJournal(exec, nil)
	j.classifier = c
	j.closeRun(context.Background(), run, "")
	return exec.calls
}

// TestATransientFailureCostsZeroProviderCalls is the design's headline
// property (work-spine record section E, epic memql#5127 D12): the
// deterministic table runs FIRST, and a failure it recognises never reaches a
// model at all.
//
// Its negative control is the test below: a novel error must reach the
// classifier exactly once. Without that pair, a classifier that was never
// invoked on ANY path would satisfy this test forever.
func TestATransientFailureCostsZeroProviderCalls(t *testing.T) {
	c := &countingClassifier{symptom: work.SymptomHuman}
	calls := closeWith(t, c, failedRun("v1:work:run:t1", "dial tcp: connection refused", 1, 3))

	if c.calls != 0 {
		t.Fatalf("the classifier was called %d time(s) for a failure the rules table recognises; "+
			"a rules-classified symptom must cost zero provider calls", c.calls)
	}
	name, args := argsOf(t, lastCallNamed(t, calls, "updateWorkRun"))
	if name != "updateWorkRun" || args["status"] != "waiting" {
		t.Fatalf("status = %v, want the run to wait on its retry", args["status"])
	}
	waiting, _ := args["waitingOn"].(map[string]any)
	if waiting["kind"] != WaitKindRetry {
		t.Fatalf("waitingOn.kind = %v, want %q", waiting["kind"], WaitKindRetry)
	}
	if waiting["resumeAt"] == nil || waiting["resumeAt"] == "" {
		t.Error("a retry wait with no resumeAt is never due, so the run would never be handed back")
	}
	if waiting["ruleId"] == nil || waiting["ruleId"] == "" {
		t.Error("the wait must name the rule that fired; without it nobody can tell a rules verdict from a model's")
	}
}

// TestANovelFailureCostsExactlyOneProviderCall is the negative control for the
// test above AND the claim in its own right. A counter that never rises on any
// path reads as zero forever.
func TestANovelFailureCostsExactlyOneProviderCall(t *testing.T) {
	c := &countingClassifier{symptom: work.SymptomPlan}
	calls := closeWith(t, c, failedRun("v1:work:run:t2", "the vendor said something nobody has a rule for", 1, 3))

	if c.calls != 1 {
		t.Fatalf("the classifier was called %d time(s); a failure the rules cannot classify costs exactly one", c.calls)
	}
	_, args := argsOf(t, lastCallNamed(t, calls, "updateWorkRun"))
	waiting, _ := args["waitingOn"].(map[string]any)
	if waiting["kind"] != WaitKindReplan {
		t.Fatalf("waitingOn.kind = %v, want %q -- a plan symptom re-plans the gap", waiting["kind"], WaitKindReplan)
	}
}

// TestAnUnclassifiableFailureStillFailsTheRun pins the direction that makes
// this wiring safe to land. A node with no classifier must behave exactly as
// the tree did before this epic; the tempting alternative -- calling an
// unclassified failure "ask a person" -- parks every ordinary failure on a
// question nobody was told about.
func TestAnUnclassifiableFailureStillFailsTheRun(t *testing.T) {
	calls := closeWith(t, nil, failedRun("v1:work:run:t3", "something nothing has a rule for", 1, 0))
	if len(calls) != 1 {
		t.Fatalf("want one close call on a node with no classifier, got %v", calls)
	}
	name, args := argsOf(t, calls[0])
	if name != "updateWorkRun" || args["status"] != "failed" {
		t.Fatalf("call = %q status = %v, want the run to fail exactly as it did before", name, args["status"])
	}
	if args["finishedAt"] == nil {
		t.Error("a failed run finishes")
	}
}

// TestAClassifierThatCannotAnswerIsNotAVerdict covers the two ways a wired
// classifier produces nothing usable. Both must leave the run failing rather
// than acting on a value nobody defined.
func TestAClassifierThatCannotAnswerIsNotAVerdict(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    *countingClassifier
	}{
		{"the call failed", &countingClassifier{err: errors.New("provider down")}},
		{"the answer was outside the enum", &countingClassifier{symptom: work.Symptom("probably fine")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := closeWith(t, tc.c, failedRun("v1:work:run:t4", "novel", 1, 0))
			if tc.c.calls != 1 {
				t.Fatalf("classifier calls = %d, want 1", tc.c.calls)
			}
			_, args := argsOf(t, calls[len(calls)-1])
			if args["status"] != "failed" {
				t.Fatalf("status = %v, want failed", args["status"])
			}
		})
	}
}

// TestARepeatedActionEscalatesRatherThanRetryingForever pins the ORDER inside
// the rules table through this path. The message here also matches a transient
// matcher; the stall rule sits above them all, so a step that spent its whole
// retry budget on the same failure asks a person instead of asking for another
// retry it has already proved does not help.
func TestARepeatedActionEscalatesRatherThanRetryingForever(t *testing.T) {
	c := &countingClassifier{symptom: work.SymptomTransient}
	calls := closeWith(t, c, failedRun("v1:work:run:t5", "connection refused", 4, 3))

	if c.calls != 0 {
		t.Fatalf("the classifier was called %d time(s); the stall rule classifies this", c.calls)
	}
	if !anyCallNamed(calls, "createWorkApproval") {
		t.Fatalf("a stalled run must raise an approval, got %v", calls)
	}
	_, args := argsOf(t, lastCallNamed(t, calls, "createWorkApproval"))
	if args["kind"] != work.ApprovalKindFeedback {
		t.Fatalf("approval kind = %v, want %q", args["kind"], work.ApprovalKindFeedback)
	}
	_, runArgs := argsOf(t, lastCallNamed(t, calls, "updateWorkRun"))
	waiting, _ := runArgs["waitingOn"].(map[string]any)
	if waiting["kind"] != WaitKindApproval {
		t.Fatalf("waitingOn.kind = %v, want %q", waiting["kind"], WaitKindApproval)
	}
	// THE ORDER IS LOAD-BEARING: the approval is written before the run parks
	// on it. A run parked on an approval id that does not exist waits on
	// nothing, which no person can decide and no sweep can resolve.
	if indexOfCall(calls, "createWorkApproval") > indexOfCall(calls, "updateWorkRun") {
		t.Fatalf("the run parked before the approval existed: %v", calls)
	}
}

// TestAPreconditionMissIsClassifiedFromTheValueNotTheMessage pins that the
// precondition signal travels as a FACT recorded on the execution rather than
// as words in an error string. The environment.literal rule keys on the flag,
// so a run whose message says nothing about preconditions still classifies.
func TestAPreconditionMissIsClassifiedFromTheValueNotTheMessage(t *testing.T) {
	c := &countingClassifier{symptom: work.SymptomHuman}
	run := failedRun("v1:work:run:t6", "the check did not hold", 1, 0)
	run.PreconditionMissed = true

	calls := closeWith(t, c, run)
	if c.calls != 0 {
		t.Fatalf("the classifier was called %d time(s); a precondition miss is a rules verdict", c.calls)
	}
	_, args := argsOf(t, lastCallNamed(t, calls, "createWorkApproval"))
	// An environment symptom heals, and healing is never a silent edit: it
	// reaches a person as a planReview (work-spine design D5).
	if args["kind"] != work.ApprovalKindPlanReview {
		t.Fatalf("approval kind = %v, want %q", args["kind"], work.ApprovalKindPlanReview)
	}
}

// TestTheSymptomLandsOnTheStepRow pins that a classification is recorded where
// a person reads it, not only in a log line.
func TestTheSymptomLandsOnTheStepRow(t *testing.T) {
	c := &countingClassifier{symptom: work.SymptomPlan}
	calls := closeWith(t, c, failedRun("v1:work:run:t7", "novel", 1, 0))
	if !anyCallNamed(calls, "updateWorkStep") {
		t.Fatalf("no step row was written: %v", calls)
	}
	_, args := argsOf(t, lastCallNamed(t, calls, "updateWorkStep"))
	if args["symptom"] != string(work.SymptomPlan) {
		t.Fatalf("step symptom = %v, want %q", args["symptom"], work.SymptomPlan)
	}
	if args["stepId"] == nil || args["stepId"] == "" {
		t.Error("updateWorkStep takes a stepId; a write with none updates nothing and reports success")
	}
}

// TestAnInferenceRefusalStillParksAndIsNotClassified pins that the two failure
// paths stay in order. A shut door is not a symptom -- nothing about the work
// was wrong -- so it must reach parkOnInference without costing a classifier
// call.
func TestAnInferenceRefusalStillParksAndIsNotClassified(t *testing.T) {
	c := &countingClassifier{symptom: work.SymptomHuman}
	run := &AutomationExecution{ID: "v1:work:run:t8"}
	run.Fail(&stubDoorRefusal{code: work.RefusalEveryDoorShut})

	calls := closeWith(t, c, run)
	if c.calls != 0 {
		t.Fatalf("the classifier was called %d time(s) for a shut door; the door check runs first", c.calls)
	}
	_, args := argsOf(t, lastCallNamed(t, calls, "createWorkApproval"))
	if args["kind"] != work.ApprovalKindInferenceUnavailable {
		t.Fatalf("approval kind = %v, want %q", args["kind"], work.ApprovalKindInferenceUnavailable)
	}
}

// TestAComposedDocumentThatFailedDoesNotParkAsWaiting is the reported bug,
// end to end through the path that produced it.
//
// The Materializer wrapped its one model call in a fixed three-minute
// deadline. A long document blew through it, the compose pipeline wrote its
// composition row terminally `failed`, and the error it handed back said
// "context deadline exceeded". Those words matched transient.timeout, so this
// path parked the run at `waiting` on a retry that would re-read a `failed`
// row -- and Nexus showed a person "Waiting" over work the database had
// already given up on.
//
// The deadline is gone (app/materializer_composer.go). This pins the other
// half: even carrying the exact words that fooled the table, a failure the
// system already recorded terminally closes the run.
func TestAComposedDocumentThatFailedDoesNotParkAsWaiting(t *testing.T) {
	c := &countingClassifier{symptom: work.SymptomTransient}
	calls := closeWith(t, c, failedRun("v1:work:run:c1",
		"composition_failed: compose: composing the draft failed: context deadline exceeded", 1, 3))

	if c.calls != 0 {
		t.Fatalf("the classifier was called %d time(s); a failure that is already over is not a symptom to classify", c.calls)
	}
	_, args := argsOf(t, lastCallNamed(t, calls, "updateWorkRun"))
	if args["status"] != "failed" {
		t.Fatalf("status = %v, want failed: the composition row is terminal and the file does not exist", args["status"])
	}
	if args["waitingOn"] != nil {
		t.Fatalf("the run parked on %v as well as failing; the two readings contradict each other", args["waitingOn"])
	}
	if args["finishedAt"] == nil {
		t.Error("a terminal run finishes; without it every terminal-run reader treats this as still in flight")
	}
	if args["errorCode"] != work.TerminalCompositionFailed {
		t.Fatalf("errorCode = %v, want %q -- Nexus keys its run notice on that field", args["errorCode"], work.TerminalCompositionFailed)
	}
	outcome, _ := args["outcome"].(map[string]any)
	if outcome["terminalReason"] != work.TerminalReason(work.TerminalCompositionFailed) {
		t.Errorf("outcome.terminalReason = %v, want the reason in words", outcome["terminalReason"])
	}
	if args["errorMessage"] == nil || args["errorMessage"] == "" {
		t.Error("the message a person reads must survive; it is the only place the underlying cause is written")
	}
}

// A clock MemQL set for itself is not evidence about anything except the
// number we chose, and the next attempt gets the same number. Long work
// carries no such clock any more, so seeing this means one was configured
// deliberately -- and then it must say so rather than impersonate a blip.
func TestASelfImposedTimeoutFailsHonestlyRatherThanRetrying(t *testing.T) {
	c := &countingClassifier{symptom: work.SymptomTransient}
	calls := closeWith(t, c, failedRun("v1:work:run:c2",
		"agent: turn wallclock timeout after 3m0s", 1, 3))

	if c.calls != 0 {
		t.Fatalf("the classifier was called %d time(s)", c.calls)
	}
	_, args := argsOf(t, lastCallNamed(t, calls, "updateWorkRun"))
	if args["status"] != "failed" || args["errorCode"] != work.TerminalSelfTimeout {
		t.Fatalf("status = %v errorCode = %v, want failed/%s", args["status"], args["errorCode"], work.TerminalSelfTimeout)
	}
}

// A PROVIDER'S OWN TIMEOUT IS STILL A BLIP, and this is the control that
// keeps the fix narrow. The terminal check runs first, so if it were loose it
// would take every retryable timeout straight to a dead run and the retry
// that would have fixed it would never happen.
func TestAProviderTimeoutStillRetries(t *testing.T) {
	c := &countingClassifier{symptom: work.SymptomHuman}
	calls := closeWith(t, c, failedRun("v1:work:run:c3",
		"Post \"https://api.example.com/v1/messages\": context deadline exceeded", 1, 3))

	if c.calls != 0 {
		t.Fatalf("the classifier was called %d time(s); transient.timeout classifies this", c.calls)
	}
	_, args := argsOf(t, lastCallNamed(t, calls, "updateWorkRun"))
	if args["status"] != "waiting" {
		t.Fatalf("status = %v, want waiting: the same call may well work again", args["status"])
	}
	waiting, _ := args["waitingOn"].(map[string]any)
	if waiting["kind"] != WaitKindRetry {
		t.Fatalf("waitingOn.kind = %v, want %q", waiting["kind"], WaitKindRetry)
	}
}

// TestTheTerminalStepRowCarriesTheCodeAndNoSymptom pins what a replay or a
// reader finds on the step afterwards.
//
// The code lands, because it is how anybody reading the journal later can
// tell this failure from a retryable one. The SYMPTOM deliberately does not:
// the five are a closed enum and none of them is true here, and Nexus renders
// each as a promise -- `transient` promises a retry inside the budget,
// `human` promises the run parked and asked -- so filling the field to avoid
// a blank would put a false sentence on the row.
func TestTheTerminalStepRowCarriesTheCodeAndNoSymptom(t *testing.T) {
	calls := closeWith(t, &countingClassifier{}, failedRun("v1:work:run:c4",
		"composition_failed: the model returned an empty document body", 1, 0))

	_, step := argsOf(t, lastCallNamed(t, calls, "updateWorkStep"))
	if step["errorCode"] != work.TerminalCompositionFailed {
		t.Fatalf("step errorCode = %v, want %q", step["errorCode"], work.TerminalCompositionFailed)
	}
	if step["symptom"] != nil {
		t.Fatalf("step symptom = %v; none of the five is true of a failure that is already over", step["symptom"])
	}
	if step["stepId"] == nil || step["stepId"] == "" {
		t.Error("updateWorkStep takes a stepId; a write with none updates nothing and reports success")
	}
}

// TestAnExhaustedBudgetAsksAboutMoneyRatherThanRetrying is the non-local
// token-budget edge.
//
// OpenAI reports a spent balance as an HTTP 429 -- the same status as ordinary
// rate limiting -- so before the budget rule this ran the full retry budget
// against a balance nobody was topping up, sat at `waiting` the whole time,
// and only then asked a person, with a symptom naming a blip. It parks on a
// `budget` approval now, which is the kind whose sentence is about money.
func TestAnExhaustedBudgetAsksAboutMoneyRatherThanRetrying(t *testing.T) {
	c := &countingClassifier{symptom: work.SymptomTransient}
	calls := closeWith(t, c, failedRun("v1:work:run:c5",
		"429 You exceeded your current quota (insufficient_quota)", 1, 3))

	if c.calls != 0 {
		t.Fatalf("the classifier was called %d time(s); money is not a thing to ask a model about", c.calls)
	}
	if !anyCallNamed(calls, "createWorkApproval") {
		t.Fatalf("no approval was raised, so nobody was told the money ran out: %v", calls)
	}
	_, approval := argsOf(t, lastCallNamed(t, calls, "createWorkApproval"))
	if approval["kind"] != work.ApprovalKindBudget {
		t.Fatalf("approval kind = %v, want %q", approval["kind"], work.ApprovalKindBudget)
	}
	_, args := argsOf(t, lastCallNamed(t, calls, "updateWorkRun"))
	waiting, _ := args["waitingOn"].(map[string]any)
	if waiting["kind"] != WaitKindApproval {
		t.Fatalf("waitingOn.kind = %v, want %q: waiting on a person is a stated wait, not a silent one", waiting["kind"], WaitKindApproval)
	}
	if waiting["resumeAt"] != nil {
		t.Error("a budget wait must carry no resumeAt: only a person changes a balance, and polling one burns a dispatch to rediscover the same number")
	}
	// THE ORDER IS LOAD-BEARING here too: a run parked on an approval id that
	// does not exist waits on nothing.
	if indexOfCall(calls, "createWorkApproval") > indexOfCall(calls, "updateWorkRun") {
		t.Fatalf("the run parked before the approval existed: %v", calls)
	}
}

// A context window that stayed full after the tool loop compressed it is a
// real edge with a clear gate, not a timeout and not a retry. It reaches this
// path only when compressing freed nothing (integrations/agent's handoff), at
// which point the same bytes meet the same limit forever.
func TestAFullContextWindowParksOnAPersonRatherThanRetrying(t *testing.T) {
	c := &countingClassifier{symptom: work.SymptomTransient}
	calls := closeWith(t, c, failedRun("v1:work:run:c6",
		"prompt is too long: 216000 tokens > 200000 maximum", 1, 3))

	if c.calls != 0 {
		t.Fatalf("the classifier was called %d time(s)", c.calls)
	}
	_, approval := argsOf(t, lastCallNamed(t, calls, "createWorkApproval"))
	if approval["kind"] != work.ApprovalKindFeedback {
		t.Fatalf("approval kind = %v, want %q", approval["kind"], work.ApprovalKindFeedback)
	}
	_, step := argsOf(t, lastCallNamed(t, calls, "updateWorkStep"))
	if step["symptom"] != string(work.SymptomHuman) {
		t.Fatalf("step symptom = %v, want %q", step["symptom"], work.SymptomHuman)
	}
}

// TestNoStatusTransitionIsEverBothWaitingAndTerminal is the invariant behind
// every test above, asserted over all of them at once.
//
// The bug was not that one status was wrong. It was that two readers of the
// same run disagreed about whether it was over, and the reader a person looks
// at said the patient thing. So: whatever a failure classifies as, the run
// ends up in exactly one of those two shapes -- parked with a stated wait and
// no finishedAt, or terminal with a finishedAt and no wait. Never both, and
// never a `waiting` row that also looks finished.
func TestNoStatusTransitionIsEverBothWaitingAndTerminal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		message string
	}{
		{"provider timeout", "context deadline exceeded"},
		{"terminal composition", "composition_failed: nothing rendered"},
		{"self-imposed clock", "agent: turn wallclock timeout after 3m0s"},
		{"exhausted budget", "429 insufficient_quota"},
		{"full context window", "context_length_exceeded"},
		{"novel", "the vendor said something nobody has a rule for"},
		{"permission", "permission denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &countingClassifier{symptom: work.SymptomPlan}
			calls := closeWith(t, c, failedRun("v1:work:run:inv", tc.message, 1, 3))
			_, args := argsOf(t, lastCallNamed(t, calls, "updateWorkRun"))
			status, _ := args["status"].(string)
			parked := status == "waiting"

			if parked {
				if args["waitingOn"] == nil {
					t.Fatal("a waiting run with no waitingOn is the silent stuckness this epic is about")
				}
				if args["finishedAt"] != nil {
					t.Fatal("a waiting run must not carry finishedAt: every terminal-run reader would treat it as done")
				}
				return
			}
			if status != "failed" {
				t.Fatalf("status = %q, want waiting or failed", status)
			}
			if args["finishedAt"] == nil {
				t.Fatal("a failed run finishes")
			}
			if args["waitingOn"] != nil {
				t.Fatalf("a failed run also parked on %v", args["waitingOn"])
			}
		})
	}
}

// TestATerminalFailureStaysJournaledAndReplayable pins that taking a failure
// terminal does not cost the run its journal.
//
// Replay reads the run and its step rows (component/work's DecideServe over
// the journaled model calls), so a terminal close has to leave both behind: a
// run row that still names its chainHead and stepOrder, and the step row that
// failed. Writing a bare status would make the run unreplayable and
// unreadable at the same time.
func TestATerminalFailureStaysJournaledAndReplayable(t *testing.T) {
	run := failedRun("v1:work:run:c7", "composition_failed: nothing rendered", 1, 0)
	// The executor records the order as it goes; the fixture above only
	// records the failure, so the prefix is set here to be the thing under
	// assertion rather than an accident of the helper.
	run.StepOrder = []string{"gather", "compose"}
	calls := closeWith(t, &countingClassifier{}, run)

	if !anyCallNamed(calls, "updateWorkStep") {
		t.Fatalf("the failed step was not journaled: %v", calls)
	}
	_, args := argsOf(t, lastCallNamed(t, calls, "updateWorkRun"))
	order, _ := args["stepOrder"].([]any)
	if len(order) != 2 || order[0] != "gather" || order[1] != "compose" {
		t.Errorf("stepOrder = %v, want the journaled prefix: replay walks it", args["stepOrder"])
	}
	if _, ok := args["chainHead"]; !ok {
		t.Error("the run's chainHead must survive a terminal close")
	}
}

// TestTheClassifierRunsAtTheCheapestLevel asserts the tier from OUTSIDE the
// implementation. It is on the interface because the cheapest tier is a
// property of the design -- the answer is one of five words and the acts it
// selects are all bounded -- not an implementation detail a later reader may
// quietly change.
func TestTheClassifierRunsAtTheCheapestLevel(t *testing.T) {
	c := &countingClassifier{}
	if got := c.Level(); got != airoute.LevelFast {
		t.Fatalf("classifier level = %q, want %q", got, airoute.LevelFast)
	}
}

func anyCallNamed(calls []string, name string) bool {
	return indexOfCall(calls, name) >= 0
}

func indexOfCall(calls []string, name string) int {
	for i, c := range calls {
		if strings.HasPrefix(c, name+"(") {
			return i
		}
	}
	return -1
}

func lastCallNamed(t *testing.T, calls []string, name string) string {
	t.Helper()
	for i := len(calls) - 1; i >= 0; i-- {
		if strings.HasPrefix(calls[i], name+"(") {
			return calls[i]
		}
	}
	t.Fatalf("no %s call in %v", name, calls)
	return ""
}
