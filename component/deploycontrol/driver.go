// driver.go defines the target-agnostic deploy-driver abstraction
// (#1878, epic #1871). A deployment record carries a `provider` (#1872),
// and one driver implementation per provider knows how to apply that
// record to its concrete target. The Deploy / RollbackDeployment
// operations (deploy.go) resolve a record's provider to a driver via
// selectDriver and call apply(), so the SAME deploy action drives both a
// local Docker cluster and the Argo/Azure (AKS) path.
//
// Drivers route every side effect through the Executor boundary, so the
// operations stay unit-testable with a fake executor (no real deploy).
package deploycontrol

import (
	"context"
	"fmt"
)

// deploymentSpec is the resolved, target-agnostic description of one
// apply. Both Deploy (ship a cut record) and RollbackDeployment
// (redeploy a historical digest) build a spec from a v1:cluster:deployment
// record and hand it to the matching driver.
type deploymentSpec struct {
	deploymentID string
	version      string
	imageDigest  string
	// consoleEnv is the promote.sh environment ("staging" | "prod"),
	// derived from the record's environment enum. Empty for local /
	// development targets that the azure driver does not serve.
	consoleEnv  string
	environment string // the deployment concept's environment enum value
	provider    string
}

// deployDriver applies a deploymentSpec to a concrete target. Exactly
// one implementation per provider value; selectDriver maps the record's
// provider field onto the implementation.
type deployDriver interface {
	// provider returns the provider value this driver handles.
	provider() string
	// apply ships the spec to the target, returning the combined output
	// of whatever it ran. A non-nil error means the deploy failed (the
	// caller transitions the record to failed and surfaces err).
	apply(ctx context.Context, spec deploymentSpec) (output string, err error)
}

// azureDriver ships to AKS via the existing Argo Rollout path: it runs
// scripts/release/promote.sh, which pins the digest-authority overlay
// for the target env and lets Argo CD + the blue/green Rollout's
// prePromotion deploy-gate (deploy/rollouts/bff-rollout.yaml) reconcile
// and cut over. Because a release version pins its image digests in
// releases/<version>.yaml, redeploying a historical version through this
// same apply re-pins that exact digest -- which is precisely how
// RollbackDeployment restores a prior deploy (NOT a blue-green color
// flip; the previous color only survives the scale-down window).
type azureDriver struct{ exec Executor }

func (azureDriver) provider() string { return "azure" }

func (d azureDriver) apply(ctx context.Context, spec deploymentSpec) (string, error) {
	if spec.version == "" {
		return "", fmt.Errorf("azure driver: deployment %s has no version to promote", spec.deploymentID)
	}
	if !validEnvs[spec.consoleEnv] {
		return "", fmt.Errorf("azure driver: unsupported environment %q (want staging|production)", spec.environment)
	}
	return d.exec.RunPromote(ctx, spec.version, spec.consoleEnv)
}

// dockerLocalDriver wraps the local staging-parity Docker cluster flow
// (docker/docker-compose.cluster.yml): it brings the 2-replica mesh up
// with freshly-built images. Local images are built from the working
// tree rather than a pinned ACR digest, so spec.imageDigest is carried
// for record provenance but not consulted by the apply itself.
type dockerLocalDriver struct{ exec Executor }

func (dockerLocalDriver) provider() string { return "docker-local" }

func (d dockerLocalDriver) apply(ctx context.Context, spec deploymentSpec) (string, error) {
	return d.exec.RunDockerComposeDeploy(ctx, spec.version)
}

// selectDriver returns the deploy driver for a record's provider value.
// An empty provider is treated as "azure" (the deploymentProviderFor
// default), so legacy records with no stamped provider still ship. An
// unknown provider is a hard error -- a misconfigured record must not
// silently fall through to the wrong target.
func selectDriver(exec Executor, provider string) (deployDriver, error) {
	switch provider {
	case "azure", "":
		return azureDriver{exec: exec}, nil
	case "docker-local":
		return dockerLocalDriver{exec: exec}, nil
	default:
		return nil, fmt.Errorf("no deploy driver for provider %q (want azure|docker-local)", provider)
	}
}

// consoleEnvFor maps a deployment record's environment enum onto the
// promote.sh console env ("staging" | "prod"). development / unknown
// envs map to "" -- they are not served by the azure driver (and the
// docker-local driver ignores the console env entirely).
func consoleEnvFor(deploymentEnv string) string {
	switch deploymentEnv {
	case "production":
		return "prod"
	case "staging":
		return "staging"
	default:
		return ""
	}
}
