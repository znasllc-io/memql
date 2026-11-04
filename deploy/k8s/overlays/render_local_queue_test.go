// Render gates on the local cluster's pipeline queue (memql#6387, design record
// docs/superpowers/specs/2026-10-30-local-kueue-queue-design.md).
//
// WHAT CHANGED. The local overlay composes deploy/k8s/components/pipelines-capacity,
// the queue production runs: Kueue's ClusterQueue, flavor and LocalQueue, the
// operator's capacity ceiling, the strict order and the deadline promoter. What it
// does NOT take is production's sizing. Production reserves every step
// container's full memory and disk limit, so a budget means what it says; on a
// laptop that would stop the engine's 8 GiB steps from ever starting at Docker's
// minimum size, and a step over the budget blocks a strict queue's head. So local
// keeps the step requests it had and makes `pods` its only binding bound, with
// every other budget too large for two step pods to reach.
package overlays

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	// pipelinesQueue is the queue's name: the ClusterQueue, the LocalQueue the
	// runner labels its Jobs with, and the flavor.
	pipelinesQueue = "memql-background"
	// localQueuePods is local's concurrency: two steps at once, the number the
	// Job quota held before the queue (#6018: six cold builds writing at once
	// stalled etcd and restarted the serving cluster). Raising it needs that
	// warning's cold-cache storage evidence.
	localQueuePods = 2
	// localJobQuota is the Job-count quota once Jobs are born suspended: it
	// counts WAITING steps too, so it no longer bounds concurrency (pods does).
	// 32 is two overlapping runs at the driver's parallel(16).
	localJobQuota = "32"
)

// queueQuota reads the ClusterQueue's single flavor's nominal quotas.
type queueQuota struct {
	Spec struct {
		StopPolicy       string `yaml:"stopPolicy"`
		QueueingStrategy string `yaml:"queueingStrategy"`
		ResourceGroups   []struct {
			CoveredResources []string `yaml:"coveredResources"`
			Flavors          []struct {
				Name      string `yaml:"name"`
				Resources []struct {
					Name         string `yaml:"name"`
					NominalQuota string `yaml:"nominalQuota"`
				} `yaml:"resources"`
			} `yaml:"flavors"`
		} `yaml:"resourceGroups"`
	} `yaml:"spec"`
}

func (q queueQuota) quotas(t *testing.T) map[string]string {
	t.Helper()
	if len(q.Spec.ResourceGroups) != 1 || len(q.Spec.ResourceGroups[0].Flavors) != 1 {
		t.Fatalf("the ClusterQueue has %d resource group(s); the ceiling policy admits exactly one group of one flavor", len(q.Spec.ResourceGroups))
	}
	out := map[string]string{}
	for _, r := range q.Spec.ResourceGroups[0].Flavors[0].Resources {
		out[r.Name] = r.NominalQuota
	}
	return out
}

type capacityPolicy struct {
	Spec struct {
		CPUMilli  int    `yaml:"cpuMilli"`
		MemoryMiB int    `yaml:"memoryMiB"`
		DiskMiB   int    `yaml:"diskMiB"`
		Jobs      int    `yaml:"jobs"`
		NodePool  string `yaml:"nodePool"`
	} `yaml:"spec"`
}

func TestTheLocalOverlayComposesThePipelineQueue(t *testing.T) {
	objs := renderedObjects(t, "local")
	for _, id := range []struct{ kind, name, namespace string }{
		{"ClusterQueue", pipelinesQueue, ""},
		{"ResourceFlavor", pipelinesQueue, ""},
		{"LocalQueue", pipelinesQueue, pipelinesNamespace},
		{"BackgroundCapacityPolicy", "memql-background-capacity", ""},
		{"ValidatingAdmissionPolicy", "memql-background-ceiling", ""},
		{"ValidatingAdmissionPolicyBinding", "memql-background-ceiling", ""},
		{"CronJob", "pipeline-deadline-priority", cloudNamespace},
	} {
		if got := theOne(t, objs, id.kind, id.name); got.Namespace != id.namespace {
			t.Errorf("%s/%s rendered in namespace %q, want %q", id.kind, id.name, got.Namespace, id.namespace)
		}
	}
	var config struct {
		Data map[string]string `yaml:"data"`
	}
	theOne(t, objs, "ConfigMap", pipelinesConfigMap).decode(t, &config)
	if got := config.Data["MEMQL_PIPELINES_QUEUE"]; got != pipelinesQueue {
		t.Errorf("the workbench's MEMQL_PIPELINES_QUEUE is %q, want %q: without it the runner creates unlabelled Jobs Kueue never admits or holds", got, pipelinesQueue)
	}
}

// pods is the bound; the other three budgets must not be one. A budget a step
// can exceed blocks the strict queue's head for everything behind it (measured:
// an 8 GiB step at the head of a 5 GiB budget held a 4 GiB step for the run's
// whole ceiling). The budgets are also larger than any laptop's build node, so a
// step the node cannot hold is admitted and fails pipeline_job_unschedulable, as
// it did before the queue; what may not happen is a queue blocked at its head.
func TestTheLocalQueueIsBoundByPodsAlone(t *testing.T) {
	objs := renderedObjects(t, "local")
	var queue queueQuota
	theOne(t, objs, "ClusterQueue", pipelinesQueue).decode(t, &queue)
	quotas := queue.quotas(t)

	if queue.Spec.StopPolicy != "None" {
		t.Errorf("stopPolicy is %q, want None: the component ships Hold for an installation still rolling old runners, "+
			"and a local cluster under Hold leaves every step suspended with `ClusterQueue %s is inactive`", queue.Spec.StopPolicy, pipelinesQueue)
	}
	if queue.Spec.QueueingStrategy != "StrictFIFO" {
		t.Errorf("queueingStrategy is %q: local runs production's strict order, which the promoter needs", queue.Spec.QueueingStrategy)
	}
	if got := quotas["pods"]; got != strconv.Itoa(localQueuePods) {
		t.Errorf("the pods quota is %q, want %d", got, localQueuePods)
	}

	// The most two step pods can ask for: the command container's largest
	// explicit reservation (the workbench's defaults, 4 CPUs and 8192 MiB) plus
	// twelve more containers -- clone, cache, services, collectors -- at the
	// LimitRange's default request, and every container's disk request.
	var limits pipelinesLimits
	theOne(t, objs, "LimitRange", "memql-pipelines-limits").decode(t, &limits)
	request := map[string]string{}
	for _, item := range limits.Spec.Limits {
		if item.Type == "Container" {
			request = item.DefaultRequest
		}
	}
	const containers = 13
	most := map[string]float64{
		"cpu":               localQueuePods * (4000 + (containers-1)*float64(milliCPU(t, "local", request["cpu"]))),
		"memory":            localQueuePods * (8192 + (containers-1)*float64(mebibytes(t, "local", request["memory"]))),
		"ephemeral-storage": localQueuePods * containers * float64(mebibytes(t, "local", request["ephemeral-storage"])),
	}
	for resource, ceiling := range most {
		var have float64
		if resource == "cpu" {
			have = float64(milliCPU(t, "local", quotas[resource]))
		} else {
			have = float64(mebibytes(t, "local", quotas[resource]))
		}
		if have < ceiling {
			t.Errorf("the %s budget %s is below the %v two step pods can request, so it can bind before pods does "+
				"and block the strict queue's head", resource, quotas[resource], ceiling)
		}
	}

	// The operator ceiling the admission policy checks every later apply and save
	// against: the queue as rendered must fit it. Argo CD can create the queue
	// before the policy on a first sync, so this test, not that apply, is what
	// proves the two agree.
	var policy capacityPolicy
	theOne(t, objs, "BackgroundCapacityPolicy", "memql-background-capacity").decode(t, &policy)
	if policy.Spec.Jobs != localQueuePods {
		t.Errorf("the ceiling allows %d jobs at once, want %d: the queue's pods quota may not exceed it", policy.Spec.Jobs, localQueuePods)
	}
	for resource, ceiling := range map[string]int{"cpu": policy.Spec.CPUMilli, "memory": policy.Spec.MemoryMiB, "ephemeral-storage": policy.Spec.DiskMiB} {
		var have int
		if resource == "cpu" {
			have = int(milliCPU(t, "local", quotas[resource]))
		} else {
			have = int(mebibytes(t, "local", quotas[resource]))
		}
		if have != ceiling {
			t.Errorf("the %s budget is %s but the operator ceiling is %d: the ceiling is the budget's limit, and local's "+
				"budget is as large as it may be", resource, quotas[resource], ceiling)
		}
	}
	if policy.Spec.NodePool != "builds" {
		t.Errorf("the ceiling names pool %q, want builds", policy.Spec.NodePool)
	}
}

// The upgrade trap (design record, "The Job quota"): a workbench that has not
// restarted still creates unlabelled Jobs, which Kueue ignores, and with the
// quota at 32 they would all start at once. envFrom does not restart a pod when
// its ConfigMap changes, so the queue's name rides the workbench's pod template
// too, and the sync that adds it rolls the workbench.
func TestTheWorkbenchRollsWithItsQueue(t *testing.T) {
	objs := renderedObjects(t, "local")
	var config struct {
		Data map[string]string `yaml:"data"`
	}
	theOne(t, objs, "ConfigMap", pipelinesConfigMap).decode(t, &config)
	var workbench struct {
		Spec struct {
			Template struct {
				Metadata struct {
					Annotations map[string]string `yaml:"annotations"`
				} `yaml:"metadata"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	theOne(t, objs, "Deployment", "workbench").decode(t, &workbench)
	got := workbench.Spec.Template.Metadata.Annotations["memql.io/pipeline-queue"]
	if got == "" || got != config.Data["MEMQL_PIPELINES_QUEUE"] {
		t.Errorf("the workbench's pod template carries memql.io/pipeline-queue=%q, want the ConfigMap's %q", got, config.Data["MEMQL_PIPELINES_QUEUE"])
	}
}

// The promoter is the workbench binary, so it runs the image `make dev` builds
// and imports, never pulled: the overlay's images: entry for the workbench
// retargets both, and a release installer's override does the same.
func TestThePromoterRunsTheLocalWorkbenchImage(t *testing.T) {
	objs := renderedObjects(t, "local")
	type container struct {
		Image           string `yaml:"image"`
		ImagePullPolicy string `yaml:"imagePullPolicy"`
	}
	var workbench struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []container `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	theOne(t, objs, "Deployment", "workbench").decode(t, &workbench)
	var cron struct {
		Spec struct {
			JobTemplate struct {
				Spec struct {
					Template struct {
						Spec struct {
							Containers []container `yaml:"containers"`
						} `yaml:"spec"`
					} `yaml:"template"`
				} `yaml:"spec"`
			} `yaml:"jobTemplate"`
		} `yaml:"spec"`
	}
	theOne(t, objs, "CronJob", "pipeline-deadline-priority").decode(t, &cron)
	promoter := cron.Spec.JobTemplate.Spec.Template.Spec.Containers
	if len(promoter) != 1 || len(workbench.Spec.Template.Spec.Containers) == 0 {
		t.Fatalf("containers: promoter %v, workbench %v", promoter, workbench.Spec.Template.Spec.Containers)
	}
	if want := workbench.Spec.Template.Spec.Containers[0].Image; promoter[0].Image != want || !strings.HasSuffix(want, ":local") {
		t.Errorf("the promoter runs %q, want the workbench's locally built %q", promoter[0].Image, want)
	}
	if promoter[0].ImagePullPolicy != "IfNotPresent" {
		t.Errorf("the promoter's pull policy is %q, want IfNotPresent: the image is imported into k3d and exists in no registry", promoter[0].ImagePullPolicy)
	}
}

// deploy/argocd/local/project.yaml allows ClusterRole and ClusterRoleBinding to
// the mesh Application, and an AppProject cannot narrow a kind by name. So the
// render itself is held to the one cluster-wide grant the queue needs: any other
// ClusterRole or ClusterRoleBinding would reach the cluster through that
// allowance, whatever subject it binds and whatever the other gates check.
func TestTheLocalOverlaysOnlyClusterWideGrantIsTheQueues(t *testing.T) {
	var got []string
	for _, o := range renderedObjects(t, "local") {
		if o.Kind == "ClusterRole" || o.Kind == "ClusterRoleBinding" {
			got = append(got, o.Kind+"/"+o.Name)
		}
	}
	slices.Sort(got)
	if want := []string{"ClusterRole/" + queueCapacityGrant, "ClusterRoleBinding/" + queueCapacityGrant}; !slices.Equal(got, want) {
		t.Errorf("the local overlay renders cluster-wide grants %v, want exactly %v: the local AppProject admits every "+
			"ClusterRole and ClusterRoleBinding, so each one here is a grant this test is the only review of", got, want)
	}
}

// Every cluster-scoped kind the local overlay renders must be one its AppProject
// allows, or Argo CD refuses the whole sync. Read off the render, so a kind the
// overlay gains later is caught here rather than as a failed sync.
func TestTheLocalAppProjectAllowsEveryClusterScopedKindTheOverlayRenders(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "argocd", "local", "project.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var project struct {
		Spec struct {
			ClusterResourceWhitelist []struct{ Group, Kind string } `yaml:"clusterResourceWhitelist"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(data, &project); err != nil {
		t.Fatal(err)
	}
	allowed := func(group, kind string) bool {
		for _, w := range project.Spec.ClusterResourceWhitelist {
			if (w.Group == group || w.Group == "*") && (w.Kind == kind || w.Kind == "*") {
				return true
			}
		}
		return false
	}
	seen := 0
	for _, o := range renderedObjects(t, "local") {
		if o.Namespace != "" {
			continue
		}
		var head struct {
			APIVersion string `yaml:"apiVersion"`
		}
		o.decode(t, &head)
		group := ""
		if i := strings.LastIndex(head.APIVersion, "/"); i >= 0 {
			group = head.APIVersion[:i]
		}
		seen++
		if !allowed(group, o.Kind) {
			t.Errorf("%s/%s (group %q) is cluster-scoped and deploy/argocd/local/project.yaml does not allow it: Argo CD refuses the sync", o.Kind, o.Name, group)
		}
	}
	if seen == 0 {
		t.Fatal("the local render has no cluster-scoped object at all; this gate is reading nothing")
	}
}
