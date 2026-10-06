package pipelinesteps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

// isolation_test.go -- the runner proves memql-pipelines is isolated before it
// creates a step (Task 6b, rulings R12, R42-R44), against runner_test.go's
// fake API server. The fake plays the probe's Indexed Job as a script of its
// three pods: they start; their listeners come up while each connector waits
// for the probe Secret; once the Secret exists the connector runs, and ends
// with the exit code a test chose; and, when a test says so, the pods as the
// runner's re-read finds them after that.

const (
	isoListenerIP = "10.42.1.3"
	isoOtherIP    = "10.42.0.9"
)

// isoSaid is the connector's line for a verdict, as the cluster printed it
// (measured on k3s v1.35).
var isoSaid = map[int32]string{
	probeExitIsolated:  "memql: isolation probe: dns 10.43.0.10:53 connected, connected, connected; listener 10.42.1.3:8080 refused, refused, refused",
	probeExitConnected: "memql: isolation probe: dns 10.43.0.10:53 connected, connected, connected; listener 10.42.1.3:8080 connected, connected, connected",
	probeExitDNS:       "memql: isolation probe: dns 10.43.0.10:53 refused, refused, refused; listener 10.42.1.3:8080 refused, refused, refused",
}

func isoName() string   { return IsolationProbeName(rtNode) }
func isoTarget() string { return IsolationTargetName(isoName()) }

// isoListener is a probe pod's listener sidecar, running since its start for
// the restarts-th time, ready or not.
func isoListener(ready bool, restarts int32) ContainerStatus {
	return ContainerStatus{Name: ContainerProbeListener, Image: testConfig().CloneImage, Ready: ready, RestartCount: restarts,
		State: ContainerState{Running: &ContainerStateRunning{StartedAt: rtAt(500 + 1000*int(restarts))}}}
}

// isoStarting is a listener sidecar whose image is still being pulled.
var isoStarting = ContainerStatus{Name: ContainerProbeListener, State: ContainerState{Waiting: &ContainerStateWaiting{Reason: "ContainerCreating"}}}

// isoWaiting is a connector waiting for the probe Secret.
func isoWaiting() ContainerStatus {
	return ContainerStatus{Name: ContainerProbeConnector, State: ContainerState{Waiting: &ContainerStateWaiting{
		Reason: "CreateContainerConfigError", Message: fmt.Sprintf("secret %q not found", isoTarget())}}}
}

// isoHolding is index 0's main container holding its pod up; isoRunning is
// index 1's connector at work.
var (
	isoHolding = ContainerStatus{Name: ContainerProbeConnector, State: ContainerState{Running: &ContainerStateRunning{StartedAt: rtAt(2000)}}}
	isoRunning = ContainerStatus{Name: ContainerProbeConnector, State: ContainerState{Running: &ContainerStateRunning{StartedAt: rtAt(2000)}}}
)

func isoEnded(exit int32, said string) ContainerStatus {
	return ContainerStatus{Name: ContainerProbeConnector, State: ContainerState{Terminated: &ContainerStateTerminated{
		ExitCode: exit, Reason: "Error", Message: said + "\n", StartedAt: rtAt(2000), FinishedAt: rtAt(12000)}}}
}

// isoPod is one of the probe's pods, as the Job controller labels it, with the
// Ready condition its containers make: True once its listener is ready and its
// main container runs (measured: kube_test.go's probe pods).
func isoPod(index int, ip string, listener, main ContainerStatus) *Pod {
	i := strconv.Itoa(index)
	ready := "False"
	if listener.Ready && main.State.Running != nil {
		ready = "True"
	}
	return &Pod{
		Metadata: ObjectMeta{
			Name:              fmt.Sprintf("%s-%d-x7kk6", isoName(), index),
			UID:               "uid-pod-" + i,
			Labels:            map[string]string{"job-name": isoName(), "batch.kubernetes.io/job-completion-index": i},
			Annotations:       map[string]string{"batch.kubernetes.io/job-completion-index": i},
			CreationTimestamp: rtT0,
		},
		Status: PodStatus{
			Phase: "Pending", PodIP: ip,
			Conditions:            []PodCondition{{Type: "Ready", Status: ready}},
			InitContainerStatuses: []ContainerStatus{listener}, ContainerStatuses: []ContainerStatus{main},
		},
	}
}

// isoScript is the probe's life: its pods start (state 0); their listeners
// come up while the connectors wait for the probe Secret (1, held until the
// Secret exists); the connector runs (2) and ends with exit, saying said (3).
func isoScript(exit int32, said string) *rtScript {
	up := isoListener(true, 0)
	return &rtScript{states: []rtState{
		{pods: []*Pod{isoPod(0, "", isoStarting, isoWaiting()), isoPod(1, "", isoStarting, isoWaiting()), isoPod(2, "", isoStarting, isoWaiting())}},
		{
			pods:  []*Pod{isoPod(0, isoListenerIP, up, isoWaiting()), isoPod(1, isoOtherIP, up, isoWaiting()), isoPod(2, "10.42.1.4", up, isoWaiting())},
			until: func(c *rtCluster) bool { _, ok := c.secrets[isoTarget()]; return ok },
		},
		{pods: []*Pod{isoPod(0, isoListenerIP, up, isoHolding), isoPod(1, isoOtherIP, up, isoRunning), isoPod(2, "10.42.1.4", up, isoRunning)}},
		{pods: []*Pod{isoPod(0, isoListenerIP, up, isoHolding), isoPod(1, isoOtherIP, up, isoEnded(exit, said)), isoPod(2, "10.42.1.4", up, isoEnded(probeExitControlPassed, "positive control connected every round"))}},
	}, tails: map[string]string{ContainerProbeListener: ""}} // the listener prints nothing
}

// then adds the moment after the last: the pods as a later read finds them.
func (s *rtScript) then(pods ...*Pod) *rtScript {
	s.states = append(s.states, rtState{pods: pods})
	return s
}

// isoScriptAfter is isoScript with first in place of its first moment: the
// pods as they stand, read after read, before both are up. The probe Secret
// may be made only once they are, so in state len(first) or later.
func isoScriptAfter(first []rtState, exit int32, said string) *rtScript {
	s := isoScript(exit, said)
	s.states = append(append([]rtState{}, first...), s.states[1:]...)
	return s
}

// isoRereadFails is the probe's life, with every list of its pods after the
// one that saw the connector end answered by answer: the re-read fails.
func isoRereadFails(answer kubeAnswer) *rtScript {
	s := isoScript(probeExitIsolated, isoSaid[probeExitIsolated]).then(isoMarked(func(*Pod) {}),
		isoPod(1, isoOtherIP, isoListener(false, 0), isoEnded(probeExitIsolated, isoSaid[probeExitIsolated])))
	s.states[3].until = func(c *rtCluster) bool { c.podsAnswer = &answer; return true }
	return s
}

// isoScriptPlanting is the probe's life, with a probe Secret naming this
// probe Job and the address target(job) planted before the proof makes its
// own -- once the Job exists, so the Secret can name it.
func isoScriptPlanting(target func(job Job) string) *rtScript {
	s := isoScript(probeExitIsolated, isoSaid[probeExitIsolated])
	s.states[0].until = func(c *rtCluster) bool {
		j := c.jobs[isoName()]
		if j == nil {
			return false
		}
		if planted, err := BuildIsolationTarget(testConfig(), j.job, target(j.job)); err == nil {
			planted.Metadata.Namespace = j.job.Metadata.Namespace
			c.secrets[isoTarget()] = planted
		}
		return true
	}
	return s
}

// isoMarked is index 0's pod with its listener as the kubelet last reported
// it -- running, ready, the incarnation first seen ready -- and mark applied:
// what the API server says of the pod that the kubelet has not yet acted on.
func isoMarked(mark func(p *Pod)) *Pod {
	p := isoPod(0, isoListenerIP, isoListener(true, 0), isoHolding)
	mark(p)
	return p
}

// isoPutLeftoverLocked puts an earlier probe Job of this replica into the
// fake, with c.mu held -- one a delete did not clear -- and answers its uid.
func isoPutLeftoverLocked(c *rtCluster, cfg Config) string {
	job := BuildIsolationProbe(cfg, isoName())
	c.uid++
	c.rv++
	job.Metadata.UID = fmt.Sprintf("uid-%d", c.uid)
	job.Metadata.ResourceVersion = strconv.Itoa(c.rv)
	c.jobs[job.Metadata.Name] = &rtJob{job: job, script: &rtScript{states: []rtState{{}}}, createdRV: job.Metadata.ResourceVersion}
	return job.Metadata.UID
}

// isoSecretMadeIn records the probe's state, and the Secret, at the moment
// the probe Secret is created.
func isoSecretMadeIn(h *rtHarness) (madeIn *int, made *Secret, uid *string) {
	madeIn, made, uid = new(int), new(Secret), new(string)
	*madeIn = -1
	h.c.with(func(c *rtCluster) {
		c.onSecretCreate = func(c *rtCluster, s Secret) {
			if s.Metadata.Name != isoTarget() {
				return
			}
			*made = s
			if j := c.jobs[isoName()]; j != nil {
				*madeIn, *uid = j.state, j.job.Metadata.UID
			}
		}
	})
	return madeIn, made, uid
}

// newIsoHarness is a replica that has proved nothing yet, with the probe's
// script in the fake.
func newIsoHarness(t *testing.T, probe *rtScript, mutate ...func(*Config)) *rtHarness {
	t.Helper()
	h := newRunnerHarness(t, mutate...)
	h.r.isoLast = IsolationVerdict{}
	if probe != nil {
		h.c.script(isoName(), probe)
	}
	return h
}

// isoStep is a step that succeeds, under its own key.
func isoStep(h *rtHarness, key string) StepRun {
	run := rtRun()
	run.StepKey = key
	name := JobName(run.RunID, run.StepKey, run.Attempt)
	h.c.script(name, rtFinishingScript(name, 0, captureKubeLine(rtAt(1100), "ok")))
	return run
}

// createsOf counts the creates of the Job, or the Secret, of that name the
// fake received, admitted or not.
func createsOf(c *rtCluster, collection, name string) int {
	n := 0
	for _, r := range c.requestsFor(http.MethodPost, collection) {
		var obj struct {
			Metadata ObjectMeta `json:"metadata"`
		}
		if json.Unmarshal([]byte(r.Body), &obj) == nil && obj.Metadata.Name == name {
			n++
		}
	}
	return n
}

// reqIndex is where the first request matching method and path is among all
// the fake received (the body's name for a create), or -1.
func reqIndex(c *rtCluster, method, path, name string) int {
	for i, r := range c.requests() {
		if r.Method != method || r.Path != path {
			continue
		}
		var obj struct {
			Metadata ObjectMeta `json:"metadata"`
		}
		if name == "" || (json.Unmarshal([]byte(r.Body), &obj) == nil && obj.Metadata.Name == name) {
			return i
		}
	}
	return -1
}

// lastReqIndex is where the last request of method to path is, or -1.
func lastReqIndex(c *rtCluster, method, path string) int {
	reqs := c.requests()
	for i := len(reqs) - 1; i >= 0; i-- {
		if reqs[i].Method == method && reqs[i].Path == path {
			return i
		}
	}
	return -1
}

// isoSettled waits for the proof in flight, if any, to finish: its goroutine
// outlives a Run that stopped waiting on it by the cleanup it does.
func isoSettled(t *testing.T, r *Runner) {
	t.Helper()
	rtWaitUntil(t, "the proof in flight to finish", func() bool {
		r.isoMu.Lock()
		defer r.isoMu.Unlock()
		return r.isoProof == nil
	})
}

// isoLeftNothing checks the probe's Job and Secret are gone once the proof is.
func isoLeftNothing(t *testing.T, h *rtHarness) {
	t.Helper()
	isoSettled(t, h.r)
	if h.c.hasJob(isoName()) || h.c.hasSecret(isoTarget()) {
		t.Errorf("the probe left its Job (%v) or its Secret (%v) behind", h.c.hasJob(isoName()), h.c.hasSecret(isoTarget()))
	}
}

// isoWantRefused checks a step was refused for the proof and that nothing of
// its own was made: no token, no Secret, no Job.
func isoWantRefused(t *testing.T, h *rtHarness, res pl.StepResult) {
	t.Helper()
	rtWantCode(t, res, pl.OutcomeRefused, pl.CodeIsolationUnenforced)
	if res.Where.Surface != "cluster" || res.Where.NodeID != rtNode || res.Where.JobName != testJobName {
		t.Errorf("where = %+v, want this node and the step's Job name", res.Where)
	}
	if n := createsOf(h.c, kubeSecrets, testSecretName); n != 0 {
		t.Errorf("the refused step's Secret was created %d times", n)
	}
	if n := createsOf(h.c, kubeJobs, testJobName); n != 0 {
		t.Errorf("the refused step's Job was created %d times", n)
	}
	if calls := h.tokens.called(); len(calls) != 0 {
		t.Errorf("a clone token was minted for a refused step: %q", calls)
	}
}

// ---------------------------------------------------------------------------
// The verdict (R42)
// ---------------------------------------------------------------------------

// TestIsolationProofPassesWhenTheListenerIsUnreachableAndDNSAnswers: the
// connector reached the cluster's DNS on all three attempts and the listener
// on none (refused or timed out alike, R42), and the listener was still ready
// when the connector had finished: memql-pipelines is isolated and the step
// is created. The probe Secret came only once the listener was ready -- so
// the connector, which waits for it, could not try before the listener
// listened -- and it names that listener and the probe's own Job, which owns
// it. Nothing of the step's own exists before the verdict, and nothing of the
// probe's after it.
func TestIsolationProofPassesWhenTheListenerIsUnreachableAndDNSAnswers(t *testing.T) {
	h := newIsoHarness(t, isoScript(probeExitIsolated, isoSaid[probeExitIsolated]))
	run := isoStep(h, rtRun().StepKey)
	var (
		made     Secret
		madeIn   = -1 // the probe's state when its Secret was created
		probeUID string
	)
	h.c.with(func(c *rtCluster) {
		c.onSecretCreate = func(c *rtCluster, s Secret) {
			if s.Metadata.Name != isoTarget() {
				return
			}
			made = s
			if j := c.jobs[isoName()]; j != nil {
				madeIn, probeUID = j.state, j.job.Metadata.UID
			}
		}
	})

	res := h.run(t, run)

	if res.Status != pl.OutcomeSucceeded || res.Failure != nil {
		t.Fatalf("result = %+v (failure %+v), want the step run once the proof passed", res, res.Failure)
	}
	if n := createsOf(h.c, kubeJobs, isoName()); n != 1 {
		t.Errorf("the probe Job was created %d times, want once", n)
	}
	if n := createsOf(h.c, kubeSecrets, isoTarget()); n != 1 {
		t.Fatalf("the probe Secret was created %d times, want once", n)
	}
	if madeIn < 1 {
		t.Errorf("the probe Secret was created while the probe was in state %d, before its listener was ready", madeIn)
	}
	if string(made.Data[probeTargetKey]) != isoListenerIP || probeUID == "" || string(made.Data[probeJobUIDKey]) != probeUID {
		t.Errorf("the probe Secret holds %q, want index 0's address %s and the probe Job's uid %q", made.Data, isoListenerIP, probeUID)
	}
	if owners := made.Metadata.OwnerReferences; len(owners) != 1 || owners[0].UID != probeUID || owners[0].Kind != "Job" || owners[0].Name != isoName() {
		t.Errorf("the probe Secret's owners = %+v, want the probe Job", owners)
	}

	v := h.r.Isolation()
	if !v.Isolated || v.Inconclusive || !v.At.Equal(rtT0) {
		t.Errorf("verdict = %+v, want isolated, at the runner's clock", v)
	}
	if !strings.Contains(v.Detail, "refused, refused, refused") || !strings.Contains(v.Detail, "connected, connected, connected") {
		t.Errorf("detail = %q, want what the connector saw", v.Detail)
	}

	probeGone := lastReqIndex(h.c, http.MethodDelete, kubeSecrets+"/"+isoTarget())
	stepFirst := reqIndex(h.c, http.MethodPost, kubeSecrets, testSecretName)
	if probeGone < 0 || stepFirst < 0 || stepFirst < probeGone {
		t.Errorf("the step's Secret came at request %d and the probe's cleanup at %d: the step's own objects come after the proof\n  %s",
			stepFirst, probeGone, h.c.summary())
	}
	isoLeftNothing(t, h)
}

func TestIsolationProofRequiresThePositiveControl(t *testing.T) {
	for _, exit := range []int32{probeExitControlFailed, probeExitForeignTarget, 0, 137} {
		t.Run(fmt.Sprint(exit), func(t *testing.T) {
			script := isoScript(probeExitIsolated, isoSaid[probeExitIsolated])
			script.states[3].pods[2] = isoPod(2, "10.42.1.4", isoListener(true, 0), isoEnded(exit, "control failed"))
			h := newIsoHarness(t, script)
			result := h.run(t, isoStep(h, rtRun().StepKey))
			if result.Status == pl.OutcomeSucceeded || h.r.Isolation().Isolated || !h.r.Isolation().Inconclusive {
				t.Fatalf("failed positive control admitted a step: %+v; %+v", result, h.r.Isolation())
			}
			if n := createsOf(h.c, kubeJobs, JobName(rtRun().RunID, rtRun().StepKey, rtRun().Attempt)); n != 0 {
				t.Fatalf("created %d step Jobs before proving the control path", n)
			}
		})
	}
}

// TestIsolationProofRefusesStepsWhenTheConnectorConnects: a probe pod reached
// another one's listener. The namespace's NetworkPolicy is not enforced, so a
// step could reach the mesh, the database and the node's metadata endpoint:
// the step is refused, named for the fix, and nothing of its own is made.
func TestIsolationProofRefusesStepsWhenTheConnectorConnects(t *testing.T) {
	h := newIsoHarness(t, isoScript(probeExitConnected, isoSaid[probeExitConnected]))

	res := h.run(t, rtRun())

	isoWantRefused(t, h, res)
	for _, w := range []string{"not network-isolated", "network policy engine", "az aks update", "--network-policy", "connected, connected, connected"} {
		if !strings.Contains(res.Failure.Message, w) {
			t.Errorf("the refusal %q does not say %q", res.Failure.Message, w)
		}
	}
	if v := h.r.Isolation(); v.Isolated || v.Inconclusive || !strings.Contains(v.Detail, "connected, connected, connected") {
		t.Errorf("verdict = %+v, want not isolated, saying what the connector saw", v)
	}
	isoLeftNothing(t, h)
}

// TestIsolationProofRefusedConnectionPassesOnlyWhileTheListenerHeld (R42): a
// policy engine that rejects (k3s's answers a denied SYN with an ICMP error
// the kernel reports as "Connection refused", measured) is
// indistinguishable, from the connector, from a pod that reset the
// connection. What tells them apart is the listener: one that stayed ready
// from before the first attempt to after the last had its socket open
// throughout, so a SYN that reached it would have been accepted. So a
// connector that never got through passes only if the runner's re-read, made
// after the connector ended, finds the same listener still ready.
func TestIsolationProofRefusedConnectionPassesOnlyWhileTheListenerHeld(t *testing.T) {
	ended := func(exit int32) *Pod {
		return isoPod(1, isoOtherIP, isoListener(false, 0), isoEnded(exit, isoSaid[probeExitIsolated]))
	}
	for _, c := range []struct {
		name         string
		script       *rtScript
		setup        func(h *rtHarness)
		isolated     bool
		inconclusive bool
		said         string
	}{
		{
			name:     "the same listener, still ready: isolated",
			script:   isoScript(probeExitIsolated, isoSaid[probeExitIsolated]).then(isoPod(0, isoListenerIP, isoListener(true, 0), isoHolding), ended(probeExitIsolated)),
			isolated: true,
		},
		{
			name:         "the listener no longer ready: inconclusive",
			script:       isoScript(probeExitIsolated, isoSaid[probeExitIsolated]).then(isoPod(0, isoListenerIP, isoListener(false, 0), isoHolding), ended(probeExitIsolated)),
			inconclusive: true, said: "not ready",
		},
		{
			name:         "the listener restarted since it was seen ready: inconclusive",
			script:       isoScript(probeExitIsolated, isoSaid[probeExitIsolated]).then(isoPod(0, isoListenerIP, isoListener(true, 1), isoHolding), ended(probeExitIsolated)),
			inconclusive: true, said: "restarted",
		},
		{
			name:         "the listener's pod is gone: inconclusive",
			script:       isoScript(probeExitIsolated, isoSaid[probeExitIsolated]).then(ended(probeExitIsolated)),
			inconclusive: true, said: "gone",
		},
		{
			name: "the listener had ended: inconclusive",
			script: isoScript(probeExitIsolated, isoSaid[probeExitIsolated]).then(isoPod(0, isoListenerIP, ContainerStatus{
				Name: ContainerProbeListener, State: ContainerState{Terminated: &ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}},
			}, isoHolding), ended(probeExitIsolated)),
			inconclusive: true, said: "no longer running",
		},
		{
			// The reviewer's reproduction (fix round 1, Critical 1): the API
			// server marks the pod for eviction before any kubelet acts, while
			// the kubelet's last report still has the listener running and
			// ready, the same incarnation.
			name: "the listener's pod is marked for eviction: inconclusive",
			script: isoScript(probeExitIsolated, isoSaid[probeExitIsolated]).then(isoMarked(func(p *Pod) {
				p.Status.Conditions = append(p.Status.Conditions, PodCondition{
					Type: "DisruptionTarget", Status: "True", Reason: "EvictionByEvictionAPI",
					Message: "Eviction API: evicting", LastTransitionTime: rtT0.Add(time.Second),
				})
			}), ended(probeExitIsolated)),
			inconclusive: true, said: "marked for disruption",
		},
		{
			name: "the listener's pod is being deleted: inconclusive",
			script: isoScript(probeExitIsolated, isoSaid[probeExitIsolated]).then(isoMarked(func(p *Pod) {
				p.Metadata.DeletionTimestamp = rtT0.Add(10 * time.Second)
			}), ended(probeExitIsolated)),
			inconclusive: true, said: "being deleted",
		},
		{
			name: "the listener's pod has ended: inconclusive",
			script: isoScript(probeExitIsolated, isoSaid[probeExitIsolated]).then(isoMarked(func(p *Pod) {
				p.Status.Phase = "Failed"
			}), ended(probeExitIsolated)),
			inconclusive: true, said: "had ended",
		},
		{
			// A replacement started within the second the first one did reads
			// the same startedAt: only the pod's own uid tells them apart.
			name: "the listener's pod was replaced: inconclusive",
			script: isoScript(probeExitIsolated, isoSaid[probeExitIsolated]).then(isoMarked(func(p *Pod) {
				p.Metadata.Name, p.Metadata.UID = isoName()+"-0-repl1", "uid-pod-0-replacement"
				p.Metadata.CreationTimestamp = rtT0.Add(time.Minute)
			}), ended(probeExitIsolated)),
			inconclusive: true, said: "replaced",
		},
		{
			// Only the uid tells a replacement of the same name apart (fix
			// round 2): a uid check reduced to the name would pass it.
			name: "a replacement of the same name, with a new uid: inconclusive",
			script: isoScript(probeExitIsolated, isoSaid[probeExitIsolated]).then(isoMarked(func(p *Pod) {
				p.Metadata.UID = "uid-pod-0-other"
			}), ended(probeExitIsolated)),
			inconclusive: true, said: "replaced by a new pod of the same name",
		},
		{
			// The node controller marks a lost node's pods not Ready -- the
			// pod's condition, which the container's ready never reflects.
			name: "the listener's pod is not Ready, its container still ready: inconclusive",
			script: isoScript(probeExitIsolated, isoSaid[probeExitIsolated]).then(isoMarked(func(p *Pod) {
				p.Status.Conditions = []PodCondition{{Type: "Ready", Status: "False", Reason: "NodeLost"}}
			}), ended(probeExitIsolated)),
			inconclusive: true, said: "Ready condition",
		},
		{
			// R42b: a node lost in the seconds after the listener was seen
			// ready can no longer update its pod, which reads as it was; only
			// a round trip through its kubelet tells.
			name:   "the listener's node is lost, its kubelet silent: inconclusive",
			script: isoScript(probeExitIsolated, isoSaid[probeExitIsolated]),
			setup: func(h *rtHarness) {
				h.r.probeKubeletWait = 200 * time.Millisecond
				h.c.with(func(c *rtCluster) { c.blockTails = true })
			},
			inconclusive: true, said: "did not answer within",
		},
		{
			name:   "the API server cannot reach the listener's kubelet: inconclusive",
			script: isoScript(probeExitIsolated, isoSaid[probeExitIsolated]),
			setup: func(h *rtHarness) {
				h.c.with(func(c *rtCluster) {
					c.tailAnswer = &kubeAnswer{code: 500, body: `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"Get \"https://10.89.0.3:10250/containerLogs/steps-ns/x/listener?tailLines=1\": dial tcp 10.89.0.3:10250: connect: no route to host","code":500}`}
				})
			},
			inconclusive: true, said: "could not be reached",
		},
		{
			name:   "a connection is never isolation, whatever the listener did after",
			script: isoScript(probeExitConnected, isoSaid[probeExitConnected]).then(isoPod(0, isoListenerIP, isoListener(false, 0), isoHolding), ended(probeExitConnected)),
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newIsoHarness(t, c.script)
			if c.setup != nil {
				c.setup(h)
			}
			run := isoStep(h, rtRun().StepKey)

			res := h.run(t, run)

			v := h.r.Isolation()
			if v.Isolated != c.isolated || v.Inconclusive != c.inconclusive {
				t.Fatalf("verdict = %+v, want isolated %v, inconclusive %v", v, c.isolated, c.inconclusive)
			}
			if c.isolated {
				if res.Status != pl.OutcomeSucceeded {
					t.Errorf("result = %+v, want the step run", res)
				}
			} else {
				isoWantRefused(t, h, res)
			}
			if c.said != "" && !strings.Contains(v.Detail, c.said) {
				t.Errorf("detail = %q, want it to say %q", v.Detail, c.said)
			}
			isoLeftNothing(t, h)
		})
	}
}

// TestIsolationProofWithoutDNSIsInconclusive: the connector could not reach
// the cluster's DNS, which the policy allows, so its failure to reach the
// listener proves nothing about the policy -- the network itself may be
// broken. Never a pass: the step is refused, saying the proof did not
// conclude and where to look if that persists -- not the remedy for a
// namespace found open -- and the next create proves again.
func TestIsolationProofWithoutDNSIsInconclusive(t *testing.T) {
	h := newIsoHarness(t, isoScript(probeExitDNS, isoSaid[probeExitDNS]))

	res := h.run(t, rtRun())

	isoWantRefused(t, h, res)
	v := h.r.Isolation()
	if v.Isolated || !v.Inconclusive || !strings.Contains(v.Detail, "DNS") {
		t.Errorf("verdict = %+v, want inconclusive, naming DNS", v)
	}
	msg := res.Failure.Message
	if !strings.Contains(msg, "could not prove") ||
		!strings.Contains(msg, "if this persists, check that the cluster's network policy engine is running and that cluster DNS (kube-system) answers") ||
		strings.Contains(msg, "az aks update") || strings.Contains(msg, "Enable a network policy engine") {
		t.Errorf("the refusal %q, want it to say the proof did not conclude and where to look if that persists, not the fix for an open namespace", msg)
	}
	isoLeftNothing(t, h)

	h.run(t, rtRun())
	if n := createsOf(h.c, kubeJobs, isoName()); n != 2 {
		t.Errorf("the probe ran %d times over two creates, want twice: an inconclusive proof is not kept", n)
	}
}

// TestIsolationProofInconclusiveIsNotAPass: every way the proof can fail to
// decide refuses the step and is not kept, so the next create tries again.
func TestIsolationProofInconclusiveIsNotAPass(t *testing.T) {
	neverUp := &rtScript{states: []rtState{{pods: []*Pod{isoPod(0, "", isoStarting, isoWaiting()), isoPod(1, "", isoStarting, isoWaiting())}}}}
	neverEnds := isoScript(probeExitIsolated, "")
	neverEnds.states = neverEnds.states[:3]
	for _, c := range []struct {
		name   string
		script *rtScript
		setup  func(h *rtHarness)
		said   string
	}{
		{name: "the probe's pods never came up", script: neverUp,
			setup: func(h *rtHarness) { h.r.probeUpWait = 300 * time.Millisecond }, said: "not up within"},
		{name: "the connector never finished", script: neverEnds,
			setup: func(h *rtHarness) { h.r.probeEndWait = 300 * time.Millisecond }, said: "did not finish within"},
		{name: "an attempt failed some other way", script: isoScript(probeExitUnclear, "memql: isolation probe: ... failed: Network is unreachable"), said: "Network is unreachable"},
		{name: "the pod had no nameserver", script: isoScript(probeExitNoNameserver, "memql: isolation probe: /etc/resolv.conf names no nameserver"), said: "nameserver"},
		{name: "the target was not this probe's", script: isoScript(probeExitForeignTarget, "memql: isolation probe: the target Secret is not this probe Job's"), said: "not this probe"},
		{name: "the pod had no part", script: isoScript(probeExitNoPart, "memql: isolation probe: no part for completion index 7"), said: "no part"},
		{name: "the connector ended without a verdict", script: isoScript(0, ""), said: "exit code 0"},
		{name: "the connector was killed", script: isoScript(137, ""), said: "exit code 137"},
		{name: "the probe Job could not be created", script: isoScript(probeExitIsolated, isoSaid[probeExitIsolated]),
			setup: func(h *rtHarness) {
				h.c.with(func(c *rtCluster) {
					c.createJobAnswers = []kubeAnswer{kubeStatus(403, "Forbidden", `jobs.batch is forbidden: User "system:serviceaccount:memql:memql-engine" cannot create resource "jobs"`)}
				})
			}, said: "cannot create resource"},
		{name: "another probe's Secret was there before the proof made its own", script: isoScript(probeExitIsolated, isoSaid[probeExitIsolated]),
			setup: func(h *rtHarness) {
				// Made after the proof cleared the way: when its Job is.
				foreign, err := BuildIsolationTarget(h.cfg, Job{Metadata: ObjectMeta{Name: isoName(), UID: "uid-another-probe"}}, "10.42.7.7")
				if err != nil {
					t.Fatal(err)
				}
				h.c.with(func(c *rtCluster) {
					c.onJobCreate = func(c *rtCluster, _ int) { c.secrets[isoTarget()] = foreign }
				})
			}, said: "already there"},
		{name: "a probe Secret answered there and then could not be read", script: isoScript(probeExitIsolated, isoSaid[probeExitIsolated]),
			setup: func(h *rtHarness) {
				h.c.with(func(c *rtCluster) {
					c.createSecretAnswers = []kubeAnswer{kubeStatus(409, "AlreadyExists", fmt.Sprintf("secrets %q already exists", isoTarget()))}
				})
			}, said: "could not be read"},
		{name: "the probe Secret could not be created", script: isoScript(probeExitIsolated, isoSaid[probeExitIsolated]),
			setup: func(h *rtHarness) {
				h.r.probeEndWait = 300 * time.Millisecond
				h.c.with(func(c *rtCluster) {
					c.createSecretAnswers = []kubeAnswer{kubeStatus(403, "Forbidden", `secrets is forbidden: User "system:serviceaccount:memql:memql-engine" cannot create resource "secrets"`)}
				})
			}, said: "Secret could not be created"},
		{name: "the API server's answer to the create could not be read", script: isoScript(probeExitIsolated, isoSaid[probeExitIsolated]),
			setup: func(h *rtHarness) {
				h.c.with(func(c *rtCluster) { c.createJobAnswers = []kubeAnswer{{code: 201, body: "not a Job"}} })
			}, said: "could not be read"},
		{name: "the probe's pods could not be read while they came up", script: isoScript(probeExitIsolated, isoSaid[probeExitIsolated]),
			setup: func(h *rtHarness) {
				h.r.probeUpWait = 300 * time.Millisecond
				h.c.with(func(c *rtCluster) { c.podsAnswer = &rtUnavailable })
			}, said: "pods could not be read"},
		{name: "the connector's pod could not be read while it ran", script: isoScript(probeExitIsolated, isoSaid[probeExitIsolated]),
			setup: func(h *rtHarness) {
				h.r.probeEndWait = 300 * time.Millisecond
				h.c.with(func(c *rtCluster) {
					c.onSecretCreate = func(c *rtCluster, s Secret) {
						if s.Metadata.Name == isoTarget() {
							c.podsAnswer = &rtUnavailable
						}
					}
				})
			}, said: "pod could not be read"},
		{name: "the pods could not be read again: forbidden", script: isoRereadFails(kubeStatus(403, "Forbidden", `pods is forbidden: User "system:serviceaccount:memql:memql-engine" cannot list resource "pods"`)),
			setup: func(h *rtHarness) { h.r.probeEndWait = 2 * time.Second }, said: "read again"},
		{name: "the pods could not be read again: unavailable on every try", script: isoRereadFails(rtUnavailable),
			setup: func(h *rtHarness) { h.r.probeEndWait = 2 * time.Second }, said: "read again"},
		{name: "an earlier probe's Secret naming this very listener", script: isoScript(probeExitIsolated, isoSaid[probeExitIsolated]),
			setup: func(h *rtHarness) {
				// Pod addresses are reused: only the Job's uid tells it apart.
				stale, err := BuildIsolationTarget(h.cfg, Job{Metadata: ObjectMeta{Name: isoName(), UID: "uid-earlier-probe"}}, isoListenerIP)
				if err != nil {
					t.Fatal(err)
				}
				h.c.with(func(c *rtCluster) {
					c.onJobCreate = func(c *rtCluster, _ int) { c.secrets[isoTarget()] = stale }
				})
			}, said: "naming another probe Job or listener"},
		{name: "a Secret naming this probe Job but another listener", script: isoScriptPlanting(func(job Job) string { return "10.42.9.9" }),
			said: "naming another probe Job or listener"},
		{name: "an earlier probe Job came back after every delete", script: isoScript(probeExitIsolated, isoSaid[probeExitIsolated]),
			setup: func(h *rtHarness) {
				h.c.with(func(c *rtCluster) {
					c.onJobCreate = func(c *rtCluster, _ int) { isoPutLeftoverLocked(c, h.cfg) }
				})
			}, said: "still there after 10 deletes"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newIsoHarness(t, c.script)
			if c.setup != nil {
				c.setup(h)
			}

			res := h.run(t, rtRun())

			isoWantRefused(t, h, res)
			v := h.r.Isolation()
			if v.Isolated || !v.Inconclusive {
				t.Errorf("verdict = %+v, want inconclusive", v)
			}
			if !strings.Contains(v.Detail, c.said) || !strings.Contains(res.Failure.Message, c.said) {
				t.Errorf("detail %q, refusal %q; want both to say %q", v.Detail, res.Failure.Message, c.said)
			}
			isoLeftNothing(t, h)
		})
	}
}

// TestIsolationOutcomeReadsTheTable: the exit codes jobspec.go documents, read
// with the listener's re-read, exactly as R42 rules -- held is the listener
// still ready, the same incarnation, when the connector had ended.
func TestIsolationOutcomeReadsTheTable(t *testing.T) {
	for _, c := range []struct {
		exit         int32
		held         bool
		isolated     bool
		inconclusive bool
	}{
		{probeExitIsolated, true, true, false},
		{probeExitIsolated, false, false, true},
		{probeExitConnected, true, false, false},
		{probeExitConnected, false, false, false},
		{probeExitDNS, true, false, true},
		{probeExitUnclear, true, false, true},
		{probeExitNoNameserver, true, false, true},
		{probeExitForeignTarget, true, false, true},
		{probeExitNoPart, true, false, true},
		{0, true, false, true},
		{1, true, false, true},
		{124, true, false, true},
		{137, true, false, true},
	} {
		t.Run(fmt.Sprintf("exit %d, listener held %v", c.exit, c.held), func(t *testing.T) {
			isolated, inconclusive, detail := isolationOutcome(ContainerStateTerminated{ExitCode: c.exit, Message: "memql: isolation probe: said\n"}, ContainerStateTerminated{ExitCode: probeExitControlPassed}, c.held, "the listener was not ready")
			if isolated != c.isolated || inconclusive != c.inconclusive || detail == "" {
				t.Errorf("= isolated %v, inconclusive %v, %q; want %v, %v and a sentence", isolated, inconclusive, detail, c.isolated, c.inconclusive)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// When the proof runs, and how many of it
// ---------------------------------------------------------------------------

// TestIsolationProofIsCachedOnlyOnPass: a pass is trusted for IsolationTTL --
// an hour -- and proved again once it is that old; a refusal or an
// inconclusive proof is never kept, so the next create proves again.
func TestIsolationProofIsCachedOnlyOnPass(t *testing.T) {
	t.Run("a pass, for an hour", func(t *testing.T) {
		h := newIsoHarness(t, isoScript(probeExitIsolated, isoSaid[probeExitIsolated]))
		for i, wait := range []time.Duration{0, 0, time.Hour - time.Second, time.Second} {
			h.clock.Advance(wait)
			res := h.run(t, isoStep(h, "tests/step-"+strconv.Itoa(i)))
			if res.Status != pl.OutcomeSucceeded {
				t.Fatalf("step %d = %+v, want it run", i, res)
			}
		}
		if n := createsOf(h.c, kubeJobs, isoName()); n != 2 {
			t.Errorf("the probe ran %d times, want twice: once at the start and once the pass was an hour old", n)
		}
		if v := h.r.Isolation(); !v.Isolated || !v.At.Equal(rtT0.Add(time.Hour)) {
			t.Errorf("verdict = %+v, want the second pass, an hour on", v)
		}
	})
	t.Run("a pass stamped ahead of the clock vouches for nothing", func(t *testing.T) {
		h := newIsoHarness(t, isoScript(probeExitIsolated, isoSaid[probeExitIsolated]))
		h.r.isoLast = IsolationVerdict{Isolated: true, Detail: "proved by a clock that ran ahead", At: rtT0.Add(2 * time.Hour)}

		if res := h.run(t, isoStep(h, rtRun().StepKey)); res.Status != pl.OutcomeSucceeded {
			t.Fatalf("result = %+v, want it run", res)
		}
		if n := createsOf(h.c, kubeJobs, isoName()); n != 1 || !h.r.Isolation().At.Equal(rtT0) {
			t.Errorf("the probe ran %d times, verdict %+v; want it proved again, now", n, h.r.Isolation())
		}
	})
	for _, c := range []struct {
		name string
		exit int32
	}{
		{"not isolated, never kept", probeExitConnected},
		{"inconclusive, never kept", probeExitDNS},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newIsoHarness(t, isoScript(c.exit, isoSaid[c.exit]))
			for i := 0; i < 2; i++ {
				isoWantRefused(t, h, h.run(t, rtRun()))
			}
			if n := createsOf(h.c, kubeJobs, isoName()); n != 2 {
				t.Errorf("the probe ran %d times over two creates, want twice", n)
			}
		})
	}
}

// TestIsolationProofIsSharedByConcurrentCreates: steps created at once on one
// replica wait on ONE proof, never one each -- a second probe would also take
// a second slot under the ceiling -- and every one of them starts once it
// passes.
func TestIsolationProofIsSharedByConcurrentCreates(t *testing.T) {
	probe := isoScript(probeExitIsolated, isoSaid[probeExitIsolated])
	held := true
	probe.states[0].until = func(*rtCluster) bool { return !held }
	h := newIsoHarness(t, probe)
	var done []<-chan pl.StepResult
	for i := 0; i < 3; i++ {
		done = append(done, h.start(context.Background(), isoStep(h, "tests/shared-"+strconv.Itoa(i))))
	}
	rtWaitUntil(t, "three steps waiting on one proof", func() bool {
		h.r.isoMu.Lock()
		defer h.r.isoMu.Unlock()
		return h.r.isoProof != nil && h.r.isoProof.waiters == 3
	})
	h.c.with(func(*rtCluster) { held = false })

	for i, d := range done {
		if res := h.await(t, d); res.Status != pl.OutcomeSucceeded {
			t.Errorf("step %d = %+v, want it run", i, res)
		}
	}
	if n := createsOf(h.c, kubeJobs, isoName()); n != 1 {
		t.Errorf("the probe Job was created %d times for three steps, want once", n)
	}
	if n := createsOf(h.c, kubeSecrets, isoTarget()); n != 1 {
		t.Errorf("the probe Secret was created %d times for three steps, want once", n)
	}
	isoLeftNothing(t, h)
}

// TestIsolationProofWaitsOutAnExceededQuota: the probe is one Job, so it waits
// for one slot under the ceiling exactly as a step does (R43) -- under a
// ceiling of one, it runs once the slot is free -- and the step it holds
// waits for it only until its run's ceiling (R31b).
func TestIsolationProofWaitsOutAnExceededQuota(t *testing.T) {
	t.Run("it waits for a slot, and says so once", func(t *testing.T) {
		h := newIsoHarness(t, isoScript(probeExitIsolated, isoSaid[probeExitIsolated]))
		logs := h.logs()
		h.c.with(func(c *rtCluster) {
			c.createJobAnswers = []kubeAnswer{rtQuotaRefusal(isoName()), rtQuotaRefusal(isoName())}
		})
		run := isoStep(h, rtRun().StepKey)

		res := h.run(t, run)

		if res.Status != pl.OutcomeSucceeded {
			t.Fatalf("result = %+v, want the step run once the probe got its slot and passed", res)
		}
		if n := createsOf(h.c, kubeJobs, isoName()); n != 3 {
			t.Errorf("the probe asked for a slot %d times, want three: two refused, then admitted", n)
		}
		if n := logs.count("pipelines: the isolation probe is waiting for a free slot under the pipelines ceiling"); n != 1 {
			t.Errorf("the wait was logged %d times, want once", n)
		}
		isoLeftNothing(t, h)
	})

	t.Run("a step waits for the proof only until its run's ceiling", func(t *testing.T) {
		h := newIsoHarness(t, isoScript(probeExitIsolated, isoSaid[probeExitIsolated]))
		// The last verdict, a pass too old to trust: the proof that stops
		// unfinished must not stand in for it.
		last := IsolationVerdict{Isolated: true, Detail: "proved two hours ago", At: rtT0.Add(-2 * time.Hour)}
		h.r.isoLast = last
		h.c.with(func(c *rtCluster) { c.quotaJobs = map[string]bool{isoName(): true} })
		run := rtRun()
		run.RunDeadline = rtRunDeadline(time.Second)

		res := h.run(t, run)

		rtWantCode(t, res, pl.OutcomeFailed, pl.CodeRunCeiling)
		if !strings.Contains(res.Failure.Message, "isolated") || !strings.Contains(res.Failure.Message, "never started") {
			t.Errorf("failure %q, want it to say the run's ceiling passed while the node proved isolation", res.Failure.Message)
		}
		if n := createsOf(h.c, kubeSecrets, testSecretName); n != 0 || len(h.tokens.called()) != 0 {
			t.Errorf("the step made its Secret (%d) or minted a token (%q) without a proof", n, h.tokens.called())
		}
		// Nobody waits on the proof any more: it stops, and decides nothing.
		isoLeftNothing(t, h)
		if v := h.r.Isolation(); v != last {
			t.Errorf("verdict = %+v, want the last one still, %+v: a proof every step gave up on found nothing", v, last)
		}
	})

	t.Run("a step whose run's ceiling passed before it arrived is not proved for", func(t *testing.T) {
		h := newIsoHarness(t, nil) // a probe Job create would be a test failure: no script describes it
		run := rtRun()
		run.RunDeadline = rtRunDeadline(-5 * time.Minute)

		res := h.run(t, run)

		rtWantCode(t, res, pl.OutcomeFailed, pl.CodeRunCeiling)
		if !strings.Contains(res.Failure.Message, "or for a runner to take it") || strings.Contains(res.Failure.Message, "isolated") {
			t.Errorf("failure %q, want the run's ceiling passed before the step reached this runner, not a proof", res.Failure.Message)
		}
		if n := createsOf(h.c, kubeJobs, isoName()); n != 0 || len(h.tokens.called()) != 0 || h.c.hasSecret(testSecretName) {
			t.Errorf("probe creates %d, tokens %q, step Secret %v; want nothing at all", n, h.tokens.called(), h.c.hasSecret(testSecretName))
		}
	})
}

// TestIsolationProofStartsNothingForAStepNoLongerWaiting: a step whose wait
// is over before it asks -- cancelled, or its run's ceiling passed -- neither starts
// a proof nor joins one: it would only start one that stops at once.
func TestIsolationProofStartsNothingForAStepNoLongerWaiting(t *testing.T) {
	h := newIsoHarness(t, nil) // a probe Job create would be a test failure: no script describes it
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	v, err := h.r.proveIsolation(ctx)

	if !errors.Is(err, context.Canceled) || v != (IsolationVerdict{}) {
		t.Errorf("= %+v, %v; want no verdict and the step's own error", v, err)
	}
	isoSettled(t, h.r)
	if reqs := h.c.requests(); len(reqs) != 0 {
		t.Errorf("%d requests for a step no longer waiting, want none:\n  %s", len(reqs), h.c.summary())
	}
}

// TestIsolationProofTriesAgainWhatMayPass: an API server that could not answer
// the probe Job's create -- a 503 -- is asked again, as for a step's.
func TestIsolationProofTriesAgainWhatMayPass(t *testing.T) {
	h := newIsoHarness(t, isoScript(probeExitIsolated, isoSaid[probeExitIsolated]))
	h.c.with(func(c *rtCluster) { c.createJobAnswers = []kubeAnswer{rtUnavailable, rtUnavailable} })

	if res := h.run(t, isoStep(h, rtRun().StepKey)); res.Status != pl.OutcomeSucceeded {
		t.Fatalf("result = %+v, want the step run", res)
	}
	if n := createsOf(h.c, kubeJobs, isoName()); n != 3 {
		t.Errorf("the probe Job was asked for %d times, want three: two unanswered, then made", n)
	}
	isoLeftNothing(t, h)
}

// TestIsolationProofKeepsItsOwnSecretWhoseAnswerWasLost (fix round 1, minor
// 2): the probe Secret's create was made, but its answer lost on the way
// back, and the retry met it (409). It names this probe Job and this listener
// -- one Runner proves at a time -- so it is this proof's own, and kept.
func TestIsolationProofKeepsItsOwnSecretWhoseAnswerWasLost(t *testing.T) {
	h := newIsoHarness(t, isoScript(probeExitIsolated, isoSaid[probeExitIsolated]))
	h.c.with(func(c *rtCluster) { c.loseSecretCreates = 1 })

	if res := h.run(t, isoStep(h, rtRun().StepKey)); res.Status != pl.OutcomeSucceeded {
		t.Fatalf("result = %+v (failure %+v), want the step run on the Secret the proof made", res, res.Failure)
	}
	if n := createsOf(h.c, kubeSecrets, isoTarget()); n != 2 {
		t.Errorf("the probe Secret was asked for %d times, want twice: made with its answer lost, then met", n)
	}
	if v := h.r.Isolation(); !v.Isolated {
		t.Errorf("verdict = %+v, want isolated", v)
	}
	isoLeftNothing(t, h)
}

// TestIsolationProofArmsOnlyOnceTheListenerIsReady: the probe Secret starts
// the connectors, so it is made only once index 0's pod has an address and its
// listener runs and is ready -- the listener of THIS probe Job, the newest of
// its index -- and index 1's listener runs. The connector waits for the
// Secret, so it cannot try before the listener listens.
func TestIsolationProofArmsOnlyOnceTheListenerIsReady(t *testing.T) {
	up := isoListener(true, 0)
	notReady := isoListener(false, 0)
	older := isoPod(0, "10.42.5.5", up, isoWaiting())
	older.Metadata.Name, older.Metadata.UID, older.Metadata.CreationTimestamp = isoName()+"-0-older", "uid-pod-0-older", rtT0.Add(-time.Minute)
	stale := isoPod(0, "10.42.7.7", up, isoWaiting())
	stale.Metadata.Name, stale.Metadata.UID, stale.Metadata.CreationTimestamp = isoName()+"-0-stale", "uid-pod-0-stale", rtT0.Add(time.Hour)
	for _, c := range []struct {
		name  string
		first []rtState
	}{
		{"index 0's listener runs, not ready yet", []rtState{
			{pods: []*Pod{isoPod(0, isoListenerIP, notReady, isoWaiting()), isoPod(1, isoOtherIP, up, isoWaiting())}, reads: 2},
		}},
		{"index 1's listener has not started", []rtState{
			{pods: []*Pod{isoPod(0, isoListenerIP, up, isoWaiting()), isoPod(1, "", isoStarting, isoWaiting())}, reads: 2},
		}},
		{"index 0 has no address yet", []rtState{
			{pods: []*Pod{isoPod(0, "", up, isoWaiting()), isoPod(1, isoOtherIP, up, isoWaiting())}, reads: 2},
		}},
		{"index 0's pod has no uid", []rtState{
			{pods: []*Pod{func() *Pod {
				p := isoPod(0, isoListenerIP, up, isoWaiting())
				p.Metadata.UID = ""
				return p
			}(), isoPod(1, isoOtherIP, up, isoWaiting())}, reads: 2},
		}},
		{"an older pod of index 0 is not the listener", []rtState{
			{pods: []*Pod{older, isoPod(0, isoListenerIP, notReady, isoWaiting()), isoPod(1, isoOtherIP, up, isoWaiting())}, reads: 2},
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newIsoHarness(t, isoScriptAfter(c.first, probeExitIsolated, isoSaid[probeExitIsolated]))
			madeIn, made, _ := isoSecretMadeIn(h)

			if res := h.run(t, isoStep(h, rtRun().StepKey)); res.Status != pl.OutcomeSucceeded {
				t.Fatalf("result = %+v, want the step run", res)
			}
			if *madeIn < len(c.first) || string(made.Data[probeTargetKey]) != isoListenerIP {
				t.Errorf("the probe Secret was made in state %d naming %q; want it once both were up (state %d), naming %s",
					*madeIn, made.Data[probeTargetKey], len(c.first), isoListenerIP)
			}
		})
	}

	t.Run("a pod an earlier probe Job left is never the listener", func(t *testing.T) {
		s := isoScript(probeExitIsolated, isoSaid[probeExitIsolated])
		for i := range s.states {
			s.states[i].leftover = stale // newest of index 0, ready, and another Job's
		}
		h := newIsoHarness(t, s)
		_, made, _ := isoSecretMadeIn(h)

		if res := h.run(t, isoStep(h, rtRun().StepKey)); res.Status != pl.OutcomeSucceeded {
			t.Fatalf("result = %+v, want the step run", res)
		}
		if got := string(made.Data[probeTargetKey]); got != isoListenerIP {
			t.Errorf("the probe Secret names %q, want this Job's listener %s, not the leftover's", got, isoListenerIP)
		}
	})
}

// TestIsolationProofDeletesALeftoverInItsWay: a probe Job of this replica
// still there when the proof creates its own -- the delete before it had not
// taken yet -- is deleted again, and the proof is made with a Job of its own:
// its Secret names the new Job, never the leftover.
func TestIsolationProofDeletesALeftoverInItsWay(t *testing.T) {
	h := newIsoHarness(t, isoScript(probeExitIsolated, isoSaid[probeExitIsolated]))
	h.r.probeUpWait = 500 * time.Millisecond
	var leftover string
	h.c.with(func(c *rtCluster) {
		c.onJobCreate = func(c *rtCluster, n int) {
			if n == 1 {
				leftover = isoPutLeftoverLocked(c, h.cfg)
			}
		}
	})
	_, made, uid := isoSecretMadeIn(h)

	if res := h.run(t, isoStep(h, rtRun().StepKey)); res.Status != pl.OutcomeSucceeded {
		t.Fatalf("result = %+v, want the step run", res)
	}
	if got := string(made.Data[probeJobUIDKey]); got == leftover || got != *uid {
		t.Errorf("the probe Secret names Job %q, want the proof's own %q, not the leftover %q", got, *uid, leftover)
	}
	if n := createsOf(h.c, kubeJobs, isoName()); n != 2 {
		t.Errorf("the probe Job was created %d times, want twice: refused by the leftover, then made", n)
	}
	isoLeftNothing(t, h)
}

// TestIsolationProofAbandonedAsAStepJoinsIsProvedAgain: a proof every step gave
// up on stops, decides nothing and cleans up; a step that joins it in that
// moment does not take its empty verdict for a refusal, but proves again.
func TestIsolationProofAbandonedAsAStepJoinsIsProvedAgain(t *testing.T) {
	h := newIsoHarness(t, isoScript(probeExitIsolated, isoSaid[probeExitIsolated]))
	probeJob := kubeJobs + "/" + isoName()
	// The first proof never gets a slot, and its step's run has a second left.
	h.c.with(func(c *rtCluster) { c.quotaJobs = map[string]bool{isoName(): true} })
	gaveUp := rtRun()
	gaveUp.RunDeadline = rtRunDeadline(time.Second)
	first := h.start(context.Background(), gaveUp)
	// Once it has cleared the way and asked for a slot, its next delete --
	// the cleanup of a proof nobody waits on -- is held on its way.
	rtWaitUntil(t, "the first proof asking for a slot", func() bool { return createsOf(h.c, kubeJobs, isoName()) > 0 })
	hold := make(chan struct{})
	h.c.with(func(c *rtCluster) { c.holdJobDeletes = map[string]chan struct{}{isoName(): hold} })
	if res := h.await(t, first); res.Failure == nil || res.Failure.Code != pl.CodeRunCeiling {
		t.Fatalf("the first step = %+v, want its run's ceiling passed waiting for the proof", res)
	}
	rtWaitUntil(t, "the stopped proof's cleanup on its way", func() bool { return len(h.c.requestsFor(http.MethodDelete, probeJob)) == 2 })
	h.r.isoMu.Lock()
	stopped := h.r.isoProof
	h.r.isoMu.Unlock()
	if stopped == nil {
		t.Fatal("no proof in flight while its cleanup is held")
	}

	// A step joins the stopped proof before it has finished.
	joined := h.start(context.Background(), isoStep(h, "tests/joined"))
	rtWaitUntil(t, "the second step joining the stopped proof", func() bool {
		h.r.isoMu.Lock()
		defer h.r.isoMu.Unlock()
		return h.r.isoProof == stopped && stopped.waiters == 1
	})
	h.c.with(func(c *rtCluster) {
		c.quotaJobs = nil
		c.holdJobDeletes = nil
	})
	close(hold)

	if res := h.await(t, joined); res.Status != pl.OutcomeSucceeded {
		t.Fatalf("the joining step = %+v, want it run on a proof of its own", res)
	}
	if v := h.r.Isolation(); !v.Isolated {
		t.Errorf("verdict = %+v, want the new proof's pass", v)
	}
	isoLeftNothing(t, h)
}

// TestAdoptionNeverWaitsOnTheProof: adopting a Job another Run created starts
// nothing new in the namespace, so it neither runs the proof nor waits for
// one: a replica that cannot prove isolation still settles the steps another
// replica started.
func TestAdoptionNeverWaitsOnTheProof(t *testing.T) {
	adopted := func(h *rtHarness, t *testing.T) StepRun {
		run := rtRun()
		run.StepKey = "tests/adopted"
		name := JobName(run.RunID, run.StepKey, run.Attempt)
		job, err := BuildJob(h.cfg, run, name)
		if err != nil {
			t.Fatalf("BuildJob: %v", err)
		}
		job.Metadata.Annotations[AnnotRunner] = rtStamp(rtOther, rtT0.Add(-time.Minute))
		h.c.putJob(job, rtFinishingScript(name, 0, captureKubeLine(rtAt(1100), "adopted")))
		h.c.putSecret(BuildSecret(h.cfg, run, name, rtCloneToken))
		return run
	}

	t.Run("an adopter runs no proof", func(t *testing.T) {
		h := newIsoHarness(t, nil) // a probe Job create would be a test failure: no script describes it

		res := h.run(t, adopted(h, t))

		if res.Status != pl.OutcomeSucceeded {
			t.Fatalf("result = %+v, want the adopted step settled", res)
		}
		if n := createsOf(h.c, kubeJobs, isoName()); n != 0 {
			t.Errorf("adopting a step created the probe %d times", n)
		}
		if v := h.r.Isolation(); v != (IsolationVerdict{}) {
			t.Errorf("verdict = %+v, want none", v)
		}
	})

	t.Run("a proof in flight holds no adoption", func(t *testing.T) {
		never := &rtScript{states: []rtState{{pods: []*Pod{isoPod(0, "", isoStarting, isoWaiting()), isoPod(1, "", isoStarting, isoWaiting())}}}}
		h := newIsoHarness(t, never)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		creating := h.start(ctx, rtRun())
		rtWaitUntil(t, "a proof in flight", func() bool {
			h.r.isoMu.Lock()
			defer h.r.isoMu.Unlock()
			return h.r.isoProof != nil && h.r.isoProof.waiters == 1
		})

		res := h.run(t, adopted(h, t))

		if res.Status != pl.OutcomeSucceeded {
			t.Fatalf("result = %+v, want the adopted step settled while the proof waits", res)
		}
		select {
		case res := <-creating:
			t.Fatalf("the creating step answered %+v before its proof did", res)
		default:
		}
		cancel()
		if res := h.await(t, creating); res.Status != pl.OutcomeCancelled {
			t.Errorf("the creating step = %+v, want cancelled", res)
		}
		isoLeftNothing(t, h)
	})
}

// ---------------------------------------------------------------------------
// What the proof leaves behind: nothing
// ---------------------------------------------------------------------------

func TestIsolationProbeRefusesToReuseUnconfirmedCleanup(t *testing.T) {
	h := newIsoHarness(t, isoScript(probeExitIsolated, isoSaid[probeExitIsolated]))
	h.c.with(func(c *rtCluster) { c.podsAnswer = &rtUnavailable })
	verdict, decided := h.r.probeIsolation(context.Background())
	if !decided || !verdict.Inconclusive || verdict.Isolated || !strings.Contains(verdict.Detail, "cleanup remains unconfirmed") {
		t.Fatalf("unreadable cleanup was trusted: %+v, decided=%v", verdict, decided)
	}
	if len(h.c.requestsFor(http.MethodPost, kubeJobs)) != 0 {
		t.Fatal("created another probe before confirming the previous pods were gone")
	}
}

// TestIsolationProofDeletesItsProbeJobs: whatever the verdict, and when every
// step waiting on it gave up, the proof deletes its Job and its Secret --
// foreground propagation, which the fake insists on -- before it answers.
func TestIsolationProofDeletesItsProbeJobs(t *testing.T) {
	neverEnds := isoScript(probeExitIsolated, "")
	neverEnds.states = neverEnds.states[:3]
	for _, c := range []struct {
		name   string
		script *rtScript
		setup  func(h *rtHarness)
		cancel bool
	}{
		{name: "isolated", script: isoScript(probeExitIsolated, isoSaid[probeExitIsolated])},
		{name: "not isolated", script: isoScript(probeExitConnected, isoSaid[probeExitConnected])},
		{name: "inconclusive", script: isoScript(probeExitDNS, isoSaid[probeExitDNS])},
		{name: "the connector never finished", script: neverEnds, setup: func(h *rtHarness) { h.r.probeEndWait = 300 * time.Millisecond }},
		{name: "every step waiting on it gave up", script: neverEnds, cancel: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newIsoHarness(t, c.script)
			if c.setup != nil {
				c.setup(h)
			}
			run := isoStep(h, rtRun().StepKey)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := h.start(ctx, run)
			if c.cancel {
				rtWaitUntil(t, "the connector at work", func() bool { return h.c.hasSecret(isoTarget()) })
				cancel()
			}
			h.await(t, done)

			isoLeftNothing(t, h)
			created := reqIndex(h.c, http.MethodPost, kubeJobs, isoName())
			for _, path := range []string{kubeJobs + "/" + isoName(), kubeSecrets + "/" + isoTarget()} {
				if at := lastReqIndex(h.c, http.MethodDelete, path); at < created {
					t.Errorf("no DELETE %s after the probe was created (last at %d, created at %d)", path, at, created)
				}
			}
		})
	}
}

// TestIsolationProofClearsWhatAnEarlierProofLeft: the probe's names are this
// replica's, the same every time, so whatever its last proof left -- a crash
// between the verdict and the cleanup -- is deleted before the new probe is
// made. A probe Secret left behind would otherwise start the new connector at
// once, against the old listener's address.
func TestIsolationProofClearsWhatAnEarlierProofLeft(t *testing.T) {
	h := newIsoHarness(t, isoScript(probeExitIsolated, isoSaid[probeExitIsolated]))
	old := BuildIsolationProbe(h.cfg, isoName())
	h.c.putJob(old, &rtScript{states: []rtState{{}}})
	old.Metadata.UID = "uid-earlier-probe"
	stale, err := BuildIsolationTarget(h.cfg, old, "10.42.7.7")
	if err != nil {
		t.Fatal(err)
	}
	h.c.putSecret(stale)
	var made Secret
	h.c.with(func(c *rtCluster) {
		c.onSecretCreate = func(_ *rtCluster, s Secret) {
			if s.Metadata.Name == isoTarget() {
				made = s
			}
		}
	})

	res := h.run(t, isoStep(h, rtRun().StepKey))

	if res.Status != pl.OutcomeSucceeded {
		t.Fatalf("result = %+v, want the step run", res)
	}
	created := reqIndex(h.c, http.MethodPost, kubeJobs, isoName())
	for _, path := range []string{kubeJobs + "/" + isoName(), kubeSecrets + "/" + isoTarget()} {
		if at := reqIndex(h.c, http.MethodDelete, path, ""); at < 0 || at > created {
			t.Errorf("DELETE %s came at request %d, the new probe at %d: the leftovers go first", path, at, created)
		}
	}
	if got := string(made.Data[probeTargetKey]); got != isoListenerIP || string(made.Data[probeJobUIDKey]) == "uid-earlier-probe" {
		t.Errorf("the probe Secret holds %q, want the new listener's address and the new Job's uid", made.Data)
	}
	isoLeftNothing(t, h)
}

// TestTheReaperNeverJudgesTheProbeSecret: the orphan-Secret sweep deletes a
// step's Secret no Job came to own; the probe's is owned by its Job from the
// moment it exists and is named like no step's, so the sweep never even asks
// after it.
func TestTheReaperNeverJudgesTheProbeSecret(t *testing.T) {
	h := newRunnerHarness(t)
	probe := BuildIsolationProbe(h.cfg, isoName())
	probe.Metadata.UID = "uid-probe"
	s, err := BuildIsolationTarget(h.cfg, probe, isoListenerIP)
	if err != nil {
		t.Fatal(err)
	}
	s.Metadata.CreationTimestamp = rtT0.Add(-30 * 24 * time.Hour)
	h.c.putSecret(s)
	ownerless := s
	ownerless.Metadata.Name = IsolationTargetName(IsolationProbeName(rtOther))
	ownerless.Metadata.OwnerReferences = nil
	h.c.putSecret(ownerless)

	if n, _, _ := h.r.reap(context.Background()); n != 0 {
		t.Errorf("the sweep deleted %d Secrets, want none", n)
	}
	if !h.c.hasSecret(s.Metadata.Name) || !h.c.hasSecret(ownerless.Metadata.Name) {
		t.Error("the sweep deleted a probe Secret")
	}
	rtNoRequests(t, h.c, http.MethodGet, kubeJobs+"/"+isoName())
	rtNoRequests(t, h.c, http.MethodGet, kubeJobs+"/"+IsolationProbeName(rtOther))
}

// TestRunnerIsolationDefaults: an hour of trust in a pass, unless the Config
// says otherwise, and the probe's two bounds -- 90 s for its pods to come
// up, 60 s for the connector to finish (written out, not read back from the
// constants).
func TestRunnerIsolationDefaults(t *testing.T) {
	cfg := rtConfig()
	cfg.IsolationTTL = 0
	r := NewRunner(cfg, nil, nil, nil, nil)
	if r.cfg.IsolationTTL != time.Hour || r.probeUpWait != 90*time.Second || r.probeEndWait != 60*time.Second || r.probeKubeletWait != 5*time.Second {
		t.Errorf("IsolationTTL %v, bounds %v, %v and %v; want 1h, 1m30s, 1m0s and 5s",
			r.cfg.IsolationTTL, r.probeUpWait, r.probeEndWait, r.probeKubeletWait)
	}
	cfg.IsolationTTL = 5 * time.Minute
	if r := NewRunner(cfg, nil, nil, nil, nil); r.cfg.IsolationTTL != 5*time.Minute {
		t.Errorf("IsolationTTL = %v, want the Config's 5m0s", r.cfg.IsolationTTL)
	}
	if v := NewRunner(cfg, nil, nil, nil, nil).Isolation(); v != (IsolationVerdict{}) {
		t.Errorf("a new Runner's verdict = %+v, want none", v)
	}
}
