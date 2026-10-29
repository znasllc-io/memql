package pipelinesteps

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/node"
	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/workbench"
)

// executor.go -- the one pl.Executor the seam's driver calls, on the AGENT
// node, for every step of every pipeline run (epic memql#5478; #5493, #5494,
// #5496).
//
// Dedicated cluster steps go to a workbench replica, whose runner creates or
// adopts their Kubernetes Job (cluster.go). Dedicated Fleet steps use the
// owner's opted-in machines (fleet.go). An explicit pool step tries Fleet
// first; only confirmed pre-start capacity refusal can reach cloud fallback,
// under its captured installed policy and the runner's live spending gate.
// In every case the executor decides first what the step may be
// given: it refuses what it must not route, and it bounds the step by the
// lesser of its own timeout and what is left of its run's wall-clock ceiling,
// and names the moment that ceiling is reached (StepRun.RunDeadline). A
// step may wait for host or cluster capacity within its original run ceiling.
// A Fleet preference deadline never cancels a started command or permits a
// second copy after an uncertain reply. A cluster command's own timeout runs
// from its Job's creation (ruling R31b).
//
// ATTEMPTS (spec 2026-10-28-pipeline-fleet-first-retries-failover-design.md).
// The driver retries a failed step; the executor makes each attempt safe to
// retry, in two ways that are deliberately its own and not the driver's.
//
//   - It CLASSIFIES every attempt that did not succeed (attempt_class.go,
//     attempt_class_fleet.go): did the command not start, ask to be run again,
//     leave its outcome unknown, or fail for good? Only the executor knows
//     whether a dispatch reached a machine and what the machine or Job
//     answered, and the driver retries from the class alone.
//   - It takes the attempt's PLACE literally (req.Placement). A step's first
//     attempt has none and goes where its manifest placed it, with a pool
//     step's fleet-then-cloud preference inside that one attempt. A later
//     attempt arrives with its place chosen by the driver from the journal,
//     and nothing inside it widens that: a fleet attempt never spills to the
//     cluster, a cluster attempt never asks a machine, and neither reaches a
//     place the manifest does not allow. The driver, not this file, decides
//     what the next place is, because only the driver has the history.

// Forwarder is the agent's road to a workbench replica: integrations/
// workbench's ForwardRouter on an agent node, a fake mesh in a test. A step is
// forwarded WATCHED, steering away from excludeNodeId while another healthy
// replica exists; status, ack and cancel are plain forwards. SelfNodeId and
// SelfNodeType stamp the assertion's origin.
type Forwarder interface {
	ForwardWatchedExcluding(ctx context.Context, req *nodev1.WorkbenchForwardRequest, pinnedNodeId, excludeNodeId string, interval time.Duration, onSelected func(string)) (*nodev1.WorkbenchForwardResponse, string, error)
	Forward(ctx context.Context, req *nodev1.WorkbenchForwardRequest, pinnedNodeId string) (*nodev1.WorkbenchForwardResponse, string, error)
	SelfNodeId() string
	SelfNodeType() string
}

// FleetRouter runs a Fleet or pool step on one of its owner's opted-in machines.
type FleetRouter interface {
	RunStep(ctx context.Context, req pl.StepRequest, run StepRun) (pl.StepResult, error)
}

// The executor's clocks. Every one is a field on the Executor, so a test can
// shrink it; these are the values a node runs with.
const (
	// statusPollInterval is how often a forwarded step's status is read.
	// LOAD-BEARING: a stream flap is invisible to the watched forward -- the
	// request rode the reconnecting outbox and the reply on the old attempt
	// is lost -- so this poll is the only way such a step's outcome is seen.
	statusPollInterval = 30 * time.Second
	// peerWatchInterval is how often the watched forward re-checks the
	// replica it is waiting on: the mesh's heartbeat interval.
	peerWatchInterval = 5 * time.Second
	// nodeLostGrace is how long past the bound it waits for -- the deadline
	// of a step's Job (its timeout from its creation, never past its run's
	// ceiling), or for a step whose Job does not exist yet, its run's ceiling
	// (ruling R31b) -- the executor waits for anything before it gives the
	// step up. The Job's own deadline ends it at its bound; the grace covers
	// settling it, every phase at its deadline (settleBudget, the runner's),
	// and settleMargin past that -- derived, so the two cannot drift apart
	// and the agent never acks away a Job whose outcome is still being
	// recorded.
	nodeLostGrace = settleBudget + settleMargin
	// settleMargin is the grace past settleBudget: the poll that sees the
	// step decided, the Job controller marking its deadline, and the reply's
	// way back to the agent.
	settleMargin = time.Minute
	// noPeerRetry is how soon a step is forwarded again when no workbench
	// replica could be reached at all.
	noPeerRetry = 2 * time.Second
	// noPeerPatience is how long a step that no replica has taken yet waits
	// for one to appear -- a rolling deploy of the workbench -- before it
	// fails pipeline_runner_unavailable.
	noPeerPatience = 2 * time.Minute
	// workbenchCallTimeout bounds one status, ack or cancel forward: the
	// workbench answers each at once, from the Job.
	workbenchCallTimeout = 10 * time.Second
	// stalePatience is how long after a forward a stale heartbeat is taken
	// as the replica holding the Job having gone, and an absent Job as the
	// forward never having reached a runner. Shorter would forward again
	// while the replica just forwarded to is still adopting the Job --
	// proving the pipelines namespace is isolated first can take two and a
	// half minutes.
	stalePatience = 3 * time.Minute
	// absentBackoffMax caps the doublings of stalePatience for a Job that
	// stays absent: one most often absent because its runner is waiting for
	// a free slot under the ceiling, which another forward cannot speed up.
	absentBackoffMax = 3
	// cancelAttempts bounds the run-wide delete's retries.
	cancelAttempts = 5
	// ackAttempts bounds how many times one step's ack is sent (ack).
	ackAttempts = 5
)

// errRunCancelled is the cause Cancel ends a step's context with, so the
// step reports the run's cancel rather than a caller that stopped waiting.
var errRunCancelled = errors.New("pipelinesteps: the run was cancelled")

// Executor is the agent node's pl.Executor.
type Executor struct {
	cfg    Config
	fwd    Forwarder
	fleet  FleetRouter
	logger *slog.Logger

	// now is the executor's clock: the run's ceiling is measured on it, and
	// so is every patience between forwards. Timers stay real; what a test
	// controls through it is when a patience has run out.
	now func() time.Time

	statusEvery    time.Duration
	peerWatch      time.Duration
	lostGrace      time.Duration
	noPeerWait     time.Duration
	noPeerPatience time.Duration
	callTimeout    time.Duration
	stalePatience  time.Duration
	// onAcked, when set, is told how each step's ack ended: how many sends
	// it took, and why the last went unconfirmed when none was confirmed.
	onAcked func(job string, attempts int, err error)

	mu sync.Mutex
	// inflight is this node's Execute calls in flight, by pipelines run id,
	// so Cancel reaches every step of a run at once.
	inflight map[string]map[*inflightStep]struct{}
}

// inflightStep is one Execute call's cancel, held by pointer so a release
// removes its own entry and no other.
type inflightStep struct {
	cancel context.CancelCauseFunc
}

// NewExecutor builds the agent's executor. fwd is nil on an agent with no
// route to a workbench replica, and fleet nil on one with no dispatcher: a
// step that needs the missing half then fails pipeline_runner_unavailable,
// saying which half.
func NewExecutor(cfg Config, fwd Forwarder, fleet FleetRouter, logger *slog.Logger) *Executor {
	if logger == nil {
		logger = slog.Default()
	}
	if cfg.RunCeiling <= 0 {
		// Never unbounded: a zero ceiling is a Config nobody filled in.
		cfg.RunCeiling = pl.DefaultRunCeiling
	}
	if cfg.DefaultStepTimeout <= 0 {
		cfg.DefaultStepTimeout = pl.DefaultStepTimeout
	}
	return &Executor{
		cfg:            cfg,
		fwd:            fwd,
		fleet:          fleet,
		logger:         logger,
		now:            time.Now,
		statusEvery:    statusPollInterval,
		peerWatch:      peerWatchInterval,
		lostGrace:      nodeLostGrace,
		noPeerWait:     noPeerRetry,
		noPeerPatience: noPeerPatience,
		callTimeout:    workbenchCallTimeout,
		stalePatience:  stalePatience,
		inflight:       map[string]map[*inflightStep]struct{}{},
	}
}

var _ pl.Executor = (*Executor)(nil)

// Execute runs one step and answers how it ended. Every ending is a typed
// StepResult: an error is returned only when the fleet path could not report
// an outcome at all.
//
// EVERY ENDING THAT IS NOT A SUCCESS CARRIES A CLASS (AttemptClass), including
// the ones this method makes itself before any surface is asked: a refusal, a
// run past its ceiling, a node with no route. The fleet path classifies its
// own answers (Fleet.RunStep), because only it holds the dispatcher's word; a
// result nothing classified reads by the shared rules (clusterAttemptClass),
// which is what keeps the driver's invariant -- one class on every failure --
// from depending on which FleetRouter is installed. An error return carries
// none: the driver fails that step pipeline_executor_error and never retries
// it, which is what "final" means.
func (e *Executor) Execute(ctx context.Context, req pl.StepRequest) (pl.StepResult, error) {
	res, err := e.execute(ctx, req)
	if err != nil {
		return res, err
	}
	if res.Status != pl.OutcomeSucceeded && res.AttemptClass == "" {
		res.AttemptClass = clusterAttemptClass(res)
	}
	return res, nil
}

// execute routes one attempt to the surface its place names, and runs it.
func (e *Executor) execute(ctx context.Context, req pl.StepRequest) (pl.StepResult, error) {
	if res, refused := refuseStep(req); refused {
		return res, nil
	}
	timeout, deadlineCode, runDeadline, ceiling := e.deadlineFor(req, e.now())
	if ceiling != nil {
		return *ceiling, nil
	}
	run := stepRunFor(req, int(timeout/time.Second), deadlineCode)
	run.RunDeadline = runDeadline.UTC().Format(time.RFC3339Nano)

	stepCtx, release := e.track(ctx, req.RunID)
	defer release()

	switch routeOf(req) {
	case routePool:
		return e.runPool(stepCtx, req, run, timeout, runDeadline)
	case routeFleet:
		return e.runFleet(stepCtx, req, run)
	}
	return e.runClusterAttempt(stepCtx, run, timeout, runDeadline), nil
}

// route is the place an attempt goes, once its placement and its step are read.
type route int

const (
	// routeCluster: a Job on a workbench replica.
	routeCluster route = iota
	// routeFleet: one of the owner's machines, and nowhere else.
	routeFleet
	// routePool: a pool step's own placement -- the fleet first, and the cluster
	// only after a confirmed pre-start capacity refusal, all inside the one
	// attempt. Only an attempt with no placement of its own reads this way.
	routePool
)

// routeOf is where an attempt goes. A placement that names a surface wins and
// is taken literally (refusePlacement has already refused one the step may not
// have); otherwise the step's own placement decides, which is how every step's
// first attempt goes. The order of the step's own checks -- pool before fleet --
// is the order Execute has always had.
func routeOf(req pl.StepRequest) route {
	if p := req.Placement; p != nil {
		switch p.Surface {
		case pl.PlacementFleet:
			return routeFleet
		case pl.PlacementCluster:
			return routeCluster
		}
	}
	switch {
	case req.Step.Placement == pl.PlacementPool:
		return routePool
	case req.Step.RequiresFleet():
		return routeFleet
	}
	return routeCluster
}

// runFleet runs an attempt on the owner's machines. A fleet attempt cannot be
// recovered: a machine holds no Job to re-attach to, so a previous driver that
// may have started the command and left no result is an unknown outcome
// (pipeline_execution_uncertain), and nothing is sent again. Whether the step
// may run once more is the driver's question, answered from the manifest's
// `repeatable`, not this method's.
func (e *Executor) runFleet(ctx context.Context, req pl.StepRequest, run StepRun) (pl.StepResult, error) {
	if req.RecoverOnly {
		return failedResult(pl.CodeExecutionUncertain, "A previous driver may have started this fleet command, and no durable result was recorded. Reconcile its outcome before authorizing new work; no replacement command was sent."), nil
	}
	if e.fleet == nil {
		what := "The attempt was placed on one of the owner's machines"
		if len(req.Step.Needs) > 0 {
			what = fmt.Sprintf("The step needs %s, which only one of the owner's machines can meet", strings.Join(req.Step.Needs, ", "))
		}
		// Made by the agent before any machine was asked: nothing ran (noRunnerReached).
		return noRunnerReached(what + ", and this agent node has no fleet dispatcher to reach one. Nothing ran."), nil
	}
	// The fleet classifies what it was answered (Fleet.RunStep): it alone holds
	// the dispatcher's word on whether anything started.
	return e.fleet.RunStep(ctx, req, run)
}

// runClusterAttempt runs an attempt as a Job and classifies the answer here.
// The class is the AGENT's reading, never a runner's: every outcome a runner
// reported arrives bare, because the two places one enters drop the class fields
// (fromARunner), so a reply from a replica of another version, or one that put a
// class of its own on the wire, cannot make an ordinary failure retryable. A
// failure the agent made itself before any runner took the step arrives already
// classified (noRunnerReached, whose doc lists the sites and says why the class is
// decided where it is made); what is still bare is read from its result
// (clusterAttemptClass).
func (e *Executor) runClusterAttempt(ctx context.Context, run StepRun, timeout time.Duration, runDeadline time.Time) pl.StepResult {
	res := e.runCluster(ctx, run, timeout, runDeadline)
	if res.AttemptClass == "" {
		res.AttemptClass = clusterAttemptClass(res)
	}
	return res
}

// refuseStep is what the executor will not route, decided before anything
// is: a step that is not a command (a notify step is the driver's), a need
// outside the closed set (the compiler should have refused it; the executor
// never routes on a name it does not know), and a need on a pipeline whose
// owner did not consent to the fleet.
func refuseStep(req pl.StepRequest) (pl.StepResult, bool) {
	if err := pl.CheckCPUMilli(req.Step.CPUMilli, req.Step.Execution == pl.ExecutionNative); err != nil {
		return refusedResult(pl.CodeStepInvalid, err.Error()), true
	}
	if err := pl.CheckMemoryMiB(req.Step.MemoryMiB, req.Step.Execution == pl.ExecutionNative); err != nil {
		return refusedResult(pl.CodeStepInvalid, err.Error()), true
	}
	if req.Step.ImagePullSecret != "" && req.Step.RequiresFleet() {
		return refusedResult(pl.CodeStepInvalid, "Image pull credentials require cluster execution."), true
	}
	if req.Step.Kind != pl.StepCommand {
		return refusedResult(pl.CodeExecutorError, fmt.Sprintf(
			"The executor runs command steps, and %q is a %q step, which the driver delivers itself. Nothing ran.",
			req.StepKey, req.Step.Kind)), true
	}
	if err := pl.CheckExecution(req.Step.Execution, req.Step.Platform, req.Step.RequiresFleet()); err != nil {
		return refusedResult(pl.CodeStepInvalid, err.Error()), true
	}
	for _, need := range req.Step.Needs {
		if !pl.IsNeed(need) {
			return refusedResult(pl.CodeNeedUnknown, fmt.Sprintf(
				"The step needs %q, which is not one of the needs a step may name (%s), so it was routed nowhere.",
				need, strings.Join(pl.Needs(), ", "))), true
		}
	}
	if err := pl.CheckExecutionNeeds(req.Step.Execution, req.Step.Needs); err != nil {
		return refusedResult(pl.CodeStepInvalid, err.Error()), true
	}
	if (req.Step.RequiresFleet() || req.Step.Placement == pl.PlacementPool) && req.Compute != pl.ComputeClusterAndFleet {
		return refusedResult(pl.CodeFleetNotConsented, fmt.Sprintf(
			"The step needs %s, which only one of the owner's machines can meet, and the pipeline does not declare "+
				"compute: %s. Nothing was sent to a machine.", strings.Join(req.Step.Needs, ", "), pl.ComputeClusterAndFleet)), true
	}
	if req.Step.Placement == pl.PlacementPool {
		if err := pl.CheckPoolStep(req.Step); err != nil {
			return refusedResult(pl.CodeStepInvalid, err.Error()), true
		}
		if req.ComputePlacement == nil || req.ComputePlacement.FleetWaitSeconds < 0 || req.ComputePlacement.FleetWaitSeconds > 3600 {
			return refusedResult(pl.CodeStepInvalid, "Pool placement requires a captured installation compute policy with a bounded Fleet preference."), true
		}
	}
	// Last, because it judges the placement against everything checked above:
	// the step's own placement, and the consent a fleet or pool step needs.
	return refusePlacement(req)
}

// maxPlacementWaitSeconds bounds an attempt's MaxWaitSeconds at the same hour a
// pool step's fleetWaitSeconds may not pass (above), because the driver passes
// that value here for a pool step's fleet failover. The run's ceiling bounds
// every wait regardless.
const maxPlacementWaitSeconds = 3600

// refusePlacement is the executor's check of WHERE the driver asked one attempt
// to go. The driver chose the place from the journal; the executor does not
// take its word for what the step is allowed, because the places differ in what
// they cost and what they may touch. A placement is refused, with nothing sent
// anywhere, when:
//
//   - it names a surface the executor does not know. Reading it as "the step's
//     own" would send a step somewhere nobody chose;
//   - it puts a plain cluster step on the owner's machines. The step's manifest
//     placement is the limit (spec D4: a cluster step retries on the cluster
//     only), and the pipeline that holds it may declare compute: cluster, whose
//     owner never agreed to a machine: the checks above ask for that consent
//     (compute: cluster_and_fleet) only of a step that needs a machine or is a
//     pool step;
//   - it puts a step that needs a machine on the cluster, which has none to give;
//   - it puts a pool step on the cluster when the installation's captured policy
//     does not allow cloud fallback. Rule 3 of spec section 5 only names the
//     cluster when it does; the cluster is the one place an attempt costs money,
//     so the executor asks again (spec D7: nothing can spend cloud the policy did
//     not allow);
//   - it carries a pin, exclusions or a wait for an attempt that is not going to
//     the fleet, where they would be silently ignored;
//   - its wait is negative or past an hour.
//
// A refusal is final: the same placement would be refused again, and it is the
// driver's mistake, not a transient. The class is set by Execute like any other.
func refusePlacement(req pl.StepRequest) (pl.StepResult, bool) {
	p := req.Placement
	if p == nil {
		return pl.StepResult{}, false
	}
	refuse := func(why string) (pl.StepResult, bool) {
		return refusedResult(pl.CodeStepInvalid, "The attempt's placement was refused: "+why+" Nothing ran."), true
	}
	switch p.Surface {
	case "", pl.PlacementFleet, pl.PlacementCluster:
	default:
		return refuse(fmt.Sprintf("%q is not a surface an attempt may be placed on (fleet or cluster).", p.Surface))
	}
	if p.MaxWaitSeconds < 0 || p.MaxWaitSeconds > maxPlacementWaitSeconds {
		return refuse(fmt.Sprintf("a wait of %d seconds is outside 0 to %d.", p.MaxWaitSeconds, maxPlacementWaitSeconds))
	}
	step := req.Step
	pool := step.Placement == pl.PlacementPool
	switch {
	case p.Surface == pl.PlacementFleet && !step.RequiresFleet() && !pool:
		return refuse("the step is a cluster step, which runs on the cluster only; its manifest placement does not offer it the owner's machines.")
	case p.Surface == pl.PlacementCluster && step.RequiresFleet():
		return refuse("the step needs the owner's machines (fleet placement, native execution or host needs), which the cluster cannot give it.")
	case p.Surface == pl.PlacementCluster && pool && (req.ComputePlacement == nil || !req.ComputePlacement.CloudFallback):
		return refuse("the installation's compute policy does not allow a pool step to spend cloud (cloudFallback is off).")
	case routeOf(req) != routeFleet && (p.PinWorker != "" || len(p.ExcludeWorkers) > 0 || p.MaxWaitSeconds != 0):
		return refuse("it names machines (a pin, exclusions or a wait), and the attempt is not going to the owner's machines.")
	}
	return pl.StepResult{}, false
}

// runPool changes location only after a positive pre-start refusal. A lost
// reply, cancellation or failed command is not spare-capacity evidence: trying
// the cloud then could execute the same command twice. Recovery may adopt the
// stable cluster job identity but can never replace an uncertain Fleet effect.
//
// This is a pool step's FIRST attempt, the one with no placement of its own:
// the Codex fleet-first behaviour, kept whole inside one attempt. An attempt
// the driver placed never comes here (routeOf), because it goes exactly where it
// says; in particular a fleet attempt must not do this method's fall to the
// cluster, since the driver is the one deciding, from the journal and the
// policy, when the cluster is the next place.
func (e *Executor) runPool(ctx context.Context, req pl.StepRequest, run StepRun, timeout time.Duration, runDeadline time.Time) (pl.StepResult, error) {
	if req.RecoverOnly {
		return e.runClusterAttempt(ctx, run, timeout, runDeadline), nil
	}
	if e.fleet == nil {
		// Made by the agent before any machine or Job was asked: nothing ran (noRunnerReached).
		return noRunnerReached("Pool placement requires a configured Fleet dispatcher. Nothing ran."), nil
	}
	res, err := e.fleet.RunStep(ctx, req, run)
	if err != nil || ctx.Err() != nil || !req.ComputePlacement.CloudFallback || res.Status != pl.OutcomeRefused || res.Failure == nil || res.Failure.Code != pl.CodeNoMachineForNeed {
		return res, err
	}
	// Queue waiting does not consume the command's execution timeout, but it
	// cannot extend the original run deadline. The receiving workbench also
	// checks fresh cloud admission before each new Job effect.
	timeout, deadlineCode, runDeadline, ceiling := e.deadlineFor(req, e.now())
	if ceiling != nil {
		return *ceiling, nil
	}
	run.TimeoutSeconds = int(timeout / time.Second)
	run.DeadlineCode = deadlineCode
	run.RunDeadline = runDeadline.UTC().Format(time.RFC3339Nano)
	return e.runClusterAttempt(ctx, run, timeout, runDeadline), nil
}

// deadlineFor is the step's effective timeout -- the lesser of its own
// timeout and what is left of its run's ceiling, in whole seconds -- the code
// naming whichever bound it is, and the moment the run reaches its ceiling. A
// run past its ceiling, or with less than a second of it left, answers the
// failure instead: no Job deadline can say less than a second.
func (e *Executor) deadlineFor(req pl.StepRequest, now time.Time) (time.Duration, string, time.Time, *pl.StepResult) {
	started, err := time.Parse(time.RFC3339, strings.TrimSpace(req.RunStartedAt))
	if err != nil {
		res := failedResult(pl.CodeExecutorError, fmt.Sprintf(
			"The run's start time (%q) could not be read, so the step's share of the run's %s ceiling could not be "+
				"computed, and a step with no bound is not started. Nothing ran.", req.RunStartedAt, e.cfg.RunCeiling))
		return 0, "", time.Time{}, &res
	}
	runDeadline := started.Add(e.cfg.RunCeiling)
	remaining := runDeadline.Sub(now)
	if remaining < time.Second {
		res := failedResult(pl.CodeRunCeiling, fmt.Sprintf(
			"The run has used its %s ceiling (MEMQL_PIPELINES_RUN_MAX_MINUTES), so this step was not started.",
			e.cfg.RunCeiling))
		return 0, "", time.Time{}, &res
	}
	own := e.cfg.DefaultStepTimeout
	if req.Step.TimeoutSeconds > 0 {
		own = time.Duration(req.Step.TimeoutSeconds) * time.Second
	}
	if own <= remaining {
		return own.Truncate(time.Second), pl.CodeStepTimeout, runDeadline, nil
	}
	return remaining.Truncate(time.Second), pl.CodeRunCeiling, runDeadline, nil
}

// Cancel stops every step of a run: this node's Execute calls in flight end
// at once -- a forwarded step's forward is cancelled, which the replica
// running it turns into deleting its Job; a fleet step's dispatch is
// cancelled, which the dispatcher turns into ToolCancel -- and a workbench
// replica is asked to delete the run's Jobs by label, which reaches the Jobs
// other replicas hold and the ones no forward names any more. Safe for a run
// with nothing in flight.
func (e *Executor) Cancel(ctx context.Context, runID string) error {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return errors.New("pipelinesteps: a cancel names the run it stops; the run's Jobs are deleted by its label")
	}
	stopped := e.cancelInflight(runID)
	if e.fwd == nil {
		e.logger.Info("pipelines: cancelled the run's steps in flight here; this node has no route to a workbench, so no Job was deleted from it",
			slog.String("runId", runID), slog.Int("steps", stopped))
		return nil
	}
	return e.deleteRunJobs(ctx, runID)
}

// deleteRunJobs forwards pipelineCancel until a replica confirms it. It is
// retried because a stream that drops mid-answer loses the replica's answer;
// deleting by label is idempotent, so a retry that repeats a delete costs
// nothing.
func (e *Executor) deleteRunJobs(ctx context.Context, runID string) error {
	args, err := json.Marshal(CancelRequest{RunID: runID})
	if err != nil {
		return fmt.Errorf("pipelinesteps: encoding the run's cancel: %w", err)
	}
	var last error
	for attempt := 1; attempt <= cancelAttempts; attempt++ {
		req, err := e.request(workbench.PipelineCancelAction, runID, "", args, 0)
		if err != nil {
			return err
		}
		callCtx, done := context.WithTimeout(ctx, e.callTimeout)
		resp, servedBy, err := e.fwd.Forward(callCtx, req, "")
		done()
		switch {
		case err == nil && resp.GetErrorCode() == "":
			e.logger.Info("pipelines: a workbench replica deleted the cancelled run's Jobs",
				slog.String("runId", runID), slog.String("nodeId", servedBy), slog.String("reply", string(resp.GetPayloadJson())))
			return nil
		case err == nil:
			last = fmt.Errorf("workbench replica %s refused it (%s): %s", servedBy, resp.GetErrorCode(), resp.GetErrorMessage())
		default:
			last = err
		}
		if attempt == cancelAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("pipelinesteps: no workbench replica confirmed deleting run %s's Jobs before the cancel's time ran out; "+
				"they end at their deadline and go at their TTL: %w", runID, last)
		case <-time.After(e.noPeerWait):
		}
	}
	return fmt.Errorf("pipelinesteps: no workbench replica confirmed deleting run %s's Jobs after %d attempts; "+
		"they end at their deadline and go at their TTL: %w", runID, cancelAttempts, last)
}

// track registers one Execute call under its run, and returns the context it
// runs under with the release that forgets it. The release also ends the
// context, which is how a forward the step abandoned is finally cancelled:
// by then the step's outcome is persisted, and a cancel reaching a runner
// whose outcome is persisted is a no-op.
func (e *Executor) track(parent context.Context, runID string) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(parent)
	step := &inflightStep{cancel: cancel}
	e.mu.Lock()
	set := e.inflight[runID]
	if set == nil {
		set = map[*inflightStep]struct{}{}
		e.inflight[runID] = set
	}
	set[step] = struct{}{}
	e.mu.Unlock()
	return ctx, func() {
		e.mu.Lock()
		if set := e.inflight[runID]; set != nil {
			delete(set, step)
			if len(set) == 0 {
				delete(e.inflight, runID)
			}
		}
		e.mu.Unlock()
		cancel(nil)
	}
}

// cancelInflight ends every in-flight Execute of a run with errRunCancelled
// and answers how many there were.
func (e *Executor) cancelInflight(runID string) int {
	e.mu.Lock()
	steps := make([]*inflightStep, 0, len(e.inflight[runID]))
	for s := range e.inflight[runID] {
		steps = append(steps, s)
	}
	e.mu.Unlock()
	for _, s := range steps {
		s.cancel(errRunCancelled)
	}
	return len(steps)
}

// inflightCount is how many Execute calls of a run are in flight here.
func (e *Executor) inflightCount(runID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.inflight[runID])
}

// request is one pipeline forward, under the engine's own SYSTEM-class
// assertion -- the only class the workbench answers the four pipeline actions
// under, and one only the engine can mint -- with a FRESH request id: the
// router fills only an empty one, and a re-forward that reused an id would
// collide in the router's in-flight table and in the handler's.
func (e *Executor) request(action, runID, stepKey string, args []byte, timeoutSeconds int) (*nodev1.WorkbenchForwardRequest, error) {
	self := strings.TrimSpace(e.fwd.SelfNodeId())
	authority, err := auth.ForwardedAuthorityForSystem("pipelines-executor:"+cmp.Or(self, "unnamed"), time.Now())
	if err != nil {
		return nil, fmt.Errorf("pipelinesteps: asserting the engine's authority for the workbench hop: %w", err)
	}
	return &nodev1.WorkbenchForwardRequest{
		RequestId:  id.NewShortId(),
		RunId:      runID,
		StepId:     stepKey,
		Action:     action,
		ArgsJson:   args,
		TimeoutSec: int32(min(max(timeoutSeconds, 0), math.MaxInt32)),
		Authority:  node.ForwardedAuthorityToProto(authority, self, e.fwd.SelfNodeType()),
	}, nil
}

// failedResult is a step that failed without its command reporting.
func failedResult(code, message string) pl.StepResult {
	return pl.StepResult{Status: pl.OutcomeFailed, ExitCode: -1, Failure: &pl.Failure{Code: code, Message: message}}
}

// refusedResult is a step the executor would not start.
func refusedResult(code, message string) pl.StepResult {
	return pl.StepResult{Status: pl.OutcomeRefused, ExitCode: -1, Failure: &pl.Failure{Code: code, Message: message}}
}

// cancelledResult is a step stopped before it reported.
func cancelledResult(message string) pl.StepResult {
	return pl.StepResult{Status: pl.OutcomeCancelled, ExitCode: -1,
		Failure: &pl.Failure{Code: pl.CodeStepCancelled, Message: message}}
}

// The sentences a step stopped by its context reports, one per cause.
const (
	runCancelledMessage   = "The run was cancelled while the step ran; the step was stopped."
	driverDeadlineMessage = "The driver stopped waiting for the step past its run's ceiling and the grace after it; the step was stopped."
	driveEndedMessage     = "The step was stopped before it reported: the drive running it ended. It was cancelled."
)

// stoppedResult is how a step whose context ended reports: the run's cancel,
// the driver's own deadline, or the drive ending around it. The driver's
// deadline is its run's ceiling and the grace past it (ruling R31b), so a
// step it ends fails pipeline_run_ceiling, whichever bound its own timeout
// had.
func stoppedResult(ctx context.Context) pl.StepResult {
	switch {
	case errors.Is(context.Cause(ctx), errRunCancelled):
		return cancelledResult(runCancelledMessage)
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return failedResult(pl.CodeRunCeiling, driverDeadlineMessage)
	}
	return cancelledResult(driveEndedMessage)
}
