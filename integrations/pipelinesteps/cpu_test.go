package pipelinesteps

import (
	"encoding/json"
	"strconv"
	"testing"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

func TestCommandCPUIsReservedAcrossForwardWithoutChangingOtherContainers(t *testing.T) {
	for _, cpu := range []int{1, 250, 4000} {
		for _, memory := range []int{0, 6144} {
			req := exRequest()
			req.Step.CPUMilli, req.Step.MemoryMiB = cpu, memory
			forward := stepRunFor(req, 600, pl.CodeStepTimeout)
			// Workbench sees only the wire payload, without the agent's request.
			encoded, err := json.Marshal(forward)
			if err != nil {
				t.Fatal(err)
			}
			var received StepRun
			if err := json.Unmarshal(encoded, &received); err != nil {
				t.Fatal(err)
			}
			job := mustBuild(t, testConfig(), received)
			resources := job.Spec.Template.Spec.Containers[0].Resources
			want := strconv.Itoa(cpu) + "m"
			if resources == nil || resources.Requests["cpu"] != want || resources.Limits["cpu"] != want {
				t.Fatalf("CPU reservation lost across forward: %+v", resources)
			}
			fields := 1
			if memory != 0 {
				fields++
				if resources.Requests["memory"] != "6144Mi" || resources.Limits["memory"] != "6144Mi" {
					t.Fatalf("CPU changed explicit memory reservation: %+v", resources)
				}
			}
			if len(resources.Requests) != fields || len(resources.Limits) != fields {
				t.Fatalf("unspecified resources lost operator defaults: %+v", resources)
			}
			for _, init := range job.Spec.Template.Spec.InitContainers {
				if init.Resources != nil {
					t.Fatal("command CPU changed clone/cache/service sizing")
				}
			}
		}
	}
}

func TestCPUReservationRefusedBeforeDispatchOrJobCreation(t *testing.T) {
	for _, cpu := range []int{-1, 4001, 256001} {
		run := testRun()
		run.CPUMilli = cpu
		if job, err := BuildJob(testConfig(), run, testJobName); err == nil || job.Kind != "" {
			t.Fatalf("invalid or oversized CPU accepted: %d", cpu)
		}
	}
	run, cfg := testRun(), testConfig()
	run.CPUMilli, cfg.StepCPUMaxMilli = 4000, 2000
	if _, err := BuildJob(cfg, run, testJobName); err == nil {
		t.Fatal("operator CPU cap ignored")
	}
	req := exRequest()
	req.Step.CPUMilli, req.Step.Placement = 4000, pl.PlacementFleet
	if _, refused := refuseStep(req); !refused {
		t.Fatal("fleet CPU reservation silently ignored")
	}
}

func TestStepCPUOperatorConfigurationRemainsBounded(t *testing.T) {
	for raw, want := range map[string]int{"": 4000, "no": 4000, "0": 4000, "-1": 4000, "0.5": 4000, "1": 1, "2000": 2000, "999999999": 256000} {
		cfg := ConfigFromEnv(envOf(map[string]string{"MEMQL_PIPELINES_STEP_CPU_MAX_MILLI": raw}))
		if cfg.StepCPUMaxMilli != want {
			t.Fatalf("cap %q -> %d; want %d", raw, cfg.StepCPUMaxMilli, want)
		}
	}
}
