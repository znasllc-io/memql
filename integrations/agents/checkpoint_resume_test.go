package agents

import (
	"context"
	"errors"
	"testing"

	"github.com/znasllc-io/memql/component/memql"
)

type continuationEngine struct {
	memql.IntegrationEngineAccess
	request memql.CheckpointResumeRequest
	calls   int
	err     error
}

func (e *continuationEngine) PrepareWorkContinuation(ctx context.Context, request memql.CheckpointResumeRequest) (context.Context, error) {
	e.request, e.calls = request, e.calls+1
	return ctx, e.err
}

func TestOnlyAgentTurnsCanPrepareAnOwnedCheckpoint(t *testing.T) {
	engine := &continuationEngine{}
	i := &Integration{engine: engine}
	request := memql.CheckpointResumeRequest{RunID: "run", StepKey: "step", ApprovalID: "answered"}
	for _, capability := range []string{"invoke", "ai", "produceArtifact", "unknown"} {
		_, accepted, err := i.PrepareCheckpointResume(context.Background(), capability, request)
		if err != nil || accepted || engine.calls != 0 {
			t.Fatalf("%s gained a continuation: %v %v", capability, accepted, err)
		}
	}
	_, accepted, err := i.PrepareCheckpointResume(context.Background(), "runAgentTurn", request)
	if err != nil || !accepted || engine.calls != 1 || engine.request != request {
		t.Fatalf("checkpoint preparation: %v %v", accepted, err)
	}
	engine.err = errors.New("checkpoint unavailable")
	if _, accepted, err := i.PrepareCheckpointResume(context.Background(), "runAgentTurn", request); err == nil || accepted {
		t.Fatal("checkpoint failure accepted")
	}
	i.engine = nil
	if _, accepted, err := i.PrepareCheckpointResume(context.Background(), "runAgentTurn", request); err == nil || accepted {
		t.Fatal("missing engine accepted")
	}
}
