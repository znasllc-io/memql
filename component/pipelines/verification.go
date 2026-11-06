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

// validateVerification admits a step whose pass may be reused as evidence by
// another run: an immutable container command with no artifacts, secrets,
// repository tokens or external effects, so what it proves is a function of its
// source and image alone.
//
// WHERE IT MAY RUN (the owner's decision, 2026-11-06). Cluster, or the
// Fleet-first pool. A pool step's pass that ran on one of the owner's consented
// machines counts as evidence between pushes (carry-forward, same-commit reuse),
// and a release never stands on it: a release reuses only a pass the cluster
// produced (InspectReusedReleaseReceipt reads the attempt that passed), and a
// release runs its own pool steps on the cluster
// (component/pipelinerun.executionStep). So the Fleet speeds up the runs that
// happen on every push, while everything a release records is still produced
// on the cluster. A fleet step stays refused: it is native or needs a host, so
// its pass is not a function of the image.
func validateVerification(spec *Spec, step StepSpec, scope string) *Refusal {
	image := step.Image
	if image == "" {
		image = spec.Image
	}
	placement := PlacementOf(step)
	if step.ImageBuild != nil || step.SARIF != nil || step.RepositoryChange != nil || step.GitHubReadToken || len(step.Secrets) > 0 || step.ImagePullSecret != "" || len(step.Artifacts) > 0 || (placement != PlacementCluster && placement != PlacementPool) || ExecutionOf(step.Execution) != ExecutionContainer || !digestPinnedImage(image) {
		return Refuse(CodeStepInvalid, scope, "Reusable verification requires an immutable container image on the cluster or the pool, and no artifacts, secrets, repository tokens or external effects.")
	}
	for _, name := range step.Services {
		if !digestPinnedImage(spec.Services[name].Image) {
			return Refuse(CodeStepInvalid, scope, "Reusable verification service %s must use an immutable image digest.", name)
		}
	}
	return nil
}
