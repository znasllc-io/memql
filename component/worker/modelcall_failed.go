package worker

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
// The TEXT is unchanged from what each path printed before, so logs and the
// errorMessage a person already reads look the same; only Code() is new.

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
func (e *ModelCallFailed) Code() string { return e.ErrorCode }
