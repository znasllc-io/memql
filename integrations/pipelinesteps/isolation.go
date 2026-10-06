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
//     one proof -- one probe, one slot under the ceiling -- each until its
//     run's ceiling (ruling R31b: like the wait for a slot, the proof's is
//     bounded by the run, and the step's own timeout runs from its Job's
//     creation). A proof every waiting step gave up on is stopped, and
//     decides nothing.
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
// depends on nothing; create the probe Secret naming its address and Job uid;
// wait for the listener's whole pod to be Ready, then release the connectors'
// scheduling gates. This prevents a connector completing while the listener's
// holder is still waiting for the Secret. Wait (probeEndWait) for both connectors to end;
// read the pods again (R42) and ask the listener's kubelet once (R42b); and
// judge by the restricted and positive-control exit codes and by whether the listener it tried is
// still ready, the incarnation that was ready before the Secret existed, in a
// pod still standing on a node that still answers. The probe's Job and Secret
// are deleted before anyone is answered, so under a ceiling of one the slot is
// free for the step.

// IsolationVerdict is what this replica's last proof found (Runner.Isolation).
type IsolationVerdict struct {
	// Isolated: a probe pod reached the cluster's DNS on every attempt and
	// another probe pod's listener, which stayed ready, on none. Only this
	// lets a step be created, and only when the positive control reached that
	// same listener on every round.
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
	// probeKubeletTimeout bounds the re-read's one round trip through the
	// listener's kubelet (R42b).
	probeKubeletTimeout = 5 * time.Second
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
// let it through, when its run reaches its ceiling first (R31b), or when it
// is cancelled.
func (s *step) isolationGate(runDeadline time.Time) (res pl.StepResult, done bool) {
	left := runDeadline.Sub(s.r.now())
	if left < time.Second {
		// The run's ceiling passed before the step reached this runner:
		// nothing to prove for.
		return s.ceilingPassed(runDeadline, "waiting for a free slot under the pipelines ceiling, or for a runner to take it"), true
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
		return s.ceilingPassed(runDeadline, "while this workbench node proved memql-pipelines isolated"), true
	}
	return s.refused(pl.CodeIsolationUnenforced, isolationRefusal(v)), true
}

// isolationRefusal is the sentence a step the proof did not let through is
// refused with: for a namespace found open, the fix; for a proof that could
// not decide, that it is tried again, and where to look if that persists.
func isolationRefusal(v IsolationVerdict) string {
	if v.Inconclusive {
		return "this workbench node could not prove memql-pipelines is network-isolated, and starts no pipeline step until it can: " +
			v.Detail + ". It tries again before the next step; if this persists, check that the cluster's network policy engine " +
			"is running and that cluster DNS (kube-system) answers."
	}
	return "memql-pipelines is not network-isolated, so this workbench node starts no pipeline step: " + v.Detail +
		". Enable a network policy engine on the cluster so the namespace's NetworkPolicy is enforced (on AKS: " +
		"az aks update --resource-group <group> --name <cluster> --network-policy <engine>); the runner proves it again before the next step."
}

// proveIsolation is this replica's verdict: a pass still fresh, or else the
// one proof in flight -- started here if none is -- once it decides. ctx is
// the waiting step's, bounded by its run's ceiling: when it ends first the
// step stops waiting (its error), and a proof no step waits on any more is
// stopped.
func (r *Runner) proveIsolation(ctx context.Context) (IsolationVerdict, error) {
	if err := ctx.Err(); err != nil {
		return IsolationVerdict{}, err
	}
	// Inspect CIDR grants even when the live pod verdict is cached: policy
	// drift must not inherit a previous proof of a different enforcement path.
	if err := r.kube.CheckIsolationCIDRs(ctx); err != nil {
		if ctx.Err() != nil {
			return IsolationVerdict{}, ctx.Err()
		}
		return IsolationVerdict{Inconclusive: true, Detail: err.Error(), At: r.now()}, nil
	}
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

// probeListener is index 0's listener as the probe saw it ready: its pod --
// by name and uid -- and its address, and the incarnation that was listening.
type probeListener struct {
	pod, uid  string
	ip        string
	restarts  int32
	startedAt time.Time
}

// probeIsolation runs the probe once, and answers its verdict; decided is
// false when no step waited on it any more before there was one. Cleanup is
// confirmed before a successful verdict is returned.
func (r *Runner) probeIsolation(ctx context.Context) (verdict IsolationVerdict, decided bool) {
	if err := r.cfg.ValidatePlacement(); err != nil {
		return IsolationVerdict{Inconclusive: true, Detail: err.Error(), At: r.now()}, true
	}
	if err := r.kube.CheckIsolationCIDRs(ctx); err != nil {
		return IsolationVerdict{Inconclusive: true, Detail: err.Error(), At: r.now()}, true
	}
	name := IsolationProbeName(r.cfg.NodeID)
	p := &prober{
		r: r, ctx: ctx, name: name, target: IsolationTargetName(name),
		log: r.log.With("probeJob", name, "node", r.cfg.NodeID), trouble: apiTrouble{},
	}
	// Whatever this replica's last probe left goes first: a probe Secret of
	// its would start the new connector at once, at another pod's address.
	if err := p.remove(); err != nil {
		return IsolationVerdict{Inconclusive: true, Detail: err.Error(), At: r.now()}, true
	}
	defer func() {
		if err := p.remove(); err != nil {
			p.log.Warn("pipelines: isolation probe cleanup remains unconfirmed", "error", err)
			if decided {
				detail := err.Error()
				if !verdict.Isolated && verdict.Detail != "" {
					detail = verdict.Detail + "; " + detail
				}
				verdict = IsolationVerdict{Inconclusive: true, Detail: detail, At: r.now()}
			}
		}
	}()

	var (
		job      Job
		listener probeListener
		end      ContainerStateTerminated
		control  ContainerStateTerminated
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
		err = p.startConnectors(job, listener)
	}
	if err == nil {
		end, control, err = p.end(job)
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
	isolated, inconclusive, detail := isolationOutcome(end, control, held, why)
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
			// Stopped with the create in flight: the API server may have made
			// the Job though its answer never came back, and remove's delete
			// can overtake the create. A Job left so ends at its own deadline
			// and is collected by its TTL -- probeJobDeadline and probeJobTTL,
			// some four minutes -- unless this replica's next proof deletes it
			// first. Until then it holds a slot under the ceiling: on
			// cloud-entry, the only one.
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
			if err := p.remove(); err != nil {
				return Job{}, probeFailure(err.Error())
			}
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

// up releases only index 0 and waits for its listener and address. The
// connectors stay gated until the listener's holder is ready.
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
		zero, one, two := probePods(pods, job)
		// Network policies select our role label: Cilium deliberately excludes
		// the Job's completion-index label from endpoint identities. Read the
		// labels back before arming, including after a lost patch response.
		if zero == nil || probeRole(zero) != "listener" {
			seen = "waiting for the probe listener's identity"
			continue
		}
		if zero.Metadata.Labels[LabelProbeRole] != "listener" || probeSchedulingHeld(zero) {
			if err := p.r.kube.SetProbeRole(ctx, job, *zero); err != nil {
				p.warn("labeling the isolation probe's listener", err)
			}
			seen = "waiting for the probe listener's verified network-policy role"
			continue
		}
		if l, ok := listening(zero); ok {
			return l, nil
		}
		seen = "index 0: " + describeProbePod(zero) + "; index 1: " + describeProbePod(one) + "; index 2: " + describeProbePod(two)
	}
}

// The holder and connectors reference the same Secret. Kubernetes may retry
// their missing-Secret lookups at different times. Keep the connectors gated
// until the holder has started and the listener's full pod is Ready; a ready
// sidecar alone is insufficient. Role labels still precede CNI endpoint creation.
func (p *prober) startConnectors(job Job, listener probeListener) error {
	ctx, cancel := context.WithTimeout(p.ctx, p.r.probeUpWait)
	defer cancel()
	released := false
	for first := true; ; first = false {
		if !first && !sleepCtx(ctx, p.r.cfg.PollInterval) {
			if p.ctx.Err() != nil {
				return errProbeStopped
			}
			return probeFailure("the listener pod and gated connectors did not become ready before the probe deadline")
		}
		pods, err := p.r.kube.JobPods(ctx, p.name)
		if err != nil {
			continue
		}
		zero, one, two := probePods(pods, job)
		current, listeningNow := listening(zero)
		if !listeningNow || current.uid != listener.uid || current.pod != listener.pod || current.ip != listener.ip ||
			current.restarts != listener.restarts || !current.startedAt.Equal(listener.startedAt) ||
			!zero.Metadata.DeletionTimestamp.IsZero() || disruption(zero) != nil {
			return probeFailure("the listener changed before the connectors started")
		}
		if !podReady(zero) || !probeContainerRuns(zero, false, ContainerProbeConnector) {
			if released {
				return probeFailure("the listener pod stopped being ready while the connectors started")
			}
			continue
		}
		rolesReady := true
		for _, pod := range []*Pod{one, two} {
			if pod == nil || probeRole(pod) == "" {
				rolesReady = false
				continue
			}
			if pod.Metadata.Labels[LabelProbeRole] != probeRole(pod) || probeSchedulingHeld(pod) {
				rolesReady = false
				if err := p.r.kube.SetProbeRole(ctx, job, *pod); err != nil {
					p.warn("releasing the isolation probe's connector", err)
				}
			}
		}
		released = true
		if rolesReady && probeContainerRuns(one, true, ContainerProbeListener) && probeContainerRuns(two, true, ContainerProbeListener) {
			return nil
		}
	}
}

// probeRole rejects a disagreement between the controller's annotation and
// label instead of assigning a network exception to an ambiguous index.
func probeRole(pod *Pod) string {
	if pod == nil {
		return ""
	}
	annotation, label := pod.Metadata.Annotations[completionIndexKey], pod.Metadata.Labels[completionIndexKey]
	if annotation != "" && label != "" && annotation != label {
		return ""
	}
	switch cmp.Or(annotation, label) {
	case "0":
		return "listener"
	case "1":
		return "restricted"
	case "2":
		return "control"
	default:
		return ""
	}
}

// arm creates the probe Secret, starting the listener's holder. The connectors
// reference it too, but remain gated until startConnectors observes readiness.
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
	case !existed:
		return nil
	}
	// The way was cleared before the probe Job was made, so a Secret already
	// there is this proof's own -- a create made, its answer lost, and the
	// retry met it -- or somebody else's since. Its own names this probe Job
	// and this listener, and is kept: a Runner proves one at a time. Any
	// other, the connectors may have read already.
	var target, uid string
	err = p.retry(func() (err error) {
		target, uid, err = p.r.kube.ProbeTarget(p.ctx, p.target)
		return err
	})
	switch {
	case p.ctx.Err() != nil:
		return errProbeStopped
	case err != nil:
		return probeFailure(fmt.Sprintf("a probe Secret, %s, was already there when this proof made its own, and could not be read: %s",
			p.target, apiMessage(err)))
	case uid != job.Metadata.UID || target != listenerIP:
		return probeFailure(fmt.Sprintf("a probe Secret, %s, was already there when this proof made its own, naming another probe Job or listener", p.target))
	}
	return nil
}

// end waits for index 1's connector to end, and answers how.
func (p *prober) end(job Job) (ContainerStateTerminated, ContainerStateTerminated, error) {
	ctx, cancel := context.WithTimeout(p.ctx, p.r.probeEndWait)
	defer cancel()
	seen := "it had not started"
	for first := true; ; first = false {
		if !first && !sleepCtx(ctx, p.r.cfg.PollInterval) {
			if p.ctx.Err() != nil {
				return ContainerStateTerminated{}, ContainerStateTerminated{}, errProbeStopped
			}
			return ContainerStateTerminated{}, ContainerStateTerminated{}, probeFailure(fmt.Sprintf("the probe's connector did not finish within %v: %s", p.r.probeEndWait, seen))
		}
		pods, err := p.r.kube.JobPods(ctx, p.name)
		if err != nil {
			if ctx.Err() == nil {
				p.warn("reading the isolation probe's pods", err)
				seen = "its pod could not be read: " + apiMessage(err)
			}
			continue
		}
		_, one, two := probePods(pods, job)
		c := podContainer(one, false, ContainerProbeConnector)
		control := podContainer(two, false, ContainerProbeConnector)
		if c != nil && c.State.Terminated != nil && control != nil && control.State.Terminated != nil {
			return *c.State.Terminated, *control.State.Terminated, nil
		}
		seen = "restricted: " + describeConnector(one) + "; positive control: " + describeConnector(two)
	}
}

// held reads the probe's pods again, after the connector ended (R42), and
// says whether index 0's listener is still the incarnation first seen ready,
// in the same pod, and ready. Then its socket was open from before the
// connector's first attempt -- the connector waited for the Secret, made once
// the listener was ready -- to after its last, so an attempt that reached it
// would have connected. why says how it did not hold.
//
// The API server's own marks on the pod come first (fix round 1): a delete
// or an eviction is recorded on the pod -- its deletionTimestamp, a True
// DisruptionTarget condition -- before any kubelet acts, while the kubelet's
// last report of the listener still reads running and ready, and a dying
// pod's refusals are no evidence of a policy. A replacement started within
// the second the first one did reads the same startedAt, so the pod is
// matched by its uid.
//
// And the pod's status is its kubelet's last word (R42b): a node lost in the
// seconds after the listener was seen ready can no longer update its pod,
// which reads as it was, and the node controller acts only after its grace
// period -- by marking the pod's Ready condition, never the container's
// ready. On a cluster that enforces no policy, the SYNs a
// lost node never answers time out, and DNS answers from another node, which
// reads as a pass. So the pod's Ready condition is read too, and, last, one
// round trip is made THROUGH the listener's kubelet -- its log, bounded at
// probeKubeletWait -- which a lost node cannot answer.
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
	zero, _, _ := probePods(pods, job)
	l := podContainer(zero, true, ContainerProbeListener)
	switch {
	case zero == nil:
		return false, "the listener's pod was gone", nil
	case zero.Metadata.UID != was.uid && zero.Metadata.Name == was.pod:
		return false, fmt.Sprintf("the listener's pod %s had been replaced by a new pod of the same name since it was seen ready", was.pod), nil
	case zero.Metadata.UID != was.uid || zero.Metadata.Name != was.pod:
		return false, fmt.Sprintf("the listener's pod %s had been replaced by %s since it was seen ready", was.pod, zero.Metadata.Name), nil
	case !zero.Metadata.DeletionTimestamp.IsZero():
		return false, "the listener's pod was being deleted", nil
	case disruption(zero) != nil:
		d := disruption(zero)
		return false, "the listener's pod was marked for disruption (" + reasonAnd(d.Reason, d.Message) + ")", nil
	case zero.Status.Phase == "Succeeded" || zero.Status.Phase == "Failed":
		return false, "the listener's pod had ended (" + zero.Status.Phase + ")", nil
	case l == nil || l.State.Running == nil:
		return false, "the listener was no longer running", nil
	case l.RestartCount != was.restarts || !l.State.Running.StartedAt.Equal(was.startedAt):
		return false, "the listener had restarted since it was seen ready", nil
	case !l.Ready:
		return false, "the listener was not ready", nil
	case !podReady(zero):
		return false, "the listener's pod's Ready condition was not True", nil
	}
	return p.kubeletAnswers(zero)
}

// podReady says a pod's Ready condition is True.
func podReady(pod *Pod) bool {
	c := podCondition(pod, "Ready")
	return c != nil && c.Status == "True"
}

// kubeletAnswers makes the re-read's one round trip through the listener's
// kubelet (R42b): a tail of the listener's log, which the API server can
// fetch only from that kubelet, bounded at probeKubeletWait. The log itself
// is nothing -- the listener prints none -- the answer is the point. Any
// failure is a node that may be lost: not held.
func (p *prober) kubeletAnswers(pod *Pod) (bool, string, error) {
	ctx, cancel := context.WithTimeout(p.ctx, p.r.probeKubeletWait)
	defer cancel()
	_, err := p.r.kube.TailLog(ctx, pod.Metadata.Name, ContainerProbeListener, 1)
	switch {
	case err == nil:
		return true, "", nil
	case p.ctx.Err() != nil:
		return false, "", errProbeStopped
	case ctx.Err() != nil:
		return false, fmt.Sprintf("the listener's kubelet did not answer within %v, so its node may be lost", p.r.probeKubeletWait), nil
	}
	return false, "the listener's kubelet could not be reached through the API server, so its node may be lost: " + apiMessage(err), nil
}

// remove confirms foreground deletion of the probe and its pods before the
// same name or quota slot can be reused. Its independent context also gives
// an abandoned proof a bounded opportunity to clean up.
func (p *prober) remove() error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(p.ctx), quickCallTimeout)
	defer cancel()
	jobErr := p.r.kube.DeleteJob(ctx, p.name)
	secretErr := p.r.kube.DeleteSecret(ctx, p.target)
	if err := errors.Join(jobErr, secretErr); err != nil {
		return fmt.Errorf("isolation probe cleanup remains unconfirmed: %w", err)
	}
	for {
		_, jobErr := p.r.kube.GetJob(ctx, p.name)
		_, secretErr := p.r.kube.SecretMetadata(ctx, p.target)
		pods, podErr := p.r.kube.JobPods(ctx, p.name)
		if deploycontrol.IsNotFound(jobErr) && deploycontrol.IsNotFound(secretErr) && podErr == nil && len(pods) == 0 {
			return nil
		}
		if jobErr != nil && !deploycontrol.IsNotFound(jobErr) {
			return fmt.Errorf("isolation probe cleanup remains unconfirmed: Job could not be read: %w", jobErr)
		}
		if secretErr != nil && !deploycontrol.IsNotFound(secretErr) {
			return fmt.Errorf("isolation probe cleanup remains unconfirmed: Secret could not be read: %w", secretErr)
		}
		if podErr != nil {
			return fmt.Errorf("isolation probe cleanup remains unconfirmed: pods could not be read: %w", podErr)
		}
		if !sleepCtx(ctx, p.r.cfg.PollInterval) {
			return fmt.Errorf("isolation probe cleanup remains unconfirmed: %w", ctx.Err())
		}
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
func probePods(pods []Pod, job Job) (zero, one, two *Pod) {
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
		case "2":
			two = newerPod(two, pod)
		}
	}
	return zero, one, two
}

// listening is index 0's listener, when its pod has an address and a uid --
// the re-read knows the same pod by it, never by its name alone -- and the
// listener runs and is ready.
func listening(pod *Pod) (probeListener, bool) {
	l := podContainer(pod, true, ContainerProbeListener)
	if pod == nil || pod.Status.PodIP == "" || pod.Metadata.UID == "" || l == nil || l.State.Running == nil || !l.Ready {
		return probeListener{}, false
	}
	return probeListener{
		pod: pod.Metadata.Name, uid: pod.Metadata.UID, ip: pod.Status.PodIP,
		restarts: l.RestartCount, startedAt: l.State.Running.StartedAt,
	}, true
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
func isolationOutcome(end, control ContainerStateTerminated, held bool, why string) (isolated, inconclusive bool, detail string) {
	said := probeSaid(end)
	switch end.ExitCode {
	case probeExitIsolated:
		if control.ExitCode != probeExitControlPassed {
			return false, true, "the positive control did not reach the same listener on every attempt; ingress or connectivity may explain the restricted connector's failure (" + probeSaid(control) + ")"
		}
		if held {
			return true, false, "a probe pod in memql-pipelines could not reach another probe pod's listener on any attempt, " +
				"while DNS and a positive control to the same listener answered every time, and the listener stayed ready (" + said + ")"
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
