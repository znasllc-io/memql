package pipelinesteps

import (
	"strings"
	"testing"
	"time"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

// Every fixture here is a Job or pod status a k3s v1.32.13 API server returned
// for the situation its row names (a throwaway k3d cluster, 2026-10-04), cut
// down to the fields the classifier reads: the reasons, messages, exit codes,
// restart counts and timestamps are the cluster's own. Where a row exists
// because a measurement contradicted the obvious reading, its comment says
// what was measured.

var (
	// clsStart is the Job's status.startTime; the runner created it then too.
	clsStart   = time.Date(2026, 10, 4, 7, 34, 51, 0, time.UTC)
	clsCreated = clsStart
	// clsNow is half a minute later: inside every patience the config grants.
	clsNow = clsStart.Add(30 * time.Second)
)

// clsConfig is the one setting the classifier reads.
func clsConfig() Config { return Config{ScheduleTimeout: 10 * time.Minute} }

// clsJob is the step's Job: started at clsStart, a deadline of ads seconds,
// and the given conditions.
func clsJob(ads int64, conds ...JobCondition) Job {
	return Job{
		Metadata: ObjectMeta{Name: "mp-a7a72726d5075767e0b6d115"},
		Spec:     JobSpec{ActiveDeadlineSeconds: ptrTo(ads)},
		Status:   JobStatus{StartTime: clsStart, Conditions: conds},
	}
}

func clsCond(typ, reason, message string) JobCondition {
	return JobCondition{Type: typ, Status: "True", Reason: reason, Message: message}
}

// clsPod is the step's pod, scheduled, with its init containers (cache-prep,
// clone, the services) and the step container.
func clsPod(phase string, init []ContainerStatus, step ContainerStatus) *Pod {
	return &Pod{
		Metadata: ObjectMeta{Name: "mp-a7a72726d5075767e0b6d115-x7kk6"},
		Status: PodStatus{
			Phase:                 phase,
			Conditions:            []PodCondition{{Type: "PodScheduled", Status: "True"}},
			InitContainerStatuses: init,
			ContainerStatuses:     []ContainerStatus{step},
		},
	}
}

func clsWaiting(name, image, reason, message string) ContainerStatus {
	return ContainerStatus{Name: name, Image: image, State: ContainerState{
		Waiting: &ContainerStateWaiting{Reason: reason, Message: message},
	}}
}

func clsRunning(name string) ContainerStatus {
	return ContainerStatus{Name: name, Image: "docker.io/library/alpine:3.20", State: ContainerState{
		Running: &ContainerStateRunning{StartedAt: clsStart.Add(3 * time.Second)},
	}}
}

func clsTerminated(name string, exit int32, reason, message string, finished time.Time) ContainerStatus {
	return ContainerStatus{Name: name, Image: "docker.io/library/alpine:3.20", State: ContainerState{
		Terminated: &ContainerStateTerminated{
			ExitCode: exit, Reason: reason, Message: message,
			StartedAt: clsStart.Add(time.Second), FinishedAt: finished,
		},
	}}
}

// Container states that recur: a clone that finished, and a container waiting
// for the init containers before it.
var (
	clsCloneDone = clsTerminated("clone", 0, "Completed", "", clsStart.Add(time.Second))
	clsStepInit  = clsWaiting("step", "alpine:3.20", "PodInitializing", "")
	clsSvcInit   = clsWaiting("svc-db", "alpine:3.20", "PodInitializing", "")
)

type clsWant struct {
	phase    Phase
	exitCode int
	code     string   // the Failure's code; "" means no Failure at all
	mentions []string // each appears in the Failure's message, or in Detail when there is no Failure
}

type clsCase struct {
	name         string
	job          Job
	pod          *Pod
	created      time.Time // zero means clsCreated
	now          time.Time // zero means clsNow
	deadlineCode string
	want         clsWant
}

var clsPhaseNames = map[Phase]string{
	PhasePending: "pending", PhaseRunning: "running", PhaseSucceeded: "succeeded", PhaseFailed: "failed",
}

func runClassifyCase(t *testing.T, c clsCase) {
	t.Helper()
	created, now := c.created, c.now
	if created.IsZero() {
		created = clsCreated
	}
	if now.IsZero() {
		now = clsNow
	}
	got := Classify(c.job, c.pod, created, now, clsConfig(), c.deadlineCode)

	if got.Phase != c.want.phase {
		t.Errorf("phase = %s, want %s (detail %q, failure %+v)", clsPhaseNames[got.Phase], clsPhaseNames[c.want.phase], got.Detail, got.Failure)
	}
	if got.ExitCode != c.want.exitCode {
		t.Errorf("exit code = %d, want %d", got.ExitCode, c.want.exitCode)
	}
	if got.Detail == "" {
		t.Error("detail is empty; the runner logs it as the step's state")
	}
	text := got.Detail
	switch {
	case c.want.code == "" && got.Failure != nil:
		t.Errorf("failure = %+v, want none", *got.Failure)
	case c.want.code != "" && got.Failure == nil:
		t.Errorf("failure = nil, want code %s", c.want.code)
	case c.want.code != "":
		if got.Failure.Code != c.want.code {
			t.Errorf("failure code = %q, want %q (message %q)", got.Failure.Code, c.want.code, got.Failure.Message)
		}
		if got.Failure.Message == "" {
			t.Error("failure message is empty; it is the sentence the check run shows")
		}
		text = got.Failure.Message
	}
	for _, m := range c.want.mentions {
		if !strings.Contains(text, m) {
			t.Errorf("%q does not mention %q", text, m)
		}
	}
}

// TestClassify is the classification table, one case per row, named for it,
// in the order the rows are decided.
func TestClassify(t *testing.T) {
	wantFailed := func(code string, mentions ...string) clsWant {
		return clsWant{phase: PhaseFailed, exitCode: -1, code: code, mentions: mentions}
	}
	wantPending := func(mentions ...string) clsWant {
		return clsWant{phase: PhasePending, exitCode: -1, mentions: mentions}
	}
	unschedulable := &Pod{
		Metadata: ObjectMeta{Name: "mp-a7a72726d5075767e0b6d115-w4pdd"},
		Status: PodStatus{Phase: "Pending", Conditions: []PodCondition{{
			Type: "PodScheduled", Status: "False", Reason: "Unschedulable",
			Message: "0/1 nodes are available: 1 node(s) didn't match Pod's node affinity/selector. preemption: 0/1 nodes are available: 1 Preemption is not helpful for scheduling.",
		}}},
	}

	cases := []clsCase{
		// -- the deadline --
		{
			// Measured: past the deadline the Job controller deletes the pod,
			// so by the time the Job reads Failed the pod is gone.
			name: "Job condition Failed with reason DeadlineExceeded",
			job: clsJob(12,
				clsCond("FailureTarget", "DeadlineExceeded", "Job was active longer than specified deadline"),
				clsCond("Failed", "DeadlineExceeded", "Job was active longer than specified deadline")),
			pod:          nil,
			deadlineCode: pl.CodeRunCeiling,
			want:         wantFailed(pl.CodeRunCeiling),
		},
		{
			// Measured: for the 30 s the pod takes to terminate, the Job says
			// only FailureTarget, and the step container still reads running.
			name:         "the deadline is read from FailureTarget while the pod is being deleted",
			job:          clsJob(12, clsCond("FailureTarget", "DeadlineExceeded", "Job was active longer than specified deadline")),
			pod:          clsPod("Running", []ContainerStatus{clsCloneDone, clsRunning("svc-db")}, clsRunning("step")),
			deadlineCode: pl.CodeStepTimeout,
			want:         wantFailed(pl.CodeStepTimeout),
		},
		{
			// Measured: a step that traps the deadline's TERM exits 0, at the
			// deadline (07:35:03 = start + 12 s), and the pod even reads
			// Succeeded. It was interrupted, not finished.
			name: "a step that ended at its deadline is the deadline though it exited 0",
			job: clsJob(12,
				clsCond("FailureTarget", "DeadlineExceeded", "Job was active longer than specified deadline"),
				clsCond("Failed", "DeadlineExceeded", "Job was active longer than specified deadline")),
			pod: clsPod("Succeeded",
				[]ContainerStatus{clsCloneDone, clsTerminated("svc-db", 137, "Error", "", clsStart.Add(43*time.Second))},
				clsTerminated("step", 0, "Completed", "", clsStart.Add(12*time.Second))),
			deadlineCode: pl.CodeStepTimeout,
			want:         wantFailed(pl.CodeStepTimeout),
		},
		{
			// Measured: the step exited 0 at 07:35:02, three seconds inside its
			// 14 s deadline, but a service's 30 s termination grace kept the pod
			// alive past it and the Job then failed DeadlineExceeded. The
			// command finished in time; its own answer stands.
			name:         "a step that finished before its deadline keeps its own answer",
			job:          clsJob(14, clsCond("FailureTarget", "DeadlineExceeded", "Job was active longer than specified deadline")),
			pod:          clsPod("Running", []ContainerStatus{clsCloneDone, clsRunning("svc-db")}, clsTerminated("step", 0, "Completed", "", clsStart.Add(11*time.Second))),
			deadlineCode: pl.CodeStepTimeout,
			want:         clsWant{phase: PhaseSucceeded, exitCode: 0},
		},
		{
			name:         "an empty deadline code reads as the step's own timeout",
			job:          clsJob(12, clsCond("Failed", "DeadlineExceeded", "Job was active longer than specified deadline")),
			pod:          nil,
			deadlineCode: "",
			want:         wantFailed(pl.CodeStepTimeout),
		},

		// -- images --
		{
			name: "any container waiting.reason ErrImagePull",
			job:  clsJob(1200),
			pod: clsPod("Pending", []ContainerStatus{clsCloneDone},
				clsWaiting("step", "registry.invalid.example/acme/nope:1", "ErrImagePull",
					`failed to pull and unpack image "registry.invalid.example/acme/nope:1": failed to resolve reference "registry.invalid.example/acme/nope:1": failed to do request: Head "https://registry.invalid.example/v2/acme/nope/manifests/1": dial tcp: lookup registry.invalid.example: no such host`)),
			want: wantFailed(pl.CodeImagePullFailed, "registry.invalid.example/acme/nope:1", "no such host"),
		},
		{
			name: "any container waiting.reason ImagePullBackOff",
			job:  clsJob(1200),
			pod: clsPod("Pending",
				[]ContainerStatus{clsWaiting("clone", "registry.invalid.example/acme/git:1", "ImagePullBackOff",
					`Back-off pulling image "registry.invalid.example/acme/git:1": ErrImagePull: failed to pull and unpack image "registry.invalid.example/acme/git:1": failed to resolve reference "registry.invalid.example/acme/git:1": failed to do request: Head "https://registry.invalid.example/v2/acme/git/manifests/1": dial tcp: lookup registry.invalid.example: no such host`)},
				clsStepInit),
			want: wantFailed(pl.CodeImagePullFailed, "registry.invalid.example/acme/git:1"),
		},
		{
			name: "any container waiting.reason InvalidImageName",
			job:  clsJob(1200),
			pod: clsPod("Pending", []ContainerStatus{clsCloneDone},
				clsWaiting("step", "Alpine:3.20", "InvalidImageName",
					`Failed to apply default image tag "Alpine:3.20": couldn't parse image name "Alpine:3.20": invalid reference format: repository name (library/Alpine) must be lowercase`)),
			want: wantFailed(pl.CodeImagePullFailed, "Alpine:3.20"),
		},
		{
			name: "a service's image that cannot be pulled",
			job:  clsJob(1200),
			pod: clsPod("Pending",
				[]ContainerStatus{clsCloneDone, clsWaiting("svc-db", "redis:7.9-nope", "ErrImagePull",
					`failed to pull and unpack image "docker.io/library/redis:7.9-nope": failed to resolve reference "docker.io/library/redis:7.9-nope": docker.io/library/redis:7.9-nope: not found`)},
				clsStepInit),
			want: wantFailed(pl.CodeImagePullFailed, "redis:7.9-nope"),
		},
		{
			// Constructed: every pull message measured names the image, but
			// that is the runtime's wording; the failure names it regardless.
			name: "a pull failure whose message does not name the image",
			job:  clsJob(1200),
			pod: clsPod("Pending", []ContainerStatus{clsCloneDone},
				clsWaiting("step", "ghcr.io/acme/toolchain:1.4", "ErrImagePull", "rpc error: code = DeadlineExceeded desc = context deadline exceeded")),
			want: wantFailed(pl.CodeImagePullFailed, "ghcr.io/acme/toolchain:1.4", "context deadline exceeded"),
		},
		{
			// Measured: fifteen pods starting at once tripped the kubelet's own
			// pull rate limit (registryPullQPS) on an image every other pod
			// pulled fine. The registry never refused anything: the next try,
			// after the back-off, pulls.
			name: "a pull the node throttled is not a failure",
			job:  clsJob(1200),
			pod: clsPod("Pending",
				[]ContainerStatus{clsWaiting("clone", "alpine:3.20", "ErrImagePull", "pull QPS exceeded"), clsSvcInit},
				clsStepInit),
			want: wantPending("pull QPS exceeded"),
		},
		{
			// Measured: the back-off after a throttled pull names the throttle.
			name: "the back-off after a throttled pull is not a failure",
			job:  clsJob(1200),
			pod: clsPod("Pending",
				[]ContainerStatus{clsWaiting("clone", "alpine:3.20", "ImagePullBackOff", `Back-off pulling image "alpine:3.20": ErrImagePull: pull QPS exceeded`)},
				clsStepInit),
			want: wantPending("pull QPS exceeded"),
		},

		// -- a container that cannot be created --
		{
			name: "waiting.reason == CreateContainerConfigError",
			job:  clsJob(1200),
			pod: clsPod("Pending", []ContainerStatus{clsCloneDone},
				clsWaiting("step", "alpine:3.20", "CreateContainerConfigError", `secret "mp-a7a72726d5075767e0b6d115-env" not found`)),
			want: wantFailed(pl.CodeJobRejected, `secret "mp-a7a72726d5075767e0b6d115-env" not found`),
		},

		// -- the owner's cache directory --
		{
			name: "cache-prep terminated with non-zero exit",
			job:  clsJob(1200),
			pod: clsPod("Failed",
				[]ContainerStatus{
					clsTerminated("cache-prep", 1, "Error", "memql: cannot prepare the cache directory /cache-root/owners/0dde18ce172fff450b32bf41\n", clsStart.Add(time.Second)),
					clsWaiting("clone", "alpine:3.20", "PodInitializing", ""),
				},
				clsStepInit),
			want: wantFailed(pl.CodeJobRejected, "/cache-root/owners/0dde18ce172fff450b32bf41"),
		},
		{
			name: "cache-prep waiting on a reason it cannot start past",
			job:  clsJob(1200),
			pod: clsPod("Pending",
				[]ContainerStatus{clsWaiting("cache-prep", "alpine:3.20", "CreateContainerError", "failed to generate container spec: failed to stat /var/lib/kubelet/pods/x/volumes/cache: no such device")},
				clsStepInit),
			want: wantFailed(pl.CodeJobRejected, "no such device"),
		},
		{
			// Measured: a volume that cannot mount leaves the pod scheduled and
			// every container waiting PodInitializing; the FailedMount reason is
			// only an event. Only the time it has waited tells it apart.
			name: "cache-prep still waiting to start past cfg.ScheduleTimeout",
			job:  clsJob(1200),
			pod: clsPod("Pending",
				[]ContainerStatus{clsWaiting("cache-prep", "alpine:3.20", "PodInitializing", ""), clsWaiting("clone", "alpine:3.20", "PodInitializing", "")},
				clsStepInit),
			now:  clsCreated.Add(10*time.Minute + time.Second),
			want: wantFailed(pl.CodeJobRejected, "10m0s"),
		},
		{
			// cache-prep runs the clone image, the first pull of a fresh node,
			// where the node's pull throttle (measured above) lands.
			name: "cache-prep waiting out a throttled pull",
			job:  clsJob(1200),
			pod: clsPod("Pending",
				[]ContainerStatus{clsWaiting("cache-prep", "alpine:3.20", "ImagePullBackOff", `Back-off pulling image "alpine:3.20": ErrImagePull: pull QPS exceeded`), clsWaiting("clone", "alpine:3.20", "PodInitializing", "")},
				clsStepInit),
			want: wantPending("pull QPS exceeded"),
		},
		{
			name: "cache-prep still waiting to start within cfg.ScheduleTimeout",
			job:  clsJob(1200),
			pod: clsPod("Pending",
				[]ContainerStatus{clsWaiting("cache-prep", "alpine:3.20", "PodInitializing", ""), clsWaiting("clone", "alpine:3.20", "PodInitializing", "")},
				clsStepInit),
			now:  clsCreated.Add(9*time.Minute + 59*time.Second),
			want: wantPending(),
		},

		// -- the clone --
		{
			// With terminationMessagePolicy FallbackToLogsOnError the clone's
			// last lines are its termination message (measured).
			name: "clone init container terminated with non-zero exit",
			job:  clsJob(1200, clsCond("Failed", "BackoffLimitExceeded", "Job has reached the specified backoff limit")),
			pod: clsPod("Failed",
				[]ContainerStatus{clsTerminated("clone", 128, "Error", "fatal: couldn't find remote ref 4c1f0a7e\n", clsStart.Add(time.Second)), clsSvcInit},
				clsStepInit),
			want: wantFailed(pl.CodeCloneFailed, "128", "couldn't find remote ref 4c1f0a7e"),
		},
		{
			// Measured: under the default termination-message policy a failed
			// clone's message is empty; its words are only in its log.
			name: "clone init container terminated with non-zero exit and no message",
			job:  clsJob(1200, clsCond("Failed", "BackoffLimitExceeded", "Job has reached the specified backoff limit")),
			pod: clsPod("Failed",
				[]ContainerStatus{clsTerminated("clone", 1, "Error", "", clsStart.Add(time.Second)), clsSvcInit},
				clsStepInit),
			want: wantFailed(pl.CodeCloneFailed),
		},

		// -- services --
		{
			// Measured: a sidecar that exits is restarted (restartPolicy
			// Always), and the pod never fails; the step waits for it forever.
			name: "a svc-* sidecar terminated before step started",
			job:  clsJob(1200),
			pod: clsPod("Pending",
				[]ContainerStatus{clsCloneDone, clsTerminated("svc-db", 1, "Error", "", clsStart.Add(2*time.Second))},
				clsStepInit),
			want: wantFailed(pl.CodeServiceFailed, "db"),
		},
		{
			// Measured: a sidecar whose startup probe fails is killed (137) and
			// restarted, again and again; the pod stays Pending until its
			// deadline. Between restarts the sidecar reads running, with the
			// kill in lastState.
			name: "a svc-* sidecar's startupProbe failed before step started",
			job:  clsJob(1200),
			pod: clsPod("Pending",
				[]ContainerStatus{clsCloneDone, {
					Name: "svc-db", Image: "docker.io/library/alpine:3.20", RestartCount: 1,
					State:     ContainerState{Running: &ContainerStateRunning{StartedAt: clsStart.Add(11 * time.Second)}},
					LastState: ContainerState{Terminated: &ContainerStateTerminated{ExitCode: 137, Reason: "Error", FinishedAt: clsStart.Add(10 * time.Second)}},
				}},
				clsStepInit),
			want: wantFailed(pl.CodeServiceFailed, "db", "exited 137"),
		},
		{
			// Measured: during the back-off the sidecar reads terminated with an
			// empty lastState and only the restart count says it has run before.
			name: "a svc-* sidecar in its restart back-off before step started",
			job:  clsJob(1200),
			pod: clsPod("Pending",
				[]ContainerStatus{clsCloneDone, {
					Name: "svc-db", Image: "docker.io/library/alpine:3.20", RestartCount: 7,
					State: ContainerState{Terminated: &ContainerStateTerminated{ExitCode: 137, Reason: "Error", FinishedAt: clsStart.Add(40 * time.Second)}},
				}},
				clsStepInit),
			want: wantFailed(pl.CodeServiceFailed, "db"),
		},
		{
			// Constructed: the kubelet garbage-collects dead containers, and a
			// last state goes with them; the restart count is the record that
			// survives.
			name: "a svc-* sidecar restarted with no record of how it stopped",
			job:  clsJob(1200),
			pod: clsPod("Pending",
				[]ContainerStatus{clsCloneDone, {
					Name: "svc-db", Image: "docker.io/library/alpine:3.20", RestartCount: 2,
					State: ContainerState{Running: &ContainerStateRunning{StartedAt: clsStart.Add(50 * time.Second)}},
				}},
				clsStepInit),
			want: wantFailed(pl.CodeServiceFailed, "db", "restarted 2 times"),
		},
		{
			name: "a service that restarts while the step runs leaves the step running",
			job:  clsJob(1200),
			pod: clsPod("Running",
				[]ContainerStatus{clsCloneDone, {
					Name: "svc-db", Image: "docker.io/library/alpine:3.20", RestartCount: 1,
					State:     ContainerState{Running: &ContainerStateRunning{StartedAt: clsStart.Add(20 * time.Second)}},
					LastState: ContainerState{Terminated: &ContainerStateTerminated{ExitCode: 1, Reason: "Error", FinishedAt: clsStart.Add(19 * time.Second)}},
				}},
				clsRunning("step")),
			want: clsWant{phase: PhaseRunning, exitCode: -1},
		},

		// -- scheduling --
		{
			name: "pod PodScheduled=False reason Unschedulable for longer than cfg.ScheduleTimeout since created",
			job:  clsJob(1200),
			pod:  unschedulable,
			now:  clsCreated.Add(10*time.Minute + time.Second),
			want: wantFailed(pl.CodeJobUnschedulable, "didn't match Pod's node affinity/selector"),
		},
		{
			// Dates a quarter-century old: a classifier that read the wall clock
			// instead of now would find this pod unschedulable for decades.
			name:    "an unschedulable pod within cfg.ScheduleTimeout is pending",
			job:     clsJob(1200),
			pod:     unschedulable,
			created: time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC),
			now:     time.Date(2001, 1, 1, 0, 1, 0, 0, time.UTC),
			want:    wantPending("didn't match Pod's node affinity/selector"),
		},

		// -- the step --
		{
			// Measured: the step's termination is visible seconds before the Job
			// completes, while the kubelet still stops the services.
			name: "step container terminated exit 0",
			job:  clsJob(1200),
			pod:  clsPod("Running", []ContainerStatus{clsCloneDone, clsRunning("svc-db")}, clsTerminated("step", 0, "Completed", "", clsStart.Add(4*time.Second))),
			want: clsWant{phase: PhaseSucceeded, exitCode: 0},
		},
		{
			// Measured: after the step ends the kubelet kills each service
			// (137, Error) before the Job completes. That is not a service
			// failure.
			name: "step container terminated exit 0 with its services stopped after it",
			job: clsJob(1200,
				clsCond("SuccessCriteriaMet", "CompletionsReached", "Reached expected number of succeeded pods"),
				clsCond("Complete", "CompletionsReached", "Reached expected number of succeeded pods")),
			pod: clsPod("Succeeded",
				[]ContainerStatus{clsCloneDone, clsTerminated("svc-db", 137, "Error", "", clsStart.Add(13*time.Second))},
				clsTerminated("step", 0, "Completed", "", clsStart.Add(4*time.Second))),
			want: clsWant{phase: PhaseSucceeded, exitCode: 0},
		},
		{
			name: "Job Complete",
			job: clsJob(1200,
				clsCond("SuccessCriteriaMet", "CompletionsReached", "Reached expected number of succeeded pods"),
				clsCond("Complete", "CompletionsReached", "Reached expected number of succeeded pods")),
			pod:  nil,
			want: clsWant{phase: PhaseSucceeded, exitCode: 0},
		},
		{
			name: "step container terminated exit N != 0",
			job: clsJob(1200,
				clsCond("FailureTarget", "BackoffLimitExceeded", "Job has reached the specified backoff limit"),
				clsCond("Failed", "BackoffLimitExceeded", "Job has reached the specified backoff limit")),
			pod: clsPod("Failed",
				[]ContainerStatus{clsCloneDone, clsTerminated("svc-db", 137, "Error", "", clsStart.Add(13*time.Second))},
				clsTerminated("step", 3, "Error", "", clsStart.Add(4*time.Second))),
			want: clsWant{phase: PhaseFailed, exitCode: 3},
		},
		{
			name: "step container running",
			job:  clsJob(1200),
			pod:  clsPod("Running", []ContainerStatus{clsCloneDone, clsRunning("svc-db")}, clsRunning("step")),
			want: clsWant{phase: PhaseRunning, exitCode: -1},
		},

		// -- nothing decided yet --
		{
			name: "otherwise: the Job has no pod yet",
			job:  clsJob(1200),
			pod:  nil,
			want: wantPending(),
		},
		{
			name: "otherwise: the clone is running",
			job:  clsJob(1200),
			pod:  clsPod("Pending", []ContainerStatus{clsRunning("clone"), clsSvcInit}, clsStepInit),
			want: wantPending(),
		},
		{
			name: "otherwise: a service is starting",
			job:  clsJob(1200),
			pod:  clsPod("Pending", []ContainerStatus{clsCloneDone, clsRunning("svc-db")}, clsStepInit),
			want: wantPending(),
		},

		// -- a failure nothing else explains --
		{
			// The pod went away (evicted, deleted, its node lost) and the Job
			// failed for it. Read as pending, the runner would wait for an
			// answer that cannot come.
			name: "a failed Job whose pod is gone",
			job: clsJob(1200,
				clsCond("FailureTarget", "BackoffLimitExceeded", "Job has reached the specified backoff limit"),
				clsCond("Failed", "BackoffLimitExceeded", "Job has reached the specified backoff limit")),
			pod:  nil,
			want: wantFailed(pl.CodeNodeLost, "BackoffLimitExceeded"),
		},
		{
			// Measured: a pod the kubelet refuses at admission fails with the
			// reason on the pod alone -- no conditions, no container statuses
			// -- before the Job has caught up.
			name: "a pod the kubelet rejected before any container ran",
			job:  clsJob(1200),
			pod: &Pod{
				Metadata: ObjectMeta{Name: "mp-a7a72726d5075767e0b6d115-cznfj"},
				Status: PodStatus{
					Phase: "Failed", Reason: "OutOfcpu",
					Message: "Pod was rejected: Node didn't have enough resource: cpu, requested: 500000, used: 100, capacity: 24000",
				},
			},
			want: wantFailed(pl.CodeNodeLost, "OutOfcpu"),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { runClassifyCase(t, c) })
	}
}

// TestClassifyImagePullBackOffIsTerminal (Review Focus 3): an image that
// cannot be pulled fails the step seconds after its Job is created, typed and
// naming the image, rather than holding the step until its 20-minute deadline.
func TestClassifyImagePullBackOffIsTerminal(t *testing.T) {
	job := clsJob(1200)
	pod := clsPod("Pending", []ContainerStatus{clsCloneDone},
		clsWaiting("step", "ghcr.io/acme/toolchain:does-not-exist", "ImagePullBackOff",
			`Back-off pulling image "ghcr.io/acme/toolchain:does-not-exist": ErrImagePull: failed to pull and unpack image "ghcr.io/acme/toolchain:does-not-exist": failed to resolve reference "ghcr.io/acme/toolchain:does-not-exist": ghcr.io/acme/toolchain:does-not-exist: not found`))

	got := Classify(job, pod, clsCreated, clsCreated.Add(5*time.Second), clsConfig(), pl.CodeStepTimeout)

	if got.Phase != PhaseFailed || got.Failure == nil {
		t.Fatalf("5 s after creation the step is %s with failure %+v; want failed now, not at its deadline", clsPhaseNames[got.Phase], got.Failure)
	}
	if got.Failure.Code != pl.CodeImagePullFailed {
		t.Errorf("code = %q, want %q", got.Failure.Code, pl.CodeImagePullFailed)
	}
	if !strings.Contains(got.Failure.Message, "ghcr.io/acme/toolchain:does-not-exist") {
		t.Errorf("message %q does not name the image", got.Failure.Message)
	}
	if got.ExitCode != -1 {
		t.Errorf("exit code = %d, want -1: the command never ran", got.ExitCode)
	}
}

// TestClassifyNeverRepeatsTheStepsOwnTerminationMessage: the step container's
// termination message is whatever the step's command wrote to
// /dev/termination-log, which can be a secret it was given. The runner logs
// Detail and puts Failure on the check run, unmasked, so neither may carry it.
// A service's or the clone's message is platform or declared output and does
// appear (the rows above).
func TestClassifyNeverRepeatsTheStepsOwnTerminationMessage(t *testing.T) {
	planted := "npm-" + strings.Repeat("q", 16)
	for _, c := range []struct {
		name  string
		step  ContainerStatus
		phase Phase
		exit  int
	}{
		{"a failed step", clsTerminated("step", 3, "Error", "NPM_TOKEN="+planted+"\n", clsStart.Add(4*time.Second)), PhaseFailed, 3},
		{"a succeeded step", clsTerminated("step", 0, "Completed", planted, clsStart.Add(4*time.Second)), PhaseSucceeded, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := Classify(clsJob(1200), clsPod("Failed", []ContainerStatus{clsCloneDone}, c.step), clsCreated, clsNow, clsConfig(), pl.CodeStepTimeout)
			if got.Phase != c.phase || got.ExitCode != c.exit || got.Detail == "" {
				t.Fatalf("observation = %s exit %d detail %q, want %s exit %d and a detail", clsPhaseNames[got.Phase], got.ExitCode, got.Detail, clsPhaseNames[c.phase], c.exit)
			}
			seen := got.Detail
			if got.Failure != nil {
				seen += " " + got.Failure.Message
			}
			if strings.Contains(seen, planted) {
				t.Errorf("the observation repeats the step's termination message: %q", seen)
			}
		})
	}
}
