package pipelinerun

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
)

// Kept separate from the ordinary pipeline provider interface: older/test
// adapters cannot accidentally advertise a proof they do not implement.
type releaseBuildGitHub interface {
	ObserveReleaseSource(context.Context, string, string, string, string) (githubapp.ReleaseSourceEvidence, error)
}

func (g *packagesGitHub) ObserveReleaseSource(ctx context.Context, token, repository, branch, selected string) (githubapp.ReleaseSourceEvidence, error) {
	c, owner, repo, err := g.target(repository)
	if err != nil {
		return githubapp.ReleaseSourceEvidence{}, err
	}
	return c.ObserveReleaseSource(ctx, token, owner, repo, branch, selected)
}

func validReleaseCommit(sha string) bool {
	decoded, err := hex.DecodeString(sha)
	return len(sha) == 40 && sha == strings.ToLower(sha) && err == nil && len(decoded) == 20
}

func validateBuildRequest(r pipelines.ReleaseBuildRequest, bound bool) error {
	if r.PipelineID == "" || bareID(r.PipelineID) != r.PipelineID || r.Repository == "" || strings.ToLower(r.Repository) != r.Repository || !pipelines.ValidBranchName(r.Branch) {
		return errors.New("release build requires an exact pipeline, repository and default branch")
	}
	if _, _, err := splitRepository(r.Repository); err != nil {
		return err
	}
	if r.ExpectedSHA != "" && !validReleaseCommit(r.ExpectedSHA) || r.Tag != "" && !pipelines.ValidBranchName(r.Tag) {
		return errors.New("release build has an invalid selected commit or tag")
	}
	bound = bound || r.SourceID != "" || r.CredentialID != "" || r.InstallationID != 0 || r.RepositoryID != 0 || r.ExpectedSHA != ""
	if bound && (r.SourceID == "" || bareID(r.SourceID) != r.SourceID || r.CredentialID == "" || bareID(r.CredentialID) != r.CredentialID || r.InstallationID <= 0 || r.RepositoryID <= 0 || !validReleaseCommit(r.ExpectedSHA)) {
		return errors.New("release build requires its complete previously inspected source binding")
	}
	return nil
}

// releaseBuildPipeline checks both pipeline and source under the actual caller.
// A cluster owner reading another person's source cannot borrow its grant here.
func releaseBuildPipeline(ctx context.Context, d Deps, r pipelines.ReleaseBuildRequest) (Pipeline, error) {
	caller, err := personFrom(ctx)
	if err != nil {
		return Pipeline{}, err
	}
	if err = ctx.Err(); err != nil {
		return Pipeline{}, err
	}
	if d.Store == nil {
		return Pipeline{}, errNoStore
	}
	p, err := d.Store.PipelineForOwner(memql.ContextWithFreshRead(ctx), r.PipelineID)
	if err != nil {
		return Pipeline{}, err
	}
	if p == nil || !sameID(p.OwnerUserID, caller) || p.ID != r.PipelineID || p.PackageID == "" || p.Repository != r.Repository || p.DefaultBranch != r.Branch || p.InstallationID <= 0 || p.CredentialID == "" || p.Status != PipelineActive && p.Status != PipelineDisconnected {
		return Pipeline{}, errors.New("release pipeline is absent, foreign or no longer matches its selected source")
	}
	source, err := d.Store.PackageForCaller(memql.ContextWithFreshRead(ctx), p.PackageID)
	if err != nil {
		return Pipeline{}, err
	}
	if source == nil || source.ID != p.PackageID || !sameID(source.OwnerUserID, caller) || source.SourceKind != "repo" || source.Status != "" && source.Status != "active" || source.CredentialID != p.CredentialID {
		return Pipeline{}, errors.New("release source is absent, archived, foreign or has a changed connection")
	}
	repository, err := githubRepository(source.RepoURL)
	if err != nil || repository != p.Repository {
		return Pipeline{}, errors.New("release source repository changed")
	}
	if r.SourceID != "" && r.SourceID != p.PackageID || r.CredentialID != "" && r.CredentialID != p.CredentialID || r.InstallationID != 0 && r.InstallationID != p.InstallationID {
		return Pipeline{}, errors.New("release source binding changed after selection")
	}
	return *p, nil
}

func releaseBuildToken(ctx context.Context, d Deps, p Pipeline) (string, error) {
	if d.GitHub == nil {
		return "", errNoGitHub
	}
	token, installation, err := d.GitHub.InstallationToken(ctx, p.CredentialID, p.OwnerUserID, p.Repository)
	if err != nil {
		return "", err
	}
	if token == "" || installation != p.InstallationID {
		return "", errors.New("release source grant or installation changed")
	}
	return token, ctx.Err()
}

func (i *Integration) InspectReleaseBuild(ctx context.Context, r pipelines.ReleaseBuildRequest) (pipelines.ReleaseBuildSource, error) {
	source, _, _, err := i.inspectReleaseBuild(ctx, r)
	return source, err
}

func (i *Integration) inspectReleaseBuild(ctx context.Context, r pipelines.ReleaseBuildRequest) (pipelines.ReleaseBuildSource, Pipeline, string, error) {
	empty := pipelines.ReleaseBuildSource{}
	if err := validateBuildRequest(r, false); err != nil {
		return empty, Pipeline{}, "", err
	}
	d := i.snapshot()
	p, err := releaseBuildPipeline(ctx, d, r)
	if err != nil {
		return empty, p, "", err
	}
	provider, ok := d.GitHub.(releaseBuildGitHub)
	if !ok {
		return empty, p, "", errors.New("release source verification is unavailable")
	}
	token, err := releaseBuildToken(ctx, d, p)
	if err != nil {
		return empty, p, "", err
	}
	proof, err := provider.ObserveReleaseSource(ctx, token, p.Repository, p.DefaultBranch, r.ExpectedSHA)
	if err != nil {
		return empty, p, "", err
	}
	if proof.Repository != p.Repository || proof.Branch != p.DefaultBranch || proof.RepositoryID <= 0 || r.RepositoryID != 0 && proof.RepositoryID != r.RepositoryID || !validReleaseCommit(proof.SHA) || !validReleaseCommit(proof.HeadSHA) || !validReleaseCommit(proof.TreeSHA) || !validReleaseCommit(proof.ProofSHA) || r.ExpectedSHA != "" && proof.SHA != r.ExpectedSHA || len(proof.Checks) == 0 {
		return empty, p, "", errors.New("release verification returned a different or incomplete source")
	}
	if proof.ProofSHA != proof.SHA && proof.ProofPullRequest <= 0 || proof.ProofSHA == proof.SHA && proof.ProofPullRequest != 0 {
		return empty, p, "", errors.New("release check proof does not identify its source")
	}
	out := pipelines.ReleaseBuildSource{OwnerUserID: bareID(p.OwnerUserID), PipelineID: p.ID, SourceID: p.PackageID, CredentialID: p.CredentialID, Repository: p.Repository, Branch: p.DefaultBranch, HeadSHA: proof.HeadSHA, SHA: proof.SHA, TreeSHA: proof.TreeSHA, ProofSHA: proof.ProofSHA, InstallationID: p.InstallationID, RepositoryID: proof.RepositoryID, ProofPullRequest: proof.ProofPullRequest}
	for _, check := range proof.Checks {
		if check.Context == "" || check.AppID <= 0 || check.SHA != proof.ProofSHA || check.Status != "completed" || check.Conclusion != "success" {
			return empty, p, "", errors.New("required release check is not bound and successful")
		}
		out.Checks = append(out.Checks, pipelines.ReleaseBuildCheck{Context: check.Context, AppID: check.AppID, SHA: check.SHA, Status: check.Status, Conclusion: check.Conclusion})
	}
	if r.Tag != "" {
		sha, _, readErr := d.GitHub.CommitForRef(ctx, token, p.Repository, "refs/tags/"+r.Tag)
		if readErr != nil && githubapp.StatusOf(readErr) != http.StatusNotFound {
			return empty, p, "", readErr
		}
		if readErr == nil {
			if !validReleaseCommit(sha) {
				return empty, p, "", errors.New("release tag does not name a valid commit")
			}
			out.TagExists, out.TagSHA = true, sha
		}
	}
	selected := r
	selected.ExpectedSHA = proof.SHA
	if run, found, readErr := existingReleaseBuild(ctx, d, p, selected); readErr != nil {
		return empty, p, "", readErr
	} else if found {
		out.ExistingRun = &run
	}
	// Revalidate actual rows and grant after provider reads, not cached context.
	fresh, err := releaseBuildPipeline(ctx, d, r)
	if err != nil {
		return empty, p, "", err
	}
	if !sameReleasePipeline(p, fresh) {
		return empty, p, "", errors.New("release pipeline binding changed during inspection")
	}
	token, err = releaseBuildToken(ctx, d, fresh)
	if err != nil {
		return empty, p, "", err
	}
	return out, fresh, token, nil
}

func sameReleasePipeline(a, b Pipeline) bool {
	return a.ID == b.ID && sameID(a.OwnerUserID, b.OwnerUserID) && a.PackageID == b.PackageID && a.CredentialID == b.CredentialID && a.Repository == b.Repository && a.DefaultBranch == b.DefaultBranch && a.InstallationID == b.InstallationID && a.Compute == b.Compute && reflect.DeepEqual(a.SecretNames, b.SecretNames)
}

func existingReleaseBuild(ctx context.Context, d Deps, p Pipeline, r pipelines.ReleaseBuildRequest) (pipelines.ReleaseBuildRun, bool, error) {
	rows, err := d.Store.RunsForPipelineSHA(memql.ContextWithFreshRead(ctx), p.ID, r.ExpectedSHA)
	if err != nil {
		return pipelines.ReleaseBuildRun{}, false, err
	}
	if len(rows) >= 50 {
		return pipelines.ReleaseBuildRun{}, false, errors.New("release run history exceeded its complete observation bound")
	}
	var latest *Run
	for n := range rows {
		run := &rows[n]
		if run.Event != pipelines.EventRelease {
			continue
		}
		if run.ID == "" || run.PipelineID != p.ID || !sameID(run.OwnerUserID, p.OwnerUserID) || run.Repository != p.Repository || run.SHA != r.ExpectedSHA || run.Mode != pipelines.ModeFull || run.RunKey != pipelines.RunKey(p.Repository, r.ExpectedSHA, pipelines.ModeFull, pipelines.EventRelease) || run.Attempt <= 0 || run.Version == "" || run.ReleaseTag != "" && run.ReleaseTag != run.Version {
			return pipelines.ReleaseBuildRun{}, false, errors.New("existing release run has a conflicting source or version binding")
		}
		if latest != nil && latest.Attempt == run.Attempt && latest.ID != run.ID {
			return pipelines.ReleaseBuildRun{}, false, errors.New("existing release attempt is ambiguous")
		}
		if latest == nil || latest.Attempt < run.Attempt {
			latest = run
		}
	}
	if latest == nil {
		return pipelines.ReleaseBuildRun{}, false, nil
	}
	return projectReleaseBuild(*latest), true, nil
}

func projectReleaseBuild(r Run) pipelines.ReleaseBuildRun {
	return pipelines.ReleaseBuildRun{ID: r.ID, OwnerUserID: bareID(r.OwnerUserID), PipelineID: r.PipelineID, Repository: r.Repository, SHA: r.SHA, Tag: r.Version, Mode: string(r.Mode), Event: string(r.Event), Status: r.Status, Conclusion: r.Conclusion, WorkRunID: r.WorkRunID, Attempt: r.Attempt, CancelRequested: r.CancelRequested}
}

// ObserveReleaseBuild never opens a run or reports a check. It uses the same
// native identity that ordinary open/dedup uses, including another replica's
// result after an interrupted response.
func (i *Integration) ObserveReleaseBuild(ctx context.Context, r pipelines.ReleaseBuildRequest) (pipelines.ReleaseBuildRun, bool, error) {
	if err := validateBuildRequest(r, true); err != nil {
		return pipelines.ReleaseBuildRun{}, false, err
	}
	if r.Tag == "" {
		return pipelines.ReleaseBuildRun{}, false, errors.New("release observation requires the selected tag")
	}
	d := i.snapshot()
	p, err := releaseBuildPipeline(ctx, d, r)
	if err != nil {
		return pipelines.ReleaseBuildRun{}, false, err
	}
	token, err := releaseBuildToken(ctx, d, p)
	if err != nil {
		return pipelines.ReleaseBuildRun{}, false, err
	}
	info, err := d.GitHub.Repository(ctx, token, p.Repository)
	if err != nil {
		return pipelines.ReleaseBuildRun{}, false, err
	}
	if info.ID != r.RepositoryID || !strings.EqualFold(info.FullName, p.Repository) || info.DefaultBranch != p.DefaultBranch {
		return pipelines.ReleaseBuildRun{}, false, errors.New("release repository identity changed")
	}
	run, found, err := existingReleaseBuild(ctx, d, p, r)
	if err == nil && found && run.Tag != r.Tag {
		return pipelines.ReleaseBuildRun{}, false, errors.New("existing release run has a different tag")
	}
	return run, found, err
}

// StartReleaseBuild opens only the verified tag's exact selected source. Its
// ordinary open gate deduplicates across replicas; this never requests rerun.
func (i *Integration) StartReleaseBuild(ctx context.Context, r pipelines.ReleaseBuildRequest, beforeEffect func(context.Context) error) (pipelines.ReleaseBuildRun, error) {
	if beforeEffect == nil {
		return pipelines.ReleaseBuildRun{}, errors.New("release build requires its native authority fence")
	}
	if err := validateBuildRequest(r, true); err != nil {
		return pipelines.ReleaseBuildRun{}, err
	}
	if r.Tag == "" {
		return pipelines.ReleaseBuildRun{}, errors.New("release build requires its selected tag")
	}
	proof, p, token, err := i.inspectReleaseBuild(ctx, r)
	if err != nil {
		return pipelines.ReleaseBuildRun{}, err
	}
	if !proof.TagExists || proof.TagSHA != r.ExpectedSHA {
		return pipelines.ReleaseBuildRun{}, errors.New("selected release tag is not confirmed")
	}
	if proof.ExistingRun != nil {
		if proof.ExistingRun.Tag != r.Tag {
			return pipelines.ReleaseBuildRun{}, errors.New("this commit already has a release run for a different tag")
		}
		return *proof.ExistingRun, nil
	}
	if err := beforeEffect(ctx); err != nil {
		return pipelines.ReleaseBuildRun{}, err
	}
	d := i.snapshot()
	opened, err := i.openWithToken(ctx, d, p, Opening{Event: pipelines.EventRelease, Mode: pipelines.ModeFull, SHA: r.ExpectedSHA, ReleaseTag: r.Tag, Version: r.Tag, Title: "Release " + r.Tag, Trigger: TriggerManual,
		beforeCreate: func(fctx context.Context) error {
			fresh, err := releaseBuildPipeline(fctx, d, r)
			if err != nil {
				return err
			}
			if !sameReleasePipeline(p, fresh) {
				return errors.New("release source changed before opening")
			}
			if err := fctx.Err(); err != nil {
				return err
			}
			return beforeEffect(fctx)
		},
	}, token)
	if err != nil {
		return pipelines.ReleaseBuildRun{}, err
	}
	if opened.Run.ID == "" || opened.Run.Version != r.Tag || opened.Run.SHA != r.ExpectedSHA || opened.Run.Mode != pipelines.ModeFull || opened.Run.Event != pipelines.EventRelease {
		return pipelines.ReleaseBuildRun{}, fmt.Errorf("release opening did not confirm the selected source and version")
	}
	return projectReleaseBuild(opened.Run), nil
}
