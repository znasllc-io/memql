package pipelinerun

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/pipelines"
)

func (i *Integration) handleRunRelease(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	run, err := i.RunRelease(handlerContext(ctx), stringArg(args, "pipelineId"), stringArg(args, "tag"))
	if err != nil {
		return nil, err
	}
	return resultNode(map[string]any{"runId": run.ID, "attempt": run.Attempt, "status": run.Status, "sha": run.SHA, "version": run.Version}), nil
}

// RunRelease opens a release build at an existing repository tag. It creates
// neither a tag nor a public release. The owner requests one run; source and
// version are resolved through that owner's current grant, and the ordinary
// release run key deduplicates this request with a later published webhook.
// Automatic delivery may stay disconnected during a bootstrap: the explicit
// owner request authorizes this release, without subscribing to other changes.
func (i *Integration) RunRelease(ctx context.Context, pipelineID, tag string) (Run, error) {
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
	if !pipelines.ValidBranchName(tag) {
		return Run{}, fmt.Errorf("pipelines: tag must name one existing Git tag without a refs/ prefix")
	}
	p, err := d.Store.PipelineForOwner(ctx, pipelineID)
	if err != nil {
		return Run{}, err
	}
	if p == nil {
		return Run{}, fmt.Errorf("pipelines: you have no pipeline %q", pipelineID)
	}
	if strings.TrimSpace(p.CredentialID) == "" {
		return Run{}, fmt.Errorf("pipelines: this source has no GitHub connection; reconnect it before building a release")
	}
	token, installationID, err := d.GitHub.InstallationToken(ctx, p.CredentialID, p.OwnerUserID, p.Repository)
	if err != nil {
		return Run{}, fmt.Errorf("pipelines: reading %s through the source owner's GitHub grant: %w", p.Repository, err)
	}
	if installationID == 0 || installationID != p.InstallationID {
		return Run{}, fmt.Errorf("pipelines: the source's GitHub installation changed; reconnect the pipeline before building a release")
	}
	sha, _, err := d.GitHub.CommitForRef(ctx, token, p.Repository, "tags/"+tag)
	if err != nil {
		return Run{}, fmt.Errorf("pipelines: resolving release tag %s: %w", tag, err)
	}
	sha = strings.ToLower(strings.TrimSpace(sha))
	decoded, decodeErr := hex.DecodeString(sha)
	if len(sha) != 40 || decodeErr != nil || len(decoded) != 20 {
		return Run{}, fmt.Errorf("pipelines: GitHub returned an invalid release commit: expected 40 hexadecimal characters")
	}
	opened, err := i.openWithToken(ctx, d, *p, Opening{
		Event: pipelines.EventRelease, Mode: pipelines.ModeFull, SHA: sha,
		ReleaseTag: tag, Title: "Release " + tag, Trigger: TriggerManual,
	}, token)
	if err != nil {
		return Run{}, err
	}
	if opened.Run.Version != tag {
		return Run{}, fmt.Errorf("pipelines: this commit already has a release run for %s; use its recorded version", opened.Run.Version)
	}
	return opened.Run, nil
}
