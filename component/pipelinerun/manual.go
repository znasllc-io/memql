package pipelinerun

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/pipelines"
)

// handleRunLatest is the owner-facing entry for an on-demand local rehearsal.
func (i *Integration) handleRunLatest(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	run, err := i.RunLatest(handlerContext(ctx), stringArg(args, "pipelineId"))
	if err != nil {
		return nil, err
	}
	return resultNode(map[string]any{
		"runId":   run.ID,
		"attempt": run.Attempt,
		"status":  run.Status,
		"sha":     run.SHA,
		"branch":  run.HeadBranch,
	}), nil
}

// RunLatest opens a full manual rehearsal of the current default-branch head.
// Automatic delivery must already be disconnected, so a local run cannot
// duplicate a webhook or poll run. The source's owner's GitHub grant is used
// to read and check the exact commit; the commit is pinned in the run row.
func (i *Integration) RunLatest(ctx context.Context, pipelineID string) (Run, error) {
	if _, err := personFrom(ctx); err != nil {
		return Run{}, err
	}
	d := i.snapshot()
	if d.Store == nil {
		return Run{}, errNoStore
	}
	if d.GitHub == nil {
		return Run{}, errNoGitHub
	}
	pipelineID = strings.TrimSpace(pipelineID)
	if pipelineID == "" {
		return Run{}, fmt.Errorf("pipelines: pipelineId is required")
	}
	// Owner-scoped first: a stranger resolves no row and makes no GitHub call.
	p, err := d.Store.PipelineForOwner(ctx, pipelineID)
	if err != nil {
		return Run{}, err
	}
	if p == nil {
		return Run{}, fmt.Errorf("pipelines: you have no pipeline %q", pipelineID)
	}
	if p.Active() {
		return Run{}, pipelines.Refuse(pipelines.CodeManualRequiresDisconnected, "",
			"Disconnect automatic delivery before starting a manual rehearsal, so both paths do not run the same change.")
	}
	if strings.TrimSpace(p.CredentialID) == "" {
		return Run{}, fmt.Errorf("pipelines: this source has no GitHub connection; reconnect it before running a rehearsal")
	}
	token, installationID, err := d.GitHub.InstallationToken(ctx, p.CredentialID, p.OwnerUserID, p.Repository)
	if err != nil {
		return Run{}, fmt.Errorf("pipelines: reading %s through the source owner's GitHub grant: %w", p.Repository, err)
	}
	if installationID == 0 || installationID != p.InstallationID {
		return Run{}, fmt.Errorf("pipelines: the source's GitHub installation changed; reconnect the pipeline before running a rehearsal")
	}
	info, err := d.GitHub.Repository(ctx, token, p.Repository)
	if err != nil {
		return Run{}, fmt.Errorf("pipelines: reading %s: %w", p.Repository, err)
	}
	branch := strings.TrimSpace(info.DefaultBranch)
	if branch == "" {
		return Run{}, fmt.Errorf("pipelines: GitHub names no default branch for %s", p.Repository)
	}
	sha, title, err := d.GitHub.BranchHead(ctx, token, p.Repository, branch)
	if err != nil {
		return Run{}, fmt.Errorf("pipelines: reading %s's head: %w", branch, err)
	}
	sha = strings.ToLower(strings.TrimSpace(sha))
	decoded, decodeErr := hex.DecodeString(sha)
	if len(sha) != 40 || decodeErr != nil || len(decoded) != 20 {
		return Run{}, fmt.Errorf("pipelines: GitHub returned an invalid commit SHA for %s: expected 40 hexadecimal characters", branch)
	}
	title = strings.TrimSpace(strings.SplitN(title, "\n", 2)[0])
	opened, err := i.openWithToken(ctx, d, *p, Opening{
		Event: pipelines.EventManual, Mode: pipelines.ModeFull, SHA: sha,
		Branch: branch, Title: title, Trigger: TriggerManual,
	}, token)
	if err != nil {
		return Run{}, err
	}
	return opened.Run, nil
}
