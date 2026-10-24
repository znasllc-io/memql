package pipelinesteps

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
)

const queueLabel = "kueue.x-k8s.io/queue-name"
const queuePriorityLabel = "kueue.x-k8s.io/priority-class"

// Priorities affect admission order only. The installed ClusterQueue still
// owns quota and preemption policy; no Pod scheduling priority is introduced.
func queuePriorityClass(cfg Config, run StepRun) (string, error) {
	raw := cfg.QueuePriorityClasses
	if raw == "" {
		return "", nil
	}
	if cfg.QueueName == "" || len(raw) > 4096 {
		return "", errors.New("priority classes require a queue and bounded configuration")
	}
	validEvent := func(event string) bool {
		switch event {
		case "pull_request", "merge_group", "push", "release", "schedule", "manual":
			return true
		}
		return false
	}
	if !validEvent(string(run.Event)) {
		return "", errors.New("priority selection requires the trusted pipeline event")
	}
	d := json.NewDecoder(strings.NewReader(raw))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return "", errors.New("priority classes must be an event-to-class object")
	}
	classes := map[string]string{}
	for d.More() {
		tok, err = d.Token()
		event, ok := tok.(string)
		if err != nil || !ok || !validEvent(event) || classes[event] != "" {
			return "", errors.New("unknown or repeated priority event")
		}
		var class string
		if err := d.Decode(&class); err != nil || len(class) > 63 || !dnsLabelShape.MatchString(class) {
			return "", errors.New("priority classes must name installed DNS labels")
		}
		classes[event] = class
	}
	if tok, err = d.Token(); err != nil || tok != json.Delim('}') || d.Decode(new(any)) != io.EOF {
		return "", errors.New("priority classes contain trailing data")
	}
	return classes[string(run.Event)], nil
}

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
