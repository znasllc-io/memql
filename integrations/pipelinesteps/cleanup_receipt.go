package pipelinesteps

import (
	"context"
	"fmt"
	"time"

	"github.com/znasllc-io/memql/component/deploycontrol"
)

// A replacement driver repeats this using the committed receipt's deterministic
// identity. Never delete a namesake that has lost the runner's ownership label.
// UID and revision preconditions guard against replacement after the read.
func (r *Runner) cleanupReceipt(ctx context.Context, jobName string) error {
	job, err := r.kube.GetJob(ctx, jobName)
	jobExists := err == nil
	if err == nil {
		if job.Metadata.Labels[LabelManagedBy] != ManagedBy {
			return fmt.Errorf("pipelinesteps: refusing cleanup of unowned Job %s", jobName)
		}
	} else if !deploycontrol.IsNotFound(err) {
		return err
	}
	marker, err := r.retireAttempt(ctx, jobName)
	if err != nil {
		return err
	}
	secretName := SecretName(jobName)
	secret, err := r.kube.SecretMetadata(ctx, secretName)
	if err == nil {
		if secret.Labels[LabelManagedBy] != ManagedBy {
			return fmt.Errorf("pipelinesteps: refusing cleanup of unowned Secret %s", secretName)
		}
		// A POST may have left its creator but not reached admission yet, and
		// absence cannot prove that it will not arrive later, so while one
		// still could (latePostWindow) the claim stays and the durable
		// receipt's cleanup stays pending.
		//
		// After that the claim is spent, and only this code can say so. The one
		// creator who could release it has no answer to go on (its context ended
		// with the POST unanswered, or it is gone), so waiting for a resolution
		// has no end: a driver that cannot confirm this keeps the run unfinished,
		// and recovery asks again for ever (run 131819ff, 2026-10-29, in_progress
		// with every step terminal). The Secret goes, since it holds a clone
		// token that nothing will use. The marker stays, and refuses any later
		// creation, so the claim is never replayed.
		if !jobExists && secret.Annotations[annotCreation] != creationQueued {
			if r.latePostPossible(marker) {
				return fmt.Errorf("pipelinesteps: cleanup of %s waits until %s: a Job POST sent before the attempt was stopped may still land",
					jobName, marker.CreationTimestamp.Add(latePostWindow).UTC().Format(time.RFC3339))
			}
			r.log.Info("pipelines: a stopped attempt's Job creation can no longer land, so its Secret goes with no Job",
				"jobName", jobName, "stoppedAt", marker.CreationTimestamp, "claim", secret.Annotations[annotCreation])
		}
		if err := r.kube.DeleteObservedSecret(ctx, secret); err != nil {
			return err
		}
	} else if !deploycontrol.IsNotFound(err) {
		return err
	}
	if jobExists {
		if err := r.kube.DeleteObservedJob(ctx, job.Metadata); err != nil {
			return err
		}
	}
	for {
		gone, err := r.receiptResourcesAbsent(ctx, jobName)
		if err != nil {
			return err
		}
		if gone {
			return nil
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("pipelinesteps: cleanup of %s remains unconfirmed: %w", jobName, ctx.Err())
		case <-timer.C:
		}
	}
}

func (r *Runner) receiptResourcesAbsent(ctx context.Context, jobName string) (bool, error) {
	_, jobErr := r.kube.GetJob(ctx, jobName)
	if jobErr != nil && !deploycontrol.IsNotFound(jobErr) {
		return false, jobErr
	}
	_, secretErr := r.kube.SecretMetadata(ctx, SecretName(jobName))
	if secretErr != nil && !deploycontrol.IsNotFound(secretErr) {
		return false, secretErr
	}
	pods, err := r.kube.JobPods(ctx, jobName)
	if err != nil {
		return false, err
	}
	return deploycontrol.IsNotFound(jobErr) && deploycontrol.IsNotFound(secretErr) && len(pods) == 0, nil
}
