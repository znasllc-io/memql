package pipelinesteps

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/workbench"
)

// executor_hop_test.go -- the cross-node hop, in process (CLAUDE.md: a green
// single-node test is a false signal for cross-node behaviour).
//
// An executor on agent-a forwards through hopMesh -- a stand-in for the
// agent's workbench ForwardRouter and the NodeService streams behind it -- to
// two workbench replicas, workbench-a and workbench-b. Each replica answers
// with the REAL integrations/workbench ForwardHandler: its SYSTEM-class gate,
// its step goroutine detached from the stream, its in-flight table and the
// cancel that reaches it. Behind the handlers sits a hopBackend: the runners,
// and the cluster API they share, which is the only state a step has that
// outlives the replica running it.
//
// The backend here is a fake that keeps the runner's multi-replica contract
// (an outcome on the Job wins, a fresh heartbeat is waited on, a stale one is
// adopted, a cancel after the outcome is a no-op), so these tests are about
// the executor's own paths. executor_runner_hop_test.go puts two REAL Runners
// over one fake API server behind the same mesh.
//
// What the mesh fakes is exactly what production produces when a replica
// goes away: the agent's peer table sees the replica DEGRADED -- heartbeats
// stopped -- and nothing the replica would have sent arrives. Not a DRAINING
// flip, which the agent's peer table never sees.

// hopBackend is the workbench side behind the handlers: one runner per
// replica over one shared cluster.
type hopBackend interface {
	// runner is the PipelineRunner the named replica's handler calls.
	runner(node string) workbench.PipelineRunner
	// kill stops the named replica's runner the way a pod's death does: no
	// more heartbeats, no more replies, and nothing persisted after it.
	kill(node string)
	// release lets the step's pod finish successfully.
	release(jobName string)
	// holder is the replica whose heartbeat is on the Job, or "" when there is
	// no such Job.
	holder(jobName string) string
	// jobsCreated is every Job ever created, by name, in order.
	jobsCreated() []string
	// jobsLeft is every Job still in the cluster.
	jobsLeft() []string
	// logFiles is every log archive stored in the Library.
	logFiles() []RunFile
	// putOrphan creates a Job of a run with no runner holding it -- the
	// shape a driver that lost its lease leaves behind.
	putOrphan(run StepRun)
	// stall wedges the named replica's runner while the replica stays
	// healthy in the mesh: its heartbeats stop and no step it is given
	// progresses, though it still answers a status from the cluster.
	stall(node string)
}

// ---------------------------------------------------------------------------
// The mesh
// ---------------------------------------------------------------------------

type hopReplica struct {
	id      string
	handler *workbench.ForwardHandler
	// healthy is the replica as the agent's peer table has it.
	healthy bool
	// alive is whether its process answers at all. A dead replica reads
	// nothing it is sent and sends nothing.
	alive bool
	// lost names the requests whose replies a stream flap lost: the request
	// rode the reconnecting outbox, the reply went out on the old attempt.
	lost map[string]bool
	// dropNextStep loses the next pipelineStep sent to the replica on the
	// way: it never arrives, and nothing tells the sender.
	dropNextStep bool
}

type hopSend struct {
	node, action, requestID string
	pinned, excluded        string
}

// hopMesh is a Forwarder over in-process replicas.
type hopMesh struct {
	mu       sync.Mutex
	replicas []*hopReplica
	inflight map[string]chan *nodev1.WorkbenchForwardResponse
	sends    []hopSend
	// statusNode makes unpinned status reads choose a different replica.
	statusNode string
}

// newHopMesh is the named replicas, each a real ForwardHandler over the
// PipelineRunner runner answers for it.
func newHopMesh(t *testing.T, runner func(node string) workbench.PipelineRunner, nodes ...string) *hopMesh {
	t.Helper()
	m := &hopMesh{inflight: map[string]chan *nodev1.WorkbenchForwardResponse{}}
	for _, n := range nodes {
		m.replicas = append(m.replicas, &hopReplica{id: n, handler: hopHandler(runner(n)), healthy: true, alive: true, lost: map[string]bool{}})
	}
	return m
}

// hopHandler is one replica process's forward handler over its runner.
func hopHandler(runner workbench.PipelineRunner) *workbench.ForwardHandler {
	h := workbench.NewForwardHandler(nil, quietLogger())
	h.SetPipelineRunner(runner)
	return h
}

func (m *hopMesh) SelfNodeId() string   { return exAgent }
func (m *hopMesh) SelfNodeType() string { return "agent" }

func (m *hopMesh) WorkbenchNodeIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for _, replica := range m.replicas {
		ids = append(ids, replica.id)
	}
	return ids
}

func (m *hopMesh) Forward(ctx context.Context, req *nodev1.WorkbenchForwardRequest, pin string) (*nodev1.WorkbenchForwardResponse, string, error) {
	return m.forward(ctx, req, pin, "", 0, nil)
}

func (m *hopMesh) ForwardWatchedExcluding(ctx context.Context, req *nodev1.WorkbenchForwardRequest, pin, exclude string, every time.Duration, onSelected func(string)) (*nodev1.WorkbenchForwardResponse, string, error) {
	if every <= 0 {
		every = 5 * time.Millisecond
	}
	return m.forward(ctx, req, pin, exclude, every, onSelected)
}

// pickLocked is the router's choice: the pinned replica while it is one this
// node would send to, else the first that is -- passing over the excluded
// one while another is healthy, as the router does.
func (m *hopMesh) pickLocked(pin, exclude string) *hopReplica {
	var first, excluded *hopReplica
	for _, r := range m.replicas {
		if !r.healthy {
			continue
		}
		if exclude != "" && r.id == exclude {
			excluded = r
			continue
		}
		if pin != "" && r.id == pin {
			return r
		}
		if first == nil {
			first = r
		}
	}
	if first != nil {
		return first
	}
	return excluded
}

func (m *hopMesh) forward(ctx context.Context, req *nodev1.WorkbenchForwardRequest, pin, exclude string, watch time.Duration, onSelected func(string)) (*nodev1.WorkbenchForwardResponse, string, error) {
	m.mu.Lock()
	choice := pin
	if choice == "" && req.GetAction() == workbench.PipelineStatusAction {
		choice = m.statusNode
	}
	r := m.pickLocked(choice, exclude)
	if r == nil {
		m.mu.Unlock()
		return nil, "", workbench.ErrNoWorkbenchPeer
	}
	if req.RequestId == "" {
		req.RequestId = id.NewShortId()
	}
	ch := make(chan *nodev1.WorkbenchForwardResponse, 1)
	m.inflight[req.RequestId] = ch
	m.sends = append(m.sends, hopSend{node: r.id, action: req.GetAction(), requestID: req.GetRequestId(),
		pinned: pin, excluded: exclude})
	alive, handler := r.alive, r.handler
	if req.GetAction() == workbench.PipelineStepAction && r.dropNextStep {
		// Lost on the way: the replica never reads it.
		r.dropNextStep, alive = false, false
	}
	m.mu.Unlock()
	if onSelected != nil {
		onSelected(r.id)
	}
	defer func() {
		m.mu.Lock()
		delete(m.inflight, req.RequestId)
		m.mu.Unlock()
	}()

	if alive {
		// The replica's receive loop: the node stream calls the handler
		// inline, which is why a step must not block it.
		handler.HandleForwardedRequest(context.Background(), req, m.replyFrom(r))
	}

	var tick <-chan time.Time
	if watch > 0 {
		ticker := time.NewTicker(watch)
		defer ticker.Stop()
		tick = ticker.C
	}
	for {
		select {
		case resp := <-ch:
			return resp, r.id, nil
		case <-ctx.Done():
			// The cancel reaches whichever process answers for the replica
			// now -- after a restart, one that never saw the request.
			if h := m.answering(r); h != nil {
				h.CancelForwardedRequest(context.Background(), req.RequestId)
			}
			return nil, r.id, ctx.Err()
		case <-tick:
			if m.serving(r) {
				continue
			}
			select {
			case resp := <-ch:
				return resp, r.id, nil
			default:
			}
			return nil, r.id, workbench.ErrWorkbenchPeerLost
		}
	}
}

// replyFrom is the replica's send: lost when the replica is dead or a flap
// took the stream the request came in on, delivered to whoever still waits
// otherwise.
func (m *hopMesh) replyFrom(r *hopReplica) func(*nodev1.NodeServerMessage) error {
	return func(msg *nodev1.NodeServerMessage) error {
		m.mu.Lock()
		defer m.mu.Unlock()
		resp := msg.GetWorkbenchForwardResponse()
		if !r.alive || r.lost[resp.GetRequestId()] {
			return errors.New("the stream is gone")
		}
		if ch := m.inflight[resp.GetRequestId()]; ch != nil {
			select {
			case ch <- resp:
			default:
			}
		}
		return nil
	}
}

func (m *hopMesh) serving(r *hopReplica) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return r.healthy
}

// answering is the handler of the process answering for the replica, or nil
// when nothing does.
func (m *hopMesh) answering(r *hopReplica) *workbench.ForwardHandler {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !r.alive {
		return nil
	}
	return r.handler
}

func (m *hopMesh) replica(node string) *hopReplica {
	for _, r := range m.replicas {
		if r.id == node {
			return r
		}
	}
	panic("no replica " + node)
}

// lose takes a replica away the way a dying pod does: DEGRADED in the agent's
// peer table, and silent.
func (m *hopMesh) lose(node string) {
	m.mu.Lock()
	r := m.replica(node)
	r.healthy, r.alive = false, false
	m.mu.Unlock()
}

// dropNextStepTo loses the next step forward on its way to the replica.
func (m *hopMesh) dropNextStepTo(node string) {
	m.mu.Lock()
	m.replica(node).dropNextStep = true
	m.mu.Unlock()
}

// flap bounces the stream to a replica that stays healthy: every reply to a
// request in flight on it is lost, and requests sent after are answered. The
// peer table never notices, which is why the watched forward cannot.
func (m *hopMesh) flap(node string) { m.bounce(node, nil) }

// bounce is flap, and -- given a handler -- the replica's process restarted
// in place with it: a container restart in the same pod, whose new process,
// under the same node id, answers what is sent from now on.
func (m *hopMesh) bounce(node string, restarted *workbench.ForwardHandler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.replica(node)
	for _, s := range m.sends {
		if _, waiting := m.inflight[s.requestID]; waiting && s.node == node {
			r.lost[s.requestID] = true
		}
	}
	if restarted != nil {
		r.handler = restarted
	}
}

func (m *hopMesh) sent(action string) []hopSend {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []hopSend
	for _, s := range m.sends {
		if s.action == action {
			out = append(out, s)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// The fake backend: two runners over one cluster
// ---------------------------------------------------------------------------

type hopJob struct {
	run     StepRun
	runner  string
	beat    time.Time
	outcome []byte
	adopted []string
}

type hopCluster struct {
	mu       sync.Mutex
	jobs     map[string]*hopJob
	created  []string
	library  []RunFile
	released map[string]chan struct{}
	stale    time.Duration
	poll     time.Duration
	runners  map[string]*hopRunner
}

func newHopCluster(nodes ...string) *hopCluster {
	c := &hopCluster{
		jobs:     map[string]*hopJob{},
		released: map[string]chan struct{}{},
		stale:    120 * time.Millisecond,
		poll:     5 * time.Millisecond,
		runners:  map[string]*hopRunner{},
	}
	for _, n := range nodes {
		life, die := context.WithCancel(context.Background())
		c.runners[n] = &hopRunner{node: n, cluster: c, life: life, die: die, stalled: make(chan struct{})}
	}
	return c
}

func (c *hopCluster) runner(node string) workbench.PipelineRunner { return c.runners[node] }
func (c *hopCluster) kill(node string)                            { c.runners[node].die() }
func (c *hopCluster) stall(node string)                           { close(c.runners[node].stalled) }

func (c *hopCluster) releaseChan(job string) chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch, ok := c.released[job]
	if !ok {
		ch = make(chan struct{})
		c.released[job] = ch
	}
	return ch
}

func (c *hopCluster) release(job string) { close(c.releaseChan(job)) }

func (c *hopCluster) holder(job string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if j := c.jobs[job]; j != nil {
		return j.runner
	}
	return ""
}

func (c *hopCluster) jobsCreated() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.created)
}

func (c *hopCluster) jobsLeft() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for name := range c.jobs {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

func (c *hopCluster) logFiles() []RunFile {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []RunFile
	for _, f := range c.library {
		if strings.HasSuffix(f.Name, ".log") {
			out = append(out, f)
		}
	}
	return out
}

func (c *hopCluster) putOrphan(run StepRun) {
	c.mu.Lock()
	defer c.mu.Unlock()
	name := JobName(run.RunID, run.StepKey, run.Attempt)
	c.jobs[name] = &hopJob{run: run, runner: "gone", beat: time.Now()}
	c.created = append(c.created, name)
}

// hopRunner keeps the runner's multi-replica contract over the shared
// cluster, with a life of its own that kill ends, and a stall that wedges it
// while its replica stays up.
type hopRunner struct {
	node    string
	cluster *hopCluster
	life    context.Context
	die     context.CancelFunc
	stalled chan struct{}
}

func (r *hopRunner) isStalled() bool {
	select {
	case <-r.stalled:
		return true
	default:
		return false
	}
}

// wedged is a stalled runner's Run: it touches nothing and answers nothing
// until its context or its replica ends.
func (r *hopRunner) wedged(ctx context.Context) []byte {
	select {
	case <-ctx.Done():
	case <-r.life.Done():
	}
	return nil
}

func (r *hopRunner) RunStep(ctx context.Context, args []byte) []byte {
	if r.isStalled() {
		return r.wedged(ctx)
	}
	var run StepRun
	if err := json.Unmarshal(args, &run); err != nil {
		return mustMarshal(pl.StepResult{Status: pl.OutcomeFailed, ExitCode: -1,
			Failure: &pl.Failure{Code: pl.CodeExecutorError, Message: err.Error()}})
	}
	c := r.cluster
	name := JobName(run.RunID, run.StepKey, run.Attempt)
	for {
		if r.life.Err() != nil {
			return nil
		}
		c.mu.Lock()
		j := c.jobs[name]
		switch {
		case j == nil:
			j = &hopJob{run: run, runner: r.node, beat: time.Now()}
			c.jobs[name] = j
			c.created = append(c.created, name)
			c.mu.Unlock()
			return r.own(ctx, name, run)
		case j.outcome != nil:
			out := j.outcome
			c.mu.Unlock()
			return out
		case time.Since(j.beat) < c.stale:
			// A fresh heartbeat -- another replica's, or this one's own from
			// an earlier forward -- is waited on, never adopted.
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return r.cancelled(name)
			case <-r.life.Done():
				return nil
			case <-time.After(c.poll):
			}
		default:
			j.runner, j.beat = r.node, time.Now()
			j.adopted = append(j.adopted, r.node)
			c.mu.Unlock()
			return r.own(ctx, name, run)
		}
	}
}

func (r *hopRunner) Readiness(context.Context) ([]byte, string) {
	return nil, "readiness_unavailable"
}

// own holds the Job: heartbeats it while this replica lives, and finishes it
// when its pod does.
func (r *hopRunner) own(ctx context.Context, name string, run StepRun) []byte {
	c := r.cluster
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		t := time.NewTicker(c.poll)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-r.life.Done():
				return
			case <-r.stalled:
				return
			case <-t.C:
				c.mu.Lock()
				if j := c.jobs[name]; j != nil && j.runner == r.node {
					j.beat = time.Now()
				}
				c.mu.Unlock()
			}
		}
	}()
	released := c.releaseChan(name)
	for {
		select {
		case <-r.stalled:
			return r.wedged(ctx)
		case <-released:
			if r.isStalled() {
				return r.wedged(ctx)
			}
			return r.finish(name, run)
		case <-ctx.Done():
			return r.cancelled(name)
		case <-r.life.Done():
			return nil
		case <-time.After(c.poll):
			c.mu.Lock()
			j := c.jobs[name]
			c.mu.Unlock()
			if j == nil {
				// Deleted under it: another replica cancelled the run.
				return mustMarshal(pl.StepResult{Status: pl.OutcomeCancelled, ExitCode: -1,
					Failure: &pl.Failure{Code: pl.CodeStepCancelled, Message: "the step's Job was deleted"}})
			}
		}
	}
}

// finish stores the log and persists the outcome on the Job BEFORE the reply.
func (r *hopRunner) finish(name string, run StepRun) []byte {
	c := r.cluster
	c.mu.Lock()
	defer c.mu.Unlock()
	j := c.jobs[name]
	if j == nil {
		return mustMarshal(pl.StepResult{Status: pl.OutcomeCancelled, ExitCode: -1,
			Failure: &pl.Failure{Code: pl.CodeStepCancelled, Message: "the step's Job was deleted"}})
	}
	if j.outcome != nil {
		return j.outcome
	}
	c.library = append(c.library, RunFile{OwnerUserID: run.OwnerUserID, WorkRunID: run.WorkRunID,
		StepKey: run.StepKey, Name: logFileName(run.StepKey), MimeType: "text/plain; charset=utf-8"})
	j.outcome = mustMarshal(pl.StepResult{
		Status:    pl.OutcomeSucceeded,
		Where:     pl.Where{Surface: "cluster", NodeID: r.node, JobName: name},
		LogFileID: "file-" + r.node,
	})
	return j.outcome
}

// cancelled is a cancel: a no-op on a persisted outcome, else the Job goes.
func (r *hopRunner) cancelled(name string) []byte {
	c := r.cluster
	c.mu.Lock()
	defer c.mu.Unlock()
	if j := c.jobs[name]; j != nil {
		if j.outcome != nil {
			return j.outcome
		}
		delete(c.jobs, name)
	}
	return mustMarshal(pl.StepResult{Status: pl.OutcomeCancelled, ExitCode: -1,
		Failure: &pl.Failure{Code: pl.CodeStepCancelled, Message: "cancelled"}})
}

func (r *hopRunner) Status(_ context.Context, args []byte) ([]byte, string) {
	var req StatusRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, "invalid_request"
	}
	c := r.cluster
	c.mu.Lock()
	defer c.mu.Unlock()
	j := c.jobs[req.JobName]
	switch {
	case j == nil:
		return mustMarshal(StatusReply{State: StateAbsent}), ""
	case j.outcome != nil:
		var res pl.StepResult
		_ = json.Unmarshal(j.outcome, &res)
		return mustMarshal(StatusReply{State: StateFinished, Result: &res}), ""
	case time.Since(j.beat) < c.stale:
		return mustMarshal(StatusReply{State: StateRunning, Runner: j.runner}), ""
	}
	return mustMarshal(StatusReply{State: StateStale, Runner: j.runner}), ""
}

func (r *hopRunner) Ack(_ context.Context, args []byte) string {
	var req AckRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return "invalid_request"
	}
	c := r.cluster
	c.mu.Lock()
	delete(c.jobs, req.JobName)
	c.mu.Unlock()
	return ""
}

func (r *hopRunner) CancelRun(_ context.Context, args []byte) ([]byte, string) {
	var req CancelRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, "invalid_request"
	}
	c := r.cluster
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for name, j := range c.jobs {
		if j.run.RunID == req.RunID {
			delete(c.jobs, name)
			n++
		}
	}
	return mustMarshal(map[string]int{"jobsDeleted": n}), ""
}

// ---------------------------------------------------------------------------
// The tests
// ---------------------------------------------------------------------------

// newHop is an executor on agent-a and two workbench replicas over one fake
// cluster (the real Runners' version is newRunnerHop).
func newHop(t *testing.T) (*Executor, *hopMesh, hopBackend) {
	t.Helper()
	cluster := newHopCluster("workbench-a", "workbench-b")
	t.Cleanup(func() {
		for _, r := range cluster.runners {
			r.die()
		}
	})
	mesh := newHopMesh(t, cluster.runner, "workbench-a", "workbench-b")
	// On the executor's fixed clock a stale reading -- the adopting replica
	// waiting out the dead one's heartbeat -- never forwards the step again;
	// the tests about a stale re-forward move the clock.
	return newTestExecutor(mesh, nil), mesh, cluster
}

// TestExecuteReattachesWhenTheWorkbenchIsLost is Review Focus 2: a workbench
// replica restarting mid-step -- a deploy -- must neither hang the step until
// its deadline nor run it twice. workbench-a creates the Job and goes away;
// the agent sees it go, forwards again; workbench-b finds the Job by its
// derived name, waits out a's heartbeat and adopts it. One Job, one Library
// log, the outcome b reports.
func TestExecuteReattachesWhenTheWorkbenchIsLost(t *testing.T) {
	e, mesh, cluster := newHop(t)
	req := exRequest()
	job := JobName(req.RunID, req.StepKey, req.Attempt)

	done := executeAsync(e, context.Background(), req)
	awaitCond(t, func() bool { return cluster.holder(job) == "workbench-a" },
		"workbench-a never created the step's Job")

	mesh.lose("workbench-a")
	cluster.kill("workbench-a")

	awaitCond(t, func() bool { return cluster.holder(job) == "workbench-b" },
		"workbench-b never adopted the Job: a step whose replica died waits out its whole deadline")
	cluster.release(job)

	res := awaitResult(t, done, "workbench-b finished the adopted step")
	if res.Status != pl.OutcomeSucceeded || res.Where.NodeID != "workbench-b" || res.LogFileID != "file-workbench-b" {
		t.Fatalf("result = %+v, want the adopting replica's outcome", res)
	}
	if created := cluster.jobsCreated(); len(created) != 1 || created[0] != job {
		t.Fatalf("Jobs created = %v, want exactly the one %q: the step ran twice", created, job)
	}
	if logs := cluster.logFiles(); len(logs) != 1 || logs[0].StepKey != req.StepKey {
		t.Fatalf("Library logs = %+v, want exactly one for the step", logs)
	}
	steps := mesh.sent(workbench.PipelineStepAction)
	if len(steps) < 2 || steps[0].node != "workbench-a" || steps[len(steps)-1].node != "workbench-b" {
		t.Fatalf("step forwards = %+v, want the first to workbench-a and the re-forward to workbench-b", steps)
	}
	for _, s := range steps[1:] {
		if s.pinned != "" {
			t.Errorf("a re-forward was pinned to %q", s.pinned)
		}
	}
	awaitCond(t, func() bool { return len(cluster.jobsLeft()) == 0 },
		"the finished step's Job was never acked away; it holds a slot of the ceiling until its TTL")
}

// TestExecuteHopRecoversALostReplyThroughTheStatus: the replica that ran the
// step finished it and persisted the outcome, and its reply was lost on the
// way back -- a stream flap the watched forward cannot see, because the
// replica never looked unhealthy. The status, which any replica answers from
// the Job, is how the outcome arrives; the step is never forwarded again.
func TestExecuteHopRecoversALostReplyThroughTheStatus(t *testing.T) {
	e, mesh, cluster := newHop(t)
	req := exRequest()
	job := JobName(req.RunID, req.StepKey, req.Attempt)

	done := executeAsync(e, context.Background(), req)
	awaitCond(t, func() bool { return cluster.holder(job) == "workbench-a" }, "the step never started")
	mesh.flap("workbench-a")
	cluster.release(job)

	res := awaitResult(t, done, "the status found the persisted outcome")
	if res.Status != pl.OutcomeSucceeded || res.Where.NodeID != "workbench-a" {
		t.Fatalf("result = %+v, want workbench-a's persisted outcome", res)
	}
	if n := len(mesh.sent(workbench.PipelineStepAction)); n != 1 {
		t.Fatalf("step forwards = %d, want 1: a finished step is never sent again", n)
	}
	if created, logs := cluster.jobsCreated(), cluster.logFiles(); len(created) != 1 || len(logs) != 1 {
		t.Fatalf("Jobs created %v, logs %d; want one of each", created, len(logs))
	}
	awaitCond(t, func() bool { return len(cluster.jobsLeft()) == 0 }, "the Job was never acked away")
}

// TestCancelHopDeletesTheRunsJobsOnEveryReplica: a run's cancel reaches the
// steps this agent has in flight -- their forwards are cancelled, so the
// replica running each deletes its Job -- and deletes by label the Jobs no
// in-flight forward names, such as one a driver that lost its lease left on
// the cluster. Another run's Job stays.
func TestCancelHopDeletesTheRunsJobsOnEveryReplica(t *testing.T) {
	e, _, cluster := newHop(t)
	e.statusEvery = time.Hour
	first := exRequest()
	second := exRequest()
	second.StepKey, second.Step.Key = "tests.go-tests#3", "tests.go-tests#3"
	r1 := executeAsync(e, context.Background(), first)
	r2 := executeAsync(e, context.Background(), second)
	firstJob := JobName(first.RunID, first.StepKey, first.Attempt)
	secondJob := JobName(second.RunID, second.StepKey, second.Attempt)
	awaitCond(t, func() bool { return cluster.holder(firstJob) != "" && cluster.holder(secondJob) != "" },
		"the run's two steps never started")

	orphan := stepRunFor(exRequest(), 900, pl.CodeStepTimeout)
	orphan.StepKey = "lint.vet"
	cluster.putOrphan(orphan)
	other := stepRunFor(exRequest(), 900, pl.CodeStepTimeout)
	other.RunID = "run-9b9b"
	cluster.putOrphan(other)

	ctx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	if err := e.Cancel(ctx, first.RunID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	for _, ch := range []<-chan pl.StepResult{r1, r2} {
		if res := awaitResult(t, ch, "a cancelled step"); res.Status != pl.OutcomeCancelled {
			t.Errorf("a step of the cancelled run ended %+v, want cancelled", res)
		}
	}
	otherJob := JobName(other.RunID, other.StepKey, other.Attempt)
	awaitCond(t, func() bool { left := cluster.jobsLeft(); return len(left) == 1 && left[0] == otherJob },
		"the cancelled run's Jobs were not all deleted, or another run's Job went with them")
}

// TestExecuteHopForwardsAwayFromTheReplicaThatWentQuiet: the runner holding a
// step's Job wedges on a replica the mesh still thinks healthy -- its
// heartbeat stops and nothing it is handed progresses, so the watched forward
// sees no loss. The stale status names that replica, and the step is
// forwarded AWAY from it: workbench-b adopts the Job, though workbench-a is
// still the first healthy replica a plain pick would hand it straight back to.
func TestExecuteHopForwardsAwayFromTheReplicaThatWentQuiet(t *testing.T) {
	e, mesh, cluster := newHop(t)
	clock := withClock(e)
	e.stalePatience = time.Minute
	req := exRequest()
	job := JobName(req.RunID, req.StepKey, req.Attempt)

	done := executeAsync(e, context.Background(), req)
	awaitCond(t, func() bool { return cluster.holder(job) == "workbench-a" }, "workbench-a never created the step's Job")
	cluster.stall("workbench-a")
	// The patience since the forward has run out: the next stale reading --
	// once workbench-a's heartbeat ages -- is believed.
	clock.advance(time.Minute)

	awaitCond(t, func() bool { return cluster.holder(job) == "workbench-b" },
		"workbench-b never adopted the Job: the step was handed back to the replica that went quiet")
	cluster.release(job)
	res := awaitResult(t, done, "workbench-b finished the adopted step")
	if res.Status != pl.OutcomeSucceeded || res.Where.NodeID != "workbench-b" {
		t.Fatalf("result = %+v, want workbench-b's outcome", res)
	}
	steps := mesh.sent(workbench.PipelineStepAction)
	if len(steps) != 2 || steps[0].node != "workbench-a" || steps[1].node != "workbench-b" || steps[1].excluded != "workbench-a" {
		t.Fatalf("step forwards = %+v, want the first to workbench-a and the re-forward to workbench-b, excluding workbench-a", steps)
	}
	if created, logs := cluster.jobsCreated(), cluster.logFiles(); len(created) != 1 || len(logs) != 1 {
		t.Fatalf("Jobs created %v, logs %d; want one of each", created, len(logs))
	}
}

// TestExecuteHopReforwardsAStepWhoseRequestNeverArrived: the forward is lost
// on its way to workbench-a -- nothing arrives, nothing answers, and the mesh
// still sees a healthy replica, so the watched forward waits on. The status
// reads absent; past the patience the step is forwarded again, and a runner
// makes the Job.
func TestExecuteHopReforwardsAStepWhoseRequestNeverArrived(t *testing.T) {
	e, mesh, cluster := newHop(t)
	clock := withClock(e)
	e.stalePatience = time.Minute
	req := exRequest()
	job := JobName(req.RunID, req.StepKey, req.Attempt)
	mesh.dropNextStepTo("workbench-a")

	done := executeAsync(e, context.Background(), req)
	awaitCond(t, func() bool { return len(mesh.sent(workbench.PipelineStatusAction)) >= 3 },
		"the lost step's status was never read")
	if created := cluster.jobsCreated(); len(created) != 0 {
		t.Fatalf("Jobs created %v from a request that never arrived", created)
	}
	clock.advance(time.Minute)
	awaitCond(t, func() bool { return cluster.holder(job) != "" },
		"a Job absent past the patience was never forwarded again")
	cluster.release(job)
	res := awaitResult(t, done, "the re-forwarded step finished")
	if res.Status != pl.OutcomeSucceeded {
		t.Fatalf("result = %+v, want the step's outcome", res)
	}
	if steps := mesh.sent(workbench.PipelineStepAction); len(steps) != 2 {
		t.Fatalf("step forwards = %+v, want the lost one and one re-forward", steps)
	}
	if created := cluster.jobsCreated(); len(created) != 1 {
		t.Fatalf("Jobs created %v, want one", created)
	}
}
