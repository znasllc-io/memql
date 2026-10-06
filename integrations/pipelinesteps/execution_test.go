package pipelinesteps

import (
	pl "github.com/znasllc-io/memql/component/pipelines"
	"reflect"
	"testing"
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
