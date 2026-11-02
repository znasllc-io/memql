package packages

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// StartConfirmation accepts one parked attempt and answers before anything is
// built, leaving the live row to carry progress and the result.
func StartConfirmation(ctx context.Context, d *Deps, req DeployRequest) (*DeployOutcome, error) {
	if !req.Confirmed || req.Automatic || strings.TrimSpace(req.DeploymentId) == "" {
		return nil, fmt.Errorf("background confirmation requires a parked deployment ID and confirm:true")
	}
	pkg, out, prepared, err := prepareConfirmation(ctx, d, req, true)
	if err != nil {
		return nil, err
	}
	accepted := *out
	startConfirmed(ctx, d, prepared, pkg, out)
	return &accepted, nil
}

// confirmedRun is what a confirmed attempt ended with.
type confirmedRun struct {
	out *DeployOutcome
	err error
}

// startConfirmed runs one confirmed attempt for as long as the attempt takes, so
// closing the browser does not cancel a build somebody confirmed. It keeps the
// caller's authority and drops only its lifetime: what reaches the row is the
// caller's actor, and the run's heartbeat, Cancel and terminal write all live on
// the detached context. Builds keep their existing per-build deadlines.
//
// A budget replaces the caller's lifetime as the outer bound. The heartbeat ends
// with it, so the sweep can close the row of a stage that ignores its own
// deadline, even while that stage stays stuck.
//
// This is not a persistent executor. If the process running the attempt dies,
// including when a package's own DSL restarts that node, the sweep closes the
// row `abandoned` and a person retries it (sweep.go). Nothing here replays a
// build or continues on another replica.
func startConfirmed(ctx context.Context, d *Deps, req DeployRequest, pkg map[string]any, out *DeployOutcome) <-chan confirmedRun {
	budget := req.confirmationTimeout
	if budget <= 0 {
		budget = confirmationTimeoutWithoutReport(pkg)
	}
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), budget)
	finished := make(chan confirmedRun, 1)
	go func() {
		defer cancel()
		ended, err := executeDeployment(runCtx, d, req, pkg, out)
		finished <- confirmedRun{out: ended, err: err}
	}()
	return finished
}

// Both synchronous and background confirmations take the existing database
// gate. A second replica must read the committed claim, not its cached parked
// row. The claim never creates another attempt or changes its snapshot.
func prepareConfirmation(ctx context.Context, d *Deps, req DeployRequest, requireSnapshot bool) (pkg map[string]any, out *DeployOutcome, prepared DeployRequest, err error) {
	prepared = req
	err = d.Store.withDeploymentOpeningGate(ctx, req.PackageId, func(ctx context.Context) error {
		ctx = memql.ContextWithFreshRead(ctx)
		row, err := d.Store.deploymentById(ctx, req.DeploymentId)
		if err != nil {
			return err
		}
		if row == nil {
			return refuse(CodeSourceUnreadable, "no deployment %q is readable by this caller", req.DeploymentId)
		}
		if !sameShortId(rowString(row, "packageId"), req.PackageId) {
			return refuse(CodeSourceUnreadable, "deployment %q belongs to a different package", req.DeploymentId)
		}
		if rowString(row, "status") != StatusAwaitingConfirm {
			return fmt.Errorf("deployment %q already exists and is not awaiting confirmation", req.DeploymentId)
		}
		if rowBool(row, "cancelRequested") {
			return refuse(CodeDeploymentCancelled, "This deployment was stopped before confirmation.")
		}
		// A background confirmation must accept exactly the report the person
		// reviewed. A synchronous one (the Go SDK's) waits for its run and
		// re-analyses as it always did, so it requires neither.
		report := reportFromRow(row)
		if requireSnapshot && (report == nil || !report.OK) {
			return refuse(CodeSnapshotUnavailable, "This deployment has no saved analysis report. Analyze the source again.")
		}
		if report != nil {
			req.confirmationTimeout, err = confirmationTimeout(report, req.Placements)
			if err != nil {
				return err
			}
		}
		if requireSnapshot {
			pkg, err = d.Store.packageById(ctx, req.PackageId)
			if err != nil {
				return err
			}
			if pkg == nil {
				return refuse(CodeSourceUnreadable, "no package %q is readable by this caller", req.PackageId)
			}
			if rowString(pkg, "sourceKind") == "repo" {
				if rowString(row, "snapshotArtifactId") == "" {
					return refuse(CodeSnapshotUnavailable, "This deployment has no saved source snapshot. Analyze the source again.")
				}
				if from := strings.TrimSpace(req.FromDeploymentId); from != "" && !sameShortId(from, req.DeploymentId) {
					return refuse(CodeSnapshotUnavailable, "Confirmation must use the snapshot of the reviewed deployment.")
				}
				req.FromDeploymentId = req.DeploymentId
			}
		}
		pkg, out, prepared, err = prepareDeploymentUnclaimed(ctx, d, req)
		if err != nil {
			return err
		}
		// One write moves the row out of the parked state and refreshes its
		// execution identity/liveness. Unknown write outcomes never launch work.
		return d.Store.claimConfirmation(ctx, req.DeploymentId, selfNodeId(), d.now())
	})
	if err != nil {
		out = nil
	}
	return
}

// finishParking exposes the confirm gate only after the analysis heartbeat has
// joined. Cancel and confirmation take the same gate, so a Cancel that won it
// just before this is read here rather than left on a row nobody would read.
func finishParking(ctx context.Context, d *Deps, req DeployRequest, out *DeployOutcome) error {
	return d.Store.withDeploymentOpeningGate(ctx, req.PackageId, func(ctx context.Context) error {
		ctx = memql.ContextWithFreshRead(ctx)
		asked, err := d.Store.cancelRequestedFor(ctx, out.DeploymentId)
		if err != nil {
			return err
		}
		if !asked {
			if err := d.Store.advance(ctx, out.DeploymentId, StatusAwaitingConfirm); err != nil {
				return err
			}
		}
		if asked {
			out.AwaitingConfirm = false
			return refuse(CodeDeploymentCancelled, "You stopped this analysis. Nothing was built or published.")
		}
		return nil
	})
}

// Preserve every selected build's configured budget and its one-minute mesh
// reply allowance (workbench.RunBuild), plus each declared asset's existing
// download budget. Thirty minutes separately bounds snapshot/reanalysis and
// stage/roll/publication. This bounds detachment without making a third app
// spend the first two apps' build budgets. No client-supplied timeout is used.
func confirmationTimeout(report *Report, placements map[string]Placement) (time.Duration, error) {
	budget := 30 * time.Minute
	add := func(amount time.Duration) error {
		if amount <= 0 || budget > time.Duration(1<<63-1)-amount {
			return fmt.Errorf("confirmation duration exceeds the supported bound")
		}
		budget += amount
		return nil
	}
	for _, dep := range report.Deployables {
		if placements[dep.Name].Skip || dep.Problem != nil {
			continue
		}
		if !dep.Prebuilt {
			if err := add(time.Duration(BuildTimeoutSeconds())*time.Second + time.Minute); err != nil {
				return 0, err
			}
		}
		for range dep.Assets {
			if err := add(sourceDownloadTimeout); err != nil {
				return 0, err
			}
		}
	}
	return budget, nil
}

// unreportedConfirmationBuilds is how many builds a confirmation with no saved
// report is budgeted for at the least.
const unreportedConfirmationBuilds = 8

// confirmationTimeoutWithoutReport bounds a confirmation nothing was analysed
// for: a call confirming in one step has no report until its own run writes one,
// and the run's deadline has to be set before that. So it takes the budget of
// every app the source last declared, with room for a source never analysed or
// grown since. Too long costs only how long a stage that ignores its own deadline
// can hold the run; too short would fail a build that was going fine.
func confirmationTimeoutWithoutReport(pkg map[string]any) time.Duration {
	builds := max(len(declaredNames(pkg)), unreportedConfirmationBuilds)
	return 30*time.Minute + time.Duration(builds)*(time.Duration(BuildTimeoutSeconds())*time.Second+time.Minute)
}
