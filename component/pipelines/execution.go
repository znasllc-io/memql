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
	if step.Image != "" && (execution == ExecutionNative || strings.ContainsFunc(step.Image, isSpaceOrControl)) {
		return Refuse(CodeStepInvalid, scope, "A step image must be a container image reference without whitespace; native steps cannot name an image.")
	}
	placement := PlacementOf(step)
	if err := CheckMemoryMiB(step.MemoryMiB, placement == PlacementFleet); err != nil {
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
	if placement != PlacementCluster && placement != PlacementFleet {
		return Refuse(CodeStepInvalid, scope, "placement must be cluster or fleet.")
	}
	if placement == PlacementCluster && (execution == ExecutionNative || len(stepNeeds(step)) > 0) {
		return Refuse(CodeStepInvalid, scope, "Native execution and host needs require fleet placement.")
	}
	if err := CheckExecutionNeeds(execution, stepNeeds(step)); err != nil {
		return Refuse(CodeStepInvalid, scope, "%s", err)
	}
	if err := CheckExecution(execution, platform, placement == PlacementFleet); err != nil {
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
// its operator-owned maximum before creating any Kubernetes resource.
func CheckMemoryMiB(memory int, fleet bool) error {
	if memory == 0 {
		return nil
	}
	if memory < 128 || memory > 1048576 {
		return fmt.Errorf("memoryMiB must be zero or a whole number from 128 through 1048576.")
	}
	if fleet {
		return fmt.Errorf("memoryMiB requires cluster container execution.")
	}
	return nil
}
