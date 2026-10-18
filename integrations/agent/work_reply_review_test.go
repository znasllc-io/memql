package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/core/common"
)

type replyReviewEngine struct {
	workPromptEngine
	calls int
	fail  bool
}

func (e *replyReviewEngine) Execute(ctx context.Context, query string) (any, error) {
	e.calls++
	run, _ := common.RunFromContext(ctx)
	actor, _ := auth.AccessFromContext(ctx)
	if run.RunId != "run" || actor == nil || actor.UserId != "owner" || !strings.HasPrefix(query, "builtin work.workReviewReply(") {
		return nil, errors.New("lost owned execution context")
	}
	if e.fail {
		return nil, errors.New("review unavailable")
	}
	// The real CognitionEngineAdapter retains the typed builtin node set under
	// data; a flat fake concealed the production result-wrapper boundary.
	return map[string]any{"data": map[string]memorynodes.MemoryNode{
		"work-reviewed-reply": {ID: "work-reviewed-reply", Payload: json.RawMessage(`{"reply":"The recorded observation does not establish a cause."}`)},
	}}, nil
}

func TestOwnedAnswerWaitsForEvidenceReviewBeforeDelivery(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "reviewed", true: "failed"}[fail], func(t *testing.T) {
			engine := &replyReviewEngine{fail: fail}
			r := newTestReplier(engine)
			ctx := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), "owner"), common.RunContext{RunId: "run", GoalId: "goal", OwnerUserId: "owner"})
			sink := &captureSink{}
			held := &replyReviewSink{DeltaSink: sink}
			held.ToolCall("read", "readStatus", `{}`)
			held.ToolResult("read", `{"status":"degraded"}`, "")
			held.TextDelta("An invented cause.")
			held.ToolCall("answer", RespondToUserToolName, `{"response":"An invented cause."}`)
			require.Empty(t, sink.texts)
			require.Equal(t, []string{"readStatus"}, sink.toolCalls)
			require.Equal(t, 1, sink.toolResults)
			result, err := r.reviewWorkResult(ctx, &TurnResult{FinalText: "An invented cause."}, held)
			require.Equal(t, 1, engine.calls)
			if fail {
				require.Error(t, err)
				require.Nil(t, result)
				require.Empty(t, sink.texts)
				require.Equal(t, []string{"readStatus"}, sink.toolCalls)
			} else {
				require.NoError(t, err)
				require.Equal(t, []string{result.FinalText}, sink.texts)
				require.NotContains(t, result.FinalText, "invented")
				require.Equal(t, []string{"readStatus", RespondToUserToolName}, sink.toolCalls)
			}
		})
	}
}
