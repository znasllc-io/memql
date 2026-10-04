package pipelinesteps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	nodev1 "github.com/znasllc-io/memql/component/node/gen"
	pl "github.com/znasllc-io/memql/component/pipelines"
	"github.com/znasllc-io/memql/integrations/workbench"
)

// executor_cluster.go -- a step with no need, run as a Kubernetes Job by a
// workbench replica (epic memql#5478, #5493; Review Focus 2).
//
// The agent forwards the StepRun to any healthy workbench replica, whose
// runner creates the step's Job -- or finds it: every name the runner uses is
// derived from (run, step, attempt), so whichever replica is asked finds the
// same Job. The forward is WATCHED: a replica that stops being one this node
// would send to ends the wait with no cancel, and the agent forwards again,
// unpinned, so another replica adopts the still-running Job. Beside it, the
// step's status is read every statusEvery: a heartbeat gone stale is the
// replica gone while the mesh still thinks it healthy, and a finished status
// is the outcome whose reply a stream flap lost -- neither of which the
// watched forward can see -- and a Job still absent long after the forward
// is one whose request may never have reached a runner, so it is forwarded
// again too (the runner finds or creates the Job by its name). A stale
// re-forward steers AWAY from the replica whose runner went quiet. The
// outcome, however it arrives, is acked so the Job and its Secret go now
// rather than at their TTL.
//
// Nothing waits forever. Past the step's effective deadline and lostGrace,
// one last status decides how the step is reported, and its Job is deleted.

// forwardEnd is how one step forward ended.
type forwardEnd struct {
	resp     *nodev1.WorkbenchForwardResponse
	servedBy string
	err      error
}

// clusterStep is one forwarded step in progress.
type clusterStep struct {
	e       *Executor
	run     StepRun
	args    []byte
	job     string
	timeout time.Duration

	// pending is the live step forward, nil while a re-forward is waiting to
	// go. A forward replaced by another is ABANDONED, never cancelled:
	// cancelling is how the router stops waiting AND how the runner is told
	// to delete the Job, which is the work the replacement exists to keep.
	pending      chan forwardEnd
	pendingSince time.Time
	// reached says some replica answered something about this step.
	reached bool
	// absentForwards counts the forwards an absent Job has caused; each
	// waits twice as long as the one before (absentBackoffMax doublings).
	absentForwards int
	// noPeerSince is when an unbroken run of "no workbench replica" began.
	noPeerSince time.Time
	// servedBy is the last replica known to have served the step.
	servedBy string
}

func (e *Executor) runCluster(ctx context.Context, run StepRun, timeout time.Duration) pl.StepResult {
	job := JobName(run.RunID, run.StepKey, run.Attempt)
	if e.fwd == nil {
		return withWhere(failedResult(pl.CodeRunnerUnavailable,
			"This agent node has no route to a workbench replica (MEMQL_WORKBENCH_REMOTE is not set, or the "+
				"workbench integration is missing), so a cluster step cannot be handed to a runner. Nothing ran."),
			pl.Where{Surface: surfaceCluster, JobName: job})
	}
	// Every forward of the step carries the moment it was handed over, which
	// the runner counts the step's timeout from, as giveUp below does (ruling
	// R31): time spent before the Job exists is the step's.
	run.HandedAt = e.now().UTC().Format(time.RFC3339Nano)
	args, err := json.Marshal(run)
	if err != nil {
		return withWhere(failedResult(pl.CodeExecutorError, "The step could not be encoded for the workbench: "+err.Error()),
			pl.Where{Surface: surfaceCluster, JobName: job})
	}
	s := &clusterStep{e: e, run: run, args: args, job: job, timeout: timeout}
	s.forward(ctx, "")

	status := time.NewTicker(e.statusEvery)
	defer status.Stop()
	giveUp := time.NewTimer(timeout + e.lostGrace)
	defer giveUp.Stop()
	var retry <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return s.where(stoppedResult(ctx, run.DeadlineCode))
		case end := <-s.pending:
			s.pending = nil
			if res, done := s.ended(ctx, end); done {
				return res
			}
			if res, done := s.noPeerTooLong(); done {
				return res
			}
			delay := e.noPeerWait
			if errors.Is(end.err, workbench.ErrWorkbenchPeerLost) {
				delay = 0
			}
			retry = time.After(delay)
		case <-retry:
			retry = nil
			s.forward(ctx, "")
		case <-status.C:
			if res, done := s.poll(ctx); done {
				return res
			}
		case <-giveUp.C:
			return s.giveUp(ctx)
		}
	}
}

// forward sends the step, unpinned -- any healthy replica may create or
// adopt the Job -- under a fresh request id, away from exclude while another
// healthy replica exists, and makes it the live forward.
func (s *clusterStep) forward(ctx context.Context, exclude string) {
	ch := make(chan forwardEnd, 1)
	s.pending, s.pendingSince = ch, s.e.now()
	req, err := s.e.request(workbench.PipelineStepAction, s.run.RunID, s.run.StepKey, s.args, s.run.TimeoutSeconds)
	if err != nil {
		ch <- forwardEnd{err: err}
		return
	}
	go func() {
		resp, servedBy, err := s.e.fwd.ForwardWatchedExcluding(ctx, req, "", exclude, s.e.peerWatch)
		ch <- forwardEnd{resp: resp, servedBy: servedBy, err: err}
	}()
}

// ended reads how the live forward ended: the step's outcome, or a reason to
// forward again.
func (s *clusterStep) ended(ctx context.Context, end forwardEnd) (pl.StepResult, bool) {
	log := s.e.logger.With(slog.String("runId", s.run.RunID), slog.String("stepKey", s.run.StepKey),
		slog.String("jobName", s.job), slog.String("nodeId", end.servedBy))
	switch {
	case end.err == nil:
		s.reached, s.noPeerSince = true, time.Time{}
		if end.servedBy != "" {
			s.servedBy = end.servedBy
		}
		return s.answer(ctx, end.resp, end.servedBy), true
	case ctx.Err() != nil:
		return s.where(stoppedResult(ctx, s.run.DeadlineCode)), true
	case errors.Is(end.err, workbench.ErrWorkbenchPeerLost):
		s.reached, s.noPeerSince = true, time.Time{}
		log.Warn("pipelines: the workbench replica running a step went away; forwarding it again so another replica adopts its Job")
	case errors.Is(end.err, workbench.ErrNoWorkbenchPeer):
		if s.noPeerSince.IsZero() {
			// Said once when it begins, not on every retry.
			s.noPeerSince = s.e.now()
			log.Warn("pipelines: no workbench replica is reachable for a step; trying again until one is")
		}
	default:
		log.Warn("pipelines: forwarding a step failed; trying again", slog.String("error", end.err.Error()))
	}
	return pl.StepResult{}, false
}

// noPeerTooLong fails a step that no replica has ever taken once no replica
// has been reachable for noPeerPatience. A step some replica already took
// waits on to its deadline instead: its Job may be running, and a replica
// coming back adopts it.
func (s *clusterStep) noPeerTooLong() (pl.StepResult, bool) {
	if s.reached || s.noPeerSince.IsZero() || s.e.now().Sub(s.noPeerSince) < s.e.noPeerPatience {
		return pl.StepResult{}, false
	}
	return s.where(failedResult(pl.CodeRunnerUnavailable, fmt.Sprintf(
		"No workbench replica has been reachable for %s, so the step was never handed to a runner. Nothing ran.",
		s.e.noPeerPatience))), true
}

// answer reads a replica's reply to the step forward.
func (s *clusterStep) answer(ctx context.Context, resp *nodev1.WorkbenchForwardResponse, servedBy string) pl.StepResult {
	if code := resp.GetErrorCode(); code != "" {
		if code == workbench.ErrCodePipelinesNotConfigured {
			return s.where(failedResult(pl.CodeRunnerUnavailable, fmt.Sprintf(
				"The workbench replica %s has no pipeline runner, so it could not run the step (%s). Nothing ran.",
				servedBy, resp.GetErrorMessage())))
		}
		return s.where(failedResult(pl.CodeExecutorError, fmt.Sprintf(
			"The workbench replica %s refused the step (%s): %s", servedBy, code, resp.GetErrorMessage())))
	}
	res, err := decodeOutcome(resp.GetPayloadJson())
	if err != nil {
		return s.where(failedResult(pl.CodeExecutorError, fmt.Sprintf(
			"The workbench replica %s answered something that is not a step's outcome: %v", servedBy, err)))
	}
	s.e.ack(ctx, s.run, s.job)
	return s.fill(res, servedBy)
}

// poll reads the step's status from any replica -- every replica answers
// from the Job -- and acts on it: a finished step's outcome is taken; a stale
// heartbeat forwards the step again, away from the replica that went quiet;
// a Job still absent long after the forward forwards it again too.
func (s *clusterStep) poll(ctx context.Context) (pl.StepResult, bool) {
	reply, servedBy, ok := s.e.status(ctx, s.run, s.job)
	if !ok {
		return pl.StepResult{}, false
	}
	s.reached = true
	switch reply.State {
	case StateFinished:
		res, ok := statusOutcome(reply)
		if !ok {
			return pl.StepResult{}, false
		}
		// The reply on the forward was lost. The forward is abandoned, not
		// cancelled: its replica's runner answers a cancel that arrives after
		// the outcome is persisted with nothing, and the step is done.
		s.e.logger.Info("pipelines: took a step's outcome from its status; the reply to its forward was lost",
			slog.String("runId", s.run.RunID), slog.String("stepKey", s.run.StepKey), slog.String("jobName", s.job))
		s.e.ack(ctx, s.run, s.job)
		return s.fill(res, servedBy), true
	case StateStale:
		if s.pending == nil || s.e.now().Sub(s.pendingSince) < s.e.stalePatience {
			// A re-forward is already on its way, or the last one may still
			// be adopting the Job (accepted as ruling R29b).
			return pl.StepResult{}, false
		}
		s.e.logger.Warn("pipelines: the runner holding a step's Job went quiet; forwarding the step again so another replica adopts it",
			slog.String("runId", s.run.RunID), slog.String("stepKey", s.run.StepKey), slog.String("jobName", s.job),
			slog.String("quietOn", reply.Runner))
		s.forward(ctx, reply.Runner)
	case StateAbsent:
		// No Job long after the forward: the request may never have reached
		// a runner (lost to a stream drop on the way). Forwarding again is
		// safe -- a runner finds or creates the Job by its name -- but a Job
		// is most often absent because its runner is waiting for a free slot
		// under the ceiling, which another forward cannot speed up, so each
		// forward an absence causes waits twice as long as the one before.
		wait := s.e.stalePatience << min(s.absentForwards, absentBackoffMax)
		if s.pending == nil || s.e.now().Sub(s.pendingSince) < wait {
			return pl.StepResult{}, false
		}
		s.absentForwards++
		s.e.logger.Warn("pipelines: no runner has made a step's Job since it was forwarded; forwarding it again",
			slog.String("runId", s.run.RunID), slog.String("stepKey", s.run.StepKey), slog.String("jobName", s.job),
			slog.Int("forwards", s.absentForwards))
		s.forward(ctx, "")
	}
	return pl.StepResult{}, false
}

// giveUp ends a step nothing reported within its deadline and the grace
// past it. One last status decides how it reads; whatever it says, the Job is
// deleted, because nothing will report it now.
func (s *clusterStep) giveUp(ctx context.Context) pl.StepResult {
	reply, servedBy, ok := s.e.status(ctx, s.run, s.job)
	if ok && reply.State == StateFinished {
		if res, done := statusOutcome(reply); done {
			s.e.ack(ctx, s.run, s.job)
			return s.fill(res, servedBy)
		}
	}
	s.e.ack(ctx, s.run, s.job)
	limit := time.Duration(s.run.TimeoutSeconds) * time.Second
	switch {
	case ok && reply.State == StateRunning:
		return s.where(failedResult(s.run.DeadlineCode, fmt.Sprintf(
			"The step was still running %s after it was handed to the cluster, past its %s deadline: it waited for "+
				"a free slot under the pipelines ceiling before it started. It was stopped.", s.timeout+s.e.lostGrace, limit)))
	case ok && reply.State == StateAbsent:
		return s.where(failedResult(s.run.DeadlineCode, fmt.Sprintf(
			"The step did not start within its %s deadline: the cluster had no free slot for it under the pipelines "+
				"ceiling, or its request never reached a runner.", limit)))
	}
	return s.where(failedResult(pl.CodeNodeLost, fmt.Sprintf(
		"No workbench replica reported the step within its %s deadline and the %s past it: the replica running "+
			"it went away and no other took its Job over.", limit, s.e.lostGrace)))
}

// fill completes a runner's outcome with what the executor knows of where it
// ran, never overwriting what the runner said.
func (s *clusterStep) fill(res pl.StepResult, servedBy string) pl.StepResult {
	if res.Where.Surface == "" {
		res.Where.Surface = surfaceCluster
	}
	if res.Where.NodeID == "" {
		res.Where.NodeID = servedBy
	}
	if res.Where.JobName == "" {
		res.Where.JobName = s.job
	}
	return res
}

// where places an outcome the executor wrote itself.
func (s *clusterStep) where(res pl.StepResult) pl.StepResult {
	return withWhere(res, pl.Where{Surface: surfaceCluster, NodeID: s.servedBy, JobName: s.job})
}

// surfaceCluster is pl.Where.Surface for a step run as a Job.
const surfaceCluster = "cluster"

func withWhere(res pl.StepResult, where pl.Where) pl.StepResult {
	res.Where = where
	return res
}

// status reads a step's status from any replica. ok is false when no replica
// answered with a readable status.
func (e *Executor) status(ctx context.Context, run StepRun, job string) (StatusReply, string, bool) {
	args, err := json.Marshal(StatusRequest{JobName: job})
	if err != nil {
		return StatusReply{}, "", false
	}
	req, err := e.request(workbench.PipelineStatusAction, run.RunID, run.StepKey, args, 0)
	if err != nil {
		return StatusReply{}, "", false
	}
	callCtx, done := context.WithTimeout(ctx, e.callTimeout)
	defer done()
	resp, servedBy, err := e.fwd.Forward(callCtx, req, "")
	if err != nil || resp.GetErrorCode() != "" {
		e.logger.Debug("pipelines: a step's status went unanswered",
			slog.String("runId", run.RunID), slog.String("stepKey", run.StepKey),
			slog.String("errorCode", resp.GetErrorCode()), slog.Any("error", err))
		return StatusReply{}, servedBy, false
	}
	var reply StatusReply
	if err := json.Unmarshal(resp.GetPayloadJson(), &reply); err != nil {
		return StatusReply{}, servedBy, false
	}
	return reply, servedBy, true
}

// ack tells a replica the agent holds the step's outcome, so the Job and its
// Secret are deleted now; fire and forget, because the Job's TTL is the
// backstop. It outlives the step's context: it is sent as the step returns.
func (e *Executor) ack(ctx context.Context, run StepRun, job string) {
	args, err := json.Marshal(AckRequest{JobName: job})
	if err != nil {
		return
	}
	req, err := e.request(workbench.PipelineAckAction, run.RunID, run.StepKey, args, 0)
	if err != nil {
		return
	}
	go func() {
		ackCtx, done := context.WithTimeout(context.WithoutCancel(ctx), e.callTimeout)
		defer done()
		resp, servedBy, err := e.fwd.Forward(ackCtx, req, "")
		if err != nil || resp.GetErrorCode() != "" {
			e.logger.Warn("pipelines: a step's outcome was not acked; its Job and Secret go at their TTL instead",
				slog.String("runId", run.RunID), slog.String("stepKey", run.StepKey), slog.String("jobName", job),
				slog.String("nodeId", servedBy), slog.String("errorCode", resp.GetErrorCode()), slog.Any("error", err))
		}
	}()
}

// decodeOutcome reads a runner's reply as a step's outcome.
func decodeOutcome(payload []byte) (pl.StepResult, error) {
	if len(bytes.TrimSpace(payload)) == 0 {
		return pl.StepResult{}, errors.New("the reply is empty")
	}
	var res pl.StepResult
	if err := json.Unmarshal(payload, &res); err != nil {
		return pl.StepResult{}, err
	}
	if !isOutcome(res.Status) {
		return pl.StepResult{}, fmt.Errorf("its status %q is not one an outcome has", res.Status)
	}
	return res, nil
}

// statusOutcome is a finished status's outcome, when it carries one.
func statusOutcome(reply StatusReply) (pl.StepResult, bool) {
	if reply.Result == nil || !isOutcome(reply.Result.Status) {
		return pl.StepResult{}, false
	}
	return *reply.Result, true
}

func isOutcome(o pl.Outcome) bool {
	switch o {
	case pl.OutcomeSucceeded, pl.OutcomeFailed, pl.OutcomeCancelled, pl.OutcomeRefused:
		return true
	}
	return false
}
