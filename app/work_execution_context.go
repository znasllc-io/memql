package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
	"github.com/znasllc-io/memql/core/common"
	workspine "github.com/znasllc-io/memql/integrations/work"
)

// Execution starts on a different replica from intake and compilation. The
// persisted journal supplies identity and lineage; no context value can be
// assumed to have followed the graph event here.
func workExecutionContext(ctx context.Context, j, source *automations.RunJournal, resolver *auth.IdentityResolver) (context.Context, error) {
	run := common.RunContext{RunId: j.RunId, GoalId: j.GoalId, OwnerUserId: j.OwnerUserId, Mode: j.Mode, ReplayPolicy: j.ReplayPolicy, ForkAtStepKey: j.ForkAtStepKey}
	if run.Mode == "" {
		run.Mode = common.RunModeLive
	}
	if j.ForkedFromRunId != "" {
		if source == nil || memql.BareShortId(source.RunId) != memql.BareShortId(j.ForkedFromRunId) || memql.BareShortId(source.GoalId) != memql.BareShortId(j.GoalId) || source.OwnerUserId != j.OwnerUserId {
			return ctx, fmt.Errorf("the source journal does not belong to this goal and owner")
		}
		run.SourceRunId = j.ForkedFromRunId
		run.SourceGoalId = source.GoalId
		run.StepOrder = source.StepOrder
	}
	// Ownerless scheduler journals keep their system context. A goal-backed
	// run must name its persisted owner before any owned work can execute.
	if j.OwnerUserId != "" || j.GoalId != "" {
		var err error
		ctx, err = auth.ContextWithPersistedOwner(ctx, j.OwnerUserId, j.ExecutionAuthority, resolver)
		if err != nil {
			return nil, err
		}
	}
	ctx = common.ContextWithRun(ctx, run)
	return memql.ContextWithBudgetScope(ctx, memql.BudgetScopeId("run", j.RunId), memql.BudgetScopeId("goal", j.GoalId)), nil
}

// Codes a re-run request is refused with at dispatch (epic memql#5414, task
// memql#5415). The step codes are the ones the person's act refuses with
// (integrations/work), so a request that slipped past the act reads the same
// here as it would have there.
const (
	workRerunNeedsGoal      = "rerun_needs_goal"
	workRerunReasonInvalid  = "rerun_reason_invalid"
	workRerunStepNested     = "step_nested"
	workRerunStepNotInRun   = "step_not_in_run"
	workRerunPrefixUnserved = "fork_prefix_unavailable"
	workRerunSourceRefused  = "source_journal_refused"
	workRerunRefused        = "rerun_refused"
	workRerunSourceMissing  = "source_journal_unavailable"
)

// workRerunServable decides whether THIS dispatch may serve the re-run the row
// carries (epic memql#5414).
//
// integrations/work claims a re-run under the request's own id, so a re-run is
// never refused by the lease of the execution before it. That makes the
// request id part of the claim, and this the other half: an event claimed for
// one request -- or for none, a late event from the run's previous execution --
// must not serve another, or two replicas holding two different claims would
// run one request at once. The sweep's recovery is the exception: it read the
// row itself rather than an event, and holds the run's own claim.
func workRerunServable(req workspine.DispatchRequest, j *automations.RunJournal) bool {
	if j.Rerun == nil {
		// An event claimed for a request the row no longer carries is late:
		// that request closed, or a newer one replaced it and has its own.
		return strings.TrimSpace(req.RerunRequestId) == ""
	}
	return req.Recovery || strings.TrimSpace(req.RerunRequestId) == j.Rerun.RequestId
}

// workRerunResumption decides how a run carrying a re-run request resumes
// (epic memql#5414, task memql#5415): the journal and options to hand
// ResumeFrom, or the code the run is failed with. It answers nil, nil, "", nil
// for a run that carries no request.
//
// It decides from the rows alone. The request rode the run row from whichever
// node served the person's act, and nothing that node held in memory is here --
// which is also what lets any replica the sweep hands an interrupted re-run to
// resume it exactly where it stopped. sources are the runs the prefix is served
// from (automations.RerunSources), already loaded by the caller; each must be
// the same goal's and the same owner's, for workExecutionContext's reason.
func workRerunResumption(j *automations.RunJournal, sources []*automations.RunJournal, auto *automations.Automation) (*automations.RunJournal, *automations.ResumeOptions, string, error) {
	if j.Rerun == nil {
		return nil, nil, "", nil
	}
	if code, err := workRerunRefusal(j); err != nil {
		return nil, nil, code, err
	}
	for _, s := range sources {
		if s == nil || s.OwnerUserId != j.OwnerUserId || memql.BareShortId(s.GoalId) != memql.BareShortId(j.GoalId) {
			id := ""
			if s != nil {
				id = s.RunId
			}
			return nil, nil, workRerunSourceRefused, fmt.Errorf("run %s, which the re-run's prefix would be served from, does not belong to this goal and owner", id)
		}
	}
	resume, opts, err := automations.PrepareRerun(j, sources, auto)
	switch {
	case err == nil:
		return resume, opts, "", nil
	case errors.Is(err, work.ErrNestedStep):
		return nil, nil, workRerunStepNested, err
	case errors.Is(err, work.ErrStepNotInRun):
		return nil, nil, workRerunStepNotInRun, err
	case errors.Is(err, automations.ErrForkPrefixUnavailable):
		return nil, nil, workRerunPrefixUnserved, err
	}
	return nil, nil, workRerunRefused, err
}

// workRerunRefusal is what the row alone refuses a re-run request for, asked
// before any other run is read for the request's prefix.
func workRerunRefusal(j *automations.RunJournal) (string, error) {
	spec := j.Rerun
	if spec == nil {
		return "", nil
	}
	if strings.TrimSpace(j.GoalId) == "" {
		// A goalless run is a scheduler's journal: its trigger, its trust and
		// its owner are the scheduler's, and a person's request has nothing to
		// borrow them from. The event path never dispatches one; the sweep's
		// recovery can, which is why this is refused rather than assumed.
		return workRerunNeedsGoal, fmt.Errorf("run %s carries a re-run request but serves no goal; only a goal's run can be re-run", j.RunId)
	}
	switch spec.Reason {
	case automations.RerunReasonRerun, automations.RerunReasonHeadMove:
	case automations.RerunReasonBranch:
		if j.Mode != common.RunModeFork || strings.TrimSpace(j.ForkedFromRunId) == "" {
			return workRerunReasonInvalid, fmt.Errorf("run %s carries a branch request but is not a fork of another run", j.RunId)
		}
	default:
		return workRerunReasonInvalid, fmt.Errorf("run %s carries a re-run request with reason %q, which this node does not serve", j.RunId, spec.Reason)
	}
	return "", nil
}
