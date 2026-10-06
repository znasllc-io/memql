package pipelinesteps

import (
	"reflect"
	"strings"
	"testing"

	pl "github.com/znasllc-io/memql/component/pipelines"
)

func TestJobSchedulingPreservesTheDeclaredPlatform(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64", ""} {
		run := testRun()
		run.Platform = ""
		want := map[string]string{"kubernetes.io/os": "linux"}
		if arch != "" {
			run.Platform = "linux/" + arch
			want["kubernetes.io/arch"] = arch
		}
		job := mustBuild(t, testConfig(), run)
		if got := job.Spec.Template.Spec.NodeSelector; !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: %v", arch, got)
		}
	}
	run := testRun()
	run.Execution, run.Platform = pl.ExecutionNative, "darwin/arm64"
	if _, err := BuildJob(testConfig(), run, testJobName); err == nil {
		t.Fatal("native work became a Kubernetes container")
	}
	run.Execution = pl.ExecutionContainer
	if _, err := BuildJob(testConfig(), run, testJobName); err == nil {
		t.Fatal("Darwin work scheduled on Linux")
	}
}

func TestBuildPoolKeepsPlatformAndProbeOnReservedNodes(t *testing.T) {
	cfg := ConfigFromEnv(envOf(map[string]string{"MEMQL_PIPELINES_NODE_POOL": " builds "}))
	if cfg.NodePool != "builds" {
		t.Fatalf("pool = %q", cfg.NodePool)
	}
	cfg = testConfig()
	cfg.NodePool = "builds"
	for _, arch := range []string{"arm64", "amd64"} {
		run := testRun()
		run.Platform = "linux/" + arch
		pod := mustBuild(t, cfg, run).Spec.Template.Spec
		want := map[string]string{"kubernetes.io/os": "linux", "kubernetes.io/arch": arch, "memql.io/pipeline-pool": "builds"}
		if !reflect.DeepEqual(pod.NodeSelector, want) {
			t.Fatalf("selector = %v, want %v", pod.NodeSelector, want)
		}
		wantTolerations := []Toleration{{Key: "memql.io/pipeline-pool", Operator: "Equal", Value: "builds", Effect: "NoSchedule"}}
		if !reflect.DeepEqual(pod.Tolerations, wantTolerations) {
			t.Fatalf("tolerations = %+v", pod.Tolerations)
		}
		probe := BuildIsolationProbe(cfg, "probe").Spec.Template.Spec
		if !reflect.DeepEqual(probe.Tolerations, wantTolerations) || !reflect.DeepEqual(probe.NodeSelector,
			map[string]string{"kubernetes.io/os": "linux", "memql.io/pipeline-pool": "builds"}) {
			t.Fatalf("probe escaped build pool: %+v", probe)
		}
	}
	if got := mustBuild(t, testConfig(), testRun()).Spec.Template.Spec.Tolerations; len(got) != 0 {
		t.Fatalf("unconfigured pool tolerates reserved nodes: %+v", got)
	}
}

func TestInvalidBuildPoolRefusesBeforeExternalEffects(t *testing.T) {
	for _, pool := range []string{"*", "builds/one", "Uppercase", "-builds", "builds-", strings.Repeat("a", 64)} {
		t.Run(pool, func(t *testing.T) {
			cfg := testConfig()
			cfg.NodePool = pool
			if _, err := BuildJob(cfg, testRun(), testJobName); err == nil {
				t.Fatal("invalid pool produced a step Job")
			}
			// No Kubernetes client: even the isolation gate must refuse before
			// deleting an old probe or creating a new Job or Secret.
			runner := NewRunner(cfg, nil, nil, nil, nil)
			verdict, decided := runner.probeIsolation(t.Context())
			if !decided || !verdict.Inconclusive || verdict.Isolated || !strings.Contains(verdict.Detail, "MEMQL_PIPELINES_NODE_POOL") {
				t.Fatalf("invalid pool proof = %+v", verdict)
			}
			if runner.Readiness().Available {
				t.Fatal("invalid pool advertised a ready runner")
			}
		})
	}
}
