package pipelinescapacity

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/integrations/pipelinesteps"
	"gopkg.in/yaml.v3"
)

// renderedObject is the part of a rendered manifest these gates read.
type renderedObject struct {
	Kind     string
	Metadata struct{ Name, Namespace string }
	Spec     map[string]any
	Data     map[string]string
	Value    int
	Rules    []struct {
		APIGroups []string `yaml:"apiGroups"`
		Resources []string
		Verbs     []string
	}
	Subjects []struct{ Kind, Name, Namespace string }
	RoleRef  struct{ Kind, Name string } `yaml:"roleRef"`
}

// renderComponent renders the component over one instance overlay, the way an
// installation composes it: above the overlay, with whatever else it states in
// that same layer (extra).
func renderComponent(t *testing.T, target, extra string) []renderedObject {
	t.Helper()
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
	source := fmt.Sprintf("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n- %s\ncomponents:\n- %s\ntransformers:\n- %s\n%s", relative(overlay), relative(here), relative(filepath.Join(overlay, "namespace-transformer.yaml")), extra)
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(renderer, append(args, dir)...).CombinedOutput()
	if err != nil {
		t.Fatalf("render: %v\n%s", err, out)
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(out)))
	var objects []renderedObject
	for {
		var o renderedObject
		err = decoder.Decode(&o)
		if err == io.EOF {
			return objects
		}
		if err != nil {
			t.Fatal(err)
		}
		objects = append(objects, o)
	}
}

func TestAdmissionComponentRendersSameBoundariesOnEveryTarget(t *testing.T) {
	for _, target := range []string{"local", "cloud", "cloud-entry"} {
		t.Run(target, func(t *testing.T) {
			found := map[string]bool{}
			for _, o := range renderComponent(t, target, "") {
				switch o.Kind + "/" + o.Metadata.Name {
				case "ClusterQueue/memql-background", "ResourceFlavor/memql-background", "BackgroundCapacityPolicy/memql-background-capacity",
					"ValidatingAdmissionPolicy/memql-background-ceiling", "ValidatingAdmissionPolicyBinding/memql-background-ceiling",
					"ClusterRole/memql-background-capacity", "ClusterRoleBinding/memql-background-capacity",
					"WorkloadPriorityClass/" + pipelinesteps.ClassExpiring, "WorkloadPriorityClass/" + pipelinesteps.ClassBoosted, "WorkloadPriorityClass/" + pipelinesteps.ClassAged:
					if o.Metadata.Namespace != "" {
						t.Fatalf("cluster-scoped %s rendered in namespace %s", o.Kind, o.Metadata.Namespace)
					}
					found[o.Kind] = true
				case "LocalQueue/memql-background":
					if o.Metadata.Namespace != "memql-pipelines" {
						t.Fatal("LocalQueue escaped the batch namespace")
					}
					found[o.Kind] = true
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
			if len(found) != 9 {
				t.Fatalf("incomplete admission objects: %v", found)
			}
		})
	}
}

// promoterJob is the part of the CronJob these gates read.
type promoterJob struct {
	Schedule          string
	ConcurrencyPolicy string `yaml:"concurrencyPolicy"`
	Suspend           bool
	JobTemplate       struct {
		Spec struct {
			ActiveDeadlineSeconds int `yaml:"activeDeadlineSeconds"`
			BackoffLimit          int `yaml:"backoffLimit"`
			Template              struct {
				Metadata struct{ Labels map[string]string }
				Spec     struct {
					ServiceAccountName string            `yaml:"serviceAccountName"`
					NodeSelector       map[string]string `yaml:"nodeSelector"`
					Tolerations        []any
					SecurityContext    struct {
						RunAsNonRoot bool `yaml:"runAsNonRoot"`
						RunAsUser    int  `yaml:"runAsUser"`
					} `yaml:"securityContext"`
					Containers []struct {
						Image           string
						Command         []string
						Args            []string
						Env             []struct{ Name, Value string }
						EnvFrom         []any `yaml:"envFrom"`
						SecurityContext struct {
							ReadOnlyRootFilesystem   bool  `yaml:"readOnlyRootFilesystem"`
							AllowPrivilegeEscalation *bool `yaml:"allowPrivilegeEscalation"`
						} `yaml:"securityContext"`
					}
				}
			}
		}
	} `yaml:"jobTemplate"`
}

func promoterOf(t *testing.T, objects []renderedObject) promoterJob {
	t.Helper()
	for _, o := range objects {
		if o.Kind != "CronJob" || o.Metadata.Name != "pipeline-deadline-priority" {
			continue
		}
		if o.Metadata.Namespace != "memql" {
			t.Fatalf("the promoter rendered in %q: in the step namespace its pod would be behind the steps' network isolation and count against their Job quota", o.Metadata.Namespace)
		}
		raw, err := yaml.Marshal(o.Spec)
		if err != nil {
			t.Fatal(err)
		}
		var job promoterJob
		if err := yaml.Unmarshal(raw, &job); err != nil {
			t.Fatal(err)
		}
		if len(job.JobTemplate.Spec.Template.Spec.Containers) != 1 {
			t.Fatalf("containers = %+v", job.JobTemplate.Spec.Template.Spec.Containers)
		}
		return job
	}
	t.Fatal("no pipeline-deadline-priority CronJob rendered")
	return promoterJob{}
}

// The promoter is one thing on every target: what a target varies is the overlay
// above it, in the same layer as its images.
func TestDeadlinePromoterRendersTheSameOnEveryTarget(t *testing.T) {
	base, err := os.ReadFile("../../base/workbench.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var workbenchImage string
	for _, line := range strings.Split(string(base), "\n") {
		if value, found := strings.CutPrefix(strings.TrimSpace(line), "image: "); found {
			workbenchImage = value
			break
		}
	}
	if workbenchImage == "" {
		t.Fatal("no image in the base workbench")
	}
	for _, target := range []string{"local", "cloud", "cloud-entry"} {
		t.Run(target, func(t *testing.T) {
			objects := renderComponent(t, target, "")
			job := promoterOf(t, objects)
			if job.Schedule != "*/5 * * * *" || job.ConcurrencyPolicy != "Forbid" {
				t.Errorf("schedule %q, concurrency %q", job.Schedule, job.ConcurrencyPolicy)
			}
			if job.Suspend {
				t.Error("the promoter ships off: a queue that uses it would quietly expire work again")
			}
			if job.JobTemplate.Spec.BackoffLimit != 0 || job.JobTemplate.Spec.ActiveDeadlineSeconds != 120 {
				t.Errorf("a pass is one shot under the subcommand's own 100-second budget: %+v", job.JobTemplate.Spec.ActiveDeadlineSeconds)
			}
			pod := job.JobTemplate.Spec.Template.Spec
			if pod.ServiceAccountName != "pipeline-deadline-priority" {
				t.Errorf("service account %q", pod.ServiceAccountName)
			}
			// The build pool is tainted, so no toleration is what keeps this off the
			// pool it is freeing. A pool name would be one provider's value.
			if len(pod.NodeSelector) != 0 || len(pod.Tolerations) != 0 {
				t.Errorf("selector %v, tolerations %v", pod.NodeSelector, pod.Tolerations)
			}
			if !pod.SecurityContext.RunAsNonRoot || pod.SecurityContext.RunAsUser != 65532 {
				t.Errorf("security context %+v; 65532 is the runtime image's nonroot user", pod.SecurityContext)
			}
			container := pod.Containers[0]
			if container.Image != workbenchImage {
				t.Errorf("image %q, want the base workbench's %q, so every images: entry that retargets the workbench retargets this", container.Image, workbenchImage)
			}
			if !reflect.DeepEqual(container.Command, []string{"/app/memql", "pipeline-deadline-priority"}) {
				t.Errorf("command %v", container.Command)
			}
			// No window stated: on every target they follow each run's ceiling.
			if !reflect.DeepEqual(container.Args, []string{"--namespace=memql-pipelines", "--queue=memql-background"}) {
				t.Errorf("args %v", container.Args)
			}
			if !container.SecurityContext.ReadOnlyRootFilesystem || container.SecurityContext.AllowPrivilegeEscalation == nil || *container.SecurityContext.AllowPrivilegeEscalation {
				t.Errorf("container security context %+v", container.SecurityContext)
			}
			if len(container.EnvFrom) != 0 {
				t.Errorf("envFrom %v: a pass holds no secret and no pool, and needs neither", container.EnvFrom)
			}

			seen := map[string]bool{}
			classes := map[string]int{}
			for _, o := range objects {
				id := o.Kind + "/" + o.Metadata.Name
				switch id {
				case "ServiceAccount/pipeline-deadline-priority":
					seen[id] = o.Metadata.Namespace == "memql"
				case "Role/pipeline-deadline-priority":
					// The one grant: no get, so no reading a Job by name or a Secret;
					// no create or delete, so a pass can only reorder.
					seen[id] = o.Metadata.Namespace == "memql-pipelines" && len(o.Rules) == 1 &&
						reflect.DeepEqual(o.Rules[0].APIGroups, []string{"batch"}) &&
						reflect.DeepEqual(o.Rules[0].Resources, []string{"jobs"}) &&
						reflect.DeepEqual(o.Rules[0].Verbs, []string{"list", "patch"})
				case "RoleBinding/pipeline-deadline-priority":
					seen[id] = o.Metadata.Namespace == "memql-pipelines" && len(o.Subjects) == 1 &&
						o.Subjects[0].Kind == "ServiceAccount" && o.Subjects[0].Name == "pipeline-deadline-priority" && o.Subjects[0].Namespace == "memql" &&
						o.RoleRef.Kind == "Role" && o.RoleRef.Name == "pipeline-deadline-priority"
				case "ClusterQueue/memql-background":
					// Promotion means nothing without strict order: a backfilling queue
					// admits whatever fits around the promoted Job.
					seen[id] = o.Spec["queueingStrategy"] == "StrictFIFO"
				case "ConfigMap/memql-pipelines":
					seen[id] = o.Data["MEMQL_PIPELINES_QUEUE"] == "memql-background"
				case "LocalQueue/memql-background":
					seen[id] = o.Metadata.Namespace == "memql-pipelines"
				}
				if o.Kind == "WorkloadPriorityClass" {
					classes[o.Metadata.Name] = o.Value
				}
				if strings.HasPrefix(o.Kind, "Cluster") && strings.Contains(o.Metadata.Name, "deadline-priority") {
					t.Errorf("%s: a pass needs no cluster-wide grant", id)
				}
			}
			for _, id := range []string{"ServiceAccount/pipeline-deadline-priority", "Role/pipeline-deadline-priority", "RoleBinding/pipeline-deadline-priority",
				"ClusterQueue/memql-background", "ConfigMap/memql-pipelines", "LocalQueue/memql-background"} {
				if !seen[id] {
					t.Errorf("%s is missing or differs", id)
				}
			}
			// Only the classes a pass sets are the component's. An installation's own
			// event classes are its configuration (MEMQL_PIPELINES_QUEUE_PRIORITY_CLASSES),
			// and the order among these three is what lets a boost never cost an expiry.
			if want := (map[string]int{pipelinesteps.ClassExpiring: 400, pipelinesteps.ClassBoosted: 375, pipelinesteps.ClassAged: 350}); !reflect.DeepEqual(classes, want) {
				t.Errorf("classes %v, want %v", classes, want)
			}
		})
	}
}

// How an installation turns the promoter off, and sets its windows, its boost list
// and its pool: patches in the layer that composes the component. A second patch
// target or a reordered container would break this quietly, and the README tells
// an operator to write exactly these.
func TestAnInstallationSetsThePromoterFromItsOverlay(t *testing.T) {
	const digest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	const boost = "79bb3043f59bf98ed9c0898cb74d823f"
	extra := fmt.Sprintf(`images:
- name: acrmemql.azurecr.io/memql-workbench
  digest: %s
patches:
- target: {kind: CronJob, name: pipeline-deadline-priority}
  patch: |-
    - {op: add, path: /spec/suspend, value: true}
    - {op: add, path: /spec/jobTemplate/spec/template/spec/containers/0/args/-, value: --window-minutes=180}
    - {op: add, path: /spec/jobTemplate/spec/template/spec/containers/0/args/-, value: --aged-window-minutes=360}
    - {op: add, path: /spec/jobTemplate/spec/template/spec/containers/0/args/-, value: --boost-run=%s}
    - {op: add, path: /spec/jobTemplate/spec/template/spec/nodeSelector, value: {agentpool: mesh}}
`, digest, boost)
	job := promoterOf(t, renderComponent(t, "cloud-entry", extra))
	if !job.Suspend {
		t.Error("suspend did not reach the CronJob")
	}
	pod := job.JobTemplate.Spec.Template.Spec
	if want := map[string]string{"agentpool": "mesh"}; !reflect.DeepEqual(pod.NodeSelector, want) {
		t.Errorf("selector %v", pod.NodeSelector)
	}
	container := pod.Containers[0]
	wantArgs := []string{"--namespace=memql-pipelines", "--queue=memql-background", "--window-minutes=180", "--aged-window-minutes=360", "--boost-run=" + boost}
	if !reflect.DeepEqual(container.Args, wantArgs) {
		t.Errorf("args %v, want %v", container.Args, wantArgs)
	}
	if want := "acrmemql.azurecr.io/memql-workbench@" + digest; container.Image != want {
		t.Errorf("image %q, want %q: the same images: entry that pins the workbench must pin this", container.Image, want)
	}
}
