package pipelines

import (
	"path"
	"strings"
	"time"
)

func validateSARIFStep(spec *Spec, stage string, step StepSpec) *Refusal {
	scope := stage + "/" + step.Name
	r := step.SARIF
	if r == nil || r.Path == "" || len(r.Path) > 1024 || path.Clean(r.Path) != r.Path ||
		strings.HasPrefix(r.Path, "/") || strings.HasPrefix(r.Path, "../") ||
		strings.ContainsAny(r.Path, "\\\x00\r\n*?[]") || !strings.HasSuffix(r.Path, ".sarif") {
		return Refuse(CodeStepInvalid, scope, "sarif requires one exact repository-relative .sarif artifact path.")
	}
	if step.Run != "" || step.ImageBuild != nil || step.Image != "" || step.Execution != "" || step.Placement != "" ||
		step.Platform != "" || step.CPUMilli != 0 || step.MemoryMiB != 0 || step.Packages != "" || step.Only != "" ||
		step.Shards != 0 || step.When != nil || len(step.Needs) > 0 || len(step.Services) > 0 || step.Caches != nil ||
		len(step.Secrets) > 0 || step.ImagePullSecret != "" || len(step.Artifacts) > 0 {
		return Refuse(CodeStepInvalid, scope, "sarif accepts only name, producer step, artifact path and timeout; the driver owns its execution and current source grant.")
	}
	if step.Timeout != "" {
		d, err := time.ParseDuration(step.Timeout)
		if err != nil || d < time.Minute || d > 10*time.Minute {
			return Refuse(CodeStepInvalid, scope, "SARIF delivery timeout must be between 1m and 10m.")
		}
	}
	for _, prior := range spec.Stages {
		if prior.Name == stage {
			break
		}
		for _, producer := range prior.Steps {
			if StepKey(prior.Name, producer.Name) == r.Step && producer.SARIF == nil && producer.ImageBuild == nil &&
				producer.Shards <= 1 && len(producer.Artifacts) > 0 {
				return nil
			}
		}
	}
	return Refuse(CodeStepInvalid, scope, "sarif.step must name an unsharded scanner in an earlier stage that retains artifacts.")
}
