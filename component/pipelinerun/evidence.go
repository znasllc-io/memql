package pipelinerun

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
)

// reuseVerification is invoked only before a new work journal opens. Recovery
// keeps the saved evidence references and does not ask newer runs for a verdict.
//
// A re-run (rerunOf) asks for nothing here. A whole re-run is someone asking for
// the checks to run again, and a failed-only re-run carries passes inside its own
// chain (carryPassed). A release compares trees (reuseSameTree); every other event
// compares commits, below.
func (dr *runDriver) reuseVerification(ctx context.Context) bool {
	if dr.run.PolicySnapshot == nil || dr.run.Event == pipelines.EventSchedule || dr.run.RerunOf != "" {
		return true
	}
	if dr.run.Event == pipelines.EventRelease {
		return dr.reuseSameTree(ctx)
	}
	eligible := false
	for _, t := range dr.tracks {
		if t.step.Skip == nil && t.verification != "" {
			eligible = true
			break
		}
	}
	if !eligible {
		return true
	}
	runs, err := dr.d.Store.RunsForPipelineSHA(memql.ContextWithFreshRead(ctx), dr.p.ID, dr.run.SHA)
	if err != nil {
		dr.abort("reading verification evidence", err)
		return false
	}
	for _, source := range runs {
		age := dr.d.now().Sub(source.FinishedAt)
		if source.ID == dr.run.ID || source.Conclusion != ConclusionSuccess || !source.Finished() || source.Mode != pipelines.ModeFull || source.Event == pipelines.EventSchedule || source.WorkRunID == "" || source.SHA != dr.run.SHA || source.Repository != dr.run.Repository || !sameID(source.OwnerUserID, dr.run.OwnerUserID) || !sameID(source.PipelineID, dr.run.PipelineID) || age < 0 || age > 24*time.Hour {
			continue
		}
		if source.PolicySnapshot == nil || source.PolicySnapshot["version"] != dr.run.PolicySnapshot["version"] {
			continue
		}
		allowed, ok := dr.evidenceAdmitted(ctx, source, age)
		if !ok {
			return false
		}
		if !allowed {
			continue
		}
		rows, err := dr.d.Store.WorkSteps(memql.ContextWithFreshRead(ctx), source.WorkRunID)
		if err != nil {
			dr.abort("reading verification receipts", err)
			return false
		}
		byKey := map[string]WorkStep{}
		for _, row := range rows {
			byKey[row.Key] = row
		}
		for _, t := range dr.tracks {
			row, ok := byKey[t.step.Key]
			// Only a directly executed successful receipt counts. Do not grow
			// a transitive chain of skips with an ever-renewed freshness window.
			if !ok || t.step.Skip != nil || t.verification == "" || row.Status != WorkStepDone || row.VerificationFingerprint != t.verification || !slices.Equal(row.Packages, t.step.Packages) || len(row.ArtifactFileIDs) > 0 {
				continue
			}
			t.evidenceRunID = source.ID
			t.step.Skip = &pipelines.Skip{Code: pipelines.CodePassedEarlier, Reason: "Verified at this commit in run " + source.ID + "."}
		}
	}
	return true
}

// evidenceAdmitted asks the installed policy about one source. ok is false when
// the drive must stop: a policy that errs is not a policy that said no.
func (dr *runDriver) evidenceAdmitted(ctx context.Context, source Run, age time.Duration) (allowed, ok bool) {
	value, err := dr.runPinnedWorkflow(ctx, "pipelineEvidenceReuse", map[string]any{"event": string(dr.run.Event), "sourceEvent": string(source.Event), "sourceMode": string(source.Mode), "ageSeconds": int64(age / time.Second)})
	if err != nil {
		dr.abort("evaluating verification evidence policy", err)
		return false, false
	}
	allowed, isBool := value.(bool)
	if !isBool {
		dr.abort("evaluating verification evidence policy", fmt.Errorf("evidence policy must return bool"))
		return false, false
	}
	return allowed, true
}

// reuseSameTree plans a release's artifact-free verification steps as skips when
// a successful run of the same pipeline executed them on the same git tree
// (docs/superpowers/specs/2026-10-29-release-same-tree-reuse-design.md).
//
//   - Trigger: a release's first attempt opening its work. A re-run never gets
//     here, so failing a step and asking again runs it.
//   - Evidence: pipelines.SameTreeProblem and pipelines.InspectReusedReleaseReceipt,
//     the two rules candidate coverage proves the same skip with
//     (integrations/release/candidate_reused.go), over the same rows. A skip the
//     proof refused would cost the release its version, because a successful
//     release run is never replaced (adoptableBuildRun), and that is how 0.31.4
//     and 0.31.5 were lost.
//   - Scope: only what declares verification, which is a pinned cluster container
//     with no secrets, no artifacts and no run identity in its environment, so
//     every release build, vulnerability scan and native fleet lane still runs.
//   - Choice: the installed policy still decides, source by source, after the
//     native rules. Mode is the policy's to weigh rather than a native gate: a
//     pull request runs affected, and the fingerprint already binds what the step
//     executed.
//   - Fallback: anything unproven runs the step. A read that does not answer stops
//     the drive, as carryFromOriginal does, because running everything is not what
//     was planned, and recovery tries again.
func (dr *runDriver) reuseSameTree(ctx context.Context) bool {
	groups := stepGroups(dr.tracks, func(t *stepTrack) bool { return t.step.Skip == nil && t.tree != "" })
	if len(groups) == 0 {
		// No step declares verification, or the tree could not be read.
		return true
	}
	fresh := memql.ContextWithFreshRead(ctx)
	sources, err := dr.d.Store.RunsForPipelineTree(fresh, dr.p.ID, dr.run.TreeHash)
	if err != nil {
		dr.abort("reading the runs that tested this tree", err)
		return false
	}
	now := dr.d.now()
	at := pipelines.SameTreeJudgedAt(now, dr.run.StartedAt)
	reuser, reused := sameTreeRunOf(dr.run), 0
	for _, source := range sources {
		if len(groups) == 0 {
			break
		}
		if problem := pipelines.SameTreeProblem(reuser, sameTreeRunOf(source), at); problem != "" {
			dr.log.Debug("pipelines: a run of this tree cannot stand for this release", "source", source.ID, "why", problem)
			continue
		}
		if source.PolicySnapshot == nil || source.PolicySnapshot["version"] != dr.run.PolicySnapshot["version"] {
			continue
		}
		allowed, ok := dr.evidenceAdmitted(ctx, source, max(now.Sub(source.FinishedAt), 0))
		if !ok {
			return false
		}
		if !allowed {
			continue
		}
		work, err := dr.d.Store.WorkRunRow(fresh, source.WorkRunID)
		if err != nil {
			dr.abort("reading the work run of a run that tested this tree", err)
			return false
		}
		rows, err := dr.d.Store.WorkSteps(fresh, source.WorkRunID)
		if err != nil {
			dr.abort("reading the receipts of a run that tested this tree", err)
			return false
		}
		byKey := make(map[string]WorkStep, len(rows))
		for _, row := range rows {
			byKey[row.Key] = row
		}
		remaining := groups[:0]
		for _, group := range groups {
			if dr.reuseGroup(group, source, work, byKey) {
				reused += len(group)
			} else {
				remaining = append(remaining, group)
			}
		}
		groups = remaining
	}
	dr.log.Info("pipelines: a release reuses the receipts of the runs that tested its tree", "tree", dr.run.TreeHash, "steps", reused)
	return true
}

// reuseGroup plans one step, with all its shards, as reused from source, or
// changes nothing and answers false.
//
// IT TAKES THE SOURCE'S SLICES. A shard's slice comes from the timing table, which
// every successful full run moves, so a release's slices rarely equal its
// source's, and requiring them to would mean go-tests and db-tests are almost
// never reused. When the source's shards hold exactly the packages the plan
// selected, coverage is the same and only the split differs. Fingerprints are
// compared under the source's slices, since a fingerprint binds its packages.
//
// EVERY SHARD OR NONE. Reusing half a step and running the rest under another split
// could leave a package no receipt covers.
func (dr *runDriver) reuseGroup(group []*stepTrack, source Run, work map[string]any, rows map[string]WorkStep) bool {
	if work == nil {
		return false
	}
	owner, workRunID, from := bareID(dr.run.OwnerUserID), bareID(source.WorkRunID), sameTreeRunOf(source)
	run := receiptRow(work)
	adopted := make([]pipelines.Step, len(group))
	var planned, held []string
	for i, t := range group {
		row, ok := rows[t.step.Key]
		// Only a directly executed receipt counts: a chain of skips must not
		// renew its own freshness. And what the row reported counts as well as
		// what the step declares, because an artifact stands on no other run.
		if !ok || row.Status != WorkStepDone || releaseProducer(t.step, row) {
			return false
		}
		step := t.step
		step.Packages = slices.Clone(row.Packages)
		if fingerprint := dr.treeFingerprintOf(step); fingerprint == "" || fingerprint != row.TreeFingerprint {
			return false
		}
		if _, err := pipelines.InspectReusedReleaseReceipt(owner, workRunID, t.step.Key, run, receiptRow(row.Raw), from); err != nil {
			dr.log.Info("pipelines: a receipt of this tree could not stand for this release, so its step runs", "source", source.ID, "step", t.step.Key, "why", dr.mask(err.Error()))
			return false
		}
		adopted[i] = step
		planned = append(planned, t.step.Packages...)
		held = append(held, row.Packages...)
	}
	if !samePackages(planned, held) {
		return false
	}
	for i, t := range group {
		t.step = adopted[i]
		dr.refingerprint(t)
		t.evidenceRunID = source.ID
		t.step.Skip = &pipelines.Skip{Code: pipelines.CodePassedEarlier, Reason: "Verified on the same source tree in run " + source.ID + "."}
	}
	return true
}

// adoptReleaseSlices gives a failed-only re-run of a release the slices of the
// attempt it re-runs, for each step whose rows hold exactly the packages the plan
// selected. The plan's split follows today's timing table and the attempt's
// followed an older one, or its source's, so without this a moved table would make
// every shard run again in a release whose shards passed, and the re-run would cost
// what it exists to save. Coverage is unchanged: the union is identical. A failed
// shard runs again over the slice it failed on.
func (dr *runDriver) adoptReleaseSlices(prior []WorkStep) {
	rows := make(map[string]WorkStep, len(prior))
	for _, row := range prior {
		rows[row.Key] = row
	}
	for _, group := range stepGroups(dr.tracks, func(t *stepTrack) bool { return t.step.Skip == nil && t.step.Kind == pipelines.StepCommand }) {
		var planned, held []string
		complete := true
		for _, t := range group {
			row, ok := rows[t.step.Key]
			complete = complete && ok
			planned = append(planned, t.step.Packages...)
			held = append(held, row.Packages...)
		}
		if !complete || !samePackages(planned, held) {
			continue
		}
		for _, t := range group {
			t.step.Packages = slices.Clone(rows[t.step.Key].Packages)
			dr.refingerprint(t)
		}
	}
}

// refingerprint recomputes what a step's call declares after its slice changed.
func (dr *runDriver) refingerprint(t *stepTrack) {
	t.definition, t.verification, t.tree = dr.definitionOf(t.step), dr.verificationOf(t.step), dr.treeFingerprintOf(t.step)
}

// stepGroups is the tracks keep admits, one group per manifest step: its shards
// together, in plan order. Shards share a stage and a name and differ in key.
func stepGroups(tracks []*stepTrack, keep func(*stepTrack) bool) [][]*stepTrack {
	var groups [][]*stepTrack
	at := map[string]int{}
	for _, t := range tracks {
		if !keep(t) {
			continue
		}
		name := t.step.Stage + "." + t.step.Name
		i, seen := at[name]
		if !seen {
			i = len(groups)
			at[name] = i
			groups = append(groups, nil)
		}
		groups[i] = append(groups[i], t)
	}
	return groups
}

// samePackages reports whether two lists hold the same packages, however split.
func samePackages(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(slices.Compact(a), slices.Compact(b))
}

// sameTreeRunOf is a run row as the shared reuse rule reads it. Ids are bare,
// because the proof reads them from rows that spell some of them canonically.
func sameTreeRunOf(r Run) pipelines.SameTreeRun {
	return pipelines.SameTreeRun{
		ID: bareID(r.ID), OwnerUserID: bareID(r.OwnerUserID), PipelineID: bareID(r.PipelineID), Repository: r.Repository, SHA: r.SHA,
		Mode: string(r.Mode), Event: string(r.Event), TreeHash: r.TreeHash, Status: r.Status, Conclusion: r.Conclusion,
		WorkRunID: bareID(r.WorkRunID), Attempt: r.Attempt, CancelRequested: r.CancelRequested, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
	}
}

// receiptRow is a journal row as candidate evidence reads it: its identities bare,
// the rest untouched (integrations/release/candidate_evidence.go, rows).
func receiptRow(row map[string]any) map[string]any {
	out := maps.Clone(row)
	for _, key := range []string{"id", "ownerUserId", "runId"} {
		if value, ok := out[key].(string); ok {
			out[key] = bareID(value)
		}
	}
	return out
}
