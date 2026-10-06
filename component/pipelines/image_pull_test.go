package pipelines

import "testing"

func TestImagePullCredentialRequiresOwnerConsentAndNeverBecomesEnvironment(t *testing.T) {
	spec := &Spec{Image: "registry.example/toolchain@sha256:test", Stages: []StageSpec{{Name: "checks", Steps: []StepSpec{{Name: "unit", Run: "make test", ImagePullSecret: "REGISTRY_AUTH"}}}}}
	input := CompileInput{Mode: ModeFull, Event: EventPush, Compute: ComputeCluster}
	if _, refusal := Compile(spec, input); refusal == nil || refusal.Code != CodeSecretNotAllowed {
		t.Fatalf("unconsented image credential: %v", refusal)
	}
	input.AllowedSecrets = []string{"REGISTRY_AUTH"}
	plan, refusal := Compile(spec, input)
	if refusal != nil {
		t.Fatal(refusal)
	}
	step := plan.Steps()[0]
	if step.ImagePullSecret != "REGISTRY_AUTH" {
		t.Fatal("credential reference lost in compilation")
	}
	req := StepRequest{Step: step, Secrets: map[string]string{"REGISTRY_AUTH": "private", "ENV_SECRET": "command"}}
	if _, exists := req.Environment()["REGISTRY_AUTH"]; exists || req.Environment()["ENV_SECRET"] != "command" {
		t.Fatal("credential exposed to command")
	}
	for _, name := range []string{"MEMQL_REGISTRY", "bad/name"} {
		spec.Stages[0].Steps[0].ImagePullSecret = name
		if refusal := Validate(spec); refusal == nil || refusal.Code != CodeSecretInvalid {
			t.Fatalf("invalid credential reference accepted: %v", refusal)
		}
	}
	spec.Stages[0].Steps[0].ImagePullSecret = "REGISTRY_AUTH"
	spec.Stages[0].Steps[0].Secrets = []string{"REGISTRY_AUTH"}
	if refusal := Validate(spec); refusal == nil {
		t.Fatal("dual-use credential accepted")
	}
	spec.Stages[0].Steps[0].Secrets = nil
	spec.Stages[0].Steps[0].Placement = PlacementFleet
	if refusal := Validate(spec); refusal == nil {
		t.Fatal("fleet credential accepted")
	}
}
