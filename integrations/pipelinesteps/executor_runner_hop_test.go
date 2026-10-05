package pipelinesteps

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/deploycontrol"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/logger"
	"github.com/znasllc-io/memql/integrations/workbench"
)

// executor_runner_hop_test.go -- the cross-node hop with the REAL runners
// (epic memql#5478, #5493; CLAUDE.md: a green single-node test is a false
// signal for cross-node behaviour).
//
// An Executor on agent-a forwards through executor_hop_test.go's in-process
// mesh -- real integrations/workbench ForwardHandlers on workbench-a and
// workbench-b -- to two REAL Runners over ONE fake API server (runner_test.go's
// rtCluster: Jobs and Secrets, the claim's compare-and-swap, a pod whose life
// is scripted, its followed log). Each replica's process reaches the API
// server, the owner's Library and the log store through links of its own,
// which the process's death cuts: nothing it does reaches any of them again,
// a log it was following included.
//
// Deterministic: when a claim goes stale is the runners' clock and when the
// executor's patience runs out is the executor's clock, both moved by the
// test alone, and the step's life is the fake server's state machine, moved
// on by gates the test opens. Nothing sleeps for time to pass; every wait is
// for a condition. (A status request in flight at the moment a replica goes is
// lost, as production loses it, and costs the executor its callTimeout: time,
// never an outcome.)

// errProcessGone is what a gone process's links answer.
var errProcessGone = errors.New("the replica's process is gone")

// hopLines is the step's output: three lines workbench-a captures live, and
// two more the step prints once the test lets it.
var hopLines = []string{
	captureKubeLine(rtAt(1100), "go test ./a/... ./b"),
	captureKubeLine(rtAt(1200), "ok  \tgithub.com/acme/widget/a\t1.500s"),
	captureKubeLine(rtAt(1300), "=== RUN   TestB"),
	captureKubeLine(rtAt(5100), "--- PASS: TestB (2.00s)"),
	captureKubeLine(rtAt(5200), "ok  \tgithub.com/acme/widget/b\t2.000s"),
}

// hopStamp is a log line's timestamp as the runner records it on the Job.
func hopStamp(line string) string {
	stamp, _, _ := strings.Cut(line, " ")
	return stamp
}

// hopRequest is the Go test step both tests run, with no artifacts.
func hopRequest() pl.StepRequest {
	req := exRequest()
	req.Step.Artifacts = nil
	return req
}

// hopStep is the step's life: it prints hopLines[:3], prints the rest once
// the test opens printing, and exits 0 once the test opens ending.
type hopStep struct {
	printing, ending atomic.Bool
}

func (s *hopStep) script(job string) *rtScript {
	running := rtPod(job, rtStepRunning(rtAt(1000)), clsCloneDone)
	return &rtScript{
		states: []rtState{
			{pod: running, visible: 3, until: func(*rtCluster) bool { return s.printing.Load() }},
			{pod: running, visible: len(hopLines), until: func(*rtCluster) bool { return s.ending.Load() }},
			{pod: rtPod(job, rtStepEnded(0, rtAt(1000), rtAt(9000)), clsCloneDone), visible: len(hopLines)},
		},
		log:   hopLines,
		tails: map[string]string{ContainerClone: rtCloneTail},
	}
}

// ---------------------------------------------------------------------------
// The replicas' processes
// ---------------------------------------------------------------------------

// hopProcess is one workbench replica's process: a real Runner reaching the
// API server, the Library and the log store through links its death cuts.
type hopProcess struct {
	node      string
	runner    *Runner
	transport *http.Transport
	life      context.Context
	die       context.CancelFunc
}

// endRuns ends the process's runs of the steps through the runner's own
// cancel -- CancelRun, which a gone process's cut links leave nothing but
// that -- and waits until the runner holds none of them.
func (p *hopProcess) endRuns(t *testing.T, steps []hopStepRef) {
	t.Helper()
	for _, s := range steps {
		_, _ = p.runner.CancelRun(context.Background(), CancelRequest{RunID: s.run})
	}
	for _, s := range steps {
		rtWaitUntil(t, p.node+"'s runs of "+s.job+" to end", func() bool { return !p.runner.holds(s.job) })
	}
}

// hopStepRef is a step the hop has run: its pipelines run and its Job.
type hopStepRef struct{ run, job string }

// hopLink is a process's connection to the API server. Once the process is
// gone every request is refused, and those in flight -- a followed log among
// them -- are cut off.
type hopLink struct {
	base *http.Transport
	life context.Context
}

func (l hopLink) RoundTrip(req *http.Request) (*http.Response, error) {
	if l.life.Err() != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, errProcessGone
	}
	ctx, cancel := context.WithCancel(req.Context())
	stop := context.AfterFunc(l.life, cancel)
	resp, err := l.base.RoundTrip(req.WithContext(ctx))
	if err != nil {
		stop()
		cancel()
		return nil, err
	}
	resp.Body = &hopBody{ReadCloser: resp.Body, release: func() { stop(); cancel() }}
	return resp, nil
}

type hopBody struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (b *hopBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}

// hopLibrary is the owner's Library as a process reaches it.
type hopLibrary struct {
	life    context.Context
	library LibraryStore
}

func (l hopLibrary) StoreRunFile(ctx context.Context, f RunFile) (StoredFile, error) {
	if l.life.Err() != nil {
		return StoredFile{}, errProcessGone
	}
	return l.library.StoreRunFile(ctx, f)
}

// hopSink is the log store as a process reaches it.
type hopSink struct {
	life context.Context
	sink LineSink
}

func (s hopSink) Write(l logger.Line) {
	if s.life.Err() == nil {
		s.sink.Write(l)
	}
}

// hopRunnerAdapter is the workbench node's PipelineRunner over a real Runner:
// JSON in and out, as app/pipelines_runner_adapter.go speaks it.
type hopRunnerAdapter struct{ r *Runner }

func (a hopRunnerAdapter) RunStep(ctx context.Context, args []byte) []byte {
	var run StepRun
	if err := json.Unmarshal(args, &run); err != nil {
		return mustMarshal(failedResult(pl.CodeExecutorError, err.Error()))
	}
	return mustMarshal(a.r.Run(ctx, run))
}

func (a hopRunnerAdapter) Readiness(context.Context) ([]byte, string) {
	raw, _ := json.Marshal(a.r.Readiness())
	return raw, ""
}

func (a hopRunnerAdapter) Status(ctx context.Context, args []byte) ([]byte, string) {
	var req StatusRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, "decode_args"
	}
	return mustMarshal(a.r.Status(ctx, req)), ""
}

func (a hopRunnerAdapter) Ack(ctx context.Context, args []byte) string {
	var req AckRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return "decode_args"
	}
	if err := a.r.Ack(ctx, req); err != nil {
		return "ack_failed"
	}
	return ""
}

func (a hopRunnerAdapter) CancelRun(ctx context.Context, args []byte) ([]byte, string) {
	var req CancelRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return nil, "decode_args"
	}
	n, err := a.r.CancelRun(ctx, req)
	reply := mustMarshal(map[string]int{"jobsDeleted": n})
	if err != nil {
		return reply, "cancel_failed"
	}
	return reply, ""
}

// ---------------------------------------------------------------------------
// The hop
// ---------------------------------------------------------------------------

// runnerHop is an executor on agent-a, the mesh, and every replica process
// that has run, over one fake API server.
type runnerHop struct {
	t     *testing.T
	h     *rtHarness       // the fake API server, the Library, the log store, the runners' clock
	api   *httptest.Server // the fake API server, as the replicas dial it
	mesh  *hopMesh
	e     *Executor
	clock *exClock // the executor's
	ctx   context.Context
	stop  context.CancelFunc
	logs  *rtLogs // what every process logged

	mu    sync.Mutex
	procs map[string]*hopProcess // the process answering for each replica now
	all   []*hopProcess
	steps []hopStepRef // every step executed
}

// newRunnerHop is the executor and two replicas, workbench-a and
// workbench-b, each a real Runner, over one fake API server.
func newRunnerHop(t *testing.T) *runnerHop {
	t.Helper()
	// Registered before the fake's own cleanups, so it runs after them: the
	// fake ends its followed logs on its closing.
	var api *httptest.Server
	t.Cleanup(func() {
		if api != nil {
			api.Close()
		}
	})
	h := newRunnerHarness(t)
	api = httptest.NewServer(http.HandlerFunc(h.c.serve))
	w := &runnerHop{t: t, h: h, api: api, logs: &rtLogs{}, procs: map[string]*hopProcess{}}
	w.ctx, w.stop = context.WithCancel(context.Background())
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("what the replicas logged:\n%s", w.logs.String())
		}
	})
	for _, node := range []string{"workbench-a", "workbench-b"} {
		w.procs[node] = w.start(node)
	}
	w.mesh = newHopMesh(t, func(node string) workbench.PipelineRunner { return hopRunnerAdapter{w.procs[node].runner} },
		"workbench-a", "workbench-b")
	w.e = newTestExecutor(w.mesh, nil)
	w.clock = withClock(w.e)
	w.e.stalePatience = time.Minute
	// Registered last, so it runs first: every Execute and every Run has ended
	// before the API server closes.
	t.Cleanup(w.end)
	return w
}

// start starts a process for the replica: a Runner under its node id, built
// as the runner's own tests build one, dialing through links of its own.
func (w *runnerHop) start(node string) *hopProcess {
	life, die := context.WithCancel(context.Background())
	cfg := w.h.cfg
	cfg.NodeID = node
	r := w.h.newRunner(cfg)
	transport := &http.Transport{}
	r.kube = NewKube(deploycontrol.NewClusterAPIWith(w.api.URL, "test-token", &http.Client{Transport: hopLink{base: transport, life: life}}), cfg.Namespace)
	r.library = hopLibrary{life: life, library: w.h.lib}
	r.sink = func() LineSink { return hopSink{life: life, sink: w.h.sink} }
	p := &hopProcess{node: node, runner: r, transport: transport, life: life, die: die}
	w.mu.Lock()
	r.log = slog.New(slog.NewTextHandler(w.logs, nil)).With("process", len(w.all), "node", node)
	w.all = append(w.all, p)
	w.mu.Unlock()
	return p
}

func (w *runnerHop) process(node string) *hopProcess {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.procs[node]
}

// execute runs the step through the executor, as the pipeline's driver does,
// and remembers it, so a process's runs of it can be ended through the
// runner's own cancel.
func (w *runnerHop) execute(req pl.StepRequest) <-chan pl.StepResult {
	w.mu.Lock()
	w.steps = append(w.steps, hopStepRef{run: req.RunID, job: JobName(req.RunID, req.StepKey, req.Attempt)})
	w.mu.Unlock()
	return executeAsync(w.e, w.ctx, req)
}

func (w *runnerHop) executed() []hopStepRef {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]hopStepRef(nil), w.steps...)
}

// kill ends a process: its links are cut, and then its runs end -- each by a
// cancel whose every effect the cut links refuse, as a process's death ends
// them with no effect at all.
func (w *runnerHop) kill(p *hopProcess) {
	w.t.Helper()
	p.die()
	p.endRuns(w.t, w.executed())
}

// restart is a container restart of the replica's process, in its pod: the
// process holding the step goes away -- nothing it does reaches the API
// server, the Library or the log store again, and its runs end -- and its
// stream to the agent bounces, losing every reply in flight on it. The
// agent's peer table never sees the replica unhealthy: it reconnects under
// the same node id, a new process whose runner holds nothing.
func (w *runnerHop) restart(node string) {
	w.t.Helper()
	gone, fresh := w.process(node), w.start(node)
	w.mesh.bounce(node, hopHandler(hopRunnerAdapter{fresh.runner}))
	w.kill(gone)
	w.mu.Lock()
	w.procs[node] = fresh
	w.mu.Unlock()
}

// end stops what a test left running, before the API server closes: every
// Execute, then every process's runs, through the runner's own cancel.
func (w *runnerHop) end() {
	w.stop()
	w.mu.Lock()
	all := append([]*hopProcess(nil), w.all...)
	w.mu.Unlock()
	steps := w.executed()
	for _, p := range all {
		p.endRuns(w.t, steps)
		p.die()
		p.transport.CloseIdleConnections()
	}
}

// annotations is the Job's annotations now; nil when there is no such Job.
func (w *runnerHop) annotations(job string) map[string]string {
	var out map[string]string
	w.h.c.with(func(c *rtCluster) {
		if j := c.jobs[job]; j != nil {
			out = maps.Clone(j.job.Metadata.Annotations)
		}
	})
	return out
}

// holder is the replica whose claim is on the Job, "" when none is.
func (w *runnerHop) holder(job string) string {
	node, _, _ := holder(Job{Metadata: ObjectMeta{Annotations: w.annotations(job)}})
	return node
}

// ackedAway waits for the step's Job and Secret to be deleted, and says how
// many times each was.
func (w *runnerHop) ackedAway(t *testing.T, job string) {
	t.Helper()
	secret := SecretName(job)
	rtWaitUntil(t, "the step's Job and Secret to be acked away", func() bool {
		return !w.h.c.hasJob(job) && !w.h.c.hasSecret(secret)
	})
	jobs, secrets := w.h.c.requestsFor(http.MethodDelete, kubeJobs+"/"+job), w.h.c.requestsFor(http.MethodDelete, kubeSecrets+"/"+secret)
	if len(jobs) != 1 || len(secrets) != 1 {
		t.Errorf("the Job was deleted %d time(s) and its Secret %d, want once each", len(jobs), len(secrets))
	}
}

func awaitHop(t *testing.T, ch <-chan pl.StepResult, what string) pl.StepResult {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(30 * time.Second):
		t.Fatalf("Execute did not return: %s", what)
		return pl.StepResult{}
	}
}

// withoutNotices is the step's own lines among lines: the runner's notices
// ("memql: ...") left out.
func withoutNotices(lines []string) []string {
	var out []string
	for _, l := range lines {
		if !strings.HasPrefix(l, "memql: ") {
			out = append(out, l)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// The tests
// ---------------------------------------------------------------------------

// TestExecuteHopRealRunnersAdoptAStepAwayFromTheReplicaThatWentSilent is
// Review Focus 2 with the real runners. workbench-a's runner creates the
// step's Job and captures its first lines; then it goes silent -- its
// heartbeat stops, nothing it does reaches the cluster again -- the way
// production loses it:
//
//   - its process restarts in place: its stream to the agent bounces, losing
//     the reply the step forward waits on, and the peer table never sees the
//     replica unhealthy, so the watched forward waits on. The status the
//     replica answers from the Job names workbench-a as the holder whose
//     claim went stale (StatusReply.Runner, which only the real runner's
//     Status fills); the executor forwards the step again AWAY from it.
//   - its pod dies: the peer table sees it DEGRADED, the watched forward ends,
//     and the executor forwards the step again at once.
//
// Either way workbench-b ADOPTS the Job. Exactly one Job and one Library log
// file, the outcome reaches the executor, and the adopter's archive is the
// whole log (ruling R36): the lines workbench-a captured, replayed, then the
// rest, each once -- as the log store holds them, each once.
func TestExecuteHopRealRunnersAdoptAStepAwayFromTheReplicaThatWentSilent(t *testing.T) {
	for _, c := range []struct {
		name string
		// silence takes workbench-a's runner away, lets the step print on, and
		// moves the clocks past what decides workbench-b may take the step.
		silence func(w *runnerHop, step *hopStep)
		// excluded is what the forward that reached workbench-b steered away
		// from.
		excluded string
	}{
		{"its process restarts in place, and its stream bounces", func(w *runnerHop, step *hopStep) {
			w.restart("workbench-a")
			step.printing.Store(true)
			// workbench-a's claim is stale now, and the executor's patience
			// since its forward has run out.
			w.h.clock.Advance(w.h.cfg.HeartbeatStale + time.Second)
			w.clock.advance(time.Minute)
		}, "workbench-a"},
		{"its pod dies, and the peer table sees it DEGRADED", func(w *runnerHop, step *hopStep) {
			w.mesh.lose("workbench-a")
			w.kill(w.process("workbench-a"))
			step.printing.Store(true)
			// workbench-b waits out workbench-a's claim. The executor's
			// clock stands still: the loss itself forwarded the step again.
			w.h.clock.Advance(w.h.cfg.HeartbeatStale + time.Second)
		}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := newRunnerHop(t)
			req := hopRequest()
			job := JobName(req.RunID, req.StepKey, req.Attempt)
			step := &hopStep{}
			w.h.c.script(job, step.script(job))

			done := w.execute(req)
			// workbench-a's heartbeat has recorded on the Job how far it
			// captured the step, and the step's first line.
			rtWaitUntil(t, "workbench-a to capture the step's first lines", func() bool {
				a := w.annotations(job)
				return w.holder(job) == "workbench-a" && a[AnnotLogCursor] == hopStamp(hopLines[2]) && a[AnnotLogFirst] == hopStamp(hopLines[0])
			})

			c.silence(w, step)
			rtWaitUntil(t, "workbench-b to adopt the step", func() bool { return w.holder(job) == "workbench-b" })
			step.ending.Store(true)
			res := awaitHop(t, done, "workbench-b's outcome")

			if res.Status != pl.OutcomeSucceeded || res.ExitCode != 0 || res.Failure != nil {
				t.Fatalf("result = %+v (failure %+v), want the step's success", res, res.Failure)
			}
			if res.Where.Surface != "cluster" || res.Where.NodeID != "workbench-b" || res.Where.JobName != job {
				t.Errorf("Where = %+v, want the cluster, workbench-b, and the step's Job", res.Where)
			}
			steps := w.mesh.sent(workbench.PipelineStepAction)
			if len(steps) != 2 || steps[0].node != "workbench-a" || steps[0].excluded != "" ||
				steps[1].node != "workbench-b" || steps[1].excluded != c.excluded {
				t.Fatalf("step forwards = %+v, want the first to workbench-a and one more to workbench-b, excluding %q", steps, c.excluded)
			}

			// ONE Job, ONE Library log file.
			if n := len(w.h.c.requestsFor(http.MethodPost, kubeJobs)); n != 1 {
				t.Errorf("%d Jobs created, want exactly one: the step ran twice", n)
			}
			files := w.h.lib.stored()
			if len(files) != 1 || files[0].Name != logFileName(req.StepKey) || res.LogFileID != "file-1" {
				t.Fatalf("Library = %+v and the outcome's log file %q; want exactly the step's log, the outcome naming it", files, res.LogFileID)
			}

			// THE ARCHIVE IS THE WHOLE LOG (ruling R36), and the store has
			// each line once: workbench-a's to its cursor, workbench-b's after.
			archive := string(files[0].Bytes)
			want := rtTexts(hopLines...)
			if got := withoutNotices(strings.Split(strings.TrimSuffix(archive, "\n"), "\n")); !reflect.DeepEqual(got, want) {
				t.Errorf("the archived log is %q\nwant every line the step wrote, once, in order: %q", got, want)
			}
			if !strings.Contains(archive, "memql: re-attached on workbench-b: earlier lines replayed from the node's log, from the step's first line") {
				t.Errorf("the archive does not say it holds the step's log from its first line:\n%s", archive)
			}
			if got := withoutNotices(w.h.sink.messages()); !reflect.DeepEqual(got, want) {
				t.Errorf("the log store holds %q\nwant every line once, in order: %q", got, want)
			}
			// Package a passed in a line only workbench-a captured live.
			if want := map[string]float64{"github.com/acme/widget/a": 1.5, "github.com/acme/widget/b": 2}; !reflect.DeepEqual(res.Timings, want) {
				t.Errorf("Timings = %v, want %v", res.Timings, want)
			}

			w.ackedAway(t, job)
			w.h.leftNoArchive(t)
		})
	}
}

// TestExecuteHopRealRunnersTakeALostReplyFromTheJob: workbench-a runs the step
// to its end and records the outcome on the Job BEFORE it replies; its stream
// to the agent bounced in between, so the reply is lost while the replica
// stays healthy -- nothing the watched forward can see. The status, which
// every replica answers from the Job, finds the step finished: the executor
// takes the outcome from it, never forwards the step again, and acks, and the
// Job and its Secret are deleted once.
func TestExecuteHopRealRunnersTakeALostReplyFromTheJob(t *testing.T) {
	w := newRunnerHop(t)
	req := hopRequest()
	job := JobName(req.RunID, req.StepKey, req.Attempt)
	step := &hopStep{}
	step.printing.Store(true)
	w.h.c.script(job, step.script(job))

	done := w.execute(req)
	rtWaitUntil(t, "workbench-a to capture the step's output", func() bool {
		return w.holder(job) == "workbench-a" && w.annotations(job)[AnnotLogCursor] == hopStamp(hopLines[len(hopLines)-1])
	})
	w.mesh.flap("workbench-a")
	step.ending.Store(true)
	res := awaitHop(t, done, "the outcome, from the step's status")

	var recorded pl.StepResult
	for _, p := range w.h.c.appliedPatches() {
		if v, ok := p.get(AnnotOutcome); ok && p.job == job {
			if err := json.Unmarshal([]byte(v), &recorded); err != nil {
				t.Fatalf("the outcome recorded on the Job does not decode: %v", err)
			}
		}
	}
	if recorded.Status != pl.OutcomeSucceeded || recorded.Where.NodeID != "workbench-a" || !reflect.DeepEqual(res, recorded) {
		t.Fatalf("result = %+v\nwant the outcome workbench-a recorded on the Job: %+v", res, recorded)
	}
	if n := len(w.mesh.sent(workbench.PipelineStepAction)); n != 1 {
		t.Errorf("step forwards = %d, want 1: a step whose outcome is on its Job is never sent again", n)
	}
	if n := len(w.h.c.requestsFor(http.MethodPost, kubeJobs)); n != 1 {
		t.Errorf("%d Jobs created, want exactly one", n)
	}
	if files := w.h.lib.stored(); len(files) != 1 || files[0].Name != logFileName(req.StepKey) {
		t.Errorf("Library = %+v, want exactly the step's log", files)
	}
	w.ackedAway(t, job)
}

// TestExecuteHopAStageWiderThanTheCeilingRunsEveryStep is Review Focus 4 and
// design record D11 under ruling R31b, with the real runners: a stage of six
// steps meets a cluster whose ceiling admits two Jobs at once. The four that
// do not fit wait for a free slot -- longer than their own fifteen-minute
// timeout, by the runners' clock -- and each runs to its end once a slot
// frees, its Job given its whole timeout from its creation: a step's own
// timeout runs from its Job's creation, and only its run's ceiling bounds the
// wait. (Under R31 the four timed out in the queue, never started.) At no
// moment do more Jobs exist than the ceiling admits, and each outcome's ack
// is what frees the next slot.
func TestExecuteHopAStageWiderThanTheCeilingRunsEveryStep(t *testing.T) {
	const width, ceiling = 6, 2
	queued := 20 * time.Minute // past each step's own 15-minute timeout
	w := newRunnerHop(t)
	// The executor's clock starts where the runners' does: the run started
	// ten minutes before it, and its two-hour ceiling is far away. The
	// queue's minutes below pass on the runners' clock alone, so this proves
	// the runner's half end to end; the executor's wait for a queued step is
	// executor_test.go's (TestExecuteWaitsForAQueuedStepUntilItsRunsCeiling,
	// TestExecuteGivesUpOnACreatedJobByItsCreation).
	w.clock.set(rtT0)
	var steppedPast atomic.Bool
	w.h.c.with(func(c *rtCluster) {
		c.jobCeiling = ceiling
		c.onJobCreate = func(c *rtCluster, _ int) {
			// The first try that finds the ceiling full: from here on, the
			// steps still queued have waited longer than they may run.
			if len(c.jobs) >= ceiling && steppedPast.CompareAndSwap(false, true) {
				w.h.clock.Advance(queued)
			}
		}
	})

	var (
		done []<-chan pl.StepResult
		jobs = map[string]string{} // Job -> step key
	)
	for i := 1; i <= width; i++ {
		req := hopRequest()
		req.RunStartedAt = rtT0.Add(-10 * time.Minute).Format(time.RFC3339)
		req.StepKey = "tests.go-tests#" + strconv.Itoa(i)
		req.Step.Key = req.StepKey
		job := JobName(req.RunID, req.StepKey, req.Attempt)
		w.h.c.script(job, rtFinishingScript(job, 0, captureKubeLine(rtAt(1100), "ok  \tgithub.com/acme/widget/a\t1.500s")))
		jobs[job] = req.StepKey
		done = append(done, w.execute(req))
	}
	for i, d := range done {
		res := awaitHop(t, d, "step "+strconv.Itoa(i+1)+" of the stage")
		if res.Status != pl.OutcomeSucceeded || res.ExitCode != 0 || res.Failure != nil {
			t.Errorf("step %d = %+v (failure %+v), want it run to success: a step that waits for a free slot is "+
				"bounded by its run's ceiling, and its own timeout runs from its Job's creation", i+1, res, res.Failure)
		}
	}

	var (
		made     []Job
		most     int
		refusals int
	)
	w.h.c.with(func(c *rtCluster) { made, most = append([]Job(nil), c.made...), c.mostJobs })
	refusals = len(w.h.c.requestsFor(http.MethodPost, kubeJobs)) - len(made)
	if !steppedPast.Load() || refusals == 0 {
		t.Fatalf("the ceiling was never full (%d refused creates): the case is a stage wider than it", refusals)
	}
	if most > ceiling {
		t.Errorf("%d Jobs existed at once, past the ceiling of %d", most, ceiling)
	}
	if len(made) != width {
		t.Fatalf("%d Jobs created, want one per step, %d", len(made), width)
	}
	late := 0
	for _, job := range made {
		if _, ok := jobs[job.Metadata.Name]; !ok {
			t.Errorf("a Job no step names was created: %s", job.Metadata.Name)
		}
		if ads := job.Spec.ActiveDeadlineSeconds; ads == nil || *ads != 900 {
			t.Errorf("Job %s was created with deadline %v, want the step's whole 900s from its creation", job.Metadata.Name, ads)
		}
		if !job.Metadata.CreationTimestamp.Before(rtT0.Add(queued)) {
			late++
		}
	}
	if late < width-ceiling {
		t.Errorf("%d Jobs were created after the queue outlasted the steps' own timeout, want at least %d", late, width-ceiling)
	}
	if files := w.h.lib.stored(); len(files) != width {
		t.Errorf("the Library holds %d files, want each step's log, %d", len(files), width)
	}
	for job := range jobs {
		rtWaitUntil(t, "Job "+job+" and its Secret to be acked away", func() bool {
			return !w.h.c.hasJob(job) && !w.h.c.hasSecret(SecretName(job))
		})
	}
	w.h.leftNoArchive(t)
}
