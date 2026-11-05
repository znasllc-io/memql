package pipelinerun

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/automations/workflowhost"
	"github.com/znasllc-io/memql/component/identity/githubapp"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
)

// carry.go -- a push to the default branch carries a verified step, or one
// slice of a sharded step, forward from an earlier push that executed it, when
// the change between the two commits leaves it untouched (design record
// docs/superpowers/specs/2026-11-05-owner-pushes-main-design.md, D5; the
// connected workflow is docs/public/operate/pipelines.md, "Carrying a verified
// step forward on the default branch").
//
// WHY. The owner pushes to main with no pull request, and every push opens a
// full run, which used to execute every verification step again even when the
// push touched one package. A pull request's affected run already decides what a
// change can reach; this asks the same question of the change since an earlier
// push, and stands on that push's receipt for what it cannot reach.
//
// THE RULE, every part of which must hold for one slice:
//
//   - The run is a push's first attempt, in full mode, on the pipeline's default
//     branch (carriesForward).
//   - The installed evidenceReuse template declares `relation` and admits the
//     source when asked with relation "ancestor" (evidenceAsksRelation,
//     evidenceAdmitted). A template that does not declare it is never asked, and
//     the portable default declares none, so carrying forward stays off until an
//     installation opts in.
//   - The source is an earlier successful full push of the same pipeline, owner,
//     repository and branch, under the same policy version, that finished within
//     carryWindow and was never asked to stop (carrySourceProblem). At most
//     maxCarrySources of them are compared, newest first.
//   - GitHub proves the source's commit a proper ancestor of this one and lists
//     the change between them whole (githubapp.CompareAncestry; at 300 paths it
//     stops listing, and an unlisted change proves nothing).
//   - The change, read through the pinned policies an affected pull request's
//     selection is read through, leaves the slice untouched (readChange,
//     untouched): no package of the slice is reached by it, or the step's bucket
//     is not selected, or the tree did not change at all. A change that selects
//     everything (a module file, the manifest, a select.full path, an incomplete
//     graph) carries nothing.
//   - The source EXECUTED the step over exactly the slice's packages: a done
//     command naming no artifact and no other run's evidence, under the slice's
//     carry fingerprint (carrySourceRow, carryFingerprintOf). A step the source
//     carried or reused is a skip and never a source, so a pass is never carried
//     from a carry, and every slice executes for real at least once within
//     carryWindow while the branch moves.
//
// EVIDENCE AND TRUST. The selection is the one pull requests have relied on: the
// import graph plus the manifest's select.full, buckets and test inputs. What the
// graph cannot see -- a test that reads another package's files by path, a script
// a step runs -- is the residual risk. The window and "executed only" bound it in
// time; a repository bounds it in its manifest (this repository's
// memql-package.yaml puts its step runners under select.full and runs its
// repository-reading gates on every push).
//
// DECIDED ONCE. The decision is made before the work run opens (openWork), so it
// is journaled in each step's declaration -- call.skip, evidenceRunId,
// carriedFrom, carryFingerprint and the slice's packages -- and a takeover
// restores it from the rows (resume, adoptRows) and never decides again, even
// after GitHub stops proving the ancestry or the timing table moves.
//
// FALLBACK. Anything unproven runs the step. GitHub not answering costs the run
// its saving and nothing else. A store read or a policy that fails stops the
// drive, as reuse does (reuseVerification), because running everything is not
// what was planned and recovery tries again.
//
// RELEASES never stand on a carried step. A release reuses only receipts a run
// executed (reuseGroup, pipelines.InspectReusedReleaseReceipt), so it executes
// every step the push of its tree carried, all its shards; and candidate coverage
// refuses a carried row named as evidence (pipelines.ProveReusedStep accepts only
// a full release as a reuser). A failed-only re-run of a push runs a carried step
// again rather than stretching its pass past the window (carryPassed).
//
// Tests: carry_test.go; TestACarriedStepShowsWhereItWasVerified
// (step_checks_test.go); the db-gated TestAPushCarriesForwardOverRealRows
// (carry_db_test.go); and TestACarriedForwardStepStandsForNoRelease
// (integrations/release, db-gated).

const (
	// carryWindow is how old a receipt may be when a push carries it: from the
	// source run's finish to the decision. It is evidence reuse's window
	// (reuseVerification, pipelines.ReleaseReuseWindow).
	carryWindow = 24 * time.Hour
	// maxCarrySources bounds the earlier pushes one run compares itself with:
	// each costs one GitHub compare, newest first.
	maxCarrySources = 4
	// carryCommit stands for the commit in a carry fingerprint
	// (carryFingerprintOf). It is no hash, so a carry fingerprint never equals
	// one that binds a commit or a tree.
	carryCommit = "carried-forward"
)

// carriedFrom is what a carried step's call records of the receipt it stands
// on: the push that executed it and that push's work run, its commit and tree,
// and the paths GitHub's compare listed from that commit to this one. The run's
// definition fingerprint covers every call, so the spelling is fixed (call) and
// a takeover writes the record it read back exactly (carriedFromCall).
type carriedFrom struct {
	PipelineRunID string
	WorkRunID     string
	SHA           string
	TreeHash      string
	ChangedPaths  []string
}

// call is the record as a step's call spells it. The change is always a list,
// an empty one included, so a record read back from its row writes the same
// JSON it was declared with.
func (c *carriedFrom) call() map[string]any {
	out := map[string]any{
		"pipelineRunId": c.PipelineRunID,
		"workRunId":     c.WorkRunID,
		"sha":           c.SHA,
		"changedPaths":  append([]string{}, c.ChangedPaths...),
	}
	if c.TreeHash != "" {
		out["treeHash"] = c.TreeHash
	}
	return out
}

// carriedFromCall reads the record back from a step's call: nil when it holds
// none. A path is kept exactly as stored, never trimmed, because the record must
// write back what it was declared as.
func carriedFromCall(value any) *carriedFrom {
	m, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	c := &carriedFrom{
		PipelineRunID: rowString(m, "pipelineRunId"),
		WorkRunID:     rowString(m, "workRunId"),
		SHA:           rowString(m, "sha"),
		TreeHash:      rowString(m, "treeHash"),
		ChangedPaths:  []string{},
	}
	switch paths := m["changedPaths"].(type) {
	case []string:
		c.ChangedPaths = append(c.ChangedPaths, paths...)
	case []any:
		for _, p := range paths {
			if s, ok := p.(string); ok {
				c.ChangedPaths = append(c.ChangedPaths, s)
			}
		}
	}
	return c
}

// carriesForward reports a run that may carry a step forward: a push's first
// attempt, in full mode, on its pipeline's default branch. A push to another
// branch opens no run (pipelines.ClassifyDelivery), so the branch is asked here
// only as a guard. A re-run is someone asking for the checks to run, or carries
// inside its own attempt chain (carryPassed); an affected run has a selection of
// its own; a qualification proves its own execution.
func (dr *runDriver) carriesForward() bool {
	branch := strings.TrimPrefix(strings.TrimSpace(dr.run.HeadBranch), "refs/heads/")
	return dr.run.Event == pipelines.EventPush && dr.run.Mode == pipelines.ModeFull &&
		strings.TrimSpace(dr.run.RerunOf) == "" && !dr.run.RerunFailedOnly && dr.run.Qualification == nil &&
		branch != "" && branch == strings.TrimSpace(dr.p.DefaultBranch)
}

// evidenceAsksRelation reports whether the installed evidenceReuse template
// declares a `relation` argument: the installation's opt-in to being told how a
// source relates to the run reusing it (relationCommit, relationTree,
// relationAncestor), and so to carrying forward. It reads the run's captured
// policy, as stageUsesPullRequest does, so a takeover on another replica reads
// the same answer; and it reads it once per drive. A relation declared as
// anything but a string is the policy's mistake, refused as an error.
func (dr *runDriver) evidenceAsksRelation() (bool, error) {
	if dr.evidenceRelation != nil {
		return *dr.evidenceRelation, nil
	}
	if dr.workflow == nil {
		return false, errors.New("the evidence policy has not been prepared")
	}
	a := dr.workflow.definitions["pipelineEvidenceReuse"]
	if s := dr.workflow.snapshot; s != nil {
		entry := s.PhaseEntry("pipelineEvidenceReuse")
		prepared, err := workflowhost.PrepareSnapshotEntry(s, pipelinePolicyContract, entry, workflowhost.Options{Operations: pipelinePolicyOperations()})
		if err != nil {
			return false, err
		}
		if a, err = prepared.Automation(entry); err != nil {
			return false, err
		}
	}
	declared := false
	if a != nil && a.Args != nil {
		for _, f := range a.Args.Fields {
			if f == nil || f.Name != "relation" {
				continue
			}
			if f.Type != "string" && f.Type != "any" {
				return false, errors.New("the evidence policy's relation argument must be a string")
			}
			declared = true
		}
	}
	dr.evidenceRelation = &declared
	return declared, nil
}

// carryGroup is one manifest step's tracks, its shards together in plan order,
// with the step as the manifest declares it. fixed: a slice was carried, so the
// split may no longer move.
type carryGroup struct {
	tracks   []*stepTrack
	declared pipelines.StepSpec
	fixed    bool
}

// carryGroups is every step that declares a carry fingerprint, as groups.
func (dr *runDriver) carryGroups() []*carryGroup {
	if dr.planSpec == nil {
		return nil
	}
	declared := map[string]pipelines.StepSpec{}
	for _, stage := range dr.planSpec.Stages {
		for _, step := range stage.Steps {
			declared[pipelines.StepKey(stage.Name, step.Name)] = step
		}
	}
	var out []*carryGroup
	for _, tracks := range stepGroups(dr.tracks, func(t *stepTrack) bool { return t.carry != "" }) {
		if step, ok := declared[pipelines.StepKey(tracks[0].step.Stage, tracks[0].step.Name)]; ok {
			out = append(out, &carryGroup{tracks: tracks, declared: step})
		}
	}
	return out
}

// undecided reports a slice of groups that is still to run: neither carried nor
// skipped for another reason.
func undecided(groups []*carryGroup) bool {
	for _, g := range groups {
		for _, t := range g.tracks {
			if t.step.Skip == nil {
				return true
			}
		}
	}
	return false
}

// carryForward plans as skips, before the work run opens, the verification
// steps and shard slices earlier pushes executed and the change since left
// untouched: the rule is in the file comment. It answers false when the drive
// must stop.
func (dr *runDriver) carryForward(ctx context.Context) bool {
	groups := dr.carryGroups()
	if !undecided(groups) {
		return true
	}
	declared, err := dr.evidenceAsksRelation()
	if err != nil {
		dr.abort("reading the verification evidence policy", err)
		return false
	}
	if !declared {
		// The installation has not opted in: nothing is read and nothing asked.
		return true
	}
	fresh := memql.ContextWithFreshRead(ctx)
	sources, err := dr.d.Store.PreviousRuns(fresh, dr.p.ID, pipelines.EventPush)
	if err != nil {
		dr.abort("reading the earlier pushes a step could be carried forward from", err)
		return false
	}
	now := dr.d.now()
	compared, carried := 0, 0
	for _, source := range sources {
		if compared >= maxCarrySources || !undecided(groups) {
			break
		}
		if problem := dr.carrySourceProblem(source, now); problem != "" {
			dr.log.Debug("pipelines: an earlier push cannot stand for this one", "source", source.ID, "why", problem)
			continue
		}
		allowed, ok := dr.evidenceAdmitted(ctx, source, now.Sub(source.FinishedAt), relationAncestor)
		if !ok {
			return false
		}
		if !allowed {
			continue
		}
		compared++
		ancestry, err := dr.compareAncestry(ctx, source.SHA)
		switch {
		case dr.lease.isLost():
			return false
		case dr.lease.isCancelled():
			// The drive answers the cancel next. What was decided stands; nothing
			// more is.
			return true
		case err != nil:
			dr.log.Info("pipelines: GitHub did not compare an earlier push with this one, so nothing is carried from it",
				"source", source.ID, "error", dr.mask(err.Error()))
			continue
		case !ancestry.Ancestor:
			dr.log.Info("pipelines: an earlier push is not proven an ancestor of this one, so nothing is carried from it", "source", source.ID)
			continue
		case !ancestry.Complete:
			dr.log.Info("pipelines: GitHub stopped listing the change since an earlier push, so nothing is carried from it", "source", source.ID)
			continue
		}
		change, ok := dr.readChange(ctx, source, ancestry.Files)
		if !ok {
			return false
		}
		if change.full {
			dr.log.Info("pipelines: the change since an earlier push reaches everything, so nothing is carried from it",
				"source", source.ID, "why", dr.mask(change.reason))
			continue
		}
		rows, err := dr.d.Store.WorkSteps(fresh, source.WorkRunID)
		if err != nil {
			dr.abort("reading the receipts of an earlier push", err)
			return false
		}
		byKey := make(map[string]WorkStep, len(rows))
		for _, row := range rows {
			byKey[row.Key] = row
		}
		from := carriedFrom{PipelineRunID: bareID(source.ID), WorkRunID: bareID(source.WorkRunID), SHA: source.SHA,
			TreeHash: source.TreeHash, ChangedPaths: slices.Clone(ancestry.Files)}
		for _, g := range groups {
			carried += dr.carryFromSource(g, source, from, change, byKey)
		}
	}
	if carried > 0 {
		dr.log.Info("pipelines: a push carries forward the verified steps its change left untouched", "steps", carried, "compared", compared)
	}
	return true
}

// carrySourceProblem says why an earlier run cannot stand for this push, or ""
// when it can. These are native rules, asked before the policy is: only a push
// that ran everything and passed, of this pipeline, owner, repository and
// branch, under this run's policy version, finished within carryWindow and never
// asked to stop. A source at this run's own commit is same-commit reuse's
// (reuseVerification), and no proper ancestor.
func (dr *runDriver) carrySourceProblem(source Run, now time.Time) string {
	age := now.Sub(source.FinishedAt)
	switch {
	case source.ID == "" || sameID(source.ID, dr.run.ID):
		return "the source is this run"
	case source.Event != pipelines.EventPush || source.Mode != pipelines.ModeFull:
		return "the source is not a full push"
	case !source.Finished() || source.Conclusion != ConclusionSuccess:
		return "the source did not succeed"
	case source.CancelRequested:
		return "the source was asked to stop"
	case !sameID(source.OwnerUserID, dr.run.OwnerUserID):
		return "the source belongs to another owner"
	case !sameID(source.PipelineID, dr.run.PipelineID):
		return "the source belongs to another pipeline"
	case source.Repository != dr.run.Repository:
		return "the source belongs to another repository"
	case source.HeadBranch != dr.run.HeadBranch:
		return "the source pushed another branch"
	case strings.TrimSpace(source.WorkRunID) == "":
		return "the source has no work run"
	case source.SHA == "" || source.SHA == dr.run.SHA:
		return "the source is at this run's commit"
	case source.FinishedAt.IsZero() || age < 0 || age > carryWindow:
		return "the source finished outside the reuse window"
	case source.PolicySnapshot == nil || dr.run.PolicySnapshot == nil || source.PolicySnapshot["version"] != dr.run.PolicySnapshot["version"]:
		return "the source ran under another policy version"
	}
	return ""
}

// compareAncestry asks GitHub whether base is a proper ancestor of this run's
// commit, and what changed between them. Bounded, and given up the moment the
// run is cancelled or lost: a call to GitHub never holds a drive.
func (dr *runDriver) compareAncestry(ctx context.Context, base string) (githubapp.Ancestry, error) {
	compareCtx, done := dr.stoppable(ctx, githubCallTimeout)
	defer done()
	return dr.d.GitHub.CompareAncestry(compareCtx, dr.token, dr.p.Repository, base, dr.run.SHA)
}

// carryChange is the change from a source's commit to this run's, read the way
// an affected pull request reads its own.
type carryChange struct {
	// identical: the compare listed no path, and the trees agree where both
	// were read.
	identical bool
	// full: the selection covers everything, so nothing carries from the source.
	full   bool
	reason string
	// affected are the packages the change reaches, importers included.
	affected map[string]bool
	// coverage is each packages step's coverage in an affected run, by its
	// manifest key "stage.step" (pipelinePackageStepSelection).
	coverage map[string]pipelines.PackagePolicy
	// buckets are the buckets the change selects (pipelineBucketIncluded).
	buckets map[string]bool
}

func fullChange(reason string) carryChange { return carryChange{full: true, reason: reason} }

// readChange reads the change between source's commit and this run's through
// what readPlan selects an affected run by: the paths analyzed against this
// commit's import graph, select.full, buckets and test inputs, then the pinned
// package-selection decision, the per-step coverage and the bucket decisions,
// all asked in affected mode. So a push carries a slice exactly when a pull
// request making the same change would leave it out. ok is false when the drive
// must stop: a policy that errs is not a policy that said no.
func (dr *runDriver) readChange(ctx context.Context, source Run, files []string) (carryChange, bool) {
	if len(files) == 0 {
		if pipelines.ValidTreeHash(source.TreeHash) && pipelines.ValidTreeHash(dr.run.TreeHash) && source.TreeHash != dr.run.TreeHash {
			// No path listed between two different trees: the compare and the trees
			// disagree, and what disagrees proves nothing.
			return fullChange("the compare listed no change between two different trees"), true
		}
		return carryChange{identical: true}, true
	}
	spec, graph := dr.planSpec, dr.planGraph
	if spec == nil {
		return fullChange("the pipeline block is not at hand"), true
	}
	var fullGlobs, testInputs []string
	if spec.Select != nil {
		fullGlobs, testInputs = spec.Select.Full, spec.Select.TestInputs
	}
	change := carryChange{affected: map[string]bool{}, buckets: map[string]bool{}}
	switch {
	case pipelines.NeedsSelector(spec):
		if graph == nil {
			return fullChange("the import graph is not at hand"), true
		}
		facts, err := pipelines.AnalyzeSelectionWithTestInputs(graph, files, true, fullGlobs, testInputs)
		if err != nil {
			return fullChange(err.Error()), true
		}
		decision, err := dr.selectPackageCoverage(ctx, facts, pipelines.ModeAffected)
		if err != nil {
			dr.abort("selecting what the change since an earlier push reaches", err)
			return carryChange{}, false
		}
		if decision.Full {
			return fullChange(decision.Reason), true
		}
		selection, err := pipelines.ResolveSelection(graph, facts, decision)
		if err != nil {
			// The policy narrowed what the facts say cannot be narrowed. A pull
			// request is refused for it; a push carries nothing and runs.
			return fullChange(err.Error()), true
		}
		if selection.Full {
			return fullChange(selection.Reason), true
		}
		for _, pkg := range selection.Packages {
			change.affected[pkg] = true
		}
		policies, err := dr.selectPackagePolicies(ctx, spec, pipelines.ModeAffected, selection)
		if err != nil {
			dr.abort("selecting each step's coverage of the change since an earlier push", err)
			return carryChange{}, false
		}
		change.coverage = policies
	case pipelines.NeedsBucketSelection(spec):
		facts, err := pipelines.AnalyzeChangedPaths(files, true, fullGlobs)
		if err != nil {
			return fullChange(err.Error()), true
		}
		decision, err := dr.selectPackageCoverage(ctx, facts, pipelines.ModeAffected)
		if err != nil {
			dr.abort("deciding whether the change since an earlier push reaches everything", err)
			return carryChange{}, false
		}
		if decision.Full {
			return fullChange(decision.Reason), true
		}
	}
	if pipelines.NeedsBucketSelection(spec) {
		buckets, err := dr.selectBuckets(ctx, spec, pipelines.ModeAffected, files, true, false)
		if err != nil {
			dr.abort("selecting the buckets the change since an earlier push touches", err)
			return carryChange{}, false
		}
		for _, name := range buckets.Included {
			change.buckets[name] = true
		}
	}
	return change, true
}

// untouched reports whether the change leaves one slice of a step untouched,
// the way an affected run would leave the slice out: every slice when the tree
// did not change; a step gated on a bucket the change did not select, which an
// affected run skips whole; a slice of a step whose coverage the change narrows
// that holds no package the change reaches. A whole-tree step, and a packages
// step whose coverage stays "all", are touched by any change.
func (c carryChange) untouched(declared pipelines.StepSpec, key string, packages []string) bool {
	switch {
	case c.identical:
		return true
	case declared.When != nil && !c.buckets[declared.When.Bucket]:
		return true
	case declared.Packages == "":
		return false
	}
	policy, ok := c.coverage[key]
	if !ok || policy.Coverage != pipelines.PackageCoverageAffected || len(packages) == 0 {
		return false
	}
	for _, pkg := range packages {
		if c.affected[pkg] {
			return false
		}
	}
	return true
}

// carryFromSource carries what source can supply of one step and answers how
// many slices it carried.
//
// IT MAY TAKE THE SOURCE'S SPLIT. A shard's slice follows the timing table, which
// moves, so requiring this plan's slices to equal the source's would mean a
// sharded step seldom carries. When the source split the step over exactly the
// plan's keys and exactly the packages the plan selected (sourceSplit), coverage
// is the same and only the split differs, so the plan takes the source's slices
// -- once, before any slice of the step is carried, and only when that carries
// something -- and every track is fingerprinted again. The slices this push runs
// then cover what the carried ones do not, and the union is the plan's.
//
// EACH SLICE IS JUDGED ALONE, against this source's change: untouched, and
// executed by the source over exactly its packages (carrySourceRow). A slice the
// source did not execute, or one its change reaches, stays undecided for an older
// source, whose change may leave it untouched where a newer one does not.
func (dr *runDriver) carryFromSource(g *carryGroup, source Run, from carriedFrom, change carryChange, rows map[string]WorkStep) int {
	key := pipelines.StepKey(g.tracks[0].step.Stage, g.tracks[0].step.Name)
	var split map[string][]string
	if !g.fixed && !slices.ContainsFunc(g.tracks, func(t *stepTrack) bool { return t.step.Skip != nil }) {
		split, _ = sourceSplit(g.tracks, rows)
	}
	var carry []*stepTrack
	for _, t := range g.tracks {
		if t.step.Skip != nil {
			continue
		}
		packages, fingerprint := t.step.Packages, t.carry
		if split != nil {
			step := t.step
			step.Packages = split[t.step.Key]
			packages, fingerprint = step.Packages, dr.carryFingerprintOf(step)
		}
		row, ok := rows[t.step.Key]
		if change.untouched(g.declared, key, packages) && carrySourceRow(row, ok, t.step, packages, fingerprint) {
			carry = append(carry, t)
		}
	}
	if len(carry) == 0 {
		return 0
	}
	if split != nil {
		for _, t := range g.tracks {
			t.step.Packages = slices.Clone(split[t.step.Key])
			dr.refingerprint(t)
		}
	}
	g.fixed = true
	for _, t := range carry {
		record := from
		record.ChangedPaths = slices.Clone(from.ChangedPaths)
		t.carriedFrom = &record
		t.evidenceRunID = bareID(source.ID)
		t.step.Skip = &pipelines.Skip{Code: pipelines.CodePassedEarlier,
			Reason: "Inputs unchanged since " + shortSHA(source.SHA) + "; verified in run " + bareID(source.ID) + "."}
	}
	return len(carry)
}

// sourceSplit is the source's slices of one step, by key, when the source split
// the step over exactly the plan's keys and exactly the packages the plan
// selected, however split -- the rule reuseGroup and adoptReleaseSlices take a
// split by; ok is false otherwise.
func sourceSplit(group []*stepTrack, rows map[string]WorkStep) (map[string][]string, bool) {
	stage, name := group[0].step.Stage, group[0].step.Name
	keys := make(map[string]bool, len(group))
	var planned, held []string
	for _, t := range group {
		keys[t.step.Key] = true
		planned = append(planned, t.step.Packages...)
	}
	split := map[string][]string{}
	for key, row := range rows {
		if row.Stage != stage || row.Name != name {
			continue
		}
		if !keys[key] {
			return nil, false // the source split it over other keys
		}
		split[key] = row.Packages
		held = append(held, row.Packages...)
	}
	if len(split) != len(keys) || !samePackages(planned, held) {
		return nil, false
	}
	return split, true
}

// carrySourceRow reports a source row a slice may stand on: the step EXECUTED
// there, a done command that named no artifact and no other run's evidence,
// over exactly the slice's packages and under the slice's carry fingerprint. A
// row the source carried or reused is a skip and never done, so no pass is
// carried from a carry, and every slice executes for real at least once within
// carryWindow.
func carrySourceRow(row WorkStep, ok bool, step pipelines.Step, packages []string, fingerprint string) bool {
	return ok && row.Status == WorkStepDone && row.Skip == nil && row.EvidenceRunID == "" && row.CarriedFrom == nil &&
		row.PipelineKind == pipelines.StepCommand && fingerprint != "" && row.CarryFingerprint == fingerprint &&
		slices.Equal(row.Packages, packages) && !releaseProducer(step, row)
}
