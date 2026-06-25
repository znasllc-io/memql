// driver.go holds the provider allow-list + environment mapping that the
// deploy-control actions (deploy.go) share.
//
// The synchronous Go apply -- the deployDriver/azureDriver abstraction that
// ran scripts/release/promote.sh inline and transitioned the record to its
// terminal status -- was RETIRED in #2115 step 6. The deploy-as-a-pack
// automations (examples/deploypack: driveDeploymentInProgress /
// recordReconciledState) now own promote + the terminal transition off the
// deployment-record CDC edge, through the SAME Executor effects. So
// Deploy / RollbackDeployment only validate the record and kick off the
// lifecycle (transition to in_progress); they never apply here.
//
// What remains is the provider allow-list (validateDeploymentProvider) and
// the promote.sh env mapping (ConsoleEnvFor) -- both still needed: the
// kick-off validates the provider before transitioning, and the pack's
// runPromote effect reuses ConsoleEnvFor so the env mapping lives in ONE
// place. The Executor effect boundary (executor.go) + the proto/gRPC surface
// are preserved.
package deploycontrol

import "fmt"

// validateDeploymentProvider rejects a deployment record whose provider is
// not a supported deploy target. An empty provider is treated as "azure"
// (the deploymentProviderFor default), so legacy records with no stamped
// provider still ship. The retired "docker-local" provider is rejected with
// a pointer to `make up`; any other value is an unknown-provider error. The
// only live deploy target is the Argo/Azure (AKS) path; local clusters are
// operated via `make up` (k3d + ArgoCD), not the deploy console.
func validateDeploymentProvider(provider string) error {
	switch provider {
	case "azure", "":
		return nil
	case "docker-local":
		return fmt.Errorf("provider %q is no longer supported: local clusters are operated via `make up` (k3d + ArgoCD), not the deploy console", provider)
	default:
		return fmt.Errorf("no deploy driver for provider %q (want azure)", provider)
	}
}

// ConsoleEnvFor maps a deployment record's environment enum (or an
// already-console env) onto the promote.sh console env ("staging" |
// "prod"). development / unknown envs map to "" -- they are not served by
// the azure deploy target. Exported (Epic 2 / #2096) so the deploy PACK's
// runPromote effect normalizes the deployment.environment enum the same way
// the console does, keeping the env mapping in ONE place instead of
// duplicating it in the DSL automation.
func ConsoleEnvFor(deploymentEnv string) string {
	switch deploymentEnv {
	case "production", "prod":
		return "prod"
	case "staging":
		return "staging"
	default:
		return ""
	}
}
