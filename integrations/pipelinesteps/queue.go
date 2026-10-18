package pipelinesteps

import "time"

const queueLabel = "kueue.x-k8s.io/queue-name"

// Memory and disk are hard reservations, including native service sidecars
// and artifact collectors. CPU remains burstable inside the dedicated pool.
// LimitRange defaults must reserve the same memory/disk as their limits.
func reserveContainerLimits(containers []Container) {
	for n := range containers {
		r := containers[n].Resources
		if r == nil {
			continue
		}
		if r.Requests == nil {
			r.Requests = map[string]string{}
		}
		for _, name := range []string{"memory", "ephemeral-storage"} {
			if limit := r.Limits[name]; limit != "" {
				r.Requests[name] = limit
			}
		}
	}
}

// A queued Job may exist for longer than its execution allowance. The Job
// controller sets startTime only on admission; every replica reads that same
// fact. Existing non-queued Jobs retain their original creation-time bound.
func executionStart(job Job) time.Time {
	if job.Metadata.Labels[queueLabel] != "" {
		return job.Status.StartTime
	}
	return job.Metadata.CreationTimestamp
}
