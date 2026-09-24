package local

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Small inline builds hide a missing workbench container setting. The sender
// and receiver must both reach Azurite when source or output crosses that cap.
func TestBuildAndAttachmentNodesShareLocalBlobStorage(t *testing.T) {
	want := map[string]bool{"bff": false, "agent": false, "workbench": false}
	for _, doc := range strings.Split(render(t), "\n---\n") {
		var resource struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Env []struct {
								Name  string `yaml:"name"`
								Value string `yaml:"value"`
							} `yaml:"env"`
							EnvFrom []struct {
								SecretRef struct {
									Name string `yaml:"name"`
								} `yaml:"secretRef"`
							} `yaml:"envFrom"`
						} `yaml:"containers"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal([]byte(doc), &resource); err != nil {
			t.Fatal(err)
		}
		if _, ok := want[resource.Metadata.Name]; !ok || resource.Kind != "Deployment" {
			continue
		}
		want[resource.Metadata.Name] = true
		for _, container := range resource.Spec.Template.Spec.Containers {
			env := map[string]string{}
			for _, entry := range container.Env {
				env[entry.Name] = entry.Value
			}
			if env["MEMQL_AZURE_BLOB_CONTAINER"] != "memql" || env["MEMQL_AZURE_BLOB_AUTOCREATE"] != "true" {
				t.Errorf("%s cannot use local build blob storage", resource.Metadata.Name)
			}
			secret := false
			for _, from := range container.EnvFrom {
				secret = secret || from.SecretRef.Name == "memql-secrets"
			}
			if !secret {
				t.Errorf("%s has no storage connection secret", resource.Metadata.Name)
			}
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("missing %s deployment", name)
		}
	}
}
