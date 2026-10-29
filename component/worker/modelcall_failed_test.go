package worker

import (
	"context"
	"errors"
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
