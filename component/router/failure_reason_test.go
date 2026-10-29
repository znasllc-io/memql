package router

import (
	"fmt"
	"strings"
	"testing"
)

// machineFailure stands in for component/worker's ModelCallFailed: the router
// names a failure by any Code() it can find, and this module does not import
// the worker package.
type machineFailure struct{ code, text string }

func (e machineFailure) Error() string { return e.text }
func (e machineFailure) Code() string  { return e.code }

// A local model that a machine started and then failed is named by the
// machine's code, in the router's words, and never by the runtime's text. On
// 2026-10-28 two such failures read `failed when called: other`.
func TestAMachineFailureIsNamedOnTheDecisionLine(t *testing.T) {
	err := fmt.Errorf("fleet call: %w", machineFailure{code: "runtime_error",
		text: "laptop: runtime_error worker: model call failed: ollama: /api/chat returned 400: failed to parse grammar"})
	got := FailureReason(err)
	if got != "failed when called: runtime_error: the machine's model runtime failed the call" {
		t.Fatalf("FailureReason = %q", got)
	}
	if strings.Contains(got, "ollama") {
		t.Fatal("the runtime's text belongs on errorMessage, not on the decision line")
	}
}
