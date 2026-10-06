package pipelinesteps

import (
	"context"
	"errors"
	"net/http"
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
// such a Secret once nothing can want it any more: past its stamped run
// deadline, by when every step of its run has ended (a step still queued for a
// slot when its run reaches its ceiling is never started, ruling R31b), and
// the Job TTL past it, within which an outcome may still be read. A Secret
// whose Job exists is never deleted, owned or not.
//
// A lifecycle-owned maintenance loop sweeps without waiting for a build. Each
// batch reads at most reapMaxPages pages and deletes at most reapMaxDeletes
// Secrets. Its continuation and unfinished page survive the batch, so later
// objects are not starved by the first thousand retained Secrets.

const (
	// reapInterval is how often one replica sweeps.
	reapInterval = 10 * time.Minute
	// reapPageSize and reapMaxPages bound what one sweep reads.
	reapPageSize = 100
	reapMaxPages = 10
	// reapMaxDeletes bounds what one sweep deletes.
	reapMaxDeletes = 50
)

// reap handles one bounded batch. more asks maintenance to continue promptly:
// Kubernetes list continuations expire, so they must not wait the full sweep
// interval. A 410 discards the expired snapshot and starts a fresh scan.
func (r *Runner) reap(ctx context.Context) (deleted int, more bool, err error) {
	r.reapMu.Lock()
	defer r.reapMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	now := r.now()
	pages := 0
	for {
		for len(r.reapPending) > 0 {
			if ctx.Err() != nil || deleted >= reapMaxDeletes {
				return deleted, true, nil
			}
			meta := r.reapPending[0]
			r.reapPending = r.reapPending[1:]
			if jobName, ok := orphanedSecret(meta, now, r.cfg.RunCeiling, r.cfg.JobTTL); ok && r.reapSecret(ctx, meta, jobName) {
				deleted++
			}
			if len(r.reapPending) == 0 && r.reapCursor == "" {
				return deleted, false, nil
			}
		}
		if ctx.Err() != nil || pages >= reapMaxPages || deleted >= reapMaxDeletes {
			return deleted, r.reapCursor != "", nil
		}
		call, done := context.WithTimeout(ctx, quickCallTimeout)
		secrets, next, err := r.kube.ManagedSecrets(call, reapPageSize, r.reapCursor)
		done()
		if err != nil {
			var status *deploycontrol.StatusError
			if errors.As(err, &status) && status.Code == http.StatusGone {
				r.reapCursor = ""
			}
			return deleted, true, err
		}
		pages++
		r.reapPending, r.reapCursor = secrets, next
		if len(secrets) == 0 && next == "" {
			return deleted, false, nil
		}
	}
}

// orphanedSecret says a step Secret is old enough to reap and has no owner,
// and names the Job it was made for. A Secret whose name is not a step Job's
// is not one the reaper judges.
func orphanedSecret(meta ObjectMeta, now time.Time, fallbackCeiling, ttl time.Duration) (jobName string, ok bool) {
	jobName, named := strings.CutSuffix(meta.Name, SecretName(""))
	if !named || !isStepJobName(jobName) || len(meta.OwnerReferences) != 0 || meta.CreationTimestamp.IsZero() {
		return jobName, false
	}
	// Old or malformed metadata has a bounded fallback. A valid stamped run
	// deadline is authoritative even when another replica sweeps the Secret.
	expires := meta.CreationTimestamp.Add(fallbackCeiling + ttl)
	if deadline, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(meta.Annotations[AnnotRunDeadline])); err == nil {
		expires = deadline.Add(ttl)
	}
	return jobName, expires.Before(now)
}

// reapSecret deletes an orphaned Secret once its Job is known to be gone.
func (r *Runner) reapSecret(parent context.Context, meta ObjectMeta, jobName string) bool {
	secretName := meta.Name
	ctx, cancel := context.WithTimeout(parent, quickCallTimeout)
	defer cancel()
	switch _, err := r.kube.GetJob(ctx, jobName); {
	case err == nil:
		return false // its Job exists: the Secret is the Job's to take
	case !deploycontrol.IsNotFound(err):
		r.log.Warn("pipelines: an orphaned step Secret's Job could not be read, so the Secret is kept", "secret", secretName, "error", err)
		return false
	}
	if err := r.kube.DeleteObservedSecret(ctx, meta); err != nil {
		r.log.Warn("pipelines: an orphaned step Secret could not be deleted", "secret", secretName, "error", err)
		return false
	}
	r.log.Info("pipelines: deleted an orphaned step Secret, which no Job owned", "secret", secretName)
	return true
}
