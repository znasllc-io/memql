package pipelinerun

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
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
	WalkReleaseSource(context.Context, string, string, string, int) (githubapp.ReleaseSourceWalk, error)
}

func (g *packagesGitHub) ObserveReleaseSource(ctx context.Context, token, repository, branch, selected string) (githubapp.ReleaseSourceEvidence, error) {
	c, owner, repo, err := g.target(repository)
	if err != nil {
		return githubapp.ReleaseSourceEvidence{}, err
	}
	return c.ObserveReleaseSource(ctx, token, owner, repo, branch, selected)
}

func (g *packagesGitHub) WalkReleaseSource(ctx context.Context, token, repository, branch string, lookBack int) (githubapp.ReleaseSourceWalk, error) {
	c, owner, repo, err := g.target(repository)
	if err != nil {
		return githubapp.ReleaseSourceWalk{}, err
	}
	return c.WalkReleaseSource(ctx, token, owner, repo, branch, lookBack)
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

// InspectReleaseBuildAt selects a named commit rather than the branch's head: a train
// tags the commit its run tested, and main's head, pushed to directly, has no check of
// its own (design record 2026-11-02-nightly-release-from-main-design.md, D5). It answers
// the request bound to that commit, and what a bound request is checked against is the
// same as for a person's selection.
func (i *Integration) InspectReleaseBuildAt(ctx context.Context, r pipelines.ReleaseBuildRequest, sha string) (pipelines.ReleaseBuildRequest, pipelines.ReleaseBuildSource, error) {
	empty := pipelines.ReleaseBuildSource{}
	if !validReleaseCommit(sha) {
		return r, empty, errors.New("release build requires an exact selected commit")
	}
	if err := validateBuildRequest(r, false); err != nil {
		return r, empty, err
	}
	d := i.snapshot()
	p, err := releaseBuildPipeline(ctx, d, r)
	if err != nil {
		return r, empty, err
	}
	provider, ok := d.GitHub.(releaseBuildGitHub)
	if !ok {
		return r, empty, errors.New("release source verification is unavailable")
	}
	token, err := releaseBuildToken(ctx, d, p)
	if err != nil {
		return r, empty, err
	}
	proof, err := provider.ObserveReleaseSource(ctx, token, p.Repository, p.DefaultBranch, sha)
	if err != nil {
		return r, empty, err
	}
	r.ExpectedSHA, r.SourceID, r.CredentialID = sha, p.PackageID, p.CredentialID
	r.InstallationID, r.RepositoryID = p.InstallationID, proof.RepositoryID
	source, err := i.InspectReleaseBuild(ctx, r)
	return r, source, err
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
	// Trigger: the request names no commit, which only release preparation's
	// first read does (every later read is bound to the commit it saved). The
	// walk chooses the newest commit whose required checks passed, and the
	// proof below judges that choice exactly as it judges a named commit. A
	// refused pick is answered with the proof's refusal, never by walking on:
	// the walk read it as green (or as a merge only the proof can judge) and
	// the proof found otherwise, so an older pick would hide why, and a retry
	// walks again from the evidence as it then stands.
	chosen, walked := r.ExpectedSHA, int64(0)
	var walk *pipelines.ReleaseSourceWalk
	if chosen == "" {
		if chosen, walk, walked, err = walkReleaseSource(ctx, provider, token, p); err != nil {
			return empty, p, "", err
		}
	}
	proof, err := provider.ObserveReleaseSource(ctx, token, p.Repository, p.DefaultBranch, chosen)
	if err != nil {
		return empty, p, "", err
	}
	if proof.Repository != p.Repository || proof.Branch != p.DefaultBranch || proof.RepositoryID <= 0 || r.RepositoryID != 0 && proof.RepositoryID != r.RepositoryID || walked != 0 && proof.RepositoryID != walked || !validReleaseCommit(proof.SHA) || !validReleaseCommit(proof.HeadSHA) || !validReleaseCommit(proof.TreeSHA) || !validReleaseCommit(proof.ProofSHA) || proof.SHA != chosen || len(proof.Checks) == 0 {
		return empty, p, "", errors.New("release verification returned a different or incomplete source")
	}
	if proof.ProofSHA != proof.SHA && proof.ProofPullRequest <= 0 || proof.ProofSHA == proof.SHA && proof.ProofPullRequest != 0 {
		return empty, p, "", errors.New("release check proof does not identify its source")
	}
	out := pipelines.ReleaseBuildSource{OwnerUserID: bareID(p.OwnerUserID), PipelineID: p.ID, SourceID: p.PackageID, CredentialID: p.CredentialID, Repository: p.Repository, Branch: p.DefaultBranch, HeadSHA: proof.HeadSHA, SHA: proof.SHA, TreeSHA: proof.TreeSHA, ProofSHA: proof.ProofSHA, InstallationID: p.InstallationID, RepositoryID: proof.RepositoryID, ProofPullRequest: proof.ProofPullRequest, Walk: walk}
	for _, check := range proof.Checks {
		if check.Context == "" || check.AppID <= 0 || check.SHA != proof.ProofSHA || check.Status != "completed" || check.Conclusion != "success" {
			return empty, p, "", errors.New("required release check is not bound and successful")
		}
		out.Checks = append(out.Checks, pipelines.ReleaseBuildCheck{Context: check.Context, AppID: check.AppID, SHA: check.SHA, Status: check.Status, Conclusion: check.Conclusion})
	}
	if r.Tag != "" {
		sha, _, readErr := d.GitHub.CommitForRef(ctx, token, p.Repository, "refs/tags/"+r.Tag)
		if readErr != nil && !githubapp.IsMissingRef(readErr) {
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
	byID := map[string]Run{}
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
		byID[bareID(run.ID)] = *run
		if latest == nil || latest.Attempt < run.Attempt {
			latest = run
		}
	}
	if latest == nil {
		return pipelines.ReleaseBuildRun{}, false, nil
	}
	out := projectReleaseBuild(*latest)
	out.RerunOf = bareID(latest.RerunOf)
	out.Chain = releaseAttemptChain(*latest, byID)
	return out, true, nil
}

// maxReleaseAttempts bounds how far back a release attempt chain is followed.
// Attempt numbers strictly decrease along a chain, so it also bounds the
// chain's length and rules out a cycle.
const maxReleaseAttempts = 50

// releaseSourceLookBack bounds how many commits of the default branch's
// first-parent history release preparation examines, the one it chooses
// included. A full push run takes two to three hours and the owner may push
// several times while one runs, so the newest green commit is usually a
// handful of commits back; 20 covers several hours of pushes. The cost is about one
// GitHub call per commit examined plus three (the head, its rules, the
// history), inside preparation's two-minute budget (integrations/release
// prepareBuildAt). Further back than that, the release would be of work old
// enough that the person should look at why nothing since has passed, which is
// what the refusal release_source_untested says.
const releaseSourceLookBack = 20

// walkReleaseSource asks GitHub for the newest commit worth proving and checks
// what it answers before believing any of it: the repository, branch and bound
// it was asked about, and a record of the commits it passed over that starts at
// the head and never names the commit it chose. It returns the chosen commit,
// the record to save with it (nil when the head itself was chosen, so that
// source encodes as before), and the repository id the walk read, which the
// proof must match. A walk that chose nothing is the typed
// ReleaseSourceUntestedError, carrying what it found.
func walkReleaseSource(ctx context.Context, provider releaseBuildGitHub, token string, p Pipeline) (string, *pipelines.ReleaseSourceWalk, int64, error) {
	w, err := provider.WalkReleaseSource(ctx, token, p.Repository, p.DefaultBranch, releaseSourceLookBack)
	if err != nil {
		return "", nil, 0, err
	}
	if w.Repository != p.Repository || w.Branch != p.DefaultBranch || w.RepositoryID <= 0 || w.LookBack != releaseSourceLookBack || !validReleaseCommit(w.HeadSHA) || w.Pick != "" && !validReleaseCommit(w.Pick) {
		return "", nil, 0, errors.New("release source walk returned a different or incomplete history")
	}
	record := pipelines.ReleaseSourceWalk{HeadSHA: w.HeadSHA, LookBack: w.LookBack}
	for _, skip := range w.Newer {
		record.Newer = append(record.Newer, pipelines.ReleaseSourceSkip{SHA: skip.SHA, State: skip.State, Check: skip.Check, Conclusion: skip.Conclusion})
	}
	if len(record.Newer) == 0 {
		// Nothing passed over: the walk chose the head, or it examined nothing.
		if w.Pick == "" || w.Pick != w.HeadSHA {
			return "", nil, 0, errors.New("release source walk passed over commits it did not record")
		}
		return w.Pick, nil, w.RepositoryID, nil
	}
	if err := record.Validate(w.Pick); err != nil {
		return "", nil, 0, err
	}
	if w.Pick == "" {
		return "", nil, 0, &pipelines.ReleaseSourceUntestedError{Branch: p.DefaultBranch, Walk: record}
	}
	return w.Pick, &record, w.RepositoryID, nil
}

// releaseAttemptChain follows latest's rerunOf links through byID back to
// attempt 1 and returns the earlier attempts, newest first. Every link must
// stay inside latest's run key and name a strictly earlier attempt. A missing
// link, a chain that ends above attempt 1, or one longer than
// maxReleaseAttempts answers nil. A nil chain is not an observation error: the
// release keeps being observed, and a saved release simply cannot adopt the
// attempt (docs/superpowers/specs/2026-10-28-release-failed-only-rerun-design.md).
func releaseAttemptChain(latest Run, byID map[string]Run) []pipelines.ReleaseBuildAttempt {
	if latest.Attempt > maxReleaseAttempts {
		return nil
	}
	var chain []pipelines.ReleaseBuildAttempt
	current := latest
	for current.Attempt > 1 {
		prev, ok := byID[bareID(current.RerunOf)]
		if !ok || prev.RunKey != latest.RunKey || prev.Attempt >= current.Attempt {
			return nil
		}
		chain = append(chain, pipelines.ReleaseBuildAttempt{ID: prev.ID, Attempt: prev.Attempt, Status: prev.Status, Conclusion: prev.Conclusion, CancelRequested: prev.CancelRequested})
		current = prev
	}
	if current.Attempt != 1 {
		return nil
	}
	return chain
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
