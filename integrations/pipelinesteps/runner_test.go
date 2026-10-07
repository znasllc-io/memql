package pipelinesteps

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/deploycontrol"
	pl "github.com/znasllc-io/memql/component/pipelines"
)

// runner_test.go -- the runner against a fake API server (epic memql#5478,
// #5493, #5495).
//
// rtCluster stands in for the API server, the Job controller and the kubelet
// as a small state machine. It keeps Jobs and Secrets the way the API server
// does (a resourceVersion bumped on every write, compare-and-swap patches,
// 404 for what is absent, 409 for what exists, the ceiling's quota refusal);
// it gives each Job one pod whose state a script advances on every read of
// the Job's pods; and it serves the step container's log the way the API
// server follows one -- the stream stays open while the container runs and
// ends when it stops. Other replicas are played by annotations a test writes
// into the fake at the moment a hook names.

const (
	rtNode  = "workbench-b" // the replica under test
	rtOther = "workbench-a" // another replica
)

var (
	// rtT0 is the runner's clock in every test. It stands still unless a
	// test moves it, so a claim is fresh or stale because the test said so.
	rtT0 = time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	// rtCloneToken is what the token minter answers, assembled from parts
	// so a secret scanner reads it as the fixture it is.
	rtCloneToken = "clonetok-" + strings.Repeat("q", 12)
)

func rtAt(ms int) time.Time { return rtT0.Add(time.Duration(ms) * time.Millisecond) }

// rtStamp is a claim as names.go documents AnnotRunner: "<nodeId> <RFC3339>".
func rtStamp(node string, at time.Time) string { return node + " " + at.UTC().Format(time.RFC3339) }

func rtConfig() Config {
	cfg := testConfig()
	cfg.NodeID = rtNode
	cfg.PollInterval = 2 * time.Millisecond
	cfg.HeartbeatInterval = 3 * time.Millisecond
	cfg.HeartbeatStale = 45 * time.Second
	cfg.ScheduleTimeout = 10 * time.Minute
	cfg.LogStoreMaxLines = 100
	cfg.ArchiveMaxBytes = 1 << 20
	cfg.ArtifactMaxBytes = 1 << 20
	return cfg
}

// rtRun is testRun without services, caches or artifacts, of a run whose
// ceiling is a day away (rtRunDeadline): a test that needs one of them, or a
// nearer ceiling, sets it.
func rtRun() StepRun {
	run := testRun()
	run.Services, run.Caches, run.Artifacts = nil, nil, nil
	run.RunDeadline = rtRunDeadline(24 * time.Hour)
	return run
}

// rtRunDeadline is a run's deadline as the agent stamps one (StepRun.
// RunDeadline): its ceiling, d after the runner's clock begins.
func rtRunDeadline(d time.Duration) string { return rtT0.Add(d).Format(time.RFC3339Nano) }

// ---------------------------------------------------------------------------
// Pods and scripts
// ---------------------------------------------------------------------------

func rtPodName(job string) string { return job + "-x7kk6" }

// rtPod is the Job's pod, scheduled, with the step container and the init
// containers given.
func rtPod(job string, step ContainerStatus, init ...ContainerStatus) *Pod {
	phase := "Pending"
	switch s := step.State; {
	case s.Running != nil:
		phase = "Running"
	case s.Terminated != nil && s.Terminated.ExitCode == 0:
		phase = "Succeeded"
	case s.Terminated != nil:
		phase = "Failed"
	}
	return &Pod{
		Metadata: ObjectMeta{Name: rtPodName(job), Labels: map[string]string{"job-name": job}, CreationTimestamp: rtT0},
		Status: PodStatus{
			Phase:                 phase,
			Conditions:            []PodCondition{{Type: "PodScheduled", Status: "True"}},
			InitContainerStatuses: init,
			ContainerStatuses:     []ContainerStatus{step},
		},
	}
}

func rtStepRunning(since time.Time) ContainerStatus {
	return ContainerStatus{Name: ContainerStep, Image: "registry.example.com/acme/toolchain:1.4", State: ContainerState{
		Running: &ContainerStateRunning{StartedAt: since},
	}}
}

func rtStepEnded(exit int32, started, finished time.Time) ContainerStatus {
	reason := "Completed"
	if exit != 0 {
		reason = "Error"
	}
	return ContainerStatus{Name: ContainerStep, Image: "registry.example.com/acme/toolchain:1.4", State: ContainerState{
		Terminated: &ContainerStateTerminated{ExitCode: exit, Reason: reason, StartedAt: started, FinishedAt: finished},
	}}
}

var rtCloneRunning = ContainerStatus{Name: ContainerClone, Image: "registry.example.com/library/git:2", State: ContainerState{
	Running: &ContainerStateRunning{StartedAt: rtT0},
}}

// rtCloneTail is what a tail of a finished clone returns.
var rtCloneTail = "memql: checked out " + testSHA + "\n"

// rtState is one moment of a Job and its pod. Every read of the Job's pods
// answers the current state, then moves to the next once the state has
// answered reads reads (at least one) and until, when set, holds.
type rtState struct {
	pod     *Pod // nil: the Job controller has not created the pod
	job     JobStatus
	visible int // how many of the script's log lines the step has written
	reads   int
	until   func(c *rtCluster) bool // called with c.mu held
	// leftover is a pod of an earlier Job of the same name, deleted (a
	// cancel, an ack) and not yet collected: it carries the job-name label
	// the runner selects by, and the earlier Job's uid.
	leftover *Pod
	// pods are the pods of an Indexed Job -- the isolation probe's, one per
	// completion index -- answered after pod and leftover, each owned by the
	// Job. Nil entries are pods the controller has not created.
	pods []*Pod
}

// rtLeftoverUID is the uid of the earlier Job a leftover pod belonged to.
const rtLeftoverUID = "uid-earlier-job"

// rtScript is a Job's life: its states, the step container's log as the API
// returns it with timestamps=true, and what a tail of each other container
// returns -- a container with no tail has not started.
type rtScript struct {
	states []rtState
	log    []string
	tails  map[string]string
	// dropAfter ends the first followed stream after that many lines while
	// the step still runs: a connection that dropped.
	dropAfter int
	// final are lines the step wrote as it ended: only a stream opened once
	// the step container has terminated serves them. A stream held open
	// while it ran ends only after the runner has read the pod as ended --
	// the API's stream lags the container -- and without them.
	final []string
	// cutFirstStream is written, without its newline, at the end of the
	// first followed stream, which then ends: a connection that dropped
	// while the step was writing a line.
	cutFirstStream string
	// finalNoEOL serves the last line of a stream opened once the step has
	// ended without its newline: a step whose last output did not end one.
	finalNoEOL bool
}

// rtRunningScript is a step that prints its lines and runs until the test
// ends it.
func rtRunningScript(job string, lines ...string) *rtScript {
	return &rtScript{
		states: []rtState{{pod: rtPod(job, rtStepRunning(rtAt(1000)), clsCloneDone), visible: len(lines)}},
		log:    lines,
		tails:  map[string]string{ContainerClone: rtCloneTail},
	}
}

// rtFinishingScript is a step that prints its lines and exits with exit.
func rtFinishingScript(job string, exit int32, lines ...string) *rtScript {
	return &rtScript{
		states: []rtState{
			{pod: rtPod(job, rtStepRunning(rtAt(1000)), clsCloneDone), visible: len(lines), reads: 2},
			{pod: rtPod(job, rtStepEnded(exit, rtAt(1000), rtAt(9000)), clsCloneDone), visible: len(lines)},
		},
		log:   lines,
		tails: map[string]string{ContainerClone: rtCloneTail},
	}
}

// ---------------------------------------------------------------------------
// The fake API server
// ---------------------------------------------------------------------------

type rtJob struct {
	job       Job
	script    *rtScript
	state     int
	reads     int
	createdRV string
	// endSeen: a read of the Job's pods answered its step container ended.
	endSeen bool
}

// rtReq is one request the fake received, and when.
type rtReq struct {
	kubeReq
	at time.Time
}

// rtPatch is one patch of a Job's annotations the fake applied, and the
// resourceVersion it was conditioned on ("" for none).
type rtPatch struct {
	job    string
	annots map[string]*string
	rv     string
}

func (p rtPatch) get(key string) (string, bool) {
	v, ok := p.annots[key]
	if !ok || v == nil {
		return "", false
	}
	return *v, true
}

type rtCluster struct {
	t     *testing.T
	clock *rtClock
	kube  *Kube

	mu      sync.Mutex
	rv, uid int
	jobs    map[string]*rtJob
	secrets map[string]Secret
	scripts map[string]*rtScript
	// Applied deletes exclude rejected preconditions and already-absent objects.
	// Requests alone cannot count effects: cleanup deliberately retries conflicts.
	deletedJobs, deletedSecrets []ObjectMeta
	// quotaRefusals is how many Job creates the ceiling refuses first.
	quotaRefusals int
	// quotaJobs are Jobs the ceiling refuses for as long as they are named.
	quotaJobs map[string]bool
	// jobCeiling, when set, is the namespace's count/jobs.batch quota (Review
	// Focus 4): a create is refused exceeded-quota while that many Jobs exist,
	// finished ones included, as the ResourceQuota counts them. mostJobs is
	// the most that ever existed at once, and made is every Job admitted, as
	// it was created.
	jobCeiling int
	mostJobs   int
	made       []Job
	// createJobAnswer, when set, answers every Job create instead.
	createJobAnswer *kubeAnswer
	// createJobAnswers answer the first Job creates, one each, before
	// anything else does.
	createJobAnswers []kubeAnswer
	loseJobCreates   int // store the Job, then lose its create response
	// jobGetFailures answer the first GETs of a Job, one each.
	jobGetFailures []kubeAnswer
	reqs           []rtReq
	patches        []rtPatch
	jobGets        int
	streams        int
	changed        chan struct{}
	closing        chan struct{}
	// onJobGet runs, with c.mu held, on every GET of a Job before it is
	// answered: n counts them.
	onJobGet func(c *rtCluster, n int)
	// onJobPatch runs, with c.mu held, on every patch of a Job before it is
	// applied.
	onJobPatch func(c *rtCluster, p rtPatch)
	// onJobCreate runs, with c.mu held, on every Job create before it is
	// answered: n counts them. What it does is done before the runner hears
	// the answer, so the runner's next try sees it.
	onJobCreate func(c *rtCluster, n int)
	jobCreates  int
	// refuseBeats answers 503 to every patch that carries a log cursor -- a
	// heartbeat of a runner that has captured a line -- applying nothing.
	refuseBeats bool
	// loseClaims applies that many claims -- patches of the runner
	// annotation alone, on a version -- and answers each 503, as a reply
	// lost on its way back.
	loseClaims int
	// blockTails makes every tail of a container wait for its caller to
	// give up: a kubelet that does not answer.
	blockTails bool
	// onSecretCreate runs, with c.mu held, on every Secret create before it
	// is answered.
	onSecretCreate  func(c *rtCluster, s Secret)
	onSecretPatched func(c *rtCluster, s Secret)
	// createSecretAnswers answer the first Secret creates, one each, before
	// anything else does.
	createSecretAnswers []kubeAnswer
	// holdJobDeletes holds every DELETE of the named Job until its channel
	// is closed: a delete still on its way.
	holdJobDeletes map[string]chan struct{}
	// podsAnswer, when set, answers every list of pods instead.
	podsAnswer *kubeAnswer
	// Probe role patches persist across scripted status transitions, by UID.
	observedPods   map[string]Pod
	probeRoles     map[string]ObjectMeta
	podPatchAnswer *kubeAnswer
	policyAnswer   *kubeAnswer
	// loseSecretCreates makes that many Secret creates, and answers each
	// 503, as a reply lost on its way back.
	loseSecretCreates int
	loseSecretPatches int // persist a patch, then lose its response
	// tailAnswer, when set, answers every tail of a container's log
	// instead: the API server's word for a kubelet it cannot reach.
	tailAnswer *kubeAnswer
}

func newRTCluster(t *testing.T, clock *rtClock) *rtCluster {
	t.Helper()
	c := &rtCluster{
		t: t, clock: clock,
		jobs: map[string]*rtJob{}, secrets: map[string]Secret{}, scripts: map[string]*rtScript{},
		changed: make(chan struct{}), closing: make(chan struct{}),
		observedPods: map[string]Pod{}, probeRoles: map[string]ObjectMeta{},
	}
	srv := httptest.NewServer(http.HandlerFunc(c.serve))
	t.Cleanup(srv.Close)
	// Registered after srv.Close, so it runs first: a stream still open
	// would otherwise keep Close waiting for it.
	t.Cleanup(func() { close(c.closing) })
	c.kube = NewKube(deploycontrol.NewClusterAPIWith(srv.URL, "test-token", srv.Client()), "steps-ns")
	return c
}

func rtJSON(w http.ResponseWriter, code int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(b)
}

func rtAnswer(w http.ResponseWriter, a kubeAnswer) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(a.code)
	_, _ = io.WriteString(w, a.body)
}

// rtQuotaRefusal is the ceiling's quota refusing a Job create.
func rtQuotaRefusal(name string) kubeAnswer {
	return kubeStatus(403, "Forbidden", fmt.Sprintf(`jobs.batch %q is forbidden: exceeded quota: memql-pipelines-ceiling, requested: count/jobs.batch=1, used: count/jobs.batch=2, limited: count/jobs.batch=2`, name))
}

// rtUnavailable is an API server answer that may pass.
var rtUnavailable = kubeStatus(503, "ServiceUnavailable", "the server is currently unable to handle the request")

// rtNotStarted is the kubelet's answer for the log of a container that has
// not started (measured, kube_test.go).
func rtNotStarted(container, pod string) kubeAnswer {
	return kubeStatus(400, "BadRequest", fmt.Sprintf("container %q in pod %q is waiting to start: PodInitializing", container, pod))
}

func (c *rtCluster) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		// A step abandoning the isolation proof can cancel a create while
		// its body is still arriving. The API never admitted that request;
		// do not parse its partial JSON or count it as a malformed Job.
		http.Error(w, "request body interrupted", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	c.reqs = append(c.reqs, rtReq{kubeReq: kubeReq{
		Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, ContentType: r.Header.Get("Content-Type"), Body: string(body),
	}, at: time.Now()})
	c.mu.Unlock()

	p, q := r.URL.Path, r.URL.Query()
	switch {
	case p == "/apis/networking.k8s.io/v1/namespaces/steps-ns/networkpolicies" && r.Method == http.MethodGet:
		c.mu.Lock()
		answer := c.policyAnswer
		c.mu.Unlock()
		if answer != nil {
			rtAnswer(w, *answer)
			return
		}
		rtJSON(w, 200, map[string]any{"items": []any{map[string]any{"metadata": map[string]any{"name": "memql-pipelines-isolate"}, "spec": map[string]any{"podSelector": map[string]any{}, "policyTypes": []string{"Egress"}, "egress": []any{map[string]any{"to": []any{map[string]any{"ipBlock": map[string]any{"cidr": "0.0.0.0/0", "except": isolationProtectedCIDRs}}}}}}}}})
	case p == kubeJobs && r.Method == http.MethodPost:
		c.createJob(w, body)
	case p == kubeJobs && r.Method == http.MethodGet:
		c.listJobMetadata(w, r)
	case p == kubeJobs && r.Method == http.MethodDelete:
		c.deleteCollection(w, q.Get("labelSelector"), true)
	case strings.HasPrefix(p, kubeJobs+"/"):
		name := strings.TrimPrefix(p, kubeJobs+"/")
		switch r.Method {
		case http.MethodGet:
			c.getJob(w, name)
		case http.MethodPatch:
			c.patchJob(w, name, body)
		case http.MethodDelete:
			c.deleteJob(w, name, q, body)
		default:
			c.unexpected(w, r)
		}
	case p == kubeSecrets && r.Method == http.MethodPost:
		c.createSecret(w, body)
	case p == kubeSecrets && r.Method == http.MethodGet:
		c.listSecrets(w, r)
	case p == kubeSecrets && r.Method == http.MethodDelete:
		c.deleteCollection(w, q.Get("labelSelector"), false)
	case strings.HasPrefix(p, kubeSecrets+"/"):
		name := strings.TrimPrefix(p, kubeSecrets+"/")
		switch r.Method {
		case http.MethodGet:
			c.getSecret(w, name)
		case http.MethodPatch:
			c.patchSecret(w, name, body)
		case http.MethodDelete:
			c.deleteSecret(w, name, q, body)
		default:
			c.unexpected(w, r)
		}
	case strings.HasPrefix(p, kubePods+"/") && r.Method == http.MethodPatch:
		c.patchPod(w, strings.TrimPrefix(p, kubePods+"/"), body)
	case p == kubePods && r.Method == http.MethodGet:
		c.listPods(w, q.Get("labelSelector"))
	case strings.HasPrefix(p, kubePods+"/") && strings.HasSuffix(p, "/log") && r.Method == http.MethodGet:
		c.podLog(w, r, strings.TrimSuffix(strings.TrimPrefix(p, kubePods+"/"), "/log"))
	default:
		c.unexpected(w, r)
	}
}

func (c *rtCluster) listJobMetadata(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key, value, ok := strings.Cut(r.URL.Query().Get("labelSelector"), "=")
	if !ok {
		c.t.Error("Jobs listed without a label selector")
	}
	items := []map[string]any{}
	for _, job := range c.jobs {
		if job.job.Metadata.Labels[key] == value {
			items = append(items, map[string]any{"metadata": job.job.Metadata})
		}
	}
	rtJSON(w, 200, map[string]any{"items": items})
}

func (c *rtCluster) unexpected(w http.ResponseWriter, r *http.Request) {
	c.t.Errorf("the fake API server got an unexpected request %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
	rtAnswer(w, kubeAnswer{code: 599, body: "no such route in the fake"})
}

// bumpLocked wakes every stream waiting for the fake to change.
func (c *rtCluster) bumpLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// viewLocked is a Job as a GET answers it: its status is its script's.
func (c *rtCluster) viewLocked(j *rtJob) Job {
	v := j.job
	v.Metadata.Annotations = make(map[string]string, len(j.job.Metadata.Annotations))
	for k, val := range j.job.Metadata.Annotations {
		v.Metadata.Annotations[k] = val
	}
	if j.script != nil && len(j.script.states) > 0 {
		v.Status = j.script.states[j.state].job
	}
	return v
}

func (c *rtCluster) createJob(w http.ResponseWriter, body []byte) {
	var job Job
	if err := json.Unmarshal(body, &job); err != nil {
		c.t.Errorf("a Job create that is not a Job: %v", err)
		rtAnswer(w, kubeStatus(400, "BadRequest", "not a Job"))
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.jobCreates++
	if c.onJobCreate != nil {
		c.onJobCreate(c, c.jobCreates)
	}
	name := job.Metadata.Name
	switch {
	case len(c.createJobAnswers) > 0:
		a := c.createJobAnswers[0]
		c.createJobAnswers = c.createJobAnswers[1:]
		rtAnswer(w, a)
		return
	case c.createJobAnswer != nil:
		rtAnswer(w, *c.createJobAnswer)
		return
	case c.quotaRefusals > 0 || c.quotaJobs[name]:
		if !c.quotaJobs[name] {
			c.quotaRefusals--
		}
		rtAnswer(w, rtQuotaRefusal(name))
		return
	case c.jobCeiling > 0 && len(c.jobs) >= c.jobCeiling:
		// Admission, before storage: a full ceiling refuses the create
		// whether or not a Job of that name exists.
		rtAnswer(w, rtQuotaRefusal(name))
		return
	case c.jobs[name] != nil:
		rtAnswer(w, kubeStatus(409, "AlreadyExists", fmt.Sprintf(`jobs.batch %q already exists`, name)))
		return
	}
	s := c.scripts[name]
	if s == nil {
		c.t.Errorf("the runner created Job %s, which no script describes", name)
		s = &rtScript{states: []rtState{{}}}
	}
	c.uid++
	c.rv++
	job.Metadata.UID = fmt.Sprintf("uid-%d", c.uid)
	job.Metadata.ResourceVersion = strconv.Itoa(c.rv)
	job.Metadata.CreationTimestamp = c.clock.Now()
	j := &rtJob{job: job, script: s, createdRV: job.Metadata.ResourceVersion}
	c.jobs[name] = j
	c.mostJobs = max(c.mostJobs, len(c.jobs))
	c.made = append(c.made, c.viewLocked(j))
	c.bumpLocked()
	if c.loseJobCreates > 0 {
		c.loseJobCreates--
		rtAnswer(w, rtUnavailable)
	} else {
		rtJSON(w, 201, c.viewLocked(j))
	}
	// The Job controller writes the new Job's status at once (measured on
	// k3s v1.32: the creator's first claim, conditioned on the version the
	// create answered, is refused 409). A status write is a new version.
	c.rv++
	j.job.Metadata.ResourceVersion = strconv.Itoa(c.rv)
}

func (c *rtCluster) getJob(w http.ResponseWriter, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.jobGets++
	if c.onJobGet != nil {
		c.onJobGet(c, c.jobGets)
	}
	if len(c.jobGetFailures) > 0 {
		a := c.jobGetFailures[0]
		c.jobGetFailures = c.jobGetFailures[1:]
		rtAnswer(w, a)
		return
	}
	j := c.jobs[name]
	if j == nil {
		rtAnswer(w, kubeStatus(404, "NotFound", fmt.Sprintf(`jobs.batch %q not found`, name)))
		return
	}
	rtJSON(w, 200, c.viewLocked(j))
}

func (c *rtCluster) patchJob(w http.ResponseWriter, name string, body []byte) {
	var patch struct {
		Metadata struct {
			Annotations     map[string]*string `json:"annotations"`
			ResourceVersion string             `json:"resourceVersion"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &patch); err != nil {
		c.t.Errorf("a Job patch that is not a merge patch: %v", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	p := rtPatch{job: name, annots: patch.Metadata.Annotations, rv: patch.Metadata.ResourceVersion}
	if c.onJobPatch != nil {
		c.onJobPatch(c, p)
	}
	j := c.jobs[name]
	_, beat := p.get(AnnotLogCursor)
	_, runner := p.get(AnnotRunner)
	lose := c.loseClaims > 0 && runner && !beat && len(p.annots) == 1 && p.rv != ""
	switch {
	case j == nil:
		rtAnswer(w, kubeStatus(404, "NotFound", fmt.Sprintf(`jobs.batch %q not found`, name)))
		return
	case p.rv != "" && p.rv != j.job.Metadata.ResourceVersion:
		rtAnswer(w, kubeStatus(409, "Conflict", fmt.Sprintf(`Operation cannot be fulfilled on jobs.batch %q: the object has been modified; please apply your changes to the latest version and try again`, name)))
		return
	case c.refuseBeats && beat:
		rtAnswer(w, kubeStatus(503, "ServiceUnavailable", "the server is currently unable to handle the request"))
		return
	}
	merged := map[string]string{}
	for k, v := range j.job.Metadata.Annotations {
		merged[k] = v
	}
	for k, v := range p.annots {
		if v == nil {
			delete(merged, k)
		} else {
			merged[k] = *v
		}
	}
	// The API server's own limit: all of an object's annotations together.
	total := 0
	for k, v := range merged {
		total += len(k) + len(v)
	}
	if total > 256<<10 {
		c.t.Errorf("the runner's annotations on Job %s add up to %d bytes, past the API server's 262144", name, total)
		rtAnswer(w, kubeStatus(422, "Invalid", fmt.Sprintf(`Job.batch %q is invalid: metadata.annotations: Too long: must have at most 262144 bytes`, name)))
		return
	}
	j.job.Metadata.Annotations = merged
	c.rv++
	j.job.Metadata.ResourceVersion = strconv.Itoa(c.rv)
	c.patches = append(c.patches, p)
	c.bumpLocked()
	if lose {
		c.loseClaims--
		rtAnswer(w, kubeStatus(503, "ServiceUnavailable", "the server is currently unable to handle the request"))
		return
	}
	rtJSON(w, 200, c.viewLocked(j))
}

func (c *rtCluster) wantPropagation(q map[string][]string, what, policy string) {
	if got := q["propagationPolicy"]; len(got) != 1 || got[0] != policy {
		c.t.Errorf("%s deleted with propagationPolicy %q, want %s", what, got, policy)
	}
}

func (c *rtCluster) deleteJob(w http.ResponseWriter, name string, q map[string][]string, body []byte) {
	c.wantPropagation(q, "job "+name, "Foreground")
	c.mu.Lock()
	hold := c.holdJobDeletes[name]
	c.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-c.closing:
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.jobs[name] == nil {
		rtAnswer(w, kubeStatus(404, "NotFound", fmt.Sprintf(`jobs.batch %q not found`, name)))
		return
	}
	if len(body) > 0 {
		var opts struct {
			Preconditions struct{ UID, ResourceVersion string }
		}
		if err := json.Unmarshal(body, &opts); err != nil {
			c.t.Fatal(err)
		}
		meta := c.jobs[name].job.Metadata
		if opts.Preconditions.UID != meta.UID || opts.Preconditions.ResourceVersion != meta.ResourceVersion {
			rtAnswer(w, kubeStatus(409, "Conflict", "delete preconditions changed"))
			return
		}
	}
	c.deletedJobs = append(c.deletedJobs, c.jobs[name].job.Metadata)
	c.deleteJobLocked(name)
	rtJSON(w, 200, map[string]any{"kind": "Status", "status": "Success"})
}

func (c *rtCluster) createSecret(w http.ResponseWriter, body []byte) {
	var s Secret
	if err := json.Unmarshal(body, &s); err != nil {
		c.t.Errorf("a Secret create that is not a Secret: %v", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.onSecretCreate != nil {
		c.onSecretCreate(c, s)
	}
	if len(c.createSecretAnswers) > 0 {
		a := c.createSecretAnswers[0]
		c.createSecretAnswers = c.createSecretAnswers[1:]
		rtAnswer(w, a)
		return
	}
	if _, ok := c.secrets[s.Metadata.Name]; ok {
		rtAnswer(w, kubeStatus(409, "AlreadyExists", fmt.Sprintf(`secrets %q already exists`, s.Metadata.Name)))
		return
	}
	c.rv++
	s.Metadata.UID = fmt.Sprintf("secret-uid-%d", c.rv)
	s.Metadata.ResourceVersion = strconv.Itoa(c.rv)
	s.Metadata.CreationTimestamp = c.clock.Now()
	c.secrets[s.Metadata.Name] = s
	if c.loseSecretCreates > 0 {
		c.loseSecretCreates--
		rtAnswer(w, rtUnavailable)
		return
	}
	rtJSON(w, 201, s)
}

// listSecrets answers a list of Secrets as the API server answers one asked
// for metadata alone (measured on k3s v1.35, ClusterAPI.ListMetadata): the
// label selector applied, at most limit items in name order, and a continue
// token while more remain. A list that would carry the Secrets' values fails
// the test.
func (c *rtCluster) listSecrets(w http.ResponseWriter, r *http.Request) {
	if accept := r.Header.Get("Accept"); accept != "application/json;as=PartialObjectMetadataList;g=meta.k8s.io;v=v1" {
		c.t.Errorf("Secrets listed with Accept %q: the answer would carry their values", accept)
	}
	q := r.URL.Query()
	key, value, ok := strings.Cut(q.Get("labelSelector"), "=")
	if !ok {
		c.t.Errorf("Secrets listed by %q, not by one label: that would list every Secret in the namespace", q.Get("labelSelector"))
	}
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil || limit < 1 {
		c.t.Errorf("Secrets listed with limit %q: an unbounded list", q.Get("limit"))
		limit = 1 << 30
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var names []string
	for name, s := range c.secrets {
		if s.Metadata.Labels[key] == value && name > q.Get("continue") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	type item struct {
		Metadata ObjectMeta `json:"metadata"`
	}
	list := struct {
		Kind     string            `json:"kind"`
		Metadata map[string]string `json:"metadata"`
		Items    []item            `json:"items"`
	}{Kind: "PartialObjectMetadataList", Metadata: map[string]string{}, Items: []item{}}
	for i, name := range names {
		if i == limit {
			list.Metadata["continue"] = names[i-1]
			break
		}
		list.Items = append(list.Items, item{Metadata: c.secrets[name].Metadata})
	}
	rtJSON(w, 200, list)
}

func (c *rtCluster) getSecret(w http.ResponseWriter, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.secrets[name]
	if !ok {
		rtAnswer(w, kubeStatus(404, "NotFound", fmt.Sprintf(`secrets %q not found`, name)))
		return
	}
	rtJSON(w, 200, s)
}

// patchSecret applies a merge patch of a Secret's owners or of its data.
func (c *rtCluster) patchSecret(w http.ResponseWriter, name string, body []byte) {
	var patch struct {
		Metadata struct {
			OwnerReferences []OwnerReference  `json:"ownerReferences"`
			UID             string            `json:"uid"`
			ResourceVersion string            `json:"resourceVersion"`
			Annotations     map[string]string `json:"annotations"`
		} `json:"metadata"`
		Data map[string][]byte `json:"data"`
	}
	if err := json.Unmarshal(body, &patch); err != nil {
		c.t.Errorf("a Secret patch that is not a merge patch: %v", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.secrets[name]
	if !ok {
		rtAnswer(w, kubeStatus(404, "NotFound", fmt.Sprintf(`secrets %q not found`, name)))
		return
	}
	if patch.Metadata.OwnerReferences != nil {
		s.Metadata.OwnerReferences = patch.Metadata.OwnerReferences
	}
	if (patch.Metadata.UID != "" && patch.Metadata.UID != s.Metadata.UID) ||
		(patch.Metadata.ResourceVersion != "" && patch.Metadata.ResourceVersion != s.Metadata.ResourceVersion) {
		rtAnswer(w, kubeStatus(409, "Conflict", "Secret revision changed"))
		return
	}
	if patch.Metadata.Annotations != nil {
		annotations := make(map[string]string, len(s.Metadata.Annotations)+len(patch.Metadata.Annotations))
		for key, value := range s.Metadata.Annotations {
			annotations[key] = value
		}
		for key, value := range patch.Metadata.Annotations {
			annotations[key] = value
		}
		s.Metadata.Annotations = annotations
	}
	if len(patch.Data) > 0 {
		data := make(map[string][]byte, len(s.Data)+len(patch.Data))
		for k, v := range s.Data {
			data[k] = v
		}
		for k, v := range patch.Data {
			data[k] = v
		}
		s.Data = data
	}
	c.rv++
	s.Metadata.ResourceVersion = strconv.Itoa(c.rv)
	c.secrets[name] = s
	if c.onSecretPatched != nil {
		c.onSecretPatched(c, s)
	}
	if c.loseSecretPatches > 0 {
		c.loseSecretPatches--
		rtAnswer(w, rtUnavailable)
		return
	}
	rtJSON(w, 200, s)
}

func (c *rtCluster) deleteSecret(w http.ResponseWriter, name string, q map[string][]string, body []byte) {
	c.wantPropagation(q, "secret "+name, "Background")
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.secrets[name]; !ok {
		rtAnswer(w, kubeStatus(404, "NotFound", fmt.Sprintf(`secrets %q not found`, name)))
		return
	}
	if len(body) > 0 {
		var opts struct {
			Preconditions struct{ UID, ResourceVersion string }
		}
		if err := json.Unmarshal(body, &opts); err != nil {
			c.t.Error(err)
			rtAnswer(w, kubeStatus(400, "BadRequest", "invalid delete options"))
			return
		}
		meta := c.secrets[name].Metadata
		if opts.Preconditions.UID != meta.UID || opts.Preconditions.ResourceVersion != meta.ResourceVersion {
			rtAnswer(w, kubeStatus(409, "Conflict", "delete preconditions changed"))
			return
		}
	}
	c.deletedSecrets = append(c.deletedSecrets, c.secrets[name].Metadata)
	delete(c.secrets, name)
	rtJSON(w, 200, map[string]any{"kind": "Status", "status": "Success"})
}

func (c *rtCluster) deleteCollection(w http.ResponseWriter, selector string, jobs bool) {
	value, ok := strings.CutPrefix(selector, LabelRun+"=")
	if !ok {
		c.t.Errorf("a collection deleted by %q, not by the run label: that would delete other runs' objects", selector)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	items := []any{}
	if jobs {
		for name, j := range c.jobs {
			if j.job.Metadata.Labels[LabelRun] == value {
				items = append(items, j.job)
				c.deleteJobLocked(name)
			}
		}
	} else {
		for name, s := range c.secrets {
			if s.Metadata.Labels[LabelRun] == value {
				items = append(items, s)
				delete(c.secrets, name)
			}
		}
	}
	rtJSON(w, 200, map[string]any{"kind": "List", "items": items})
}

func (c *rtCluster) listPods(w http.ResponseWriter, selector string) {
	name, ok := strings.CutPrefix(selector, "job-name=")
	if !ok {
		c.t.Errorf("pods listed by %q, not by the Job's job-name label", selector)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.podsAnswer != nil {
		rtAnswer(w, *c.podsAnswer)
		return
	}
	items := []Pod{}
	if j := c.jobs[name]; j != nil && j.script != nil && len(j.script.states) > 0 {
		st := j.script.states[j.state]
		if st.leftover != nil {
			items = append(items, rtOwnedBy(*st.leftover, rtLeftoverUID))
		}
		if st.pod != nil {
			items = append(items, rtOwnedBy(*st.pod, j.job.Metadata.UID))
			if cs := rtStepState(st.pod); cs != nil && cs.State.Terminated != nil && !j.endSeen {
				j.endSeen = true
				c.bumpLocked()
			}
		}
		for _, p := range st.pods {
			if p != nil {
				items = append(items, rtOwnedBy(*p, j.job.Metadata.UID))
			}
		}
		j.reads++
		if j.state < len(j.script.states)-1 && j.reads >= max(st.reads, 1) && (st.until == nil || st.until(c)) {
			j.state++
			j.reads = 0
			c.bumpLocked()
		}
	}
	for i := range items {
		p := &items[i]
		if meta, ok := c.probeRoles[p.Metadata.UID]; ok {
			p.Spec.SchedulingGates = nil
			p.Metadata.ResourceVersion = meta.ResourceVersion
			p.Metadata.Labels[LabelProbeRole] = meta.Labels[LabelProbeRole]
		}
		c.observedPods[p.Metadata.Name] = *p
	}
	rtJSON(w, 200, PodList{Items: items})
}

func (c *rtCluster) patchPod(w http.ResponseWriter, name string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.podPatchAnswer != nil {
		rtAnswer(w, *c.podPatchAnswer)
		return
	}
	var patch struct {
		Metadata ObjectMeta `json:"metadata"`
	}
	if err := json.Unmarshal(body, &patch); err != nil {
		c.t.Fatal(err)
	}
	pod, ok := c.observedPods[name]
	if !ok || patch.Metadata.UID != pod.Metadata.UID || patch.Metadata.ResourceVersion != pod.Metadata.ResourceVersion {
		rtAnswer(w, kubeStatus(409, "Conflict", "pod incarnation changed"))
		return
	}
	if len(patch.Metadata.Labels) != 1 || patch.Metadata.Labels[LabelProbeRole] != probeRole(&pod) {
		c.t.Errorf("unexpected pod patch: %s", body)
	}
	patch.Metadata.ResourceVersion = pod.Metadata.ResourceVersion + "1"
	c.probeRoles[pod.Metadata.UID] = patch.Metadata
	rtJSON(w, 200, map[string]any{})
}

// rtOwnedBy is a pod as the Job controller labels it: its Job's uid on the
// controller-uid labels and on its controller owner reference.
func rtOwnedBy(p Pod, uid string) Pod {
	labels := make(map[string]string, len(p.Metadata.Labels)+2)
	for k, v := range p.Metadata.Labels {
		labels[k] = v
	}
	labels["controller-uid"] = uid
	labels["batch.kubernetes.io/controller-uid"] = uid
	p.Metadata.Labels = labels
	p.Metadata.OwnerReferences = []OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: p.Metadata.Labels["job-name"], UID: uid, Controller: ptrTo(true), BlockOwnerDeletion: ptrTo(true)}}
	return p
}

func rtStepState(p *Pod) *ContainerStatus {
	if p == nil {
		return nil
	}
	for i := range p.Status.ContainerStatuses {
		if p.Status.ContainerStatuses[i].Name == ContainerStep {
			return &p.Status.ContainerStatuses[i]
		}
	}
	return nil
}

// rtLineStamp reads a log line's timestamp, independently of the code under
// test; ok is false for a line the kubelet wrote into the stream itself.
func rtLineStamp(line string) (time.Time, bool) {
	stamp, _, _ := strings.Cut(line, " ")
	at, err := time.Parse(time.RFC3339Nano, stamp)
	return at, err == nil
}

func (c *rtCluster) podLog(w http.ResponseWriter, r *http.Request, pod string) {
	q := r.URL.Query()
	container := q.Get("container")
	c.mu.Lock()
	var j *rtJob
	var jobName string
	for name, cand := range c.jobs {
		if rtPodName(name) == pod {
			j, jobName = cand, name
		}
		if cand.script == nil {
			continue
		}
		// An Indexed Job's pods are found by their own names.
		if len(cand.script.states) > 0 {
			for _, p := range cand.script.states[cand.state].pods {
				if p != nil && p.Metadata.Name == pod {
					j, jobName = cand, name
				}
			}
		}
		for _, st := range cand.script.states {
			if st.leftover != nil && st.leftover.Metadata.Name == pod {
				c.t.Errorf("the runner read the log of %s, a leftover pod of an earlier Job", pod)
			}
		}
	}
	if j == nil || j.script == nil {
		c.mu.Unlock()
		rtAnswer(w, kubeStatus(404, "NotFound", fmt.Sprintf(`pods %q not found`, pod)))
		return
	}
	if q.Get("follow") != "true" {
		text, ok := j.script.tails[container]
		block, answer := c.blockTails, c.tailAnswer
		c.mu.Unlock()
		if answer != nil {
			rtAnswer(w, *answer)
			return
		}
		if block {
			select {
			case <-r.Context().Done():
			case <-c.closing:
			}
			return
		}
		if !ok {
			rtAnswer(w, rtNotStarted(container, pod))
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, text)
		return
	}
	if container != ContainerStep {
		c.t.Errorf("the runner followed container %q; only the step's output is followed", container)
	}
	if q.Get("timestamps") != "true" {
		c.t.Errorf("the runner followed the log without timestamps: it has no cursor to resume from")
	}
	cs := rtStepState(j.script.states[j.state].pod)
	if cs == nil || (cs.State.Running == nil && cs.State.Terminated == nil) {
		c.mu.Unlock()
		rtAnswer(w, rtNotStarted(container, pod))
		return
	}
	withFinal := cs.State.Terminated != nil
	var since time.Time
	if s := q.Get("sinceTime"); s != "" {
		var err error
		if since, err = time.Parse(time.RFC3339, s); err != nil {
			c.t.Errorf("sinceTime %q is not RFC3339: %v", s, err)
		}
	}
	c.streams++
	drop, cut := 0, ""
	if c.streams == 1 {
		drop, cut = j.script.dropAfter, j.script.cutFirstStream
	}
	noEOL := withFinal && j.script.finalNoEOL
	c.mu.Unlock()

	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	flusher := w.(http.Flusher)
	next, sent := 0, 0
	for {
		c.mu.Lock()
		gone := c.jobs[jobName] != j
		st := j.script.states[j.state]
		lines := j.script.log[:min(st.visible, len(j.script.log))]
		if withFinal {
			lines = append(append([]string(nil), lines...), j.script.final...)
		}
		cs := rtStepState(st.pod)
		running := cs != nil && cs.State.Running != nil
		ended := !running && (j.endSeen || cs == nil || cs.State.Terminated == nil)
		changed := c.changed
		c.mu.Unlock()
		if gone {
			return
		}
		for ; next < len(lines); next++ {
			if at, ok := rtLineStamp(lines[next]); ok && !since.IsZero() && at.Before(since) {
				continue
			}
			eol := "\n"
			if noEOL && next == len(lines)-1 {
				eol = ""
			}
			_, _ = io.WriteString(w, lines[next]+eol)
			sent++
			if drop > 0 && sent >= drop {
				flusher.Flush()
				return
			}
		}
		if cut != "" {
			_, _ = io.WriteString(w, cut)
			flusher.Flush()
			return
		}
		flusher.Flush()
		if withFinal || ended {
			return
		}
		select {
		case <-changed:
		case <-r.Context().Done():
			return
		case <-c.closing:
			return
		}
	}
}

// deleteJobLocked removes a Job and wakes whoever follows its pod's log.
func (c *rtCluster) deleteJobLocked(name string) {
	delete(c.jobs, name)
	c.bumpLocked()
}

// annotateLocked writes an annotation as another replica would, bumping the
// Job's resourceVersion.
func (c *rtCluster) annotateLocked(name, key, value string) {
	j := c.jobs[name]
	if j == nil {
		c.t.Errorf("annotating Job %s, which is not there", name)
		return
	}
	if j.job.Metadata.Annotations == nil {
		j.job.Metadata.Annotations = map[string]string{}
	}
	j.job.Metadata.Annotations[key] = value
	c.rv++
	j.job.Metadata.ResourceVersion = strconv.Itoa(c.rv)
	c.bumpLocked()
}

// cursorPatchedLocked says a heartbeat carried the cursor at.
func (c *rtCluster) cursorPatchedLocked(name string, at time.Time) bool {
	for _, p := range c.patches {
		if v, ok := p.get(AnnotLogCursor); ok && p.job == name && v == at.Format(time.RFC3339Nano) {
			return true
		}
	}
	return false
}

func (c *rtCluster) with(fn func(c *rtCluster)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn(c)
}

func (c *rtCluster) script(name string, s *rtScript) {
	c.with(func(c *rtCluster) { c.scripts[name] = s })
}

// putJob stores a Job another replica created.
func (c *rtCluster) putJob(job Job, s *rtScript) {
	c.with(func(c *rtCluster) {
		c.uid++
		c.rv++
		job.Metadata.UID = fmt.Sprintf("uid-%d", c.uid)
		job.Metadata.ResourceVersion = strconv.Itoa(c.rv)
		job.Metadata.CreationTimestamp = rtT0.Add(-time.Minute)
		c.jobs[job.Metadata.Name] = &rtJob{job: job, script: s, createdRV: job.Metadata.ResourceVersion}
	})
}

func (c *rtCluster) putSecret(s Secret) {
	c.with(func(c *rtCluster) {
		c.rv++
		s.Metadata.ResourceVersion = strconv.Itoa(c.rv)
		if s.Metadata.UID == "" {
			s.Metadata.UID = "uid-" + s.Metadata.Name
		}
		c.secrets[s.Metadata.Name] = s
	})
}

func (c *rtCluster) requests() []rtReq {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]rtReq(nil), c.reqs...)
}

func (c *rtCluster) requestsFor(method, path string) []rtReq {
	var out []rtReq
	for _, r := range c.requests() {
		if r.Method == method && r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

func (c *rtCluster) summary() string {
	var parts []string
	for _, r := range c.requests() {
		parts = append(parts, r.String())
	}
	return strings.Join(parts, "\n  ")
}

func (c *rtCluster) hasJob(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.jobs[name] != nil
}

func (c *rtCluster) hasSecret(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.secrets[name]
	return ok
}

func (c *rtCluster) jobNow(t *testing.T, name string) Job {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	j := c.jobs[name]
	if j == nil {
		t.Fatalf("Job %s is not in the fake", name)
	}
	return c.viewLocked(j)
}

func (c *rtCluster) secretNow(t *testing.T, name string) Secret {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.secrets[name]
	if !ok {
		t.Fatalf("Secret %s is not in the fake", name)
	}
	return s
}

func (c *rtCluster) appliedPatches() []rtPatch {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]rtPatch(nil), c.patches...)
}

func (c *rtCluster) streamCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.streams
}

// ---------------------------------------------------------------------------
// The runner's other dependencies, and the harness
// ---------------------------------------------------------------------------

type rtClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *rtClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *rtClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

// rtLibrary is the owner's Library: it records every file and answers an id,
// Omitted or an error, by the file's name.
type rtLibrary struct {
	mu    sync.Mutex
	files []RunFile
	omit  map[string]string
	fail  map[string]error
	// block makes every store wait for its context to end, and fail with it:
	// a Library that does not answer.
	block bool
	// idPad lengthens every file id it answers.
	idPad      string
	nextIntent uint64
}

func (l *rtLibrary) StoreRunFile(ctx context.Context, f RunFile) (StoredFile, error) {
	l.mu.Lock()
	block := l.block
	l.mu.Unlock()
	if block {
		<-ctx.Done()
		return StoredFile{}, ctx.Err()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.files = append(l.files, f)
	if reason, ok := l.omit[f.Name]; ok {
		return StoredFile{Omitted: reason}, nil
	}
	if err, ok := l.fail[f.Name]; ok {
		return StoredFile{}, err
	}
	return StoredFile{FileID: fmt.Sprintf("file-%d", len(l.files)) + l.idPad}, nil
}

func (l *rtLibrary) stored() []RunFile {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]RunFile(nil), l.files...)
}

// rtTokens mints the clone token: token, or the next of seq while it lasts.
type rtTokens struct {
	mu    sync.Mutex
	token string
	seq   []string
	err   error
	calls []string
}

func (m *rtTokens) CloneToken(_ context.Context, installationID int64, owner, name string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, fmt.Sprintf("%d %s/%s", installationID, owner, name))
	if len(m.seq) > 0 {
		token := m.seq[0]
		m.seq = m.seq[1:]
		return token, m.err
	}
	return m.token, m.err
}

func (m *rtTokens) called() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

// rtLogs records what a Runner logs.
type rtLogs struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *rtLogs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *rtLogs) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// count is how many records say msg.
func (l *rtLogs) count(msg string) int {
	return strings.Count(l.String(), "msg="+strconv.Quote(msg)+" ")
}

type rtHarness struct {
	r      *Runner
	c      *rtCluster
	lib    *rtLibrary
	tokens *rtTokens
	sink   *captureTestSink
	clock  *rtClock
	dir    string
	cfg    Config
}

func newRunnerHarness(t *testing.T, mutate ...func(*Config)) *rtHarness {
	t.Helper()
	clock := &rtClock{at: rtT0}
	cfg := rtConfig()
	for _, m := range mutate {
		m(&cfg)
	}
	h := &rtHarness{
		c: newRTCluster(t, clock), lib: &rtLibrary{}, tokens: &rtTokens{token: rtCloneToken},
		sink: &captureTestSink{}, clock: clock, dir: t.TempDir(), cfg: cfg,
	}
	h.r = h.newRunner(cfg)
	return h
}

// logs makes the harness's Runner log into a recorder.
func (h *rtHarness) logs() *rtLogs {
	l := &rtLogs{}
	h.r.log = slog.New(slog.NewTextHandler(l, nil)).With("component", "pipelines.runner")
	return l
}

// newRunner builds a Runner over the fake, with the test's clock, a private
// store bucket (the node's is process-wide) and a directory the test can
// check is left empty.
//
// It is a replica that has proved memql-pipelines isolated already, a pass
// stamped at the test's clock: a step's own mechanics are these tests', and
// the proof (isolation.go) is isolation_test.go's, whose harness takes the
// pass away (newIsoHarness). A pass lasts IsolationTTL, an hour, and no test
// moves the clock that far before a step's create.
func (h *rtHarness) newRunner(cfg Config) *Runner {
	r := NewRunner(cfg, h.c.kube, func() LineSink { return h.sink }, h.lib, h.tokens)
	r.now = h.clock.Now
	r.tempDir = h.dir
	r.openCapture = func(o CaptureOptions) (*Capture, error) {
		return newCapture(o, h.clock.Now, newCaptureBucket(1<<20, h.clock.Now()))
	}
	r.isoLast = IsolationVerdict{Isolated: true, Detail: "proved before the test began", At: h.clock.Now()}
	return r
}

// start runs a step in the background.
func (h *rtHarness) start(ctx context.Context, run StepRun) <-chan pl.StepResult {
	done := make(chan pl.StepResult, 1)
	go func() { done <- h.r.Run(ctx, run) }()
	return done
}

// await waits for a started step, failing the test if it never answers.
func (h *rtHarness) await(t *testing.T, done <-chan pl.StepResult) pl.StepResult {
	t.Helper()
	select {
	case res := <-done:
		return res
	case <-time.After(20 * time.Second):
		t.Fatalf("Run did not answer; the fake saw:\n  %s", h.c.summary())
	}
	return pl.StepResult{}
}

func (h *rtHarness) run(t *testing.T, run StepRun) pl.StepResult {
	t.Helper()
	return h.await(t, h.start(context.Background(), run))
}

// existingJob is the step's Job as the replica that created it built it.
func (h *rtHarness) existingJob(t *testing.T, run StepRun, annots map[string]string) Job {
	t.Helper()
	job, err := BuildJob(h.cfg, run, JobName(run.RunID, run.StepKey, run.Attempt))
	if err != nil {
		t.Fatalf("BuildJob: %v", err)
	}
	for k, v := range annots {
		job.Metadata.Annotations[k] = v
	}
	return job
}

// file is the one Library file of that name.
func (h *rtHarness) file(t *testing.T, name string) RunFile {
	t.Helper()
	var found []RunFile
	for _, f := range h.lib.stored() {
		if f.Name == name {
			found = append(found, f)
		}
	}
	if len(found) != 1 {
		var names []string
		for _, f := range h.lib.stored() {
			names = append(names, f.Name)
		}
		t.Fatalf("the Library holds %d files named %q (all: %q), want exactly one", len(found), name, names)
	}
	return found[0]
}

// leftNoArchive checks the runner removed every log archive it wrote.
func (h *rtHarness) leftNoArchive(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		t.Fatalf("read the archive directory: %v", err)
	}
	for _, e := range entries {
		t.Errorf("the runner left %s behind in its archive directory", e.Name())
	}
}

func rtWaitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// rtTexts are log lines without their timestamps: what the store and the
// archive keep of them.
func rtTexts(lines ...string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		_, text, _ := strings.Cut(l, " ")
		out = append(out, text)
	}
	return out
}

func rtNoRequests(t *testing.T, c *rtCluster, method, path string) {
	t.Helper()
	if got := c.requestsFor(method, path); len(got) > 0 {
		t.Errorf("%d %s %s requests; want none", len(got), method, path)
	}
}

func rtWantCode(t *testing.T, res pl.StepResult, status pl.Outcome, code string) {
	t.Helper()
	if res.Status != status || res.ExitCode != -1 || res.Failure == nil || res.Failure.Code != code || res.Failure.Message == "" {
		t.Fatalf("result = %+v (failure %+v), want %s, exit -1, a %s failure with a sentence", res, res.Failure, status, code)
	}
}

const rtJobPath = kubeJobs + "/" + testJobName

// ---------------------------------------------------------------------------
// A fresh step
// ---------------------------------------------------------------------------

// TestRunnerRunsAStepToSuccessAndArchivesItsLog: a step no replica has
// started. The runner mints the clone token, creates the Secret and then the
// Job, makes the Job the Secret's owner, claims the Job, follows the step's
// log into the store while it heartbeats its claim and its cursor, archives
// the log with the clone's last words to the owner's Library, and persists
// the outcome on the Job before it answers.
func TestRunnerRunsAStepToSuccessAndArchivesItsLog(t *testing.T) {
	h := newRunnerHarness(t)
	run := rtRun()
	run.Services = map[string]pl.Service{"redis": {Image: "redis:7"}}
	l1 := captureKubeLine(rtAt(1100), "=== RUN   TestWidget")
	l2 := captureKubeLine(rtAt(2200), "--- PASS: TestWidget (0.01s)")
	l3 := captureKubeLine(rtAt(3300), "ok  \tacme/widget\t0.012s")
	running := rtPod(testJobName, rtStepRunning(rtAt(1000)), clsCloneDone)
	h.c.script(testJobName, &rtScript{
		states: []rtState{
			{},
			{pod: rtPod(testJobName, clsStepInit, rtCloneRunning)},
			{pod: running, visible: 2},
			// The step ends only once a heartbeat has carried its last
			// line's cursor: where a replica adopting it would resume.
			{pod: running, visible: 3, until: func(c *rtCluster) bool { return c.cursorPatchedLocked(testJobName, rtAt(3300)) }},
			{pod: rtPod(testJobName, rtStepEnded(0, rtAt(1000), rtAt(4000)), clsCloneDone), visible: 3},
		},
		log: []string{l1, l2, l3},
		tails: map[string]string{
			// A clone that printed its token -- a git trace left on, say.
			ContainerClone:          rtCloneTail + "trace: Authorization: Bearer " + rtCloneToken + "\n",
			ServicePrefix + "redis": "1:M ready to accept connections\n",
		},
	})

	res := h.run(t, run)

	want := pl.StepResult{
		Status:     pl.OutcomeSucceeded,
		ExitCode:   0,
		StartedAt:  "2026-10-04T09:00:01Z",
		FinishedAt: "2026-10-04T09:00:04Z",
		Where:      pl.Where{Surface: "cluster", NodeID: rtNode, JobName: testJobName},
		LogFileID:  "file-1",
		LogTail:    "=== RUN   TestWidget\n--- PASS: TestWidget (0.01s)\nok  \tacme/widget\t0.012s",
		LogLines:   3,
	}
	if !reflect.DeepEqual(res, want) {
		t.Fatalf("result:\n  got  %+v\n  want %+v", res, want)
	}

	// Created in this order: nothing before the read, the Secret before the
	// Job that references it, the owner once the Job has a uid, the claim on
	// the version the create answered.
	reqs := h.c.requests()
	// Reads may grow as additional safety gates are introduced. The mutation
	// order still requires credentials, then the creation CAS, then one Job.
	var mutations []string
	for _, request := range reqs {
		if request.Method != http.MethodGet {
			mutations = append(mutations, request.Method+" "+request.Path)
		}
	}
	wantMutations := []string{"POST " + kubeSecrets, "PATCH " + kubeSecrets + "/" + testSecretName, "POST " + kubeJobs}
	if len(mutations) < len(wantMutations) || !reflect.DeepEqual(mutations[:len(wantMutations)], wantMutations) {
		t.Fatalf("unsafe creation effect order: %v", mutations)
	}
	if got := h.tokens.called(); !reflect.DeepEqual(got, []string{"42 acme/widget"}) {
		t.Errorf("clone tokens minted for %q, want one for installation 42, acme/widget", got)
	}
	secret := h.c.secretNow(t, testSecretName)
	if string(secret.Data[gitTokenKey]) != rtCloneToken || string(secret.Data["NPM_TOKEN"]) != plantedNPM {
		t.Errorf("the Secret holds %q, want the minted clone token and the step's secrets", keysOf(secret.Data))
	}
	wantOwner := []OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: testJobName, UID: "uid-1", Controller: ptrTo(false), BlockOwnerDeletion: ptrTo(false)}}
	if !reflect.DeepEqual(secret.Metadata.OwnerReferences, wantOwner) {
		t.Errorf("the Secret's owners = %+v, want the Job, so collecting the Job collects the token", secret.Metadata.OwnerReferences)
	}

	// The create answered version 3, and the Job controller's status write
	// made it 4 at once, as a real API server's does: the creator's first
	// claim is refused, and the one on the version it read again holds -- with
	// no word of a re-attach, since nobody held the Job before (the store
	// check above).
	var firstClaim struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(h.c.requestsFor(http.MethodPatch, rtJobPath)[0].Body), &firstClaim); err != nil || firstClaim.Metadata.ResourceVersion != "3" {
		t.Errorf("the first claim was conditioned on %q (%v), want the version the create answered, 3", firstClaim.Metadata.ResourceVersion, err)
	}
	patches := h.c.appliedPatches()
	if len(patches) == 0 {
		t.Fatal("the runner never patched its Job")
	}
	if claim, _ := patches[0].get(AnnotRunner); claim != rtStamp(rtNode, rtT0) || patches[0].rv != "4" {
		t.Errorf("first applied patch = runner %q on version %q, want the claim %q conditioned on the version read again, 4", claim, patches[0].rv, rtStamp(rtNode, rtT0))
	}
	for _, p := range patches {
		if _, ok := p.get(AnnotRunner); ok && p.rv == "" {
			t.Errorf("a claim was stamped unconditionally: it could overwrite a claim another replica made")
		}
	}

	job := h.c.jobNow(t, testJobName)
	if got := job.Metadata.Annotations[AnnotLogCursor]; got != rtAt(3300).Format(time.RFC3339Nano) {
		t.Errorf("log cursor on the Job = %q, want the last line's timestamp %s", got, rtAt(3300).Format(time.RFC3339Nano))
	}
	var persisted pl.StepResult
	if err := json.Unmarshal([]byte(job.Metadata.Annotations[AnnotOutcome]), &persisted); err != nil || !reflect.DeepEqual(persisted, want) {
		t.Errorf("the outcome persisted on the Job = %+v (%v), want the result Run answered", persisted, err)
	}

	logFile := h.file(t, "tests-go-tests-2.log")
	if logFile.OwnerUserID != "user-5d1e" || logFile.WorkRunID != "work-91c2" || logFile.StepKey != "tests/go-tests#2" || logFile.MimeType != "text/plain; charset=utf-8" {
		t.Errorf("log file = %+v, want the owner's, bound to the work run and the step, as UTF-8 text", logFile)
	}
	archive := string(logFile.Bytes)
	output := strings.Join(rtTexts(l1, l2, l3), "\n") + "\n"
	if !strings.HasPrefix(archive, output) || !strings.Contains(archive[len(output):], strings.TrimSpace(rtCloneTail)) {
		t.Errorf("archive = %q, want the step's output, then the clone's last words", archive)
	}
	if strings.Contains(archive, "ready to accept connections") {
		t.Errorf("a service's output was archived for a step that succeeded: the kubelet stops every service after every step")
	}
	if strings.Contains(archive, rtCloneToken) || !strings.Contains(archive, "Bearer ***") {
		t.Errorf("archive = %q, want the clone token masked like any secret", archive)
	}
	if got := h.sink.messages(); !reflect.DeepEqual(got, rtTexts(l1, l2, l3)) {
		t.Errorf("store = %q, want the step's output and nothing of the clone's", got)
	}
	tails := h.c.requestsFor(http.MethodGet, kubePods+"/"+rtPodName(testJobName)+"/log")
	var tailQueries []string
	for _, r := range tails {
		if !strings.Contains(r.Query, "follow=true") {
			tailQueries = append(tailQueries, r.Query)
		}
	}
	if !reflect.DeepEqual(tailQueries, []string{"container=clone&tailLines=200"}) {
		t.Errorf("tails asked = %q, want the clone's last 200 lines only", tailQueries)
	}
	h.leftNoArchive(t)
}

// TestRunnerReportsTheCommandExitCode: a command that fails is the step
// failing with the command's own exit code, no failure code -- and its
// services' last words are archived beside the clone's, never stored live.
func TestRunnerReportsTheCommandExitCode(t *testing.T) {
	t.Run("the command's exit code, with the clone's and the services' last words archived", func(t *testing.T) {
		h := newRunnerHarness(t)
		run := rtRun()
		run.Services = map[string]pl.Service{"redis": {Image: "redis:7"}, "postgres": {Image: "postgres:16"}}
		l1 := captureKubeLine(rtAt(1100), "--- FAIL: TestWidget (0.01s)")
		l2 := captureKubeLine(rtAt(1200), "FAIL\tacme/widget\t0.013s")
		script := rtFinishingScript(testJobName, 3, l1, l2)
		script.tails[ServicePrefix+"postgres"] = "LOG:  database system is ready to accept connections\nLOG:  received fast shutdown request\n"
		h.c.script(testJobName, script)

		res := h.run(t, run)

		if res.Status != pl.OutcomeFailed || res.ExitCode != 3 || res.Failure != nil {
			t.Fatalf("result = %+v (failure %+v), want failed with exit code 3 and no failure code: the exit code is the answer", res, res.Failure)
		}
		if res.LogTail != "--- FAIL: TestWidget (0.01s)\nFAIL\tacme/widget\t0.013s" {
			t.Errorf("tail = %q, want the step's last words", res.LogTail)
		}
		archive := string(h.file(t, "tests-go-tests-2.log").Bytes)
		output := strings.Join(rtTexts(l1, l2), "\n") + "\n"
		if !strings.HasPrefix(archive, output) {
			t.Fatalf("archive = %q, want the step's output first", archive)
		}
		rest := archive[len(output):]
		clone := strings.Index(rest, strings.TrimSpace(rtCloneTail))
		postgres := strings.Index(rest, "received fast shutdown request")
		if clone < 0 || postgres < clone || !strings.Contains(rest, "redis") {
			t.Errorf("archive after the output = %q, want the clone's last words, then postgres's, and a word on redis, which left none", rest)
		}
		if got := h.sink.messages(); !reflect.DeepEqual(got, rtTexts(l1, l2)) {
			t.Errorf("store = %q, want only the step's own output", got)
		}
		var tailQueries []string
		for _, r := range h.c.requestsFor(http.MethodGet, kubePods+"/"+rtPodName(testJobName)+"/log") {
			if !strings.Contains(r.Query, "follow=true") {
				tailQueries = append(tailQueries, r.Query)
			}
		}
		want := []string{"container=clone&tailLines=200", "container=svc-postgres&tailLines=50", "container=svc-redis&tailLines=50"}
		if !reflect.DeepEqual(tailQueries, want) {
			t.Errorf("tails asked = %q, want %q", tailQueries, want)
		}
		h.leftNoArchive(t)
	})

	t.Run("a step the cluster failed carries the classifier's code and no exit code", func(t *testing.T) {
		h := newRunnerHarness(t)
		pull := ContainerStatus{Name: ContainerStep, Image: "registry.example.com/acme/nope:1", State: ContainerState{Waiting: &ContainerStateWaiting{
			Reason: "ImagePullBackOff", Message: `Back-off pulling image "registry.example.com/acme/nope:1": not found`,
		}}}
		h.c.script(testJobName, &rtScript{
			states: []rtState{{pod: rtPod(testJobName, pull, clsCloneDone)}},
			tails:  map[string]string{ContainerClone: rtCloneTail},
		})

		res := h.run(t, rtRun())

		rtWantCode(t, res, pl.OutcomeFailed, pl.CodeImagePullFailed)
		if !strings.Contains(res.Failure.Message, "registry.example.com/acme/nope:1") {
			t.Errorf("failure %q does not name the image", res.Failure.Message)
		}
		if res.LogFileID == "" {
			t.Error("a step the cluster failed has no log in the Library: the clone's last words are in it")
		}
	})
}

// TestRunnerReturnsAPersistedOutcomeWithoutRerunning: the agent asks again
// for a step whose reply was lost. The outcome is on the Job, so the answer is
// that outcome, read and nothing else.
func TestRunnerReturnsAPersistedOutcomeWithoutRerunning(t *testing.T) {
	prior := pl.StepResult{
		Status: pl.OutcomeFailed, ExitCode: 3,
		StartedAt: "2026-10-04T08:50:01Z", FinishedAt: "2026-10-04T08:51:09Z",
		Where:     pl.Where{Surface: "cluster", NodeID: rtOther, JobName: testJobName},
		LogFileID: "file-9", LogTail: "FAIL\tacme/widget", LogLines: 12,
	}
	h := newRunnerHarness(t)
	h.c.putJob(h.existingJob(t, rtRun(), map[string]string{
		AnnotRunner:  rtStamp(rtOther, rtT0.Add(-time.Second)),
		AnnotOutcome: mustJSON(t, prior),
	}), rtRunningScript(testJobName))

	res := h.run(t, rtRun())

	if !reflect.DeepEqual(res, prior) {
		t.Errorf("result:\n  got  %+v\n  want the persisted outcome %+v", res, prior)
	}
	if got := h.c.summary(); got != "GET "+rtJobPath {
		t.Errorf("requests:\n  %s\nwant the one read of the Job", got)
	}
	if len(h.tokens.called()) != 0 || len(h.lib.stored()) != 0 {
		t.Error("answering a persisted outcome minted a token or stored a file")
	}

	t.Run("an outcome that cannot be read is an executor error, never a rerun", func(t *testing.T) {
		h := newRunnerHarness(t)
		h.c.putJob(h.existingJob(t, rtRun(), map[string]string{AnnotOutcome: "{not json"}), rtRunningScript(testJobName))

		res := h.run(t, rtRun())

		rtWantCode(t, res, pl.OutcomeFailed, pl.CodeExecutorError)
		if got := h.c.summary(); got != "GET "+rtJobPath {
			t.Errorf("requests:\n  %s\nwant the one read of the Job", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Several replicas
// ---------------------------------------------------------------------------

// TestRunnerWaitsWhileAnotherReplicaHoldsAFreshHeartbeat: a Job whose claim
// is fresh belongs to the runner that holds it, wherever that is. Asked for
// the step, the runner waits for that runner's outcome: it reads the Job and
// does nothing else.
func TestRunnerWaitsWhileAnotherReplicaHoldsAFreshHeartbeat(t *testing.T) {
	outcome := pl.StepResult{
		Status: pl.OutcomeSucceeded, Where: pl.Where{Surface: "cluster", NodeID: rtOther, JobName: testJobName},
		LogFileID: "file-7", LogLines: 4,
	}
	onlyReads := func(t *testing.T, c *rtCluster) {
		t.Helper()
		for _, r := range c.requests() {
			if r.Method != http.MethodGet || r.Path != rtJobPath {
				t.Errorf("a waiting runner sent %s", r)
			}
		}
	}
	// A fresh claim this replica's own LIVE Run holds is waited on too: that
	// is TestTwoRunsOfOneReplicaNeverBothOwnAStep.
	t.Run("another replica's fresh claim: it waits for the outcome", func(t *testing.T) {
		h := newRunnerHarness(t)
		h.c.putJob(h.existingJob(t, rtRun(), map[string]string{AnnotRunner: rtStamp(rtOther, rtT0.Add(-10*time.Second))}), rtRunningScript(testJobName))
		h.c.onJobGet = func(c *rtCluster, n int) {
			if n == 4 {
				c.annotateLocked(testJobName, AnnotOutcome, mustJSON(t, outcome))
			}
		}

		res := h.run(t, rtRun())

		if !reflect.DeepEqual(res, outcome) {
			t.Errorf("result = %+v, want the holder's outcome %+v", res, outcome)
		}
		onlyReads(t, h.c)
		if len(h.lib.stored()) != 0 || len(h.tokens.called()) != 0 {
			t.Error("a waiting runner stored a file or minted a token")
		}
	})

	t.Run("a fresh claim of this replica with no Run of it alive: a previous incarnation's, adopted at once", func(t *testing.T) {
		// The workbench container restarted in its pod: same node id, and
		// nothing in this process holds the step. Waiting would only wait
		// out a heartbeat nobody will stamp again.
		h := newRunnerHarness(t)
		h.c.putJob(h.existingJob(t, rtRun(), map[string]string{AnnotRunner: rtStamp(rtNode, rtT0.Add(-10*time.Second))}),
			rtFinishingScript(testJobName, 0, captureKubeLine(rtAt(1100), "ok")))

		res := h.run(t, rtRun())

		if res.Status != pl.OutcomeSucceeded || res.Where.NodeID != rtNode || res.LogLines != 1 {
			t.Fatalf("result = %+v, want the step adopted and run to success here", res)
		}
		if store := h.sink.messages(); len(store) != 2 || !strings.Contains(store[0], "re-attached on "+rtNode) {
			t.Errorf("store = %q, want the re-attach notice and the step's line", store)
		}
	})

	t.Run("the Job vanishes while it waits: the step's node is lost", func(t *testing.T) {
		h := newRunnerHarness(t)
		h.c.putJob(h.existingJob(t, rtRun(), map[string]string{AnnotRunner: rtStamp(rtOther, rtT0.Add(-10*time.Second))}), rtRunningScript(testJobName))
		h.c.onJobGet = func(c *rtCluster, n int) {
			if n == 3 {
				c.deleteJobLocked(testJobName)
			}
		}

		res := h.run(t, rtRun())

		rtWantCode(t, res, pl.OutcomeFailed, pl.CodeNodeLost)
		onlyReads(t, h.c)
	})

	t.Run("the holder goes stale while it waits: it adopts the Job", func(t *testing.T) {
		h := newRunnerHarness(t)
		line := captureKubeLine(rtAt(1100), "ok")
		h.c.putJob(h.existingJob(t, rtRun(), map[string]string{AnnotRunner: rtStamp(rtOther, rtT0.Add(-10*time.Second))}), rtFinishingScript(testJobName, 0, line))
		h.c.onJobGet = func(c *rtCluster, n int) {
			if n == 3 {
				h.clock.Advance(time.Minute) // workbench-a stops heartbeating
			}
		}

		res := h.run(t, rtRun())

		if res.Status != pl.OutcomeSucceeded || res.Where.NodeID != rtNode || res.LogLines != 1 {
			t.Fatalf("result = %+v, want the step run to success here, by %s", res, rtNode)
		}
		rtNoRequests(t, h.c, http.MethodPost, kubeJobs)
		claims := 0
		for _, p := range h.c.appliedPatches() {
			if v, _ := p.get(AnnotRunner); v == rtStamp(rtNode, rtT0.Add(time.Minute)) && p.rv != "" {
				claims++
			}
		}
		if claims == 0 {
			t.Error("the runner never claimed the Job it adopted")
		}
	})
}

// TestRunnerAdoptsAJobWhoseRunnerWentStale (Review Focus 2, ruling R36):
// workbench-a followed the step to a cursor, then went quiet -- a deploy
// restarted it. The step's Job is still running, so this replica adopts it: it
// claims it on the version it read and creates nothing. The store has every
// line up to the cursor, so it gets only the lines after it, behind a notice
// saying this replica re-attached. The archive is complete: the step's log is
// followed from its start, the lines up to the cursor replayed into the
// archive only, and the same notice marks the seam.
func TestRunnerAdoptsAJobWhoseRunnerWentStale(t *testing.T) {
	h := newRunnerHarness(t)
	run := rtRun()
	cursor := rtAt(2500)
	l0 := captureKubeLine(rtAt(1100), "line 0, an earlier second")
	l1 := captureKubeLine(rtAt(2100), "line 1, captured by workbench-a")
	l2 := captureKubeLine(cursor, "line 2, the cursor")
	l3 := captureKubeLine(rtAt(2700), "line 3")
	l4 := captureKubeLine(rtAt(3100), "line 4")
	h.c.putJob(h.existingJob(t, run, map[string]string{
		AnnotRunner:    rtStamp(rtOther, rtT0.Add(-time.Minute)),
		AnnotLogCursor: cursor.Format(time.RFC3339Nano),
		AnnotLogFirst:  rtAt(1100).Format(time.RFC3339Nano),
	}), rtFinishingScript(testJobName, 0, l0, l1, l2, l3, l4))
	h.c.putSecret(BuildSecret(h.cfg, run, testJobName, rtCloneToken))

	res := h.run(t, run)

	if res.Status != pl.OutcomeSucceeded || res.ExitCode != 0 || res.Where.NodeID != rtNode || res.LogLines != 5 {
		t.Fatalf("result = %+v, want success with all five lines in the archive, by %s", res, rtNode)
	}
	rtNoRequests(t, h.c, http.MethodPost, kubeJobs)
	rtNoRequests(t, h.c, http.MethodPost, kubeSecrets)
	if len(h.tokens.called()) != 0 {
		t.Error("an adopter minted a clone token: the Job it adopts has its Secret")
	}
	patches := h.c.appliedPatches()
	if claim, _ := patches[0].get(AnnotRunner); claim != rtStamp(rtNode, rtT0) || patches[0].rv != "1" {
		t.Errorf("first patch = runner %q on version %q, want the claim conditioned on the version read, 1", claim, patches[0].rv)
	}
	// Its creator may have gone before it made the Job the Secret's owner;
	// owned, the Secret goes with the Job whoever collects it.
	if owners := h.c.secretNow(t, testSecretName).Metadata.OwnerReferences; len(owners) != 1 || owners[0].UID != "uid-1" {
		t.Errorf("the Secret's owners = %+v, want the adopted Job", owners)
	}
	var follows []string
	for _, r := range h.c.requestsFor(http.MethodGet, kubePods+"/"+rtPodName(testJobName)+"/log") {
		if strings.Contains(r.Query, "follow=true") {
			follows = append(follows, r.Query)
		}
	}
	if len(follows) == 0 || follows[0] != "container=step&follow=true&timestamps=true" {
		t.Errorf("follows = %q, want the first from the log's start: the archive is made whole", follows)
	}

	store := h.sink.messages()
	if len(store) != 3 {
		t.Fatalf("store = %q, want the re-attach notice and the two lines after the cursor", store)
	}
	notice := store[0]
	for _, w := range []string{"re-attached on " + rtNode, cursor.Format(time.RFC3339Nano), "in the store already", "replayed from the node's log, from the step's first line"} {
		if !strings.Contains(notice, w) {
			t.Errorf("notice %q does not say %q", notice, w)
		}
	}
	if !reflect.DeepEqual(store[1:], rtTexts(l3, l4)) {
		t.Errorf("store after the notice = %q, want lines 3 and 4 only: lines 0 to 2 are in the store already", store[1:])
	}
	archive := string(h.file(t, "tests-go-tests-2.log").Bytes)
	want := strings.Join(rtTexts(l0, l1, l2), "\n") + "\n" + notice + "\n" + strings.Join(rtTexts(l3, l4), "\n") + "\n"
	if !strings.HasPrefix(archive, want) {
		t.Errorf("archive = %q, want it to begin %q: whole, the notice at the seam", archive, want)
	}
	if res.LogTail != strings.Join(rtTexts(l0, l1, l2, l3, l4), "\n") {
		t.Errorf("tail = %q, want the step's lines, not the runner's notice", res.LogTail)
	}
	for _, p := range h.c.appliedPatches() {
		if v, ok := p.get(AnnotLogFirst); ok {
			t.Errorf("the adopter wrote the step's first line (%q): only a Run that followed the step from its start may", v)
		}
	}
}

// TestRunnerAdoptionArchiveStartsWhereTheNodesLogDoes (ruling R36): the
// kubelet serves a container's current log file only, so once the log has
// rotated, the lines before the cursor are gone from the node. The archive
// then starts where the node's log does, and says so -- and so does the note
// on a capped live log, which may not claim the Library's copy is complete.
func TestRunnerAdoptionArchiveStartsWhereTheNodesLogDoes(t *testing.T) {
	h := newRunnerHarness(t, func(c *Config) { c.LogStoreMaxLines = 2 })
	run := rtRun()
	cursor := rtAt(2500)
	after := []string{captureKubeLine(rtAt(3100), "a"), captureKubeLine(rtAt(3200), "b"), captureKubeLine(rtAt(3300), "c")}
	h.c.putJob(h.existingJob(t, run, map[string]string{
		AnnotRunner:    rtStamp(rtOther, rtT0.Add(-time.Minute)),
		AnnotLogCursor: cursor.Format(time.RFC3339Nano),
	}), rtFinishingScript(testJobName, 0, after...))

	res := h.run(t, run)

	if res.Status != pl.OutcomeSucceeded || res.LogLines != 3 {
		t.Fatalf("result = %+v, want success with the three lines the node still has", res)
	}
	archive := string(h.file(t, "tests-go-tests-2.log").Bytes)
	first, _, _ := strings.Cut(archive, "\n")
	if !strings.Contains(first, "re-attached on "+rtNode) || !strings.Contains(first, "starts where the node's log does") {
		t.Errorf("archive begins %q, want the notice saying it starts where the node's log does", first)
	}
	if !res.LogCapped {
		t.Fatalf("LogCapped false with 3 lines past a 2-line cap")
	}
	for _, n := range res.Notes {
		if n.Code == pl.CodeLogCapped && strings.Contains(n.Message, "complete log") {
			t.Errorf("note %q claims the Library holds the complete log; its archive starts where the node's log did", n.Message)
		}
	}
}

// TestRunnerSaysWhatAnAdoptersArchiveHolds (ruling R36, fix round 2): the
// kubelet serves only a container's current log file, so after a rotation an
// adopter replays the step's output only as far back as that file reaches.
// The Job records the step's first line (AnnotLogFirst); the seam notice and
// a capped live log's note say the archive is complete only when the replay
// began there, say the head may be missing when it began later, and claim
// nothing about the start when the Job records no first line.
func TestRunnerSaysWhatAnAdoptersArchiveHolds(t *testing.T) {
	cursor := rtAt(2500)
	l0 := captureKubeLine(rtAt(1100), "line 0")
	l1 := captureKubeLine(rtAt(2100), "line 1")
	l2 := captureKubeLine(cursor, "line 2, the cursor")
	after := []string{captureKubeLine(rtAt(3100), "a"), captureKubeLine(rtAt(3200), "b"), captureKubeLine(rtAt(3300), "c")}
	for _, c := range []struct {
		name       string
		first      string   // AnnotLogFirst on the Job; "" for none
		log        []string // what the node's log still serves
		notice     []string // what the seam notice says
		notNotice  string   // and must not say
		capped     string   // what the capped log's note says
		notCapped  string
		archiveTop string // the archive's first line
	}{
		{
			name: "the whole head replayed", first: rtAt(1100).Format(time.RFC3339Nano),
			log:        append([]string{l0, l1, l2}, after...),
			notice:     []string{"from the step's first line"},
			notNotice:  "may be missing",
			capped:     "the complete log is archived",
			archiveTop: "line 0",
		},
		{
			// The re-review's probe: line 0 was rotated away on the node.
			name: "the head rotated away", first: rtAt(1100).Format(time.RFC3339Nano),
			log:        append([]string{l1, l2}, after...),
			notice:     []string{"as far back as it still held the step's output", "from " + rtAt(2100).Format(time.RFC3339Nano), "after the step's first line at " + rtAt(1100).Format(time.RFC3339Nano), "the head may be missing"},
			notNotice:  "from the step's first line",
			capped:     "holds what the node's log still held of the step's output when this replica re-attached to the step; its head may be missing",
			notCapped:  "complete log",
			archiveTop: "line 1",
		},
		{
			name: "no first line on the Job", first: "",
			log:        append([]string{l1, l2}, after...),
			notice:     []string{"as far back as it still held the step's output"},
			notNotice:  "first line",
			capped:     "holds what the node's log still held of the step's output when this replica re-attached to the step",
			notCapped:  "complete log",
			archiveTop: "line 1",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newRunnerHarness(t, func(c *Config) { c.LogStoreMaxLines = 2 })
			run := rtRun()
			annots := map[string]string{AnnotRunner: rtStamp(rtOther, rtT0.Add(-time.Minute)), AnnotLogCursor: cursor.Format(time.RFC3339Nano)}
			if c.first != "" {
				annots[AnnotLogFirst] = c.first
			}
			h.c.putJob(h.existingJob(t, run, annots), rtFinishingScript(testJobName, 0, c.log...))

			res := h.run(t, run)

			if res.Status != pl.OutcomeSucceeded || !res.LogCapped {
				t.Fatalf("result = %+v, want success with the live log capped", res)
			}
			store := h.sink.messages()
			if len(store) == 0 || !strings.Contains(store[0], "re-attached on "+rtNode) {
				t.Fatalf("store = %q, want the seam notice first", store)
			}
			for _, w := range c.notice {
				if !strings.Contains(store[0], w) {
					t.Errorf("notice %q does not say %q", store[0], w)
				}
			}
			if strings.Contains(store[0], c.notNotice) {
				t.Errorf("notice %q says %q, which is not so", store[0], c.notNotice)
			}
			var capped string
			for _, n := range res.Notes {
				if n.Code == pl.CodeLogCapped {
					capped = n.Message
				}
			}
			if !strings.Contains(capped, c.capped) || (c.notCapped != "" && strings.Contains(capped, c.notCapped)) {
				t.Errorf("capped note %q, want it to say %q and not %q", capped, c.capped, c.notCapped)
			}
			if top, _, _ := strings.Cut(string(h.file(t, "tests-go-tests-2.log").Bytes), "\n"); top != c.archiveTop {
				t.Errorf("the archive begins %q, want %q", top, c.archiveTop)
			}
		})
	}
}

// TestTheHeartbeatRecordsTheStepsFirstLineOnce (ruling R36, fix round 2): a
// Run that follows a step from its start records the step's first line on
// the Job with its heartbeat -- once, and never over one already recorded --
// so an adopter can tell whether its replay reached the step's head.
func TestTheHeartbeatRecordsTheStepsFirstLineOnce(t *testing.T) {
	l1, l2 := captureKubeLine(rtAt(1100), "first"), captureKubeLine(rtAt(1200), "second")
	firstWritten := func(h *rtHarness) []string {
		var written []string
		for _, p := range h.c.appliedPatches() {
			if v, ok := p.get(AnnotLogFirst); ok {
				written = append(written, v)
			}
		}
		return written
	}
	// heldUntilCursor runs the step until a heartbeat has carried the cursor
	// to its last line, so beats have run with both lines stored.
	heldUntilCursor := func(t *testing.T, h *rtHarness, run StepRun) {
		t.Helper()
		released := false
		running := rtPod(testJobName, rtStepRunning(rtAt(1000)), clsCloneDone)
		script := &rtScript{
			states: []rtState{
				{pod: running, visible: 2, until: func(c *rtCluster) bool { return released }},
				{pod: rtPod(testJobName, rtStepEnded(0, rtAt(1000), rtAt(4000)), clsCloneDone), visible: 2},
			},
			log:   []string{l1, l2},
			tails: map[string]string{ContainerClone: rtCloneTail},
		}
		if h.c.hasJob(testJobName) {
			h.c.with(func(c *rtCluster) { c.jobs[testJobName].script = script })
		} else {
			h.c.script(testJobName, script)
		}
		done := h.start(context.Background(), run)
		rtWaitUntil(t, "the cursor at the last line", func() bool {
			var ok bool
			h.c.with(func(c *rtCluster) { ok = c.cursorPatchedLocked(testJobName, rtAt(1200)) })
			return ok
		})
		h.c.with(func(*rtCluster) { released = true })
		if res := h.await(t, done); res.Status != pl.OutcomeSucceeded {
			t.Fatalf("result = %+v, want success", res)
		}
	}

	t.Run("by the Run that followed the step from its start, once", func(t *testing.T) {
		h := newRunnerHarness(t)
		heldUntilCursor(t, h, rtRun())
		if written := firstWritten(h); len(written) != 1 || written[0] != rtAt(1100).Format(time.RFC3339Nano) {
			t.Errorf("the first line was written %q, want once, the first line's stamp", written)
		}
	})

	t.Run("never by an adopter", func(t *testing.T) {
		h := newRunnerHarness(t)
		run := rtRun()
		h.c.putJob(h.existingJob(t, run, map[string]string{
			AnnotRunner:    rtStamp(rtOther, rtT0.Add(-time.Minute)),
			AnnotLogCursor: rtAt(1000).Format(time.RFC3339Nano),
		}), &rtScript{states: []rtState{{}}})
		heldUntilCursor(t, h, run)
		if written := firstWritten(h); len(written) != 0 {
			t.Errorf("the adopter wrote the step's first line %q: the first line it stored is not the step's", written)
		}
	})
}

// TestRunnerKeepsLinesWrittenOutOfStampOrder: stdout and stderr are stamped by
// goroutines of their own, so the log file holds lines out of stamp order. A
// line stamped before the line ahead of it in the same stream is output like
// any other -- in the store and the archive, never dropped as a repeat.
func TestRunnerKeepsLinesWrittenOutOfStampOrder(t *testing.T) {
	h := newRunnerHarness(t)
	lines := []string{
		captureKubeLine(rtAt(2100), "out: building"),
		captureKubeLine(rtAt(2050), "err: warning: deprecated flag"),
		captureKubeLine(rtAt(2200), "out: done"),
	}
	h.c.script(testJobName, rtFinishingScript(testJobName, 0, lines...))

	res := h.run(t, rtRun())

	if got := h.sink.messages(); !reflect.DeepEqual(got, rtTexts(lines...)) {
		t.Errorf("store = %q, want all three lines in the order written", got)
	}
	if archive := string(h.file(t, "tests-go-tests-2.log").Bytes); !strings.HasPrefix(archive, strings.Join(rtTexts(lines...), "\n")+"\n") {
		t.Errorf("archive = %q, want all three lines", archive)
	}
	if res.LogLines != 3 {
		t.Errorf("LogLines = %d, want 3", res.LogLines)
	}
}

// TestTheHeartbeatsCursorOnlyMovesForward: the cursor the heartbeat publishes
// is where an adopter resumes the store from. A line stamped before the one
// fed ahead of it (stdout and stderr stamp on their own) must not move it
// back, or an adopter would store again what this Run stored.
func TestTheHeartbeatsCursorOnlyMovesForward(t *testing.T) {
	s := &step{}
	s.publishCursor(rtAt(2200))
	s.publishCursor(rtAt(2150))
	if got := s.getCursor(); !got.Equal(rtAt(2200)) {
		t.Errorf("cursor = %s, want %s: it moved back", got.Format(time.RFC3339Nano), rtAt(2200).Format(time.RFC3339Nano))
	}
}

// TestRunnerDropsOnlyWhatItCapturedFromAReplayedSecond: a stream opened again
// starts at the cursor's whole second and replays it. A replayed line is
// dropped only when this Run captured it; one stamped inside that second that
// it never saw -- written after the line it stopped at, stamped before it --
// is output, not a repeat.
func TestRunnerDropsOnlyWhatItCapturedFromAReplayedSecond(t *testing.T) {
	h := newRunnerHarness(t)
	lines := []string{
		captureKubeLine(rtAt(2100), "one"),
		captureKubeLine(rtAt(2400), "two"),
		captureKubeLine(rtAt(2300), "two and a half, stamped before two, written after it"),
		captureKubeLine(rtAt(2800), "three"),
	}
	running := rtPod(testJobName, rtStepRunning(rtAt(1000)), clsCloneDone)
	h.c.script(testJobName, &rtScript{
		states: []rtState{
			{pod: running, visible: 4, until: func(c *rtCluster) bool { return c.streams >= 2 && c.cursorPatchedLocked(testJobName, rtAt(2800)) }},
			{pod: rtPod(testJobName, rtStepEnded(0, rtAt(1000), rtAt(4000)), clsCloneDone), visible: 4},
		},
		log:       lines,
		tails:     map[string]string{ContainerClone: rtCloneTail},
		dropAfter: 2,
	})

	res := h.run(t, rtRun())

	if got := h.sink.messages(); !reflect.DeepEqual(got, rtTexts(lines...)) {
		t.Errorf("store = %q, want each line once, the late one kept", got)
	}
	if res.LogLines != 4 {
		t.Errorf("LogLines = %d, want 4", res.LogLines)
	}
}

// TestRunnerReopensFromWhatAStreamLeftUnfed (fix round 2, minor 1): a stream
// that ends inside a line is opened again early enough to replay that line,
// and nothing it replays is fed twice.
//
//   - A line over a mebibyte had its first pieces fed when its stream ended:
//     the next stream replays it from its start, and only the rest of it is
//     fed (the re-review's probe archived 2.5 MiB for a 1.5 MiB line).
//   - A line held back, stamped in an earlier second than the last line fed
//     (stdout and stderr are stamped apart), is in no stream opened at the
//     last line's second: the next opens at the held line's.
func TestRunnerReopensFromWhatAStreamLeftUnfed(t *testing.T) {
	t.Run("a long line partly fed", func(t *testing.T) {
		h := newRunnerHarness(t, func(cfg *Config) { cfg.ArchiveMaxBytes = 8 << 20; cfg.LogStoreMaxLines = 100000 })
		l1 := captureKubeLine(rtAt(1100), "one")
		long := captureKubeLine(rtAt(1200), strings.Repeat("p", followLineMax+followLineMax/2))
		l3 := captureKubeLine(rtAt(1300), "three")
		running := rtPod(testJobName, rtStepRunning(rtAt(1000)), clsCloneDone)
		h.c.script(testJobName, &rtScript{
			states: []rtState{
				{pod: running, visible: 1, until: func(c *rtCluster) bool { return c.streams >= 2 }},
				{pod: running, visible: 3, until: func(c *rtCluster) bool { return c.cursorPatchedLocked(testJobName, rtAt(1300)) }},
				{pod: rtPod(testJobName, rtStepEnded(0, rtAt(1000), rtAt(4000)), clsCloneDone), visible: 3},
			},
			log:            []string{l1, long, l3},
			tails:          map[string]string{ContainerClone: rtCloneTail},
			cutFirstStream: long[:followLineMax+followLineMax/4],
		})

		res := h.run(t, rtRun())

		if res.Status != pl.OutcomeSucceeded {
			t.Fatalf("result = %+v, want success", res)
		}
		archive := string(h.file(t, "tests-go-tests-2.log").Bytes)
		n := 0
		for _, l := range strings.Split(archive, "\n") {
			if l != "" && strings.Trim(l, "p") == "" {
				n += len(l)
			}
		}
		if n != followLineMax+followLineMax/2 {
			t.Errorf("the archive holds %d of the line's bytes, want the line once (%d)", n, followLineMax+followLineMax/2)
		}
		if strings.Count(archive, "one\n") != 1 || strings.Count(archive, "three\n") != 1 {
			t.Errorf("the lines around it are not each archived once")
		}
		stored := 0
		for _, m := range h.sink.messages() {
			stored += strings.Count(m, "p")
		}
		if stored != followLineMax+followLineMax/2 {
			t.Errorf("the store holds %d of the line's bytes, want the line once", stored)
		}
	})

	t.Run("a held line stamped a second before the last line fed", func(t *testing.T) {
		h := newRunnerHarness(t)
		a := captureKubeLine(rtAt(1100), "a")
		b := captureKubeLine(rtAt(2100), "b")
		c := captureKubeLine(rtAt(1500), "c, stamped before b and written after it")
		running := rtPod(testJobName, rtStepRunning(rtAt(1000)), clsCloneDone)
		h.c.script(testJobName, &rtScript{
			states: []rtState{
				{pod: running, visible: 2, until: func(c *rtCluster) bool { return c.streams >= 2 }},
				{pod: running, visible: 3, until: func(c *rtCluster) bool { return c.cursorPatchedLocked(testJobName, rtAt(2100)) }},
				{pod: rtPod(testJobName, rtStepEnded(0, rtAt(1000), rtAt(4000)), clsCloneDone), visible: 3},
			},
			log:            []string{a, b, c},
			tails:          map[string]string{ContainerClone: rtCloneTail},
			cutFirstStream: c[:len(c)-5],
		})

		res := h.run(t, rtRun())

		if res.Status != pl.OutcomeSucceeded {
			t.Fatalf("result = %+v, want success", res)
		}
		if got := h.sink.messages(); !reflect.DeepEqual(got, rtTexts(a, b, c)) {
			t.Errorf("store = %q, want each line once, the held one included", got)
		}
		if archive := string(h.file(t, "tests-go-tests-2.log").Bytes); !strings.HasPrefix(archive, strings.Join(rtTexts(a, b, c), "\n")+"\n") {
			t.Errorf("archive = %q, want each line once", archive)
		}
	})
}

// TestRunnerHoldsALineAStreamEndedInside (minor 10): a stream that ends in
// the middle of a line -- a connection dropped while the step wrote it -- has
// not seen the whole line. The piece is held back, and the stream opened again
// replays the line whole, which is what is captured. A step's last line with
// no newline at all is captured once the step has ended.
func TestRunnerHoldsALineAStreamEndedInside(t *testing.T) {
	t.Run("cut by a dropped connection", func(t *testing.T) {
		h := newRunnerHarness(t)
		l1 := captureKubeLine(rtAt(1100), "one")
		l2 := captureKubeLine(rtAt(1200), "two, written in full")
		running := rtPod(testJobName, rtStepRunning(rtAt(1000)), clsCloneDone)
		h.c.script(testJobName, &rtScript{
			states: []rtState{
				{pod: running, visible: 1, until: func(c *rtCluster) bool { return c.streams >= 2 }},
				{pod: running, visible: 2, until: func(c *rtCluster) bool { return c.cursorPatchedLocked(testJobName, rtAt(1200)) }},
				{pod: rtPod(testJobName, rtStepEnded(0, rtAt(1000), rtAt(4000)), clsCloneDone), visible: 2},
			},
			log:            []string{l1, l2},
			tails:          map[string]string{ContainerClone: rtCloneTail},
			cutFirstStream: captureKubeLine(rtAt(1200), "two, writ"),
		})

		h.run(t, rtRun())

		if got := h.sink.messages(); !reflect.DeepEqual(got, rtTexts(l1, l2)) {
			t.Errorf("store = %q, want both lines whole, and nothing of the cut", got)
		}
	})

	t.Run("the step's last line, with no newline", func(t *testing.T) {
		h := newRunnerHarness(t)
		l1 := captureKubeLine(rtAt(1100), "one")
		l2 := captureKubeLine(rtAt(1200), "done")
		h.c.script(testJobName, &rtScript{
			states: []rtState{
				{pod: rtPod(testJobName, rtStepRunning(rtAt(1000)), clsCloneDone), visible: 1, reads: 2},
				{pod: rtPod(testJobName, rtStepEnded(0, rtAt(1000), rtAt(4000)), clsCloneDone), visible: 2},
			},
			log:        []string{l1, l2},
			tails:      map[string]string{ContainerClone: rtCloneTail},
			finalNoEOL: true,
		})

		h.run(t, rtRun())

		if got := h.sink.messages(); !reflect.DeepEqual(got, rtTexts(l1, l2)) {
			t.Errorf("store = %q, want the last line too", got)
		}
	})
}

// TestRunnerLosesTheOwnershipRaceAndWaits: two replicas read an unclaimed Job
// at once and both try to claim it. The claim is a compare-and-swap, so the
// one that loses gets a conflict, reads the Job again, finds the winner's
// fresh claim, and waits for its outcome.
func TestRunnerLosesTheOwnershipRaceAndWaits(t *testing.T) {
	outcome := pl.StepResult{Status: pl.OutcomeSucceeded, Where: pl.Where{Surface: "cluster", NodeID: rtOther, JobName: testJobName}, LogFileID: "file-3"}
	h := newRunnerHarness(t)
	h.c.putJob(h.existingJob(t, rtRun(), nil), rtRunningScript(testJobName))
	lostAt := 0
	h.c.onJobPatch = func(c *rtCluster, p rtPatch) {
		if lostAt == 0 && p.rv != "" {
			lostAt = c.jobGets
			c.annotateLocked(testJobName, AnnotRunner, rtStamp(rtOther, rtT0)) // workbench-a's claim lands first
		}
	}
	h.c.onJobGet = func(c *rtCluster, n int) {
		if lostAt > 0 && n == lostAt+3 {
			c.annotateLocked(testJobName, AnnotOutcome, mustJSON(t, outcome))
		}
	}

	res := h.run(t, rtRun())

	if !reflect.DeepEqual(res, outcome) {
		t.Errorf("result = %+v, want the winner's outcome %+v", res, outcome)
	}
	if n := len(h.c.requestsFor(http.MethodPatch, rtJobPath)); n != 1 {
		t.Errorf("%d patches of the Job, want the one claim it lost", n)
	}
	rtNoRequests(t, h.c, http.MethodGet, kubePods)
	if len(h.lib.stored()) != 0 {
		t.Error("the runner that lost the claim stored a file")
	}
	h.leftNoArchive(t)
}

// TestTwoRunsOfOneReplicaNeverBothOwnAStep (review finding 3): a re-forward
// can reach the replica whose own first Run still holds the step. That Run's
// claim carries this replica's node id, so the id cannot say whose it is:
// the replica's registry of its Runs does. While the first Run lives, the
// second waits for its outcome -- even once the claim looks stale, its
// heartbeats failing -- and the step is captured and settled once.
func TestTwoRunsOfOneReplicaNeverBothOwnAStep(t *testing.T) {
	h := newRunnerHarness(t)
	released := false
	l1, l2 := captureKubeLine(rtAt(1100), "one"), captureKubeLine(rtAt(1200), "two")
	running := rtPod(testJobName, rtStepRunning(rtAt(1000)), clsCloneDone)
	h.c.script(testJobName, &rtScript{
		states: []rtState{
			{pod: running, visible: 1, until: func(*rtCluster) bool { return released }},
			{pod: running, visible: 2, reads: 2},
			{pod: rtPod(testJobName, rtStepEnded(0, rtAt(1000), rtAt(4000)), clsCloneDone), visible: 2},
		},
		log:   []string{l1, l2},
		tails: map[string]string{ContainerClone: rtCloneTail},
	})
	first := h.start(context.Background(), rtRun())
	rtWaitUntil(t, "the first Run to follow the step", func() bool { return len(h.sink.messages()) == 1 })

	// The first Run's heartbeats fail and its claim ages past HeartbeatStale.
	var base int
	h.c.with(func(c *rtCluster) { c.refuseBeats, base = true, c.jobGets })
	h.clock.Advance(time.Minute)
	second := h.start(context.Background(), rtRun())
	rtWaitUntil(t, "the second Run to read the Job a while", func() bool {
		var n int
		h.c.with(func(c *rtCluster) { n = c.jobGets })
		return n > base+20
	})
	h.c.with(func(c *rtCluster) { c.refuseBeats, released = false, true })

	a, b := h.await(t, first), h.await(t, second)

	if !reflect.DeepEqual(a, b) || a.Status != pl.OutcomeSucceeded {
		t.Errorf("results:\n  first  %+v\n  second %+v\nwant one success, the second Run answering the first's outcome", a, b)
	}
	if got := h.sink.messages(); !reflect.DeepEqual(got, rtTexts(l1, l2)) {
		t.Errorf("store = %q, want each line once: one Run captured the step", got)
	}
	outcomes := 0
	for _, p := range h.c.appliedPatches() {
		if _, ok := p.get(AnnotOutcome); ok {
			outcomes++
		}
	}
	if outcomes != 1 || len(h.lib.stored()) != 1 {
		t.Errorf("%d outcomes persisted and %d Library files, want one of each: one Run settled the step", outcomes, len(h.lib.stored()))
	}
}

// TestRunnerTakesItsOwnClaimWhoseAnswerWasLost (review finding 3, fix round 2
// Important B): the claim was applied and its answer lost on the way back.
// Read again, the Job carries this replica's claim, and the replica's
// registry says it is this Run's: the Run goes on as its holder -- it waits
// on no heartbeat, its own, to go stale. What the claim was is remembered
// from when it was sent: a creator's claim says nothing about re-attaching,
// and an ADOPTER's claim stays an adoption, from the cursor it read before
// claiming -- else every line from the step's start would reach the store
// again, with no word of the seam.
func TestRunnerTakesItsOwnClaimWhoseAnswerWasLost(t *testing.T) {
	// No heartbeat: a beat before any line is captured patches the claim
	// alone, as a claim does, and would be counted as one.
	quietHeartbeat := func(c *Config) { c.HeartbeatInterval = time.Hour }
	claimsApplied := func(h *rtHarness) int {
		n := 0
		for _, p := range h.c.appliedPatches() {
			if _, beat := p.get(AnnotLogCursor); !beat && len(p.annots) == 1 && p.rv != "" {
				if _, ok := p.get(AnnotRunner); ok {
					n++
				}
			}
		}
		return n
	}

	t.Run("a creator's claim", func(t *testing.T) {
		h := newRunnerHarness(t, quietHeartbeat)
		h.c.with(func(c *rtCluster) { c.loseClaims = 1 })
		h.c.script(testJobName, rtFinishingScript(testJobName, 0, captureKubeLine(rtAt(1100), "ok")))

		res := h.await(t, h.start(context.Background(), rtRun()))

		if res.Status != pl.OutcomeSucceeded {
			t.Fatalf("result = %+v, want success", res)
		}
		if got := h.sink.messages(); !reflect.DeepEqual(got, []string{"ok"}) {
			t.Errorf("store = %q, want the step's line and no word of a re-attach", got)
		}
		if n := claimsApplied(h); n != 1 {
			t.Errorf("%d claims applied, want the one whose answer was lost", n)
		}
	})

	for _, lose := range []int{0, 1} {
		name := map[int]string{0: "an adopter's claim, its answer received (control)", 1: "an adopter's claim"}[lose]
		t.Run(name, func(t *testing.T) {
			h := newRunnerHarness(t, quietHeartbeat)
			run := rtRun()
			cursor := rtAt(2500)
			l0 := captureKubeLine(rtAt(1100), "line 0")
			l1 := captureKubeLine(rtAt(2100), "line 1, captured by workbench-a")
			l2 := captureKubeLine(cursor, "line 2, the cursor")
			l3 := captureKubeLine(rtAt(2700), "line 3")
			l4 := captureKubeLine(rtAt(3100), "line 4")
			h.c.putJob(h.existingJob(t, run, map[string]string{
				AnnotRunner:    rtStamp(rtOther, rtT0.Add(-time.Minute)),
				AnnotLogCursor: cursor.Format(time.RFC3339Nano),
			}), rtFinishingScript(testJobName, 0, l0, l1, l2, l3, l4))
			h.c.putSecret(BuildSecret(h.cfg, run, testJobName, rtCloneToken))
			h.c.with(func(c *rtCluster) { c.loseClaims = lose })

			res := h.run(t, run)

			if res.Status != pl.OutcomeSucceeded {
				t.Fatalf("result = %+v, want success", res)
			}
			store := h.sink.messages()
			if len(store) != 3 || !strings.Contains(store[0], "re-attached") || store[1] != "line 3" || store[2] != "line 4" {
				t.Errorf("store = %q, want the re-attach notice and lines 3 and 4 only: lines 0-2 are in the store already", store)
			}
			archive := string(h.file(t, "tests-go-tests-2.log").Bytes)
			if !strings.HasPrefix(archive, strings.Join(rtTexts(l0, l1, l2), "\n")+"\n"+store[0]+"\nline 3\nline 4\n") {
				t.Errorf("archive = %q, want lines 0-2 replayed, the notice at the seam, then lines 3 and 4", archive)
			}
			if n := claimsApplied(h); n != 1 {
				t.Errorf("%d claims applied, want one", n)
			}
		})
	}
}

// TestRunnerYieldsWhenAnotherReplicaTakesTheJob: a runner whose claim went
// stale -- its calls to the API server failed for longer than HeartbeatStale
// -- finds on its return that another replica adopted the Job. It stops
// following, never stamps its claim over the adopter's, stores nothing, and
// answers the adopter's outcome.
func TestRunnerYieldsWhenAnotherReplicaTakesTheJob(t *testing.T) {
	outcome := pl.StepResult{Status: pl.OutcomeSucceeded, Where: pl.Where{Surface: "cluster", NodeID: rtOther, JobName: testJobName}, LogFileID: "file-5", LogLines: 2}
	h := newRunnerHarness(t)
	h.c.script(testJobName, rtRunningScript(testJobName, captureKubeLine(rtAt(1100), "working")))
	done := h.start(context.Background(), rtRun())
	rtWaitUntil(t, "the runner to follow the step", func() bool { return len(h.sink.messages()) == 1 })

	takenAt := 0
	h.c.with(func(c *rtCluster) {
		takenAt = len(c.patches)
		base := c.jobGets
		c.annotateLocked(testJobName, AnnotRunner, rtStamp(rtOther, rtT0))
		// The adopter's outcome lands a dozen reads later: time enough for
		// several of the runner's heartbeats.
		c.onJobGet = func(c *rtCluster, n int) {
			if n == base+12 {
				c.annotateLocked(testJobName, AnnotOutcome, mustJSON(t, outcome))
			}
		}
	})
	res := h.await(t, done)

	if !reflect.DeepEqual(res, outcome) {
		t.Errorf("result = %+v, want the adopter's outcome %+v", res, outcome)
	}
	for _, p := range h.c.appliedPatches()[takenAt:] {
		t.Errorf("after the adoption the runner patched the Job: %+v", p.annots)
	}
	if got := h.c.jobNow(t, testJobName).Metadata.Annotations[AnnotRunner]; got != rtStamp(rtOther, rtT0) {
		t.Errorf("claim = %q, want the adopter's, untouched", got)
	}
	if len(h.lib.stored()) != 0 {
		t.Error("the runner that lost the Job stored its partial log")
	}
	h.leftNoArchive(t)

	t.Run("its own poll sees the claim taken, between two heartbeats", func(t *testing.T) {
		// No heartbeat falls due here: the watch's own read of the Job is
		// what notices the adoption, a poll interval after it rather than a
		// heartbeat interval (2 s against 10 s, as configured).
		h := newRunnerHarness(t, func(c *Config) { c.HeartbeatInterval = time.Hour })
		taken := -1 // the count of Job reads at the adoption
		running := rtPod(testJobName, rtStepRunning(rtAt(1000)), clsCloneDone)
		h.c.script(testJobName, &rtScript{
			states: []rtState{
				// The step prints on only once the runner has read the Job
				// after the adoption: a poll that read the Job just before
				// it and the pod just after it has one more line to capture,
				// and that is the one poll interval the claim allows.
				{pod: running, visible: 1, until: func(c *rtCluster) bool { return taken >= 0 && c.jobGets > taken }},
				// What it prints after the adoption is the adopter's.
				{pod: running, visible: 2},
			},
			log: []string{captureKubeLine(rtAt(1100), "working"), captureKubeLine(rtAt(1200), "the adopter's to capture")},
		})
		done := h.start(context.Background(), rtRun())
		rtWaitUntil(t, "the runner to follow the step", func() bool { return len(h.sink.messages()) == 1 })
		h.c.with(func(c *rtCluster) {
			taken = c.jobGets
			base := c.jobGets
			c.annotateLocked(testJobName, AnnotRunner, rtStamp(rtOther, rtT0))
			c.onJobGet = func(c *rtCluster, n int) {
				if n == base+8 {
					c.annotateLocked(testJobName, AnnotOutcome, mustJSON(t, outcome))
				}
			}
		})

		res := h.await(t, done)

		if !reflect.DeepEqual(res, outcome) {
			t.Errorf("result = %+v, want the adopter's outcome %+v", res, outcome)
		}
		if got := h.sink.messages(); !reflect.DeepEqual(got, []string{"working"}) {
			t.Errorf("store = %q, want only the line from before the adoption", got)
		}
	})
}

// TestARunTakingAStepAgainStartsItsAdoptionAfresh (fix round 3, item 5): a
// Run adopts a step, yields it to a third replica, waits, and adopts it again
// when that replica goes quiet -- by when the node has rotated the step's
// log, and no longer serves line 0. Its second ownership is a new capture and
// a new adoption, from the cursor the third replica left: the seam notice is
// written again, and what the archive holds of the head is judged again
// (missing now), never carried over from the first ownership.
func TestARunTakingAStepAgainStartsItsAdoptionAfresh(t *testing.T) {
	h := newRunnerHarness(t)
	run := rtRun()
	l0 := captureKubeLine(rtAt(1100), "line 0, before the first cursor")
	l1 := captureKubeLine(rtAt(2100), "line 1")
	l2 := captureKubeLine(rtAt(3100), "line 2, captured by workbench-c")
	l3 := captureKubeLine(rtAt(4100), "line 3")
	taken, retaken := false, false
	running := rtPod(testJobName, rtStepRunning(rtAt(1000)), clsCloneDone)
	h.c.putJob(h.existingJob(t, run, map[string]string{
		AnnotRunner:    rtStamp(rtOther, rtT0.Add(-time.Minute)),
		AnnotLogCursor: rtAt(1100).Format(time.RFC3339Nano),
		AnnotLogFirst:  rtAt(1100).Format(time.RFC3339Nano),
	}), &rtScript{
		states: []rtState{
			{pod: running, visible: 2, until: func(*rtCluster) bool { return retaken }},
			{pod: running, visible: 4, reads: 2},
			{pod: rtPod(testJobName, rtStepEnded(0, rtAt(1000), rtAt(5000)), clsCloneDone), visible: 4},
		},
		log:   []string{l0, l1, l2, l3},
		tails: map[string]string{ContainerClone: rtCloneTail},
	})
	h.c.putSecret(BuildSecret(h.cfg, run, testJobName, rtCloneToken))
	h.c.with(func(c *rtCluster) {
		c.onJobPatch = func(c *rtCluster, p rtPatch) {
			if v, ok := p.get(AnnotRunner); ok && taken && strings.HasPrefix(v, rtNode+" ") && len(p.annots) == 1 {
				retaken = true
			}
		}
	})
	done := h.start(context.Background(), run)
	rtWaitUntil(t, "the first adoption to store line 1", func() bool { return len(h.sink.messages()) == 2 })
	// workbench-c took the step, captured line 2, and went quiet; the node
	// rotated the log meanwhile, and serves it from line 1.
	h.c.with(func(c *rtCluster) {
		taken = true
		sc := c.jobs[testJobName].script
		sc.log = sc.log[1:]
		for i := range sc.states {
			sc.states[i].visible--
		}
		c.annotateLocked(testJobName, AnnotRunner, rtStamp("workbench-c", rtT0.Add(-time.Minute)))
		c.annotateLocked(testJobName, AnnotLogCursor, rtAt(3100).Format(time.RFC3339Nano))
	})

	res := h.await(t, done)

	if res.Status != pl.OutcomeSucceeded || !retaken {
		t.Fatalf("result = %+v (retaken %v), want the step settled by its second adoption", res, retaken)
	}
	store := h.sink.messages()
	if len(store) != 4 || !strings.Contains(store[0], "from the step's first line") || store[1] != "line 1" ||
		!strings.Contains(store[2], "re-attached") || !strings.Contains(store[2], rtAt(3100).Format(time.RFC3339Nano)) || store[3] != "line 3" {
		t.Fatalf("store = %q, want each adoption's notice, then the lines after its cursor", store)
	}
	if !strings.Contains(store[2], "the head may be missing") || !strings.Contains(store[2], "from "+rtAt(2100).Format(time.RFC3339Nano)) {
		t.Errorf("second notice %q, want it to say the archive starts at line 1, the head missing", store[2])
	}
	archive := string(h.file(t, "tests-go-tests-2.log").Bytes)
	want := strings.Join(rtTexts(l1, l2), "\n") + "\n" + store[2] + "\n" + "line 3\n"
	if !strings.HasPrefix(archive, want) {
		t.Errorf("archive = %q, want the second capture whole: what the node still had, its own notice at the seam, then line 3", archive)
	}
}

// TestAFollowerReopensOnlyForTheClaimsHolder: a stream that ended while the
// step ran is opened again only by the runner that still holds the claim.
// One cut off from the API server long enough to lose it -- another replica
// adopted the step meanwhile -- would otherwise pour everything the step
// printed since its own cursor into the store beside the adopter's copy, in
// the moment before its next poll sees the claim is gone (measured on k3s:
// eight lines twice).
func TestAFollowerReopensOnlyForTheClaimsHolder(t *testing.T) {
	// A follower waits a poll interval before it opens a dropped stream
	// again: long enough here for the claim to move first.
	h := newRunnerHarness(t, func(c *Config) { c.PollInterval = 200 * time.Millisecond })
	h.c.putJob(h.existingJob(t, rtRun(), map[string]string{AnnotRunner: rtStamp(rtNode, rtT0)}), &rtScript{
		states:    []rtState{{pod: rtPod(testJobName, rtStepRunning(rtAt(1000)), clsCloneDone), visible: 3}},
		log:       []string{captureKubeLine(rtAt(1100), "one"), captureKubeLine(rtAt(1200), "two"), captureKubeLine(rtAt(1300), "three")},
		dropAfter: 1,
	})
	s := &step{r: h.r, run: rtRun(), jobName: testJobName, ctx: context.Background(), log: h.r.log}
	if err := s.ensureCapture(); err != nil {
		t.Fatalf("ensureCapture: %v", err)
	}
	defer s.discardCapture()

	f := s.follow(rtPodName(testJobName), false)
	rtWaitUntil(t, "the first stream to drop", func() bool { return len(h.sink.messages()) == 1 })
	var base int
	h.c.with(func(c *rtCluster) {
		base = c.jobGets
		c.annotateLocked(testJobName, AnnotRunner, rtStamp(rtOther, rtT0))
	})
	// The follower reads the claim before it opens a stream again: once it
	// has, it has decided.
	rtWaitUntil(t, "the follower to read the claim", func() bool {
		var n int
		h.c.with(func(c *rtCluster) { n = c.jobGets })
		return n > base
	})
	f.stop()

	if n := h.c.streamCount(); n != 1 {
		t.Errorf("%d streams opened, want the first only: the claim had moved before the second", n)
	}
	if got := h.sink.messages(); !reflect.DeepEqual(got, []string{"one"}) {
		t.Errorf("store = %q, want the line from before the claim moved", got)
	}

	t.Run("its holder opens it again", func(t *testing.T) {
		h := newRunnerHarness(t, func(c *Config) { c.PollInterval = 200 * time.Millisecond })
		h.c.putJob(h.existingJob(t, rtRun(), map[string]string{AnnotRunner: rtStamp(rtNode, rtT0)}), &rtScript{
			states:    []rtState{{pod: rtPod(testJobName, rtStepRunning(rtAt(1000)), clsCloneDone), visible: 3}},
			log:       []string{captureKubeLine(rtAt(1100), "one"), captureKubeLine(rtAt(1200), "two"), captureKubeLine(rtAt(1300), "three")},
			dropAfter: 1,
		})
		s := &step{r: h.r, run: rtRun(), jobName: testJobName, ctx: context.Background(), log: h.r.log}
		if err := s.ensureCapture(); err != nil {
			t.Fatalf("ensureCapture: %v", err)
		}
		defer s.discardCapture()
		f := s.follow(rtPodName(testJobName), false)
		rtWaitUntil(t, "all three lines", func() bool { return len(h.sink.messages()) == 3 })
		f.stop()
	})
}

// TestRunnerReopensTheLogFromItsCursor: a followed stream that ends while the
// step still runs -- a dropped connection -- is opened again replaySkew before
// the cursor's second (a line written after the stream ended may be stamped
// that far back, fix round 3), and the lines the API repeats are dropped:
// every line is captured once.
func TestRunnerReopensTheLogFromItsCursor(t *testing.T) {
	h := newRunnerHarness(t)
	lines := []string{
		captureKubeLine(rtAt(2100), "one"),
		captureKubeLine(rtAt(2400), "two"),
		captureKubeLine(rtAt(2800), "three"),
		captureKubeLine(rtAt(3200), "four"),
	}
	running := rtPod(testJobName, rtStepRunning(rtAt(1000)), clsCloneDone)
	h.c.script(testJobName, &rtScript{
		states: []rtState{
			{pod: running, visible: 4, until: func(c *rtCluster) bool { return c.streams >= 2 && c.cursorPatchedLocked(testJobName, rtAt(3200)) }},
			{pod: rtPod(testJobName, rtStepEnded(0, rtAt(1000), rtAt(4000)), clsCloneDone), visible: 4},
		},
		log:       lines,
		tails:     map[string]string{ContainerClone: rtCloneTail},
		dropAfter: 2,
	})

	res := h.run(t, rtRun())

	if res.Status != pl.OutcomeSucceeded || res.LogLines != 4 {
		t.Fatalf("result = %+v, want success with four lines", res)
	}
	if got := h.sink.messages(); !reflect.DeepEqual(got, rtTexts(lines...)) {
		t.Errorf("store = %q, want each line once, in order", got)
	}
	archive := string(h.file(t, "tests-go-tests-2.log").Bytes)
	if want := strings.Join(rtTexts(lines...), "\n") + "\n"; !strings.HasPrefix(archive, want) {
		t.Errorf("archive = %q, want each line once, in order", archive)
	}
	var follows []string
	for _, r := range h.c.requestsFor(http.MethodGet, kubePods+"/"+rtPodName(testJobName)+"/log") {
		if strings.Contains(r.Query, "follow=true") {
			follows = append(follows, r.Query)
		}
	}
	if len(follows) < 2 || follows[0] != "container=step&follow=true&timestamps=true" ||
		follows[1] != "container=step&follow=true&sinceTime=2026-10-04T09%3A00%3A01Z&timestamps=true" {
		t.Errorf("follows = %q, want the first from the start and the second a second before the cursor's", follows)
	}
}

// TestRunnerReadsTheLogToItsEnd: a step's last words are often written as it
// ends, and the API's stream lags the container. Once the step is decided the
// runner reads its log to the end before it archives anything -- also for a
// step that started and ended between two polls, which it never saw run.
func TestRunnerReadsTheLogToItsEnd(t *testing.T) {
	ended := rtPod(testJobName, rtStepEnded(1, rtAt(1000), rtAt(4000)), clsCloneDone)
	for _, c := range []struct {
		name   string
		states []rtState
	}{
		{"the last lines, written as it ended", []rtState{
			{pod: rtPod(testJobName, rtStepRunning(rtAt(1000)), clsCloneDone), visible: 1, reads: 2},
			{pod: ended, visible: 1},
		}},
		{"a step it never saw run", []rtState{
			{pod: rtPod(testJobName, clsStepInit, rtCloneRunning)},
			{pod: ended, visible: 1},
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newRunnerHarness(t)
			first := captureKubeLine(rtAt(1100), "compiling")
			last := []string{captureKubeLine(rtAt(3900), "panic: widget is nil"), captureKubeLine(rtAt(3950), "exit status 1")}
			h.c.script(testJobName, &rtScript{
				states: c.states,
				log:    []string{first},
				final:  last,
				tails:  map[string]string{ContainerClone: rtCloneTail},
			})

			res := h.run(t, rtRun())

			if res.Status != pl.OutcomeFailed || res.ExitCode != 1 || res.LogLines != 3 {
				t.Fatalf("result = %+v, want failed, exit 1, all three lines", res)
			}
			want := rtTexts(first, last[0], last[1])
			if got := h.sink.messages(); !reflect.DeepEqual(got, want) {
				t.Errorf("store = %q, want %q", got, want)
			}
			if archive := string(h.file(t, "tests-go-tests-2.log").Bytes); !strings.HasPrefix(archive, strings.Join(want, "\n")+"\n") {
				t.Errorf("archive = %q, want the step's lines to its last", archive)
			}
			if res.LogTail != strings.Join(want, "\n") {
				t.Errorf("tail = %q, want the step's last words in it", res.LogTail)
			}
		})
	}
}

// TestRunnerKeepsTheKubeletsWordsOutOfTheStepsOutput: followed with
// timestamps, every line the step writes comes stamped. An unstamped line is
// the kubelet or the API server speaking in the stream -- measured on k3s
// v1.32, following a pod as it is deleted: `failed to try resolving symlinks
// in path "/var/log/pods/.../step/0.log": lstat ...: no such file or
// directory`. It is archived as what it is, and never reaches the store or
// the tail as the step's own words.
func TestRunnerKeepsTheKubeletsWordsOutOfTheStepsOutput(t *testing.T) {
	h := newRunnerHarness(t)
	kubelet := `failed to try resolving symlinks in path "/var/log/pods/steps-ns_x/step/0.log": lstat /var/log/pods/steps-ns_x/step/0.log: no such file or directory`
	h.c.script(testJobName, rtFinishingScript(testJobName, 1,
		captureKubeLine(rtAt(1100), "compiling"), kubelet, captureKubeLine(rtAt(1300), "exit status 1")))

	res := h.run(t, rtRun())

	if got := h.sink.messages(); !reflect.DeepEqual(got, []string{"compiling", "exit status 1"}) {
		t.Errorf("store = %q, want the step's two lines only", got)
	}
	if res.LogTail != "compiling\nexit status 1" || res.LogLines != 2 {
		t.Errorf("tail %q, %d lines; want the step's own two", res.LogTail, res.LogLines)
	}
	// A stream opened again repeats it, having no timestamp to be dropped by:
	// archived once.
	archive := string(h.file(t, "tests-go-tests-2.log").Bytes)
	if !strings.Contains(archive, "\nmemql: the log stream reported: "+kubelet+"\n") || strings.Count(archive, kubelet) != 1 {
		t.Errorf("archive = %q, want the kubelet's sentence once, as a runner note", archive)
	}
}

// TestRunnerNeverCutsInsideASecret (review finding 4, fix round 2 Important
// A, Review Focus 1): an endless line is fed in pieces, and each piece is
// masked on its own, so a secret a cut fell inside would be masked in
// neither. The follower masks every whole secret before it chooses a cut, and
// holds back one that may continue: no secret is cut, whole or begun.
func TestRunnerNeverCutsInsideASecret(t *testing.T) {
	const s1, s2 = "tokA-0123456789Z", "Zeta-secret-24680"
	stamp := rtAt(1200).Format(time.RFC3339Nano) + " "
	for _, c := range []struct {
		name    string
		secrets map[string]string
		line    string
		leaks   []string // what must reach nowhere
	}{
		{
			name:    "a secret begun six bytes before the cut",
			secrets: map[string]string{"NPM_TOKEN": plantedNPM},
			line:    stamp + strings.Repeat("a", followLineMax-len(stamp)-6) + plantedNPM + strings.Repeat("b", 1000),
			leaks:   []string{plantedNPM[:6], plantedNPM[6:]},
		},
		{
			// The re-review's probe: s1 ends exactly at the cut, and its last
			// byte begins s2. Holding back what begins a secret, alone, moved
			// the cut inside the whole s1.
			name:    "a whole secret ending at the cut, whose last byte begins another",
			secrets: map[string]string{"A": s1, "B": s2},
			line:    stamp + strings.Repeat("a", followLineMax-len(stamp)-len(s1)) + s1 + strings.Repeat("b", 1000),
			leaks:   []string{s1[:len(s1)-1], s1[1:]},
		},
		{
			name:    "the same secret alone",
			secrets: map[string]string{"A": s1},
			line:    stamp + strings.Repeat("a", followLineMax-len(stamp)-len(s1)) + s1 + strings.Repeat("b", 1000),
			leaks:   []string{s1[:len(s1)-1], s1[1:]},
		},
		{
			// The line's timestamp is never masked (the capture splits it off
			// first): masked, the line would read as the stream's own words.
			name:    "a secret the timestamp holds",
			secrets: map[string]string{"YEAR": "2026"},
			line:    stamp + strings.Repeat("y", followLineMax) + "2026" + "tail",
			leaks:   []string{"2026tail"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newRunnerHarness(t, func(c *Config) { c.ArchiveMaxBytes = 4 << 20 })
			run := rtRun()
			run.Secrets = c.secrets
			h.c.script(testJobName, rtFinishingScript(testJobName, 0, c.line))

			res := h.run(t, run)

			if res.Status != pl.OutcomeSucceeded {
				t.Fatalf("result = %+v, want success", res)
			}
			archive := string(h.file(t, "tests-go-tests-2.log").Bytes)
			store := strings.Join(h.sink.messages(), "\n")
			for name, text := range map[string]string{"archive": archive, "store": store, "tail": res.LogTail} {
				for _, leak := range c.leaks {
					if strings.Contains(text, leak) {
						t.Errorf("the %s holds %q: a cut split a secret, and neither piece was masked", name, leak)
					}
				}
			}
			if n := strings.Count(archive, "***"); n != 1 {
				t.Errorf("the archive holds %d masks, want the secret masked once", n)
			}
			if !strings.Contains(archive, c.line[len(stamp):len(stamp)+1000]) || strings.Contains(archive, "stream reported") {
				t.Errorf("the line is not in the archive as the step's output: %.200q...", archive)
			}
		})
	}
}

// TestLogLinesMaskAsTheWholeLineWould (fix round 2, Important A; the final
// review of epic memql#5478): whatever window the reader cuts a line with,
// the pieces -- each then masked by the capture, as the capture masks every
// piece -- are exactly the whole line masked as the seam masks it
// (pl.MaskSecrets): no secret is cut, none is masked differently, none leaves
// a remnant. Every window from 4 bytes to past the line is tried, around
// padded, overlapping, nested, adjacent and multi-line secrets, multi-byte
// runes, and secrets longer than the window. A secret printed over and over
// with nothing between is one span however long, and the reader holds no more
// of it than a window and a form.
func TestLogLinesMaskAsTheWholeLineWould(t *testing.T) {
	const s1, s2 = "tokA-0123456789Z", "Zeta-secret-24680"
	long := "LONG-" + strings.Repeat("s3cr", 10) + "-END"
	key := "-----BEGIN KEY-----\n    \nMIIBOgIBAAJBAKj34GkxFhD90vcN\n\t\n-----END KEY-----\n"
	for _, c := range []struct {
		name    string
		secrets []string
		line    string
		bounded bool // the reader may hold no more of the line than a window and its longest form
	}{
		{"a whole secret whose last byte begins another", []string{s1, s2}, "aaaa" + s1 + "bbbb" + s2 + "cc", false},
		{"overlapping secrets are one span", []string{"abcdef12", "ef12ghij"}, "xx" + "abcdef12ghij" + "yy" + "ef12ghij" + "zz" + "abcdef12" + "ef12ghij", false},
		{"a secret that begins a longer one", []string{"SECRET", "SECRETIVE-LONGER"}, "aa" + "SECRETIVE-LONGER" + "bb" + "SECRET" + "cc" + "SECRETIV", false},
		{"a secret nested inside another", []string{"inner", "the-inner-part"}, "the-inner-part and inner and the-inn", false},
		{"secrets among multi-byte runes", []string{"péché-ëëë", "ÿ€€€"}, strings.Repeat("é", 7) + "péché-ëëë" + "€" + "ÿ€€€" + strings.Repeat("ü", 5), false},
		{"a secret longer than the window", []string{long}, "head " + long + " middle " + long[:20] + " tail", false},
		{"a repeated secret back to back", []string{"abab"}, "abababababab", false},
		{"a secret printed over and over with nothing between", []string{"TOKEN-42"}, "x" + strings.Repeat("TOKEN-42", 400) + "TOKEN-4", true},
		// A value stored with whitespace around it is printed without it (a
		// form of its own), or with it, when the two forms are one span.
		{"secrets stored padded, printed trimmed and whole", []string{"  hunter2-token \n", "pad-value  "},
			"a hunter2-token b pad-value  c pad-value d hunter2-toke", false},
		// An indent-only line of a multi-line secret is no form: the line's
		// own indentation is not masked.
		{"a multi-line secret with indent-only lines", []string{key},
			"    return nil;\t\tMIIBOgIBAAJBAKj34GkxFhD90vcN    -----END KEY----- \t", false},
		// The capture drops NUL before it masks, so a NUL inside a secret
		// must not keep the reader from seeing it whole.
		{"a NUL inside a secret", []string{"abcdefgh"}, "xx" + "abcd\x00efgh" + "yy" + "abcdefgh", false},
		{"no secret at all", nil, "plain " + strings.Repeat("ø", 9), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			wantPiecesMaskedAsTheSeamMasks(t, c.secrets, c.line, 4, len(c.line)+1, c.bounded)
		})
	}
}

// TestLogLinesMaskRandomLinesAsTheSeamDoes is the same rule, randomized:
// secrets drawn from a small alphabet -- so they overlap, nest and repeat --
// stored padded, over lines with indent-only lines among them, or in parts
// a carriage return separates, bare or before a newline; lines made of their
// forms, whole and cut short, between filler of the same alphabet, with runes
// of two and three bytes for cuts to fall inside of.
func TestLogLinesMaskRandomLinesAsTheSeamDoes(t *testing.T) {
	rng := rand.New(rand.NewSource(20261004))
	alphabet := []string{"a", "b", "c", "a", "b", "é", "€", " ", "\t"}
	word := func(n int) string {
		var b strings.Builder
		for b.Len() < n {
			b.WriteString(alphabet[rng.Intn(len(alphabet))])
		}
		return b.String()
	}
	for n := 0; n < 250; n++ {
		var secrets []string
		for k := 0; k < 1+rng.Intn(3); k++ {
			s := word(4 + rng.Intn(8))
			switch rng.Intn(7) {
			case 0: // stored padded
				s = strings.Repeat(" ", rng.Intn(3)) + s + strings.Repeat(" ", rng.Intn(3)) + []string{"", "\n"}[rng.Intn(2)]
			case 1: // over lines, an indent-only one among them
				s += "\n" + strings.Repeat(" ", 4+rng.Intn(3)) + "\n" + word(4+rng.Intn(6)) + "\n"
			case 2: // the end of another, carried on: the two overlap where printed together
				if len(secrets) > 0 {
					prev := strings.TrimSpace(secrets[rng.Intn(len(secrets))])
					cut := len(prev) / 2
					for cut > 0 && !utf8.RuneStart(prev[cut]) {
						cut--
					}
					s = prev[cut:] + word(3)
				}
			case 3: // inside another
				if len(secrets) > 0 {
					prev := strings.TrimSpace(secrets[rng.Intn(len(secrets))])
					if len(prev) > 6 {
						s = prev[1 : len(prev)-1]
						s = strings.ToValidUTF8(s, "")
					}
				}
			case 4: // in parts a bare carriage return separates
				s += "\r" + word(4+rng.Intn(6))
			case 5: // over lines CRLF ends
				s += "\r\n" + word(4+rng.Intn(6)) + "\r\n"
			}
			secrets = append(secrets, s)
		}
		var printable []string // the forms a line can hold: one with no newline
		for _, f := range pl.MaskForms(secrets) {
			if !strings.Contains(f, "\n") {
				printable = append(printable, f)
			}
		}
		var line strings.Builder
		for size := 30 + rng.Intn(90); line.Len() < size; {
			if len(printable) == 0 || rng.Intn(3) == 0 {
				line.WriteString(word(1 + rng.Intn(6)))
				continue
			}
			f := printable[rng.Intn(len(printable))]
			if rng.Intn(3) == 0 { // cut short
				f = strings.ToValidUTF8(f[:rng.Intn(len(f))], "")
			}
			line.WriteString(f)
		}
		if strings.HasSuffix(line.String(), "\r") {
			// A line's own CR before its newline is the line end's, which
			// the reader and the capture both take off.
			line.WriteString("a")
		}
		ok := t.Run(fmt.Sprintf("case %d", n), func(t *testing.T) {
			wantPiecesMaskedAsTheSeamMasks(t, secrets, line.String(), 4, line.Len()+1, false)
		})
		if !ok {
			t.Logf("secrets %q, line %q", secrets, line.String())
			break
		}
	}
}

// wantPiecesMaskedAsTheSeamMasks reads line through the follower's reader at
// every window from lo to hi bytes, masks each piece as the capture masks a
// line it is fed, and wants the pieces to join into the whole line as
// pl.MaskSecrets masks it -- the capture's repair first, as the capture
// repairs every line and every secret -- with no piece over its window, and
// the line after it read whole. Apart from the seam, it wants no part of a
// secret between its line breaks -- a newline or a carriage return --
// trimmed and four bytes or more, left in the line. bounded also wants the
// reader to hold no more of the line than a window and the longest form
// whenever it reads more -- what it holds while it reads on through a span
// gives out no piece to look at -- at windows no smaller than what one read
// gives it (bufio's 16 bytes).
func wantPiecesMaskedAsTheSeamMasks(t *testing.T, secrets []string, line string, lo, hi int, bounded bool) {
	t.Helper()
	capture, _ := newCaptureForTest(t, CaptureOptions{Secrets: secrets})
	repaired := make([]string, 0, len(secrets))
	for _, s := range secrets {
		repaired = append(repaired, captureRepair(s))
	}
	want := pl.MaskSecrets(captureRepair(line), repaired)
	for _, s := range repaired {
		for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' }) {
			if part = strings.TrimSpace(part); len(part) >= 4 && strings.Contains(want, part) {
				t.Fatalf("the seam leaves %q, a part of the secret %q, in %q", part, s, want)
			}
		}
	}
	forms := captureMaskForms(secrets)
	longest := 0
	for _, f := range forms {
		longest = max(longest, len(f))
	}
	for window := lo; window <= hi; window++ {
		var lines *logLines
		held := func() {
			if n := len(lines.carry); bounded && window >= 16 && n > window+longest {
				t.Fatalf("window %d: the reader holds %d bytes of the line, more than the window and the longest form (%d)", window, n, longest)
			}
		}
		lines = newLogLines(&readProbe{r: strings.NewReader(line + "\nnext\n"), each: held}, window, forms)
		var got strings.Builder
		for {
			piece, _, end, err := lines.next()
			if err != nil {
				t.Fatalf("window %d: %v", window, err)
			}
			if len(piece) > window {
				t.Errorf("window %d: a piece of %d bytes", window, len(piece))
			}
			held()
			got.WriteString(capture.Mask(piece))
			if end {
				break
			}
		}
		if got.String() != want {
			t.Fatalf("window %d: the pieces mask to\n  %q\nwant the whole line as the seam masks it\n  %q", window, got.String(), want)
		}
		if piece, first, end, err := lines.next(); piece != "next" || !first || !end || err != nil {
			t.Errorf("window %d: the line after = %q first %v end %v (%v), want it whole", window, piece, first, end, err)
		}
	}
}

// readProbe calls each before every read it passes on.
type readProbe struct {
	r    io.Reader
	each func()
}

func (p *readProbe) Read(b []byte) (int, error) {
	p.each()
	return p.r.Read(b)
}

// TestRunnerCutsAnEndlessLine (Review Focus 5): a step that prints one
// enormous line reaches the archive in pieces of at most a mebibyte, nothing
// of it lost, and the lines after it keep their own timestamps.
func TestRunnerCutsAnEndlessLine(t *testing.T) {
	h := newRunnerHarness(t, func(c *Config) { c.ArchiveMaxBytes = 8 << 20 })
	// "é" is two bytes. The line's first piece carries its 22-byte timestamp,
	// a space and "xx", so every "é" starts at an odd offset and a cut at a
	// mebibyte of bytes falls inside one, unless the reader moves it back to
	// where a rune starts.
	huge := "xx" + strings.Repeat("é", (5<<20)/4)
	after := captureKubeLine(rtAt(1300), "after")
	h.c.script(testJobName, rtFinishingScript(testJobName, 0, captureKubeLine(rtAt(1200), huge), after))

	res := h.run(t, rtRun())

	if res.Status != pl.OutcomeSucceeded {
		t.Fatalf("result = %+v, want success", res)
	}
	archived := strings.Split(strings.TrimSuffix(string(h.file(t, "tests-go-tests-2.log").Bytes), "\n"), "\n")
	var pieces []string
	for _, l := range archived {
		if l == "after" {
			break
		}
		pieces = append(pieces, l)
	}
	if len(pieces) < 3 {
		t.Fatalf("the %d-byte line was archived as %d lines, want it cut into pieces of at most a mebibyte", len(huge), len(pieces))
	}
	for i, p := range pieces {
		if len(p) > 1<<20 {
			t.Errorf("piece %d is %d bytes, over a mebibyte", i, len(p))
		}
		if strings.ContainsRune(p, '�') {
			t.Errorf("piece %d holds U+FFFD: a cut split a rune", i)
		}
	}
	if strings.Join(pieces, "") != huge {
		t.Error("the pieces do not join back into the line: something of it was lost")
	}
	if !strings.Contains(string(h.file(t, "tests-go-tests-2.log").Bytes), "\nafter\n") {
		t.Error("the line after the endless one is not in the archive")
	}
	// 2.5 MiB is some 640 store lines; the step may write 100 to the store.
	capped := false
	for _, n := range res.Notes {
		capped = capped || (n.Code == pl.CodeLogCapped && strings.Contains(n.Message, "100 lines"))
	}
	if !res.LogCapped || !capped {
		t.Errorf("LogCapped %v, notes %+v; want the live log capped, said in a %s note", res.LogCapped, res.Notes, pl.CodeLogCapped)
	}
}

// TestRunnerIgnoresALeftoverPodOfAnEarlierJob: names repeat. The Job of a
// (run, step, attempt) deleted by a cancel or an ack leaves its pod to the
// garbage collector, and that pod carries the same job-name label the runner
// selects by. A pod is the Job's only when its controller uid is the Job's
// uid; any other reads as no pod yet -- never as the step's.
func TestRunnerIgnoresALeftoverPodOfAnEarlierJob(t *testing.T) {
	h := newRunnerHarness(t)
	// The earlier Job's step was killed (137). Created in the same second as
	// the new pod and named after it, it is the newest pod JobPod sees.
	leftover := rtPod(testJobName, rtStepEnded(137, rtAt(-9000), rtAt(-1000)), clsCloneDone)
	leftover.Metadata.Name = testJobName + "-zz9qq"
	line := captureKubeLine(rtAt(1100), "fresh")
	running := rtPod(testJobName, rtStepRunning(rtAt(1000)), clsCloneDone)
	h.c.script(testJobName, &rtScript{
		states: []rtState{
			{leftover: leftover},                           // the new Job's pod is not there yet
			{pod: running, leftover: leftover, visible: 1}, // both, the leftover the newest
			{pod: running, visible: 1, reads: 2},           // collected
			{pod: rtPod(testJobName, rtStepEnded(0, rtAt(1000), rtAt(4000)), clsCloneDone), visible: 1},
		},
		log:   []string{line},
		tails: map[string]string{ContainerClone: rtCloneTail},
	})

	res := h.run(t, rtRun())

	if res.Status != pl.OutcomeSucceeded || res.ExitCode != 0 || res.LogLines != 1 {
		t.Fatalf("result = %+v (failure %+v), want the new Job's success: the earlier Job's 137 is not this step's", res, res.Failure)
	}
}

// TestRunnerKeepsItsFirstTerminalObservation: a step that finished inside its
// deadline reads as finished only while its pod lasts. The Job controller
// deletes a pod still running at the deadline -- its services stopping -- and
// the same Job then reads DeadlineExceeded. So the first terminal observation
// is the step's outcome: recorded on the Job the moment it is made, before
// the slow half of settling, and never taken again.
func TestRunnerKeepsItsFirstTerminalObservation(t *testing.T) {
	h := newRunnerHarness(t)
	line := captureKubeLine(rtAt(1100), "ok")
	h.c.script(testJobName, &rtScript{
		states: []rtState{
			{pod: rtPod(testJobName, rtStepRunning(rtAt(1000)), clsCloneDone), visible: 1, reads: 2},
			{pod: rtPod(testJobName, rtStepEnded(0, rtAt(1000), rtAt(4000)), clsCloneDone), visible: 1, job: JobStatus{StartTime: rtT0}},
			// Read once more, the pod is gone and the deadline has passed.
			{job: JobStatus{StartTime: rtT0, Conditions: []JobCondition{{Type: "FailureTarget", Status: "True", Reason: "DeadlineExceeded"}}}},
		},
		log:   []string{line},
		tails: map[string]string{ContainerClone: rtCloneTail},
	})

	res := h.run(t, rtRun())

	if res.Status != pl.OutcomeSucceeded || res.ExitCode != 0 || res.Failure != nil {
		t.Fatalf("result = %+v (failure %+v), want the success first observed", res, res.Failure)
	}
	observed, outcome := -1, -1
	for i, p := range h.c.appliedPatches() {
		if _, ok := p.get(AnnotObservation); ok && observed < 0 {
			observed = i
		}
		if _, ok := p.get(AnnotOutcome); ok {
			outcome = i
		}
	}
	if observed < 0 || outcome < 0 || observed > outcome {
		t.Errorf("the observation was patched at %d and the outcome at %d, want the observation recorded first", observed, outcome)
	}
	var seen pl.StepResult
	raw := h.c.jobNow(t, testJobName).Metadata.Annotations[AnnotObservation]
	want := pl.StepResult{Status: pl.OutcomeSucceeded, ExitCode: 0, StartedAt: "2026-10-04T09:00:01Z", FinishedAt: "2026-10-04T09:00:04Z"}
	if err := json.Unmarshal([]byte(raw), &seen); err != nil || !reflect.DeepEqual(seen, want) {
		t.Errorf("observation recorded on the Job = %q, want %+v", raw, want)
	}
}

// TestRunnerSettlesAnAdoptedStepByItsRecordedObservation: workbench-a saw the
// step finish, recorded what it saw, and went before it settled the step. By
// the time this replica adopts the Job, the Job controller has deleted the pod
// at the deadline and the Job reads DeadlineExceeded. The step is settled by
// what workbench-a saw -- a success -- not by what is left of it.
func TestRunnerSettlesAnAdoptedStepByItsRecordedObservation(t *testing.T) {
	h := newRunnerHarness(t)
	seen := pl.StepResult{Status: pl.OutcomeSucceeded, ExitCode: 0, StartedAt: "2026-10-04T08:59:01Z", FinishedAt: "2026-10-04T08:59:04Z"}
	h.c.putJob(h.existingJob(t, rtRun(), map[string]string{
		AnnotRunner:      rtStamp(rtOther, rtT0.Add(-time.Minute)),
		AnnotObservation: mustJSON(t, seen),
	}), &rtScript{states: []rtState{{job: JobStatus{
		StartTime:  rtT0.Add(-20 * time.Minute),
		Conditions: []JobCondition{{Type: "FailureTarget", Status: "True", Reason: "DeadlineExceeded"}},
	}}}})

	res := h.run(t, rtRun())

	if res.Status != pl.OutcomeSucceeded || res.ExitCode != 0 || res.Failure != nil ||
		res.StartedAt != seen.StartedAt || res.FinishedAt != seen.FinishedAt || res.Where.NodeID != rtNode {
		t.Fatalf("result = %+v (failure %+v), want workbench-a's observation, settled here", res, res.Failure)
	}
	var persisted pl.StepResult
	if err := json.Unmarshal([]byte(h.c.jobNow(t, testJobName).Metadata.Annotations[AnnotOutcome]), &persisted); err != nil || !reflect.DeepEqual(persisted, res) {
		t.Errorf("persisted outcome = %+v (%v), want the result Run answered", persisted, err)
	}
}

// ---------------------------------------------------------------------------
// Creating
// ---------------------------------------------------------------------------

// TestRunnerWaitsOutAnExceededQuota (Review Focus 4): with the ceiling full,
// the API server refuses the Job "exceeded quota". The step waits for a slot
// rather than failing: it says so once, where a person watching the run sees
// it, and tries again every five poll intervals.
func TestRunnerWaitsOutAnExceededQuota(t *testing.T) {
	t.Run("it waits, says so once, and runs when a slot frees", func(t *testing.T) {
		h := newRunnerHarness(t)
		h.c.with(func(c *rtCluster) { c.quotaRefusals = 3 })
		line := captureKubeLine(rtAt(1100), "done")
		h.c.script(testJobName, rtFinishingScript(testJobName, 0, line))

		res := h.run(t, rtRun())

		if res.Status != pl.OutcomeSucceeded || res.Notes != nil {
			t.Fatalf("result = %+v, want a plain success: waiting for a slot is not a failure", res)
		}
		posts := h.c.requestsFor(http.MethodPost, kubeJobs)
		if len(posts) != 4 {
			t.Fatalf("%d Job creates, want three refused and one admitted", len(posts))
		}
		for i := 1; i < len(posts); i++ {
			if gap := posts[i].at.Sub(posts[i-1].at); gap < 5*h.cfg.PollInterval {
				t.Errorf("create %d came %v after the one before, want at least five poll intervals (%v)", i+1, gap, 5*h.cfg.PollInterval)
			}
		}
		if n := len(h.c.requestsFor(http.MethodPost, kubeSecrets)); n != 1 {
			t.Errorf("%d Secret creates, want one", n)
		}
		store := h.sink.messages()
		if len(store) != 2 || !strings.Contains(store[0], "waiting for a free slot under the pipelines ceiling") || store[1] != "done" {
			t.Errorf("store = %q, want one notice of the wait, then the step's output", store)
		}
		if archive := string(h.file(t, "tests-go-tests-2.log").Bytes); !strings.HasPrefix(archive, store[0]+"\ndone\n") {
			t.Errorf("archive = %q, want the notice, then the output", archive)
		}
	})

	t.Run("cancelled while it waits, it leaves nothing behind", func(t *testing.T) {
		h := newRunnerHarness(t)
		h.c.with(func(c *rtCluster) { c.quotaRefusals = 1 << 30 })
		h.c.script(testJobName, rtRunningScript(testJobName))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// Cancel after admission's rejection is durably queued, not while
		// the second POST still has an unknown outcome on its caller's wire.
		requeued := 0
		h.c.with(func(c *rtCluster) {
			c.onSecretPatched = func(_ *rtCluster, secret Secret) {
				if secret.Metadata.Annotations[annotCreation] == creationQueued {
					requeued++
					if requeued == 2 {
						cancel()
					}
				}
			}
		})
		done := h.start(ctx, rtRun())

		res := h.await(t, done)

		rtWantCode(t, res, pl.OutcomeCancelled, pl.CodeStepCancelled)
		if h.c.hasSecret(testSecretName) || h.c.hasJob(testJobName) {
			t.Error("a step cancelled in the queue left its Secret or a Job behind")
		}
		h.leftNoArchive(t)
	})
}

// TestRunnerBoundsTheWaitByTheRunsCeiling (ruling R31b, which supersedes
// R31; Review Focus 4, design record D11): a step's own timeout runs from its
// Job's creation, and the time it waits for a free slot under the pipelines
// ceiling is bounded by its RUN's ceiling alone (StepRun.RunDeadline). So the
// Job is given the step's whole timeout however long it queued -- or what is
// left of its run, when that is less -- and a step still waiting when its run
// reaches its ceiling fails pipeline_run_ceiling, having created nothing.
func TestRunnerBoundsTheWaitByTheRunsCeiling(t *testing.T) {
	deadlineOf := func(t *testing.T, h *rtHarness) int64 {
		t.Helper()
		ads := h.c.jobNow(t, testJobName).Spec.ActiveDeadlineSeconds
		if ads == nil {
			t.Fatal("the Job has no deadline")
		}
		return *ads
	}
	// waitFor refuses the first create for quota, and the wait it costs is d.
	waitFor := func(h *rtHarness, d time.Duration) {
		h.c.with(func(c *rtCluster) {
			c.createJobAnswers = []kubeAnswer{rtQuotaRefusal(testJobName)}
			c.onJobCreate = func(_ *rtCluster, n int) {
				if n == 1 {
					h.clock.Advance(d)
				}
			}
		})
	}

	for _, c := range []struct {
		name     string
		runLeft  time.Duration // from the runner's clock as the step arrives
		queued   time.Duration
		want     int64
		wantCode string
	}{
		{"a step that never waited is given its whole timeout", 2 * time.Hour, 0, 900, pl.CodeStepTimeout},
		{"so is one that waited longer than its own timeout for a slot", 2 * time.Hour, 20 * time.Minute, 900, pl.CodeStepTimeout},
		{"a run with less left than the step's timeout gives its Job what is left", 25 * time.Minute, 20 * time.Minute, 300, pl.CodeRunCeiling},
		{"what is left of the run counts in whole seconds, never past it", 15*time.Minute + 1500*time.Millisecond, 0, 900, pl.CodeStepTimeout},
		{"part of a second less than the step's timeout is the run's bound", 15*time.Minute - 500*time.Millisecond, 0, 899, pl.CodeRunCeiling},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newRunnerHarness(t)
			h.c.script(testJobName, rtFinishingScript(testJobName, 0, captureKubeLine(rtAt(1100), "done")))
			run := rtRun()
			run.RunDeadline = rtRunDeadline(c.runLeft)
			if c.queued > 0 {
				waitFor(h, c.queued)
			}

			res := h.run(t, run)

			if res.Status != pl.OutcomeSucceeded {
				t.Fatalf("result = %+v (failure %+v), want success: a step's wait for a slot is bounded by its run's "+
					"ceiling, not by its own timeout", res, res.Failure)
			}
			job := h.c.jobNow(t, testJobName)
			if got := deadlineOf(t, h); got != c.want {
				t.Errorf("the Job's deadline is %ds, want %ds: the step's own 900 from the Job's creation, or what is "+
					"left of its run when that is less", got, c.want)
			}
			if got := (&step{run: run}).deadlineCode(job); got != c.wantCode {
				t.Errorf("the Job's deadline is named %q, want %q", got, c.wantCode)
			}
		})
	}

	t.Run("a step still waiting when its run reaches its ceiling fails pipeline_run_ceiling, and nothing is left", func(t *testing.T) {
		h := newRunnerHarness(t)
		h.c.script(testJobName, rtRunningScript(testJobName))
		// The ceiling stays full, and every refused try costs six minutes:
		// the run's ceiling, five minutes away, passes on the first.
		h.c.with(func(c *rtCluster) {
			c.quotaJobs = map[string]bool{testJobName: true}
			c.onJobCreate = func(*rtCluster, int) { h.clock.Advance(6 * time.Minute) }
		})
		run := rtRun()
		run.RunDeadline = rtRunDeadline(5 * time.Minute)

		res := h.run(t, run)

		rtWantCode(t, res, pl.OutcomeFailed, pl.CodeRunCeiling)
		if !strings.Contains(res.Failure.Message, "ceiling") || !strings.Contains(res.Failure.Message, "never started") ||
			!strings.Contains(res.Failure.Message, "free slot") {
			t.Errorf("failure %q, want it to say the run's ceiling passed while the step waited for a free slot, so it never started",
				res.Failure.Message)
		}
		if n := len(h.c.requestsFor(http.MethodPost, kubeJobs)); n != 1 {
			t.Errorf("%d Job creates, want one: refused, after which the run had no time left", n)
		}
		if h.c.hasJob(testJobName) || h.c.hasSecret(testSecretName) {
			t.Error("a step that never started left a Job, or the Secret holding its token")
		}
		h.leftNoArchive(t)
	})

	t.Run("a step that names no run deadline is not started", func(t *testing.T) {
		h := newRunnerHarness(t)
		for _, deadline := range []string{"", "tomorrow"} {
			run := rtRun()
			run.RunDeadline = deadline

			res := h.run(t, run)

			rtWantCode(t, res, pl.OutcomeFailed, pl.CodeExecutorError)
			if !strings.Contains(res.Failure.Message, "run deadline") {
				t.Errorf("RunDeadline %q: failure %q, want it to say the step names no run deadline", deadline, res.Failure.Message)
			}
		}
		if len(h.tokens.called()) != 0 || len(h.c.requestsFor(http.MethodPost, kubeJobs)) != 0 ||
			len(h.c.requestsFor(http.MethodPost, kubeSecrets)) != 0 {
			t.Errorf("tokens minted %q, %d Job and %d Secret creates; want none: a wait with no bound is never begun",
				h.tokens.called(), len(h.c.requestsFor(http.MethodPost, kubeJobs)), len(h.c.requestsFor(http.MethodPost, kubeSecrets)))
		}
	})

	t.Run("a deadline the run's ceiling shortened says so", func(t *testing.T) {
		h := newRunnerHarness(t)
		job := h.existingJob(t, rtRun(), nil)
		job.Spec.ActiveDeadlineSeconds = ptrTo(int64(300))
		h.c.putJob(job, &rtScript{states: []rtState{{job: JobStatus{
			StartTime:  rtT0.Add(-5 * time.Minute),
			Conditions: []JobCondition{{Type: "FailureTarget", Status: "True", Reason: "DeadlineExceeded"}},
		}}}})

		res := h.run(t, rtRun())

		rtWantCode(t, res, pl.OutcomeFailed, pl.CodeRunCeiling)
		if want := "within its deadline of 5m0s, what was left of its run's ceiling when its Job was created, less than its own 15m0s timeout"; !strings.Contains(res.Failure.Message, want) {
			t.Errorf("failure %q, want it to say %q", res.Failure.Message, want)
		}
	})

	t.Run("a deadline that is the step's own timeout is the step's", func(t *testing.T) {
		h := newRunnerHarness(t)
		h.c.putJob(h.existingJob(t, rtRun(), nil), &rtScript{states: []rtState{{job: JobStatus{
			StartTime:  rtT0.Add(-15 * time.Minute),
			Conditions: []JobCondition{{Type: "FailureTarget", Status: "True", Reason: "DeadlineExceeded"}},
		}}}})

		res := h.run(t, rtRun())

		rtWantCode(t, res, pl.OutcomeFailed, pl.CodeStepTimeout)
		if strings.Contains(res.Failure.Message, "ceiling") {
			t.Errorf("failure %q blames the run's ceiling for the step's own timeout", res.Failure.Message)
		}
	})
}

// TestRunnerReapsOrphanedSecrets (fix round 1): a step Secret no Job ever
// owned holds a clone token and the step's secrets, and nothing else would
// collect it. Once it is older than the run's ceiling and the Job TTL past it,
// with its Job gone, the sweep deletes it -- and nothing else: not a young
// one, not an owned one, not one whose Job exists, not one the runner did not
// make, not one whose name is not a step's.
func TestRunnerReapsOrphanedSecrets(t *testing.T) {
	// Old enough is older than the run's ceiling and the Job TTL past it;
	// young is a minute short of it.
	old := rtT0.Add(-(rtConfig().RunCeiling + rtConfig().JobTTL + time.Minute))
	young := rtT0.Add(-(rtConfig().RunCeiling + rtConfig().JobTTL - time.Minute))
	secret := func(job string, created time.Time, mutate ...func(*Secret)) Secret {
		s := BuildSecret(rtConfig(), rtRun(), job, rtCloneToken)
		// These cases exercise the age fallback for pre-deadline metadata.
		delete(s.Metadata.Annotations, AnnotRunDeadline)
		s.Metadata.CreationTimestamp = created
		for _, m := range mutate {
			m(&s)
		}
		return s
	}
	jobName := func(i int) string { return fmt.Sprintf("mp-%024x", i) }

	t.Run("what it deletes and what it keeps", func(t *testing.T) {
		h := newRunnerHarness(t)
		owned := func(s *Secret) {
			s.Metadata.OwnerReferences = []OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: "x", UID: "uid-x"}}
		}
		unlabelled := func(s *Secret) { delete(s.Metadata.Labels, LabelManagedBy) }
		renamed := func(s *Secret) { s.Metadata.Name = "deploy-key-env" }
		h.c.putSecret(secret(jobName(1), old)) // an orphan
		h.c.putSecret(secret(jobName(2), young))
		h.c.putSecret(secret(jobName(3), old, owned))
		h.c.putSecret(secret(jobName(4), old)) // its Job exists
		h.c.putJob(Job{Metadata: ObjectMeta{Name: jobName(4)}}, &rtScript{states: []rtState{{}}})
		h.c.putSecret(secret(jobName(5), old, unlabelled))
		h.c.putSecret(secret(jobName(6), old, renamed))

		if n, _, _ := h.r.reap(context.Background()); n != 1 {
			t.Errorf("the sweep deleted %d Secrets, want the one orphan", n)
		}
		if h.c.hasSecret(SecretName(jobName(1))) {
			t.Error("the orphan was kept")
		}
		for _, name := range []string{SecretName(jobName(2)), SecretName(jobName(3)), SecretName(jobName(4)), SecretName(jobName(5)), "deploy-key-env"} {
			if !h.c.hasSecret(name) {
				t.Errorf("%s was deleted", name)
			}
		}
	})

	t.Run("a sweep deletes at most fifty", func(t *testing.T) {
		h := newRunnerHarness(t)
		const orphans = 150
		for i := 1; i <= orphans; i++ {
			h.c.putSecret(secret(jobName(i), old))
		}

		if n, _, _ := h.r.reap(context.Background()); n != 50 {
			t.Errorf("the sweep deleted %d Secrets, want its bound, 50", n)
		}
		left := 0
		for i := 1; i <= orphans; i++ {
			if h.c.hasSecret(SecretName(jobName(i))) {
				left++
			}
		}
		if left != orphans-50 {
			t.Errorf("%d orphans left, want %d for the next sweeps", left, orphans-50)
		}
		if n, _, _ := h.r.reap(context.Background()); n != 50 {
			t.Errorf("the next sweep deleted %d, want 50 more", n)
		}
	})

	t.Run("a sweep reads at most a thousand Secrets", func(t *testing.T) {
		h := newRunnerHarness(t)
		for i := 1; i <= 1000; i++ {
			h.c.putSecret(secret(jobName(i), young))
		}
		beyond := jobName(1001)
		h.c.putSecret(secret(beyond, old))

		if n, _, _ := h.r.reap(context.Background()); n != 0 {
			t.Errorf("the sweep deleted %d Secrets, want none: the orphan is past its last page", n)
		}
		if n := len(h.c.requestsFor(http.MethodGet, kubeSecrets)); n != 10 {
			t.Errorf("the sweep read %d pages, want ten of a hundred", n)
		}
		if !h.c.hasSecret(SecretName(beyond)) {
			t.Error("the orphan past the sweep's last page was reached")
		}
		if n, more, err := h.r.reap(context.Background()); n != 1 || more || err != nil || h.c.hasSecret(SecretName(beyond)) {
			t.Fatalf("continuation starved the later orphan: deleted=%d more=%v err=%v", n, more, err)
		}
	})

}

func TestOrphanSweepKeepsQueuedSecretUntilItsOwnRunDeadline(t *testing.T) {
	h := newRunnerHarness(t, func(cfg *Config) { cfg.RunCeiling = 20 * time.Minute })
	run := rtRun()
	deadline := rtT0.Add(8 * time.Hour)
	run.RunDeadline = deadline.Format(time.RFC3339Nano)
	job := JobName(run.RunID, run.StepKey, run.Attempt)
	secret := BuildSecret(h.cfg, run, job, rtCloneToken)
	secret.Metadata.CreationTimestamp = rtT0.Add(-2 * time.Hour)
	if secret.Metadata.Annotations[AnnotRunDeadline] != run.RunDeadline {
		t.Fatal("the queued step's Secret lost the agent's run deadline")
	}
	h.c.putSecret(secret)
	if n, _, _ := h.r.reap(context.Background()); n != 0 || !h.c.hasSecret(secret.Metadata.Name) {
		t.Fatal("another replica's shorter ceiling collected a still-queued Secret")
	}
	h.clock.Advance(deadline.Sub(rtT0) + h.cfg.JobTTL)
	if n, _, _ := h.r.reap(context.Background()); n != 0 {
		t.Fatal("the sweep deleted the Secret before the full retention interval elapsed")
	}
	h.clock.Advance(time.Nanosecond)
	if n, _, _ := h.r.reap(context.Background()); n != 1 || h.c.hasSecret(secret.Metadata.Name) {
		t.Fatal("the expired orphan was kept after its run deadline plus TTL")
	}
}

func TestOrphanSweepMalformedDeadlineHasBoundedFallback(t *testing.T) {
	h := newRunnerHarness(t)
	run := rtRun()
	job := JobName(run.RunID, run.StepKey, run.Attempt)
	secret := BuildSecret(h.cfg, run, job, rtCloneToken)
	secret.Metadata.CreationTimestamp = rtT0.Add(-h.cfg.RunCeiling - h.cfg.JobTTL - time.Minute)
	secret.Metadata.Annotations[AnnotRunDeadline] = "not a timestamp"
	h.c.putSecret(secret)
	if n, _, _ := h.r.reap(context.Background()); n != 1 {
		t.Fatal("malformed deadline kept an orphan's credentials indefinitely")
	}
}

// TestTheAgentsGraceOutlastsSettling (fix round 2, minor 2): the agent gives a
// step up its deadline and nodeLostGrace after it was handed over, then acks
// its Job away. Settling a decided step -- every phase at its deadline -- must
// be over by then, or the outcome being recorded is deleted under the runner
// and reported as a deadline failure. The grace is derived from the
// runner's budget, with room past it for the poll that sees the step
// decided, the Job controller's mark and the reply.
func TestTheAgentsGraceOutlastsSettling(t *testing.T) {
	if margin := nodeLostGrace - settleBudget; margin < 30*time.Second {
		t.Errorf("nodeLostGrace %v leaves %v past settleBudget %v, want at least 30s for the poll, the mark and the reply", nodeLostGrace, margin, settleBudget)
	}
	if NewExecutor(exConfig(), &scriptedWorkbench{}, nil, quietLogger()).lostGrace != nodeLostGrace {
		t.Error("a new executor does not wait nodeLostGrace")
	}
	if r := NewRunner(rtConfig(), nil, nil, nil, nil); r.tailsTimeout != tailsTimeout || r.libraryTimeout != libraryPhaseTimeout || r.drainTimeout != followDrainTimeout {
		t.Error("a new runner's phases are not the ones settleBudget adds up")
	}
}

// TestRunnerTailsShareOneWindow (fix round 2, minor 2): a failed step's
// clone and every service are tailed within ONE window, however many
// services it has: a kubelet that answers none of them costs the window
// once, never once per service, so settling stays within its budget.
//
// It measures the DIFFERENCE between the same failed step with its tails
// answered and with all seven blocked, both under whatever load the machine
// is carrying: one shared window adds about one window, a window per tail
// would add seven. An absolute bound on the whole run measured the load too,
// and failed under a full parallel `make test`.
func TestRunnerTailsShareOneWindow(t *testing.T) {
	const window = 300 * time.Millisecond
	settle := func(block bool) (*rtHarness, pl.StepResult, time.Duration) {
		h := newRunnerHarness(t)
		h.r.tailsTimeout = window
		run := rtRun()
		run.Services = map[string]pl.Service{}
		for _, name := range []string{"a", "b", "c", "d", "e", "f"} {
			run.Services[name] = pl.Service{Image: "redis:7"}
		}
		h.c.script(testJobName, rtFinishingScript(testJobName, 1, captureKubeLine(rtAt(1100), "failing")))
		h.c.with(func(c *rtCluster) { c.blockTails = block })
		start := time.Now()
		res := h.run(t, run)
		return h, res, time.Since(start)
	}

	_, _, answered := settle(false)
	h, res, blocked := settle(true)

	if res.Status != pl.OutcomeFailed || res.ExitCode != 1 {
		t.Fatalf("result = %+v, want the step's own failure", res)
	}
	if extra := blocked - answered; extra > 3*window {
		t.Errorf("seven unanswered tails added %v to settling (%v blocked, %v answered), want about one %v window, not one each",
			extra, blocked, answered, window)
	}
	if archive := string(h.file(t, "tests-go-tests-2.log").Bytes); strings.Count(archive, "could not be read") != 7 {
		t.Errorf("archive = %q, want each of the seven tails noted as not read", archive)
	}
}

// TestRunnerCloneTokenFailureCreatesNothing: without a clone token there is
// no clone, so there is no Job, and no Secret waiting for one.
func TestRunnerCloneTokenFailureCreatesNothing(t *testing.T) {
	h := newRunnerHarness(t)
	h.tokens.err = errors.New("installation 42 is suspended")

	res := h.run(t, rtRun())

	rtWantCode(t, res, pl.OutcomeFailed, pl.CodeCloneFailed)
	if !strings.Contains(res.Failure.Message, "installation 42 is suspended") {
		t.Errorf("failure %q does not say why the token could not be minted", res.Failure.Message)
	}
	for _, request := range h.c.requests() {
		if request.Method != http.MethodGet {
			t.Fatalf("token failure mutated Kubernetes: %s %s", request.Method, request.Path)
		}
	}
	if len(h.lib.stored()) != 0 {
		t.Error("a step that never ran stored a file")
	}
	h.leftNoArchive(t)
}

// TestRunnerRefusedStepCreatesNothing: what the runner refuses, it refuses
// before anything exists for it.
func TestRunnerRefusedStepCreatesNothing(t *testing.T) {
	t.Run("a step BuildJob refuses is refused with its code, before a token is minted", func(t *testing.T) {
		h := newRunnerHarness(t)
		run := rtRun()
		run.Image = ""

		res := h.run(t, run)

		rtWantCode(t, res, pl.OutcomeRefused, pl.CodeJobRejected)
		if got := h.c.summary(); got != "GET "+rtJobPath {
			t.Errorf("requests:\n  %s\nwant only the read that found no Job", got)
		}
		if len(h.tokens.called()) != 0 {
			t.Error("a refused step minted a clone token")
		}
	})

	t.Run("a node without an id claims nothing, so it runs nothing", func(t *testing.T) {
		h := newRunnerHarness(t, func(c *Config) { c.NodeID = " " })

		res := h.run(t, rtRun())

		rtWantCode(t, res, pl.OutcomeFailed, pl.CodeRunnerUnavailable)
		if n := len(h.c.requests()); n != 0 {
			t.Errorf("%d requests, want none", n)
		}
	})
}

// TestRunnerReportsWhatTheAPIServerRefuses: a refusal the API server will
// repeat fails the step at once, typed, and leaves nothing behind; one that
// may pass is tried again.
func TestRunnerReportsWhatTheAPIServerRefuses(t *testing.T) {
	t.Run("a read this node may not make fails the step, creating nothing", func(t *testing.T) {
		h := newRunnerHarness(t)
		h.c.with(func(c *rtCluster) {
			c.jobGetFailures = []kubeAnswer{kubeStatus(403, "Forbidden", `jobs.batch "x" is forbidden: User "system:serviceaccount:memql:memql-engine" cannot get resource "jobs"`)}
		})

		res := h.run(t, rtRun())

		rtWantCode(t, res, pl.OutcomeFailed, pl.CodeRunnerUnavailable)
		if !strings.Contains(res.Failure.Message, "cannot get resource") {
			t.Errorf("failure %q does not carry the API server's sentence", res.Failure.Message)
		}
		rtNoRequests(t, h.c, http.MethodPost, kubeJobs)
		rtNoRequests(t, h.c, http.MethodPost, kubeSecrets)
	})

	t.Run("an answer that may pass is tried again", func(t *testing.T) {
		h := newRunnerHarness(t)
		unavailable := kubeStatus(503, "ServiceUnavailable", "the server is currently unable to handle the request")
		h.c.with(func(c *rtCluster) { c.jobGetFailures = []kubeAnswer{unavailable, unavailable} })
		h.c.script(testJobName, rtFinishingScript(testJobName, 0, captureKubeLine(rtAt(1100), "ok")))

		res := h.run(t, rtRun())

		if res.Status != pl.OutcomeSucceeded {
			t.Fatalf("result = %+v (failure %+v), want success after two 503s", res, res.Failure)
		}
	})

	for _, c := range []struct {
		name   string
		answer kubeAnswer
		code   string
	}{
		{"a Job the cluster refuses is a rejected Job", kubeStatus(422, "Invalid", `Job.batch "x" is invalid: spec.template.spec.containers[0].image: Required value`), pl.CodeJobRejected},
		{"a node that may not create Jobs has no runner", kubeStatus(403, "Forbidden", `jobs.batch is forbidden: User "system:serviceaccount:memql:memql-engine" cannot create resource "jobs"`), pl.CodeRunnerUnavailable},
	} {
		t.Run(c.name+", and its Secret is deleted", func(t *testing.T) {
			h := newRunnerHarness(t)
			answer := c.answer
			h.c.with(func(f *rtCluster) { f.createJobAnswer = &answer })

			res := h.run(t, rtRun())

			rtWantCode(t, res, pl.OutcomeFailed, c.code)
			if h.c.hasSecret(testSecretName) {
				t.Error("the Secret holding the clone token outlived the Job that was never created")
			}
			h.leftNoArchive(t)
		})
	}
}

// TestRunnerFreshensACloneTokenThatAgedInTheQueue (fix round 1, minor 7): the
// token is minted before the wait under the ceiling, which can outlast it. One
// older than tokenRefreshAge when the Job is finally created is minted again
// and written into the Secret first -- and masked like the first.
func TestRunnerFreshensACloneTokenThatAgedInTheQueue(t *testing.T) {
	const first, second = "ghs_first-token-0001", "ghs_second-token-0002"
	h := newRunnerHarness(t)
	h.tokens.seq = []string{first, second}
	h.c.with(func(c *rtCluster) {
		// One refusal, and the wait it costs outlasts the first token.
		c.createJobAnswers = []kubeAnswer{rtQuotaRefusal(testJobName)}
		c.onJobCreate = func(_ *rtCluster, n int) {
			if n == 1 {
				h.clock.Advance(tokenRefreshAge + time.Minute)
			}
		}
	})
	script := rtFinishingScript(testJobName, 0, captureKubeLine(rtAt(1100), "done"))
	script.tails = map[string]string{ContainerClone: "fetching with " + second + "\n" + rtCloneTail}
	h.c.script(testJobName, script)

	res := h.run(t, rtRun())

	if res.Status != pl.OutcomeSucceeded {
		t.Fatalf("result = %+v (failure %+v), want success", res, res.Failure)
	}
	if got := h.tokens.called(); len(got) != 2 {
		t.Errorf("%d tokens minted, want the first and one more once it had aged", len(got))
	}
	if got := string(h.c.secretNow(t, testSecretName).Data[gitTokenKey]); got != second {
		t.Errorf("the Secret holds %q, want the token minted again", got)
	}
	// Written before the create that was admitted, so the clone has it.
	var written, admitted int
	for i, r := range h.c.requests() {
		switch {
		case r.Method == http.MethodPatch && r.Path == kubeSecrets+"/"+testSecretName && strings.Contains(r.Body, `"data"`):
			written = i
		case r.Method == http.MethodPost && r.Path == kubeJobs:
			admitted = i
		}
	}
	if written == 0 || written > admitted {
		t.Errorf("the token was written at request %d and the Job admitted at %d: the clone must find the fresh one", written, admitted)
	}
	if archive := string(h.file(t, "tests-go-tests-2.log").Bytes); strings.Contains(archive, second) || !strings.Contains(archive, "fetching with ***") {
		t.Errorf("archive = %q, want the fresh token masked in the clone's output", archive)
	}
}

// TestRunnerWritesItsTokenOverAReusedSecret (fix round 1, minor 7): a Secret an
// earlier Run of the step left holds that Run's token, of any age. The Run
// writes its own over it before creating the Job, and leaves the step's
// secrets beside it as they are.
func TestRunnerWritesItsTokenOverAReusedSecret(t *testing.T) {
	h := newRunnerHarness(t)
	h.c.putSecret(BuildSecret(h.cfg, rtRun(), testJobName, "ghs_left-by-an-earlier-run"))
	h.c.script(testJobName, rtFinishingScript(testJobName, 0, captureKubeLine(rtAt(1100), "done")))

	res := h.run(t, rtRun())

	if res.Status != pl.OutcomeSucceeded {
		t.Fatalf("result = %+v (failure %+v), want success", res, res.Failure)
	}
	secret := h.c.secretNow(t, testSecretName)
	if got := string(secret.Data[gitTokenKey]); got != rtCloneToken {
		t.Errorf("the Secret holds %q, want this Run's token written over the earlier one", got)
	}
	if string(secret.Data["NPM_TOKEN"]) != plantedNPM {
		t.Errorf("the Secret holds %q, want the step's secrets kept", keysOf(secret.Data))
	}
	// Written before the Job is created, so the clone has it.
	written, created := -1, -1
	for i, r := range h.c.requests() {
		switch {
		case r.Method == http.MethodPatch && r.Path == kubeSecrets+"/"+testSecretName && strings.Contains(r.Body, `"data"`) && written < 0:
			written = i
		case r.Method == http.MethodPost && r.Path == kubeJobs && created < 0:
			created = i
		}
	}
	if written < 0 || written > created {
		t.Errorf("the token was written at request %d and the Job created at %d: the clone must find this Run's token", written, created)
	}
}

// TestRunnerMasksTheTokenTheSecretHolds (fix round 1, minor 8): a Run that
// adopts a step minted no token; the clone ran with the creator's, which the
// step's Secret holds. It is read before anything the clone printed is masked:
// the clone's tail in the archive, and a failure's message.
func TestRunnerMasksTheTokenTheSecretHolds(t *testing.T) {
	const creators = "ghs_creators-token-0099"
	stale := map[string]string{AnnotRunner: rtStamp(rtOther, rtT0.Add(-time.Minute))}

	t.Run("in the clone's output", func(t *testing.T) {
		h := newRunnerHarness(t)
		run := rtRun()
		h.c.putSecret(BuildSecret(h.cfg, run, testJobName, creators))
		script := rtFinishingScript(testJobName, 0, captureKubeLine(rtAt(1100), "done"))
		script.tails = map[string]string{ContainerClone: "fetching with " + creators + "\n" + rtCloneTail}
		h.c.putJob(h.existingJob(t, run, stale), script)

		res := h.run(t, run)

		if res.Status != pl.OutcomeSucceeded {
			t.Fatalf("result = %+v (failure %+v), want success", res, res.Failure)
		}
		if len(h.tokens.called()) != 0 {
			t.Error("an adopter minted a clone token")
		}
		if archive := string(h.file(t, "tests-go-tests-2.log").Bytes); strings.Contains(archive, creators) || !strings.Contains(archive, "fetching with ***") {
			t.Errorf("archive = %q, want the creator's token masked in the clone's output", archive)
		}
	})

	t.Run("in a failure's message", func(t *testing.T) {
		h := newRunnerHarness(t)
		run := rtRun()
		h.c.putSecret(BuildSecret(h.cfg, run, testJobName, creators))
		clone := clsTerminated(ContainerClone, 128, "Error", "fatal: could not read from https://x-access-token:"+creators+"@github.com", rtAt(2000))
		waiting := ContainerStatus{Name: ContainerStep, State: ContainerState{Waiting: &ContainerStateWaiting{Reason: "PodInitializing"}}}
		h.c.putJob(h.existingJob(t, run, stale), &rtScript{
			states: []rtState{{pod: rtPod(testJobName, waiting, clone)}},
			tails:  map[string]string{ContainerClone: "fatal: could not read from https://x-access-token:" + creators + "@github.com\n"},
		})

		res := h.run(t, run)

		rtWantCode(t, res, pl.OutcomeFailed, pl.CodeCloneFailed)
		if strings.Contains(res.Failure.Message, creators) || !strings.Contains(res.Failure.Message, "x-access-token:***@") {
			t.Errorf("failure %q, want the creator's token masked", res.Failure.Message)
		}
		recorded := h.c.jobNow(t, testJobName).Metadata.Annotations[AnnotObservation]
		if recorded == "" || strings.Contains(recorded, creators) {
			t.Errorf("observation recorded on the Job = %q, want one, with the token masked", recorded)
		}
	})

	t.Run("a Secret that is gone leaves the mask without it", func(t *testing.T) {
		h := newRunnerHarness(t)
		run := rtRun()
		h.c.putJob(h.existingJob(t, run, stale), rtFinishingScript(testJobName, 0, captureKubeLine(rtAt(1100), "done")))

		res := h.run(t, run)

		if res.Status != pl.OutcomeSucceeded || res.Notes != nil {
			t.Fatalf("result = %+v (failure %+v), want a plain success", res, res.Failure)
		}
	})
}

// TestRunnerKeepsAskingForAJobItHasSeen (fix round 1, minor 5): once a Run
// has seen the step's Job exist, an API server that cannot answer for a while
// is waited out, never a failure -- the Job runs on, under this Run or another
// replica's. Here the creator's first claim is refused (the Job controller
// wrote the Job's status since the create answered), and its next reads of
// the Job meet more failures than one read retries.
func TestRunnerKeepsAskingForAJobItHasSeen(t *testing.T) {
	h := newRunnerHarness(t)
	logs := h.logs()
	armed := false
	h.c.with(func(c *rtCluster) {
		c.onJobPatch = func(c *rtCluster, p rtPatch) {
			if !armed {
				armed = true
				for i := 0; i < 2*apiAttempts+1; i++ {
					c.jobGetFailures = append(c.jobGetFailures, rtUnavailable)
				}
			}
		}
	})
	h.c.script(testJobName, rtFinishingScript(testJobName, 0, captureKubeLine(rtAt(1100), "ok")))

	res := h.run(t, rtRun())

	if res.Status != pl.OutcomeSucceeded {
		t.Fatalf("result = %+v (failure %+v), want success: the Job ran on while the API server could not say so", res, res.Failure)
	}
	h.c.with(func(c *rtCluster) {
		if !armed || len(c.jobGetFailures) != 0 {
			t.Errorf("armed %v, %d failures left: the Run never met them", armed, len(c.jobGetFailures))
		}
	})
	if n := logs.count("pipelines: reading the step's Job"); n != 1 {
		t.Errorf("%d warnings of the failed reads, want one: the same error is logged once\n%s", n, logs)
	}
}

// TestRunnerCountsFailuresThatMayPassFromTheLastAnswer (fix round 1, minor 6):
// a quota refusal is the API server answering, so the failures that may pass
// are counted from it -- a step waiting out the ceiling is not failed for
// explicit throttling refusals spread over its whole wait. A 5xx on a create
// is ambiguous and is deliberately not safe to retry after a missing Job.
func TestRunnerCountsFailuresThatMayPassFromTheLastAnswer(t *testing.T) {
	h := newRunnerHarness(t)
	var answers []kubeAnswer
	for i := 0; i < 2*apiAttempts; i++ {
		answers = append(answers, kubeStatus(429, "TooManyRequests", "request throttled before admission"), rtQuotaRefusal(testJobName))
	}
	h.c.with(func(c *rtCluster) { c.createJobAnswers = answers })
	h.c.script(testJobName, rtFinishingScript(testJobName, 0, captureKubeLine(rtAt(1100), "done")))

	res := h.run(t, rtRun())

	if res.Status != pl.OutcomeSucceeded {
		t.Fatalf("result = %+v (failure %+v), want success: no run of failures was longer than %d", res, res.Failure, apiAttempts)
	}
	if n := len(h.c.requestsFor(http.MethodPost, kubeJobs)); n != len(answers)+1 {
		t.Errorf("%d Job creates, want %d refused or failed and one admitted", n, len(answers))
	}
	if store := h.sink.messages(); len(store) != 2 || store[1] != "done" {
		t.Errorf("store = %q, want one notice of the wait, then the step's output", store)
	}
}

// TestRunnerLogsAPersistentAPIErrorOnce (fix round 1, minor 12): an API error
// a loop keeps meeting is logged when it appears and when what it says
// changes, not once a poll interval.
func TestRunnerLogsAPersistentAPIErrorOnce(t *testing.T) {
	t.Run("the watch's reads of the Job", func(t *testing.T) {
		h := newRunnerHarness(t)
		logs := h.logs()
		armed := false
		h.c.with(func(c *rtCluster) {
			c.onJobGet = func(c *rtCluster, n int) {
				if !armed && len(c.patches) > 0 {
					// Claimed: the watch is under way.
					armed = true
					for i := 0; i < 40; i++ {
						c.jobGetFailures = append(c.jobGetFailures, rtUnavailable)
					}
				}
			}
		})
		h.c.script(testJobName, rtFinishingScript(testJobName, 0, captureKubeLine(rtAt(1100), "ok")))

		res := h.run(t, rtRun())

		if res.Status != pl.OutcomeSucceeded {
			t.Fatalf("result = %+v (failure %+v), want success", res, res.Failure)
		}
		if n := logs.count("pipelines: reading the step's Job"); n != 1 {
			t.Errorf("%d warnings of 40 failed reads, want one\n%s", n, logs)
		}
	})

	t.Run("the heartbeat's stamps", func(t *testing.T) {
		h := newRunnerHarness(t)
		logs := h.logs()
		released := false
		running := rtPod(testJobName, rtStepRunning(rtAt(1000)), clsCloneDone)
		h.c.script(testJobName, &rtScript{
			states: []rtState{
				{pod: running, visible: 1, until: func(*rtCluster) bool { return released }},
				{pod: rtPod(testJobName, rtStepEnded(0, rtAt(1000), rtAt(4000)), clsCloneDone), visible: 1},
			},
			log:   []string{captureKubeLine(rtAt(1100), "ok")},
			tails: map[string]string{ContainerClone: rtCloneTail},
		})
		h.c.with(func(c *rtCluster) { c.refuseBeats = true })
		done := h.start(context.Background(), rtRun())
		rtWaitUntil(t, "ten refused heartbeats", func() bool {
			n := 0
			for _, r := range h.c.requestsFor(http.MethodPatch, rtJobPath) {
				if strings.Contains(r.Body, AnnotLogCursor) {
					n++
				}
			}
			return n >= 10
		})
		h.c.with(func(*rtCluster) { released = true })

		res := h.await(t, done)

		if res.Status != pl.OutcomeSucceeded {
			t.Fatalf("result = %+v (failure %+v), want success", res, res.Failure)
		}
		if n := logs.count("pipelines: the step's claim could not be stamped"); n != 1 {
			t.Errorf("%d warnings of ten refused stamps, want one\n%s", n, logs)
		}
	})
}

// ---------------------------------------------------------------------------
// Cancelling
// ---------------------------------------------------------------------------

// TestRunnerCancelDeletesTheJobAndSecret: a done context is the agent
// cancelling the step. The runner deletes the Job and its Secret and answers
// cancelled, with what it captured archived -- unless the outcome was already
// persisted, which a cancel no longer changes.
func TestRunnerCancelDeletesTheJobAndSecret(t *testing.T) {
	lines := []string{captureKubeLine(rtAt(1100), "out 1"), captureKubeLine(rtAt(1200), "out 2")}

	t.Run("a cancelled step's Job and Secret are deleted and its output archived", func(t *testing.T) {
		h := newRunnerHarness(t)
		h.c.script(testJobName, rtRunningScript(testJobName, lines...))
		ctx, cancel := context.WithCancel(context.Background())
		done := h.start(ctx, rtRun())
		rtWaitUntil(t, "the step's output", func() bool { return len(h.sink.messages()) == 2 })
		cancel()

		res := h.await(t, done)

		rtWantCode(t, res, pl.OutcomeCancelled, pl.CodeStepCancelled)
		if res.LogLines != 2 || res.LogFileID == "" || res.Where.JobName != testJobName {
			t.Errorf("result = %+v, want the two lines captured, archived, and where they ran", res)
		}
		if h.c.hasJob(testJobName) || h.c.hasSecret(testSecretName) {
			t.Error("the cancelled step's Job or Secret is still there")
		}
		archive := string(h.file(t, "tests-go-tests-2.log").Bytes)
		if !strings.HasPrefix(archive, "out 1\nout 2\n") || !strings.Contains(archive, "cancelled") {
			t.Errorf("archive = %q, want the output and a word that the step was cancelled", archive)
		}
		h.leftNoArchive(t)
	})

	t.Run("a Job deleted under it by another replica's cancel ends it cancelled", func(t *testing.T) {
		h := newRunnerHarness(t)
		h.c.script(testJobName, rtRunningScript(testJobName, lines...))
		done := h.start(context.Background(), rtRun())
		rtWaitUntil(t, "the step's output", func() bool { return len(h.sink.messages()) == 2 })
		h.c.with(func(c *rtCluster) { c.deleteJobLocked(testJobName) })

		res := h.await(t, done)

		rtWantCode(t, res, pl.OutcomeCancelled, pl.CodeStepCancelled)
		if res.LogFileID == "" {
			t.Error("the output of a step cancelled elsewhere was not archived")
		}
		rtNoRequests(t, h.c, http.MethodDelete, rtJobPath)
	})

	t.Run("a cancel after the outcome is persisted changes nothing", func(t *testing.T) {
		outcome := pl.StepResult{Status: pl.OutcomeSucceeded, Where: pl.Where{Surface: "cluster", NodeID: rtOther, JobName: testJobName}, LogFileID: "file-2"}
		h := newRunnerHarness(t)
		h.c.putJob(h.existingJob(t, rtRun(), map[string]string{AnnotRunner: rtStamp(rtOther, rtT0.Add(-time.Second))}), rtRunningScript(testJobName))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// The cancel arrives while it waits; by the time it reads the Job
		// to cancel the step, the holder has persisted the outcome.
		h.c.onJobGet = func(c *rtCluster, n int) {
			switch n {
			case 3:
				cancel()
			case 4:
				c.annotateLocked(testJobName, AnnotOutcome, mustJSON(t, outcome))
			}
		}

		res := h.await(t, h.start(ctx, rtRun()))

		if !reflect.DeepEqual(res, outcome) {
			t.Errorf("result = %+v, want the persisted outcome %+v", res, outcome)
		}
		rtNoRequests(t, h.c, http.MethodDelete, rtJobPath)
		rtNoRequests(t, h.c, http.MethodDelete, kubeSecrets+"/"+testSecretName)
	})
}

// TestCancelRunStopsThisReplicasRunsAndDeletesTheRun: a run's cancel deletes
// every Job and Secret of the run, wherever they were created, and stops this
// replica's own Runs of it -- a step still waiting for a slot under the
// ceiling has no Job to delete, and would otherwise create one -- and touches
// nothing of another run.
func TestCancelRunStopsThisReplicasRunsAndDeletesTheRun(t *testing.T) {
	h := newRunnerHarness(t)
	a := rtRun()
	b := rtRun()
	b.StepKey, b.Attempt = "tests/lint", 1
	other := rtRun()
	other.RunID = "run-0b9c"
	for _, run := range []StepRun{a, b, other} {
		name := JobName(run.RunID, run.StepKey, run.Attempt)
		h.c.script(name, rtRunningScript(name, captureKubeLine(rtAt(1100), "working on "+run.StepKey)))
	}
	bJob := JobName(b.RunID, b.StepKey, b.Attempt)
	h.c.with(func(c *rtCluster) { c.quotaJobs = map[string]bool{bJob: true} })
	otherCtx, stopOther := context.WithCancel(context.Background())
	defer stopOther()
	doneA := h.start(context.Background(), a)
	doneB := h.start(context.Background(), b)
	doneOther := h.start(otherCtx, other)
	rtWaitUntil(t, "two steps running and one queued", func() bool {
		return len(h.sink.messages()) == 3 && len(h.c.requestsFor(http.MethodPost, kubeJobs)) >= 3
	})

	n, err := h.r.CancelRun(context.Background(), CancelRequest{RunID: "run-7f3a"})

	if err != nil || n != 1 {
		t.Errorf("CancelRun = %d, %v; want the run's one Job: its other step had none yet", n, err)
	}
	for _, done := range []<-chan pl.StepResult{doneA, doneB} {
		rtWantCode(t, h.await(t, done), pl.OutcomeCancelled, pl.CodeStepCancelled)
	}
	if h.c.hasJob(bJob) || h.c.hasSecret(SecretName(bJob)) {
		t.Error("the queued step of the cancelled run left a Job or a Secret")
	}
	otherJob := JobName(other.RunID, other.StepKey, other.Attempt)
	if !h.c.hasJob(otherJob) || !h.c.hasSecret(SecretName(otherJob)) {
		t.Error("cancelling one run deleted another run's Job or Secret")
	}
	if !h.c.hasSecret(runRetirementName(a.RunID)) {
		t.Fatal("run cancellation left no durable stop marker")
	}
	for _, request := range h.c.requests() {
		if request.Method == http.MethodDelete && (request.Path == kubeJobs || request.Path == kubeSecrets) {
			t.Fatal("run cancellation erased creation evidence with a collection delete")
		}
	}
	select {
	case res := <-doneOther:
		t.Fatalf("the other run's step ended (%+v): its context was not cancelled", res)
	default:
	}
	stopOther()
	rtWantCode(t, h.await(t, doneOther), pl.OutcomeCancelled, pl.CodeStepCancelled)

	t.Run("a cancel naming no run deletes nothing", func(t *testing.T) {
		h := newRunnerHarness(t)
		if _, err := h.r.CancelRun(context.Background(), CancelRequest{RunID: " "}); err == nil {
			t.Error("CancelRun accepted a blank run id")
		}
		if n := len(h.c.requests()); n != 0 {
			t.Errorf("%d requests, want none", n)
		}
	})
}

// ---------------------------------------------------------------------------
// The Library
// ---------------------------------------------------------------------------

// rtArtifactStream supplies the verified collection boundary to runner tests.
// The transport/identity seam is tested separately against its API and real K3s.
func rtArtifactStream(t *testing.T, h *rtHarness, tgz []byte) {
	t.Helper()
	h.r.collectArtifacts = func(ctx context.Context, run StepRun, _ *Pod, maxBytes int64) (*ArtifactSnapshot, error) {
		reader, err := gzip.NewReader(bytes.NewReader(tgz))
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		return SnapshotArtifacts(ctx, reader, run.Artifacts, maxBytes)
	}
}

// TestRunnerReadsGoTimings (fix round 1): a Go test step's passing packages'
// times come out of its archived log -- anchored result lines, never a cached
// result or a failed package -- and ride its outcome; a log they cannot be
// read from is a note beside the step, never its failure.
func TestRunnerReadsGoTimings(t *testing.T) {
	lines := []string{
		captureKubeLine(rtAt(1100), "ok  \tgithub.com/acme/widget/a\t1.500s"),
		captureKubeLine(rtAt(1200), "ok  \tgithub.com/acme/widget/b\t(cached)"),
		captureKubeLine(rtAt(1300), "FAIL\tgithub.com/acme/widget/c\t2.000s"),
	}
	for _, c := range []struct {
		name      string
		goTimings bool
		extra     string
		want      map[string]float64
		note      bool
	}{
		{"a Go test step's passing packages", true, "", map[string]float64{"github.com/acme/widget/a": 1.5}, false},
		{"a step that runs no Go tests reads none", false, "", nil, false},
		{"a result line that cannot be read is a note", true, "ok  \tgithub.com/acme/widget/d\t" + strings.Repeat("9", 400) + "s", nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newRunnerHarness(t)
			run := rtRun()
			run.GoTimings = c.goTimings
			script := append([]string(nil), lines...)
			if c.extra != "" {
				script = append(script, captureKubeLine(rtAt(1400), c.extra))
			}
			h.c.script(testJobName, rtFinishingScript(testJobName, 0, script...))

			res := h.run(t, run)

			if res.Status != pl.OutcomeSucceeded || res.Failure != nil {
				t.Fatalf("result = %+v (failure %+v), want success: timings never fail a step", res, res.Failure)
			}
			if !reflect.DeepEqual(res.Timings, c.want) {
				t.Errorf("Timings = %v, want %v", res.Timings, c.want)
			}
			noted := len(res.Notes) == 1 && res.Notes[0].Code == pl.CodeTimingsUnreadable && strings.Contains(res.Notes[0].Message, "Go test timings")
			if noted != c.note || (!c.note && len(res.Notes) != 0) {
				t.Errorf("notes = %+v, want a note of the unreadable timings: %v", res.Notes, c.note)
			}
			var recorded pl.StepResult
			if err := json.Unmarshal([]byte(h.c.jobNow(t, testJobName).Metadata.Annotations[AnnotOutcome]), &recorded); err != nil || !reflect.DeepEqual(recorded.Timings, c.want) {
				t.Errorf("the outcome recorded on the Job has Timings %v (%v), want %v", recorded.Timings, err, c.want)
			}
		})
	}
}

// TestRunnerFitsItsOutcomeInAJobAnnotation (fix round 1, minor 13): all of a
// Job's annotations share 256 KiB, and an outcome the API server refused would
// be lost. The two lists in it nothing bounds -- the artifact file ids and the
// Go timings -- are cut to fit, each with a note saying how many were left
// out; the log's file id stays. The fake API server refuses an annotation
// past the cap as a real one does, and fails the test.
func TestRunnerFitsItsOutcomeInAJobAnnotation(t *testing.T) {
	persistedOutcome := func(t *testing.T, h *rtHarness, res pl.StepResult) {
		t.Helper()
		raw := h.c.jobNow(t, testJobName).Metadata.Annotations[AnnotOutcome]
		var recorded pl.StepResult
		if err := json.Unmarshal([]byte(raw), &recorded); err != nil || !reflect.DeepEqual(recorded, res) {
			t.Fatalf("the outcome recorded on the Job (%d bytes, %v) is not the one Run answered", len(raw), err)
		}
		if len(raw) > outcomeMaxBytes {
			t.Errorf("the outcome is %d bytes, over outcomeMaxBytes (%d)", len(raw), outcomeMaxBytes)
		}
	}

	t.Run("artifact file ids", func(t *testing.T) {
		const files = 400
		h := newRunnerHarness(t)
		h.lib.idPad = "-" + strings.Repeat("i", 800)
		run := rtRun()
		run.Artifacts = []string{"dist/*"}
		var entries []extractTestEntry
		for i := 0; i < files; i++ {
			entries = append(entries, extractTestEntry{name: fmt.Sprintf("dist/f%04d.txt", i), body: "x"})
		}
		rtArtifactStream(t, h, extractTestTgz(t, entries))
		h.c.script(testJobName, rtFinishingScript(testJobName, 0, captureKubeLine(rtAt(1100), "ok")))

		res := h.run(t, run)

		persistedOutcome(t, h, res)
		if res.Status != pl.OutcomeSucceeded || res.LogFileID != "file-1"+h.lib.idPad {
			t.Errorf("status %s, log file %.20q...; want a success that keeps its log", res.Status, res.LogFileID)
		}
		kept := len(res.ArtifactFileIDs)
		if len(res.ArtifactIntentIDs) != files {
			t.Fatalf("trimmed durable evidence identities: got %d, want %d", len(res.ArtifactIntentIDs), files)
		}
		if kept == 0 || kept == files {
			t.Fatalf("%d of %d artifact file ids kept, want as many as fit", kept, files)
		}
		for i, id := range res.ArtifactFileIDs {
			if id != fmt.Sprintf("file-%d", i+2)+h.lib.idPad {
				t.Fatalf("artifact file id %d is %.20q..., want the first ones stored, in order", i, id)
			}
		}
		want := fmt.Sprintf("%d of the step's %d artifact file ids were left out of its outcome", files-kept, files)
		if len(res.Notes) == 0 || !strings.HasPrefix(res.Notes[0].Message, want) || res.Notes[0].Code != pl.CodeOutcomeTrimmed {
			t.Errorf("notes = %+v, want the first to say %q", res.Notes, want)
		}
	})

	t.Run("Go timings", func(t *testing.T) {
		const packages = 5000
		h := newRunnerHarness(t)
		run := rtRun()
		run.GoTimings = true
		var lines []string
		for i := 0; i < packages; i++ {
			lines = append(lines, captureKubeLine(rtAt(1100+i), fmt.Sprintf("ok  \tgithub.com/acme/widget/internal/generated/clients/v%04d\t0.%03ds", i, i%1000)))
		}
		h.c.script(testJobName, rtFinishingScript(testJobName, 0, lines...))

		res := h.run(t, run)

		persistedOutcome(t, h, res)
		kept := len(res.Timings)
		if kept == 0 || kept == packages {
			t.Fatalf("%d of %d timings kept, want as many as fit", kept, packages)
		}
		for i := 0; i < kept; i++ {
			if _, ok := res.Timings[fmt.Sprintf("github.com/acme/widget/internal/generated/clients/v%04d", i)]; !ok {
				t.Fatalf("package %d's timing was cut while a later one was kept: want the first by path", i)
			}
		}
		want := fmt.Sprintf("the Go test timings of %d of the step's %d passing packages were left out of its outcome", packages-kept, packages)
		if len(res.Notes) == 0 || !strings.HasPrefix(res.Notes[0].Message, want) || res.Notes[0].Code != pl.CodeOutcomeTrimmed {
			t.Errorf("notes = %+v, want the first to say %q", res.Notes, want)
		}
	})
}

// TestWhatTheCutsLeaveAlwaysFits: fitOutcome cuts only the artifact file ids
// and the Go timings, because the rest of an outcome is bounded where it is
// made. With neither, the largest outcome those bounds allow -- a full tail,
// full notes and a full failure, every byte one JSON spends six on -- fits in
// outcomeMaxBytes, and the largest observation, the claim, the cursor and the
// step's identity fit in what is left beside it. A bound raised past this
// needs a cut of its own.
func TestWhatTheCutsLeaveAlwaysFits(t *testing.T) {
	esc := func(n int) string { return strings.Repeat("<", n) }
	notes := &noteList{mask: func(s string) string { return s }}
	for i := 0; i <= maxNotes; i++ {
		notes.add(pl.CodeArtifactMissing, esc(notesMaxBytes/maxNotes))
	}
	failure := &pl.Failure{Code: pl.CodeServiceFailed, Message: esc(failureMaxBytes)}
	res := pl.StepResult{
		Status: pl.OutcomeFailed, ExitCode: 137, Failure: failure,
		StartedAt: rtT0.Format(time.RFC3339), FinishedAt: rtT0.Format(time.RFC3339),
		Where:     pl.Where{Surface: "cluster", NodeID: strings.Repeat("n", 253), JobName: testJobName},
		LogFileID: strings.Repeat("f", 1024), LogTail: esc(captureTailBytes), LogLines: 1 << 30, LogCapped: true,
		Notes: notes.list(),
	}
	if n := outcomeBytes(res); n > outcomeMaxBytes {
		t.Errorf("the largest outcome left after the cuts is %d bytes, over outcomeMaxBytes (%d)", n, outcomeMaxBytes)
	}
	observation, _ := json.Marshal(pl.StepResult{Status: res.Status, ExitCode: res.ExitCode, Failure: failure, StartedAt: res.StartedAt, FinishedAt: res.FinishedAt})
	others := map[string]string{
		AnnotObservation: string(observation),
		AnnotRunner:      rtStamp(strings.Repeat("n", 253), rtT0),
		AnnotLogCursor:   rtT0.Format(time.RFC3339Nano),
		AnnotLogFirst:    rtT0.Format(time.RFC3339Nano),
		AnnotStepKey:     strings.Repeat("s", 1024),
		AnnotWorkRun:     strings.Repeat("w", 256),
		AnnotOwner:       strings.Repeat("o", 256),
	}
	total := len(AnnotOutcome) + outcomeMaxBytes
	for k, v := range others {
		total += len(k) + len(v)
	}
	if total > 256<<10 {
		t.Errorf("an outcome of outcomeMaxBytes beside the largest other annotations is %d bytes, past the API server's 262144", total)
	}
}

// TestNoteListAddFirstKeepsItsBounds: a note put at the head of a full list
// pushes the last one into the count of notes left out.
func TestNoteListAddFirstKeepsItsBounds(t *testing.T) {
	n := &noteList{mask: func(s string) string { return s }}
	for i := 0; i < maxNotes; i++ {
		n.add(pl.CodeLogCapped, fmt.Sprintf("note %d", i))
	}
	n.addFirst(pl.CodeArtifactMissing, "the head")
	got := n.list()
	if len(got) != maxNotes+1 || got[0].Message != "the head" || got[maxNotes-1].Message != fmt.Sprintf("note %d", maxNotes-2) ||
		got[maxNotes].Message != "1 more note was left out" || got[maxNotes].Code != pl.CodeLogCapped {
		t.Errorf("notes = %+v, want the head, the first %d, and one note counting the last", got, maxNotes-1)
	}
}

// TestRunnerMasksStepTextInItsOwnLog (fix round 1 minor 9; fix round 2): an
// artifact's name is the step's, and the Library's answer can quote it --
// its error, or its reason for omitting the file. Neither reaches the node's
// log line or the note beside the step (and from there the run's rows and
// its check run) unmasked: every note is masked where it is added.
func TestRunnerMasksStepTextInItsOwnLog(t *testing.T) {
	path := "dist/" + plantedNPM + ".txt"
	name := artifactFileName(path)
	for _, c := range []struct {
		name   string
		answer func(l *rtLibrary)
		logged bool
	}{
		{"a Library error quoting the name", func(l *rtLibrary) {
			l.fail = map[string]error{name: fmt.Errorf("the Library refused %q: the owner's quota is spent", name)}
		}, true},
		{"a Library omitting the file, quoting the name", func(l *rtLibrary) {
			l.omit = map[string]string{name: fmt.Sprintf("%q is over the Library's size limit", name)}
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newRunnerHarness(t)
			logs := h.logs()
			run := rtRun()
			run.Artifacts = []string{"dist/*"}
			tgz := extractTestTgz(t, []extractTestEntry{{name: path, body: "x"}})
			rtArtifactStream(t, h, tgz)
			h.c.script(testJobName, rtFinishingScript(testJobName, 0, captureKubeLine(rtAt(1100), "ok")))
			c.answer(h.lib)

			res := h.run(t, run)

			if res.Status != pl.OutcomeFailed || res.Failure == nil || res.Failure.Code != pl.CodeArtifactUnavailable || len(res.Notes) != 1 {
				t.Fatalf("result = %+v (failure %+v), want required-artifact failure with a masked note", res, res.Failure)
			}
			if note := res.Notes[0].Message; strings.Contains(note, plantedNPM) || !strings.Contains(note, "***") {
				t.Errorf("note %q, want the Library's answer quoted with the secret masked", note)
			}
			text := logs.String()
			if c.logged && logs.count("pipelines: a step's file could not be stored in the Library") != 1 {
				t.Fatalf("the node logged nothing of the file it could not store:\n%s", text)
			}
			if strings.Contains(text, plantedNPM) {
				t.Errorf("the node's log carries the step's secret:\n%s", text)
			}
		})
	}
}

// Required exports are independent of the log and retain owner/run bindings.
func TestRunnerStoresArtifactsUnderTheOwner(t *testing.T) {
	h := newRunnerHarness(t)
	run := rtRun()
	run.Artifacts = []string{"dist/report.json", "coverage.out"}
	rtArtifactStream(t, h, extractTestTgz(t, []extractTestEntry{{name: "dist/report.json", body: `{"passed":3}`}, {name: "coverage.out", body: "mode: set\n"}}))
	h.c.script(testJobName, rtFinishingScript(testJobName, 0, captureKubeLine(rtAt(1100), "tests passed")))
	res := h.run(t, run)
	if res.Status != pl.OutcomeSucceeded || !reflect.DeepEqual(res.ArtifactFileIDs, []string{"file-2", "file-3"}) {
		t.Fatalf("result=%+v failure=%+v", res, res.Failure)
	}
	files := h.lib.stored()
	if len(files) != 3 {
		t.Fatal("wrong file count", len(files))
	}
	for n, want := range []RunFile{{Name: "tests-go-tests-2.log", MimeType: "text/plain; charset=utf-8"}, {Name: "coverage.out", MimeType: "application/octet-stream", Bytes: []byte("mode: set\n")}, {Name: "dist__report.json", MimeType: "application/json", Bytes: []byte(`{"passed":3}`)}} {
		got := files[n]
		if got.Name != want.Name || got.MimeType != want.MimeType || (want.Bytes != nil && !bytes.Equal(got.Bytes, want.Bytes)) {
			t.Fatalf("file %d=%+v", n, got)
		}
		if got.OwnerUserID != run.OwnerUserID || got.WorkRunID != run.WorkRunID || got.StepKey != run.StepKey {
			t.Fatal("lost ownership binding")
		}
	}
	if !reflect.DeepEqual(h.sink.messages(), []string{"tests passed"}) {
		t.Fatal("artifact bytes polluted the log")
	}
	h.leftNoArchive(t)
}

func TestRunnerFailsWhenRequiredExportCannotBeCaptured(t *testing.T) {
	for _, fault := range []string{"missing", "traversal", "oversize", "transport"} {
		t.Run(fault, func(t *testing.T) {
			h := newRunnerHarness(t, func(cfg *Config) { cfg.ArtifactMaxBytes = 4096 })
			run := rtRun()
			run.Artifacts = []string{"dist"}
			entries := []extractTestEntry{{name: "dist/proof", body: "ok"}}
			switch fault {
			case "missing":
				entries = nil
			case "traversal":
				entries = append(entries, extractTestEntry{name: "../escape", body: "bad"})
			case "oversize":
				entries[0].body = strings.Repeat("x", 8192)
			}
			rtArtifactStream(t, h, extractTestTgz(t, entries))
			if fault == "transport" {
				h.r.collectArtifacts = func(context.Context, StepRun, *Pod, int64) (*ArtifactSnapshot, error) {
					return nil, errors.New("uncertain transport")
				}
			}
			h.c.script(testJobName, rtFinishingScript(testJobName, 0, captureKubeLine(rtAt(1100), "command succeeded")))
			res := h.run(t, run)
			if res.Status != pl.OutcomeFailed || res.ExitCode != 0 || res.Failure == nil || res.Failure.Code != pl.CodeArtifactUnavailable || len(res.ArtifactFileIDs) != 0 {
				t.Fatalf("false artifact success: %+v", res)
			}
			if h.c.jobNow(t, testJobName).Metadata.Annotations[AnnotOutcome] == "" {
				t.Fatal("artifact refusal did not persist its outcome")
			}
			h.leftNoArchive(t)
		})
	}
}

// rtIncompressible is n bytes gzip cannot shrink, from a fixed sequence.
func rtIncompressible(n int) string {
	b := make([]byte, n)
	x := uint32(2463534242)
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = byte(x)
	}
	return string(b)
}

// TestRunnerOmittedLibraryFileIsANote: the Library may decline a file -- the
// owner's quota, no storage on the cluster -- or fail to store it. Either is
// a note beside the step, never its outcome.
func TestRunnerOmittedLibraryFileIsANote(t *testing.T) {
	for _, c := range []struct {
		name   string
		set    func(l *rtLibrary)
		reason string
	}{
		{"the Library omits the log", func(l *rtLibrary) {
			l.omit = map[string]string{"tests-go-tests-2.log": "the owner's Library is over its 100 GiB quota"}
		}, "over its 100 GiB quota"},
		{"the Library fails to store the log", func(l *rtLibrary) {
			l.fail = map[string]error{"tests-go-tests-2.log": errors.New("blob storage timed out")}
		}, "blob storage timed out"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newRunnerHarness(t)
			c.set(h.lib)
			h.c.script(testJobName, rtFinishingScript(testJobName, 0, captureKubeLine(rtAt(1100), "ok")))

			res := h.run(t, rtRun())

			if res.Status != pl.OutcomeSucceeded || res.Failure != nil || res.LogFileID != "" {
				t.Fatalf("result = %+v, want success with no log file", res)
			}
			if len(res.Notes) != 1 || res.Notes[0].Code != pl.CodeArtifactMissing || !strings.Contains(res.Notes[0].Message, c.reason) || !strings.Contains(res.Notes[0].Message, "log") {
				t.Errorf("notes = %+v, want one %s note saying the log was not stored and why", res.Notes, pl.CodeArtifactMissing)
			}
			h.leftNoArchive(t)
		})
	}
}

// TestRunnerPersistsItsOutcomeWhateverTheLibraryCosts: the outcome is
// recorded on the Job before Run answers (step 9), so a reply lost with a
// replica can be answered again. A Library that does not answer must cost the
// step its files -- notes say so -- and never that record: the Library phase
// has a deadline of its own, and persisting runs under its own.
func TestRunnerPersistsItsOutcomeWhateverTheLibraryCosts(t *testing.T) {
	h := newRunnerHarness(t)
	h.r.libraryTimeout = 50 * time.Millisecond
	h.lib.block = true
	h.c.script(testJobName, rtFinishingScript(testJobName, 0, captureKubeLine(rtAt(1100), "ok")))

	res := h.run(t, rtRun())

	if res.Status != pl.OutcomeSucceeded || res.LogFileID != "" || len(res.Notes) != 1 || !strings.Contains(res.Notes[0].Message, "deadline") {
		t.Fatalf("result = %+v, want success with no log file and a note saying the Library ran out of time", res)
	}
	var persisted pl.StepResult
	raw := h.c.jobNow(t, testJobName).Metadata.Annotations[AnnotOutcome]
	if err := json.Unmarshal([]byte(raw), &persisted); err != nil || !reflect.DeepEqual(persisted, res) {
		t.Errorf("outcome on the Job = %q (%v), want the result Run answered: a slow Library cost the step its record", raw, err)
	}
}

// ---------------------------------------------------------------------------
// Status and Ack
// ---------------------------------------------------------------------------

// TestAckDeletesJobAndSecret: the agent has the outcome, so the Job and its
// Secret go now rather than at the TTL; what is already gone is acked.
func TestAckDeletesJobAndSecret(t *testing.T) {
	h := newRunnerHarness(t)
	h.c.putJob(h.existingJob(t, rtRun(), nil), rtRunningScript(testJobName))
	h.c.putSecret(BuildSecret(h.cfg, rtRun(), testJobName, rtCloneToken))

	if err := h.r.Ack(context.Background(), AckRequest{JobName: testJobName}); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if h.c.hasJob(testJobName) || h.c.hasSecret(testSecretName) {
		t.Error("the acked step's Job or Secret is still there")
	}
	if err := h.r.Ack(context.Background(), AckRequest{JobName: testJobName}); err != nil {
		t.Errorf("a second Ack = %v, want nil: gone is acked", err)
	}

	t.Run("a name that is not a step Job's deletes nothing", func(t *testing.T) {
		h := newRunnerHarness(t)
		// An empty name would be the collection: every Job in the namespace.
		for _, name := range []string{"", " ", "mp-", "mp-a7a72726d5075767e0b6d115/x", "memql-pipelines-isolation-listener"} {
			if err := h.r.Ack(context.Background(), AckRequest{JobName: name}); err == nil {
				t.Errorf("Ack(%q) accepted", name)
			}
		}
		if n := len(h.c.requests()); n != 0 {
			t.Errorf("%d requests, want none", n)
		}
	})
}

// TestStatusStates: the agent asks a replica where a step's Job stands -- and,
// whenever the Job exists, when it was created, which is where the step's own
// timeout runs from (ruling R31b).
func TestStatusStates(t *testing.T) {
	outcome := pl.StepResult{Status: pl.OutcomeFailed, ExitCode: 1, Where: pl.Where{Surface: "cluster", NodeID: rtOther, JobName: testJobName}}
	// putJob's Jobs were created a minute before the runner's clock.
	created := rtT0.Add(-time.Minute).Format(time.RFC3339)
	for _, c := range []struct {
		name   string
		annots map[string]string // nil: no Job at all
		want   StatusReply
	}{
		{"no Job is absent", nil, StatusReply{State: StateAbsent}},
		{"a persisted outcome is finished, with it", map[string]string{AnnotRunner: rtStamp(rtOther, rtT0), AnnotOutcome: mustJSON(t, outcome)}, StatusReply{State: StateFinished, Result: &outcome, JobCreatedAt: created}},
		{"a fresh claim is running, on its node", map[string]string{AnnotRunner: rtStamp(rtOther, rtT0.Add(-10*time.Second))}, StatusReply{State: StateRunning, Runner: rtOther, JobCreatedAt: created}},
		{"a claim older than HeartbeatStale is stale, naming the node that went quiet", map[string]string{AnnotRunner: rtStamp(rtOther, rtT0.Add(-46*time.Second))}, StatusReply{State: StateStale, Runner: rtOther, JobCreatedAt: created}},
		{"an unclaimed Job is stale", map[string]string{}, StatusReply{State: StateStale, JobCreatedAt: created}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newRunnerHarness(t)
			if c.annots != nil {
				h.c.putJob(h.existingJob(t, rtRun(), c.annots), rtRunningScript(testJobName))
			}
			got := h.r.Status(context.Background(), StatusRequest{JobName: testJobName})
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("Status = %+v (result %+v), want %+v (result %+v)", got, got.Result, c.want, c.want.Result)
			}
		})
	}

	t.Run("a step this replica is queueing is running, though it has no Job yet", func(t *testing.T) {
		h := newRunnerHarness(t)
		h.c.with(func(c *rtCluster) { c.quotaRefusals = 1 << 30 })
		h.c.script(testJobName, rtRunningScript(testJobName))
		ctx, cancel := context.WithCancel(context.Background())
		done := h.start(ctx, rtRun())
		rtWaitUntil(t, "a refused create", func() bool { return len(h.c.requestsFor(http.MethodPost, kubeJobs)) >= 1 })

		got := h.r.Status(context.Background(), StatusRequest{JobName: testJobName})
		cancel()
		h.await(t, done)

		if got.State != StateRunning {
			t.Errorf("Status = %+v, want running: this replica holds the step, waiting for a slot", got)
		}
		if got.JobCreatedAt != "" {
			t.Errorf("Status = %+v, want no creation time: a step waiting for a slot has no Job, and the agent "+
				"waits on it until its run's ceiling rather than its own timeout", got)
		}
		if after := h.r.Status(context.Background(), StatusRequest{JobName: testJobName}); after.State != StateAbsent {
			t.Errorf("Status after the step ended = %+v, want absent", after)
		}
	})

	t.Run("a name that is not a step Job's is absent, and asks nothing", func(t *testing.T) {
		h := newRunnerHarness(t)
		if got := h.r.Status(context.Background(), StatusRequest{JobName: ""}); got.State != StateAbsent {
			t.Errorf("Status(\"\") = %+v, want absent", got)
		}
		if n := len(h.c.requests()); n != 0 {
			t.Errorf("%d requests, want none: an empty name reads the whole collection", n)
		}
	})
}

// TestStatusLogsAPersistentAPIErrorOnce (fix round 2 and 3, minor 5): the
// agent asks after every step it waits on every statusPollInterval, so an API
// error Status keeps meeting is logged when it appears, not on every question
// -- however many steps are asked after in turn: what is compared is what the
// error says about the API server, never the request's path, which names
// each step's Job.
func TestStatusLogsAPersistentAPIErrorOnce(t *testing.T) {
	h := newRunnerHarness(t)
	logs := h.logs()
	h.c.with(func(c *rtCluster) {
		for i := 0; i < 20; i++ {
			c.jobGetFailures = append(c.jobGetFailures, rtUnavailable)
		}
	})
	jobs := []string{JobName("run-1", "tests.a", 1), JobName("run-1", "tests.b", 1)}
	for i := 0; i < 20; i++ {
		if got := h.r.Status(context.Background(), StatusRequest{JobName: jobs[i%2]}); got.State != StateStale {
			t.Fatalf("Status = %+v, want stale: nothing here vouches for the step", got)
		}
	}
	if n := logs.count("pipelines: reading a step's Job for its status"); n != 1 {
		t.Errorf("%d warnings of twenty failed reads of two steps' Jobs, want one\n%s", n, logs)
	}
}

// TestTroubleKeyNamesNoObject (fix round 3): what an API error says about the
// API server, without the request -- the same for every Job it was about.
func TestTroubleKeyNamesNoObject(t *testing.T) {
	refusal := func(job string) error {
		return &deploycontrol.StatusError{Method: "GET", Path: kubeJobs + "/" + job, Code: 503,
			Status: "503 Service Unavailable", Body: rtUnavailable.body}
	}
	dial := func(job string) error {
		return fmt.Errorf("GET %s: %w", kubeJobs+"/"+job, &url.Error{Op: "Get", URL: "https://10.0.0.1:443" + kubeJobs + "/" + job,
			Err: errors.New("dial tcp 10.0.0.1:443: connect: connection refused")})
	}
	notAJob := func(job string) error {
		var v map[string]any
		err := json.Unmarshal([]byte("<html>proxy</html>"), &v)
		return fmt.Errorf("pipelinesteps: reading job %s: %w", job, err)
	}
	for name, mk := range map[string]func(string) error{"a refusal": refusal, "a transport error": dial, "a body that is not a Job": notAJob} {
		a, b := troubleKey(mk("mp-aaaaaaaaaaaaaaaaaaaaaaaa")), troubleKey(mk("mp-bbbbbbbbbbbbbbbbbbbbbbbb"))
		if a != b || strings.Contains(a, "mp-") || a == "" {
			t.Errorf("%s: keys %q and %q, want one key naming no Job", name, a, b)
		}
	}
	if troubleKey(refusal("x")) == troubleKey(&deploycontrol.StatusError{Code: 500, Body: `{"kind":"Status","reason":"InternalError"}`}) {
		t.Error("a 503 and a 500 share a key: a change in what the API server says would not be logged")
	}
}

// TestJobNamesAreWhatStatusAndAckAccept: Status and Ack refuse a name that is
// not a step Job's, so JobName's names must all be accepted.
func TestJobNamesAreWhatStatusAndAckAccept(t *testing.T) {
	for _, name := range []string{
		JobName("run-7f3a", "tests/go-tests#2", 2),
		JobName("v1:pipelines:run:"+strings.Repeat("r", 300), "stage.step#40", 1<<20),
		JobName("", "", 0),
	} {
		if !isStepJobName(name) {
			t.Errorf("JobName produced %q, which Status and Ack refuse", name)
		}
	}
}
