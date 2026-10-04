package pipelinesteps

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/znasllc-io/memql/component/deploycontrol"
	pl "github.com/znasllc-io/memql/component/pipelines"
)

// isolation.go -- the runner proves memql-pipelines is isolated before it
// creates a step (epic memql#5478, #5493; Task 6b, rulings R12, R42-R44).
//
// The namespace's NetworkPolicy (deploy/k8s/components/pipelines) admits
// nothing in and lets a step out only to the cluster's DNS and the public
// internet. A NetworkPolicy is enforced by the cluster's policy engine, not by
// the object: on an AKS cluster as provisioned there is none, the policy is
// accepted and changes nothing, and a step could reach the node's metadata
// endpoint (the kubelet's identity), the mesh's in-cluster-only /metrics and
// the database. So the runner does not take the object's word for it: before
// it creates a step, it measures, with the probe jobspec.go builds. A pod in
// the namespace tries to reach another one's listener, which the policy
// forbids, and the cluster's DNS, which the policy allows -- the reachable
// positive, without which a failure to connect proves nothing. The same code
// runs everywhere; k3s enforces NetworkPolicy, so a local cluster passes.
//
//   - A pass is trusted for cfg.IsolationTTL (an hour), then proved again. A
//     proof that did not pass is never trusted: the next create proves again.
//   - The steps created on this replica while a proof runs all wait on that
//     one proof -- one probe, one slot under the ceiling -- each for as long
//     as its own timeout lasts (ruling R31). A proof every waiting step gave
//     up on is stopped, and decides nothing.
//   - Only the create path waits on it. Adopting a Job another Run created
//     starts nothing new in the namespace, and a replica that cannot prove
//     isolation must still settle the steps another replica started.
//   - A step the proof does not let through is refused
//     pl.CodeIsolationUnenforced, with nothing of its own made: no token, no
//     Secret, no Job.
//
// The proof, in order: delete whatever this replica's last probe left, since
// the names are this replica's and the same every time; create the probe Job,
// waiting for a slot as a step does; wait (probeUpWait) for index 0's listener
// to be ready -- which it can be before anything else exists, since it
// depends on nothing -- and for index 1's to run; create the probe Secret,
// naming index 0's address and the Job's uid and owned by the Job, which
// starts both connectors; wait (probeEndWait) for index 1's connector to end;
// read the pods again (R42); and judge by the connector's exit code and by
// whether the listener it tried is still ready, the incarnation that was ready
// before the Secret existed. The probe's Job and Secret are deleted before
// anyone is answered, so under a ceiling of one the slot is free for the step.

// IsolationVerdict is what this replica's last proof found (Runner.Isolation).
type IsolationVerdict struct {
	// Isolated: a probe pod reached the cluster's DNS on every attempt and
	// another probe pod's listener, which stayed ready, on none. Only this
	// lets a step be created.
	Isolated bool
	// Inconclusive: the proof could not decide -- the probe could not run,
	// DNS did not answer, the listener did not hold -- which is never a pass.
	// Neither field set: the proof found the namespace open.
	Inconclusive bool
	// Detail is the sentence the refusal and the log carry.
	Detail string
	// At is when it was decided, by the runner's clock; zero before any proof
	// has decided.
	At time.Time
}

// isolationProof is the one proof in flight on a Runner.
type isolationProof struct {
	done   chan struct{}
	cancel context.CancelFunc
	// waiters is how many steps wait on it; guarded by Runner.isoMu.
	waiters int
	// verdict and abandoned are written before done is closed. abandoned:
	// every step that waited left before the proof decided, so it was
	// stopped and its verdict says nothing.
	verdict   IsolationVerdict
	abandoned bool
}

const (
	// probeUpTimeout bounds the probe's pods coming up, images pulled
	// included: index 0's listener ready, index 1's running.
	probeUpTimeout = 90 * time.Second
	// probeEndTimeout bounds the connector, from the probe Secret's creation
	// to its end: its settle and three rounds of two attempts, each bounded
	// at five seconds, come to at most 37 seconds.
	probeEndTimeout = 60 * time.Second
	// probeSaidMaxBytes bounds the connector's own line in a verdict.
	probeSaidMaxBytes = 1 << 10
)

// errProbeStopped is the probe giving up because no step waits on it any
// more.
var errProbeStopped = errors.New("pipelinesteps: the isolation proof was stopped: no step waits on it any more")

// probeFailure is why a probe could not decide: an inconclusive verdict's
// sentence.
type probeFailure string

func (f probeFailure) Error() string { return string(f) }

// Isolation is this replica's last verdict, zero before its first proof has
// decided.
func (r *Runner) Isolation() IsolationVerdict {
	r.isoMu.Lock()
	defer r.isoMu.Unlock()
	return r.isoLast
}

// isolationGate holds a step at its create until this replica has proved
// memql-pipelines isolated, and ends it -- done -- when the proof does not
// let it through, when its timeout runs out first (R31), or when it is
// cancelled.
func (s *step) isolationGate() (res pl.StepResult, done bool) {
	left := s.budgetLeft()
	if left <= 0 {
		// Spent before it reached this runner: nothing to prove for.
		return s.ranOut("waiting for a free slot under the pipelines ceiling, or for a runner to take it"), true
	}
	ctx, cancel := context.WithTimeout(s.ctx, left)
	defer cancel()
	v, err := s.r.proveIsolation(ctx)
	switch {
	case err == nil && v.Isolated:
		return pl.StepResult{}, false
	case s.ctx.Err() != nil:
		return s.abandon(nil), true
	case err != nil:
		return s.ranOut("while this workbench node proved memql-pipelines isolated"), true
	}
	return s.refused(pl.CodeIsolationUnenforced, isolationRefusal(v)), true
}

// isolationRefusal is the sentence a step the proof did not let through is
// refused with: for a namespace found open, the fix; for a proof that could
// not decide, that it is tried again.
func isolationRefusal(v IsolationVerdict) string {
	if v.Inconclusive {
		return "this workbench node could not prove memql-pipelines is network-isolated, and starts no pipeline step until it can: " +
			v.Detail + ". It tries again before the next step."
	}
	return "memql-pipelines is not network-isolated, so this workbench node starts no pipeline step: " + v.Detail +
		". Enable a network policy engine on the cluster so the namespace's NetworkPolicy is enforced (on AKS: " +
		"az aks update --resource-group <group> --name <cluster> --network-policy <engine>); the runner proves it again before the next step."
}

// proveIsolation is this replica's verdict: a pass still fresh, or else the
// one proof in flight -- started here if none is -- once it decides. ctx is
// the waiting step's, bounded by its timeout: when it ends first the step
// stops waiting (its error), and a proof no step waits on any more is
// stopped.
func (r *Runner) proveIsolation(ctx context.Context) (IsolationVerdict, error) {
	for {
		if err := ctx.Err(); err != nil {
			return IsolationVerdict{}, err
		}
		r.isoMu.Lock()
		if v := r.isoLast; v.Isolated {
			// A pass stamped ahead of this clock vouches for nothing.
			if age := r.now().Sub(v.At); age >= 0 && age < r.cfg.IsolationTTL {
				r.isoMu.Unlock()
				return v, nil
			}
		}
		p := r.isoProof
		if p == nil {
			// The proof's own context: the steps that wait on it come and
			// go, and it is stopped only when the last of them has gone.
			pctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
			p = &isolationProof{done: make(chan struct{}), cancel: cancel}
			r.isoProof = p
			go r.runIsolationProof(pctx, p)
		}
		p.waiters++
		r.isoMu.Unlock()

		select {
		case <-p.done:
			if !p.abandoned {
				return p.verdict, nil
			}
			// It stopped as this step joined it, every step before it
			// having given up: prove again.
		case <-ctx.Done():
			r.isoMu.Lock()
			if p.waiters--; p.waiters == 0 {
				p.cancel()
			}
			r.isoMu.Unlock()
			return IsolationVerdict{}, ctx.Err()
		}
	}
}

// runIsolationProof runs one proof to its verdict, or until no step waits on
// it, and records it: Runner.Isolation's answer when it decided.
func (r *Runner) runIsolationProof(ctx context.Context, p *isolationProof) {
	v, decided := r.probeIsolation(ctx)
	p.cancel()
	switch {
	case !decided:
		r.log.Info("pipelines: the isolation proof was stopped: no step waits on it any more")
	case v.Isolated:
		r.log.Info("pipelines: memql-pipelines is network-isolated", "detail", v.Detail)
	case v.Inconclusive:
		r.log.Warn("pipelines: memql-pipelines could not be proved network-isolated; steps are refused until it is", "detail", v.Detail)
	default:
		r.log.Warn("pipelines: memql-pipelines is not network-isolated; steps are refused", "detail", v.Detail)
	}
	r.isoMu.Lock()
	if decided {
		r.isoLast = v
	}
	r.isoProof = nil
	p.verdict, p.abandoned = v, !decided
	r.isoMu.Unlock()
	close(p.done)
}

// prober is one run of the probe.
type prober struct {
	r       *Runner
	ctx     context.Context
	name    string // the probe Job
	target  string // its Secret
	log     *slog.Logger
	trouble apiTrouble
}

// probeListener is index 0's listener as the probe saw it ready: its pod's
// address, and the incarnation that was listening.
type probeListener struct {
	ip        string
	restarts  int32
	startedAt time.Time
}

// probeIsolation runs the probe once, and answers its verdict; decided is
// false when no step waited on it any more before there was one. The probe's
// Job and Secret are gone when it returns, whatever happened.
func (r *Runner) probeIsolation(ctx context.Context) (IsolationVerdict, bool) {
	name := IsolationProbeName(r.cfg.NodeID)
	p := &prober{
		r: r, ctx: ctx, name: name, target: IsolationTargetName(name),
		log: r.log.With("probeJob", name, "node", r.cfg.NodeID), trouble: apiTrouble{},
	}
	// Whatever this replica's last probe left goes first: a probe Secret of
	// its would start the new connector at once, at another pod's address.
	p.remove()
	defer p.remove()

	var (
		job      Job
		listener probeListener
		end      ContainerStateTerminated
		held     bool
		why      string
		err      error
	)
	job, err = p.create()
	if err == nil {
		listener, err = p.up(job)
	}
	if err == nil {
		err = p.arm(job, listener.ip)
	}
	if err == nil {
		end, err = p.end(job)
	}
	if err == nil {
		held, why, err = p.held(job, listener)
	}
	switch {
	case errors.Is(err, errProbeStopped):
		return IsolationVerdict{}, false
	case err != nil:
		return IsolationVerdict{Inconclusive: true, Detail: err.Error(), At: r.now()}, true
	}
	isolated, inconclusive, detail := isolationOutcome(end, held, why)
	return IsolationVerdict{Isolated: isolated, Inconclusive: inconclusive, Detail: detail, At: r.now()}, true
}

// create creates the probe Job, waiting for a slot under the ceiling as a
// step does (R43), and answers it as the API server stored it.
func (p *prober) create() (Job, error) {
	spec := BuildIsolationProbe(p.r.cfg, p.name)
	waiting, attempts, leftovers := false, 0, 0
	for {
		job, created, err := p.r.kube.CreateJob(p.ctx, spec)
		switch {
		case err == nil && created:
			return job, nil
		case p.ctx.Err() != nil:
			return Job{}, errProbeStopped
		case created:
			return Job{}, probeFailure("the probe Job was created, but the API server's answer could not be read: " + err.Error())
		case err == nil, deploycontrol.IsNotFound(err):
			// A probe Job of this name is there still -- the one deleted
			// above, not yet gone -- or went between the conflict and the
			// read after it: delete it, and try again.
			if leftovers++; leftovers >= apiAttempts {
				return Job{}, probeFailure(fmt.Sprintf("an earlier probe Job, %s, was still there after %d deletes", p.name, leftovers))
			}
			p.remove()
			if !sleepCtx(p.ctx, p.r.cfg.PollInterval) {
				return Job{}, errProbeStopped
			}
		case deploycontrol.IsForbiddenQuota(err):
			// The API server answered, so the failures that may pass start
			// counting again.
			attempts = 0
			if !waiting {
				waiting = true
				p.log.Info("pipelines: the isolation probe is waiting for a free slot under the pipelines ceiling")
			}
			if !sleepCtx(p.ctx, quotaWaitPolls*p.r.cfg.PollInterval) {
				return Job{}, errProbeStopped
			}
		case transient(err) && attempts+1 < apiAttempts:
			attempts++
			if !sleepCtx(p.ctx, p.r.cfg.PollInterval) {
				return Job{}, errProbeStopped
			}
		default:
			return Job{}, probeFailure("the probe Job could not be created: " + apiMessage(err))
		}
	}
}

// up waits for the probe's pods: index 0's listener ready, with its pod's
// address, and index 1's listener running -- its image is on its node, so its
// connector starts as soon as the Secret exists.
func (p *prober) up(job Job) (probeListener, error) {
	ctx, cancel := context.WithTimeout(p.ctx, p.r.probeUpWait)
	defer cancel()
	seen := "index 0: no pod yet; index 1: no pod yet"
	for first := true; ; first = false {
		if !first && !sleepCtx(ctx, p.r.cfg.PollInterval) {
			if p.ctx.Err() != nil {
				return probeListener{}, errProbeStopped
			}
			return probeListener{}, probeFailure(fmt.Sprintf("the probe's pods were not up within %v: %s", p.r.probeUpWait, seen))
		}
		pods, err := p.r.kube.JobPods(ctx, p.name)
		if err != nil {
			if ctx.Err() == nil {
				p.warn("reading the isolation probe's pods", err)
				seen = "its pods could not be read: " + apiMessage(err)
			}
			continue
		}
		zero, one := probePods(pods, job)
		if l, ok := listening(zero); ok && probeContainerRuns(one, true, ContainerProbeListener) {
			return l, nil
		}
		seen = "index 0: " + describeProbePod(zero) + "; index 1: " + describeProbePod(one)
	}
}

// arm creates the probe Secret, which the connectors wait for: it starts them.
func (p *prober) arm(job Job, listenerIP string) error {
	secret, err := BuildIsolationTarget(p.r.cfg, job, listenerIP)
	if err != nil {
		return probeFailure(err.Error())
	}
	var existed bool
	err = p.retry(func() (err error) {
		existed, err = p.r.kube.CreateSecret(p.ctx, secret)
		return err
	})
	switch {
	case p.ctx.Err() != nil:
		return errProbeStopped
	case err != nil:
		return probeFailure("the probe Secret could not be created: " + apiMessage(err))
	case existed:
		// Deleted above, so another proof made it since: the connectors may
		// already have read it.
		return probeFailure(fmt.Sprintf("a probe Secret, %s, was already there when this proof made its own", p.target))
	}
	return nil
}

// end waits for index 1's connector to end, and answers how.
func (p *prober) end(job Job) (ContainerStateTerminated, error) {
	ctx, cancel := context.WithTimeout(p.ctx, p.r.probeEndWait)
	defer cancel()
	seen := "it had not started"
	for first := true; ; first = false {
		if !first && !sleepCtx(ctx, p.r.cfg.PollInterval) {
			if p.ctx.Err() != nil {
				return ContainerStateTerminated{}, errProbeStopped
			}
			return ContainerStateTerminated{}, probeFailure(fmt.Sprintf("the probe's connector did not finish within %v: %s", p.r.probeEndWait, seen))
		}
		pods, err := p.r.kube.JobPods(ctx, p.name)
		if err != nil {
			if ctx.Err() == nil {
				p.warn("reading the isolation probe's pods", err)
				seen = "its pod could not be read: " + apiMessage(err)
			}
			continue
		}
		_, one := probePods(pods, job)
		if c := podContainer(one, false, ContainerProbeConnector); c != nil && c.State.Terminated != nil {
			return *c.State.Terminated, nil
		}
		seen = describeConnector(one)
	}
}

// held reads the probe's pods again, after the connector ended (R42), and
// says whether index 0's listener is still the incarnation first seen ready,
// and ready. Then its socket was open from before the connector's first
// attempt -- the connector waited for the Secret, made once the listener was
// ready -- to after its last, so an attempt that reached it would have
// connected. why says how it did not hold.
func (p *prober) held(job Job, was probeListener) (bool, string, error) {
	var pods []Pod
	err := p.retry(func() (err error) {
		pods, err = p.r.kube.JobPods(p.ctx, p.name)
		return err
	})
	switch {
	case p.ctx.Err() != nil:
		return false, "", errProbeStopped
	case err != nil:
		return false, "", probeFailure("the probe's pods could not be read again once the connector had ended: " + apiMessage(err))
	}
	zero, _ := probePods(pods, job)
	l := podContainer(zero, true, ContainerProbeListener)
	switch {
	case zero == nil:
		return false, "the listener's pod was gone", nil
	case l == nil || l.State.Running == nil:
		return false, "the listener was no longer running", nil
	case l.RestartCount != was.restarts || !l.State.Running.StartedAt.Equal(was.startedAt):
		return false, "the listener had restarted since it was seen ready", nil
	case !l.Ready:
		return false, "the listener was not ready", nil
	}
	return true, "", nil
}

// remove deletes the probe Job -- background propagation, so its pods go
// with it -- and its Secret, under a context of its own, so a proof stopped
// by its waiters still cleans up after itself. What is gone is deleted.
func (p *prober) remove() {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(p.ctx), quickCallTimeout)
	defer cancel()
	if err := p.r.kube.DeleteJob(ctx, p.name); err != nil {
		p.log.Warn("pipelines: the isolation probe's Job could not be deleted; its deadline and TTL end it", "error", err)
	}
	if err := p.r.kube.DeleteSecret(ctx, p.target); err != nil {
		p.log.Warn("pipelines: the isolation probe's Secret could not be deleted; it goes with its Job", "error", err)
	}
}

// retry makes a call again while the API server answers what may pass, up to
// apiAttempts times, as a step's calls are made.
func (p *prober) retry(call func() error) error {
	var err error
	for attempt := 1; attempt <= apiAttempts; attempt++ {
		if err = call(); err == nil || !transient(err) || p.ctx.Err() != nil {
			return err
		}
		if !sleepCtx(p.ctx, p.r.cfg.PollInterval) {
			return err
		}
	}
	return err
}

// warn logs an API error a polling loop met, as a step's loops do: when what
// it says changes, and again every apiTroubleRepeat while it lasts.
func (p *prober) warn(what string, err error) {
	if p.trouble.due(p.r.now(), what, err) {
		p.log.Warn("pipelines: "+what, "error", err)
	}
}

// probePods are the probe Job's pods by completion index -- the newest of
// each, and only the Job's own (podOfJob): a pod of an earlier probe Job of
// the same name, deleted and not yet collected, carries the same job-name
// label.
func probePods(pods []Pod, job Job) (zero, one *Pod) {
	for i := range pods {
		pod := &pods[i]
		if !podOfJob(pod, job) {
			continue
		}
		switch cmp.Or(pod.Metadata.Annotations[completionIndexKey], pod.Metadata.Labels[completionIndexKey]) {
		case "0":
			zero = newerPod(zero, pod)
		case "1":
			one = newerPod(one, pod)
		}
	}
	return zero, one
}

// listening is index 0's listener, when its pod has an address and the
// listener runs and is ready.
func listening(pod *Pod) (probeListener, bool) {
	l := podContainer(pod, true, ContainerProbeListener)
	if pod == nil || pod.Status.PodIP == "" || l == nil || l.State.Running == nil || !l.Ready {
		return probeListener{}, false
	}
	return probeListener{ip: pod.Status.PodIP, restarts: l.RestartCount, startedAt: l.State.Running.StartedAt}, true
}

// probeContainerRuns says a container of a probe pod is running.
func probeContainerRuns(pod *Pod, init bool, name string) bool {
	c := podContainer(pod, init, name)
	return c != nil && c.State.Running != nil
}

// describeProbePod says where a probe pod stands, for a proof that gave up
// waiting for it.
func describeProbePod(pod *Pod) string {
	if pod == nil {
		return "no pod yet"
	}
	if c := podCondition(pod, "PodScheduled"); c != nil && c.Status == "False" {
		return "waiting to be scheduled: " + reasonAnd(c.Reason, c.Message)
	}
	l := podContainer(pod, true, ContainerProbeListener)
	switch {
	case l == nil:
		return "its listener has not been created"
	case l.State.Waiting != nil:
		return "its listener is waiting: " + reasonAnd(l.State.Waiting.Reason, l.State.Waiting.Message)
	case l.State.Terminated != nil:
		return "its listener " + exitPhrase(l.State.Terminated) + messageOf(l.State.Terminated)
	case !l.Ready:
		return "its listener runs, not ready yet"
	}
	return "its listener is ready"
}

// describeConnector says where index 1's connector stands, for a proof that
// gave up waiting for it.
func describeConnector(pod *Pod) string {
	c := podContainer(pod, false, ContainerProbeConnector)
	switch {
	case pod == nil:
		return "index 1 had no pod"
	case c == nil:
		return "it had not been created"
	case c.State.Waiting != nil:
		return "it was waiting: " + reasonAnd(c.State.Waiting.Reason, c.State.Waiting.Message)
	case c.State.Running != nil:
		return "it was still running"
	}
	return "it had not started"
}

// isolationOutcome is the R42 table (jobspec.go): the connector's exit code,
// read with whether index 0's listener held -- still ready, the incarnation
// first seen ready, once the connector had ended; why says how it did not.
func isolationOutcome(end ContainerStateTerminated, held bool, why string) (isolated, inconclusive bool, detail string) {
	said := probeSaid(end)
	switch end.ExitCode {
	case probeExitIsolated:
		if held {
			return true, false, "a probe pod in memql-pipelines could not reach another probe pod's listener on any attempt, " +
				"while it reached the cluster's DNS on every one and the listener stayed ready (" + said + ")"
		}
		return false, true, "the connector could not reach the listener, but " + why +
			" once the connector had finished, so its attempts prove nothing (" + said + ")"
	case probeExitConnected:
		return false, false, "a probe pod in memql-pipelines reached another probe pod's listener (" + said + "): the namespace's " +
			"NetworkPolicy is not enforced, so a step could reach the mesh, the database and the node's metadata endpoint"
	case probeExitDNS:
		return false, true, "the connector could not reach the cluster's DNS on every attempt, so its failure to reach the " +
			"listener proves nothing about the policy (" + said + ")"
	case probeExitUnclear:
		return false, true, "an attempt to reach the listener failed with neither a refusal nor a timeout (" + said + ")"
	case probeExitNoNameserver:
		return false, true, "the probe pod has no nameserver to test the network with (" + said + ")"
	case probeExitForeignTarget:
		return false, true, "the probe Secret the connector read was not this probe's (" + said + ")"
	case probeExitNoPart:
		return false, true, "the probe pod had no part for its completion index (" + said + ")"
	}
	return false, true, fmt.Sprintf("the connector ended with exit code %d, which is no verdict (%s)", end.ExitCode, said)
}

// probeSaid is the connector's own words, from its termination message -- the
// tail of its log (FallbackToLogsOnError) -- on one line and cut short.
func probeSaid(end ContainerStateTerminated) string {
	if said := oneLine(captureRepair(end.Message)); said != "" {
		return cutBytes(said, probeSaidMaxBytes)
	}
	if end.Reason != "" && end.Reason != "Error" && end.Reason != "Completed" {
		return "it printed nothing; " + end.Reason
	}
	return "it printed nothing"
}
