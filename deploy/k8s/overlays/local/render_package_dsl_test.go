package local

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The fetcher, mounted tree and narrowly scoped restart grant must travel
// together. Otherwise a successful build leaves package DSL unavailable.
func TestLocalPackagesCanLoadAndRollTheirDSL(t *testing.T) {
	want := []string{"agent", "bff", "planner", "workbench"}
	var consumers, granted []string
	storage := false
	for _, doc := range strings.Split(render(t), "\n---\n") {
		var r struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Data  map[string]string `yaml:"data"`
			Rules []struct {
				ResourceNames []string `yaml:"resourceNames"`
				Verbs         []string `yaml:"verbs"`
			} `yaml:"rules"`
			Spec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Env          []struct{ Name, Value string } `yaml:"env"`
							VolumeMounts []struct {
								Name      string `yaml:"name"`
								MountPath string `yaml:"mountPath"`
							} `yaml:"volumeMounts"`
						} `yaml:"containers"`
						InitContainers []struct {
							Name, Image string
							Command     []string `yaml:"command"`
							EnvFrom     []struct {
								SecretRef    struct{ Name string } `yaml:"secretRef"`
								ConfigMapRef struct{ Name string } `yaml:"configMapRef"`
							} `yaml:"envFrom"`
						} `yaml:"initContainers"`
					} `yaml:"spec"`
				} `yaml:"template"`
			} `yaml:"spec"`
		}
		if err := yaml.Unmarshal([]byte(doc), &r); err != nil {
			t.Fatal(err)
		}
		if r.Kind == "ConfigMap" && r.Metadata.Name == "memql-storage" {
			storage = r.Data["MEMQL_AZURE_BLOB_CONTAINER"] == "memql"
		}
		if r.Kind == "Role" && r.Metadata.Name == "memql-packages-roll" {
			if len(r.Rules) != 1 || !reflect.DeepEqual(r.Rules[0].Verbs, []string{"get", "patch"}) {
				t.Fatal("roll grant must remain get/patch only")
			}
			granted = r.Rules[0].ResourceNames
		}
		if r.Kind != "Deployment" {
			continue
		}
		for _, init := range r.Spec.Template.Spec.InitContainers {
			if init.Name != "dsl-packages-fetch" {
				continue
			}
			consumers = append(consumers, r.Metadata.Name)
			if init.Image != "memql-bff:local" || !reflect.DeepEqual(init.Command, []string{"/app/memql", "dsl-fetch"}) {
				t.Errorf("%s fetcher cannot use imported local image", r.Metadata.Name)
			}
			secret, config := false, false
			for _, from := range init.EnvFrom {
				secret = secret || from.SecretRef.Name == "memql-secrets"
				config = config || from.ConfigMapRef.Name == "memql-storage"
			}
			if !secret || !config {
				t.Errorf("%s fetcher lacks storage settings", r.Metadata.Name)
			}
			c := r.Spec.Template.Spec.Containers[0]
			env := map[string]string{}
			for _, e := range c.Env {
				env[e.Name] = e.Value
			}
			targets := strings.Split(env["MEMQL_PACKAGES_ROLL_TARGETS"], ",")
			sort.Strings(targets)
			if !reflect.DeepEqual(targets, want) || env["MEMQL_DSL_PATH"] != "/var/lib/memql/dsl" {
				t.Errorf("%s has incomplete DSL configuration", r.Metadata.Name)
			}
			mounted := false
			for _, mount := range c.VolumeMounts {
				mounted = mounted || mount.Name == "dsl-tree" && mount.MountPath == "/var/lib/memql/dsl"
			}
			if !mounted {
				t.Errorf("%s cannot read fetched DSL", r.Metadata.Name)
			}
		}
	}
	sort.Strings(consumers)
	sort.Strings(granted)
	if !storage || !reflect.DeepEqual(consumers, want) || !reflect.DeepEqual(granted, want) {
		t.Fatalf("storage=%v consumers=%v roll permissions=%v", storage, consumers, granted)
	}
}
