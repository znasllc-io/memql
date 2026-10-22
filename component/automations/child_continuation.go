package automations

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

type automationParentKey struct{}

// ContextWithAutomationParent records the synchronous caller's journal link.
// It grants no execution authority; resumed children must prove both directions.
func ContextWithAutomationParent(ctx context.Context, parentRunID string) context.Context {
	return context.WithValue(ctx, automationParentKey{}, parentRunID)
}

type automationResolver interface {
	ResolveAutomation(context.Context, string) (*Automation, error)
	EnterAutomationInvocation(context.Context, string) (context.Context, error)
}

// checkpointPlan is native, call-local evidence. It cannot arrive in a request
// or a saved row, and authorizes only the recorded suspended path, never a
// fresh execution of completed effects (including within an explicit rerun).
type checkpointPlan struct {
	journal    *RunJournal
	automation *Automation
	stepID     string
	child      *checkpointPlan
	request    memql.CheckpointResumeRequest
	depth      int
}

func (e *Executor) prepareCheckpoint(ctx context.Context, journal *RunJournal, auto *Automation, request memql.CheckpointResumeRequest, depth int) (context.Context, *checkpointPlan, error) {
	if depth >= 16 {
		return ctx, nil, fmt.Errorf("checkpoint invocation nesting exceeds 16")
	}
	run, ok := common.RunFromContext(ctx)
	if journal.CancelRequested || !ok || run.OwnerUserId == "" || memql.BareShortId(journal.OwnerUserId) != memql.BareShortId(run.OwnerUserId) {
		return ctx, nil, fmt.Errorf("checkpoint journal is not owned by this work")
	}
	rerun := depth == 0 && journal.Rerun != nil
	if err := validateRunJournal(journal, auto, fingerprintEngine, rerun); err != nil {
		return ctx, nil, err
	}
	key := statementResumePoint(journal, auto)
	if rerun {
		target, err := rerunTargetIndex(auto, journal.Rerun, journal.ForkAtStepKey)
		if err != nil {
			return ctx, nil, err
		}
		key = rerunResumePoint(journal, auto, journal.Rerun, target)
	}
	if depth == 0 && key != request.StepKey {
		return ctx, nil, fmt.Errorf("checkpoint target changed")
	}
	if journal.StepStates[key].Status != "waiting" {
		return ctx, nil, fmt.Errorf("checkpoint step is not an unchanged suspension")
	}
	if journal.Rerun != nil {
		if depth != 0 || request.RerunRequestID == "" || journal.Rerun.RequestId != request.RerunRequestID || (journal.Rerun.Snapshot != nil && len(journal.Rerun.Snapshot.Files) > 0) {
			return ctx, nil, fmt.Errorf("checkpoint rerun request changed")
		}
	} else if depth == 0 && request.RerunRequestID != "" {
		return ctx, nil, fmt.Errorf("checkpoint rerun request is unavailable")
	}
	// Restore the exact recorded override before proving a native checkpoint.
	// A repair is still the same suspended execution; new inputs are not.
	if depth == 0 {
		run.Override = journal.StepOverrides[key]
		if journal.Rerun.targets(key) {
			if !common.SameStepOverride(run.Override, journal.Rerun.Override) {
				return ctx, nil, fmt.Errorf("checkpoint step override changed")
			}
			run.Override = journal.Rerun.Override
		}
		ctx = common.ContextWithRun(ctx, run)
	} else if !journal.StepOverrides[key].Empty() {
		return ctx, nil, fmt.Errorf("nested checkpoint override changed")
	}
	var step *Step
	for _, candidate := range auto.Steps {
		if candidate.ID == key {
			step = candidate
			break
		}
	}
	if step == nil {
		return ctx, nil, fmt.Errorf("checkpoint step is unavailable")
	}
	plan := &checkpointPlan{journal: journal, automation: auto, stepID: key, request: request, depth: depth}
	switch step.Type {
	case StepTypeFunction:
		if step.Function == nil {
			return ctx, nil, nil
		}
		prepared, accepted, err := e.engine.PrepareCheckpointResume(ctx, step.Function.Name, request)
		if err != nil || !accepted {
			return ctx, nil, err
		}
		return prepared, plan, nil
	case StepTypeAutomation:
		if step.Automation == nil || step.Automation.Async {
			return ctx, nil, nil
		}
		resolver, ok := e.automationTrigger.(automationResolver)
		childID := journal.StepStates[key].ChildRunId
		if !ok || childID == "" || memql.BareShortId(childID) == memql.BareShortId(journal.RunId) {
			return ctx, nil, fmt.Errorf("suspended child journal is unavailable")
		}
		child, err := LoadRunJournal(memql.ContextWithFreshRead(ctx), e.engine, childID)
		if err != nil {
			return ctx, nil, err
		}
		if memql.BareShortId(child.ParentRunId) != memql.BareShortId(journal.RunId) ||
			child.AutomationName != step.Automation.Name || child.TemplateFingerprint == "" || child.GoalId != "" ||
			child.CallerSuppliedPayload != journal.CallerSuppliedPayload || child.Status != "waiting" ||
			stringField(child.WaitingOn, "kind") != WaitKindApproval ||
			memql.BareShortId(stringField(child.WaitingOn, "subject")) != memql.BareShortId(request.ApprovalID) {
			return ctx, nil, fmt.Errorf("suspended child does not match this invocation and decision")
		}
		childAuto, err := resolver.ResolveAutomation(ctx, step.Automation.Name)
		if err != nil {
			return ctx, nil, err
		}
		if childAuto == nil {
			return ctx, nil, fmt.Errorf("suspended child definition is unavailable")
		}
		if err := ensurePrepared(childAuto); err != nil {
			return ctx, nil, err
		}
		prepared, childPlan, err := e.prepareCheckpoint(ctx, child, childAuto, request, depth+1)
		if err != nil || childPlan == nil {
			return ctx, nil, err
		}
		plan.child = childPlan
		return prepared, plan, nil
	}
	return ctx, nil, nil
}

func (e *Executor) childContinuation(plan *checkpointPlan) func(context.Context, string, string, map[string]any) (*AutomationExecution, error) {
	return func(ctx context.Context, stepID, name string, args map[string]any) (*AutomationExecution, error) {
		child := plan.child
		if child == nil || stepID != plan.stepID || name != child.automation.Name {
			return nil, fmt.Errorf("child continuation does not match the suspended call")
		}
		fresh, err := LoadRunJournal(memql.ContextWithFreshRead(ctx), e.engine, child.journal.RunId)
		if err != nil {
			return nil, err
		}
		// The evaluator is rehydrated from completed receipts; still refuse if the
		// resolved call changed rather than silently substituting old arguments.
		saved := fresh.Variables
		if saved == nil && fresh.TriggerEvent != nil {
			saved, _ = fresh.TriggerEvent["payload"].(map[string]any)
		}
		if len(saved) == 0 {
			saved = map[string]any{}
		}
		if len(args) == 0 {
			args = map[string]any{}
		}
		have, err := json.Marshal(args)
		want, savedErr := json.Marshal(saved)
		if err != nil || savedErr != nil || string(have) != string(want) {
			return nil, fmt.Errorf("child continuation arguments changed")
		}
		if fresh.ParentRunId != child.journal.ParentRunId || fresh.AutomationName != child.journal.AutomationName ||
			fresh.CallerSuppliedPayload != child.journal.CallerSuppliedPayload || fresh.Status != "waiting" ||
			stringField(fresh.WaitingOn, "kind") != WaitKindApproval ||
			memql.BareShortId(stringField(fresh.WaitingOn, "subject")) != memql.BareShortId(plan.request.ApprovalID) {
			return nil, fmt.Errorf("suspended child changed before continuation")
		}
		prepared, current, err := e.prepareCheckpoint(ctx, fresh, child.automation, plan.request, child.depth)
		if err != nil {
			return nil, err
		}
		if current == nil {
			return nil, fmt.Errorf("child checkpoint is unavailable")
		}
		prepared, err = e.automationTrigger.(automationResolver).EnterAutomationInvocation(prepared, name)
		if err != nil {
			return nil, err
		}
		prepared = ContextWithAutomationParent(prepared, plan.journal.RunId)
		return e.ResumeFrom(prepared, fresh, child.automation, &ResumeOptions{checkpoint: current})
	}
}
