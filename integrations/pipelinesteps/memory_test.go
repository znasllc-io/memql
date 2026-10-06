package pipelinesteps

import (
	"testing"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

func TestCommandMemoryIsReservedBoundedAndIsolatedFromSidecars(t *testing.T) {
	for _, mib := range []int{128, 6144, 8192} {
		run := testRun()
		run.MemoryMiB = mib
		job := mustBuild(t, testConfig(), run)
		resources := job.Spec.Template.Spec.Containers[0].Resources
		if resources == nil || len(resources.Requests) != 1 || len(resources.Limits) != 1 || resources.Requests["memory"] != resources.Limits["memory"] {
			t.Fatalf("memory reservation missing or altered other resources: %+v", resources)
		}
		for _, init := range job.Spec.Template.Spec.InitContainers {
			if init.Resources != nil {
				t.Fatal("command memory changed clone/cache/service sizing")
			}
		}
	}
	if resources := mustBuild(t, testConfig(), testRun()).Spec.Template.Spec.Containers[0].Resources; resources != nil {
		t.Fatal("unspecified resources lost operator defaults")
	}
	for _, mib := range []int{-1, 1, 127, 8193, 1048577} {
		run := testRun()
		run.MemoryMiB = mib
		if job, err := BuildJob(testConfig(), run, testJobName); err == nil || job.Kind != "" {
			t.Fatalf("invalid/oversized memory accepted: %d", mib)
		}
	}
	run, cfg := testRun(), testConfig()
	run.MemoryMiB, cfg.StepMemoryMaxMiB = 6144, 4096
	if _, err := BuildJob(cfg, run, testJobName); err == nil {
		t.Fatal("operator cap ignored")
	}
	req := exRequest()
	req.Step.MemoryMiB = 6144
	if got := stepRunFor(req, 600, pl.CodeStepTimeout).MemoryMiB; got != 6144 {
		t.Fatal("memory lost across agent/runner forward")
	}
	req.Step.Placement = pl.PlacementFleet
	if _, refused := refuseStep(req); !refused {
		t.Fatal("unsupported fleet sizing silently ignored")
	}
}

func TestStepMemoryOperatorConfigurationRemainsBounded(t *testing.T) {
	for raw, want := range map[string]int{"": 8192, "no": 8192, "0": 8192, "-1": 8192, "1": 128, "6144": 6144, "999999999": 1048576} {
		cfg := ConfigFromEnv(envOf(map[string]string{"MEMQL_PIPELINES_STEP_MEMORY_MAX_MIB": raw}))
		if cfg.StepMemoryMaxMiB != want {
			t.Fatalf("cap %q -> %d; want %d", raw, cfg.StepMemoryMaxMiB, want)
		}
	}
}
