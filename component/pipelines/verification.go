package pipelines

import (
	"encoding/hex"
	"strings"
)

func digestPinnedImage(image string) bool {
	name, digest, ok := strings.Cut(image, "@sha256:")
	if !ok || name == "" || len(digest) != 64 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func validateVerification(spec *Spec, step StepSpec, scope string) *Refusal {
	image := step.Image
	if image == "" {
		image = spec.Image
	}
	if step.ImageBuild != nil || step.SARIF != nil || step.RepositoryChange != nil || step.GitHubReadToken || len(step.Secrets) > 0 || step.ImagePullSecret != "" || len(step.Artifacts) > 0 || PlacementOf(step) != "cluster" || ExecutionOf(step.Execution) != "container" || !digestPinnedImage(image) {
		return Refuse(CodeStepInvalid, scope, "Reusable verification requires an immutable container image and no artifacts, secrets, repository tokens or external effects.")
	}
	for _, name := range step.Services {
		if !digestPinnedImage(spec.Services[name].Image) {
			return Refuse(CodeStepInvalid, scope, "Reusable verification service %s must use an immutable image digest.", name)
		}
	}
	return nil
}
