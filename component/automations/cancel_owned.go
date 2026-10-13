package automations

import (
	"context"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/common"
)

// finishOwnedStop only persists terminal bookkeeping. A remote call can lose
// the cancellation error's Go type, so the fresh owning run's Stop receipt is
// authoritative. Neither context.Canceled nor a provider error proves Stop.
// The lifecycle hold excludes a new public rerun while the child unwinds.
func (j *workJournal) finishOwnedStop(ctx context.Context, exec *AutomationExecution, chainHead string, receipt func(context.Context)) (stopped bool, failure error) {
	run, owned := common.RunFromContext(ctx)
	if j == nil || exec == nil || !owned || run.OwnerUserId == "" || run.GoalId == "" || run.RunId == "" {
		return false, nil
	}
	if exec.cancelRecorded {
		return true, nil
	}
	defer func() {
		if failure != nil {
			failure = fmt.Errorf("%w: cancellation bookkeeping: %v", ErrJournalRequired, failure)
		}
	}()
	if failure := j.required.failure(); failure != nil {
		return false, failure
	}
	// This detached deadline never reaches a model, tool, retry or classifier.
	commit, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	read := func(runID string) (map[string]any, error) {
		call, err := journalArgs("workRunForOwner", map[string]any{"runId": runID})
		if err != nil {
			return nil, err
		}
		result, err := j.exec.Execute(memql.ContextWithFreshRead(commit), "query "+call)
		if err != nil {
			return nil, err
		}
		rows := memql.MaterializeRows(result)
		if len(rows) != 1 || memql.BareShortId(stringField(rows[0], "ownerUserId")) != memql.BareShortId(run.OwnerUserId) {
			return nil, fmt.Errorf("owned cancellation run is unavailable")
		}
		return rows[0], nil
	}
	root, err := read(run.RunId)
	if err != nil {
		// As with the ordinary cancellation poll, an unavailable Stop receipt
		// is not evidence of a human decision. Preserve the original outcome.
		return false, nil
	}
	if !boolField(root, "cancelRequested") {
		return false, nil
	}
	engine, ok := j.exec.(interface {
		IntegrationByName(string) memql.IntegrationProvider
	})
	if !ok {
		return false, fmt.Errorf("work lifecycle coordination is unavailable")
	}
	coordinator, ok := engine.IntegrationByName("work").(interface {
		HoldWorkRunLifecycle(context.Context) (*memql.GateHold, error)
	})
	if !ok {
		return false, fmt.Errorf("work lifecycle coordination is unavailable")
	}
	hold, err := coordinator.HoldWorkRunLifecycle(commit)
	if err != nil {
		return false, err
	}
	defer hold.Release()
	root, err = read(run.RunId)
	if err != nil {
		return false, err
	}
	if !boolField(root, "cancelRequested") {
		return false, fmt.Errorf("owning cancellation generation changed")
	}
	rerun, _ := root["rerun"].(map[string]any)
	if memql.BareShortId(stringField(root, "goalId")) != memql.BareShortId(run.GoalId) || stringField(rerun, "requestId") != run.RerunRequestId {
		return false, fmt.Errorf("owning cancellation generation changed")
	}
	current := root
	if memql.BareShortId(exec.ID) != memql.BareShortId(run.RunId) {
		current, err = read(exec.ID)
		if err != nil {
			return false, err
		}
		parent, _ := ctx.Value(automationParentKey{}).(string)
		if parent == "" || memql.BareShortId(stringField(current, "parentRunId")) != memql.BareShortId(parent) {
			return false, fmt.Errorf("synchronous cancellation parent changed")
		}
		if stringField(current, "goalId") != "" || rerunSpecFrom(current["rerun"]) != nil {
			return false, fmt.Errorf("child has an independent execution lifecycle")
		}
		seen := map[string]bool{memql.BareShortId(exec.ID): true}
		for depth := 0; memql.BareShortId(parent) != memql.BareShortId(run.RunId); depth++ {
			key := memql.BareShortId(parent)
			if key == "" || seen[key] || depth >= 32 {
				return false, fmt.Errorf("synchronous cancellation ancestry is invalid")
			}
			seen[key] = true
			ancestor, err := read(parent)
			if err != nil {
				return false, err
			}
			if stringField(ancestor, "goalId") != "" || rerunSpecFrom(ancestor["rerun"]) != nil {
				return false, fmt.Errorf("ancestor has an independent execution lifecycle")
			}
			parent = stringField(ancestor, "parentRunId")
		}
	}
	currentRerun, _ := current["rerun"].(map[string]any)
	wantRerun := ""
	if exec.rerun != nil {
		wantRerun = exec.rerun.RequestId
	}
	if stringField(current, "status") != "running" || stringField(current, "automationName") != exec.AutomationName ||
		stringField(current, "templateFingerprint") != exec.journalFingerprint || stringField(currentRerun, "requestId") != wantRerun {
		return false, fmt.Errorf("cancelled execution generation changed")
	}
	if exec.head != nil {
		args := map[string]any{}
		exec.head.write(args)
		expected, _ := args["head"].(map[string]any)
		call, err := journalArgs("workStepsForOwnerRun", map[string]any{"runId": exec.ID})
		if err != nil {
			return false, err
		}
		result, err := j.exec.Execute(memql.ContextWithFreshRead(commit), "query "+call)
		if err != nil {
			return false, err
		}
		for _, row := range memql.MaterializeRows(result) {
			key := stringField(row, "key")
			if isNestedStepKey(key) {
				continue
			}
			entry, ok := expected[key].(map[string]any)
			if !ok || intField(entry, "version") != max(intField(row, "version"), intField(row, "attempt")) {
				return false, fmt.Errorf("cancelled step generation changed")
			}
		}
	}
	if err := hold.Verify(commit); err != nil {
		return false, err
	}
	// Each journal write verifies the retained session again immediately before
	// dispatch. The borrowed actor is still the original, freshly proven owner.
	writes := &cancellationWrites{verify: hold.Verify}
	commit = context.WithValue(commit, cancellationWriteVerifierKey{}, writes)
	commit = auth.ContextWithInternalOrigin(commit)
	if receipt != nil {
		receipt(commit)
	}
	if writes.err != nil {
		return false, writes.err
	}
	j.cancelStop(commit, exec, chainHead, stringField(root, "cancelledBy"))
	if writes.err != nil {
		return false, writes.err
	}
	exec.cancelRecorded = true
	return true, nil
}

type cancellationWriteVerifierKey struct{}

// Local to a serial terminal-write scope; never shared with executing steps.
type cancellationWrites struct {
	verify func(context.Context) error
	err    error
}
