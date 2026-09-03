package packages

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// sweep.go is the abandoned-run half of epic memql#4900 (task memql#4902).
//
// ===========================================================================
// THE COST THE PIPELINE ALREADY NAMED, NOW PAID
// ===========================================================================
// pipeline.go states it plainly: the whole run is one call on one node, and a
// node lost mid-deploy strands its row at a non-terminal status. Nothing is
// inconsistent -- the D6 order guarantees every site is still serving what it
// was serving -- but the row does not advance again on its own, and the person
// watching sees a rail that says "building" forever.
//
// Two pieces close it. A HEARTBEAT the running pipeline writes on a fixed
// cadence, and a SWEEP that closes a row whose heartbeat stopped. The sweep is
// the one writer allowed to close a stranded row, and it closes it with a
// status of its own.
//
// ===========================================================================
// WHY `abandoned` AND NOT `failed`
// ===========================================================================
// `failed` is a verdict, and this sweep has no evidence for one. It does not
// know whether the build was about to succeed; it knows the node stopped
// answering. Writing `failed` would put a claim about somebody's package into
// an append-only record that cannot be corrected -- and it would be the wrong
// claim in the most common case, which is a rolling restart landing on the
// replica that happened to be deploying.
//
// So the status says what is known, and the error says the rest: which node
// was running it, and when it was last heard from.

const (
	// HeartbeatIntervalEnv tunes how often a running pipeline says it is
	// alive. Named beside the threshold because the two are a PAIR: the
	// threshold must be a multiple of this, or a healthy run gets swept.
	HeartbeatIntervalEnv = "MEMQL_PACKAGES_HEARTBEAT_SECONDS"
	// AbandonedAfterEnv is how old a heartbeat must be before the sweep
	// closes the row.
	AbandonedAfterEnv = "MEMQL_PACKAGES_ABANDONED_AFTER_SECONDS"
)

const (
	// DefaultHeartbeatSeconds is 15s, the fleet's own heartbeat cadence. A
	// deploy is minutes long, so this costs a handful of writes per run.
	DefaultHeartbeatSeconds = 15
	// DefaultAbandonedAfterSeconds is 90s: SIX heartbeats, not two.
	//
	// The margin is the point. A run that misses one heartbeat is a run whose
	// node had a slow moment -- a GC pause, a database hiccup, a saturated
	// mesh -- and closing it would turn a transient into a permanent record
	// nothing can amend. Six missed heartbeats in a row is a node that is
	// gone. The cost of waiting is ninety seconds of a rail that says
	// "building"; the cost of not waiting is a deploy declared lost while it
	// was still running, which then PUBLISHES -- because the sweep closes the
	// row and the node, still alive, carries on to a publish it can no longer
	// record.
	DefaultAbandonedAfterSeconds = 90
)

// HeartbeatInterval is how often a running pipeline writes heartbeatAt.
func HeartbeatInterval() time.Duration {
	return time.Duration(envInt64(HeartbeatIntervalEnv, DefaultHeartbeatSeconds)) * time.Second
}

// AbandonedAfter is how stale a heartbeat must be for the sweep to act.
//
// CLAMPED to at least three heartbeats. An operator who sets the threshold
// below the cadence has configured a cluster that sweeps its own healthy
// deploys, and the failure is invisible from the value alone: every run would
// simply end `abandoned` a minute in, looking like a broken build surface.
func AbandonedAfter() time.Duration {
	after := time.Duration(envInt64(AbandonedAfterEnv, DefaultAbandonedAfterSeconds)) * time.Second
	if floor := 3 * HeartbeatInterval(); after < floor {
		return floor
	}
	return after
}

// heartbeat keeps a run's heartbeatAt current until ctx is done.
//
// It writes ONCE IMMEDIATELY and then on the cadence, so a run that dies in
// its first seconds is still swept: without the first write, heartbeatAt would
// be whatever openPackageDeployment stamped, which is the same instant the row
// was created, and the sweep would be judging the wrong clock.
//
// A failed write is logged and the loop continues. The alternative -- failing
// the deploy because a bookkeeping write did not land -- would make a database
// hiccup destroy a build that was going fine.
func (d *Deps) heartbeat(ctx context.Context, deploymentId string) func() {
	interval := HeartbeatInterval()
	beat := func() {
		if err := d.Store.heartbeatDeployment(ctx, deploymentId, d.now()); err != nil {
			d.log().Debug("packages: the deployment heartbeat could not be written",
				"component", "packages.pipeline", "deployment", deploymentId, "err", err)
		}
	}
	beat()
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				beat()
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

// SweepResult is what one pass found.
type SweepResult struct {
	Checked   int `json:"checked"`
	Abandoned int `json:"abandoned"`
}

// SweepAbandoned closes every run whose heartbeat has stopped.
//
// The read is every in-flight run in the cluster, which the composite tier
// admits only to a cluster owner -- so this runs under the maintenance actor
// (component/auth/maintenance_actor.go names it and says why). Under the
// default automation reader the read answers zero rows and no error, and a
// sweep that closes nothing looks exactly like a cluster with nothing
// stranded.
func SweepAbandoned(ctx context.Context, d *Deps) (SweepResult, error) {
	rows, err := d.Store.deploymentsInFlight(ctx)
	if err != nil {
		return SweepResult{}, err
	}
	res := SweepResult{Checked: len(rows)}
	cutoff := d.now().Add(-AbandonedAfter())
	for _, row := range rows {
		id := rowString(row, "id")
		if id == "" {
			continue
		}
		last, ok := lastHeartbeat(row)
		if !ok {
			// A row with no heartbeat AND no start time cannot be judged, so
			// it is left alone. Sweeping on an absent timestamp would close
			// every row written before this field existed the moment this
			// shipped -- including ones a person is looking at.
			continue
		}
		if last.After(cutoff) {
			continue
		}
		problem := &Problem{
			Code:    CodeDeploymentAbandoned,
			Message: abandonedMessage(row, last),
			Fatal:   true,
		}
		// THE STAGE THE RUN HAD REACHED, kept before the status that carries
		// it is overwritten. Closing the row is what destroys this fact, and
		// without it the timeline draws a run that died mid-build as having
		// stopped at Analyze -- understating what it achieved and sending
		// somebody to look in the wrong place.
		if err := d.Store.abandonDeployment(ctx, id, rowString(row, "status"), problem, d.now()); err != nil {
			// One row that will not close must not stop the rest: the next
			// pass tries it again, and every other stranded run in the
			// cluster still deserves closing now.
			d.log().Warn("packages: could not close an abandoned deployment",
				"component", "packages.sweep", "deployment", id, "err", err)
			continue
		}
		d.log().Info("packages: closed a deployment whose node stopped answering",
			"component", "packages.sweep", "deployment", id,
			"lastHeartbeat", last.UTC().Format(time.RFC3339),
			"node", rowString(row, "nodeId"))
		res.Abandoned++
		if d.Auditor != nil {
			// AUDITED under package_deploy, beside the run's own outcome: the
			// trail's question is "what happened to this deploy", and "the
			// cluster gave up on it" is an answer to that question.
			d.Auditor.Deploy(ctx, DeployAuditEvent{
				PackageId:     rowString(row, "packageId"),
				DeploymentId:  id,
				SourceVersion: rowString(row, "sourceVersion"),
				Status:        StatusAbandoned,
				FailureReason: CodeDeploymentAbandoned,
			})
		}
	}
	return res, nil
}

// lastHeartbeat reads the newest evidence that a run was alive.
//
// heartbeatAt when there is one, and startedAt otherwise -- because a run
// written by an engine older than this field, or one that died before its
// first beat, still has a start time, and "it started nine days ago and never
// finished" is enough to act on.
func lastHeartbeat(row map[string]any) (time.Time, bool) {
	for _, key := range []string{"heartbeatAt", "startedAt", "createdAt"} {
		if v := rowString(row, key); v != "" {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				return t, true
			}
			if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

// abandonedMessage is the sentence the OS renders verbatim on the rail.
//
// It says the three things a person needs and nothing it cannot know: that the
// cluster lost the node, that nothing was published, and when the run was last
// heard from. It does NOT say the deploy failed, because the sweep does not
// know that.
func abandonedMessage(row map[string]any, last time.Time) string {
	node := rowString(row, "nodeId")
	where := ""
	if node != "" {
		where = fmt.Sprintf(" (%s)", node)
	}
	return fmt.Sprintf(
		"this cluster lost the node that was running this deploy%s; nothing was published, and every site is still serving what it was serving. It was last heard from at %s. Retry starts a fresh run from the same source.",
		where, last.UTC().Format(time.RFC3339))
}

// selfNodeId names this replica, for the row and the log line.
func selfNodeId() string {
	if v := strings.TrimSpace(os.Getenv("MEMQL_NODE_ID")); v != "" {
		return v
	}
	if host, err := os.Hostname(); err == nil {
		return strings.TrimSpace(host)
	}
	return ""
}

// handleSweepAbandoned is the automation's entry.
func (i *Integration) handleSweepAbandoned(ctx context.Context, _ map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	deps, err := i.resolve()
	if err != nil {
		return nil, err
	}
	res, serr := SweepAbandoned(ctx, deps)
	if serr != nil {
		return nil, serr
	}
	return resultNode(map[string]any{"checked": res.Checked, "abandoned": res.Abandoned}), nil
}
