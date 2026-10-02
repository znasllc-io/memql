package local

import (
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// File rows survive a pod replacement in Postgres. Their bytes must survive
// the same replacement; otherwise Files lists documents that cannot be read.
func TestLocalBlobBytesSurvivePodReplacement(t *testing.T) {
	type resource struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
		Spec struct {
			AccessModes []string `yaml:"accessModes"`
			Replicas    int      `yaml:"replicas"`
			Strategy    struct {
				Type string `yaml:"type"`
			} `yaml:"strategy"`
			Template struct {
				Spec struct {
					Containers []struct {
						Name         string   `yaml:"name"`
						Command      []string `yaml:"command"`
						VolumeMounts []struct {
							Name      string `yaml:"name"`
							MountPath string `yaml:"mountPath"`
						} `yaml:"volumeMounts"`
					} `yaml:"containers"`
					Volumes []struct {
						Name                  string `yaml:"name"`
						PersistentVolumeClaim struct {
							ClaimName string `yaml:"claimName"`
						} `yaml:"persistentVolumeClaim"`
					} `yaml:"volumes"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	objects := map[string]resource{}
	decoder := yaml.NewDecoder(strings.NewReader(render(t)))
	for {
		var r resource
		if err := decoder.Decode(&r); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		objects[r.Kind+"/"+r.Metadata.Name] = r
	}
	deployment := objects["Deployment/azurite"]
	if deployment.Spec.Replicas != 1 || deployment.Spec.Strategy.Type != "Recreate" {
		t.Fatal("Azurite needs one writer, including during rollout")
	}
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name != "azurite" {
			continue
		}
		location := ""
		for i, arg := range container.Command {
			if arg == "--location" && i+1 < len(container.Command) {
				location = container.Command[i+1]
			}
		}
		if location == "" {
			t.Fatal("Azurite must store its metadata and bytes at an explicit persistent location")
		}
		for _, mount := range container.VolumeMounts {
			if mount.MountPath != location {
				continue
			}
			for _, volume := range deployment.Spec.Template.Spec.Volumes {
				if volume.Name != mount.Name {
					continue
				}
				claim, ok := objects["PersistentVolumeClaim/"+volume.PersistentVolumeClaim.ClaimName]
				if ok && len(claim.Spec.AccessModes) == 1 && claim.Spec.AccessModes[0] == "ReadWriteOnce" {
					return
				}
			}
		}
	}
	t.Fatal("Azurite data location does not resolve to a rendered persistent volume claim")
}
