package packages

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	// One original run and two retries per source revision/policy epoch.
	// The ten-minute poll supplies the next opportunity; retries never loop
	// inside one invocation or erase the original failed deployment.
	autoDeploymentAttemptLimit = 3
	autoDeploymentRetryDelay   = 10 * time.Minute
)

// nextAutoDeployment resolves at most three exact IDs under the package
// owner's actor. Deterministic retry IDs converge on the same attempt, whose
// creation is serialized by openDeployment's cross-replica read/open gate.
// The existing package-wide live-run check runs before this lookup.
func (d *Deps) nextAutoDeployment(ctx context.Context, pkg map[string]any, version string) (id, from string, err error) {
	base := policyDeploymentId(pkg, version)
	for attempt := 1; attempt <= autoDeploymentAttemptLimit; attempt++ {
		id = base
		if attempt > 1 {
			id = fmt.Sprintf("%s-retry-%d", base, attempt)
		}
		prior, readErr := d.Store.deploymentById(ctx, id)
		if readErr != nil {
			return "", "", readErr
		}
		if prior == nil {
			return id, from, nil
		}
		if !rowBool(prior, "automatic") || !sameShortId(rowString(prior, "packageId"), rowString(pkg, "id")) ||
			rowString(prior, "sourceVersion") != version {
			return "", "", nil
		}
		if !retryableAutoInfrastructureFailure(prior) {
			retryable, err := d.retryableInheritedBindingFailure(ctx, pkg, prior)
			if err != nil || !retryable {
				return "", "", err
			}
		}
		// Retrying means the same bytes. A missing snapshot needs a person's
		// fresh deploy, never an automatic fetch of a possibly moved branch.
		if rowString(pkg, "sourceKind") == "repo" && rowString(prior, "snapshotArtifactId") == "" {
			return "", "", nil
		}
		finished, parseErr := time.Parse(time.RFC3339Nano, rowString(prior, "finishedAt"))
		if parseErr != nil || d.now().Sub(finished) < autoDeploymentRetryDelay {
			return "", "", nil
		}
		from = id
	}
	// Leave the failure visible and updateAvailable set. A person can use
	// Retry after repairing the cluster; further polls create no more runs.
	d.log().Warn("packages: automatic infrastructure retries exhausted; repair the deployment failure and retry the deployment",
		"component", "packages.autodeploy", "package", rowString(pkg, "id"), "deployment", id,
		"attemptLimit", autoDeploymentAttemptLimit)
	return "", "", nil
}

func retryableAutoInfrastructureFailure(row map[string]any) bool {
	switch rowString(row, "status") {
	case StatusFailed, StatusRefused:
	default:
		// A cancellation, a request for confirmation, success, or an unknown
		// lifecycle state is never permission to deploy again.
		return false
	}
	problem, ok := row["error"].(map[string]any)
	if !ok {
		return false
	}
	switch rowString(problem, "code") {
	case CodeNoWorkbenchPeer:
		return true
	case CodeDeployableBuildFailed:
		// Older engines recorded process-start failures with the same code
		// as a source command's nonzero exit. Match only the runtime's start
		// failure, including the distroless /bin/sh failure. Broken source,
		// authorization, timeouts and compilation failures stay terminal.
		return strings.HasPrefix(rowString(problem, "message"), "the build command could not be started:")
	default:
		return false
	}
}
