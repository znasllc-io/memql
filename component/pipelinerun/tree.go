package pipelinerun

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/packages"
	"github.com/znasllc-io/memql/component/pipelines"
)

// tree.go -- the plan a run executes, read from the repository AT THE RUN'S
// COMMIT (plan decision 5, design record D3, D8, D9).
//
// The pipeline row holds no manifest: every run reads memql-package.yaml from
// the tarball of its own SHA, so a run can never execute a pipeline its tree
// no longer declares, and a resumed run reads the same plan its first driver
// did. The driver holds a tarball, not a toolchain, so the Go import graph a
// `packages:` step selects from is read from source (pipelines.ScanGoTree),
// and what changed is GitHub's compare of the run's base with its head.
//
// EVERYTHING THAT GOES WRONG HERE IS A TYPED FAILURE OF THE RUN (D9): a tree
// GitHub would not hand over, one too large to read, a manifest that does not
// parse or declares no pipeline, a block that does not validate or compile.
// The run concludes failure carrying the refusal, and its check run says what
// is wrong and what to do; it is never skipped, because a skipped check reads
// as green. What cannot be READ is the one exception, and it widens rather
// than fails: a compare that failed or stopped listing selects everything.

// maxTreeBytes caps what a run reads of a repository: the kept files only
// (the manifest, go.mod, go.work, the .go sources). The engine's own
// repository keeps about 52 MB of them; a repository past the cap fails its
// run source_too_large rather than holding an agent's memory hostage.
const maxTreeBytes = 128 << 20

// keepForRun admits what a run reads: the manifest at the root, and every
// go.mod, go.work and .go file outside the directories the go tool skips
// (testdata, vendor, and any name beginning "." or "_") -- whose files no
// package of the graph could hold, so reading them would spend the cap on
// nothing.
func keepForRun(p string) bool {
	if p == pipelines.ManifestPath {
		return true
	}
	dir, base := path.Split(p)
	if base != "go.mod" && base != "go.work" && !strings.HasSuffix(base, ".go") {
		return false
	}
	if strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_") {
		return false
	}
	for _, segment := range strings.Split(strings.Trim(dir, "/"), "/") {
		if segment == "testdata" || segment == "vendor" || strings.HasPrefix(segment, ".") || strings.HasPrefix(segment, "_") {
			return false
		}
	}
	return true
}

// readPlan reads the tree at the run's commit and compiles its pipeline block
// into the plan this run executes, or answers the refusal that fails the run.
func (dr *runDriver) readPlan(ctx context.Context) (pipelines.Plan, *pipelines.Refusal) {
	d, p, run := dr.d, dr.p, dr.run
	treeCtx, treeDone := dr.stoppable(ctx, treeReadTimeout)
	tree, err := d.GitHub.Tree(treeCtx, dr.token, p.Repository, run.SHA, keepForRun, maxTreeBytes)
	treeDone()
	switch {
	case errors.Is(err, ErrTreeTooLarge):
		return pipelines.Plan{}, pipelines.Refuse(packages.CodeSourceTooLarge, "",
			"%s at %s holds more Go source than a pipeline run reads (%d MB of %s, go.mod, go.work and .go files), so its steps could not be chosen.",
			p.Repository, shortSHA(run.SHA), maxTreeBytes>>20, pipelines.ManifestPath)
	case err != nil:
		return pipelines.Plan{}, pipelines.Refuse(packages.CodeSourceUnreadable, "",
			"GitHub did not hand over %s at %s, so its pipeline could not be read: %v. Re-run once GitHub answers.",
			p.Repository, shortSHA(run.SHA), err)
	}

	manifest, err := packages.ReadManifest(tree)
	if err != nil {
		var refusal *packages.Refusal
		if errors.As(err, &refusal) {
			return pipelines.Plan{}, &pipelines.Refusal{Code: refusal.Code, Detail: refusal.Detail, Scope: refusal.Scope}
		}
		return pipelines.Plan{}, pipelines.Refuse(packages.CodeManifestInvalid, "", "%s could not be read: %v", pipelines.ManifestPath, err)
	}
	spec := manifest.Pipeline
	if spec == nil {
		return pipelines.Plan{}, pipelines.Refuse(pipelines.CodeNotDeclared, "",
			"%s at %s declares no pipeline block, so this commit has nothing to run.", pipelines.ManifestPath, shortSHA(run.SHA))
	}
	// Validated before anything is computed from the block: a select.full
	// glob that does not compile is the manifest's mistake, refused as one,
	// not a selection that failed.
	if refusal := pipelines.Validate(spec); refusal != nil {
		return pipelines.Plan{}, refusal
	}
	if err := dr.prepareWorkflow(spec.Workflow); err != nil {
		return pipelines.Plan{}, pipelines.Refuse(pipelines.CodeStageInvalid, "workflow", "%s", err)
	}
	stageSelection, err := dr.selectStages(ctx, spec.Stages, run.Event, run.Mode)
	if err != nil {
		return pipelines.Plan{}, pipelines.Refuse(pipelines.CodeStageInvalid, "selection",
			"The pipeline DSL could not select stages for this run: %v.", err)
	}

	var changed []string
	changedKnown := false
	if run.Mode == pipelines.ModeAffected && (pipelines.NeedsSelector(spec) || pipelines.NeedsBucketSelection(spec)) {
		changed, changedKnown = dr.changes(ctx)
	}
	in := pipelines.CompileInput{
		Mode:           run.Mode,
		Event:          run.Event,
		Compute:        p.Compute,
		AllowedSecrets: p.SecretNames,
		Timings:        p.Timings,
		StageSelection: &stageSelection,
	}
	if pipelines.NeedsBucketSelection(spec) {
		selection, err := dr.selectBuckets(ctx, spec, run.Mode, changed, changedKnown)
		if err != nil {
			return pipelines.Plan{}, pipelines.Refuse(pipelines.CodeSelectInvalid, "selection/buckets",
				"The pipeline DSL could not select changed-path buckets for this run: %v.", err)
		}
		in.BucketSelection = &selection
	}
	if pipelines.NeedsSelector(spec) {
		graph, err := pipelines.ScanGoTree(tree)
		if err != nil {
			return pipelines.Plan{}, pipelines.Refuse(packages.CodeSourceUnreadable, "",
				"The Go sources of %s at %s could not be read: %v.", p.Repository, shortSHA(run.SHA), err)
		}
		var full []string
		if spec.Select != nil {
			full = spec.Select.Full
		}
		facts, err := pipelines.AnalyzeSelection(graph, changed, changedKnown, full)
		if err != nil {
			return pipelines.Plan{}, pipelines.Refuse(pipelines.CodeSelectInvalid, "select/full", "%v", err)
		}
		decision, err := dr.selectPackageCoverage(ctx, facts, run.Mode)
		if err != nil {
			return pipelines.Plan{}, pipelines.Refuse(pipelines.CodeSelectInvalid, "selection/packages",
				"The pipeline DSL could not select package coverage for this run: %v.", err)
		}
		selection, err := pipelines.ResolveSelection(graph, facts, decision)
		if err != nil {
			return pipelines.Plan{}, pipelines.Refuse(pipelines.CodeSelectInvalid, "selection/packages",
				"The pipeline DSL selected package coverage that the import graph cannot safely execute: %v.", err)
		}
		in.Selector = pipelines.GraphSelector(graph, selection)
		policies, err := dr.selectPackagePolicies(ctx, spec, run.Mode, selection)
		if err != nil {
			return pipelines.Plan{}, pipelines.Refuse(pipelines.CodeSelectInvalid, "selection/packages",
				"The pipeline DSL could not select per-step package coverage: %v.", err)
		}
		in.PackagePolicies = policies
	}
	return pipelines.Compile(spec, in)
}

// changes is what this run changed: GitHub's compare of its base with its
// head. Known only when the compare answered whole; a run with no base, a
// compare that failed and one that stopped listing (300 files) are all
// unknown, which selects everything and gates no bucket.
func (dr *runDriver) changes(ctx context.Context) ([]string, bool) {
	base := strings.TrimSpace(dr.run.BaseSHA)
	if base == "" {
		return nil, false
	}
	compareCtx, compareDone := dr.stoppable(ctx, githubCallTimeout)
	files, complete, err := dr.d.GitHub.Compare(compareCtx, dr.token, dr.p.Repository, base, dr.run.SHA)
	compareDone()
	if err != nil || !complete {
		reason := "GitHub stopped listing the changed files"
		if err != nil {
			reason = err.Error()
		}
		dr.log.Info("pipelines: the change could not be read whole, so the run selects everything", "reason", reason)
		return nil, false
	}
	return files, true
}

// grantRefusal is a failed token mint as the refusal that fails the run:
// Deployables' own code when the grant path refused (credential_revoked,
// repository_not_installed, ...), and source_unreadable when GitHub could
// not be asked at all.
func grantRefusal(repository string, err error) *pipelines.Refusal {
	var refusal *packages.Refusal
	switch {
	case errors.As(err, &refusal):
		return &pipelines.Refusal{Code: refusal.Code, Detail: refusal.Detail, Scope: refusal.Scope}
	case errors.Is(err, githubapp.ErrNotConfigured):
		return pipelines.Refuse(packages.CodeGithubAppNotConfigured, "",
			"This cluster has no GitHub App, so no token could be minted to read %s or report its check.", repository)
	case errors.Is(err, githubapp.ErrNotInstalled):
		return pipelines.Refuse(packages.CodeRepositoryNotInstalled, "",
			"The GitHub App is not installed on %s, so no token could be minted for it.", repository)
	}
	return pipelines.Refuse(packages.CodeSourceUnreadable, "",
		"No token could be had for %s through the pipeline owner's GitHub connection: %v. Re-run once GitHub answers.",
		repository, fmt.Sprint(err))
}
