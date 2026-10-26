package pipelines

import (
	"fmt"
	"strings"
)

const (
	ExecutionContainer = "container"
	ExecutionNative    = "native"
	PlacementCluster   = "cluster"
	PlacementFleet     = "fleet"
	// Pool preserves container semantics while the installed policy selects
	// between compatible, consented Fleet capacity and admitted cluster work.
	PlacementPool = "pool"
)

// ExecutionOf resolves the manifest default without changing runner semantics.
func ExecutionOf(value string) string {
	if value == "" {
		return ExecutionContainer
	}
	return value
}

// PlatformOf resolves a step override over the pipeline default.
func PlatformOf(spec *Spec, step StepSpec) string {
	if step.Platform != "" {
		return step.Platform
	}
	return spec.Platform
}

// cachesFor resolves an explicit step override, including an empty list.
func cachesFor(spec *Spec, step StepSpec) []string {
	if step.Caches != nil {
		return *step.Caches
	}
	return spec.Caches
}

// PlacementOf resolves explicit placement or the host execution requirement.
func PlacementOf(step StepSpec) string {
	if step.Placement != "" {
		return step.Placement
	}
	if step.Execution == ExecutionNative || len(stepNeeds(step)) > 0 {
		return PlacementFleet
	}
	return PlacementCluster
}
func (s Step) RequiresFleet() bool {
	return s.Placement == PlacementFleet || s.Execution == ExecutionNative || len(s.Needs) > 0
}

// CheckPoolStep holds the portable contract at the execution seam as well as
// during manifest compilation. Pool preference never changes runtime, grants
// repository credentials, or turns a container job into a native command.
func CheckPoolStep(step Step) error {
	if step.Kind != StepCommand || step.Execution != ExecutionContainer || len(step.Needs) != 0 || (step.ImageBuild != nil && !step.ImageBuild.VerificationOnly) || step.GitHubReadToken || step.ImagePullSecret != "" || step.CheckoutHistory != "" {
		return fmt.Errorf("Pool placement requires a portable container command without host needs, publishable image builds or cluster-only credentials and checkout history.")
	}
	if step.ImageBuild != nil {
		if err := CheckImageVerificationStep(step); err != nil {
			return err
		}
	}
	return CheckExecution(step.Execution, step.Platform, true)
}

// Host needs do not pass through a container boundary. A fleet container needs
// placement: fleet; docker as a need means the command itself needs a daemon.
func CheckExecutionNeeds(execution string, needs []string) error {
	if execution == ExecutionContainer && len(needs) > 0 {
		return fmt.Errorf("Container steps cannot access host needs; use native execution for host tools or placement: fleet for a Docker container.")
	}
	return nil
}

// Runtime semantics are separate from placement. A Docker-capable host still
// runs a container step in its image; it cannot reinterpret it as native work.
func validateStepRuntime(spec *Spec, step StepSpec, scope string) *Refusal {
	execution, platform := ExecutionOf(step.Execution), PlatformOf(spec, step)
	if step.GitHubReadToken && (execution != ExecutionContainer || PlacementOf(step) != PlacementCluster) {
		return Refuse(CodeStepInvalid, scope, "githubReadToken requires a cluster container command or image build.")
	}
	if step.ImageBuild != nil {
		if err := CheckImageBuild(step.ImageBuild, platform); err != nil {
			return Refuse(CodeStepInvalid, scope, "%s", err)
		}
		if step.MemoryMiB < 512 || step.Run != "" || step.Image != "" || len(step.Services) != 0 || len(cachesFor(spec, step)) != 0 ||
			len(step.Secrets) != 0 || step.ImagePullSecret != "" || len(step.Artifacts) != 0 || step.Packages != "" ||
			execution != ExecutionContainer || (!step.ImageBuild.VerificationOnly && PlacementOf(step) != PlacementCluster) || len(stepNeeds(step)) != 0 {
			return Refuse(CodeStepInvalid, scope, "imageBuild requires at least 512 MiB and container execution without run, image, services, caches, secrets, artifacts or package selection; publishable builds require cluster placement.")
		}
		if step.ImageBuild.VerificationOnly && (step.GitHubReadToken || step.CheckoutHistory != "" || (PlacementOf(step) != PlacementCluster && platform != "linux/amd64")) {
			return Refuse(CodeStepInvalid, scope, "Image verification admits no checkout credentials/history and requires native Linux amd64 for Fleet placement.")
		}
	}
	if step.Image != "" && (execution == ExecutionNative || strings.ContainsFunc(step.Image, isSpaceOrControl)) {
		return Refuse(CodeStepInvalid, scope, "A step image must be a container image reference without whitespace; native steps cannot name an image.")
	}
	placement := PlacementOf(step)
	if err := CheckCheckoutHistory(step.CheckoutHistory, placement == PlacementFleet); err != nil {
		return Refuse(CodeStepInvalid, scope, "%s", err)
	}
	if err := CheckCPUMilli(step.CPUMilli, execution == ExecutionNative); err != nil {
		return Refuse(CodeStepInvalid, scope, "%s", err)
	}
	if err := CheckMemoryMiB(step.MemoryMiB, execution == ExecutionNative); err != nil {
		return Refuse(CodeStepInvalid, scope, "%s", err)
	}
	if step.ImagePullSecret != "" && (placement != PlacementCluster || execution != ExecutionContainer) {
		return Refuse(CodeStepInvalid, scope, "imagePullSecret requires a cluster container step.")
	}
	for _, name := range step.Secrets {
		if name == step.ImagePullSecret {
			return Refuse(CodeSecretInvalid, scope, "An image pull credential cannot also be a command environment secret.")
		}
	}
	if placement != PlacementCluster && placement != PlacementFleet && placement != PlacementPool {
		return Refuse(CodeStepInvalid, scope, "placement must be cluster, fleet or pool.")
	}
	if placement == PlacementPool && (execution != ExecutionContainer || len(stepNeeds(step)) != 0 || (step.ImageBuild != nil && !step.ImageBuild.VerificationOnly) || step.CheckoutHistory != "" || step.GitHubReadToken || step.ImagePullSecret != "") {
		return Refuse(CodeStepInvalid, scope, "Pool placement requires a portable container command without host needs, publishable image builds or cluster-only credentials and checkout history.")
	}
	if placement == PlacementCluster && (execution == ExecutionNative || len(stepNeeds(step)) > 0) {
		return Refuse(CodeStepInvalid, scope, "Native execution and host needs require fleet placement.")
	}
	if err := CheckExecutionNeeds(execution, stepNeeds(step)); err != nil {
		return Refuse(CodeStepInvalid, scope, "%s", err)
	}
	if err := CheckExecution(execution, platform, placement == PlacementFleet || placement == PlacementPool); err != nil {
		return Refuse(CodeStepInvalid, scope, "%s", err)
	}
	if execution == ExecutionNative && (len(step.Services) > 0 || len(cachesFor(spec, step)) > 0) {
		return Refuse(CodeStepInvalid, scope, "Native steps do not support container services or declared shared caches.")
	}
	return nil
}

// CheckExecution validates the concrete runner contract, including internal forwards.
func CheckExecution(execution, platform string, fleet bool) error {
	if execution != ExecutionContainer && execution != ExecutionNative {
		return fmt.Errorf("execution must be container or native.")
	}
	if platform != "" && platform != "linux/amd64" && platform != "linux/arm64" && platform != "darwin/amd64" && platform != "darwin/arm64" {
		return fmt.Errorf("platform must name linux or darwin and amd64 or arm64.")
	}
	if execution == ExecutionContainer && platform != "" && platform != "linux/amd64" && platform != "linux/arm64" {
		return fmt.Errorf("Container steps require a Linux platform.")
	}
	if fleet && platform == "" {
		return fmt.Errorf("A fleet step must declare its OS/architecture in platform.")
	}

	return nil
}

// CheckMemoryMiB checks the portable shape. The runner separately applies
// its operator-owned maximum before creating a cluster or Fleet container.
// Native execution cannot promise the container's hard limit.
func CheckMemoryMiB(memory int, native bool) error {
	if memory == 0 {
		return nil
	}
	if memory < 128 || memory > 1048576 {
		return fmt.Errorf("memoryMiB must be zero or a whole number from 128 through 1048576.")
	}
	if native {
		return fmt.Errorf("memoryMiB requires container execution.")
	}
	return nil
}

// CheckCPUMilli checks the portable shape. The runner separately applies
// its operator-owned maximum before creating a cluster or Fleet container.
func CheckCPUMilli(cpu int, native bool) error {
	if cpu == 0 {
		return nil
	}
	if cpu < 1 || cpu > 256000 {
		return fmt.Errorf("cpuMilli must be zero or a whole number from 1 through 256000.")
	}
	if native {
		return fmt.Errorf("cpuMilli requires container execution.")
	}
	return nil
}

// CheckCheckoutHistory keeps complete-history acquisition in the credentialed
// cluster checkout. Fleet runners must refuse until their contract supports it.
func CheckCheckoutHistory(history string, fleet bool) error {
	switch history {
	case "":
		return nil
	case "full":
		if fleet {
			return fmt.Errorf("checkoutHistory: full requires cluster container execution.")
		}
		return nil
	default:
		return fmt.Errorf("checkoutHistory must be omitted or full.")
	}
}
