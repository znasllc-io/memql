package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/common"
)

// A failed goal operation is evidence for the next reasoning turn. It must
// not automatically become a human question or a failed goal. This exercises
// both transport lanes with persisted authority and no originating session.
func TestOwnedWorkRecoversTaskErrorBeforeRequestingHumanInput(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("streaming=%v", streaming), func(t *testing.T) {
			ctx, err := auth.ContextWithPersistedOwner(context.Background(), "owner", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			ctx = common.ContextWithRun(ctx, common.RunContext{RunId: "adopted-research", GoalId: "research", OwnerUserId: "owner"})
			p := &outcomeProvider{steps: [][]common.ToolCall{
				{{ID: "extract", Name: "extractPDF", Arguments: `{"file":"paper.pdf"}`}},
				{{ID: "ocr", Name: "readScannedPDF", Arguments: `{"file":"paper.pdf"}`}},
				outcomeCall(`{"status":"complete","response":"The scanned paper was read using OCR and its contents verified."}`),
			}}
			r := testReplier()
			saved := &workPromptEngine{}
			r.engine = saved
			effects := 0
			r.stamper = newToolRecorder(&scriptedExecutor{fn: func(call int, name string) (string, error) {
				effects++
				if call == 0 && name == "extractPDF" {
					return "", errors.New("PDF has no text layer; scanned pages require OCR")
				}
				if call == 1 && name == "readScannedPDF" {
					return `{"pages":12,"verified":true,"text":"Measured findings from the paper"}`, nil
				}
				t.Fatalf("unexpected action or human escalation: %s", name)
				return "", nil
			}}, r.logger)
			turn := turnContext{AgentId: "assistant", RunId: "adopted-research", OwnerUserId: "owner", IsWorkExecution: true}
			tools := []common.ToolDefinition{workResponseToolDefinition()}
			if streaming {
				_, err = r.runStreamingToolLoop(ctx, p, nil, tools, &captureSink{}, time.Now(), "recovery", turn)
			} else {
				_, err = r.runNonStreamingToolLoop(ctx, p, nil, nil, tools, &captureSink{}, time.Now(), "recovery", turn)
			}
			if err != nil {
				t.Fatal(err)
			}
			if effects != 2 || p.calls != 3 {
				t.Fatalf("work did not recover in place: effects=%d model calls=%d", effects, p.calls)
			}
			trace := fmt.Sprint(p.messages)
			if !strings.Contains(trace, "no text layer") || !strings.Contains(trace, `"verified":true`) {
				t.Fatal("the model lost the failure evidence or successful recovery receipt")
			}
			if len(saved.savedContinuation) == 0 {
				t.Fatal("recovery progress was not saved")
			}
		})
	}
}
