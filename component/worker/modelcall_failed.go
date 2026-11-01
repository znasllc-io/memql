package worker

import (
	"errors"
	"regexp"
)

// modelcall_failed.go -- a model call a machine STARTED and then failed, with
// the machine's own stable code.
//
// The Cockpit ends every failed call with a code naming a different fix
// (cockpit modelcall's Code* constants: runtime_error, timeout, worker_stopped
// and the refusals). Before this type the code stopped at the stream: the
// handle answered a bare fmt error, the holding replica replaced the code with
// a generic model_call_failed, and the router -- which names a failure by any
// Code() it can find and otherwise by matching the text -- recorded `other`.
// On 2026-10-28 that is how two local models' grammar refusals reached the
// decision record of a parked pipeline diagnosis as `failed when called:
// other`, with the Ollama text readable only on the run's observations.
//
// The TEXT keeps each path's shape -- "worker: model call failed: <message>"
// on the replica holding the stream, "<machine>: <code> <message>" on the
// replica that forwarded -- except that the forwarded form now names the
// machine's own code where it used to say model_call_failed.

// ModelCallFailed is a model call a machine started and then failed.
type ModelCallFailed struct {
	// Forwarded marks the sender's form of a failure that crossed to the
	// replica holding the machine's stream: it names the machine in front.
	Forwarded bool
	// Machine is the machine's label on the forwarded form.
	Machine string
	// ErrorCode is the machine's code from ModelCallEnd; empty when it sent
	// none.
	ErrorCode string
	// Message is the machine's own error text.
	Message string
}

func (e *ModelCallFailed) Error() string {
	if e.Forwarded {
		return e.Machine + ": " + e.ErrorCode + " " + e.Message
	}
	return "worker: model call failed: " + e.Message
}

// Code is the stable machine-readable tag the router names a failure by.
//
// It is repeated only when it IS a stable word -- lowercase letters and
// underscores, at most forty -- because it lands on the decision line, which a
// person other than the machine's owner may read when the machine is lent to
// them, and that line carries codes and the router's own words, never an
// error's text (component/router's failure_reason.go). Anything else a machine
// sent reads as the generic model_call_failed; the full text stays on the
// error and the row's errorMessage. Empty stays empty, so the router falls
// back to its own category of the error.
func (e *ModelCallFailed) Code() string {
	if e.ErrorCode == "" || stableModelCallCode.MatchString(e.ErrorCode) {
		return e.ErrorCode
	}
	return "model_call_failed"
}

var stableModelCallCode = regexp.MustCompile(`^[a-z][a-z_]{0,39}$`)

// ModelCallCodeInsufficientMemory is a machine declining a call for memory
// (the Cockpit's modelcall.CodeInsufficientMemory).
const ModelCallCodeInsufficientMemory = "insufficient_memory"

// ModelCallNeverRan reports a machine stating that it handed this call to no
// runtime, so the call may be offered to another machine.
//
// refused_before_start must never be guessed (node.proto), and this is not a
// guess: the machine sends this one code from its admission gate and from nowhere
// a runtime could have been reached. It is deliberately not a list of codes that
// look safe. Any other code, a code this engine does not know, and an error that
// is not a machine's all read as a call that may have run, which is the safe
// reading: another machine is not tried.
func ModelCallNeverRan(err error) bool {
	var failed *ModelCallFailed
	return errors.As(err, &failed) && failed.ErrorCode == ModelCallCodeInsufficientMemory
}
