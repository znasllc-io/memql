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
	if err := r.retireAttempt(ctx, jobName); err != nil {
		return err
	}
	secretName := SecretName(jobName)
	secret, err := r.kube.SecretMetadata(ctx, secretName)
	if err == nil {
		if secret.Labels[LabelManagedBy] != ManagedBy {
			return fmt.Errorf("pipelinesteps: refusing cleanup of unowned Secret %s", secretName)
		}
		// A POST may have left its creator but not reached admission yet.
		// Absence cannot prove that it will not arrive later. Keep the claim
		// and leave the durable receipt's cleanup pending until reconciled.
		if !jobExists && secret.Annotations[annotCreation] != creationQueued {
			return fmt.Errorf("pipelinesteps: cleanup of %s awaits resolution of Job creation", jobName)
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
