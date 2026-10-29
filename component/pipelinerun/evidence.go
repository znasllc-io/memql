package pipelinerun

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
)

// reuseVerification is invoked only before a new work journal opens. Recovery
// keeps the saved evidence references and does not ask newer runs for a verdict.
//
// A release run never reuses, and the installed policy is not asked for it.
// Candidate preparation (releasePrepareCandidateWorkflow) accepts only receipts
// the release run executed itself, and a release request cannot adopt a re-run,
// so one carried receipt makes the version unreleasable. Reuse saves a release
// only its artifact-free checks; executing them is the cheaper failure.
func (dr *runDriver) reuseVerification(ctx context.Context) bool {
	if dr.run.PolicySnapshot == nil || dr.run.Event == pipelines.EventSchedule || dr.run.Event == pipelines.EventRelease || dr.run.RerunOf != "" {
		return true
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
		value, err := dr.runPinnedWorkflow(ctx, "pipelineEvidenceReuse", map[string]any{"event": string(dr.run.Event), "sourceEvent": string(source.Event), "sourceMode": string(source.Mode), "ageSeconds": int64(age / time.Second)})
		if err != nil {
			dr.abort("evaluating verification evidence policy", err)
			return false
		}
		allowed, ok := value.(bool)
		if !ok {
			dr.abort("evaluating verification evidence policy", fmt.Errorf("evidence policy must return bool"))
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
