package pipelinerun

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/packages"
	"github.com/znasllc-io/memql/component/packages/githubapp"
	"github.com/znasllc-io/memql/component/pipelines"
)

// preview.go -- what connecting a source's pipeline would read, before
// anything is written (epic memql#5479, D14: "the manifest read at the first
// stop so the stages show before confirming").
//
// THE SAME READ CONNECT MAKES (inspect), and nothing else: the caller's own
// source, the grant proved by a mint, the default branch's head and the
// manifest there, validated. What the rail cannot know until a person
// answers -- consent to the fleet, the secrets allowed -- is not asked here;
// the preview says which needs and which secrets the manifest names, and
// connect asks consent of the answers.
//
// A REFUSAL IS AN ANSWER, NOT AN ERROR. The stop that asked renders it, in
// place, with the copy keyed by its code -- a source with no pipeline block,
// a repository another source runs, a grant that no longer reaches it, and a
// source the caller cannot read (the same zero rows as one that is not
// there, so it says nothing more). Only a caller who is nobody, a cluster
// owner previewing a colleague's source (ErrNotOwner: they may read it, and
// connecting it is not theirs to do), or a fault -- GitHub could not be read
// -- is an error.

// PreviewResult is what pipelinesPreview answers.
type PreviewResult struct {
	Repository    string
	DefaultBranch string
	SHA           string
	// Name is the manifest's name, and CheckName what the check run is
	// called on GitHub under it.
	Name      string
	CheckName string
	Stages    []PreviewStage
	// Needs and Secrets are every step's, each once, sorted: what the
	// Compute and Confirm stops ask a person about.
	Needs   []string
	Secrets []string
	// SuggestedDelivery is webhook where GitHub can plausibly reach this
	// cluster, poll otherwise -- a suggestion the rail lets a person change.
	SuggestedDelivery string
	// Existing is the source's pipeline when it has one: a reconnect is
	// prefilled from what it restates.
	Existing *Pipeline
	Refusal  *PreviewRefusal
}

// PreviewStage is one stage as the rail shows it.
type PreviewStage struct {
	Name    string
	On      []string
	Channel string
	Steps   []PreviewStep
}

// PreviewStep is one manifest step as the rail shows it: its shape, never its
// command.
type PreviewStep struct {
	Name     string
	Packages string
	Only     string
	Shards   int
	Bucket   string
	Needs    []string
	Secrets  []string
	Services []string
}

// PreviewRefusal is a typed refusal, as data.
type PreviewRefusal struct {
	Code    string
	Message string
	Scope   string
}

func (i *Integration) handlePreview(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	res, err := i.Preview(handlerContext(ctx), stringArg(args, "packageId"))
	if err != nil {
		return nil, err
	}
	return resultNode(res.payload()), nil
}

// Preview reads what connecting one of the caller's sources would act on.
func (i *Integration) Preview(ctx context.Context, packageID string) (PreviewResult, error) {
	caller, err := personFrom(ctx)
	if err != nil {
		return PreviewResult{}, err
	}
	d := i.snapshot()
	if d.Store == nil {
		return PreviewResult{}, errNoStore
	}
	if d.GitHub == nil {
		return PreviewResult{}, errNoGitHub
	}
	packageID = strings.TrimSpace(packageID)
	if packageID == "" {
		return PreviewResult{}, fmt.Errorf("pipelines: packageId is required")
	}

	res := PreviewResult{SuggestedDelivery: DeliveryPoll}
	if d.WebhookReachable != nil && d.WebhookReachable() {
		res.SuggestedDelivery = DeliveryWebhook
	}
	insp, err := i.inspect(ctx, d, caller, packageID)
	res.Repository, res.DefaultBranch, res.SHA = insp.repository, insp.branch, insp.head
	if insp.pkg != nil {
		// The caller owns the source by now (inspect refuses otherwise), so
		// their own read of its pipeline is the one to restate.
		existing, perr := d.Store.PipelineForPackage(ctx, insp.pkg.ID)
		if perr != nil {
			return PreviewResult{}, perr
		}
		res.Existing = existing
	}
	if err != nil {
		refusal, ok := previewRefusalOf(err)
		if !ok {
			return PreviewResult{}, err
		}
		res.Refusal = refusal
		return res, nil
	}

	spec := insp.manifest.Pipeline
	res.Name = insp.manifest.Name
	res.CheckName = pipelines.CheckRunName(res.Name)
	needs, secrets := map[string]bool{}, map[string]bool{}
	for _, st := range spec.Stages {
		stage := PreviewStage{Name: st.Name, On: append([]string(nil), st.On...), Channel: st.Channel}
		for _, sp := range st.Steps {
			step := PreviewStep{
				Name: sp.Name, Packages: sp.Packages, Only: sp.Only, Shards: sp.Shards,
				Secrets: append([]string(nil), sp.Secrets...), Services: append([]string(nil), sp.Services...),
			}
			if sp.When != nil {
				step.Bucket = sp.When.Bucket
			}
			for need, on := range sp.Needs {
				if on {
					step.Needs = append(step.Needs, need)
					needs[need] = true
				}
			}
			sort.Strings(step.Needs)
			for _, name := range sp.Secrets {
				secrets[name] = true
			}
			stage.Steps = append(stage.Steps, step)
		}
		res.Stages = append(res.Stages, stage)
	}
	res.Needs, res.Secrets = sortedKeys(needs), sortedKeys(secrets)
	return res, nil
}

// previewRefusalOf is err as a typed refusal, when it is one: a pipelines or a
// packages *Refusal, or the GitHub App's own "not set up" and "not installed"
// answers, which the rail names the way a deploy's fetch does.
func previewRefusalOf(err error) (*PreviewRefusal, bool) {
	var pr *pipelines.Refusal
	if errors.As(err, &pr) && pr != nil {
		return &PreviewRefusal{Code: pr.Code, Message: pr.Detail, Scope: pr.Scope}, true
	}
	var kr *packages.Refusal
	if errors.As(err, &kr) && kr != nil {
		return &PreviewRefusal{Code: kr.Code, Message: kr.Detail, Scope: kr.Scope}, true
	}
	switch {
	case errors.Is(err, githubapp.ErrNotConfigured):
		return &PreviewRefusal{Code: packages.CodeGithubAppNotConfigured,
			Message: "This cluster has no GitHub App, so no pipeline can be connected or report a check run."}, true
	case errors.Is(err, githubapp.ErrNotInstalled):
		return &PreviewRefusal{Code: packages.CodeRepositoryNotInstalled,
			Message: "The cluster's GitHub App is not installed on this repository."}, true
	}
	return nil, false
}

// payload is the answer's one row, in the wire's names.
func (r PreviewResult) payload() map[string]any {
	stages := make([]any, 0, len(r.Stages))
	for _, st := range r.Stages {
		steps := make([]any, 0, len(st.Steps))
		for _, sp := range st.Steps {
			steps = append(steps, map[string]any{
				"name": sp.Name, "packages": sp.Packages, "only": sp.Only, "shards": sp.Shards, "bucket": sp.Bucket,
				"needs": nonNil(sp.Needs), "secrets": nonNil(sp.Secrets), "services": nonNil(sp.Services),
			})
		}
		stages = append(stages, map[string]any{"name": st.Name, "on": nonNil(st.On), "channel": st.Channel, "steps": steps})
	}
	out := map[string]any{
		"repository": r.Repository, "defaultBranch": r.DefaultBranch, "sha": r.SHA,
		"name": r.Name, "checkName": r.CheckName, "stages": stages,
		"needs": nonNil(r.Needs), "secrets": nonNil(r.Secrets), "suggestedDelivery": r.SuggestedDelivery,
		"existing": nil, "refusal": nil,
	}
	if p := r.Existing; p != nil {
		out["existing"] = map[string]any{
			"pipelineId": p.ID, "status": p.Status, "delivery": p.Delivery, "compute": string(p.Compute),
			"secretNames": nonNil(p.SecretNames),
		}
	}
	if ref := r.Refusal; ref != nil {
		out["refusal"] = map[string]any{"code": ref.Code, "message": ref.Message, "scope": ref.Scope}
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// nonNil is list, or an empty list for nil, so the wire never carries null
// where a client reads an array.
func nonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}
