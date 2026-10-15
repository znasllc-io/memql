package overlays

import (
	"reflect"
	"testing"
)

// The long-run profile (components/pipelines-long-runs) sets
// MEMQL_PIPELINES_RUN_MAX_MINUTES=480 on the agent and the workbench and
// nothing else. The local overlay composes it ON PURPOSE: the engine's own
// memql-package.yaml declares a 6-hour step, and the local cluster runs that
// package (design record 2026-10-11-pipelines-local-green-and-fast, section 4).
// The cloud overlays do not; a cloud install opts in by composing the
// component, as components/examples/pipelines-long-runs shows.
const longRunKey = "MEMQL_PIPELINES_RUN_MAX_MINUTES"

func longRunValues(t *testing.T, overlay string) map[string][]string {
	t.Helper()
	values := map[string][]string{}
	for name, deployment := range parseDeployments(t, render(t, overlay)) {
		for _, container := range deployment.Spec.Template.Spec.Containers {
			for _, variable := range container.Env {
				if variable.Name == longRunKey {
					values[name] = append(values[name], container.Name+"="+variable.Value)
				}
			}
		}
	}
	return values
}

func TestOnlyTheLocalOverlayOptsIntoLongRuns(t *testing.T) {
	want := map[string][]string{"agent": {"agent=480"}, "workbench": {"workbench=480"}}
	for _, overlay := range instanceOverlays {
		got := longRunValues(t, overlay)
		if overlay == "local" {
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("local carries %v, want exactly %v", got, want)
			}
			continue
		}
		if len(got) != 0 {
			t.Fatalf("%s silently opts all runs into the longer budget: %v", overlay, got)
		}
	}
}

func TestLongPipelineProfileIsExplicitAndPreservesOtherConfiguration(t *testing.T) {
	before := parseDeployments(t, render(t, "cloud"))
	after := parseDeployments(t, render(t, "../components/examples/pipelines-long-runs"))
	if len(before) != len(after) || len(before) < len(meshDeployments) {
		t.Fatal("long-run profile changed or lost the deployment inventory")
	}
	for name, deployment := range after {
		count := 0
		for i := range deployment.Spec.Template.Spec.Containers {
			container := &deployment.Spec.Template.Spec.Containers[i]
			for j := len(container.Env) - 1; j >= 0; j-- {
				variable := container.Env[j]
				if variable.Name != longRunKey {
					continue
				}
				count++
				if variable.Value != "480" || container.Name != name {
					t.Fatalf("%s has an incorrect long-run deadline or container", name)
				}
				container.Env = append(container.Env[:j], container.Env[j+1:]...)
			}
		}
		want := 0
		if name == "agent" || name == "workbench" {
			want = 1
		}
		if count != want {
			t.Fatalf("%s: got %d deadline values, want %d", name, count, want)
		}
		if !reflect.DeepEqual(deployment, before[name]) {
			t.Fatalf("%s: profile changed other container environment or secret/config wiring", name)
		}
	}
}
