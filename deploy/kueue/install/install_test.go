package install

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

func TestPinnedControllerNeverManagesCustomerWorkloads(t *testing.T) {
	raw, err := os.ReadFile("upstream.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(raw)); got != "1ab14ecd590575ac57f50fa4cbcf529766bc2c32ba974bda1614381281ec52b4" {
		t.Fatalf("upstream changed without qualification: %s", got)
	}
	raw, err = os.ReadFile("controller.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cm struct{ Data map[string]string }
	if err = yaml.Unmarshal(raw, &cm); err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err = yaml.Unmarshal([]byte(cm.Data["controller_manager_config.yaml"]), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["waitForPodsReady"] != nil {
		t.Fatal("a non-null waitForPodsReady enables eviction and replay")
	}
	integrations := cfg["integrations"].(map[string]any)["frameworks"].([]any)
	if len(integrations) != 1 || integrations[0] != "batch/job" {
		t.Fatal("only batch jobs may be managed")
	}
	if cfg["manageJobsWithoutQueueName"] != false {
		t.Fatal("unlabelled jobs must remain unmanaged")
	}
	raw, err = os.ReadFile("webhooks.yaml")
	if err != nil {
		t.Fatal(err)
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(raw)))
	for {
		var patch struct {
			Webhooks []struct {
				Name              string
				NamespaceSelector struct {
					MatchLabels map[string]string `yaml:"matchLabels"`
				} `yaml:"namespaceSelector"`
				Rules []struct{ Resources []string }
			}
		}
		err = decoder.Decode(&patch)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range patch.Webhooks {
			for _, rule := range w.Rules {
				for _, r := range rule.Resources {
					switch r {
					case "jobs", "workloads", "workloads/status":
						if w.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "memql-pipelines" {
							t.Fatalf("%s can block customer workloads during an operator outage", w.Name)
						}
					case "clusterqueues", "resourceflavors", "cohorts":
					default:
						t.Fatalf("unexpected webhook resource %s", r)
					}
				}
			}
		}
	}
}

// renderedInstall is what Argo CD applies: upstream with this directory's patches,
// so a patch that stops reaching its object fails these as surely as a changed value.
func renderedInstall(t *testing.T) []map[string]any {
	t.Helper()
	renderer, args := "kubectl", []string{"kustomize", "."}
	if _, err := exec.LookPath(renderer); err != nil {
		renderer, args = "kustomize", []string{"build", "."}
		if _, err = exec.LookPath(renderer); err != nil {
			t.Skip("neither supported renderer is installed")
		}
	}
	out, err := exec.Command(renderer, args...).Output()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var objects []map[string]any
	decoder := yaml.NewDecoder(bytes.NewReader(out))
	for {
		var object map[string]any
		err := decoder.Decode(&object)
		if err == io.EOF {
			return objects
		}
		if err != nil {
			t.Fatal(err)
		}
		objects = append(objects, object)
	}
}

// With the controller down the API server cannot call a webhook, and under
// failurePolicy Fail it answers every Job write in memql-pipelines with a 500. The
// runner cannot tell that from a create that landed, so a new step fails
// pipeline_execution_uncertain and stays failed; the isolation probe and a takeover
// claim are refused the same way. The hooks on Jobs therefore fail open: a queued Job
// the hook misses is still suspended, because the runner sets that itself
// (TestResourceQueueCreatesSuspendedJobAndReservesSidecars), and it waits for the
// controller. Everything else keeps failing closed: no step writes those kinds, and a
// queue or flavor written during an outage would go unvalidated.
// TestNewStepsWaitThroughAControllerOutage is the live half of this.
func TestAControllerOutageCannotRefuseAStepsJob(t *testing.T) {
	onJobs := map[string]string{}
	seen := 0
	for _, object := range renderedInstall(t) {
		if kind := object["kind"]; kind != "MutatingWebhookConfiguration" && kind != "ValidatingWebhookConfiguration" {
			continue
		}
		webhooks, _ := object["webhooks"].([]any)
		for _, item := range webhooks {
			webhook := item.(map[string]any)
			name, policy := webhook["name"].(string), webhook["failurePolicy"]
			seen++
			job := false
			rules, _ := webhook["rules"].([]any)
			for _, rule := range rules {
				for _, resource := range rule.(map[string]any)["resources"].([]any) {
					job = job || resource == "jobs"
				}
			}
			switch {
			case job:
				onJobs[name], _ = policy.(string)
				selector, _ := webhook["namespaceSelector"].(map[string]any)
				labels, _ := selector["matchLabels"].(map[string]any)
				if labels["kubernetes.io/metadata.name"] != "memql-pipelines" {
					t.Errorf("%s reaches Jobs outside memql-pipelines, where nothing suspends them: %v", name, selector)
				}
			case policy != "Fail":
				t.Errorf("%s has failurePolicy %v: only the hooks on Jobs may fail open", name, policy)
			}
		}
	}
	if seen == 0 {
		t.Fatal("the render holds no webhook, so this says nothing about them")
	}
	if want := map[string]string{"mjob.kb.io": "Ignore", "vjob.kb.io": "Ignore"}; !reflect.DeepEqual(onJobs, want) {
		t.Errorf("the hooks on Jobs are %v, want %v", onJobs, want)
	}
}

// A cluster restart makes a new controller pod. Upstream's Always then needs the
// registry, so a laptop with no network keeps the controller in ImagePullBackOff, and
// nothing admits a step until the network returns. IfNotPresent starts from the copy
// the node holds, and gives up nothing only because the image is pinned by digest.
func TestTheControllerRestartsWithoutItsRegistry(t *testing.T) {
	for _, object := range renderedInstall(t) {
		if object["kind"] != "Deployment" || object["metadata"].(map[string]any)["name"] != "kueue-controller-manager" {
			continue
		}
		spec := object["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		for _, item := range spec["containers"].([]any) {
			container := item.(map[string]any)
			if container["name"] != "manager" {
				continue
			}
			if got := container["imagePullPolicy"]; got != "IfNotPresent" {
				t.Errorf("imagePullPolicy is %v, want IfNotPresent", got)
			}
			if image, _ := container["image"].(string); !strings.Contains(image, "@sha256:") {
				t.Errorf("image %q is not pinned by digest, so IfNotPresent could keep a stale copy", image)
			}
			return
		}
	}
	t.Fatal("the render holds no kueue-controller-manager manager container")
}
