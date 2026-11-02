package packages

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/logger"
)

// rollbackBudget stands where the caller's lifetime was. A rollback is one row
// write per site, one blob write and one patch per rolled workload, with nothing to
// build or upload, so it is far shorter than the thirty minutes a confirmed run gets
// for the same stages. Too short turns a slow database into the apart-state this
// exists to prevent, and too long lets a rollback somebody has given up on land over
// whatever they did since.
const rollbackBudget = 10 * time.Minute

// RollbackRequest restores a package to what an earlier deployment left live.
type RollbackRequest struct {
	PackageId    string
	DeploymentId string
	Actor        Actor
	// budget replaces rollbackBudget for a test that needs the deadline to arrive.
	// Not accepted from a client.
	budget time.Duration
}

// RestoredState is what a rollback put back.
type RestoredState struct {
	DeploymentId string              `json:"deploymentId"`
	DslVersion   string              `json:"dslVersion,omitempty"`
	Deployables  []DeployableOutcome `json:"deployables"`
	// PointerMoved says the active-set pointer was written, which Rolled alone
	// cannot say about a roll that failed after it.
	PointerMoved bool `json:"pointerMoved"`
	Rolled       bool `json:"rolled"`
}

// Rollback executes the D6 order REVERSED: publish back FIRST, then the
// pointer back and a roll.
//
// The reversal is the entire point and it is not symmetry for its own sake.
// Forward, the schema must arrive before the app that uses it -- so stage and
// roll come before publish. Backward, the app must stop using the schema
// before the schema goes away -- so the publish comes back first. Doing it in
// the forward order on the way back would leave, for the width of the rollout,
// new application code talking to old DSL: the one window D6 exists to close.
//
// A rollback restores a TUPLE (dslVersion + each site's bundleRef), not a
// sequence of events. The prior deployment row already records exactly that
// tuple, which is why the timeline being append-only is what makes rollback
// possible at all.
//
// ===========================================================================
// ONCE IT CAN MOVE SOMETHING IT IS THE CLUSTER'S, AND THE CALL ONLY WAITS FOR IT
// ===========================================================================
// ctx is a stream's context when a person's browser made this call, so a missed
// websocket ping cancels it. Between the sites and the pointer that leaves the app
// back and the DSL not, with nothing left running to finish, which is the state the
// order above exists to prevent. So the checks stay on ctx, where going away changes
// nothing, and everything after them keeps the caller's authority and drops only its
// lifetime. A caller that stays gets the outcome. One that goes first gets its own
// context's error, and the rollback goes on to its end.
//
// Nothing stops a rollback once it has started. Cancel belongs to a run's row and is
// honoured only before the roll, and a rollback begins with the publish. Rolling back
// again, to whatever is wanted, is the way out.
//
// This is not a persistent executor either. If the process running it dies, nothing
// finishes the job or closes it. Its log lines stop where it stopped, the trail has
// no ending for it, and a person rolls back again, which repeats what is already done.
func Rollback(ctx context.Context, d *Deps, req RollbackRequest) (*RestoredState, error) {
	plan, err := prepareRollback(ctx, d, req)
	if err != nil {
		d.finishRollback(ctx, req, nil, err)
		return nil, err
	}
	budget := req.budget
	if budget <= 0 {
		budget = rollbackBudget
	}
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
	finished := make(chan finishedRollback, 1)
	go func() {
		defer cancel()
		restored, err := executeRollback(runCtx, d, req, plan)
		d.finishRollback(ctx, req, restored, err)
		finished <- finishedRollback{restored: restored, err: err}
	}()
	select {
	case run := <-finished:
		return run.restored, run.err
	case <-ctx.Done():
		d.log().Info("packages: the caller stopped waiting for a rollback, which goes on",
			"component", "packages.rollback", rollbackSubject(req),
			"package", req.PackageId, "deployment", req.DeploymentId, "err", ctx.Err())
		return &RestoredState{DeploymentId: req.DeploymentId, DslVersion: plan.dslVersion}, ctx.Err()
	}
}

// finishedRollback is what a rollback ended with.
type finishedRollback struct {
	restored *RestoredState
	err      error
}

// rollbackPlan is what a rollback learned before anything moved.
type rollbackPlan struct {
	prior      map[string]any
	outcomes   []DeployableOutcome
	dslVersion string
	// current is the active set the pointer is moved from. Nil for a deployment
	// that recorded no DSL.
	current map[string]string
}

// rollbackSubject is what a rollback's lines are about: the deployment it returns
// to, whose Logs button sits beside "Roll back to this". That button filters on the
// exact bare id, whichever spelling the caller sent.
func rollbackSubject(req RollbackRequest) slog.Attr {
	return logger.Subject(packageDeploymentConcept, memql.BareShortId(req.DeploymentId))
}

// prepareRollback is everything that changes nothing: the row, the refusals and the
// reads a refusal depends on. It runs on the caller's context.
func prepareRollback(ctx context.Context, d *Deps, req RollbackRequest) (*rollbackPlan, error) {
	prior, err := d.Store.deploymentById(ctx, req.DeploymentId)
	if err != nil {
		return nil, err
	}
	if prior == nil {
		return nil, refuse(CodeSourceUnreadable,
			"no deployment %q is readable by this caller", req.DeploymentId)
	}
	if !sameShortId(rowString(prior, "packageId"), req.PackageId) {
		return nil, refuse(CodeSourceUnreadable,
			"deployment %q belongs to a different package", req.DeploymentId)
	}
	if got := rowString(prior, "status"); got != StatusSucceeded {
		return nil, refuse(CodeSourceUnreadable,
			"deployment %q finished as %q rather than succeeded, so there is no state on it to restore. Roll back to a deployment that was live.",
			req.DeploymentId, got)
	}

	outcomes, err := deployableOutcomes(prior)
	if err != nil {
		return nil, err
	}
	dslVersion := rowString(prior, "dslVersion")

	// The D9 gate applies to a rollback for the same reason it applies to a
	// deploy: putting a DSL version back is changing what the cluster can do.
	if dslVersion != "" && !req.Actor.MayDeployDsl {
		return nil, refuse(CodeDslRequiresAuthoring,
			"this deployment carried MemQL DSL, so rolling back to it changes what the whole cluster can do -- which is owner or developer only.")
	}

	// A deployment that carried DSL cannot be put back without a surface to
	// stage it on, and a rollback that restored the sites and not the DSL would
	// leave the two apart -- and, worse for what follows, would have no DSL of the
	// earlier deployment's to describe. So it refuses exactly as stageAndRoll does
	// when it has no stager, and BEFORE anything moves, as the gate above does.
	//
	// TWO WAYS OF HAVING NO SURFACE, both answered here. A node built with no
	// stager refuses on the nil check. A node whose stager has no storage behind
	// it -- the integration always wires the blob stager, so this is every cluster
	// with no object storage -- refuses in the stager's first read, which the nil
	// check cannot see. That read changes nothing, so it is asked here rather than
	// after the sites were repointed: there it refused with the sites already
	// back and the DSL not, the apart-state this refusal exists to prevent. The
	// map it returns is the one the pointer is moved from below.
	var current map[string]string
	if dslVersion != "" {
		if d.Stager == nil {
			return nil, refuse(CodeSourceUnreadable,
				"this deployment carried MemQL DSL and this cluster has no DSL staging surface configured, so its DSL cannot be put back. Nothing was changed; roll back from a node that has one.")
		}
		var rerr error
		if current, rerr = d.Stager.ReadActiveSet(ctx); rerr != nil {
			return nil, rerr
		}
	}
	return &rollbackPlan{prior: prior, outcomes: outcomes, dslVersion: dslVersion, current: current}, nil
}

// executeRollback moves things, on a context no caller can end. Each thing it moves
// gets a line, because if the process dies those lines are all that says how far it
// got.
func executeRollback(ctx context.Context, d *Deps, req RollbackRequest, plan *rollbackPlan) (*RestoredState, error) {
	prior, dslVersion, current := plan.prior, plan.dslVersion, plan.current
	subject := rollbackSubject(req)
	restored := &RestoredState{DeploymentId: req.DeploymentId, DslVersion: dslVersion}
	d.log().Info("packages: rolling back to this deployment",
		"component", "packages.rollback", subject,
		"package", req.PackageId, "deployment", req.DeploymentId, "dslVersion", dslVersion)

	// ---- publish back FIRST ----
	for _, o := range plan.outcomes {
		if o.SiteId == "" || o.BundleRef == "" {
			continue
		}
		if err := d.Publisher.RepointSite(ctx, o.SiteId, o.BundleRef); err != nil {
			return restored, fmt.Errorf("packages: repointing %s: %w", o.SiteId, err)
		}
		restored.Deployables = append(restored.Deployables, o)
		d.log().Info("packages: this rollback put a site back",
			"component", "packages.rollback", subject,
			"package", req.PackageId, "deployment", req.DeploymentId, "site", o.SiteId, "bundleRef", o.BundleRef)
	}

	// ---- then the pointer back, and roll ----
	//
	// dslLive says the earlier deployment's DSL is the cluster's when this block
	// ends: put back with its roll requested just now, or found to be the active
	// set already. (Roll returns once the rollout is asked for, so nodes may still
	// be restarting onto it; a deploy's list is written at the same point for the
	// same reason, and a gate needs a booted registry as well as the list.) It is
	// what lets the list below be written, and it stays false for a deployment
	// that recorded no DSL (nothing to put back; the rollback leaves the DSL
	// alone) and for a recorded version that names no domain (nothing restored).
	dslLive := false
	if dslVersion != "" {
		next := map[string]string{}
		for k, v := range current {
			next[k] = v
		}
		changed := false
		named := 0
		for _, prefix := range strings.Fields(dslVersion) {
			domain, ok := domainFromPrefix(prefix)
			if !ok {
				continue
			}
			named++
			if next[domain] != prefix {
				next[domain] = prefix
				changed = true
			}
		}
		if changed {
			// Whoever puts DSL back answers for it, recorded before the pointer moves for
			// the reason a deploy's is.
			if err := d.recordDslDeployerBeforeStaging(ctx, req.PackageId, req.Actor.UserId, declaresFleetWorkflows(reportFromRow(prior))); err != nil {
				return restored, err
			}
			if err := d.Stager.WriteActiveSet(ctx, next); err != nil {
				return restored, err
			}
			restored.PointerMoved = true
			d.log().Info("packages: this rollback moved the DSL pointer back",
				"component", "packages.rollback", subject,
				"package", req.PackageId, "deployment", req.DeploymentId, "dslVersion", dslVersion)
			if d.Roller == nil {
				return restored, refuse(CodeSourceUnreadable,
					"the pointer was moved back but this node has no rollout surface, so nothing is serving the restored DSL yet. Roll again from a node that has one.")
			}
			if err := d.Roller.Roll(ctx, "package rollback "+req.DeploymentId); err != nil {
				return restored, err
			}
			restored.Rolled = true
			d.log().Info("packages: this rollback asked for the roll",
				"component", "packages.rollback", subject,
				"package", req.PackageId, "deployment", req.DeploymentId)
		}
		dslLive = named > 0
	}

	// ---- and the list of domains goes back with the DSL ----
	//
	// REWRITTEN FROM THE DEPLOYMENT IT RETURNS TO (design record 2026-10-29, D5),
	// from the report that deployment recorded, and ONLY WHEN ITS DSL IS THE
	// CLUSTER'S: put back just now, or found already active. The list is the
	// bff's reading of which domains this package's DSL belongs to, so it is never
	// written for DSL the cluster does not have -- the order a deploy keeps, for
	// the same reason, and the gap between the DSL arriving and the list arriving
	// fails closed. A rollback that did not complete returned above and wrote
	// nothing.
	//
	// A deployment that recorded no DSL has none to put back, so the rollback
	// leaves the DSL as it is and the list with it: writing that deployment's own
	// list over DSL a later deployment added would describe DSL that is not what
	// is live.
	//
	// A deployment recorded before the list existed states no namespaces, so
	// there is nothing to restore and the honest list is empty: its gates stay
	// closed until the next deploy writes one. A warning rather than a failure,
	// for the reason the deploy's write is.
	if !dslLive {
		d.log().Info("packages: rolled back; the earlier deployment's DSL was not part of what was restored, so the record of which DSL domains this package ships is left as it was",
			"component", "packages.rollback", subject,
			"package", req.PackageId, "deployment", req.DeploymentId, "dslVersion", dslVersion)
		return restored, nil
	}
	if derr := d.Store.recordPackageDslDomains(ctx, req.PackageId, dslDomainsFrom(reportFromRow(prior))); derr != nil {
		d.log().Warn("packages: rolled back, but could not record which DSL domains this package ships; its access gates stay as the previous list left them",
			"component", "packages.rollback", subject,
			"package", req.PackageId, "deployment", req.DeploymentId, "err", derr)
	}
	return restored, nil
}

// finishRollback says how a rollback ended, whoever is still connected, because a
// reply is only ever read by a caller who stayed. restored is nil when it ended before
// anything could move.
//
// The log line is what a person reads: the Logs button on the deployment they rolled
// back to opens it, and the roles holding the publish part may read logs. It is written
// first, so a trail that cannot be written costs nothing the person needed. The audit
// event is the record: who asked and how it ended, when the store has dropped the line
// or aged it out.
//
// A context that outlives both the caller and the budget, for the reason a run's close
// has one: a rollback that ran out of time still has to be able to say so.
func (d *Deps) finishRollback(ctx context.Context, req RollbackRequest, restored *RestoredState, err error) {
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	ev := RollbackAuditEvent{PackageId: req.PackageId, DeploymentId: req.DeploymentId, Outcome: "success"}
	if restored != nil {
		ev.DslVersion, ev.PointerMoved, ev.Rolled = restored.DslVersion, restored.PointerMoved, restored.Rolled
		for _, o := range restored.Deployables {
			ev.Sites = append(ev.Sites, o.SiteId)
		}
	}
	attrs := []any{"component", "packages.rollback", rollbackSubject(req),
		"package", req.PackageId, "deployment", req.DeploymentId,
		"sites", ev.Sites, "pointerMoved", ev.PointerMoved, "rolled", ev.Rolled}
	switch {
	case err == nil:
		d.log().Info("packages: rolled back to this deployment", attrs...)
	case restored == nil:
		ev.Outcome, ev.FailureReason = "failure", err.Error()
		if RefusalCode(err) != "" {
			ev.Outcome = "blocked"
		}
		d.log().Warn("packages: a rollback ended before anything moved", append(attrs, "err", err)...)
	default:
		ev.Outcome, ev.FailureReason = "failure", err.Error()
		d.log().Error("packages: a rollback stopped before it finished; sites says what was put back, and pointerMoved and rolled say where the DSL got to", append(attrs, "err", err)...)
	}
	if d.Auditor != nil {
		d.Auditor.Rollback(finishCtx, ev)
	}
}

// domainFromPrefix reads the domain out of packages/<domain>/<hash>/.
func domainFromPrefix(prefix string) (string, bool) {
	parts := strings.Split(strings.Trim(prefix, "/"), "/")
	if len(parts) < 2 || parts[0] != "packages" {
		return "", false
	}
	return parts[1], true
}

// deployableOutcomes reads the per-deployable outcomes back off a row.
func deployableOutcomes(row map[string]any) ([]DeployableOutcome, error) {
	raw, ok := row["deployables"]
	if !ok || raw == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var out []DeployableOutcome
	if err := json.Unmarshal(encoded, &out); err != nil {
		return nil, fmt.Errorf("packages: this deployment's recorded outcomes are not readable: %w", err)
	}
	return out, nil
}
