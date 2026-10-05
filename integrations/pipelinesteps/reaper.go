package pipelinesteps

import (
	"context"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/deploycontrol"
)

// reaper.go -- the orphan-Secret reaper (epic memql#5478, #5493; Task 6 fix
// round 1).
//
// A step's Secret holds its clone token and its resolved secrets, and goes
// with its Job once the Job owns it (OwnSecret): the ack, the TTL and a cancel
// all collect it. One that never came to be owned has nothing to collect it --
// its Run went between creating the Secret and creating the Job, or before it
// could give the Job ownership, and a cancel missed it. The reaper deletes
// such a Secret once nothing can want it any more: older than the run's
// ceiling, by when every step of its run has ended (a step still queued for a
// slot when its run reaches its ceiling is never started, ruling R31b), and
// the Job TTL past it, within which an outcome may still be read. A Secret
// whose Job exists is never deleted, owned or not.
//
// Each replica sweeps at most once every reapInterval, piggybacked on Run --
// any replica's sweep reaps every replica's orphans -- and a sweep is bounded:
// at most reapMaxPages pages of reapPageSize Secrets read, at most
// reapMaxDeletes deleted. What it leaves, the next sweep reaches.

const (
	// reapInterval is how often one replica sweeps.
	reapInterval = 10 * time.Minute
	// reapPageSize and reapMaxPages bound what one sweep reads.
	reapPageSize = 100
	reapMaxPages = 10
	// reapMaxDeletes bounds what one sweep deletes.
	reapMaxDeletes = 50
)

// maybeReap starts a sweep in the background unless one ran on this replica
// within reapEvery, or is running.
func (r *Runner) maybeReap() {
	r.mu.Lock()
	if r.reapEvery <= 0 || r.reaping || (!r.lastReap.IsZero() && r.now().Sub(r.lastReap) < r.reapEvery) {
		r.mu.Unlock()
		return
	}
	r.reaping, r.lastReap = true, r.now()
	r.mu.Unlock()
	go func() {
		deleted := r.reap()
		r.mu.Lock()
		r.reaping = false
		done := r.onReaped
		r.mu.Unlock()
		if done != nil {
			done(deleted)
		}
	}()
}

// reap deletes the orphaned step Secrets it finds, within the sweep's bounds,
// and answers how many.
func (r *Runner) reap() (deleted int) {
	cutoff := r.now().Add(-(r.cfg.RunCeiling + r.cfg.JobTTL))
	cont := ""
	for page := 0; page < reapMaxPages && deleted < reapMaxDeletes; page++ {
		ctx, cancel := context.WithTimeout(context.Background(), quickCallTimeout)
		secrets, next, err := r.kube.ManagedSecrets(ctx, reapPageSize, cont)
		cancel()
		if err != nil {
			r.log.Warn("pipelines: the step Secrets could not be listed for the orphan sweep", "error", err)
			return deleted
		}
		for _, meta := range secrets {
			if deleted >= reapMaxDeletes {
				break
			}
			if jobName, ok := orphanedSecret(meta, cutoff); ok && r.reapSecret(meta.Name, jobName) {
				deleted++
			}
		}
		if next == "" {
			break
		}
		cont = next
	}
	return deleted
}

// orphanedSecret says a step Secret is old enough to reap and has no owner,
// and names the Job it was made for. A Secret whose name is not a step Job's
// is not one the reaper judges.
func orphanedSecret(meta ObjectMeta, cutoff time.Time) (jobName string, ok bool) {
	jobName, named := strings.CutSuffix(meta.Name, SecretName(""))
	return jobName, named && isStepJobName(jobName) && len(meta.OwnerReferences) == 0 &&
		!meta.CreationTimestamp.IsZero() && meta.CreationTimestamp.Before(cutoff)
}

// reapSecret deletes an orphaned Secret once its Job is known to be gone.
func (r *Runner) reapSecret(secretName, jobName string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), quickCallTimeout)
	defer cancel()
	switch _, err := r.kube.GetJob(ctx, jobName); {
	case err == nil:
		return false // its Job exists: the Secret is the Job's to take
	case !deploycontrol.IsNotFound(err):
		r.log.Warn("pipelines: an orphaned step Secret's Job could not be read, so the Secret is kept", "secret", secretName, "error", err)
		return false
	}
	if err := r.kube.DeleteSecret(ctx, secretName); err != nil {
		r.log.Warn("pipelines: an orphaned step Secret could not be deleted", "secret", secretName, "error", err)
		return false
	}
	r.log.Info("pipelines: deleted an orphaned step Secret, which no Job owned", "secret", secretName)
	return true
}
