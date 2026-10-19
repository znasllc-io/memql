package pipelinerun

import (
	"context"
	"encoding/hex"
	"errors"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

// A successful Git write does not retire its evidence consumer: the durable
// work receipt does. Conclusion repeats this cleanup after a process loss,
// without reopening a permanently released reference or repeating the write.
func (dr *runDriver) releaseRepositoryChangeEvidence(ctx context.Context) error {
	if dr.run.WorkRunID == "" || dr.d.SARIF == nil || dr.d.SARIF.Library == nil {
		return nil
	}
	ctx = auth.ContextWithUserActor(ctx, dr.run.OwnerUserID)
	rows, err := dr.d.Store.WorkSteps(memql.ContextWithFreshRead(ctx), dr.run.WorkRunID)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.Status != WorkStepDone || row.PipelineKind != pipelines.StepRepositoryChange || rowString(row.Result, "repositoryChangeState") != "applied" {
			continue
		}
		operation := rowString(row.Result, "operationId")
		digest := rowString(row.Result, "sha256")
		for _, value := range []string{operation, digest} {
			decoded, err := hex.DecodeString(value)
			if err != nil || len(decoded) != 32 {
				return errors.New("completed repository change has no exact evidence identity")
			}
		}
		change := pipelines.RepositoryChange{Step: rowString(row.Result, "proposalStep"), Path: rowString(row.Result, "proposalPath")}
		if change.Step == "" || change.Path == "" {
			return errors.New("completed repository change has no producer identity")
		}
		if _, err := repositoryChangeOriginal(ctx, dr.d, dr.run); err != nil {
			return err
		}
		// A new owner retry can confirm an older uncertain write. Release only
		// that same operation's references with the same proposal bytes, across
		// the already-validated lineage. Other proposals retain their evidence.
		current := dr.run
		seen := map[string]bool{}
		for {
			if seen[bareID(current.ID)] || len(seen) >= 32 || !sameID(current.OwnerUserID, dr.run.OwnerUserID) || !sameID(current.PipelineID, dr.run.PipelineID) || current.Repository != dr.run.Repository || current.SHA != dr.run.SHA || current.Event != dr.run.Event || current.HeadBranch != dr.run.HeadBranch || current.InboundRequestID != dr.run.InboundRequestID {
				return errors.New("repository change retry lineage changed during cleanup")
			}
			seen[bareID(current.ID)] = true
			steps, err := dr.d.Store.WorkSteps(memql.ContextWithFreshRead(ctx), current.WorkRunID)
			if err != nil {
				return err
			}
			matched := false
			for _, producer := range steps {
				if producer.Key != change.Step || producer.Status != WorkStepDone {
					continue
				}
				scope, artifact, err := readProposalArtifact(ctx, dr.d, memql.BareShortId(dr.run.OwnerUserID), current, change)
				if err != nil {
					return err
				}
				if artifact.SHA256 != digest {
					continue
				}
				matched = true
				ref := pipelinesteps.RunFileReference{Scope: scope, ReferenceID: "repository-change:" + operation, IntentIDs: []string{artifact.IntentID}}
				if err := dr.d.SARIF.Library.ReleaseRunFileReference(auth.ContextWithInternalOrigin(ctx), ref); err != nil {
					return err
				}
			}
			if sameID(current.ID, dr.run.ID) && !matched {
				return errors.New("completed repository change lost its matching producer evidence")
			}
			if current.RerunOf == "" {
				break
			}
			prior, err := dr.d.Store.RunByID(memql.ContextWithFreshRead(ctx), current.RerunOf)
			if err != nil || prior == nil {
				return errors.New("repository change retry lineage became unavailable")
			}
			current = *prior
		}
	}
	return nil
}
