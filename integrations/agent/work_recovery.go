package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/core/id"
)

const workRecoveryMarker = "work_recovery_checkpoint"

type workRecoveryEngine interface {
	WorkModelCallBudget(context.Context) (int, int, error)
}

// One bounded system record is pinned by compaction, separately from the first
// system prompt which is refreshed at resume. Its position is the evidence
// watermark: only completed batches AFTER it have not yet been observed.
type workRecoveryState struct {
	EvidenceRepairs    int              `json:"evidenceRepairs,omitempty"`
	EvidenceGap        string           `json:"evidenceGap,omitempty"`
	Version            int              `json:"version"`
	ReadEvidence       workReadEvidence `json:"readEvidence,omitempty"`
	DiscoveryRounds    int              `json:"discoveryRounds"`
	RepeatedRounds     int              `json:"repeatedRounds"`
	RecentEvidence     []string         `json:"recentEvidence,omitempty"`
	ReviewActive       bool             `json:"reviewActive"`
	StalledAfterReview int              `json:"stalledAfterReview"`
	ReviewedLimit      int              `json:"reviewedLimit"`
	AnsweredAt         time.Time        `json:"answeredAt,omitempty"`
	GuidanceIDs        []string         `json:"guidanceIds,omitempty"`
	Instruction        string           `json:"instruction,omitempty"`
}

func recoveryDigest(value any) string {
	raw, _ := json.Marshal(value)
	return string(id.NewUntracked().FromBytes(raw))
}

// Only server-shaped accepted guidance can rearm an attempt. Synthetic user
// messages (memory summaries, validation corrections) and a raised limit alone
// cannot erase evidence of a loop. This grants no permission or model calls.
func recoveryGuidance(message common.ChatMessage) time.Time {
	if message.Role != "user" {
		return time.Time{}
	}
	if !strings.HasPrefix(message.Content, "[Recorded answer for this ") {
		return time.Time{}
	}
	_, raw, ok := strings.Cut(message.Content, "\n")
	if !ok {
		return time.Time{}
	}
	var receipt map[string]any
	if json.Unmarshal([]byte(raw), &receipt) != nil {
		return time.Time{}
	}
	if receipt["kind"] == "budget" || (receipt["kind"] != nil && receipt["decision"] != "answered") {
		return time.Time{}
	}
	if receipt["answer"] == nil {
		return time.Time{}
	}
	decidedAt, _ := receipt["decidedAt"].(string)
	stamp, _ := time.Parse(time.RFC3339Nano, decidedAt)
	return stamp
}

func recoveryState(messages []common.ChatMessage) (workRecoveryState, int, error) {
	s := workRecoveryState{Version: 1}
	marker := -1
	for i, message := range messages {
		if message.Role != "system" || message.Name != workRecoveryMarker {
			continue
		}
		if marker >= 0 || len(message.Content) > 8192 {
			return s, -1, fmt.Errorf("invalid recovery checkpoint")
		}
		if err := json.Unmarshal([]byte(message.Content), &s); err != nil || s.Version != 1 || s.EvidenceRepairs < 0 || s.EvidenceRepairs > 1 || len(s.EvidenceGap) > 1200 || len(s.RecentEvidence) > 8 || len(s.GuidanceIDs) > 16 || len(s.ReadEvidence.Recent) > 8 {
			return s, -1, fmt.Errorf("unreadable recovery checkpoint")
		}
		marker = i
	}
	return s, marker, nil
}

// Observe complete tool batches, not model calls or individual members of a
// parallel batch. Canonical JSON ignores argument-key order and transport IDs.
// Repeated reads can be unproductive; new read evidence is real progress.
func (s *workRecoveryState) observe(messages []common.ChatMessage) {
	for index, message := range messages {
		if stamp := recoveryGuidance(message); stamp.After(s.AnsweredAt) {
			s.AnsweredAt = stamp
			s.rearm()
		}
		if message.Role != "assistant" || len(message.ToolCalls) == 0 {
			continue
		}
		results := map[string]string{}
		for _, result := range messages[index+1:] {
			if result.Role == "assistant" {
				break
			}
			if result.Role == "tool" {
				results[result.ToolCallId] = result.Content
			}
		}
		var evidence []string
		discovery, taskOperation := false, false
		for _, call := range message.ToolCalls {
			result, ok := results[call.ID]
			if !ok || result == common.UnexecutedWorkToolResult {
				continue
			}
			args := any(call.Arguments)
			var decoded any
			if json.Unmarshal([]byte(call.Arguments), &decoded) == nil {
				args = decoded
			}
			name := call.Name[strings.LastIndex(call.Name, ".")+1:]
			s.ReadEvidence.observe(name, call.ID, args, result)
			evidence = append(evidence, recoveryDigest([]any{name, args, result}))
			if name == "discoverCapabilities" {
				discovery = true
			} else if name != "recallWorkHistory" {
				// Rereading archived context is not a new task operation and
				// must not mask discovery/archive oscillation.
				taskOperation = true
			}
		}
		if len(evidence) == 0 {
			continue
		}
		slices.Sort(evidence)
		digest := recoveryDigest(evidence)
		s.RepeatedRounds = 1
		for _, earlier := range s.RecentEvidence {
			if digest == earlier {
				s.RepeatedRounds++
			}
		}
		s.RecentEvidence = append(s.RecentEvidence, digest)
		if len(s.RecentEvidence) > 8 {
			s.RecentEvidence = s.RecentEvidence[1:]
		}
		productive := taskOperation && s.RepeatedRounds == 1
		if productive {
			s.DiscoveryRounds = 0
		} else if discovery {
			if s.DiscoveryRounds < 100 {
				s.DiscoveryRounds++
			}
		}
		stalled := !productive && (discovery || s.RepeatedRounds > 1)
		if s.ReviewActive && stalled {
			if s.StalledAfterReview < 100 {
				s.StalledAfterReview++
			}
		} else if taskOperation && !stalled {
			s.ReviewActive, s.Instruction, s.StalledAfterReview = false, "", 0
		}
	}
}

// Called before compaction and before every provider attempt. Checking twice
// cannot recount a tool batch: moving the single marker advances its watermark.
func (s *workRecoveryState) rearm() {
	s.DiscoveryRounds, s.RepeatedRounds, s.StalledAfterReview = 0, 0, 0
	s.RecentEvidence = nil
	s.ReviewActive, s.Instruction = false, ""
}

func workProgressDecision(ctx context.Context, facts map[string]any) (string, error) {
	value, err := workflowhost.Run(ctx, "agentWorkProgressRecovery", map[string]any{"facts": facts}, workflowhost.Options{})
	if err != nil {
		return "", err
	}
	decision, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("agent progress recovery returned no decision")
	}
	return decision, nil
}

func (r *Replier) recoverWorkProgress(ctx context.Context, messages []common.ChatMessage, turn turnContext, sink DeltaSink, guidance *workGuidanceCursor) (out []common.ChatMessage, failure error) {
	engine, ok := r.engine.(workRecoveryEngine)
	if !ok || !isOwnedWorkExecution(ctx) {
		return messages, nil
	}
	// Recovery is an execution boundary. If its saved state, budget or authored
	// policy cannot be read, an outer `on error continue` must not run later
	// effects as though this owned turn succeeded. A durable human wait keeps
	// its own suspension semantics.
	readStopped := false
	defer func() {
		var wait *work.HumanWait
		if failure != nil && !readStopped && !errors.As(failure, &wait) && !errors.Is(failure, memql.ErrWorkCheckpointRequired) {
			failure = fmt.Errorf("%w: work progress recovery: %w", memql.ErrWorkCheckpointRequired, failure)
		}
	}()
	state, marker, err := recoveryState(messages)
	if err != nil {
		return nil, err
	}
	state.observe(messages[marker+1:])
	// Active guidance has a bounded lifetime set of server IDs. Unlike its
	// rendered text, regrouping old messages on another replica is not new input.
	if guidance != nil {
		for id := range guidance.seen {
			if !slices.Contains(state.GuidanceIDs, id) {
				state.GuidanceIDs = append(state.GuidanceIDs, id)
				state.rearm()
			}
		}
		if len(state.GuidanceIDs) > 16 {
			return nil, fmt.Errorf("recovery guidance exceeds its bounded context")
		}
		slices.Sort(state.GuidanceIDs)
	}
	limit, used, err := engine.WorkModelCallBudget(ctx)
	if err != nil {
		failure, readStopped = r.workRecoveryReadFailure(ctx, fmt.Errorf("reading recovery budget: %w", err), messages)
		return nil, failure
	}
	facts := map[string]any{"limit": limit, "used": used, "discoveryRounds": state.DiscoveryRounds, "repeatedRounds": state.RepeatedRounds,
		"reviewActive": state.ReviewActive, "stalledAfterReview": state.StalledAfterReview, "reviewedLimit": state.ReviewedLimit}
	decision, err := workProgressDecision(ctx, facts)
	if err != nil {
		failure, readStopped = r.workRecoveryReadFailure(ctx, fmt.Errorf("deciding work recovery: %w", err), messages)
		return nil, failure
	}
	switch decision {
	case "review_budget", "review_stalled":
		state.ReviewActive = true
		if decision == "review_budget" {
			state.ReviewedLimit = limit
		}
	case "continue", "ask":
	default:
		return nil, fmt.Errorf("invalid work recovery decision %q", decision)
	}
	if state.ReviewActive {
		state.Instruction, err = r.engine.RenderPrompt("workProgressRecovery", map[string]any{"facts": facts})
		if err != nil {
			return nil, err
		}
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	out = append([]common.ChatMessage(nil), messages...)
	if marker >= 0 {
		out = append(out[:marker], out[marker+1:]...)
	}
	// The first system slot is owned by prompt refresh, never by metadata.
	if len(out) == 0 || out[0].Role != "system" {
		out = append([]common.ChatMessage{{Role: "system"}}, out...)
	}
	out = append(out, common.ChatMessage{Role: "system", Name: workRecoveryMarker, Content: string(raw)})
	if decision == "ask" {
		question, err := r.engine.RenderPrompt("workProgressQuestion", map[string]any{"facts": facts})
		if err != nil {
			return nil, err
		}
		return r.askWorkRecovery(ctx, out, turn, sink, strings.TrimSpace(question))
	}
	if marker < 0 || marker != len(messages)-1 || messages[marker].Content != string(raw) {
		if err := r.saveWorkProgress(ctx, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Only the read-only recovery preparation may resolve a cancelled context into
// its native stop or budget suspension. Once a checkpoint write or question has
// started, its failure remains authoritative; it must not publish another wait.
func (r *Replier) workRecoveryReadFailure(ctx context.Context, failure error, messages []common.ChatMessage) (error, bool) {
	var wait *work.HumanWait
	if errors.As(failure, &wait) || errors.Is(failure, memql.ErrWorkCheckpointRequired) {
		return failure, true
	}
	var stopped *memql.WorkCancelledError
	if errors.As(failure, &stopped) {
		return failure, true
	}
	var ceiling *memql.RunCeilingError
	if errors.As(failure, &ceiling) {
		return r.pauseAtBudgetBoundary(ctx, failure, messages), true
	}
	if !errors.Is(failure, context.Canceled) && !errors.Is(failure, context.DeadlineExceeded) {
		return failure, false
	}
	cause := context.Cause(ctx)
	if errors.As(cause, &stopped) {
		return cause, true
	}
	// WorkCallFailure recognizes the native deadline and refreshes its ceiling
	// with a receipt-only context. A provider timeout alone is not budget proof.
	if budget := r.workCallFailure(ctx); errors.As(budget, &ceiling) {
		return r.pauseAtBudgetBoundary(ctx, budget, messages), true
	}
	if cause != nil {
		return cause, errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded)
	}
	return failure, false
}

func (r *Replier) askWorkRecovery(ctx context.Context, messages []common.ChatMessage, turn turnContext, sink DeltaSink, question string) ([]common.ChatMessage, error) {
	args := map[string]any{"question": question, "kind": "text"}
	raw, _ := json.Marshal(args)
	// Derived from the exact checkpoint, stable on an uncertain retry and
	// different after fresh guidance. The feedback capability is idempotent.
	call := common.ToolCall{ID: "recovery-" + recoveryDigest(messages)[:24], Name: "requestUserFeedback", Arguments: string(raw)}
	messages = append(messages, common.ChatMessage{Role: "assistant", ToolCalls: []common.ToolCall{call}})
	if err := r.saveBeforeQuestion(ctx, call.Name, messages); err != nil {
		return nil, err
	}
	injectAgentContext(call.Name, args, turn)
	sink.ToolCall(call.ID, call.Name, call.Arguments)
	result, err := r.stamper.ExecuteToolByName(agentToolCallContext(ctx, call.Name, turn), call.Name, args)
	if err != nil {
		sink.ToolResult(call.ID, result, err.Error())
		return nil, err
	}
	sink.ToolResult(call.ID, result, "")
	if wait := feedbackWait(call.Name, result); wait != nil {
		return messages, wait
	}
	if receipt := feedbackReceipt(call.Name, result, "answered"); receipt != nil {
		messages = append(messages, common.ChatMessage{Role: "tool", Name: call.Name, ToolCallId: call.ID, Content: result})
		receipt["kind"], receipt["decision"] = "feedback", "answered"
		answer, _ := json.Marshal(receipt)
		messages = append(messages, common.ChatMessage{Role: "user", Content: "[Recorded answer for this recovery question]\n" + string(answer)})
		if err := r.saveWorkProgress(ctx, messages); err != nil {
			return nil, err
		}
		return messages, nil
	}
	return nil, fmt.Errorf("work recovery question did not produce a durable wait or answer")
}
