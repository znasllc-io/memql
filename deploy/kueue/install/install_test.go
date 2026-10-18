package install

import (
	"crypto/sha256"
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"os"
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
