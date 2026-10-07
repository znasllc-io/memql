package overlays

import (
	"reflect"
	"testing"
)

func TestLongPipelineProfileIsExplicitAndPreservesOtherConfiguration(t *testing.T) {
	const key = "MEMQL_PIPELINES_RUN_MAX_MINUTES"
	for _, overlay := range instanceOverlays {
		for name, deployment := range parseDeployments(t, render(t, overlay)) {
			for _, container := range deployment.Spec.Template.Spec.Containers {
				for _, variable := range container.Env {
					if variable.Name == key {
						t.Fatalf("%s/%s silently opts all runs into the longer budget", overlay, name)
					}
				}
			}
		}
	}
	before := parseDeployments(t, render(t, "local"))
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
				if variable.Name != key {
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
