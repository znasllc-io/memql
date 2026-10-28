package packages

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/memql"
)

// StartConfirmation accepts one parked attempt before returning. The caller's
// authority stays on the context; only its transport cancellation is detached.
// Builds retain their existing per-build deadlines. Only a persisted native
// self-roll checkpoint can continue on another replica; earlier node loss
// remains abandoned and never replays a build.
func StartConfirmation(ctx context.Context, d *Deps, req DeployRequest) (*DeployOutcome, error) {
	if !req.Confirmed || req.Automatic || strings.TrimSpace(req.DeploymentId) == "" {
		return nil, fmt.Errorf("background confirmation requires a parked deployment ID and confirm:true")
	}
	pkg, out, prepared, err := prepareConfirmation(ctx, d, req, true)
	if err != nil {
		return nil, err
	}
	accepted := *out
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), prepared.confirmationTimeout)
	go func() {
		defer cancel()
		_, _ = executeDeployment(runCtx, d, prepared, pkg, out)
	}()
	return &accepted, nil
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
		// The new asynchronous route must accept exactly the report the person
		// reviewed. Keep the legacy synchronous no-snapshot compatibility path.
		if requireSnapshot {
			report := reportFromRow(row)
			if report == nil || !report.OK {
				return refuse(CodeSnapshotUnavailable, "This deployment has no saved analysis report. Analyze the source again.")
			}
			req.confirmationTimeout, err = confirmationTimeout(report, req.Placements)
			if err != nil {
				return err
			}
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
