package pipelinesteps

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

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
// object's annotations at 256 KiB together. Most of it is bounded where it is
// made -- the tail at 16 KiB (the capture), a failure's sentence at
// failureMaxBytes, the notes at noteMaxBytes each and notesMaxBytes in all --
// which with JSON's six-byte escapes is at most some 96 KiB of tail, 25 of
// notes and 12 of failure. The artifact file ids (up to the extractor's 1024,
// each as long as the Library makes it) and the Go timings (one per passing
// package) are not, so settling measures the encoded outcome and cuts those
// two to outcomeMaxBytes (fitOutcome): an outcome the API server refused
// would be lost, and with it the answer to a reply lost with this replica.

const (
	// quickCallTimeout bounds Status, Ack and CancelRun. The workbench
	// answers each on a goroutine of its own, off its receive loop
	// (integrations/workbench), so an API server that hangs costs one
	// goroutine per call for at most this long, and the agent its answer.
	quickCallTimeout = 10 * time.Second
	// libraryPhaseTimeout bounds the owner's Library writes of one settled
	// step -- its log and its artifacts -- under a context a late cancel
	// cannot end, and that no other phase of settling shares.
	libraryPhaseTimeout = 2 * time.Minute
	// persistTimeout bounds recording the outcome on the Job, retries
	// included: a window of its own, so nothing spent before it is taken
	// from it.
	persistTimeout = 30 * time.Second
	// tailsTimeout bounds every tail one settled step takes -- the clone's
	// and, for a failed step, each service's -- together, however many
	// services it has.
	tailsTimeout = 20 * time.Second
	// settleBudget is the most settling a decided step can take, every
	// phase at its deadline: the observation recorded, the log drained, the
	// tails, the Library writes and the outcome persisted. The agent's grace
	// past a step's deadline is derived from it (nodeLostGrace), so the agent
	// never gives up on a step that is still settling (fix round 2, minor 2).
	settleBudget = quickCallTimeout + followDrainTimeout + tailsTimeout + libraryPhaseTimeout + persistTimeout
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
	// apiTroubleRepeat is how often a polling loop logs an API error again
	// while what it says has not changed (apiTrouble).
	apiTroubleRepeat = 5 * time.Minute
	// outcomeMaxBytes is the most the encoded outcome may take. The other
	// annotations on the Job -- the observation (at most some 13 KiB), the
	// claim, the log cursor, the step's first line (memql.io/log-first) and
	// the step's identity -- stay well inside the 32 KiB of the 256 KiB this
	// leaves them.
	outcomeMaxBytes = 224 << 10
	// tokenRefreshAge is how old a clone token may be when the step's Job is
	// created: an installation token lasts an hour, the step queues for as
	// long as the ceiling is full, and the clone runs once the pod is placed
	// and its images pulled (freshenToken).
	tokenRefreshAge = 30 * time.Minute
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
	// libraryTimeout is libraryPhaseTimeout; tailsTimeout is tailsTimeout.
	libraryTimeout time.Duration
	tailsTimeout   time.Duration

	mu       sync.Mutex
	inflight map[*inflight]struct{}
	// claims is which Run of this replica holds -- or is taking -- each
	// Job's claim. A claim names the node, and every Run here shares the
	// node's id, so the id alone cannot say whose a claim stamped by this
	// replica is (review finding 3); this can.
	claims map[string]*step
	// Maintenance retains its scan position between bounded batches so a
	// full first page cannot starve later orphans. Never shared across replicas.
	reapMu      sync.Mutex
	reapCursor  string
	reapPending []ObjectMeta
	// statusTrouble is what Status last logged of the API errors it met.
	statusTrouble apiTrouble

	// The isolation proof (isolation.go): the last verdict and the proof in
	// flight, if any, guarded by isoMu; probeUpWait, probeEndWait and
	// probeKubeletWait are probeUpTimeout, probeEndTimeout and
	// probeKubeletTimeout.
	isoMu            sync.Mutex
	isoLast          IsolationVerdict
	isoProof         *isolationProof
	probeUpWait      time.Duration
	probeEndWait     time.Duration
	probeKubeletWait time.Duration
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
	if cfg.IsolationTTL <= 0 {
		cfg.IsolationTTL = defaults.IsolationTTL
	}
	cfg.NodeID = strings.TrimSpace(cfg.NodeID)
	return &Runner{
		cfg: cfg, kube: kube, sink: sink, library: library, tokens: tokens,
		log:              slog.Default().With("component", "pipelines.runner"),
		now:              time.Now,
		tempDir:          os.TempDir(),
		openCapture:      NewCapture,
		drainTimeout:     followDrainTimeout,
		libraryTimeout:   libraryPhaseTimeout,
		tailsTimeout:     tailsTimeout,
		inflight:         map[*inflight]struct{}{},
		claims:           map[string]*step{},
		statusTrouble:    apiTrouble{},
		probeUpWait:      probeUpTimeout,
		probeEndWait:     probeEndTimeout,
		probeKubeletWait: probeKubeletTimeout,
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
		r: r, run: run, jobName: jobName, ctx: ctx, trouble: apiTrouble{},
		// The ids, never the StepRun: its secrets would print.
		log: r.log.With("runId", run.RunID, "workRunId", run.WorkRunID, "stepKey", run.StepKey,
			"attempt", run.Attempt, "jobName", jobName, "node", r.cfg.NodeID),
	}
	defer s.discardCapture()
	defer r.dropClaim(jobName, s)
	return s.execute()
}

// Status says where a step's Job stands: absent, finished with its outcome,
// running under a fresh claim, or stale. A step this replica is running
// before its Job exists -- waiting for a slot under the ceiling -- is running.
// Running and stale name the node whose claim is on the Job, when one is: the
// agent forwards a stale step again away from it. Whenever the Job exists the
// reply says when it was created, which is where the step's own timeout runs
// from (ruling R31b); a running step with no creation time is one waiting for
// its Job, which the agent waits on until the run's ceiling.
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
		// replica. The agent asks after every step it waits on every
		// statusPollInterval, so a persistent error is logged as a loop's is.
		r.mu.Lock()
		due := r.statusTrouble.due(r.now(), "reading a step's Job for its status", err)
		r.mu.Unlock()
		if due {
			r.log.Warn("pipelines: reading a step's Job for its status", "jobName", req.JobName, "error", err)
		}
		return StatusReply{State: StateStale}
	}
	var created string
	if at := job.Metadata.CreationTimestamp; !at.IsZero() {
		created = at.UTC().Format(time.RFC3339)
	}
	if out, ok := persisted(job); ok {
		return StatusReply{State: StateFinished, Result: &out, JobCreatedAt: created}
	}
	node, at, claimed := holder(job)
	if claimed && r.fresh(at) {
		return StatusReply{State: StateRunning, Runner: node, JobCreatedAt: created}
	}
	return StatusReply{State: StateStale, Runner: node, JobCreatedAt: created}
}

// Ack deletes a step's Job and Secret once the agent has durably journaled the outcome; what
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

// claimHolder is the Run of this replica that holds, or is taking, the Job's
// claim; nil when none does.
func (r *Runner) claimHolder(jobName string) *step {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.claims[jobName]
}

// takeClaim registers s as the Run taking the Job's claim, unless another
// Run of this replica holds it.
func (r *Runner) takeClaim(jobName string, s *step) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h := r.claims[jobName]; h != nil && h != s {
		return false
	}
	r.claims[jobName] = s
	return true
}

// dropClaim forgets s as the Job's holder; another Run's registration stays.
func (r *Runner) dropClaim(jobName string, s *step) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.claims[jobName] == s {
		delete(r.claims, jobName)
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
	// token is the clone token this Run minted last, at tokenAt; the step's
	// Secret holds it once tokenWritten.
	token        string
	tokenAt      time.Time
	tokenWritten bool
	// tokens is every clone token the step may have cloned with, as far as
	// this Run knows: each one it minted, and the one the Secret held once
	// this Run held the claim (tokenRead). All are masked.
	tokens        []string
	tokenRead     bool
	claimFailures int
	// ownedSecret: this Run made the Job its Secret's owner.
	ownedSecret bool
	// claimSent is the claim this Run last sent: the Job carrying exactly it
	// is this Run's own claim, whatever answer was lost. claimAdopts says
	// that claim was sent over another Run's, and claimCursor is the log
	// cursor read before sending it: a claim whose answer was lost is still
	// an adoption, from that cursor (fix round 2, Important B).
	claimSent     string
	claimAdopts   bool
	claimCursor   time.Time
	claimLogFirst time.Time
	// secrets is what the capture masks: every piece the follower cuts a
	// line into is cut outside them. Guarded by mu: the follower reads it.
	secrets []string
	// adopted: this Run took over a Job another Run had claimed; adoptedAt
	// is the cursor it was left at -- the store has every line up to it --
	// and logFirst the step's first line, as the Job records it (zero when
	// it does not).
	adopted   bool
	adoptedAt time.Time
	logFirst  time.Time
	// reattachNoted: the notice that this Run re-attached is written; head
	// is what the archive holds of the step's start, judged when it was.
	reattachNoted bool
	head          archiveHead
	// replayedHead: a line at or before the adoption cursor was replayed into
	// the archive -- the node's log still reached back that far -- and
	// firstReplayed is the first such line's stamp. Written by the follower,
	// read once it has stopped (or by the follower itself, at the seam).
	replayedHead  bool
	firstReplayed time.Time
	// firstStored is the first line this Run stored: the step's first, for
	// a Run that followed it from its start. Guarded by mu: the heartbeat
	// publishes it (AnnotLogFirst).
	firstStored time.Time
	// trouble is what this Run's own loops last logged of the API errors
	// they met; the heartbeat and the follower keep their own.
	trouble apiTrouble

	mu     sync.Mutex
	cursor time.Time // the timestamp of the last line stored, published by the heartbeat
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
			case err != nil && seen:
				// The Job exists, and runs on -- under this Run or another
				// replica's -- whatever the API server answers for now: ask
				// again, as watch and wait do.
				s.trouble.warn(s, "reading the step's Job", err)
				if !s.sleep(s.r.cfg.PollInterval) {
					return s.abandon(nil)
				}
				continue
			case err != nil:
				return s.failed(pl.CodeRunnerUnavailable, "the Kubernetes API server could not be asked for the step's Job: "+apiMessage(err))
			case !found && seen:
				// Deleted between two reads of this Run: a cancel elsewhere.
				return s.vanished(nil)
			case !found:
				if s.run.RecoverOnly {
					return s.failed(pl.CodeExecutionUncertain, "The previous attempt's Job and result are missing. It may already have executed; no replacement Job was created. Reconcile that attempt before authorizing new work.")
				}
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
		verdict := s.judge(job)
		if verdict == claimWait {
			if res, done := s.wait(); done {
				return res
			}
			continue
		}
		res, done := s.own(job, verdict == claimMine)
		if done {
			return res
		}
	}
}

// claimVerdict is what a Run does about a Job's claim.
type claimVerdict int

const (
	// claimTake: unclaimed, a claim gone stale, or one this replica's
	// previous incarnation left -- claim it.
	claimTake claimVerdict = iota
	// claimMine: the claim this Run itself sent, whose answer was lost.
	claimMine
	// claimWait: another Run holds it, on this replica or another -- wait
	// for its outcome.
	claimWait
)

// judge reads a Job's claim (review finding 3). A claim stamped by another
// node is that node's while fresh. One stamped by THIS node means what this
// replica's registry says: this Run's own (an answer lost on the way back),
// another live Run's here -- waited on however old its stamp, since that Run
// still lives and only its heartbeats are failing -- or, with no Run here
// holding it, a previous incarnation's, which is adopted at once.
func (s *step) judge(job Job) claimVerdict {
	h := s.r.claimHolder(s.jobName)
	if h != nil && h != s {
		return claimWait
	}
	node, at, claimed := holder(job)
	switch {
	case !claimed:
		return claimTake
	case node == s.r.cfg.NodeID && h == s && job.Metadata.Annotations[AnnotRunner] == s.claimSent:
		return claimMine
	case node == s.r.cfg.NodeID:
		return claimTake
	case s.r.fresh(at):
		return claimWait
	}
	return claimTake
}

// create creates the step's Secret and then its Job, refusing first what
// cannot run, and holding the step until this replica has proved
// memql-pipelines isolated (isolation.go). The Job it answers may be one
// another Run created first; the caller reads it like any existing one.
//
// Everything before the Job exists -- the proof, the wait for a free slot
// under the ceiling -- is bounded by the step's run's ceiling alone, never by
// its own timeout, which runs from the Job's creation (ruling R31b).
func (s *step) create() (res pl.StepResult, job Job, done bool) {
	spec, err := BuildJob(s.r.cfg, s.run, s.jobName)
	if err != nil {
		var refusal *pl.Refusal
		if errors.As(err, &refusal) {
			return s.refused(refusal.Code, refusal.Detail), Job{}, true
		}
		return s.failed(pl.CodeJobRejected, err.Error()), Job{}, true
	}
	runDeadline, ok := s.runDeadline()
	if !ok {
		return s.failed(pl.CodeExecutorError, fmt.Sprintf(
			"the step names no run deadline this runner can read (%q), so its wait for a free slot under the pipelines "+
				"ceiling would have no bound, and a step with no bound is not started; nothing was created",
			s.run.RunDeadline)), Job{}, true
	}
	// Before anything exists for the step: a refused step leaves no token,
	// no Secret and no Job.
	if res, done := s.isolationGate(runDeadline); done {
		return res, Job{}, true
	}

	token, err := s.mintToken()
	switch {
	case err != nil && s.ctx.Err() != nil:
		return s.abandon(nil), Job{}, true
	case err != nil:
		// Nothing exists yet, and without a token nothing should.
		return s.failed(pl.CodeCloneFailed, "the clone token could not be minted: "+err.Error()), Job{}, true
	}
	s.token, s.tokenAt = token, s.r.now()
	s.maskToken(token)
	if err := s.ensureCapture(); err != nil {
		return s.failed(pl.CodeRunnerUnavailable, "this workbench node cannot open the step's log archive: "+err.Error()), Job{}, true
	}

	secret := BuildSecret(s.r.cfg, s.run, s.jobName, token)
	var existed bool
	if err := s.retryAPI(func() (err error) {
		existed, err = s.r.kube.CreateSecret(s.ctx, secret)
		return err
	}); err != nil {
		if s.ctx.Err() != nil {
			return s.abandon(nil), Job{}, true
		}
		code, why := createFailure("Secret", err)
		return s.failed(code, why), Job{}, true
	}
	// A Secret an earlier Run of the step left holds that Run's token, of
	// any age: freshenToken writes this Run's over it.
	s.tokenWritten = !existed

	own := time.Duration(s.run.TimeoutSeconds) * time.Second
	waiting, attempts := false, 0
	for {
		left := runDeadline.Sub(s.r.now())
		if left < time.Second {
			// No Job deadline can say less than a second. The token must not
			// outlive the Job that will not come.
			s.deleteSecret()
			return s.ceilingPassed(runDeadline, "waiting for a free slot under the pipelines ceiling, or for a runner to take it"), Job{}, true
		}
		// The step's own timeout runs from its Job's creation, however long
		// it waited for this slot, and never past its run's ceiling: the Job
		// is given all of it, or what is left of the run, in whole seconds,
		// when that is less (deadlineCode names which).
		spec.Spec.ActiveDeadlineSeconds = ptr(int64(min(own, left) / time.Second))
		s.freshenToken()
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
			// as the step is wanted and its run's ceiling allows, and say so
			// once. The API server answered, so the failures that may pass
			// start counting again.
			attempts = 0
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
// status since the create answered. mine says the claim on the Job is the one
// this Run sent, whose answer was lost: it is held already. done is false
// when the claim went to another runner -- one that claimed the Job first, or
// adopted it from under this one -- and the Run should read the Job again.
func (s *step) own(job Job, mine bool) (pl.StepResult, bool) {
	if !s.r.takeClaim(s.jobName, s) {
		// Another Run of this replica took it first: wait for its outcome.
		return s.wait()
	}
	if err := s.ensureCapture(); err != nil {
		return s.failed(pl.CodeRunnerUnavailable, "this workbench node cannot open the step's log archive: "+err.Error()), true
	}
	claimed := job
	if !mine {
		var err error
		_, _, s.claimAdopts = holder(job)
		s.claimCursor, s.claimLogFirst = logCursor(job), logFirst(job)
		s.claimSent = s.r.stamp()
		claimed, err = s.r.kube.AnnotateJob(s.ctx, s.jobName, map[string]string{AnnotRunner: s.claimSent}, job.Metadata.ResourceVersion)
		switch {
		case err == nil:
		case s.ctx.Err() != nil:
			return s.abandon(nil), true
		case deploycontrol.IsConflict(err):
			// The Job changed since it was read: another runner's claim, or
			// only its status. Read it again and decide again.
			s.r.dropClaim(s.jobName, s)
			s.log.Info("pipelines: the step's Job changed before it could be claimed; reading it again")
			return pl.StepResult{}, false
		case deploycontrol.IsNotFound(err):
			return s.vanished(nil), true
		default:
			// The claim may have been applied with its answer lost: this Run
			// stays registered, so reading the Job again tells (judge).
			s.claimFailures++
			if s.claimFailures >= apiAttempts || !transient(err) {
				return s.failed(pl.CodeRunnerUnavailable, "the step's Job could not be claimed: "+apiMessage(err)), true
			}
			if !s.sleep(s.r.cfg.PollInterval) {
				return s.abandon(nil), true
			}
			return pl.StepResult{}, false
		}
	}

	hb := s.startHeartbeat()
	defer hb.stop()
	s.ownSecret(claimed)
	s.readToken()
	if s.claimAdopts {
		cursor := s.claimCursor
		s.publishCursor(cursor)
		s.adopted, s.adoptedAt, s.logFirst = true, cursor, s.claimLogFirst
		if cursor.IsZero() {
			// Nothing was captured before: there is no seam to wait for.
			s.noteReattach()
		}
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
				s.trouble.warn(s, "reading the step's Job", err)
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
				s.trouble.warn(s, "reading the step's pod", err)
			}
			continue
		}
		if pod != nil && !podOfJob(pod, job) {
			pod = nil // a leftover of an earlier Job of this name: no pod yet
		}
		if pod != nil {
			last = pod
		}
		obs := Classify(job, pod, created, s.r.now(), s.r.cfg, s.deadlineCode(job))
		switch obs.Phase {
		case PhaseRunning:
			if f == nil {
				f = s.follow(pod.Metadata.Name, false)
			}
		case PhaseSucceeded, PhaseFailed:
			// The first terminal observation is the step's outcome: what
			// decided it can be gone the next time anyone looks.
			dec := s.decision(obs, pod, job)
			s.record(dec)
			return s.settle(dec, pod, f), true
		}
	}
}

// yield gives the step up to the runner that holds it now.
func (s *step) yield() {
	s.log.Info("pipelines: another runner holds the step's Job now; waiting for its outcome")
	s.r.dropClaim(s.jobName, s)
	s.discardCapture()
}

// wait waits on a Job another Run holds, here or on another replica, for its
// outcome. done is false once the Job is no longer another Run's to wait on
// (judge): the Run reads the Job again, and adopts it.
func (s *step) wait() (pl.StepResult, bool) {
	s.r.dropClaim(s.jobName, s)
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
				s.trouble.warn(s, "reading the step's Job", err)
			}
			continue
		}
		if out, ok := persisted(job); ok {
			return out, true
		}
		if s.judge(job) != claimWait {
			return pl.StepResult{}, false
		}
	}
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

// ceilingPassed is the step's failure when its run reached its ceiling
// before its Job could be created (ruling R31b), while it was doing what: it
// never started.
func (s *step) ceilingPassed(runDeadline time.Time, what string) pl.StepResult {
	return s.failed(pl.CodeRunCeiling, fmt.Sprintf(
		"the run reached its ceiling at %s before the step's Job could be created -- %s -- so the step was never started",
		runDeadline.UTC().Format(time.RFC3339), what))
}

// deadlineCode names the bound the step's Job's deadline is (ruling R31b):
// the step's own timeout -- or its run's ceiling, when the StepRun says the
// ceiling bound the timeout at the hand-over, or when the Job was given less
// than the timeout because less of the run was left at its creation. Read off
// the Job, so every replica that adopts it names the same bound.
func (s *step) deadlineCode(job Job) string {
	if ads := job.Spec.ActiveDeadlineSeconds; ads != nil && *ads < int64(s.run.TimeoutSeconds) {
		return pl.CodeRunCeiling
	}
	return cmp.Or(strings.TrimSpace(s.run.DeadlineCode), pl.CodeStepTimeout)
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
	// The one list the capture masks and the follower keeps its cuts out
	// of (secretValues, the fleet path's too).
	secrets := s.setSecrets()
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
//
// What an ownership of the step learned goes with its capture (fix round 3,
// item 5): a Run that takes the step again -- after yielding it, or waiting
// on another replica -- adopts it afresh, from the cursor the Job carries
// then, with its own seam notice and its own judgement of the head.
func (s *step) discardCapture() {
	if s.capture == nil {
		return
	}
	_, _ = s.capture.Close()
	s.removeArchive()
	s.capture = nil
	s.adopted, s.adoptedAt, s.logFirst = false, time.Time{}, time.Time{}
	s.reattachNoted, s.head = false, headWhole
	s.replayedHead, s.firstReplayed = false, time.Time{}
	s.mu.Lock()
	s.cursor = time.Time{}
	s.mu.Unlock()
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

func (s *step) mintToken() (string, error) {
	if s.r.tokens == nil {
		return "", errors.New("this workbench node has no token minter")
	}
	return s.r.tokens.CloneToken(s.ctx, s.run.InstallationID, s.run.Repository.Owner, s.run.Repository.Name)
}

// runDeadline is when the step's run reaches its ceiling
// (StepRun.RunDeadline, ruling R31b): what bounds every wait before the
// step's Job exists. ok is false when the StepRun names none this runner can
// read.
func (s *step) runDeadline() (time.Time, bool) {
	at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(s.run.RunDeadline))
	return at, err == nil
}

// setSecrets makes the step's secret list the run's secrets and every token
// in tokens, and returns it.
func (s *step) setSecrets() []string {
	secrets := secretValues(s.run, s.tokens...)
	s.mu.Lock()
	s.secrets = secrets
	s.mu.Unlock()
	return secrets
}

// maskSecrets is what the capture masks now.
func (s *step) maskSecrets() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.secrets
}

// maskToken masks a clone token from now on: in the capture open now, and in
// every one this Run opens after it.
func (s *step) maskToken(token string) {
	if token == "" || slices.Contains(s.tokens, token) {
		return
	}
	s.tokens = append(s.tokens, token)
	s.setSecrets()
	if s.capture != nil {
		s.capture.AddSecrets(token)
	}
}

// freshenToken sees, before each try at creating the Job, that the step's
// Secret holds a clone token young enough to clone with (fix round 1, minor
// 7): a token is minted before a wait under the ceiling that can outlast it,
// and a Secret an earlier Run of the step left holds that Run's token, of any
// age. One older than tokenRefreshAge is minted again, and the Secret's token
// key is rewritten whenever it does not hold this Run's latest. A failure is
// logged and tried again before the next try: the token the Secret holds may
// serve yet.
func (s *step) freshenToken() {
	if s.token == "" {
		return // an anonymous clone
	}
	if s.r.now().Sub(s.tokenAt) >= tokenRefreshAge {
		token, err := s.mintToken()
		switch {
		case err != nil:
			if s.ctx.Err() == nil {
				s.trouble.warn(s, "minting a fresh clone token for the step", err)
			}
		case token != "":
			s.maskToken(token)
			s.token, s.tokenAt, s.tokenWritten = token, s.r.now(), false
		}
	}
	if s.tokenWritten {
		return
	}
	err := s.retryAPI(func() error { return s.r.kube.SetCloneToken(s.ctx, SecretName(s.jobName), s.token) })
	switch {
	case err == nil:
		s.tokenWritten = true
	case s.ctx.Err() == nil:
		s.trouble.warn(s, "writing the clone token into the step's Secret", err)
	}
}

// readToken masks the clone token the step's Secret holds, read once this Run
// holds the claim and before anything the clone printed -- its tail, a
// failure's message -- is masked (fix round 1, minor 8). An adopter minted no
// token, and the Secret holds the one the clone ran with whoever wrote it:
// the Run that created the Job, an earlier Run whose Secret was reused, or
// one that raced this Run under the ceiling and freshened it last. A token is
// written only before a try at creating the Job, so the Secret's settles once
// the Job exists. Read once; a Secret that is gone, or holds no token, leaves
// the mask without one.
func (s *step) readToken() {
	if s.tokenRead {
		return
	}
	var token string
	err := s.retryAPI(func() (err error) {
		token, err = s.r.kube.SecretCloneToken(s.ctx, SecretName(s.jobName))
		return err
	})
	switch {
	case err == nil:
		s.tokenRead = true
		s.maskToken(token)
	case deploycontrol.IsNotFound(err):
		s.tokenRead = true
	case s.ctx.Err() == nil:
		s.log.Warn("pipelines: the clone token in the step's Secret could not be read, so what the clone printed is masked without it", "error", err)
	}
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

// apiTrouble is what a polling loop last logged of each call it makes, so an
// API error that persists is logged when what it says changes, and again
// every apiTroubleRepeat while it lasts -- not once a poll interval (fix
// round 1, minor 12). Each goroutine that polls keeps its own.
type apiTrouble map[string]troubleLogged

type troubleLogged struct {
	err string
	at  time.Time
}

// warn logs err, met making the call what, unless it is what was logged for
// that call last, less than apiTroubleRepeat ago.
func (t apiTrouble) warn(s *step, what string, err error) {
	if t.due(s.r.now(), what, err) {
		s.log.Warn("pipelines: "+what, "error", err)
	}
}

// due says err, met making the call what at now, is to be logged -- it is not
// what was logged for that call last, less than apiTroubleRepeat ago -- and
// records it when it is. What is compared is troubleKey's: the same failure
// met asking after two steps' Jobs is one failure (fix round 3).
func (t apiTrouble) due(now time.Time, what string, err error) bool {
	said := troubleKey(err)
	if last, ok := t[what]; ok && last.err == said && now.Sub(last.at) < apiTroubleRepeat {
		return false
	}
	t[what] = troubleLogged{err: said, at: now}
	return true
}

// troubleKey is what an API error says about the API server, not about what
// was asked of it: a refusal's status code and reason, or the transport's own
// error -- never the request, whose path names the object (a Job).
func troubleKey(err error) string {
	var se *deploycontrol.StatusError
	if errors.As(err, &se) {
		var status struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal([]byte(se.Body), &status)
		return strconv.Itoa(se.Code) + " " + status.Reason
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	// Anything else -- a body that is not a Job, say -- is keyed by its
	// innermost error, which says what went wrong without the wrapping that
	// names the Job.
	root := err
	for next := errors.Unwrap(root); next != nil; next = errors.Unwrap(root) {
		root = next
	}
	return root.Error()
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
		trouble := apiTrouble{}
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if !s.beat(ctx, trouble) {
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
func (s *step) beat(ctx context.Context, trouble apiTrouble) bool {
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
		// The step's first line, once, by a Run that followed it from its
		// start: an adopter's first line is the cursor's successor.
		if first := s.getFirstStored(); !s.claimAdopts && !first.IsZero() && job.Metadata.Annotations[AnnotLogFirst] == "" {
			annots[AnnotLogFirst] = first.UTC().Format(time.RFC3339Nano)
		}
		_, err = s.r.kube.AnnotateJob(ctx, s.jobName, annots, job.Metadata.ResourceVersion)
		if !deploycontrol.IsConflict(err) {
			if err != nil && ctx.Err() == nil {
				trouble.warn(s, "the step's claim could not be stamped", err)
			}
			return true
		}
		// The Job changed between the read and the stamp -- its status, or
		// a claim: read it again.
	}
	return true
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
func logCursor(job Job) time.Time { return annotTime(job, AnnotLogCursor) }

// logFirst is the step's first line as the Job records it, or zero.
func logFirst(job Job) time.Time { return annotTime(job, AnnotLogFirst) }

func annotTime(job Job, key string) time.Time {
	at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(job.Metadata.Annotations[key]))
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

// archiveHead is what an adopted step's archive holds of the step's start
// (ruling R36, fix round 2). The kubelet serves only the container's current
// log file, so once the node has rotated it, what an adopter replays begins
// wherever that file does -- and the notices say only what is so.
type archiveHead int

const (
	// headWhole: the archive begins at the step's first line -- a step this
	// Run followed from its start, or one whose first replayed line is the
	// first line the Job records (AnnotLogFirst).
	headWhole archiveHead = iota
	// headHeld: replayed as far back as the node's log still held the
	// output; the Job records no first line to tell whether that is the start.
	headHeld
	// headMissing: the first line replayed is not the step's first, which
	// the Job records: the head may be missing.
	headMissing
	// headLost: nothing at or before the cursor was replayed: the archive
	// starts where the node's log does, after the cursor.
	headLost
	// headUncaptured: no line had been captured before this Run, so all of
	// the output is followed -- as far back as the node's log held it.
	headUncaptured
)

// judgeHead is what the archive holds of the step's start, once the lines at
// or before the adoption cursor have been replayed (or none was).
func (s *step) judgeHead() archiveHead {
	switch {
	case !s.adopted:
		return headWhole
	case s.adoptedAt.IsZero():
		return headUncaptured
	case !s.replayedHead:
		return headLost
	case s.logFirst.IsZero():
		return headHeld
	case s.firstReplayed.Equal(s.logFirst):
		return headWhole
	}
	return headMissing
}

// noteReattach writes, once, the notice that this Run re-attached to a step
// another Run had held: to the store, where the lines after the cursor follow
// it, and to the archive, at the seam between the lines replayed from the
// node's log and the ones after the cursor (ruling R36). It says what the
// archive holds of the step's start, which is judged here.
func (s *step) noteReattach() {
	if !s.adopted || s.reattachNoted || s.capture == nil {
		return
	}
	s.reattachNoted, s.head = true, s.judgeHead()
	s.capture.Notice(s.reattachNotice())
}

func (s *step) reattachNotice() string {
	at := func(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
	lead := "memql: re-attached on " + s.r.cfg.NodeID
	stored := "lines before " + at(s.adoptedAt) + " are in the store already"
	switch s.head {
	case headUncaptured:
		return lead + "; no line of the step's output had been captured, so this archive holds what the node's log still held of it"
	case headLost:
		return lead + "; " + stored + ", but the node's log no longer holds them, so this archive starts where the node's log does"
	case headWhole:
		return lead + ": earlier lines replayed from the node's log, from the step's first line; " + stored
	case headMissing:
		return lead + ": earlier lines replayed from the node's log, as far back as it still held the step's output -- from " +
			at(s.firstReplayed) + ", after the step's first line at " + at(s.logFirst) + ", so the head may be missing; " + stored
	}
	return lead + ": earlier lines replayed from the node's log, as far back as it still held the step's output; " + stored
}

// noteFirstStored records the first line this Run stored.
func (s *step) noteFirstStored(at time.Time) {
	s.mu.Lock()
	if s.firstStored.IsZero() {
		s.firstStored = at
	}
	s.mu.Unlock()
}

func (s *step) getFirstStored() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.firstStored
}
