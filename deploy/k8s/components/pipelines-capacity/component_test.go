package pipelinescapacity

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAdmissionComponentRendersSameBoundariesOnEveryTarget(t *testing.T) {
	renderer, args := "kubectl", []string{"kustomize", "--load-restrictor", "LoadRestrictionsNone"}
	if _, err := exec.LookPath(renderer); err != nil {
		renderer, args = "kustomize", []string{"build", "--load-restrictor", "LoadRestrictionsNone"}
		if _, err = exec.LookPath(renderer); err != nil {
			t.Skip("neither supported renderer is installed")
		}
	}
	here, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"local", "cloud", "cloud-entry"} {
		t.Run(target, func(t *testing.T) {
			dir := t.TempDir()
			dir, err = filepath.EvalSymlinks(dir)
			if err != nil {
				t.Fatal(err)
			}
			overlay := filepath.Join(here, "../../overlays", target)
			relative := func(path string) string {
				p, e := filepath.Rel(dir, path)
				if e != nil {
					t.Fatal(e)
				}
				return filepath.ToSlash(p)
			}
			source := fmt.Sprintf("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n- %s\ncomponents:\n- %s\ntransformers:\n- %s\n", relative(overlay), relative(here), relative(filepath.Join(overlay, "namespace-transformer.yaml")))
			if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(renderer, append(args, dir)...).CombinedOutput()
			if err != nil {
				t.Fatalf("render: %v\n%s", err, out)
			}
			decoder := yaml.NewDecoder(strings.NewReader(string(out)))
			found := map[string]bool{}
			for {
				var o struct {
					Kind     string
					Metadata struct{ Name, Namespace string }
					Spec     map[string]any
				}
				err = decoder.Decode(&o)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				switch o.Kind + "/" + o.Metadata.Name {
				case "ClusterQueue/memql-background", "ResourceFlavor/memql-background", "BackgroundCapacityPolicy/memql-background-capacity":
					if o.Metadata.Namespace != "" {
						t.Fatalf("cluster-scoped %s rendered in namespace %s", o.Kind, o.Metadata.Namespace)
					}
					found[o.Kind] = true
				case "LocalQueue/memql-background":
					if o.Metadata.Namespace != "memql-pipelines" {
						t.Fatal("LocalQueue escaped the batch namespace")
					}
					found[o.Kind] = true
				case "ConfigMap/memql-background-capacity", "Role/memql-background-capacity":
					if o.Metadata.Namespace != "memql" {
						t.Fatalf("%s escaped the engine namespace", o.Kind)
					}
				case "LimitRange/memql-pipelines-limits":
					for _, raw := range o.Spec["limits"].([]any) {
						v := raw.(map[string]any)
						if v["type"] != "Container" {
							continue
						}
						limits, requests := v["default"].(map[string]any), v["defaultRequest"].(map[string]any)
						for _, resource := range []string{"memory", "ephemeral-storage"} {
							if limits[resource] != requests[resource] {
								t.Fatalf("unreserved %s default: %v versus %v", resource, limits[resource], requests[resource])
							}
						}
					}
				}
			}
			if len(found) != 4 {
				t.Fatalf("incomplete admission objects: %v", found)
			}
		})
	}
}
