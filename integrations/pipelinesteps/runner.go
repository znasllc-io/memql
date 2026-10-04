package pipelinesteps

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/znasllc-io/memql/component/deploycontrol"
	pl "github.com/znasllc-io/memql/component/pipelines"
)

// runner.go -- the workbench half (epic memql#5478, #5493, #5495): one step's
// Kubernetes Job, from its creation or adoption to an outcome persisted on it.
//
// Every replica of the workbench node runs a Runner, and any of them may be
// asked for any step: the agent forwards a step to one replica, and forwards it
// again -- to the same replica or another -- when it loses sight of the first.
// The Job is the one thing the replicas share, so everything that has to
// survive a replica is written on it:
//
//   - Its NAME is derived from (run, step, attempt) (JobName), so every replica
//     finds the same Job and none creates a second one.
//   - AnnotRunner is the CLAIM, "<node> <heartbeat>". A runner takes a Job by a
//     compare-and-swap on the resourceVersion it read, so of two runners racing
//     for one Job exactly one wins; it re-stamps the claim every
//     HeartbeatInterval, by compare-and-swap too, so it never stamps over a
//     claim another replica made; and a claim older than HeartbeatStale is
//     abandoned, for any runner to adopt. A runner that finds its claim taken
//     stops and waits for the taker's outcome.
//   - AnnotLogCursor is how far the holder captured the step's log, so an
//     adopter resumes there rather than re-capturing what the store has.
//   - AnnotObservation is how the step ended as the holder first saw it,
//     recorded before the slow half of settling it; AnnotOutcome is the
//     pl.StepResult, persisted BEFORE the runner answers, so a reply lost with
//     a replica is answered again from the Job without running anything.
//
// A done context is a CANCEL, never a lost stream: integrations/workbench
// detaches a step from the stream it arrived on, and only
// WorkbenchForwardCancel ends it. On a cancel the runner deletes the Job and
// its Secret -- unless the outcome is already persisted, which a cancel no
// longer changes.
//
// The persisted outcome must fit a Job annotation: Kubernetes caps all of an
// object's annotations at 256 KiB together. Everything in it is bounded where
// it is made -- the tail at 16 KiB (the capture), a failure's sentence at
// failureMaxBytes, the notes at noteMaxBytes each and notesMaxBytes in all,
// the artifact ids at the extractor's 1024 files. With JSON's six-byte escapes
// that is at most some 96 KiB of tail, 25 of notes and 12 of failure, and
// with the observation beside it the Job stays under the cap for any Library
// file id shorter than about 80 bytes.

const (
	// quickCallTimeout bounds Status, Ack and CancelRun, which run on the
	// workbench's receive loop (integrations/workbench): it reads nothing
	// else while they do.
	quickCallTimeout = 10 * time.Second
	// settleTimeout bounds what a Run does once its step is decided or
	// cancelled -- reading the log to its end, the deletions, the Library,
	// persisting the outcome -- under a context a late cancel cannot end.
	settleTimeout = 2 * time.Minute
	// followDrainTimeout bounds how long a decided step's log may take to
	// be read to its end.
	followDrainTimeout = 30 * time.Second
	// followLineMax is the longest piece of a log line the runner holds at
	// once: a longer line reaches the capture cut into pieces of at most
	// this size, each stamped like the line, so a step that prints one
	// endless line cannot make the workbench hold it whole (Review Focus 5).
	followLineMax = 1 << 20
	// apiAttempts is how many times the runner makes a call it cannot do
	// without -- reading the Job, creating it, claiming it -- while the API
	// server answers what may pass (no answer, a 429, a 5xx), before it
	// fails the step.
	apiAttempts = 10
	// quotaWaitPolls is how many poll intervals the runner waits between two
	// tries at a Job the ceiling's quota refused.
	quotaWaitPolls = 5
	// The notes on one result: at most maxNotes, each at most noteMaxBytes
	// and all together notesMaxBytes; the rest are counted in one last note.
	maxNotes      = 16
	noteMaxBytes  = 2 << 10
	notesMaxBytes = 4 << 10
	// failureMaxBytes bounds a failure's sentence.
	failureMaxBytes = 2 << 10
	// entryNameMaxBytes bounds each tar entry name a note quotes: the names
	// are the step's, and as long as it likes.
	entryNameMaxBytes = 256
	// archiveMIME is the archived log's content type.
	archiveMIME = "text/plain; charset=utf-8"
	// The tails kept of the clone and, when the step failed, of each
	// service.
	cloneTailLines   = 200
	serviceTailLines = 50
	// maxStreamNotes bounds how many distinct sentences of the stream's own
	// (the kubelet's, not the step's) one step's archive keeps.
	maxStreamNotes = 16
)

// stepJobNameShape is what JobName produces. Status and Ack refuse any other
// name: an empty one would address the whole collection -- a GET lists every
// Job, a DELETE deletes them -- and no other Job is a step's to delete.
var stepJobNameShape = regexp.MustCompile(`^mp-[0-9a-f]{24}$`)

func isStepJobName(name string) bool { return stepJobNameShape.MatchString(name) }

// Runner runs pipeline steps as Kubernetes Jobs on the workbench node.
type Runner struct {
	cfg     Config
	kube    *Kube
	sink    func() LineSink
	library LibraryStore
	tokens  TokenMinter
	log     *slog.Logger

	// now is the clock of the claim, of its freshness and of the classifier.
	now func() time.Time
	// tempDir holds each step's log archive until the Library has it.
	tempDir string
	// openCapture opens a step's capture: NewCapture, over this node's
	// store bucket.
	openCapture func(CaptureOptions) (*Capture, error)
	// drainTimeout is followDrainTimeout.
	drainTimeout time.Duration

	mu       sync.Mutex
	inflight map[*inflight]struct{}
}

// inflight is one Run on this replica: what CancelRun cancels by run, and
// what Status asks after by Job.
type inflight struct {
	runID, jobName string
	cancel         context.CancelFunc
}

// NewRunner is the workbench node's runner. sink is the log store, read when
// a step starts (logger.CurrentSink(); nil, not a typed nil, when there is
// none); library and tokens are app/'s.
func NewRunner(cfg Config, kube *Kube, sink func() LineSink, library LibraryStore, tokens TokenMinter) *Runner {
	// The intervals are the loops' bounds; a zero one would spin.
	defaults := ConfigFromEnv(func(string) string { return "" })
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaults.PollInterval
	}
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = defaults.HeartbeatInterval
	}
	if cfg.HeartbeatStale <= 0 {
		cfg.HeartbeatStale = defaults.HeartbeatStale
	}
	cfg.NodeID = strings.TrimSpace(cfg.NodeID)
	return &Runner{
		cfg: cfg, kube: kube, sink: sink, library: library, tokens: tokens,
		log:          slog.Default().With("component", "pipelines.runner"),
		now:          time.Now,
		tempDir:      os.TempDir(),
		openCapture:  NewCapture,
		drainTimeout: followDrainTimeout,
		inflight:     map[*inflight]struct{}{},
	}
}

// Run runs one step, or adopts it, to its outcome (the multi-replica contract
// in the file comment), persists the outcome on the Job and answers it. ctx
// ending is a cancel.
func (r *Runner) Run(ctx context.Context, run StepRun) pl.StepResult {
	jobName := JobName(run.RunID, run.StepKey, run.Attempt)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer r.track(run.RunID, jobName, cancel)()

	s := &step{
		r: r, run: run, jobName: jobName, ctx: ctx,
		// The ids, never the StepRun: its secrets would print.
		log: r.log.With("runId", run.RunID, "workRunId", run.WorkRunID, "stepKey", run.StepKey,
			"attempt", run.Attempt, "jobName", jobName, "node", r.cfg.NodeID),
	}
	defer s.discardCapture()
	return s.execute()
}

// Status says where a step's Job stands: absent, finished with its outcome,
// running under a fresh claim, or stale. A step this replica is running
// before its Job exists -- waiting for a slot under the ceiling -- is running.
func (r *Runner) Status(ctx context.Context, req StatusRequest) StatusReply {
	if !isStepJobName(req.JobName) {
		return StatusReply{State: StateAbsent}
	}
	ctx, cancel := context.WithTimeout(ctx, quickCallTimeout)
	defer cancel()
	job, err := r.kube.GetJob(ctx, req.JobName)
	switch {
	case deploycontrol.IsNotFound(err) && r.holds(req.JobName):
		return StatusReply{State: StateRunning}
	case deploycontrol.IsNotFound(err):
		return StatusReply{State: StateAbsent}
	case err != nil && r.holds(req.JobName):
		return StatusReply{State: StateRunning}
	case err != nil:
		// Nothing here vouches for the step: whoever asks may try another
		// replica.
		r.log.Warn("pipelines: reading a step's Job for its status", "jobName", req.JobName, "error", err)
		return StatusReply{State: StateStale}
	}
	if out, ok := persisted(job); ok {
		return StatusReply{State: StateFinished, Result: &out}
	}
	if _, at, ok := holder(job); ok && r.fresh(at) {
		return StatusReply{State: StateRunning}
	}
	return StatusReply{State: StateStale}
}

// Ack deletes a step's Job and Secret once the agent holds the outcome; what
// is already gone is acked.
func (r *Runner) Ack(ctx context.Context, req AckRequest) error {
	if !isStepJobName(req.JobName) {
		return fmt.Errorf("pipelinesteps: %q is not the name of a step's Job, so nothing was deleted", req.JobName)
	}
	ctx, cancel := context.WithTimeout(ctx, quickCallTimeout)
	defer cancel()
	return errors.Join(r.kube.DeleteJob(ctx, req.JobName), r.kube.DeleteSecret(ctx, SecretName(req.JobName)))
}

// CancelRun deletes every Job and Secret of a run, wherever they were created,
// and cancels this replica's own Runs of it. It answers how many Jobs it
// deleted.
func (r *Runner) CancelRun(ctx context.Context, req CancelRequest) (int, error) {
	if strings.TrimSpace(req.RunID) == "" {
		return 0, errors.New("pipelinesteps: a cancel names the run it stops, and this one names none")
	}
	ctx, cancel := context.WithTimeout(ctx, quickCallTimeout)
	defer cancel()
	// Deleted first, so the count is of the Jobs the run had; the Runs then
	// find their Jobs gone, or delete them again.
	n, err := r.kube.DeleteRun(ctx, req.RunID)
	r.mu.Lock()
	var cancels []context.CancelFunc
	for e := range r.inflight {
		if e.runID == req.RunID {
			cancels = append(cancels, e.cancel)
		}
	}
	r.mu.Unlock()
	for _, c := range cancels {
		c()
	}
	return n, err
}

// track registers a Run until the returned func is called.
func (r *Runner) track(runID, jobName string, cancel context.CancelFunc) func() {
	e := &inflight{runID: runID, jobName: jobName, cancel: cancel}
	r.mu.Lock()
	r.inflight[e] = struct{}{}
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		delete(r.inflight, e)
		r.mu.Unlock()
	}
}

// holds says a Run of this replica is running the Job's step.
func (r *Runner) holds(jobName string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for e := range r.inflight {
		if e.jobName == jobName {
			return true
		}
	}
	return false
}

// isolationGate is where a step is refused before anything exists for it,
// beyond what BuildJob refuses. Task 6b (#5493, ruling R12) proves here that
// memql-pipelines is isolated -- before this replica's first step, and again
// after any proof that did not pass -- and refuses every step while the
// proof fails. Until it lands, every step passes.
func (r *Runner) isolationGate(context.Context) *pl.Refusal { return nil }

// stamp is this replica's claim, as of now.
func (r *Runner) stamp() string {
	return r.cfg.NodeID + " " + r.now().UTC().Format(time.RFC3339)
}

// fresh says a claim stamped at is still held. A stamp further in the future
// than a claim lasts is a clock that cannot vouch for anything: stale.
func (r *Runner) fresh(at time.Time) bool {
	age := r.now().Sub(at)
	return age < r.cfg.HeartbeatStale && age > -r.cfg.HeartbeatStale
}

func (r *Runner) nowText() string { return r.now().UTC().Format(time.RFC3339) }

// ---------------------------------------------------------------------------
// One Run
// ---------------------------------------------------------------------------

// step is one Run of one step.
type step struct {
	r       *Runner
	run     StepRun
	jobName string
	ctx     context.Context
	log     *slog.Logger

	// capture is open while this Run captures the step's output; nil while
	// it only waits on another runner.
	capture     *Capture
	archivePath string
	// token is the clone token this Run minted: masked in the archive.
	token         string
	claimFailures int
	// ownedSecret: this Run made the Job its Secret's owner.
	ownedSecret bool

	mu     sync.Mutex
	cursor time.Time // the timestamp of the last line captured
}

// execute is the multi-replica contract, read and decided again after every
// turn that leaves the Job's state unknown.
func (s *step) execute() pl.StepResult {
	if s.r.cfg.NodeID == "" {
		return s.failed(pl.CodeRunnerUnavailable, "this workbench node has no node id (MEMQL_NODE_ID, or its hostname), so it cannot claim a step's Job; nothing was created")
	}
	var (
		job     Job
		current bool // job was just read or created
		seen    bool // this Run has seen the step's Job exist
	)
	for {
		if s.ctx.Err() != nil {
			return s.abandon(nil)
		}
		if !current {
			var found bool
			var err error
			job, found, err = s.getJob()
			switch {
			case err != nil && s.ctx.Err() != nil:
				return s.abandon(nil)
			case err != nil:
				return s.failed(pl.CodeRunnerUnavailable, "the Kubernetes API server could not be asked for the step's Job: "+apiMessage(err))
			case !found && seen:
				// Deleted between two reads of this Run: a cancel elsewhere.
				return s.vanished(nil)
			case !found:
				res, j, done := s.create()
				if done {
					return res
				}
				job, seen, current = j, true, true
				continue
			}
			seen = true
		}
		current = false

		if out, ok := persisted(job); ok {
			// The reply was lost; the outcome was not.
			s.discardCapture()
			return out
		}
		// A fresh claim is waited on whoever holds it -- this replica too: a
		// re-forward can arrive while its own first Run still holds the Job.
		if _, at, ok := holder(job); ok && s.r.fresh(at) {
			if res, done := s.wait(); done {
				return res
			}
			continue
		}
		res, done := s.own(job)
		if done {
			return res
		}
	}
}

// create creates the step's Secret and then its Job, refusing first what
// cannot run. The Job it answers may be one another Run created first; the
// caller reads it like any existing one.
func (s *step) create() (res pl.StepResult, job Job, done bool) {
	spec, err := BuildJob(s.r.cfg, s.run, s.jobName)
	if err != nil {
		var refusal *pl.Refusal
		if errors.As(err, &refusal) {
			return s.refused(refusal.Code, refusal.Detail), Job{}, true
		}
		return s.failed(pl.CodeJobRejected, err.Error()), Job{}, true
	}
	if refusal := s.r.isolationGate(s.ctx); refusal != nil {
		return s.refused(refusal.Code, refusal.Detail), Job{}, true
	}

	token, err := s.mintToken()
	switch {
	case err != nil && s.ctx.Err() != nil:
		return s.abandon(nil), Job{}, true
	case err != nil:
		// Nothing exists yet, and without a token nothing should.
		return s.failed(pl.CodeCloneFailed, "the clone token could not be minted: "+err.Error()), Job{}, true
	}
	s.token = token
	if err := s.ensureCapture(); err != nil {
		return s.failed(pl.CodeRunnerUnavailable, "this workbench node cannot open the step's log archive: "+err.Error()), Job{}, true
	}

	secret := BuildSecret(s.r.cfg, s.run, s.jobName, token)
	if err := s.retryAPI(func() error { return s.r.kube.CreateSecret(s.ctx, secret) }); err != nil {
		if s.ctx.Err() != nil {
			return s.abandon(nil), Job{}, true
		}
		code, why := createFailure("Secret", err)
		return s.failed(code, why), Job{}, true
	}

	waiting, attempts := false, 0
	for {
		j, made, err := s.r.kube.CreateJob(s.ctx, spec)
		switch {
		case err == nil:
			if made {
				s.log.Info("pipelines: created the step's Job")
				s.ownSecret(j)
			}
			return pl.StepResult{}, j, false
		case s.ctx.Err() != nil:
			return s.abandon(nil), Job{}, true
		case deploycontrol.IsForbiddenQuota(err):
			// The ceiling is full (Review Focus 4): wait for a slot, as long
			// as the step is wanted, and say so once.
			if !waiting {
				waiting = true
				s.capture.Notice("memql: waiting for a free slot under the pipelines ceiling")
				s.log.Info("pipelines: waiting for a free slot under the pipelines ceiling")
			}
			if !s.sleep(quotaWaitPolls * s.r.cfg.PollInterval) {
				return s.abandon(nil), Job{}, true
			}
		case transient(err) && attempts+1 < apiAttempts:
			attempts++
			if !s.sleep(s.r.cfg.PollInterval) {
				return s.abandon(nil), Job{}, true
			}
		default:
			// The Job will not come, so the token must not outlive it.
			s.deleteSecret()
			code, why := createFailure("Job", err)
			return s.failed(code, why), Job{}, true
		}
	}
}

// ownSecret makes the Job its Secret's owner, so the Secret goes with the Job
// -- the ack, the TTL, a cancel; without it, only the ack and a run's cancel
// reach it. The Run that created the Job does it first; a Run that claims a
// Job another Run created does it again, in case that Run went before it
// could.
func (s *step) ownSecret(job Job) {
	if s.ownedSecret {
		return
	}
	switch err := s.r.kube.OwnSecret(s.ctx, SecretName(s.jobName), job); {
	case err == nil:
		s.ownedSecret = true
	case !deploycontrol.IsNotFound(err) && s.ctx.Err() == nil:
		s.log.Warn("pipelines: the step's Secret could not be given its Job as owner", "error", err)
	}
}

// own claims the Job and sees the step to its outcome. A Job that already
// carried a claim -- another runner's, or an earlier one of this replica's --
// is adopted: its step is followed from the cursor its holder left, and the
// log says so. Who created the Job does not matter: the creator's own first
// claim is often refused, because the Job controller has written the Job's
// status since the create answered. done is false when the claim went to
// another runner -- one that claimed the Job first, or adopted it from under
// this one -- and the Run should read the Job again.
func (s *step) own(job Job) (pl.StepResult, bool) {
	if err := s.ensureCapture(); err != nil {
		return s.failed(pl.CodeRunnerUnavailable, "this workbench node cannot open the step's log archive: "+err.Error()), true
	}
	_, _, adopted := holder(job)
	claimed, err := s.r.kube.AnnotateJob(s.ctx, s.jobName, map[string]string{AnnotRunner: s.r.stamp()}, job.Metadata.ResourceVersion)
	switch {
	case err == nil:
	case s.ctx.Err() != nil:
		return s.abandon(nil), true
	case deploycontrol.IsConflict(err):
		// The Job changed since it was read: another runner's claim, or
		// only its status. Read it again and decide again.
		s.log.Info("pipelines: the step's Job changed before it could be claimed; reading it again")
		return pl.StepResult{}, false
	case deploycontrol.IsNotFound(err):
		return s.vanished(nil), true
	default:
		s.claimFailures++
		if s.claimFailures >= apiAttempts || !transient(err) {
			return s.failed(pl.CodeRunnerUnavailable, "the step's Job could not be claimed: "+apiMessage(err)), true
		}
		if !s.sleep(s.r.cfg.PollInterval) {
			return s.abandon(nil), true
		}
		return pl.StepResult{}, false
	}

	hb := s.startHeartbeat()
	defer hb.stop()
	s.ownSecret(claimed)
	if adopted {
		cursor := logCursor(claimed)
		s.setCursor(cursor)
		s.capture.Notice(reattachNotice(s.r.cfg.NodeID, cursor))
		s.log.Info("pipelines: adopted the step's Job", "cursor", cursor)
	}
	if dec, ok := recordedObservation(claimed); ok {
		// Decided before this Run adopted it: settled by what was seen then.
		return s.settle(dec, s.podOf(claimed), nil), true
	}
	return s.watch(hb)
}

// watch polls the claimed Job until its step is decided, following the step's
// log while it runs.
func (s *step) watch(hb *heartbeat) (pl.StepResult, bool) {
	created := s.r.now()
	var (
		f    *follower
		last *Pod // the Job's pod, as last seen
	)
	stopFollowing := func() {
		if f != nil {
			f.stop()
		}
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-s.ctx.Done():
			stopFollowing()
			hb.stop()
			return s.abandon(last), true
		case <-hb.lost:
			stopFollowing()
			s.yield()
			return pl.StepResult{}, false
		case <-timer.C:
		}
		timer.Reset(s.r.cfg.PollInterval)

		job, err := s.r.kube.GetJob(s.ctx, s.jobName)
		switch {
		case deploycontrol.IsNotFound(err):
			stopFollowing()
			hb.stop()
			if s.ctx.Err() != nil {
				return s.abandon(last), true
			}
			return s.vanished(last), true
		case err != nil:
			if s.ctx.Err() == nil {
				s.log.Warn("pipelines: reading the step's Job", "error", err)
			}
			continue
		}
		if out, ok := persisted(job); ok {
			// Another runner settled it.
			stopFollowing()
			s.discardCapture()
			return out, true
		}
		if node, _, ok := holder(job); ok && node != s.r.cfg.NodeID {
			stopFollowing()
			s.yield()
			return pl.StepResult{}, false
		}
		pod, err := s.r.kube.JobPod(s.ctx, s.jobName)
		if err != nil {
			if s.ctx.Err() == nil {
				s.log.Warn("pipelines: reading the step's pod", "error", err)
			}
			continue
		}
		if pod != nil && !podOfJob(pod, job) {
			pod = nil // a leftover of an earlier Job of this name: no pod yet
		}
		if pod != nil {
			last = pod
		}
		obs := Classify(job, pod, created, s.r.now(), s.r.cfg, s.run.DeadlineCode)
		switch obs.Phase {
		case PhaseRunning:
			if f == nil {
				f = s.follow(pod.Metadata.Name, false)
			}
		case PhaseSucceeded, PhaseFailed:
			// The first terminal observation is the step's outcome: what
			// decided it can be gone the next time anyone looks.
			dec := s.decision(obs, pod)
			s.record(dec)
			return s.settle(dec, pod, f), true
		}
	}
}

// yield gives the step up to the runner that holds it now.
func (s *step) yield() {
	s.log.Info("pipelines: another runner holds the step's Job now; waiting for its outcome")
	s.discardCapture()
}

// wait waits on a Job another Run holds, here or on another replica, for its
// outcome. done is false when the claim went stale: the Run reads the Job
// again, and adopts it.
func (s *step) wait() (pl.StepResult, bool) {
	s.discardCapture()
	s.log.Info("pipelines: another runner holds the step's Job; waiting for its outcome")
	for {
		if !s.sleep(s.r.cfg.PollInterval) {
			return s.abandon(nil), true
		}
		job, err := s.r.kube.GetJob(s.ctx, s.jobName)
		switch {
		case deploycontrol.IsNotFound(err):
			if s.ctx.Err() != nil {
				return s.abandon(nil), true
			}
			return s.failed(pl.CodeNodeLost, "the step's Job was deleted while another runner held it, before an outcome was recorded on it"), true
		case err != nil:
			if s.ctx.Err() == nil {
				s.log.Warn("pipelines: reading the step's Job", "error", err)
			}
			continue
		}
		if out, ok := persisted(job); ok {
			return out, true
		}
		if _, at, ok := holder(job); !ok || !s.r.fresh(at) {
			return pl.StepResult{}, false
		}
	}
}

// ---------------------------------------------------------------------------
// Settling a step
// ---------------------------------------------------------------------------

// decision is a terminal observation as the step's result: its status, exit
// code, failure and times, and nothing else yet.
func (s *step) decision(obs Observation, pod *Pod) pl.StepResult {
	dec := pl.StepResult{Status: pl.OutcomeSucceeded, ExitCode: obs.ExitCode}
	if obs.Phase == PhaseFailed {
		dec.Status = pl.OutcomeFailed
	}
	if obs.Failure != nil {
		dec.Failure = &pl.Failure{Code: obs.Failure.Code, Message: cutBytes(s.mask(obs.Failure.Message), failureMaxBytes)}
	}
	dec.StartedAt, dec.FinishedAt = stepTimes(pod)
	if dec.FinishedAt == "" {
		dec.FinishedAt = s.r.nowText()
	}
	return dec
}

// record writes the decision on the Job, so a runner that adopts the step
// before its outcome is persisted settles it the same way.
func (s *step) record(dec pl.StepResult) {
	body, err := json.Marshal(dec)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), quickCallTimeout)
	defer cancel()
	if _, err := s.r.kube.AnnotateJob(ctx, s.jobName, map[string]string{AnnotObservation: string(body)}, ""); err != nil {
		s.log.Warn("pipelines: how the step ended could not be recorded on its Job", "error", err)
	}
}

// settle turns a decided step into its outcome: the log read to its end, the
// clone's and -- for a failed step -- the services' last words archived, the
// log and the artifacts stored in the owner's Library, the outcome persisted
// on the Job. A cancel arriving now changes none of it.
func (s *step) settle(dec pl.StepResult, pod *Pod, f *follower) pl.StepResult {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), settleTimeout)
	defer cancel()
	if pod != nil {
		if f == nil {
			// It ended between two polls: its log is read now.
			f = s.follow(pod.Metadata.Name, true)
		}
		f.drain(s.r.drainTimeout)
		s.tails(ctx, pod.Metadata.Name, dec.Status == pl.OutcomeFailed)
	} else if f != nil {
		f.stop()
	}

	res := dec
	res.Where = s.where()
	notes := s.newNotes()
	cr := s.storeLog(ctx, &res, notes)
	s.storeArtifacts(ctx, cr, &res, notes)
	res.Notes = notes.list()
	// Timings stay empty until the seam exports ParseGoTestOutput (ledger
	// ruling R6).
	s.persist(ctx, res)
	s.log.Info("pipelines: the step is settled", "status", res.Status, "exitCode", res.ExitCode)
	return res
}

// tails archives the last words of the clone and, when the step failed, of
// each service -- as notes in the archive, never in the store, whose live view
// is the step's own output. The kubelet stops every service after every step,
// so their last words only say something when the step failed.
func (s *step) tails(ctx context.Context, pod string, failed bool) {
	s.tail(ctx, pod, ContainerClone, "the clone", cloneTailLines)
	if !failed {
		return
	}
	for _, name := range sortedKeys(s.run.Services) {
		s.tail(ctx, pod, ServicePrefix+name, "service "+name, serviceTailLines)
	}
}

func (s *step) tail(ctx context.Context, pod, container, who string, lines int) {
	out, err := s.r.kube.TailLog(ctx, pod, container, lines)
	switch {
	case errors.Is(err, ErrContainerNotStarted):
		s.capture.Note(fmt.Sprintf("memql: %s never started, so it left no output", who))
	case err != nil:
		s.capture.Note(fmt.Sprintf("memql: %s's output could not be read: %s", who, apiMessage(err)))
	case strings.TrimSpace(out) != "":
		s.capture.Note(fmt.Sprintf("memql: %s's output, its last %d lines:", who, lines))
		s.capture.Note(out)
	}
}

// persist writes the outcome on the Job, unconditionally, before the Run
// answers: a reply lost now is answered again from the Job.
func (s *step) persist(ctx context.Context, res pl.StepResult) {
	body, err := json.Marshal(res)
	if err != nil {
		s.log.Error("pipelines: the step's outcome cannot be encoded", "error", err)
		return
	}
	for attempt := 1; ; attempt++ {
		_, err = s.r.kube.AnnotateJob(ctx, s.jobName, map[string]string{AnnotOutcome: string(body)}, "")
		if err == nil || deploycontrol.IsNotFound(err) || !transient(err) || attempt >= apiAttempts || !sleepCtx(ctx, s.r.cfg.PollInterval) {
			break
		}
	}
	switch {
	case err == nil:
	case deploycontrol.IsNotFound(err):
		s.log.Info("pipelines: the step's Job was deleted before its outcome was recorded on it")
	default:
		s.log.Error("pipelines: the step's outcome could not be recorded on its Job; a reply lost now cannot be answered again", "error", err)
	}
}

// storeLog closes the capture and stores its archive in the owner's Library,
// filling in the result's log fields.
func (s *step) storeLog(ctx context.Context, res *pl.StepResult, notes *noteList) CaptureResult {
	cr, err := s.capture.Close()
	if err != nil {
		notes.add(pl.CodeArtifactMissing, "the step's log archive could not be written whole, so its Library copy may stop early: "+err.Error())
	}
	res.LogTail, res.LogLines, res.LogCapped = cr.Tail, cr.Lines, cr.StoreCapped
	if cr.StoreCapped {
		notes.add(pl.CodeLogCapped, fmt.Sprintf("the live log of this step stops at %d lines; the complete log is archived to the Library", s.r.cfg.LogStoreMaxLines))
	}
	body, err := os.ReadFile(cr.ArchivePath)
	s.removeArchive()
	if err != nil {
		notes.add(pl.CodeArtifactMissing, "the step's log was not stored in the Library: its archive could not be read: "+err.Error())
		return cr
	}
	res.LogFileID = s.store(ctx, logFileName(s.run.StepKey), archiveMIME, body, "the step's log", notes)
	return cr
}

// storeArtifacts stores the step's artifacts, one Library file each, named by
// their path.
func (s *step) storeArtifacts(ctx context.Context, cr CaptureResult, res *pl.StepResult, notes *noteList) {
	if cr.ArtifactNote != nil {
		s.artifactFact(res, notes, *cr.ArtifactNote)
	}
	if cr.Artifacts == nil {
		return
	}
	files, missing, err := ExtractArtifacts(cr.Artifacts, s.run.Artifacts, s.r.cfg.ArtifactMaxBytes)
	var skipped *SkippedEntriesError
	switch {
	case errors.Is(err, ErrArtifactsTooLarge):
		s.artifactFact(res, notes, pl.Failure{Code: pl.CodeArtifactTooLarge,
			Message: "the step's artifacts were not stored: " + strings.TrimPrefix(err.Error(), pl.CodeArtifactTooLarge+": ")})
		return
	case errors.As(err, &skipped):
		notes.add(pl.CodeArtifactMissing, s.skippedNote(skipped))
	case err != nil:
		notes.add(pl.CodeArtifactMissing, "the step's artifacts were not stored: "+err.Error())
		return
	}
	for _, p := range missing {
		notes.add(pl.CodeArtifactMissing, "the declared artifact path "+s.quote(p)+" matched no file")
	}
	for _, f := range files {
		if id := s.store(ctx, artifactFileName(f.Path), artifactMIME(f.Path), f.Bytes, "the artifact "+s.quote(f.Path), notes); id != "" {
			res.ArtifactFileIDs = append(res.ArtifactFileIDs, id)
		}
	}
}

// artifactFact records what became of the artifacts by the class of its
// code: a note changes nothing; a failure-class code fails the step even when
// its command succeeded -- the command's exit code stays as it was, and an
// earlier typed failure keeps its place.
func (s *step) artifactFact(res *pl.StepResult, notes *noteList, f pl.Failure) {
	if class, _ := pl.ClassOf(f.Code); class != pl.ClassFailure {
		notes.add(f.Code, f.Message)
		return
	}
	res.Status = pl.OutcomeFailed
	if res.Failure == nil {
		res.Failure = &pl.Failure{Code: f.Code, Message: cutBytes(s.mask(f.Message), failureMaxBytes)}
	}
}

// skippedNote names the entries the extractor refused, within a note's
// bounds: the names are the step's, masked, and each cut to
// entryNameMaxBytes.
func (s *step) skippedNote(e *SkippedEntriesError) string {
	total := len(e.Entries) + e.More
	var b strings.Builder
	if total == 1 {
		b.WriteString("1 entry of the step's artifacts was not stored: ")
	} else {
		fmt.Fprintf(&b, "%d entries of the step's artifacts were not stored: ", total)
	}
	const room = len(", and 99999 more")
	listed := 0
	for _, entry := range e.Entries {
		name := s.mask(entry.Name)
		if cut := cutBytes(name, entryNameMaxBytes); cut != name {
			name = cut + "..."
		}
		item := strconv.Quote(name) + " (" + entry.Reason + ")"
		if listed > 0 {
			item = ", " + item
		}
		if b.Len()+len(item)+room > noteMaxBytes {
			break
		}
		b.WriteString(item)
		listed++
	}
	if rest := total - listed; rest > 0 {
		fmt.Fprintf(&b, ", and %d more", rest)
	}
	return b.String()
}

// store stores one file in the owner's Library and answers its id, or "" with
// a note saying why it was not stored: never a failure of the step.
func (s *step) store(ctx context.Context, name, mimeType string, body []byte, what string, notes *noteList) string {
	if s.r.library == nil {
		notes.add(pl.CodeArtifactMissing, what+" was not stored: this workbench node has no Library to store it in")
		return ""
	}
	got, err := s.r.library.StoreRunFile(ctx, RunFile{
		OwnerUserID: s.run.OwnerUserID, WorkRunID: s.run.WorkRunID, StepKey: s.run.StepKey,
		Name: name, MimeType: mimeType, Bytes: body,
	})
	switch {
	case err != nil:
		s.log.Warn("pipelines: a step's file could not be stored in the Library", "file", name, "error", err)
		notes.add(pl.CodeArtifactMissing, what+" was not stored in the Library: "+err.Error())
	case got.Omitted != "":
		notes.add(pl.CodeArtifactMissing, what+" was not stored in the Library: "+got.Omitted)
	case got.FileID == "":
		notes.add(pl.CodeArtifactMissing, what+" was not stored in the Library: it answered no file id")
	default:
		return got.FileID
	}
	return ""
}

// ---------------------------------------------------------------------------
// Cancelled, failed, refused
// ---------------------------------------------------------------------------

// abandon answers a Run whose context ended: the agent cancelled the step. A
// persisted outcome stands -- a cancel after it changes nothing, and nothing
// rewrites it. Otherwise the Job and its Secret are deleted and the step is
// cancelled, with what this Run captured archived.
func (s *step) abandon(pod *Pod) pl.StepResult {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), settleTimeout)
	defer cancel()
	if job, err := s.r.kube.GetJob(ctx, s.jobName); err == nil {
		if out, ok := persisted(job); ok {
			s.discardCapture()
			return out
		}
	}
	if err := s.r.kube.DeleteJob(ctx, s.jobName); err != nil {
		s.log.Warn("pipelines: the cancelled step's Job could not be deleted; its TTL will", "error", err)
	}
	if err := s.r.kube.DeleteSecret(ctx, SecretName(s.jobName)); err != nil {
		s.log.Warn("pipelines: the cancelled step's Secret could not be deleted", "error", err)
	}
	s.log.Info("pipelines: the step was cancelled; its Job and Secret are deleted")
	return s.cancelled(ctx, pod, "the step was cancelled")
}

// vanished answers a Run whose Job was deleted under it with no cancel of its
// own: another replica cancelled the run.
func (s *step) vanished(pod *Pod) pl.StepResult {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), settleTimeout)
	defer cancel()
	s.log.Info("pipelines: the step's Job was deleted under this runner: its run was cancelled elsewhere")
	return s.cancelled(ctx, pod, "the step's Job was deleted while it ran: its run was cancelled")
}

func (s *step) cancelled(ctx context.Context, pod *Pod, why string) pl.StepResult {
	res := pl.StepResult{
		Status: pl.OutcomeCancelled, ExitCode: -1,
		Failure: &pl.Failure{Code: pl.CodeStepCancelled, Message: why},
		Where:   s.where(),
	}
	res.StartedAt, _ = stepTimes(pod)
	res.FinishedAt = s.r.nowText()
	if s.capture != nil {
		s.capture.Note("memql: " + why)
		notes := s.newNotes()
		s.storeLog(ctx, &res, notes)
		res.Notes = notes.list()
	}
	return res
}

func (s *step) failed(code, why string) pl.StepResult {
	return pl.StepResult{
		Status: pl.OutcomeFailed, ExitCode: -1,
		Failure:    &pl.Failure{Code: code, Message: cutBytes(why, failureMaxBytes)},
		Where:      s.where(),
		FinishedAt: s.r.nowText(),
	}
}

func (s *step) refused(code, why string) pl.StepResult {
	res := s.failed(code, why)
	res.Status = pl.OutcomeRefused
	return res
}

func (s *step) where() pl.Where {
	return pl.Where{Surface: "cluster", NodeID: s.r.cfg.NodeID, JobName: s.jobName}
}

// deleteSecret deletes the Secret of a Job that was never created.
func (s *step) deleteSecret() {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.ctx), quickCallTimeout)
	defer cancel()
	if err := s.r.kube.DeleteSecret(ctx, SecretName(s.jobName)); err != nil {
		s.log.Warn("pipelines: the Secret of a Job that was never created could not be deleted", "error", err)
	}
}

// ---------------------------------------------------------------------------
// The capture, the claim, the API
// ---------------------------------------------------------------------------

// ensureCapture opens the step's capture over a new archive file.
func (s *step) ensureCapture() error {
	if s.capture != nil {
		return nil
	}
	f, err := os.CreateTemp(s.r.tempDir, "memql-step-*.log")
	if err != nil {
		return err
	}
	archive := f.Name()
	_ = f.Close()
	var sink LineSink
	if s.r.sink != nil {
		sink = s.r.sink()
	}
	secrets := make([]string, 0, len(s.run.Secrets)+1)
	for _, v := range s.run.Secrets {
		secrets = append(secrets, v)
	}
	if s.token != "" {
		secrets = append(secrets, s.token)
	}
	o := CaptureOptions{
		RunID: s.run.RunID, WorkRunID: s.run.WorkRunID, StepKey: s.run.StepKey,
		Secrets:       secrets,
		StoreMaxLines: s.r.cfg.LogStoreMaxLines,
		ArchiveMax:    s.r.cfg.ArchiveMaxBytes,
		ArtifactMax:   s.r.cfg.ArtifactMaxBytes,
		ArchivePath:   archive,
		Sink:          sink,
	}
	// Only a step that declares artifacts expects a frame; any other would
	// be noted for lacking one.
	if len(s.run.Artifacts) > 0 {
		o.Marker = ArtifactMarker(s.jobName)
	}
	c, err := s.r.openCapture(o)
	if err != nil {
		_ = os.Remove(archive)
		return err
	}
	s.capture, s.archivePath = c, archive
	return nil
}

// discardCapture closes the capture and removes its archive unstored: what it
// holds is another runner's to settle, or nobody's.
func (s *step) discardCapture() {
	if s.capture == nil {
		return
	}
	_, _ = s.capture.Close()
	s.removeArchive()
	s.capture = nil
}

func (s *step) removeArchive() {
	if s.archivePath != "" {
		_ = os.Remove(s.archivePath)
		s.archivePath = ""
	}
}

// mask cleans and masks text the step controls, for the result.
func (s *step) mask(text string) string {
	if s.capture == nil {
		return captureRepair(text)
	}
	return s.capture.Mask(text)
}

// quote is text the step controls, masked and quoted.
func (s *step) quote(text string) string { return strconv.Quote(s.mask(text)) }

func (s *step) newNotes() *noteList { return &noteList{mask: s.mask} }

func (s *step) mintToken() (string, error) {
	if s.r.tokens == nil {
		return "", errors.New("this workbench node has no token minter")
	}
	return s.r.tokens.CloneToken(s.ctx, s.run.InstallationID, s.run.Repository.Owner, s.run.Repository.Name)
}

func (s *step) setCursor(at time.Time) {
	s.mu.Lock()
	s.cursor = at
	s.mu.Unlock()
}

func (s *step) getCursor() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursor
}

// holdsClaim reads the Job and says this replica still holds it: the claim on
// it is this replica's, or there is none. A Job that cannot be read is not
// known to be held.
func (s *step) holdsClaim(ctx context.Context) bool {
	job, err := s.r.kube.GetJob(ctx, s.jobName)
	if err != nil {
		return false
	}
	node, _, ok := holder(job)
	return !ok || node == s.r.cfg.NodeID
}

// podOf is the Job's pod, or nil.
func (s *step) podOf(job Job) *Pod {
	pod, err := s.r.kube.JobPod(s.ctx, s.jobName)
	if err != nil || pod == nil || !podOfJob(pod, job) {
		return nil
	}
	return pod
}

// getJob reads the step's Job; found is false for a 404.
func (s *step) getJob() (job Job, found bool, err error) {
	err = s.retryAPI(func() (err error) {
		job, err = s.r.kube.GetJob(s.ctx, s.jobName)
		return err
	})
	if deploycontrol.IsNotFound(err) {
		return Job{}, false, nil
	}
	return job, err == nil, err
}

// retryAPI makes a call again while the API server answers what may pass,
// up to apiAttempts times.
func (s *step) retryAPI(call func() error) error {
	var err error
	for attempt := 1; attempt <= apiAttempts; attempt++ {
		if err = call(); err == nil || !transient(err) || s.ctx.Err() != nil {
			return err
		}
		if !s.sleep(s.r.cfg.PollInterval) {
			return err
		}
	}
	return err
}

func (s *step) sleep(d time.Duration) bool { return sleepCtx(s.ctx, d) }

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// transient is an answer that may pass: none at all, a 429 or a 5xx. The
// caller asks its own context whether it was cancelled.
func transient(err error) bool {
	var se *deploycontrol.StatusError
	if errors.As(err, &se) {
		return se.Code == http.StatusTooManyRequests || se.Code >= 500
	}
	return err != nil
}

// apiMessage is the API server's own sentence for a refusal -- its Status
// message -- or the error.
func apiMessage(err error) string {
	var se *deploycontrol.StatusError
	if errors.As(err, &se) {
		var status struct {
			Message string `json:"message"`
		}
		if json.Unmarshal([]byte(se.Body), &status) == nil && status.Message != "" {
			return status.Message
		}
		return se.Status
	}
	return err.Error()
}

// createFailure is the step's failure when the API server refused to create
// one of its objects outright.
func createFailure(what string, err error) (code, why string) {
	var se *deploycontrol.StatusError
	if errors.As(err, &se) {
		switch se.Code {
		case http.StatusForbidden:
			return pl.CodeRunnerUnavailable, fmt.Sprintf("this workbench node may not create the step's %s: %s", what, apiMessage(err))
		case http.StatusBadRequest, http.StatusUnprocessableEntity:
			return pl.CodeJobRejected, fmt.Sprintf("the cluster refused the step's %s: %s", what, apiMessage(err))
		}
	}
	return pl.CodeRunnerUnavailable, fmt.Sprintf("the step's %s could not be created: %s", what, apiMessage(err))
}

// ---------------------------------------------------------------------------
// The heartbeat
// ---------------------------------------------------------------------------

// heartbeat re-stamps a claim until stopped, and closes lost when another
// runner holds the Job.
type heartbeat struct {
	lost <-chan struct{}
	stop func()
}

func (s *step) startHeartbeat() *heartbeat {
	ctx, cancel := context.WithCancel(context.WithoutCancel(s.ctx))
	lost := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		t := time.NewTicker(s.r.cfg.HeartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if !s.beat(ctx) {
				close(lost)
				return
			}
		}
	}()
	var once sync.Once
	return &heartbeat{lost: lost, stop: func() {
		once.Do(func() {
			cancel()
			<-exited
		})
	}}
}

// beat re-stamps the claim, with the log cursor beside it, by compare-and-swap
// on the version just read: a claim another runner made is never stamped
// over. It answers false when another runner holds the Job.
func (s *step) beat(ctx context.Context) bool {
	for attempt := 0; attempt < 3; attempt++ {
		job, err := s.r.kube.GetJob(ctx, s.jobName)
		if err != nil {
			return true // gone or unreadable: the watch decides on its own read
		}
		if node, _, ok := holder(job); ok && node != s.r.cfg.NodeID {
			return false
		}
		annots := map[string]string{AnnotRunner: s.r.stamp()}
		if c := s.getCursor(); !c.IsZero() {
			annots[AnnotLogCursor] = c.UTC().Format(time.RFC3339Nano)
		}
		_, err = s.r.kube.AnnotateJob(ctx, s.jobName, annots, job.Metadata.ResourceVersion)
		if !deploycontrol.IsConflict(err) {
			if err != nil && ctx.Err() == nil {
				s.log.Warn("pipelines: the step's claim could not be stamped", "error", err)
			}
			return true
		}
		// The Job changed between the read and the stamp -- its status, or
		// a claim: read it again.
	}
	return true
}

// ---------------------------------------------------------------------------
// Following the log
// ---------------------------------------------------------------------------

// follower follows the step container's log into the capture, opening it
// again from the cursor whenever a stream ends while the step runs.
type follower struct {
	s       *step
	capture *Capture
	pod     string
	cancel  context.CancelFunc
	drainC  chan struct{}
	once    sync.Once
	done    chan struct{}
	// noted are the stream's own sentences already archived.
	noted map[string]bool
}

// follow starts following the pod's step container. draining starts it
// already reading to the end: the step has ended.
func (s *step) follow(pod string, draining bool) *follower {
	// Not s.ctx: a decided step's log is read to its end whatever a late
	// cancel says. The watch stops a follower itself on a cancel.
	ctx, cancel := context.WithCancel(context.WithoutCancel(s.ctx))
	f := &follower{
		s: s, capture: s.capture, pod: pod, cancel: cancel,
		drainC: make(chan struct{}), done: make(chan struct{}), noted: map[string]bool{},
	}
	if draining {
		f.once.Do(func() { close(f.drainC) })
	}
	go f.run(ctx)
	return f
}

func (f *follower) run(ctx context.Context) {
	defer close(f.done)
	for opened := false; ctx.Err() == nil; opened = true {
		// A stream opened once the step has ended holds its log to the end.
		final := f.draining()
		// A stream that ended while the step ran -- a dropped connection, or
		// this replica cut off from the API server -- is opened again only
		// while this runner still holds the claim. One cut off long enough
		// to lose it would otherwise pour everything the step printed since
		// its own cursor into the store beside the adopter's copy, in the
		// moment before its next poll sees the claim gone (measured on k3s).
		if opened && !final && !f.s.holdsClaim(ctx) {
			if !f.pause(ctx, final) {
				return
			}
			continue
		}
		rc, err := f.s.r.kube.FollowLog(ctx, f.pod, ContainerStep, f.s.getCursor())
		if err == nil {
			err = f.consume(rc)
			_ = rc.Close()
		}
		if ctx.Err() != nil {
			return
		}
		ended := err == nil || errors.Is(err, ErrContainerNotStarted) || deploycontrol.IsNotFound(err)
		if final && ended {
			return
		}
		if !ended {
			f.s.log.Warn("pipelines: reading the step's log", "error", err)
		}
		if !f.pause(ctx, final) {
			return
		}
	}
}

// pause waits before the next stream: a poll interval, or only until the step
// is decided -- unless it already was, when what failed was the final stream
// and the next waits the interval.
func (f *follower) pause(ctx context.Context, final bool) bool {
	select {
	case <-ctx.Done():
		return false
	case <-f.drainC:
		return !final || sleepCtx(ctx, f.s.r.cfg.PollInterval)
	case <-time.After(f.s.r.cfg.PollInterval):
		return true
	}
}

func (f *follower) draining() bool {
	select {
	case <-f.drainC:
		return true
	default:
		return false
	}
}

// drain reads the log to its end, waiting at most timeout, and stops.
func (f *follower) drain(timeout time.Duration) {
	f.once.Do(func() { close(f.drainC) })
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-f.done:
	case <-t.C:
		f.s.log.Warn("pipelines: the step's log did not end in time; its archive may stop early")
	}
	f.stop()
}

func (f *follower) stop() {
	f.cancel()
	<-f.done
}

// noteStream archives, once, a sentence the kubelet or the API server wrote
// into the stream: a stream opened again repeats it, having no timestamp to
// be dropped by.
func (f *follower) noteStream(text string) {
	if f.noted[text] || len(f.noted) >= maxStreamNotes {
		return
	}
	f.noted[text] = true
	f.capture.Note("memql: the log stream reported: " + text)
}

// consume feeds one stream to the capture, line by line. A resumed stream
// starts at the cursor's whole second (Kube.FollowLog), so the lines stamped
// at or before the cursor are the store's already and are dropped; every line
// fed moves the cursor.
func (f *follower) consume(r io.Reader) error {
	lines := newLogLines(r, followLineMax)
	cursor := f.s.getCursor()
	var (
		stamp time.Time // the line's timestamp, for its continuation pieces
		skip  bool      // the line is not the step's to capture
	)
	for {
		piece, first, err := lines.next()
		switch {
		case piece == "" && (err != nil || !first):
			// Nothing after the last newline, or after a long line's last cut.
		case first:
			at, _, ok := captureSplitStamp(piece)
			switch {
			case !ok:
				// Followed with timestamps, every line the step writes comes
				// stamped: an unstamped one is the kubelet or the API server
				// speaking in the stream (measured on k3s v1.32, following a
				// pod as it is deleted: "failed to try resolving symlinks in
				// path .../step/0.log ..."). Archived as that, never as the
				// step's own words.
				skip = true
				f.noteStream(piece)
			case !cursor.IsZero() && !at.After(cursor):
				skip = true
			default:
				skip, stamp, cursor = false, at, at
				f.capture.Feed(piece)
				f.s.setCursor(at)
			}
		case !skip:
			// The rest of a long line, stamped like its start.
			f.capture.Feed(stamp.Format(time.RFC3339Nano) + " " + piece)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// logLines reads a stream as lines without holding more than max bytes of any
// one: a longer line comes in pieces of at most max bytes, each cut where a
// rune starts.
type logLines struct {
	br    *bufio.Reader
	max   int
	carry []byte
	mid   bool // the last piece ended inside its line
}

func newLogLines(r io.Reader, max int) *logLines {
	return &logLines{br: bufio.NewReaderSize(r, max), max: max}
}

// next is the next piece of the stream, without its newline. first says it
// begins a line rather than continuing one. A stream that ends inside a line
// answers that line's last piece with the stream's error.
func (l *logLines) next() (piece string, first bool, err error) {
	first = !l.mid
	chunk, err := l.br.ReadSlice('\n')
	buf := append(l.carry, chunk...)
	l.carry = nil
	switch {
	case err == nil:
		l.mid = false
		buf = buf[:len(buf)-1]
		if n := len(buf); n > 0 && buf[n-1] == '\r' {
			buf = buf[:n-1]
		}
		return string(buf), first, nil
	case errors.Is(err, bufio.ErrBufferFull):
		cut := runeCut(buf, l.max)
		l.carry = append([]byte(nil), buf[cut:]...)
		l.mid = true
		return string(buf[:cut]), first, nil
	default:
		l.mid = false
		return string(buf), first, err
	}
}

// runeCut is where a piece of b of at most max bytes ends on a rune boundary:
// at max, moved back to where a rune starts when it falls inside one -- or,
// when b is no longer than max, at its end, less an incomplete last rune.
func runeCut(b []byte, max int) int {
	if len(b) > max {
		for c := max; c > 0 && c > max-utf8.UTFMax; c-- {
			if utf8.RuneStart(b[c]) {
				return c
			}
		}
		return max
	}
	for c := len(b) - 1; c >= 0 && c > len(b)-1-utf8.UTFMax; c-- {
		if utf8.RuneStart(b[c]) {
			if utf8.FullRune(b[c:]) {
				return len(b)
			}
			return c
		}
	}
	return len(b)
}

// ---------------------------------------------------------------------------
// Reading what is on a Job
// ---------------------------------------------------------------------------

// holder reads a Job's claim: the node that holds it, and its last heartbeat.
func holder(job Job) (node string, at time.Time, ok bool) {
	v := strings.TrimSpace(job.Metadata.Annotations[AnnotRunner])
	i := strings.LastIndexByte(v, ' ')
	if i <= 0 {
		return "", time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, v[i+1:])
	if err != nil {
		return "", time.Time{}, false
	}
	return strings.TrimSpace(v[:i]), at, true
}

// persisted is the outcome recorded on a Job. One that cannot be read is
// answered as an executor error, never by running the step again.
func persisted(job Job) (pl.StepResult, bool) {
	raw := job.Metadata.Annotations[AnnotOutcome]
	if raw == "" {
		return pl.StepResult{}, false
	}
	var out pl.StepResult
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return pl.StepResult{
			Status: pl.OutcomeFailed, ExitCode: -1,
			Failure: &pl.Failure{Code: pl.CodeExecutorError,
				Message: "the outcome recorded on the step's Job cannot be read (" + err.Error() + "); the step was not run again"},
			Where: pl.Where{Surface: "cluster", JobName: job.Metadata.Name},
		}, true
	}
	return out, true
}

// recordedObservation is the decision recorded on a Job (AnnotObservation):
// its status, exit code, failure and times only.
func recordedObservation(job Job) (pl.StepResult, bool) {
	raw := job.Metadata.Annotations[AnnotObservation]
	if raw == "" {
		return pl.StepResult{}, false
	}
	var dec pl.StepResult
	if json.Unmarshal([]byte(raw), &dec) != nil {
		return pl.StepResult{}, false
	}
	switch dec.Status {
	case pl.OutcomeSucceeded, pl.OutcomeFailed:
		return pl.StepResult{Status: dec.Status, ExitCode: dec.ExitCode, Failure: dec.Failure, StartedAt: dec.StartedAt, FinishedAt: dec.FinishedAt}, true
	}
	return pl.StepResult{}, false
}

// logCursor is the Job's log cursor, or zero.
func logCursor(job Job) time.Time {
	at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(job.Metadata.Annotations[AnnotLogCursor]))
	if err != nil {
		return time.Time{}
	}
	return at
}

// podOfJob says a pod is the Job's: its controller-uid label, or else its
// controller owner reference, names the Job's uid. JobPod selects by the
// job-name label alone, and names repeat: the Job a cancel or an ack deleted
// leaves its pod to the garbage collector, and the next Job of the same (run,
// step, attempt) would otherwise read that pod as its own.
func podOfJob(pod *Pod, job Job) bool {
	uid := job.Metadata.UID
	if uid == "" {
		return false
	}
	for _, key := range []string{"batch.kubernetes.io/controller-uid", "controller-uid"} {
		if v, ok := pod.Metadata.Labels[key]; ok {
			return v == uid
		}
	}
	for _, ref := range pod.Metadata.OwnerReferences {
		if ref.Controller != nil && *ref.Controller {
			return ref.UID == uid
		}
	}
	return false
}

// stepTimes are when the step container started and ended, as RFC 3339.
func stepTimes(pod *Pod) (started, finished string) {
	cs := podContainer(pod, false, ContainerStep)
	switch {
	case cs == nil:
	case cs.State.Terminated != nil:
		return rfc3339(cs.State.Terminated.StartedAt), rfc3339(cs.State.Terminated.FinishedAt)
	case cs.State.Running != nil:
		return rfc3339(cs.State.Running.StartedAt), ""
	}
	return "", ""
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func reattachNotice(node string, cursor time.Time) string {
	if cursor.IsZero() {
		return "memql: re-attached on " + node + "; no line of the step's output had been captured, so it is followed from its start"
	}
	return "memql: re-attached on " + node + "; lines before " + cursor.UTC().Format(time.RFC3339Nano) + " are in the store already"
}

// logFileName is the Library name of a step's archived log: the step key --
// "stage.step" or "stage.step#i" -- with every byte outside [A-Za-z0-9._-]
// made a dash.
func logFileName(stepKey string) string {
	b := []byte(stepKey)
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			b[i] = '-'
		}
	}
	return string(b) + ".log"
}

// artifactFileName is an artifact's Library name: its path, every slash a
// double underscore.
func artifactFileName(p string) string { return strings.ReplaceAll(p, "/", "__") }

// artifactMIME is an artifact's content type, by its extension.
func artifactMIME(p string) string {
	if t := mime.TypeByExtension(path.Ext(p)); t != "" {
		return t
	}
	return "application/octet-stream"
}

// cutBytes is s cut to at most n bytes, where a rune starts.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// noteList collects a result's notes within their bounds: at most maxNotes,
// each at most noteMaxBytes and all together notesMaxBytes; past the bounds
// they are counted in one last note.
type noteList struct {
	mask  func(string) string
	notes []pl.Failure
	bytes int
	more  int
	code  string
}

func (n *noteList) add(code, message string) {
	message = cutBytes(n.mask(message), noteMaxBytes)
	if len(n.notes) >= maxNotes || n.bytes+len(message) > notesMaxBytes {
		if n.more == 0 {
			n.code = code
		}
		n.more++
		return
	}
	n.notes = append(n.notes, pl.Failure{Code: code, Message: message})
	n.bytes += len(message)
}

func (n *noteList) list() []pl.Failure {
	if n.more == 0 {
		return n.notes
	}
	more := "1 more note was left out"
	if n.more > 1 {
		more = fmt.Sprintf("%d more notes were left out", n.more)
	}
	return append(n.notes, pl.Failure{Code: n.code, Message: more})
}
