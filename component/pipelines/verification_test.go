package pipelines

import (
	"strings"
	"testing"
)

func TestVerificationEnvironmentDoesNotExposeEventOrRunIdentity(t *testing.T) {
	r := StepRequest{RunID: "run", WorkRunID: "work", SHA: "commit", BaseSHA: "base", Mode: ModeFull, Event: EventRelease, Version: "v2", Step: Step{Verification: true, Packages: []string{"module/pkg"}}}
	env := r.Environment()
	for _, key := range []string{"MEMQL_RUN_ID", "MEMQL_WORK_RUN_ID", "MEMQL_BASE_SHA", "MEMQL_MODE", "MEMQL_EVENT", "MEMQL_VERSION"} {
		if _, ok := env[key]; ok {
			t.Fatalf("event-specific input leaked: %s", key)
		}
	}
	if env["MEMQL_SHA"] != "commit" || env["MEMQL_PACKAGES"] != "module/pkg" || env["MEMQL_VERIFICATION"] != "true" {
		t.Fatalf("missing declared inputs: %v", env)
	}
}

func TestReusableVerificationRefusesMutableToolchainsAndSideEffects(t *testing.T) {
	base := StepSpec{Name: "unit", Run: "go test ./unit", Verification: true, Placement: "cluster"}
	spec := &Spec{Image: "registry/toolchain@sha256:" + strings.Repeat("a", 64)}
	if r := validateVerification(spec, base, "checks/unit"); r != nil {
		t.Fatal(r)
	}
	for _, mutate := range []func(*StepSpec){
		func(s *StepSpec) { s.Image = "registry/toolchain:latest" },
		func(s *StepSpec) { s.Secrets = []string{"token"} },
		func(s *StepSpec) { s.GitHubReadToken = true },
		func(s *StepSpec) { s.Artifacts = []string{"result.json"} },
		func(s *StepSpec) { s.Placement = "fleet" },
	} {
		s := base
		mutate(&s)
		if r := validateVerification(spec, s, "checks/unit"); r == nil {
			t.Fatalf("unsafe evidence contract accepted: %+v", s)
		}
	}
}
