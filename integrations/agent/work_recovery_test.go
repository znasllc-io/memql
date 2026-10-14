package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
)

type progressRecoveryEngine struct {
	workPromptEngine
	limit, used, saves         int
	budgetError                error
	failSave, failQuestionSave bool
}

func newProgressRecoveryEngine(t *testing.T) *progressRecoveryEngine {
	t.Helper()
	registry := memql.NewPromptRegistry()
	_, err := memql.LoadUnifiedPrompts(nil, registry, template.New("partials"))
	require.NoError(t, err)
	return &progressRecoveryEngine{workPromptEngine: workPromptEngine{prompts: registry}, limit: 12}
}

func (e *progressRecoveryEngine) WorkModelCallBudget(context.Context) (int, int, error) {
	return e.limit, e.used, e.budgetError
}

func (e *progressRecoveryEngine) SaveWorkContinuation(_ context.Context, messages []common.ChatMessage) error {
	if e.failSave {
		return fmt.Errorf("recovery checkpoint unavailable")
	}
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			if e.failQuestionSave && call.Name == "requestUserFeedback" {
				return fmt.Errorf("question checkpoint unavailable")
			}
		}
	}
	e.saves++
	e.savedContinuation = append([]common.ChatMessage(nil), messages...)
	return nil
}

type progressRecoveryExecutor struct {
	questions, operations int
	questionArgs          map[string]any
	answered              bool
	beforeQuestion        func()
}

func (*progressRecoveryExecutor) Execute(context.Context, string) (any, error) { return nil, nil }
func (e *progressRecoveryExecutor) ExecuteToolByName(_ context.Context, name string, args map[string]any) (string, error) {
	if name == "requestUserFeedback" {
		if e.beforeQuestion != nil {
			e.beforeQuestion()
		}
		e.questions++
		e.questionArgs = args
		if e.answered {
			return `{"data":[{"receipt":{"id":"receipt","payload":{"status":"answered","approvalId":"recovery-question","answer":{"text":"Use the verified record."},"decidedAt":"2026-10-14T12:00:00Z"}}}]}`, nil
		}
		return `{"data":[{"receipt":{"id":"receipt","payload":{"status":"awaiting_user","approvalId":"recovery-question"}}}]}`, nil
	}
	e.operations++
	if name == "discoverCapabilities" {
		return `{"capabilities":[]}`, nil
	}
	if name == "recallWorkHistory" {
		return `{"source":"same archived search evidence"}`, nil
	}
	// A read may advance the task without performing a write.
	return fmt.Sprintf(`{"verifiedPage":%v}`, args["page"]), nil
}

func progressRecoveryContext() (context.Context, turnContext) {
	ctx := common.ContextWithRun(auth.ContextWithUserActor(context.Background(), "owner"), common.RunContext{
		RunId: "recovery-run", GoalId: "recovery-goal", OwnerUserId: "owner", StepKey: "section",
	})
	return ctx, turnContext{AgentId: "assistant", RunId: "recovery-run", OwnerUserId: "owner", IsWorkExecution: true}
}

func progressRecoveryCall(id, name, args string) []common.ToolCall {
	return []common.ToolCall{{ID: id, Name: name, Arguments: args}}
}

func progressDiscoverySteps(count int) [][]common.ToolCall {
	var steps [][]common.ToolCall
	for n := 0; n < count; n++ {
		steps = append(steps, progressRecoveryCall(fmt.Sprintf("discovery-%d", n), "discoverCapabilities", `{"query":"requested operation"}`))
	}
	return steps
}

func progressRecoveryRun(t *testing.T, streaming bool, engine *progressRecoveryEngine, executor *progressRecoveryExecutor, steps [][]common.ToolCall, inspect func(int, []common.ChatMessage)) (*guidanceLoopProvider, *TurnResult, error) {
	t.Helper()
	provider := &guidanceLoopProvider{outcomeProvider: outcomeProvider{steps: steps}}
	provider.before = func(index int, messages []common.ChatMessage) {
		if inspect != nil {
			inspect(index, messages)
		}
		engine.used++
	}
	r := testReplier()
	r.engine = engine
	r.stamper = newToolRecorder(executor, r.logger)
	ctx, turn := progressRecoveryContext()
	messages := []common.ChatMessage{{Role: "system", Content: "Current installed instructions"}, {Role: "user", Content: "Complete the requested operation."}}
	tools := []common.ToolDefinition{workResponseToolDefinition()}
	var result *TurnResult
	var err error
	if streaming {
		result, err = r.runStreamingToolLoop(ctx, provider, messages, tools, &captureSink{}, time.Now(), "recovery", turn)
	} else {
		result, err = r.runNonStreamingToolLoop(ctx, provider, nil, messages, tools, &captureSink{}, time.Now(), "recovery", turn)
	}
	return provider, result, err
}

func TestWorkProgressRecoveryInstalledDSLPolicy(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		limit, used, discovery, repeat int
		review                         bool
		after, reviewed                int
		want                           string
	}{
		{"ordinary", 12, 1, 1, 1, false, 0, 0, "continue"},
		{"discovery", 12, 3, 3, 1, false, 0, 0, "review_stalled"},
		{"equivalent evidence", 12, 3, 0, 3, false, 0, 0, "review_stalled"},
		{"near cap", 12, 9, 0, 1, false, 0, 0, "review_budget"},
		{"productive after review", 12, 10, 0, 1, true, 0, 12, "continue"},
		{"continued stall", 12, 5, 5, 5, true, 2, 0, "ask"},
		{"last call reserved", 12, 11, 4, 4, true, 1, 12, "ask"},
		{"hard cap remains native", 12, 12, 5, 5, true, 2, 12, "continue"},
		{"raised total is not progress", 24, 5, 5, 5, true, 2, 12, "ask"},
		{"unbounded still detects loops", 0, 5, 5, 5, true, 2, 0, "ask"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := workProgressDecision(context.Background(), map[string]any{
				"limit": tc.limit, "used": tc.used, "discoveryRounds": tc.discovery, "repeatedRounds": tc.repeat,
				"reviewActive": tc.review, "stalledAfterReview": tc.after, "reviewedLimit": tc.reviewed,
			})
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestWorkProgressRecoveryPausesBothLoopsBeforeExhaustion(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, archived := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%v/archive=%v", streaming, archived), func(t *testing.T) {
				e := newProgressRecoveryEngine(t)
				x := &progressRecoveryExecutor{}
				steps, wantCalls := progressDiscoverySteps(5), 5
				if archived {
					steps, wantCalls = nil, 7
					for n := 0; n < wantCalls; n++ {
						name := "discoverCapabilities"
						if n%2 == 1 {
							name = "recallWorkHistory"
						}
						steps = append(steps, progressRecoveryCall(fmt.Sprint(n), name, `{"query":"requested operation"}`))
					}
				}
				x.beforeQuestion = func() {
					require.Contains(t, fmt.Sprint(e.savedContinuation), "requestUserFeedback", "checkpoint must precede the question")
					require.Contains(t, fmt.Sprint(e.savedContinuation), common.UnexecutedWorkToolResult)
				}
				reviews := 0
				p, _, err := progressRecoveryRun(t, streaming, e, x, steps, func(_ int, messages []common.ChatMessage) {
					state, _, err := recoveryState(messages)
					require.NoError(t, err)
					if state.ReviewActive {
						reviews++
						require.Contains(t, state.Instruction, "normal reasoning turn")
					}
				})
				var wait *work.HumanWait
				require.ErrorAs(t, err, &wait)
				require.Equal(t, "recovery-question", wait.ApprovalID)
				require.Equal(t, wantCalls, p.calls, "recovery must not add a critic call")
				require.Equal(t, wantCalls, e.used)
				require.Equal(t, 2, reviews, "two ordinary reasoning attempts receive recovery guidance")
				require.Equal(t, 1, x.questions)
				require.Equal(t, wantCalls, x.operations)
				require.Equal(t, "owner", x.questionArgs["ownerUserId"])
				require.Equal(t, "recovery-run", x.questionArgs["runId"])
			})
		}
	}
}

func TestWorkProgressRecoveryAllowsProductiveReadsAndMixedBatches(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, mode := range []string{"reads", "mixed", "near-cap"} {
			t.Run(fmt.Sprintf("stream=%v/%s", streaming, mode), func(t *testing.T) {
				e := newProgressRecoveryEngine(t)
				x := &progressRecoveryExecutor{}
				var steps [][]common.ToolCall
				count := 6
				if mode == "near-cap" {
					e.used, count = 8, 2
				}
				if mode == "mixed" {
					steps, count = progressDiscoverySteps(3), 2
				}
				for n := 0; n < count; n++ {
					calls := progressRecoveryCall(fmt.Sprintf("read-%d", n), "readPage", fmt.Sprintf(`{"page":%d}`, n))
					if mode == "mixed" {
						calls = append(calls, progressRecoveryCall(fmt.Sprintf("discover-%d", n), "discoverCapabilities", `{"query":"next supported action"}`)...)
					}
					steps = append(steps, calls)
				}
				steps = append(steps, outcomeCall(`{"status":"complete","response":"Verified the requested result."}`))
				p, result, err := progressRecoveryRun(t, streaming, e, x, steps, nil)
				require.NoError(t, err)
				require.Equal(t, len(steps), p.calls)
				require.Equal(t, "Verified the requested result.", result.FinalText)
				require.Zero(t, x.questions, "progress must not require a budget discussion")
			})
		}
	}
}

func recoveryMessages(t *testing.T, state workRecoveryState) []common.ChatMessage {
	t.Helper()
	raw, err := json.Marshal(state)
	require.NoError(t, err)
	return []common.ChatMessage{{Role: "system", Content: "Fresh installed instructions"}, {Role: "user", Content: "Original request"}, {Role: "system", Name: workRecoveryMarker, Content: string(raw)}}
}

func recoveryBatch(id, name, args, result string) []common.ChatMessage {
	return []common.ChatMessage{{Role: "assistant", ToolCalls: progressRecoveryCall(id, name, args)}, {Role: "tool", Name: name, ToolCallId: id, Content: result}}
}

func recoveryHarness(t *testing.T) (*Replier, *progressRecoveryEngine, *progressRecoveryExecutor, context.Context, turnContext) {
	t.Helper()
	e, x := newProgressRecoveryEngine(t), &progressRecoveryExecutor{}
	r := testReplier()
	r.engine, r.stamper = e, newToolRecorder(x, r.logger)
	ctx, turn := progressRecoveryContext()
	return r, e, x, ctx, turn
}

func TestWorkProgressRecoveryWatermarkSurvivesCompactionAndRetries(t *testing.T) {
	r, e, x, ctx, turn := recoveryHarness(t)
	messages := recoveryMessages(t, workRecoveryState{Version: 1})
	for n := 0; n < 3; n++ {
		args := `{"a":1,"b":2}`
		if n%2 == 1 {
			args = `{"b":2,"a":1}`
		}
		messages = append(messages, recoveryBatch(fmt.Sprint(n), "discoverCapabilities", args, `{"capabilities":[]}`)...)
		var err error
		messages, err = r.recoverWorkProgress(ctx, messages, turn, &captureSink{}, nil)
		require.NoError(t, err)
	}
	state, marker, err := recoveryState(messages)
	require.NoError(t, err)
	require.Equal(t, 3, state.DiscoveryRounds)
	require.Equal(t, 3, state.RepeatedRounds, "argument-key order and transport IDs are not new evidence")
	require.True(t, state.ReviewActive)
	// This is the compactor's authority-preserving seam: prior tool exchanges
	// may disappear, but the separately named system record survives intact.
	messages = append(messages[:2:2], messages[marker])
	saves := e.saves
	for n := 0; n < 3; n++ {
		messages, err = r.recoverWorkProgress(ctx, messages, turn, &captureSink{}, nil)
		require.NoError(t, err)
	}
	state, marker, err = recoveryState(messages)
	require.NoError(t, err)
	require.Equal(t, saves, e.saves, "pre/post-compaction and provider retry are not new work")
	require.Zero(t, state.StalledAfterReview)
	require.Equal(t, 3, state.DiscoveryRounds)
	require.Equal(t, 2, marker)
	require.Zero(t, x.questions)
	// Restore the compacted checkpoint on a fresh execution instance.
	r, e, x, ctx, turn = recoveryHarness(t)
	for n := 0; n < 2; n++ {
		messages = append(messages, recoveryBatch(fmt.Sprintf("resumed-%d", n), "discoverCapabilities", `{"a":1,"b":2}`, `{"capabilities":[]}`)...)
		messages, err = r.recoverWorkProgress(ctx, messages, turn, &captureSink{}, nil)
		if n == 0 {
			require.NoError(t, err)
		}
	}
	var wait *work.HumanWait
	require.ErrorAs(t, err, &wait)
	require.Equal(t, 1, x.questions)
	state, _, err = recoveryState(e.savedContinuation)
	require.NoError(t, err)
	require.Equal(t, 5, state.DiscoveryRounds)
}

func TestWorkProgressRecoveryFailuresNeverPublishAnUncheckpointedQuestion(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, failure := range []string{"budget-read", "initial-save", "question-save"} {
			t.Run(fmt.Sprintf("stream=%v/%s", streaming, failure), func(t *testing.T) {
				e := newProgressRecoveryEngine(t)
				wantCalls, wantError := 0, "recovery checkpoint unavailable"
				switch failure {
				case "budget-read":
					e.budgetError, wantError = fmt.Errorf("budget unavailable"), "budget unavailable"
				case "initial-save":
					e.failSave = true
				case "question-save":
					e.failQuestionSave, wantCalls, wantError = true, 5, "question checkpoint unavailable"
				}
				x := &progressRecoveryExecutor{}
				p, _, err := progressRecoveryRun(t, streaming, e, x, progressDiscoverySteps(5), nil)
				require.ErrorContains(t, err, wantError)
				require.ErrorIs(t, err, memql.ErrWorkCheckpointRequired, "authored retry/continue must not skip a failed recovery boundary")
				require.Equal(t, wantCalls, p.calls)
				require.Zero(t, x.questions)
			})
		}
	}
}

type progressRecoveryReadEngine struct {
	*progressRecoveryEngine
	read                           func(context.Context) (int, int, error)
	budgetFailure, suspensionError error
	pauseAttempts, parked          int
}

func (e *progressRecoveryReadEngine) WorkModelCallBudget(ctx context.Context) (int, int, error) {
	return e.read(ctx)
}

func (e *progressRecoveryReadEngine) WorkCallFailure(ctx context.Context) error {
	if ctx.Err() == nil {
		return nil
	}
	return e.budgetFailure
}

func (e *progressRecoveryReadEngine) PauseWorkForBudget(_ context.Context, _ *memql.RunCeilingError, messages []common.ChatMessage) error {
	e.pauseAttempts++
	if e.suspensionError != nil {
		return e.suspensionError
	}
	e.parked++
	e.savedContinuation = append([]common.ChatMessage(nil), messages...)
	return &work.HumanWait{ApprovalID: "native-recovery-budget"}
}

func TestWorkProgressRecoveryReadCancellationKeepsItsNativeMeaning(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, boundary := range []string{"budget-read", "policy"} {
			for _, kind := range []string{"native-deadline", "owner-stop", "provider-deadline", "caller-cancel", "suspension-failure", "read-failure", "read-timeout", "cancel-observer-failure"} {
				if boundary == "policy" && (kind == "read-failure" || kind == "read-timeout") {
					continue
				}
				t.Run(fmt.Sprintf("stream=%v/%s/%s", streaming, boundary, kind), func(t *testing.T) {
					ctx, turn := progressRecoveryContext()
					ctx, cancel := context.WithCancelCause(ctx)
					defer cancel(nil)
					ceiling := &memql.RunCeilingError{RunId: "recovery-run", StepKey: "section", Breach: memql.RunCeilingBreach{Ceiling: "wallClock", Limit: "1000ms", Actual: "1001ms"}}
					engine := &progressRecoveryReadEngine{progressRecoveryEngine: newProgressRecoveryEngine(t)}
					cause := error(context.Canceled)
					switch kind {
					case "native-deadline", "suspension-failure", "read-failure":
						cause = fmt.Errorf("native run deadline: %w", context.DeadlineExceeded)
						engine.budgetFailure = ceiling
					case "owner-stop":
						cause = &memql.WorkCancelledError{By: "owner"}
					case "provider-deadline":
						cause = context.DeadlineExceeded
					case "cancel-observer-failure":
						cause = fmt.Errorf("cancellation observer storage read refused")
					}
					if kind == "suspension-failure" {
						engine.suspensionError = fmt.Errorf("native checkpoint refused")
					}
					engine.read = func(context.Context) (int, int, error) {
						if kind == "read-timeout" {
							return 0, 0, context.DeadlineExceeded
						}
						cancel(cause)
						if kind == "read-failure" {
							return 0, 0, fmt.Errorf("storage read refused")
						}
						if boundary == "budget-read" {
							return 0, 0, ctx.Err()
						}
						// The installed pure DSL interpreter observes this cancellation.
						return 12, 1, nil
					}
					r := testReplier()
					r.engine = engine
					executor := &progressRecoveryExecutor{}
					r.stamper = newToolRecorder(executor, r.logger)
					provider := &outcomeProvider{}
					messages := []common.ChatMessage{
						{Role: "system", Content: "Current instructions"}, {Role: "user", Content: "Continue the existing work"},
						{Role: "assistant", ToolCalls: progressRecoveryCall("completed", "readRecord", `{}`)},
						{Role: "tool", Name: "readRecord", ToolCallId: "completed", Content: "Exact completed receipt"},
					}
					var err error
					if streaming {
						_, err = r.runStreamingToolLoop(ctx, provider, messages, nil, &captureSink{}, time.Now(), "recovery-read", turn)
					} else {
						_, err = r.runNonStreamingToolLoop(ctx, provider, nil, messages, nil, &captureSink{}, time.Now(), "recovery-read", turn)
					}
					require.Zero(t, provider.calls)
					require.Zero(t, executor.questions)
					require.Zero(t, executor.operations)
					require.Zero(t, engine.saves, "read cancellation must precede marker persistence")
					var wait *work.HumanWait
					switch kind {
					case "native-deadline":
						require.ErrorAs(t, err, &wait)
						require.Equal(t, "native-recovery-budget", wait.ApprovalID)
						require.Equal(t, 1, engine.pauseAttempts)
						require.Equal(t, 1, engine.parked)
						require.Equal(t, messages, engine.savedContinuation)
						require.NotErrorIs(t, err, memql.ErrWorkCheckpointRequired)
					case "suspension-failure":
						require.ErrorIs(t, err, memql.ErrWorkCheckpointRequired)
						require.ErrorContains(t, err, "native checkpoint refused")
						require.Equal(t, 1, engine.pauseAttempts)
						require.Zero(t, engine.parked)
						require.NotErrorAs(t, err, &wait)
					case "read-failure", "read-timeout", "cancel-observer-failure":
						require.ErrorIs(t, err, memql.ErrWorkCheckpointRequired)
						if kind == "read-failure" {
							require.ErrorContains(t, err, "storage read refused")
						} else if kind == "cancel-observer-failure" {
							require.ErrorIs(t, err, cause)
						} else {
							require.ErrorIs(t, err, context.DeadlineExceeded)
						}
						require.Zero(t, engine.pauseAttempts)
						require.Zero(t, engine.parked)
					default:
						require.ErrorIs(t, err, cause)
						require.NotErrorIs(t, err, memql.ErrWorkCheckpointRequired)
						require.Zero(t, engine.pauseAttempts)
						require.Zero(t, engine.parked)
					}
				})
			}
		}
	}
}

func TestWorkProgressRecoveryFastHumanAnswerContinuesBothLoops(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			e := newProgressRecoveryEngine(t)
			x := &progressRecoveryExecutor{answered: true}
			steps := append(progressDiscoverySteps(5), outcomeCall(`{"status":"complete","response":"Used the verified record."}`))
			p, result, err := progressRecoveryRun(t, streaming, e, x, steps, func(index int, messages []common.ChatMessage) {
				if index != 5 {
					return
				}
				state, _, err := recoveryState(messages)
				require.NoError(t, err)
				require.False(t, state.ReviewActive)
				require.Equal(t, "2026-10-14T12:00:00Z", state.AnsweredAt.Format(time.RFC3339))
				require.Contains(t, fmt.Sprint(messages), "Use the verified record.")
				for _, message := range messages {
					if message.Role == "tool" && strings.HasPrefix(message.ToolCallId, "recovery-") {
						require.Contains(t, message.Content, `"status":"answered"`)
					}
				}
			})
			require.NoError(t, err)
			require.Equal(t, 6, p.calls)
			require.Equal(t, "Used the verified record.", result.FinalText)
			require.Equal(t, 1, x.questions)
			require.Equal(t, 5, x.operations)
		})
	}
}

func recoveryRecordedAnswer(stamp string, extra map[string]any) common.ChatMessage {
	receipt := map[string]any{"answer": map[string]any{"text": "Use the verified record."}, "decidedAt": stamp}
	for k, v := range extra {
		receipt[k] = v
	}
	raw, _ := json.Marshal(receipt)
	return common.ChatMessage{Role: "user", Content: "[Recorded answer for this run]\n" + string(raw)}
}

func TestWorkProgressRecoveryOldAnswersAndActiveGuidanceDoNotRearm(t *testing.T) {
	r, e, x, ctx, turn := recoveryHarness(t)
	e.used = 5
	b, err := time.Parse(time.RFC3339, "2026-10-14T12:00:02Z")
	require.NoError(t, err)
	state := workRecoveryState{Version: 1, ReviewActive: true, DiscoveryRounds: 3, AnsweredAt: b, GuidanceIDs: []string{"guidance-a", "guidance-b"}}
	messages := recoveryMessages(t, state)
	messages = append(messages,
		recoveryRecordedAnswer("2026-10-14T12:00:01Z", nil), // A reappears after compaction; B is already accepted.
		recoveryRecordedAnswer("2026-10-14T12:00:02Z", nil),
		common.ChatMessage{Role: "user", Content: "[Memory checkpoint archived] summarized old feedback"},
		common.ChatMessage{Role: "user", Content: `{"type":"validation","message":"Choose another operation"}`},
		recoveryRecordedAnswer("2026-10-14T12:00:03Z", map[string]any{"kind": "budget", "decision": "approved", "answer": map[string]any{"newLimit": 24}}),
	)
	cursor := &workGuidanceCursor{seen: map[string]bool{"guidance-b": true, "guidance-a": true}}
	messages, err = r.recoverWorkProgress(ctx, messages, turn, &captureSink{}, cursor)
	require.NoError(t, err)
	state, _, err = recoveryState(messages)
	require.NoError(t, err)
	require.True(t, state.ReviewActive)
	require.Equal(t, 3, state.DiscoveryRounds)
	require.Equal(t, b, state.AnsweredAt)
	require.Equal(t, 5, e.used, "an accepted answer never refunds spend")
	require.Zero(t, x.questions)
	// One newly accepted answer permits one new attempt.
	messages = append(messages, recoveryRecordedAnswer("2026-10-14T12:00:04Z", nil))
	messages, err = r.recoverWorkProgress(ctx, messages, turn, &captureSink{}, cursor)
	require.NoError(t, err)
	state, _, err = recoveryState(messages)
	require.NoError(t, err)
	require.False(t, state.ReviewActive)
	require.Zero(t, state.DiscoveryRounds)
	// Restore active review state with the accepted identity on another host.
	state.ReviewActive, state.DiscoveryRounds = true, 3
	messages = recoveryMessages(t, state)
	r, e, x, ctx, turn = recoveryHarness(t)
	e.used = 5
	cursor.seen["guidance-c"] = true
	messages, err = r.recoverWorkProgress(ctx, messages, turn, &captureSink{}, cursor)
	require.NoError(t, err)
	state, _, err = recoveryState(messages)
	require.NoError(t, err)
	require.False(t, state.ReviewActive)
	require.Contains(t, state.GuidanceIDs, "guidance-c")
	state.ReviewActive, state.DiscoveryRounds = true, 3
	messages, err = r.recoverWorkProgress(ctx, recoveryMessages(t, state), turn, &captureSink{}, &workGuidanceCursor{seen: map[string]bool{"guidance-c": true, "guidance-a": true}})
	require.NoError(t, err)
	state, _, err = recoveryState(messages)
	require.NoError(t, err)
	require.True(t, state.ReviewActive, "replayed active-guidance IDs cannot rearm again")
	require.Equal(t, 3, state.DiscoveryRounds)
	require.Zero(t, x.questions)
}
