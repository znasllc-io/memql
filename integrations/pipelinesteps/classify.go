package pipelinesteps

import (
	"fmt"
	"strings"
	"time"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

// Phase is where a step's Job stands, in the terms the runner's poll loop acts
// on.
type Phase int

const (
	// PhasePending: nothing has decided the step yet. Its pod is not created
	// or not scheduled, an image is being pulled, the owner's cache, the clone
	// or a service is getting ready. Poll again.
	PhasePending Phase = iota
	// PhaseRunning: the step container runs. Follow its log.
	PhaseRunning
	// PhaseSucceeded: the command exited 0. Terminal.
	PhaseSucceeded
	// PhaseFailed: terminal. Failure says why, except when the command itself
	// exited non-zero: then the exit code is the whole answer.
	PhaseFailed
)

// Observation is what Classify reads off a step's Job and pod.
type Observation struct {
	Phase Phase
	// ExitCode is the command's exit status when it ended on its own, else -1.
	// A step that never started has none, and neither does one its deadline or
	// the cluster stopped: whatever status the kill left is not the command's
	// answer.
	ExitCode int
	// Failure is the typed reason a step failed for something other than its
	// command: a code from the seam's catalogue and the sentence the check run
	// shows. It is nil for a command that exited non-zero.
	Failure *pl.Failure
	// Detail is one sentence for the step's log: what the step is doing or
	// waiting on, or how it ended.
	Detail string
}

// pullReasons are the waiting reasons of a container whose image cannot be
// pulled.
var pullReasons = map[string]bool{"ErrImagePull": true, "ImagePullBackOff": true, "InvalidImageName": true}

// Classify reads a step's Job and its newest pod (nil when there is none) and
// says where the step stands. created is when the runner created or adopted
// the Job; with now and cfg.ScheduleTimeout it bounds how long the pod may
// wait on the cluster -- to be scheduled, or for its cache volume. deadlineCode
// is the StepRun's: pl.CodeStepTimeout or pl.CodeRunCeiling, whichever bound
// the Job's deadline is; empty reads as pl.CodeStepTimeout.
//
// It is pure -- no I/O, no clock -- and the runner calls it on every read. The
// order below is the classification, each rule taken only when none before it
// applied:
//
//  1. The cluster is stopping the pod -- DisruptionTarget=True: a node drain,
//     a preemption, an eviction -- the cluster lost the step (pl.CodeNodeLost,
//     ruling R24), unless the command ended before the disruption began, when
//     its own answer stands.
//  2. The Job is past its deadline: deadlineCode -- unless the command ended
//     before the deadline, when its own answer stands.
//  3. The step container ended: exit 0 succeeded; any other exit failed, with
//     no code, because the exit code is the answer.
//  4. The Job completed: succeeded.
//  5. Something in the pod means the step can never run: an image that
//     cannot be pulled (pl.CodeImagePullFailed), a container that cannot be
//     created or the owner's cache directory (pl.CodeJobRejected), the clone
//     (pl.CodeCloneFailed), a service that stopped before the step started
//     (pl.CodeServiceFailed), a pod nobody can schedule
//     (pl.CodeJobUnschedulable).
//  6. The step container runs: running.
//  7. The Job or its pod failed and nothing above says why -- rejected by the
//     kubelet, deleted, gone with its node: the cluster lost the step
//     (pl.CodeNodeLost).
//  8. Otherwise pending.
//
// Most of these rules exist because of what an API server was measured to
// report (k3s v1.32, kept in classify_test.go's fixtures): a sidecar is
// restarted whatever the pod's restart policy, so a failed service never
// fails the pod and has to be read from its restart history; the kubelet
// stops every service once the step ends, so a stopped service is only a
// failure before the step starts; a step can end inside its deadline and its
// Job still fail DeadlineExceeded while the services stop; the node's own
// pull throttle reports ErrImagePull for an image that pulls on the next try;
// and an evicted step is killed (137) and its services stopped after the pod
// is marked, which read alone would be the command failing or a service
// failing.
func Classify(job Job, pod *Pod, created, now time.Time, cfg Config, deadlineCode string) Observation {
	step := podContainer(pod, false, ContainerStep)
	var stepEnd *ContainerStateTerminated
	if step != nil {
		stepEnd = step.State.Terminated
	}

	if d := disruption(pod); d != nil && !endedBefore(stepEnd, d.LastTransitionTime) {
		return failed(pl.CodeNodeLost, "the cluster stopped the step's pod ("+reasonAnd(d.Reason, d.Message)+
			"): a node drain, a preemption or an eviction, not the step's command")
	}
	if jobCondition(job, "DeadlineExceeded", "Failed", "FailureTarget") != nil && !endedBefore(stepEnd, jobDeadline(job)) {
		code := deadlineCode
		if strings.TrimSpace(code) == "" {
			code = pl.CodeStepTimeout
		}
		return failed(code, deadlineSentence(job))
	}
	if stepEnd != nil {
		return stepEnded(stepEnd)
	}
	if jobCondition(job, "", "Complete") != nil {
		return Observation{Phase: PhaseSucceeded, ExitCode: 0, Detail: "the step's Job completed"}
	}
	if pod != nil {
		if f := podFailure(pod, step, created, now, cfg); f != nil {
			return failed(f.Code, f.Message)
		}
		if step != nil && step.State.Running != nil {
			return Observation{Phase: PhaseRunning, ExitCode: -1, Detail: "the step is running"}
		}
	}
	if f := unexplainedFailure(job, pod); f != nil {
		return failed(f.Code, f.Message)
	}
	return Observation{Phase: PhasePending, ExitCode: -1, Detail: pendingDetail(pod)}
}

func failed(code, message string) Observation {
	return Observation{Phase: PhaseFailed, ExitCode: -1, Failure: &pl.Failure{Code: code, Message: message}, Detail: message}
}

// stepEnded is the command's own answer. It never repeats the step
// container's termination message: that is whatever the command wrote to
// /dev/termination-log, which can be a secret it was given, and Detail is
// logged unmasked. The reason is the kubelet's (OOMKilled, StartError).
func stepEnded(t *ContainerStateTerminated) Observation {
	detail := "the step " + exitPhrase(t)
	if t.ExitCode == 0 {
		return Observation{Phase: PhaseSucceeded, ExitCode: 0, Detail: detail}
	}
	return Observation{Phase: PhaseFailed, ExitCode: int(t.ExitCode), Detail: detail}
}

// jobDeadline is the instant the Job controller enforces the deadline at,
// status.startTime plus activeDeadlineSeconds -- whole seconds, as the
// controller itself compares them -- or zero when either is unknown.
func jobDeadline(job Job) time.Time {
	if job.Status.StartTime.IsZero() || job.Spec.ActiveDeadlineSeconds == nil {
		return time.Time{}
	}
	return job.Status.StartTime.Add(time.Duration(*job.Spec.ActiveDeadlineSeconds) * time.Second)
}

// endedBefore says the step container ended strictly before an instant -- the
// deadline, or the moment the cluster began stopping the pod: on its own, not
// because it was stopped. A step that is stopped ends at or after the instant,
// even one that traps the signal and exits 0 (measured for both: the Job
// controller and the eviction mark the Job or the pod before anything is
// killed). Unknown times are not before.
func endedBefore(t *ContainerStateTerminated, instant time.Time) bool {
	return t != nil && !instant.IsZero() && !t.FinishedAt.IsZero() && t.FinishedAt.Before(instant)
}

// disruption is the pod's DisruptionTarget condition when it is True: the
// cluster is stopping the pod for its own reasons (EvictionByEvictionAPI for a
// drain, PreemptionByScheduler, TerminationByKubelet under node pressure,
// DeletionByTaintManager, DeletionByPodGC) and has said so before killing
// anything. The Job controller's deadline is not one of them: a pod it deletes
// carries no such condition (measured).
func disruption(pod *Pod) *PodCondition {
	if pod == nil {
		return nil
	}
	if c := podCondition(pod, "DisruptionTarget"); c != nil && c.Status == "True" {
		return c
	}
	return nil
}

func deadlineSentence(job Job) string {
	if ads := job.Spec.ActiveDeadlineSeconds; ads != nil {
		return fmt.Sprintf("the step did not finish within its deadline of %s", time.Duration(*ads)*time.Second)
	}
	return "the step did not finish within its deadline"
}

// podFailure is the first reason, in rule 4's order, that the pod can never
// run the step, or nil.
func podFailure(pod *Pod, step *ContainerStatus, created, now time.Time, cfg Config) *pl.Failure {
	all := make([]ContainerStatus, 0, len(pod.Status.InitContainerStatuses)+len(pod.Status.ContainerStatuses))
	all = append(append(all, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)

	for _, cs := range all {
		if w := cs.State.Waiting; w != nil && pullReasons[w.Reason] && !pullThrottled(w) {
			return &pl.Failure{Code: pl.CodeImagePullFailed, Message: fmt.Sprintf("%s cannot pull %s: %s", role(cs.Name), imageOf(cs), reasonAnd(w.Reason, w.Message))}
		}
	}
	for _, cs := range all {
		if w := cs.State.Waiting; w != nil && w.Reason == "CreateContainerConfigError" {
			return &pl.Failure{Code: pl.CodeJobRejected, Message: role(cs.Name) + " cannot be created: " + reasonAnd(w.Reason, w.Message)}
		}
	}
	if f := cachePrepFailure(podContainer(pod, true, ContainerCachePrep), created, now, cfg); f != nil {
		return f
	}
	if clone := podContainer(pod, true, ContainerClone); clone != nil {
		if t := clone.State.Terminated; t != nil && t.ExitCode != 0 {
			return &pl.Failure{Code: pl.CodeCloneFailed, Message: "the clone " + exitPhrase(t) + messageOf(t)}
		}
	}
	if step == nil || step.State.Running == nil {
		for _, cs := range pod.Status.InitContainerStatuses {
			if strings.HasPrefix(cs.Name, ServicePrefix) {
				if f := serviceFailure(cs); f != nil {
					return f
				}
			}
		}
	}
	if c := podCondition(pod, "PodScheduled"); c != nil && c.Status == "False" && c.Reason == "Unschedulable" && now.Sub(created) > cfg.ScheduleTimeout {
		return &pl.Failure{Code: pl.CodeJobUnschedulable, Message: fmt.Sprintf("the step's pod could not be scheduled within %s: %s", cfg.ScheduleTimeout, oneLine(c.Message))}
	}
	return nil
}

// cachePrepFailure: the owner's cache directory is a cluster and storage
// matter, never the repository's, so every way it fails is a rejected Job.
// cache-prep exits non-zero when it cannot make the directory; it waits on a
// reason other than starting when its container cannot be made; and it never
// starts at all when the cache volume cannot be mounted -- measured, every
// container then waits PodInitializing and only the pod's events name the
// mount error -- which only the time it has waited tells apart from a slow
// start.
func cachePrepFailure(cs *ContainerStatus, created, now time.Time, cfg Config) *pl.Failure {
	if cs == nil {
		return nil
	}
	const prefix = "the owner's cache directory could not be prepared: "
	switch s := cs.State; {
	case s.Terminated != nil && s.Terminated.ExitCode != 0:
		return &pl.Failure{Code: pl.CodeJobRejected, Message: prefix + "cache-prep " + exitPhrase(s.Terminated) + messageOf(s.Terminated)}
	case s.Waiting != nil && !waitingToStart(s.Waiting):
		return &pl.Failure{Code: pl.CodeJobRejected, Message: prefix + "cache-prep cannot start: " + reasonAnd(s.Waiting.Reason, s.Waiting.Message)}
	case s.Waiting != nil && now.Sub(created) > cfg.ScheduleTimeout:
		return &pl.Failure{Code: pl.CodeJobRejected, Message: fmt.Sprintf("%scache-prep has not started within %s, which is how a cache volume that cannot be mounted looks (the pod's events name the mount error)", prefix, cfg.ScheduleTimeout)}
	}
	return nil
}

// serviceFailure: a service that has stopped at least once before the step
// started has failed. Its sidecar is restarted whatever the pod's restart
// policy, so it never fails the pod and the step would wait for it until the
// deadline; and between restarts it can read running again, with only its
// last state and restart count saying it stopped (measured, both for a
// service that exits and for one its ready check kept failing).
func serviceFailure(cs ContainerStatus) *pl.Failure {
	t := cs.State.Terminated
	if t == nil {
		t = cs.LastState.Terminated
	}
	if t == nil && cs.RestartCount == 0 {
		return nil
	}
	msg := role(cs.Name) + " stopped before the step started"
	if t != nil {
		msg += ": it " + exitPhrase(t) + messageOf(t)
	}
	switch {
	case cs.RestartCount == 1:
		msg += " (restarted once)"
	case cs.RestartCount > 1:
		msg += fmt.Sprintf(" (restarted %d times)", cs.RestartCount)
	}
	return &pl.Failure{Code: pl.CodeServiceFailed, Message: msg}
}

// unexplainedFailure: the Job or its pod failed and no rule before this one
// says why -- the pod was rejected by the kubelet, deleted, or lost with its
// node, or it was evicted and is already gone, taking its DisruptionTarget
// with it. Read as pending, the runner would wait for an answer that cannot
// come.
func unexplainedFailure(job Job, pod *Pod) *pl.Failure {
	if pod != nil && pod.Status.Phase == "Failed" {
		msg := "the step's pod failed without any of its containers saying why"
		if pod.Status.Reason != "" {
			msg = "the step's pod failed: " + reasonAnd(pod.Status.Reason, pod.Status.Message)
		}
		return &pl.Failure{Code: pl.CodeNodeLost, Message: msg}
	}
	if c := jobCondition(job, "", "Failed", "FailureTarget"); c != nil {
		return &pl.Failure{Code: pl.CodeNodeLost, Message: "the step's Job failed (" + reasonAnd(c.Reason, c.Message) + ") and its pod does not say why"}
	}
	return nil
}

// pendingDetail says what an undecided step is waiting on.
func pendingDetail(pod *Pod) string {
	if pod == nil {
		return "waiting for the Job to create the step's pod"
	}
	if c := podCondition(pod, "PodScheduled"); c != nil && c.Status == "False" {
		return "waiting to be scheduled: " + reasonAnd(c.Reason, c.Message)
	}
	serviceUp := false
	for _, list := range [][]ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, cs := range list {
			switch s := cs.State; {
			case s.Waiting != nil && s.Waiting.Reason != "" && s.Waiting.Reason != "PodInitializing":
				return role(cs.Name) + " is waiting: " + reasonAnd(s.Waiting.Reason, s.Waiting.Message)
			case s.Running != nil && strings.HasPrefix(cs.Name, ServicePrefix):
				// Running is not ready: the step starts once its startup probe
				// passes. Keep looking for anything more specific.
				serviceUp = true
			case s.Running != nil:
				return role(cs.Name) + " is running"
			}
		}
	}
	if serviceUp {
		return "waiting for the services to be ready"
	}
	return "waiting for the step's pod to start its containers"
}

// waitingToStart is a container on its way to running: being created, waiting
// for the containers before it, or waiting out the node's own pull throttle.
func waitingToStart(w *ContainerStateWaiting) bool {
	switch w.Reason {
	case "", "PodInitializing", "ContainerCreating":
		return true
	}
	return pullReasons[w.Reason] && pullThrottled(w)
}

// pullThrottled is the kubelet refusing to START a pull because its own pull
// rate limit (registryPullQPS) is spent: no registry refused anything, and the
// next attempt, after the back-off, pulls. The back-off after it repeats the
// cause ("Back-off pulling image ...: ErrImagePull: pull QPS exceeded").
func pullThrottled(w *ContainerStateWaiting) bool {
	return strings.Contains(w.Message, "pull QPS exceeded")
}

// role names a container of the step's pod as a person reads it.
func role(name string) string {
	switch {
	case name == ContainerStep:
		return "the step"
	case name == ContainerClone:
		return "the clone"
	case name == ContainerCachePrep:
		return "cache-prep"
	case strings.HasPrefix(name, ServicePrefix):
		return "service " + strings.TrimPrefix(name, ServicePrefix)
	}
	return "container " + name
}

// imageOf names the image a container's status reports, rather than leaving
// it to the runtime's message to mention.
func imageOf(cs ContainerStatus) string {
	if cs.Image == "" {
		return "its image"
	}
	return fmt.Sprintf("image %q", cs.Image)
}

// exitPhrase is "exited N", with the kubelet's reason when it says more than
// that the container failed or finished.
func exitPhrase(t *ContainerStateTerminated) string {
	s := fmt.Sprintf("exited %d", t.ExitCode)
	if t.Reason != "" && t.Reason != "Error" && t.Reason != "Completed" {
		s += " (" + t.Reason + ")"
	}
	return s
}

// messageOf is a container's own last words, when it left any: its
// termination message, on one line. It is read for the clone, cache-prep and
// the services -- platform code, and images given no secrets -- and never for
// the step container (stepEnded says why).
func messageOf(t *ContainerStateTerminated) string {
	if m := oneLine(t.Message); m != "" {
		return ": " + m
	}
	return ""
}

func reasonAnd(reason, message string) string {
	if m := oneLine(message); m != "" {
		return reason + ": " + m
	}
	return reason
}

// oneLine collapses a message's whitespace, newlines included, so it reads as
// one sentence on a check run and as one line in a log.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// jobCondition is the Job's first true condition of one of the types, and
// with the reason when one is given, or nil.
func jobCondition(job Job, reason string, types ...string) *JobCondition {
	for i, c := range job.Status.Conditions {
		if c.Status != "True" || (reason != "" && c.Reason != reason) {
			continue
		}
		for _, t := range types {
			if c.Type == t {
				return &job.Status.Conditions[i]
			}
		}
	}
	return nil
}

func podCondition(pod *Pod, typ string) *PodCondition {
	for i, c := range pod.Status.Conditions {
		if c.Type == typ {
			return &pod.Status.Conditions[i]
		}
	}
	return nil
}

// podContainer is the named container's status, among the init containers or
// the main ones, or nil.
func podContainer(pod *Pod, init bool, name string) *ContainerStatus {
	if pod == nil {
		return nil
	}
	list := pod.Status.ContainerStatuses
	if init {
		list = pod.Status.InitContainerStatuses
	}
	for i := range list {
		if list[i].Name == name {
			return &list[i]
		}
	}
	return nil
}
