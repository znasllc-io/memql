package pipelinerun

import (
	"context"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
)

const queueInvalidatedBy = "system:merge-group-invalidated"

// A re-queued candidate may have exactly the same commit. Only an observed
// current queue ref can reopen a system-invalidated attempt; a manual cancel
// or an uncertain provider read remains terminal until an explicit rerun.
func queueRefRestored(ctx context.Context, d Deps, p Pipeline, o Opening, sha, token string, tokenErr error) bool {
	if tokenErr != nil || token == "" || o.Branch == "" || d.GitHub == nil {
		return false
	}
	branch := strings.TrimPrefix(o.Branch, "refs/heads/")
	if !strings.HasPrefix(branch, "gh-readonly-queue/") {
		return false
	}
	callCtx, cancel := context.WithTimeout(ctx, githubCallTimeout)
	defer cancel()
	current, _, err := d.GitHub.CommitForRef(callCtx, token, p.Repository, "refs/heads/"+branch)
	return err == nil && strings.EqualFold(current, sha)
}

// Reconciliation reads the exact queue ref under the source owner's current
// grant. It does not infer invalidation from elapsed time or another merge.
// Provider errors preserve work. Cancellation uses the existing fenced journal
// path, so active jobs drain and publication recovery is not bypassed.
func (i *Integration) reconcileQueueRun(ctx context.Context, d Deps, r Run) (bool, error) {
	if r.Event != pipelines.EventMergeGroup || r.Finished() || r.PolicySnapshot == nil || r.HeadBranch == "" || r.CancelRequested {
		return false, nil
	}
	policy, err := readPipelinePolicy(r.PolicySnapshot)
	if err != nil {
		return false, err
	}
	p, err := d.Store.PipelineByID(memql.ContextWithFreshRead(ctx), r.PipelineID)
	if err != nil {
		return false, err
	}
	if p == nil || !sameID(p.OwnerUserID, r.OwnerUserID) || p.Repository != r.Repository || !p.Active() {
		return false, nil
	}
	callCtx, cancel := context.WithTimeout(ctx, githubCallTimeout)
	defer cancel()
	token, installation, err := d.GitHub.InstallationToken(callCtx, p.CredentialID, p.OwnerUserID, p.Repository)
	if err != nil || installation != p.InstallationID {
		return false, err
	}
	ref := "refs/heads/" + strings.TrimPrefix(r.HeadBranch, "refs/heads/")
	sha, _, readErr := d.GitHub.CommitForRef(callCtx, token, p.Repository, ref)
	state := "unknown"
	switch {
	case githubapp.IsMissingRef(readErr):
		state = "absent"
	case readErr == nil && strings.EqualFold(sha, r.SHA):
		state = "present"
	case readErr == nil && sha != "":
		state = "moved"
	}
	value, err := runPolicyPhase(ctx, policy, "pipelineRunReconciliation", map[string]any{
		"event": string(r.Event), "trigger": r.Trigger, "refState": state, "status": r.Status,
	}, workflowhost.Options{})
	if err != nil {
		return false, err
	}
	decision, ok := value.(string)
	if !ok || (decision != "continue" && decision != "cancel") {
		return false, fmt.Errorf("invalid queue reconciliation decision")
	}
	if decision != "cancel" {
		return false, nil
	}
	if state != "absent" && state != "moved" {
		return false, fmt.Errorf("queue cancellation requires a confirmed obsolete ref")
	}
	_, err = i.requestCancel(ctx, d, r.ID, queueInvalidatedBy)
	return err == nil, err
}
