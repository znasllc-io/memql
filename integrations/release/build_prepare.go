package release

import (
	"context"
	"errors"
	"time"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	pl "github.com/znasllc-io/memql/component/pipelines"
)

type releaseBuildRecipes struct{ plan, workflow, publication *workflowhost.Snapshot }
type releaseBuildRecipeIdentity struct{ Plan, Workflow, Publication string }
type releaseBuildProfileView struct {
	Component string `json:"component"`
	Run       string `json:"run"`
	Branch    string `json:"branch"`
}

func (r releaseBuildRecipes) identity() *releaseBuildRecipeIdentity {
	if r.plan == nil {
		return nil
	}
	return &releaseBuildRecipeIdentity{r.plan.Version, r.workflow.Version, r.publication.Version}
}

func (i *Integration) captureBuildRecipes(p candidateProfile) (releaseBuildRecipes, error) {
	var r releaseBuildRecipes
	if p.Build == nil {
		return r, nil
	}
	var err error
	r.plan, err = workflowhost.Capture(p.Build.Plan, buildPlanContract, i.profileSource, nil)
	if err != nil {
		return r, err
	}
	r.workflow, err = workflowhost.Capture(p.Build.Workflow, buildContract, i.profileSource, buildCaptureOperations(false))
	if err != nil {
		return r, err
	}
	r.publication, err = workflowhost.Capture(p.Build.PublicationWorkflow, buildPublicationContract, i.profileSource, buildCaptureOperations(true))
	return r, err
}

func (i *Integration) buildPipeline() (ReleaseBuildPipeline, error) {
	if i == nil {
		return nil, errors.New("release builds are unavailable")
	}
	binding := i.releaseBuild.Load()
	if binding == nil || binding.pipeline == nil {
		return nil, errors.New("release build pipeline is not configured")
	}
	return binding.pipeline, nil
}

func (i *Integration) prepareBuild(ctx context.Context, name, pipelineID, expected string) (releaseBuildRecord, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var record releaseBuildRecord
	owner, err := candidateOwner(ctx)
	if err != nil {
		return record, err
	}
	profile, _, configuration, err := i.candidateProfile(ctx, name)
	if err != nil {
		return record, err
	}
	if expected == "" || configuration != expected || profile.Build == nil {
		return record, errors.New("choose a currently configured release build profile")
	}
	storage, err := i.candidateStorage(ctx)
	if err != nil {
		return record, err
	}
	pipeline, err := i.buildPipeline()
	if err != nil {
		return record, err
	}
	b := profile.Build
	component := profile.Components[0]
	request := pl.ReleaseBuildRequest{PipelineID: pipelineID, Repository: component.Repository, Branch: b.Branch}
	source, err := pipeline.InspectReleaseBuild(ctx, request)
	if err != nil {
		return record, err
	}
	if source.OwnerUserID != owner || source.PipelineID != pipelineID || source.Repository != component.Repository || source.Branch != b.Branch || !candidateSourceCommit(source.SHA) {
		return record, errors.New("release source does not match the selected owner and profile")
	}
	request.ExpectedSHA, request.SourceID, request.CredentialID = source.SHA, source.SourceID, source.CredentialID
	request.InstallationID, request.RepositoryID = source.InstallationID, source.RepositoryID
	client := i.github
	if deps := i.candidateDeps.Load(); deps != nil && deps.SourceClient != nil {
		client = deps.SourceClient
	}
	versions := candidateVersionReader{client: client, resolver: i.resolver, sources: map[string]candidateVersionSource{b.Component: component.candidateVersionSource}}
	version, err := versions.readVersion(ctx, pl.ReleaseComponent{Name: b.Component, Repository: component.Repository, Commit: source.SHA})
	if err != nil {
		return record, err
	}
	if _, ok := parseReleaseTag("v" + version); !ok {
		return record, errors.New("source version requires a normal version-preparation pull request")
	}
	recipes, err := i.captureBuildRecipes(profile)
	if err != nil {
		return record, err
	}
	// Native facts are immutable source reads. The pure product recipe names
	// the release; it cannot choose a URL, credential or a different version.
	facts := map[string]any{"profileName": name, "component": b.Component, "repository": component.Repository, "branch": b.Branch, "sha": source.SHA, "version": version}
	value, err := workflowhost.RunSnapshot(ctx, recipes.plan, buildPlanContract, map[string]any{"facts": facts}, workflowhost.Options{Logger: i.logger})
	if err != nil {
		return record, err
	}
	var plan releaseBuildPlan
	if err := profileDecode(value, &plan); err != nil {
		return record, err
	}
	if plan.Version != version || plan.Tag != b.TagPrefix+version || !pl.ValidBranchName(plan.Tag) {
		return record, errors.New("release recipe must select the exact source version and configured tag prefix")
	}
	request.Tag = plan.Tag
	source, err = pipeline.InspectReleaseBuild(ctx, request)
	if err != nil {
		return record, err
	}
	selection := releaseBuildSelection{Owner: owner, ProfileName: name, Configuration: configuration, Request: request, Source: source, Version: version, Tag: plan.Tag, Component: b.Component, RunAlias: b.Run, Plan: recipes.plan, Workflow: recipes.workflow, Publication: recipes.publication}
	key, err := buildRequestID(selection)
	if err != nil {
		return record, err
	}
	digest, err := profileDigest(selection)
	if err != nil {
		return record, err
	}
	record = releaseBuildRecord{ID: key, SelectionDigest: digest, Selection: selection, Phase: "prepared", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if source.TagExists && source.TagSHA != request.ExpectedSHA {
		record.Phase, record.ReasonCode, record.Reason = "blocked", "version_prepare_required", "This version already names another commit. Merge a reviewed version-preparation pull request, then prepare a new release."
	} else if source.ExistingRun != nil && !matchingBuildRun(selection, *source.ExistingRun) {
		record.Phase, record.ReasonCode, record.Reason = "blocked", "source_already_released", "This commit already has a different release run. Use a separately verified version-preparation commit."
	}
	// A configuration change between capture and insert refuses before writes.
	_, _, current, err := i.candidateProfile(ctx, name)
	if err != nil || current != configuration {
		return releaseBuildRecord{}, errors.New("release profile changed during selection")
	}
	return storage.ledger.saveBuild(ctx, record)
}

func (i *Integration) handleBuildPrepare(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	record, err := i.prepareBuild(ctx, asString(args["profileName"]), asString(args["pipelineId"]), asString(args["expectedConfiguration"]))
	if err != nil {
		return nil, err
	}
	return i.buildResult(ctx, record)
}
