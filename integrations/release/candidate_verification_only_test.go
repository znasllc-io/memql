package release

import (
	"strings"
	"testing"
)

func TestAssemblyRefusesVerificationProducerBeforeReadingAnyArtifact(t *testing.T) {
	for _, kind := range []string{"oci", "file"} {
		t.Run(kind, func(t *testing.T) {
			plan := assemblyTestPlan()
			plan.Components[0].Artifacts[0].Kind = kind
			s := candidateAssemblyScope{plan: plan, resolvedRuns: map[string]candidateRunPlan{"engine_arm64": {VerificationOnly: map[string]bool{"build.image": true}}}}
			// A nil library is intentional: a verification receipt must be refused
			// before discovery or pinning, even if selected as an ordinary file.
			_, err := s.readArtifact(ownerCtx(), map[string]any{"componentName": "engine", "artifactName": "image"})
			if err == nil || !strings.Contains(err.Error(), "verification") {
				t.Fatal("verification artifact accepted", err)
			}
		})
	}
}
