package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

func (r *Replier) workTrainingContext(ctx context.Context, msg *memqlv1.AgentGenerateTurnMsg) ([]map[string]any, error) {
	run, _ := common.RunFromContext(ctx)
	if !isOwnedWorkExecution(ctx) || run.Mode == common.RunModeReplay {
		return nil, nil
	}
	// The hint is authored query text, never authority. The native reader
	// derives run, goal and owner from the actual execution context.
	section := msg.Hints["work_section_prompt"]
	if section == "" {
		section = latestUserQuery(msg)
	}
	bounded := func(s string) string {
		if len(s) <= 4000 {
			return s
		}
		s = s[:4000]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
		return s
	}
	var passages []map[string]any
	_, err := workflowhost.Run(ctx, "agentWorkTrainingContext", nil, workflowhost.Options{Logger: r.logger, Operations: map[string]workflowhost.Operation{
		"agentWorkTrainingFacts": func(ctx context.Context, _ map[string]any) (any, error) {
			result, err := r.engine.Execute(ctx, "builtin libraryWorkTrainingContext()")
			if err != nil {
				return nil, err
			}
			rows := trainingResultRows(result)
			if len(rows) == 0 {
				return nil, nil
			}
			if len(rows) != 1 {
				return nil, fmt.Errorf("invalid Training scope response")
			}
			goal, _ := rows[0]["goal"].(string)
			return map[string]any{"goal": bounded(goal), "section": bounded(section)}, nil
		},
		"agentReadWorkTraining": func(ctx context.Context, args map[string]any) (any, error) {
			call, err := parser.RenderCall("libraryOwnedTrainingLessons", args)
			if err != nil {
				return nil, err
			}
			result, err := r.engine.Execute(ctx, "builtin "+call)
			if err != nil {
				return nil, err
			}
			passages = trainingResultRows(result)
			return passages, nil
		},
	}})
	return passages, err
}

// A cancelled embedding/read can arrive before the first reasoning call. Keep
// the current full paired continuation before asking about a native ceiling;
// never replace an existing saved tail with just the new section prompt.
func (r *Replier) workTrainingFailure(ctx context.Context, failure error, messages []common.ChatMessage) error {
	var ceiling *memql.RunCeilingError
	if !errors.As(failure, &ceiling) {
		if reader, ok := r.engine.(interface{ WorkCallFailure(context.Context) error }); ok {
			if budget := reader.WorkCallFailure(ctx); budget != nil {
				failure = budget
			}
		}
	}
	if !errors.As(failure, &ceiling) {
		return fmt.Errorf("prepare reviewed Training context: %w", failure)
	}
	savedCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	saved, err := r.restoreAfterQuestion(savedCtx, messages)
	if err != nil {
		return fmt.Errorf("%w: restore before Training budget pause: %v", memql.ErrWorkCheckpointRequired, err)
	}
	return r.pauseAtBudgetBoundary(ctx, failure, saved)
}

func appendWorkTraining(data map[string]any, passages []map[string]any) {
	if len(passages) == 0 {
		return
	}
	existing, _ := data["knowledge"].([]map[string]any)
	data["knowledge"] = append(existing, passages...)
	data["systemKnowledge"] = data["knowledge"]
}

// CognitionEngineAdapter wraps native builtin output under data. Unwrap that
// axis before the canonical node-set decoder; PreserveOrder answers may retain
// their payload envelope, unlike shaped query rows.
func trainingResultRows(result any) []map[string]any {
	if envelope, ok := result.(map[string]any); ok {
		if data, exists := envelope["data"]; exists {
			result = data
		}
	}
	rows := memql.MaterializeRows(result)
	for n, row := range rows {
		if payload, ok := row["payload"].(map[string]any); ok {
			rows[n] = maps.Clone(row)
			maps.Copy(rows[n], payload)
		}
	}
	return rows
}
