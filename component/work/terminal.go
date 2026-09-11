package work

// terminal.go -- the failures that are an OUTCOME rather than a symptom.
//
// # A goal has no duration, so nothing here is a clock
//
// A goal runs until the work is done. It may take a minute or a month, and how
// long it will take cannot be known when it starts, because the steps that
// remain are discovered by doing the earlier ones. So MemQL imposes no
// duration ceiling on a goal or a run -- not a fixed timeout and not a
// wall-clock cancel -- and this file adds none: the only things that end a run
// are a person cancelling it and a real failure.
//
// v1:work:goal.ceilings does carry a `wallClockMs`, and it stays a RECORDED
// figure rather than an enforced one. Nothing reads it to cancel anything,
// and a reader of this file should not make it the first such caller.
//
// What it does add is the other half of that promise. Once the app stops
// imposing clocks, the clocks that remain belong to somebody else -- a
// provider's request limit, a model's context window, a budget -- and every
// one of them has to arrive as the thing it is. The failure this file exists
// to prevent is the one that shipped: a deadline MemQL set for itself expired,
// the symptom table read the words "deadline exceeded", called it a blip, and
// parked the run at `waiting` on a retry -- while the composition it was
// composing had already been written to the database as `failed`. Two rows
// disagreeing about whether the work is over, and the one a person reads said
// the patient thing.
//
// # Why this is not a sixth symptom
//
// The five symptoms in symptom.go are all answers to "what should the loop do
// about this?", and each has an act. These failures have no act. The
// composition row is already terminal; the file does not exist; re-running the
// step reads the same `failed` row and refuses. There is nothing to retry,
// nothing to repair, nothing to replan, and nothing a person can decide that
// would change the answer -- so the honest state is the terminal one, and
// v1:work:run.errorCode is where the reason goes.
//
// # The codes are stable strings, matched from the message
//
// For InferenceRefusalCode's reason, restated because it applies unchanged:
// the two ends are in different modules. The compose integration raises the
// failure and the executor that decides what the run does about it sees only
// the string the executor recorded. So the code is a STABLE WORD carried in
// the message, asserted at the raising end, and matched here -- a contract
// rather than a guess about wording. And it is a CONTAINS match, because the
// message travels through wrapping ("automation step 3: ...") on the way.

import "strings"

// The terminal failure codes. A CLOSED SET, and each one means "the work this
// step was doing is over, and no attempt at it will end differently".
const (
	// TerminalSelfTimeout is a clock MEMQL SET FOR ITSELF running out.
	//
	// It is terminal rather than transient for a reason that is easy to
	// state and was got wrong: retrying against it cannot help. A provider
	// timeout is evidence about the far side and the same call may well
	// work again; a self-imposed deadline is evidence about nothing except
	// the number we chose, and the next attempt gets the same number. So
	// re-dispatching burns the retry budget to arrive at the same second,
	// and the run reads `waiting` the whole time it is doing it.
	//
	// The goal is that no long-running work ever produces this -- the
	// duration caps that killed hour-long compositions are gone, and none
	// replaced them. The code stays because "no self-imposed clock exists
	// today" is a property of the tree that a future caller can break
	// quietly, and the failure mode when they do should be an honest
	// terminal row rather than a silent retry loop.
	TerminalSelfTimeout = "self_timeout"

	// TerminalCompositionFailed is the Materializer having already written
	// its own terminal state.
	//
	// integrations/compose's failComposition sets the composition row to
	// `failed` with its reason and THEN returns the error. By the time the
	// work spine sees it, the terminal record exists and the output file
	// does not. Retrying the step re-reads a `failed` composition, and the
	// run parked at `waiting` in the meantime is telling a person that
	// something is still in flight when the row it came from says it is
	// finished and broken.
	TerminalCompositionFailed = "composition_failed"
)

// terminalFailureCodes is the matcher's set. Longest first, so a code is
// never shadowed by a shorter one sharing its prefix.
var terminalFailureCodes = []string{
	TerminalCompositionFailed,
	TerminalSelfTimeout,
}

// selfImposedClockMarkers are the sentences a MemQL-imposed clock produces
// that do NOT carry a code, because they are raised in trees that cannot
// import this package.
//
// Each entry is a sentinel some other module declares as a constant and pins
// with its own test, listed here rather than matched loosely: "timeout" on its
// own would catch every provider timeout in the tree and make all of them
// terminal, which is the opposite of true. Only a clock WE set belongs here.
var selfImposedClockMarkers = []string{
	// integrations/agent's turnWallclockSentinel. A work-execution turn no
	// longer carries a wallclock at all, so this is reachable only when an
	// operator sets one deliberately -- and then it should say so rather
	// than impersonate a network blip.
	"turn wallclock timeout",
}

// TerminalFailureCode reports which terminal failure an error message carries,
// if any. ok=false means "this is not one of these", and the caller goes on to
// the symptom table -- which is where every ordinary failure is decided.
func TerminalFailureCode(errorMessage string) (string, bool) {
	msg := strings.ToLower(errorMessage)
	if msg == "" {
		return "", false
	}
	for _, code := range terminalFailureCodes {
		if strings.Contains(msg, code) {
			return code, true
		}
	}
	for _, marker := range selfImposedClockMarkers {
		if strings.Contains(msg, marker) {
			return TerminalSelfTimeout, true
		}
	}
	return "", false
}

// TerminalReason is the verdict in words a person reads on the run. It says
// what is true and, for both codes, why another attempt is not the answer --
// because the reader's first instinct on seeing `failed` is to press retry.
func TerminalReason(code string) string {
	switch code {
	case TerminalCompositionFailed:
		return "the composition recorded its own failure, so the document does not exist and re-running the step reads the same failed record"
	case TerminalSelfTimeout:
		return "a deadline this system set for itself ran out; the work was not given longer, and another attempt would be given the same deadline"
	default:
		return "the work this step was doing cannot end differently on another attempt"
	}
}
