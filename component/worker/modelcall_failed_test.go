package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
)

// A call the machine started and then failed keeps the machine's own code. On
// 2026-10-28 two local models failed a pipeline diagnosis with Ollama's
// grammar refusal, and the decision record called both failures `other`
// because nothing between the machine and the router carried `runtime_error`.
func TestAFinishedCallWithAnErrorKeepsTheMachinesCode(t *testing.T) {
	session, _ := newToolStreamTestSession()
	defer session.cancel()
	handle, err := session.openModelCall(context.Background(), ModelCallRequest{RequestId: "failed", Model: "local", Kind: "chat"})
	if err != nil {
		t.Fatal(err)
	}
	session.handleModelCallEnd(&memqlv1.ModelCallEnd{RequestId: "failed", FinishReason: ModelFinishError,
		ErrorCode: "runtime_error", Error: "ollama: /api/chat returned 400"})
	_, err = handle.Wait(context.Background())
	var failed *ModelCallFailed
	if !errors.As(err, &failed) {
		t.Fatalf("a machine's failure must stay typed: %T %v", err, err)
	}
	if failed.Code() != "runtime_error" {
		t.Fatalf("code = %q, want the machine's runtime_error", failed.Code())
	}
	if err.Error() != "worker: model call failed: ollama: /api/chat returned 400" {
		t.Fatalf("the text a reader already knows must not change: %q", err.Error())
	}
}

// The forwarded form names the machine in front, as the sender has always
// written it.
func TestAForwardedFailureNamesTheMachine(t *testing.T) {
	err := &ModelCallFailed{Forwarded: true, Machine: "laptop", ErrorCode: "runtime_error", Message: "ollama: /api/chat returned 400"}
	if err.Error() != "laptop: runtime_error ollama: /api/chat returned 400" || err.Code() != "runtime_error" {
		t.Fatalf("forwarded failure reads %q / %q", err.Error(), err.Code())
	}
}

// A machine's code reaches the decision line, which another person may read
// when the machine is lent to them, and that line is a stable code and the
// router's own words, never the error's text (component/router's
// failure_reason.go). A code that is not a stable word is not repeated.
func TestAMachineCodeThatIsNotAStableWordIsNotRepeated(t *testing.T) {
	for code, want := range map[string]string{
		"runtime_error":                     "runtime_error",
		"timeout":                           "timeout",
		"":                                  "",
		"see https://example.test/ secrets": "model_call_failed",
		"Runtime_Error":                     "model_call_failed",
		strings.Repeat("a", 41):             "model_call_failed",
	} {
		if got := (&ModelCallFailed{ErrorCode: code}).Code(); got != want {
			t.Errorf("Code() for %q = %q, want %q", code, got, want)
		}
	}
}

// The re-pick predicate must never be guessed (CLAUDE.md), so exactly one code
// means nothing ran: the machine declining for memory, which it sends only before
// handing the call to a runtime. Everything else may have generated, and an
// unknown code or a bare error is the same as having run.
func TestOnlyAMemoryRefusalSaysTheCallNeverRan(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"the machine declined for memory", &ModelCallFailed{ErrorCode: "insufficient_memory"}, true},
		{"the same on the forwarded form", &ModelCallFailed{Forwarded: true, Machine: "laptop", ErrorCode: "insufficient_memory"}, true},
		{"wrapped on its way up", fmt.Errorf("fleet call: %w", &ModelCallFailed{ErrorCode: "insufficient_memory"}), true},
		{"a runtime that failed", &ModelCallFailed{ErrorCode: "runtime_error"}, false},
		{"a timeout", &ModelCallFailed{ErrorCode: "timeout"}, false},
		{"a worker that stopped", &ModelCallFailed{ErrorCode: "worker_stopped"}, false},
		{"no code at all", &ModelCallFailed{}, false},
		{"not a machine's failure", errors.New("insufficient_memory"), false},
		{"nothing", nil, false},
	} {
		if got := ModelCallNeverRan(tc.err); got != tc.want {
			t.Errorf("%s: ModelCallNeverRan = %v, want %v", tc.name, got, tc.want)
		}
	}
}
