// Render gates on the pipelines substrate (epic memql#5478, task #5492).
//
// WHAT RENDERS, AND WHERE. A compiled pipeline step runs as a Kubernetes Job in
// a namespace of its own, memql-pipelines, created and watched by the workbench
// node. deploy/k8s/components/pipelines ships that namespace and everything in
// it -- the runner's Role, the step's empty identity, the shared cache volume,
// the concurrency ceiling, the container limits and the network isolation --
// plus ONE object in the mesh namespace: the ConfigMap that tells the workbench
// where the steps go. Every instance overlay composes it, so every gate here
// runs against all three.
//
// WHY A RENDER AND NOT A READ OF THE COMPONENT. The component states
// `namespace: memql-pipelines` on every object it puts there, and whether that
// SURVIVES is decided by the consuming overlay, not by the component. A plain
// `namespace:` field rewrites every explicit namespace it meets -- and with a
// second Namespace object in the stream it fails the render outright
// ("namespace transformation produces ID conflict"). So the overlays set the
// mesh namespace through a NamespaceTransformer with `unsetOnly: true`. A
// regression to the plain field is loud; a component object that forgets its
// namespace is not: it lands in the MESH namespace, where the runner's Role
// would grant Jobs next to the engine instead of where the steps run, and
// nothing in either file looks wrong.
//
// THE LOCAL OVERLAY IS IN THE SAME TABLE. The overlays package already renders
// `local` (internal TLS, deploy RBAC, both federations), and one table over all
// three keeps one copy of each assertion: a second copy in package `local`
// would be a copy that can be updated alone.
package overlays

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	// pipelinesNamespace is where every pipeline step runs: one name per
	// cluster, which is why tenants do not compose the component.
	pipelinesNamespace = "memql-pipelines"
	// pipelinesConfigMap is the workbench's half: the namespace above and the
	// clone image, delivered as env through envFrom.
	pipelinesConfigMap = "memql-pipelines"
	// pipelinesValuesFile is where an overlay states its pipelines VALUES.
	pipelinesValuesFile = "pipelines-values.yaml"
)

// The objects the component must render, by kind and name, in memql-pipelines.
var wantPipelinesObjects = map[string]string{
	"ServiceAccount/memql-pipelines-step":         pipelinesNamespace,
	"Role/memql-pipelines-runner":                 pipelinesNamespace,
	"RoleBinding/memql-pipelines-runner":          pipelinesNamespace,
	"PersistentVolumeClaim/memql-pipelines-cache": pipelinesNamespace,
	"ResourceQuota/memql-pipelines-ceiling":       pipelinesNamespace,
	"LimitRange/memql-pipelines-limits":           pipelinesNamespace,
	"NetworkPolicy/memql-pipelines-isolate":       pipelinesNamespace,
	"ConfigMap/memql-pipelines":                   cloudNamespace, // the workbench's env
}

// pipelinesOverlays are the instance overlays that compose the component.
// Listed rather than discovered, for the reason rbacOverlays is: the failure
// this catches is an overlay arriving without the wiring.
var pipelinesOverlays = []string{"cloud", "cloud-entry", "local"}

// pipelinesCache is the cache volume's class and access mode, per overlay.
//
// Both are VALUES and neither is free. The steps of one run land on whichever
// nodes have room, so on a multi-node cloud cluster a cache two step pods mount
// at once must be ReadWriteMany -- on AKS, the Blob CSI driver's NFS class,
// which exists only when the cluster runs that driver (the coupling
// substrate_overlay_coupling_test.go holds). Locally the cluster's default
// class is k3d's local-path provisioner, which offers ReadWriteOnce only; that
// is per NODE rather than per pod, and a local-path volume pins the pods that
// mount it to its node, so concurrent steps still share it there.
//
// "No class" is an ABSENT storageClassName, never an empty one: `""` means
// "bind only to a pre-made PersistentVolume", which no local cluster has, so
// the claim would sit Pending forever.
//
// bindsOnFirstUse is the bindsOnFirstUseAnnotation mark, LOCAL ONLY:
// local-path binds on first consumer, so the claim is Pending until a step
// mounts it, and the mark is what lets Argo CD read that as Healthy (see
// TestTheArgoCDBootstrapReadsAMarkedUnboundClaimAsHealthy). On a cloud
// cluster a Pending cache can mean its class does not exist, which must keep
// reading Progressing.
var pipelinesCache = map[string]struct {
	storageClass    *string
	accessMode      string
	bindsOnFirstUse bool
}{
	"cloud":       {storageClass: ptr("azureblob-nfs-premium"), accessMode: "ReadWriteMany"},
	"cloud-entry": {storageClass: ptr("azureblob-nfs-premium"), accessMode: "ReadWriteMany"},
	"local":       {storageClass: nil, accessMode: "ReadWriteOnce", bindsOnFirstUse: true},
}

// bindsOnFirstUseAnnotation marks a claim whose Pending phase is its healthy
// steady state; deploy/argocd/bootstrap/pvc-health.yaml is what reads it.
const bindsOnFirstUseAnnotation = "memql.io/binds-on-first-use"

func ptr[T any](v T) *T { return &v }

// pipelinesSizing is the step sizing each overlay states: the concurrency
// ceiling and every step container's default limit and request.
//
// Pinned, because each number is a capacity decision. The cloud overlays are
// sized for the pool scripts/deploy/azure-provision.sh creates by default --
// 2 x Standard_D2as_v4, about 3.8 allocatable CPU -- with that overlay's mesh
// already on it (cloud requests 2.4 CPU / 3Gi). The LimitRange applies to
// every container of a step pod, the clone init container and each service
// sidecar included, so services multiply a step's request. At the 12 Jobs x
// 1 CPU / 2Gi the cloud overlay first carried, not one step fit. A CI-heavy
// instance raises these in its own overlay together with a dedicated node pool
// for the steps; changing them here is that decision, so it shows in review.
//
// DISK (review M4) is a container's ephemeral storage: its own files outside
// any volume and its logs, and -- summed over the pod's containers -- every
// emptyDir too, the step's workspace among them. The kubelet evicts a pod
// past either, so a step cannot fill its node's disk. The entry overlay, the
// smallest install, gives a step half what the others do. The workspace's own
// size limit is the same number (TestThePipelinesWorkspaceLimitIsTheLimitRanges).
var pipelinesSizing = map[string]struct {
	ceiling                   string
	limitCPU, limitMemory     string
	requestCPU, requestMemory string
	limitDisk, requestDisk    string
}{
	"cloud":       {ceiling: "2", limitCPU: "2", limitMemory: "4Gi", requestCPU: "250m", requestMemory: "512Mi", limitDisk: "20Gi", requestDisk: "1Gi"},
	"cloud-entry": {ceiling: "1", limitCPU: "2", limitMemory: "4Gi", requestCPU: "250m", requestMemory: "512Mi", limitDisk: "10Gi", requestDisk: "1Gi"},
	"local":       {ceiling: "4", limitCPU: "2", limitMemory: "4Gi", requestCPU: "250m", requestMemory: "512Mi", limitDisk: "20Gi", requestDisk: "1Gi"},
}

// wantRunnerGrants is the runner's whole grant, as group/resource -> verbs:
// exactly the calls it makes, every one of them through
// integrations/pipelinesteps/kube.go -- the step's runner, the orphan-Secret
// sweep and the isolation proof alike -- and nothing it does not (review M3).
//
//	batch/jobs create            CreateJob: a step's Job, the isolation probe's
//	           get               GetJob: a step's status, adopting a Job, the
//	                             sweep asking whether a Secret's Job exists, and
//	                             CreateJob's read-back when the Job was there
//	           patch             AnnotateJob: the heartbeat, a claim, how the
//	                             step ended, its outcome
//	           delete            DeleteJob: the ack, the probe's own cleanup
//	           deletecollection  DeleteRun: a cancelled run's Jobs, by label
//	pods       list              JobPods: a Job's pods, by the job-name label --
//	                             never one pod by name, and never a watch
//	pods/log   get               FollowLog (follow=true is a get) and TailLog
//	secrets    create            CreateSecret: a step's Secret, the probe's
//	           get               SecretCloneToken, ProbeTarget
//	           list              ManagedSecrets: the sweep's list by label, as
//	                             metadata alone
//	           patch             SetCloneToken, OwnSecret
//	           delete            DeleteSecret
//	           deletecollection  DeleteRun: a cancelled run's Secrets
//
// No list or watch of Jobs and no get or watch of a pod: the runner polls the
// one Job it holds by name, and finds its pods by label.
var wantRunnerGrants = map[string][]string{
	"batch/jobs": {"create", "get", "patch", "delete", "deletecollection"},
	"/pods":      {"list"},
	"/pods/log":  {"get"},
	"/secrets":   {"create", "get", "list", "patch", "delete", "deletecollection"},
}

// renderedObject is one document of a rendered overlay: its identity, plus the
// node, so each gate decodes only the kinds it reasons about. Decoding every
// document into one wide struct would make an unrelated kind's field of the
// same name a decode error in a gate that never looks at it.
type renderedObject struct {
	Kind      string
	Name      string
	Namespace string
	node      *yaml.Node
}

func renderedObjects(t *testing.T, overlay string) []renderedObject {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(render(t, overlay)))
	var out []renderedObject
	for {
		var node yaml.Node
		err := dec.Decode(&node)
		if errors.Is(err, io.EOF) {
			break
		}
		// Fatal, as parse() is: a quiet stop would leave every gate below
		// asserting about a truncated prefix of the overlay.
		if err != nil {
			t.Fatalf("decoding document %d of the rendered %s overlay: %v", len(out)+1, overlay, err)
		}
		var head struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			} `yaml:"metadata"`
		}
		if err := node.Decode(&head); err != nil {
			t.Fatalf("decoding the head of document %d of the rendered %s overlay: %v", len(out)+1, overlay, err)
		}
		if head.Kind == "" {
			continue
		}
		out = append(out, renderedObject{
			Kind:      head.Kind,
			Name:      head.Metadata.Name,
			Namespace: head.Metadata.Namespace,
			node:      &node,
		})
	}
	if len(out) == 0 {
		t.Fatalf("the rendered %s overlay parsed to zero resources", overlay)
	}
	return out
}

func (o renderedObject) decode(t *testing.T, into any) {
	t.Helper()
	if err := o.node.Decode(into); err != nil {
		t.Fatalf("decoding %s/%s: %v", o.Kind, o.Name, err)
	}
}

func findObjects(objs []renderedObject, kind, name string) []renderedObject {
	var out []renderedObject
	for _, o := range objs {
		if o.Kind == kind && o.Name == name {
			out = append(out, o)
		}
	}
	return out
}

// theOne returns the single rendered object of kind/name, failing the test
// when there is none or more than one -- two would mean a sync that applies
// one of them and reports the other as drift forever.
func theOne(t *testing.T, objs []renderedObject, kind, name string) renderedObject {
	t.Helper()
	found := findObjects(objs, kind, name)
	if len(found) != 1 {
		t.Fatalf("%s/%s renders %d time(s), want exactly 1", kind, name, len(found))
	}
	return found[0]
}

type rbacSubject struct {
	Kind      string `yaml:"kind"`
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
}

type rbacBinding struct {
	Subjects []rbacSubject `yaml:"subjects"`
	RoleRef  struct {
		APIGroup string `yaml:"apiGroup"`
		Kind     string `yaml:"kind"`
		Name     string `yaml:"name"`
	} `yaml:"roleRef"`
}

type rbacRole struct {
	Rules []struct {
		APIGroups       []string `yaml:"apiGroups"`
		Resources       []string `yaml:"resources"`
		Verbs           []string `yaml:"verbs"`
		ResourceNames   []string `yaml:"resourceNames"`
		NonResourceURLs []string `yaml:"nonResourceURLs"`
	} `yaml:"rules"`
}

type pipelinesContainer struct {
	Name    string `yaml:"name"`
	EnvFrom []struct {
		ConfigMapRef *struct {
			Name string `yaml:"name"`
		} `yaml:"configMapRef"`
	} `yaml:"envFrom"`
	Env []struct {
		Name      string `yaml:"name"`
		ValueFrom *struct {
			ConfigMapKeyRef *struct {
				Name string `yaml:"name"`
			} `yaml:"configMapKeyRef"`
		} `yaml:"valueFrom"`
	} `yaml:"env"`
}

type pipelinesWorkload struct {
	Spec struct {
		Template struct {
			Spec struct {
				ServiceAccountName string               `yaml:"serviceAccountName"`
				InitContainers     []pipelinesContainer `yaml:"initContainers"`
				Containers         []pipelinesContainer `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

// readsPipelinesConfig reports whether any container of the workload takes
// env from the pipelines ConfigMap, by envFrom or by a single key.
func (w pipelinesWorkload) readsPipelinesConfig() bool {
	spec := w.Spec.Template.Spec
	for _, c := range slices.Concat(spec.InitContainers, spec.Containers) {
		for _, from := range c.EnvFrom {
			if from.ConfigMapRef != nil && from.ConfigMapRef.Name == pipelinesConfigMap {
				return true
			}
		}
		for _, e := range c.Env {
			if e.ValueFrom != nil && e.ValueFrom.ConfigMapKeyRef != nil &&
				e.ValueFrom.ConfigMapKeyRef.Name == pipelinesConfigMap {
				return true
			}
		}
	}
	return false
}

// TestPipelinesObjectsRenderInTheirNamespace is the placement gate: every
// object the component ships renders exactly once, in the namespace it
// belongs to, and the namespace itself exists and refuses privileged pods.
func TestPipelinesObjectsRenderInTheirNamespace(t *testing.T) {
	for _, overlay := range pipelinesOverlays {
		t.Run(overlay, func(t *testing.T) {
			objs := renderedObjects(t, overlay)
			for key, wantNS := range wantPipelinesObjects {
				kind, name, _ := strings.Cut(key, "/")
				found := findObjects(objs, kind, name)
				switch len(found) {
				case 0:
					t.Errorf("%s does not render; deploy/k8s/components/pipelines must be composed by the %s overlay", key, overlay)
				case 1:
					if got := found[0].Namespace; got != wantNS {
						t.Errorf("%s lands in namespace %q, want %q. An object the component means for %s that "+
							"comes out elsewhere means its explicit namespace was lost, or the overlay's "+
							"namespace transformer is no longer unsetOnly.", key, got, wantNS, pipelinesNamespace)
					}
				default:
					t.Errorf("%s renders %d times, want exactly 1", key, len(found))
				}
			}

			// The namespace is a resource in the reconciled set, like the mesh's
			// own: nothing creates it out of band.
			var ns struct {
				Metadata struct {
					Labels map[string]string `yaml:"labels"`
				} `yaml:"metadata"`
			}
			theOne(t, objs, "Namespace", pipelinesNamespace).decode(t, &ns)

			// Steps run images the platform did not build. Pod Security
			// admission at `baseline` is what refuses a privileged, host-path
			// or host-network pod there whatever spec reaches the API server.
			switch level := ns.Metadata.Labels["pod-security.kubernetes.io/enforce"]; level {
			case "baseline", "restricted":
			default:
				t.Errorf("the %s Namespace enforces Pod Security level %q, want baseline (or restricted). "+
					"Without it nothing at admission stops a privileged pod in the namespace that runs "+
					"images the platform did not build.", pipelinesNamespace, level)
			}
		})
	}
}

// TestPipelinesRoleGrantsJobsThereAndNothingElse pins the runner's grant to
// the table above, verb for verb, and confines it to the steps' namespace.
func TestPipelinesRoleGrantsJobsThereAndNothingElse(t *testing.T) {
	want := map[string]bool{}
	for groupResource, verbs := range wantRunnerGrants {
		for _, verb := range verbs {
			want[groupResource+" "+verb] = true
		}
	}

	for _, overlay := range pipelinesOverlays {
		t.Run(overlay, func(t *testing.T) {
			objs := renderedObjects(t, overlay)

			var role rbacRole
			theOne(t, objs, "Role", "memql-pipelines-runner").decode(t, &role)

			got := map[string]bool{}
			for _, rule := range role.Rules {
				if len(rule.ResourceNames) > 0 || len(rule.NonResourceURLs) > 0 {
					t.Errorf("a runner rule carries resourceNames %v / nonResourceURLs %v. Job and Secret names are "+
						"derived per step attempt, and RBAC cannot pin `create` to a name, so neither belongs here.",
						rule.ResourceNames, rule.NonResourceURLs)
				}
				for _, group := range rule.APIGroups {
					for _, resource := range rule.Resources {
						for _, verb := range rule.Verbs {
							got[group+"/"+resource+" "+verb] = true
						}
					}
				}
			}
			for grant := range want {
				if !got[grant] {
					t.Errorf("the runner Role does not grant %q; the runner makes that call and would be refused with a 403", grant)
				}
			}
			for grant := range got {
				if !want[grant] {
					t.Errorf("the runner Role grants %q, which is not in the runner's table. Every grant here is a "+
						"privilege of the memql-engine identity every mesh node runs as; a call the runner never "+
						"makes is privilege issued for nothing.", grant)
				}
			}

			for _, o := range objs {
				switch o.Kind {
				case "ClusterRole", "ClusterRoleBinding":
					if strings.Contains(o.Name, "pipelines") {
						t.Errorf("%s/%s renders. The pipelines grant is a Role confined to %s; a cluster-wide one "+
							"would hand the engine identity Jobs and Secrets in every namespace.", o.Kind, o.Name, pipelinesNamespace)
					}
				case "Role", "RoleBinding":
					if o.Namespace == pipelinesNamespace && o.Name != "memql-pipelines-runner" {
						t.Errorf("%s/%s renders in %s. That namespace carries the runner's grant and nothing else.",
							o.Kind, o.Name, pipelinesNamespace)
					}
				}
			}
		})
	}
}

// engineAccount is the ServiceAccount every engine Deployment runs as, the
// runner's included.
const engineAccount = "memql-engine"

// reachesTheEngine reports whether a binding's subject grants the engine
// identity what the binding binds: by its ServiceAccount's name (in any
// namespace, which only widens what is caught), as the user its token
// authenticates as, or through a group every ServiceAccount in its namespace,
// or every authenticated caller, belongs to.
func reachesTheEngine(s rbacSubject) bool {
	switch s.Kind {
	case "ServiceAccount":
		return s.Name == engineAccount
	case "User":
		return s.Name == "system:serviceaccount:"+cloudNamespace+":"+engineAccount
	case "Group":
		switch s.Name {
		case "system:serviceaccounts", "system:serviceaccounts:" + cloudNamespace, "system:authenticated":
			return true
		}
	}
	return false
}

// TestNoClusterRoleBindingReachesTheEngineIdentity (review M3): the pipelines
// grant is safe because it is a Role, bound in memql-pipelines alone. The
// check above finds a cluster-wide copy of it by NAME only; a
// ClusterRoleBinding of the engine identity under any other name would hand
// every engine node -- the workbench, whose token creates the step Jobs,
// among them -- whatever its ClusterRole holds, in every namespace. So no
// ClusterRoleBinding in any overlay's render may reach memql-engine at all.
func TestNoClusterRoleBindingReachesTheEngineIdentity(t *testing.T) {
	for _, overlay := range pipelinesOverlays {
		t.Run(overlay, func(t *testing.T) {
			objs := renderedObjects(t, overlay)

			// The reachable positive: the runner's RoleBinding names the engine,
			// and the matcher finds it there, so a clean result below is about
			// the cluster-wide bindings and not a matcher that matches nothing.
			var runner rbacBinding
			theOne(t, objs, "RoleBinding", "memql-pipelines-runner").decode(t, &runner)
			if !slices.ContainsFunc(runner.Subjects, reachesTheEngine) {
				t.Fatalf("the runner's RoleBinding subjects %+v do not read as the engine identity; this gate's matcher "+
					"would miss a ClusterRoleBinding of it too", runner.Subjects)
			}

			for _, o := range objs {
				if o.Kind != "ClusterRoleBinding" {
					continue
				}
				var binding rbacBinding
				o.decode(t, &binding)
				for _, s := range binding.Subjects {
					if reachesTheEngine(s) {
						t.Errorf("ClusterRoleBinding/%s binds ClusterRole %q to %s %q, which reaches the %s identity every "+
							"engine Deployment runs as: a cluster-wide grant to the workbench that creates step Jobs, in "+
							"every namespace. The engine's grants are Roles, each in its own namespace.",
							o.Name, binding.RoleRef.Name, s.Kind, s.Name, engineAccount)
					}
				}
			}
		})
	}
}

// TestPipelinesRoleBindsTheEngineIdentityOnly asserts the binding names the
// identity the workbench actually runs as, and nobody else.
//
// It is the shared memql-engine ServiceAccount rather than a workbench-only
// one: the workbench also makes model calls through workload identity
// federation, whose trust names memql-engine, so a dedicated account would cut
// it off from both vendors. The custom-domain Role binds the same account for
// the same reason (deploy/k8s/base/custom-domain-rbac.yaml).
func TestPipelinesRoleBindsTheEngineIdentityOnly(t *testing.T) {
	for _, overlay := range pipelinesOverlays {
		t.Run(overlay, func(t *testing.T) {
			objs := renderedObjects(t, overlay)

			var binding rbacBinding
			theOne(t, objs, "RoleBinding", "memql-pipelines-runner").decode(t, &binding)

			if r := binding.RoleRef; r.APIGroup != "rbac.authorization.k8s.io" || r.Kind != "Role" || r.Name != "memql-pipelines-runner" {
				t.Errorf("the binding's roleRef is %s %s/%s, want rbac.authorization.k8s.io Role/memql-pipelines-runner",
					r.APIGroup, r.Kind, r.Name)
			}
			want := rbacSubject{Kind: "ServiceAccount", Name: "memql-engine", Namespace: cloudNamespace}
			if len(binding.Subjects) != 1 || binding.Subjects[0] != want {
				t.Errorf("the binding's subjects are %+v, want exactly [%+v]", binding.Subjects, want)
			}

			// The subject is only right if it is who the workbench IS.
			wb := theOne(t, objs, "Deployment", "workbench")
			var w pipelinesWorkload
			wb.decode(t, &w)
			if got := w.Spec.Template.Spec.ServiceAccountName; got != want.Name || wb.Namespace != want.Namespace {
				t.Errorf("the workbench runs as ServiceAccount %q in %q, but the runner's grant binds %q in %q: "+
					"the node that creates the Jobs would hold none of the permission to do it",
					got, wb.Namespace, want.Name, want.Namespace)
			}
		})
	}
}

// TestPipelinesStepIdentityHoldsNothing is the step pod's side: it runs as an
// account that no token is mounted for and nothing is bound to, so a step --
// arbitrary code from a repository -- has no credential for the API server.
func TestPipelinesStepIdentityHoldsNothing(t *testing.T) {
	const stepAccount = "memql-pipelines-step"
	// A Group subject reaches every account in the namespace without naming
	// any of them.
	groupsReachingTheStep := map[string]bool{
		"system:serviceaccounts":                       true,
		"system:serviceaccounts:" + pipelinesNamespace: true,
	}

	for _, overlay := range pipelinesOverlays {
		t.Run(overlay, func(t *testing.T) {
			objs := renderedObjects(t, overlay)

			var sa struct {
				AutomountServiceAccountToken *bool `yaml:"automountServiceAccountToken"`
			}
			theOne(t, objs, "ServiceAccount", stepAccount).decode(t, &sa)
			if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
				t.Errorf("ServiceAccount %s does not set automountServiceAccountToken: false; a step pod would "+
					"then carry an API token whether or not the runner asks for one", stepAccount)
			}

			for _, o := range objs {
				if o.Kind != "RoleBinding" && o.Kind != "ClusterRoleBinding" {
					continue
				}
				var binding rbacBinding
				o.decode(t, &binding)
				for _, s := range binding.Subjects {
					if (s.Kind == "ServiceAccount" && s.Name == stepAccount) ||
						(s.Kind == "Group" && groupsReachingTheStep[s.Name]) {
						t.Errorf("%s/%s binds %s %q, which reaches the step identity. A step runs code from a "+
							"repository; its account must hold nothing.", o.Kind, o.Name, s.Kind, s.Name)
					}
				}
			}
		})
	}
}

type networkPolicySpec struct {
	PodSelector map[string]any   `yaml:"podSelector"`
	PolicyTypes []string         `yaml:"policyTypes"`
	Ingress     []map[string]any `yaml:"ingress"`
	Egress      []struct {
		To []struct {
			NamespaceSelector *struct {
				MatchLabels      map[string]string `yaml:"matchLabels"`
				MatchExpressions []any             `yaml:"matchExpressions"`
			} `yaml:"namespaceSelector"`
			PodSelector map[string]any `yaml:"podSelector"`
			IPBlock     *struct {
				CIDR   string   `yaml:"cidr"`
				Except []string `yaml:"except"`
			} `yaml:"ipBlock"`
		} `yaml:"to"`
		Ports []struct {
			Protocol string `yaml:"protocol"`
			Port     any    `yaml:"port"`
			EndPort  *int   `yaml:"endPort"`
		} `yaml:"ports"`
	} `yaml:"egress"`
}

// TestPipelinesNetworkPolicyIsolatesTheNamespace pins what a step can reach:
// cluster DNS, and the internet minus every private and link-local range and
// Azure's WireServer (168.63.129.16) -- so not the mesh, the database, another
// pod, a node, the cloud's instance-metadata endpoint or the WireServer's
// provisioning material, while `git fetch`, a module proxy and a package
// registry still work. Nothing may connect IN. (What the object cannot pin is
// enforcement: that needs a network policy engine in the cluster -- the
// component's README says where there is one.)
func TestPipelinesNetworkPolicyIsolatesTheNamespace(t *testing.T) {
	wantExcept := []string{"10.0.0.0/8", "168.63.129.16/32", "169.254.0.0/16", "172.16.0.0/12", "192.168.0.0/16"}

	for _, overlay := range pipelinesOverlays {
		t.Run(overlay, func(t *testing.T) {
			objs := renderedObjects(t, overlay)
			var np struct {
				Spec networkPolicySpec `yaml:"spec"`
			}
			theOne(t, objs, "NetworkPolicy", "memql-pipelines-isolate").decode(t, &np)
			spec := np.Spec

			if len(spec.PodSelector) != 0 {
				t.Errorf("podSelector is %v, want {} -- the policy must select every pod in the namespace, "+
					"including a step's service sidecars", spec.PodSelector)
			}
			types := slices.Sorted(slices.Values(spec.PolicyTypes))
			if !slices.Equal(types, []string{"Egress", "Ingress"}) {
				t.Errorf("policyTypes is %v, want [Ingress Egress]. A type that is not listed is not "+
					"restricted at all.", spec.PolicyTypes)
			}
			if len(spec.Ingress) != 0 {
				t.Errorf("the policy admits %d ingress rule(s), want none: nothing connects to a step", len(spec.Ingress))
			}

			var sawDNS, sawInternet bool
			for i, rule := range spec.Egress {
				if len(rule.To) != 1 {
					t.Errorf("egress rule %d names %d peers, want exactly 1", i, len(rule.To))
					continue
				}
				peer := rule.To[0]
				switch {
				case peer.NamespaceSelector != nil && peer.IPBlock == nil:
					sawDNS = true
					sel := peer.NamespaceSelector
					if len(sel.MatchLabels) != 1 || sel.MatchLabels["kubernetes.io/metadata.name"] != "kube-system" ||
						len(sel.MatchExpressions) != 0 || len(peer.PodSelector) != 0 {
						t.Errorf("egress rule %d selects namespaces %v / pods %v, want exactly kube-system (cluster DNS)",
							i, sel.MatchLabels, peer.PodSelector)
					}
					var ports []string
					for _, p := range rule.Ports {
						if p.EndPort != nil {
							t.Errorf("egress rule %d opens a port range ending at %d; DNS is port 53 alone", i, *p.EndPort)
						}
						ports = append(ports, fmt.Sprintf("%s/%v", p.Protocol, p.Port))
					}
					slices.Sort(ports)
					if !slices.Equal(ports, []string{"TCP/53", "UDP/53"}) {
						t.Errorf("egress rule %d opens %v into kube-system, want exactly [TCP/53 UDP/53]", i, ports)
					}
				case peer.IPBlock != nil && peer.NamespaceSelector == nil && len(peer.PodSelector) == 0:
					sawInternet = true
					if peer.IPBlock.CIDR != "0.0.0.0/0" {
						t.Errorf("egress rule %d allows %s, want 0.0.0.0/0 minus the private ranges", i, peer.IPBlock.CIDR)
					}
					if got := slices.Sorted(slices.Values(peer.IPBlock.Except)); !slices.Equal(got, wantExcept) {
						t.Errorf("egress rule %d excepts %v, want exactly %v. A missing range is a step reaching "+
							"the mesh, the database or the instance-metadata endpoint.", i, got, wantExcept)
					}
					if len(rule.Ports) != 0 {
						t.Errorf("egress rule %d restricts ports %v; the internet rule is every port", i, rule.Ports)
					}
				default:
					t.Errorf("egress rule %d is neither the DNS rule nor the internet rule: %+v", i, peer)
				}
			}
			if !sawDNS || !sawInternet || len(spec.Egress) != 2 {
				t.Errorf("the policy carries %d egress rule(s) (DNS=%v internet=%v), want exactly those two",
					len(spec.Egress), sawDNS, sawInternet)
			}
		})
	}
}

type pipelinesLimits struct {
	Spec struct {
		Limits []struct {
			Type           string            `yaml:"type"`
			Default        map[string]string `yaml:"default"`
			DefaultRequest map[string]string `yaml:"defaultRequest"`
		} `yaml:"limits"`
	} `yaml:"spec"`
}

type pipelinesQuota struct {
	Spec struct {
		Hard map[string]string `yaml:"hard"`
	} `yaml:"spec"`
}

// stated reads one overlay's pipelines-values.yaml and returns the document of
// the given kind, or nil when the overlay does not state it.
func stated(t *testing.T, overlay, kind string) *yaml.Node {
	t.Helper()
	path := filepath.Join(overlay, pipelinesValuesFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v -- an overlay that composes the pipelines component states its values there", path, err)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	for {
		var node yaml.Node
		err := dec.Decode(&node)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			t.Fatalf("decoding %s: %v", path, err)
		}
		var head struct {
			Kind string `yaml:"kind"`
		}
		if err := node.Decode(&head); err != nil {
			t.Fatalf("decoding %s: %v", path, err)
		}
		if head.Kind == kind {
			return &node
		}
	}
}

// statedNamed is stated for the document of the given kind AND name, or nil.
func statedNamed(t *testing.T, overlay, kind, name string) *yaml.Node {
	t.Helper()
	path := filepath.Join(overlay, pipelinesValuesFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	for {
		var node yaml.Node
		err := dec.Decode(&node)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			t.Fatalf("decoding %s: %v", path, err)
		}
		var head struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
		}
		if err := node.Decode(&head); err != nil {
			t.Fatalf("decoding %s: %v", path, err)
		}
		if head.Kind == kind && head.Metadata.Name == name {
			return &node
		}
	}
}

// TestPipelinesCeilingAndLimitsAreValues covers the two numbers that bound
// what steps can take from a cluster.
//
// The CEILING is a ResourceQuota on count/jobs.batch: the API server counts it
// atomically across every replica of the workbench, so two replicas cannot
// both create the Job that exceeds it; the runner waits on `exceeded quota`
// instead of failing. The LIMITS are a LimitRange default, so the runner sets
// no resources and this is the one place a step's size is decided.
//
// Both are an overlay's VALUES -- what a laptop and a cloud node pool can give
// differ -- so each overlay states them in pipelines-values.yaml rather than
// inheriting the component's, this asserts the stated value is the one that
// renders, and pipelinesSizing pins what each overlay states.
//
// The quota counts Jobs and NOTHING ELSE: a compute quota is enforced when the
// Job's pod is created, after the Job exists, so a step it refused would wait
// inside its own activeDeadlineSeconds and time out having never run.
func TestPipelinesCeilingAndLimitsAreValues(t *testing.T) {
	for _, overlay := range pipelinesOverlays {
		t.Run(overlay, func(t *testing.T) {
			want, ok := pipelinesSizing[overlay]
			if !ok {
				t.Fatalf("pipelinesSizing has no entry for %s", overlay)
			}
			objs := renderedObjects(t, overlay)

			var quota pipelinesQuota
			theOne(t, objs, "ResourceQuota", "memql-pipelines-ceiling").decode(t, &quota)
			ceiling, ok := quota.Spec.Hard["count/jobs.batch"]
			if !ok {
				t.Fatalf("the ceiling sets no count/jobs.batch (hard: %v); nothing bounds how many steps run at once",
					quota.Spec.Hard)
			}
			if n, err := strconv.Atoi(ceiling); err != nil || n <= 0 {
				t.Errorf("count/jobs.batch is %q, want a positive integer -- zero refuses every step forever", ceiling)
			}
			if ceiling != want.ceiling {
				t.Errorf("count/jobs.batch is %q, want %q (pipelinesSizing says why)", ceiling, want.ceiling)
			}
			for resource := range quota.Spec.Hard {
				if resource != "count/jobs.batch" {
					t.Errorf("the ceiling also limits %q. It counts Jobs and nothing else: a compute quota refuses "+
						"at POD creation, after the Job exists, so the step waits out its own deadline unrun.", resource)
				}
			}

			var limits pipelinesLimits
			theOne(t, objs, "LimitRange", "memql-pipelines-limits").decode(t, &limits)
			var containerItems int
			for _, item := range limits.Spec.Limits {
				if item.Type != "Container" {
					continue
				}
				containerItems++
				for res, pinned := range map[string][2]string{
					"cpu":               {want.limitCPU, want.requestCPU},
					"memory":            {want.limitMemory, want.requestMemory},
					"ephemeral-storage": {want.limitDisk, want.requestDisk},
				} {
					if item.Default[res] != pinned[0] || item.DefaultRequest[res] != pinned[1] {
						t.Errorf("the Container %s default limit / request is %q / %q, want %q / %q "+
							"(pipelinesSizing says why)", res, item.Default[res], item.DefaultRequest[res], pinned[0], pinned[1])
					}
				}
				for _, res := range []string{"cpu", "memory", "ephemeral-storage"} {
					lim, req := item.Default[res], item.DefaultRequest[res]
					if lim == "" || req == "" {
						t.Errorf("the Container limits leave %s unset (default %q, defaultRequest %q); a step "+
							"would then run unbounded or unscheduled by whatever the node has", res, lim, req)
						continue
					}
					// The API server refuses a LimitRange whose default request
					// exceeds its default limit -- at sync time, not here.
					if res == "cpu" && milliCPU(t, overlay, req) > milliCPU(t, overlay, lim) ||
						res != "cpu" && mebibytes(t, overlay, req) > mebibytes(t, overlay, lim) {
						t.Errorf("the default %s request %s exceeds the default limit %s; the API server refuses "+
							"that LimitRange", res, req, lim)
					}
				}
			}
			if containerItems != 1 {
				t.Errorf("the LimitRange carries %d Container item(s), want exactly 1", containerItems)
			}

			// STATED, not inherited.
			quotaNode, limitsNode := stated(t, overlay, "ResourceQuota"), stated(t, overlay, "LimitRange")
			if quotaNode == nil || limitsNode == nil {
				t.Fatalf("%s/%s states ResourceQuota=%v LimitRange=%v; both are this overlay's values, and an "+
					"inherited number is one nobody decided", overlay, pipelinesValuesFile, quotaNode != nil, limitsNode != nil)
			}
			var statedQuota pipelinesQuota
			var statedLimits pipelinesLimits
			if err := quotaNode.Decode(&statedQuota); err != nil {
				t.Fatalf("decoding the stated ResourceQuota: %v", err)
			}
			if err := limitsNode.Decode(&statedLimits); err != nil {
				t.Fatalf("decoding the stated LimitRange: %v", err)
			}
			if got, want := ceiling, statedQuota.Spec.Hard["count/jobs.batch"]; got != want {
				t.Errorf("the ceiling renders %q but %s states %q", got, pipelinesValuesFile, want)
			}
			if fmt.Sprint(limits.Spec.Limits) != fmt.Sprint(statedLimits.Spec.Limits) {
				t.Errorf("the limits render %+v but %s states %+v", limits.Spec.Limits, pipelinesValuesFile, statedLimits.Spec.Limits)
			}
		})
	}
}

// TestThePipelinesWorkspaceLimitIsTheLimitRanges (review M4): the workbench
// gives every step's workspace a sizeLimit, MEMQL_PIPELINES_WORKSPACE_LIMIT
// from the pipelines ConfigMap, and the LimitRange gives every container a
// default ephemeral-storage limit. The two are one decision: the LimitRange's
// bounds the whole pod, the workspace included, and the same number on the
// workspace bounds it however many services share that sum and shows the
// bound on the Job. So in every overlay the rendered ConfigMap carries the
// rendered LimitRange's number, and the overlay states it beside the
// LimitRange in pipelines-values.yaml rather than inheriting the component's.
func TestThePipelinesWorkspaceLimitIsTheLimitRanges(t *testing.T) {
	const key = "MEMQL_PIPELINES_WORKSPACE_LIMIT"
	for _, overlay := range pipelinesOverlays {
		t.Run(overlay, func(t *testing.T) {
			objs := renderedObjects(t, overlay)

			var limits pipelinesLimits
			theOne(t, objs, "LimitRange", "memql-pipelines-limits").decode(t, &limits)
			var disk string
			for _, item := range limits.Spec.Limits {
				if item.Type == "Container" {
					disk = item.Default["ephemeral-storage"]
				}
			}
			if disk == "" {
				t.Fatal("the LimitRange gives a container no default ephemeral-storage limit; nothing bounds a step's disk")
			}

			var cm struct {
				Data map[string]string `yaml:"data"`
			}
			theOne(t, objs, "ConfigMap", pipelinesConfigMap).decode(t, &cm)
			if got := cm.Data[key]; got != disk {
				t.Errorf("the %s ConfigMap sets %s=%q, but the LimitRange's default ephemeral-storage limit is %q: "+
					"a step's workspace would show one bound while the kubelet enforces another", pipelinesConfigMap, key, got, disk)
			}

			// STATED, not inherited: the ConfigMap's number is this overlay's,
			// next to the LimitRange it must equal.
			node := statedNamed(t, overlay, "ConfigMap", pipelinesConfigMap)
			if node == nil {
				t.Fatalf("%s/%s states no %s ConfigMap; the workspace limit is this overlay's value, beside its LimitRange",
					overlay, pipelinesValuesFile, pipelinesConfigMap)
			}
			var stated struct {
				Data map[string]string `yaml:"data"`
			}
			if err := node.Decode(&stated); err != nil {
				t.Fatalf("decoding the stated ConfigMap: %v", err)
			}
			if stated.Data[key] != disk {
				t.Errorf("%s/%s states %s=%q, want the LimitRange's %q", overlay, pipelinesValuesFile, key, stated.Data[key], disk)
			}
		})
	}
}

// TestPipelinesCacheClassAndAccessModeAreTheOverlaysValues pins the cache
// volume per overlay (see pipelinesCache for why each value is what it is).
func TestPipelinesCacheClassAndAccessModeAreTheOverlaysValues(t *testing.T) {
	for _, overlay := range pipelinesOverlays {
		t.Run(overlay, func(t *testing.T) {
			want, ok := pipelinesCache[overlay]
			if !ok {
				t.Fatalf("pipelinesCache has no entry for %s", overlay)
			}
			var pvc struct {
				Spec struct {
					StorageClassName *string  `yaml:"storageClassName"`
					AccessModes      []string `yaml:"accessModes"`
					Resources        struct {
						Requests map[string]string `yaml:"requests"`
					} `yaml:"resources"`
				} `yaml:"spec"`
			}
			theOne(t, renderedObjects(t, overlay), "PersistentVolumeClaim", "memql-pipelines-cache").decode(t, &pvc)

			switch got := pvc.Spec.StorageClassName; {
			case want.storageClass == nil && got != nil:
				t.Errorf("the cache claims storageClassName %q, want the field ABSENT so the cluster's default "+
					"class provisions it (an empty string disables dynamic provisioning outright)", *got)
			case want.storageClass != nil && (got == nil || *got != *want.storageClass):
				t.Errorf("the cache claims storageClassName %v, want %q", orAbsent(got), *want.storageClass)
			}
			if !slices.Equal(pvc.Spec.AccessModes, []string{want.accessMode}) {
				t.Errorf("the cache's accessModes are %v, want [%s]", pvc.Spec.AccessModes, want.accessMode)
			}
			if pvc.Spec.Resources.Requests["storage"] == "" {
				t.Error("the cache requests no storage size; the claim is refused without one")
			}
		})
	}
}

// TestWorkbenchReadsThePipelinesConfig is the delivery half: the workbench --
// and only the workbench, the one binary that contains the runner -- takes its
// pipelines env from the ConfigMap, and that env points where the component
// put everything else.
func TestWorkbenchReadsThePipelinesConfig(t *testing.T) {
	for _, overlay := range pipelinesOverlays {
		t.Run(overlay, func(t *testing.T) {
			objs := renderedObjects(t, overlay)

			var sawWorkbench bool
			for _, o := range objs {
				if o.Kind != "Deployment" {
					continue
				}
				var w pipelinesWorkload
				o.decode(t, &w)
				reads := w.readsPipelinesConfig()
				switch {
				case o.Name == "workbench":
					sawWorkbench = true
					if !reads {
						t.Errorf("the workbench does not take env from the %s ConfigMap; the runner would not "+
							"know which namespace its Jobs go to or which image clones the repository", pipelinesConfigMap)
					}
				case reads:
					t.Errorf("Deployment %s takes env from the %s ConfigMap; only the workbench runs steps", o.Name, pipelinesConfigMap)
				}
			}
			if !sawWorkbench {
				t.Fatal("no workbench Deployment rendered")
			}

			var cm struct {
				Data map[string]string `yaml:"data"`
			}
			theOne(t, objs, "ConfigMap", pipelinesConfigMap).decode(t, &cm)
			if got := cm.Data["MEMQL_PIPELINES_NAMESPACE"]; got != pipelinesNamespace {
				t.Errorf("MEMQL_PIPELINES_NAMESPACE is %q, but the component renders its Role, quota and limits in %q: "+
					"the runner would create Jobs where it holds no grant", got, pipelinesNamespace)
			}
			// The clone container holds the repository token. A tag alone is
			// whatever was pushed to it last, so the image is pinned by digest.
			image := cm.Data["MEMQL_PIPELINES_CLONE_IMAGE"]
			if _, digest, ok := strings.Cut(image, "@sha256:"); !ok || len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
				t.Errorf("MEMQL_PIPELINES_CLONE_IMAGE is %q, want a reference pinned by @sha256:<64 hex>. The clone "+
					"container is handed the repository token; whoever controls a mutable tag controls that token.", image)
			}
		})
	}
}

// TestOnlyTheLocalCacheIsMarkedToBindOnFirstUse is the overlay half of the
// health coupling: the local cache claim carries the mark, so the dev
// cluster's Application is not held at Progressing until the first pipeline
// runs; the cloud ones do not, so a cache whose class is missing there still
// reads Progressing.
func TestOnlyTheLocalCacheIsMarkedToBindOnFirstUse(t *testing.T) {
	for _, overlay := range pipelinesOverlays {
		t.Run(overlay, func(t *testing.T) {
			want, ok := pipelinesCache[overlay]
			if !ok {
				t.Fatalf("pipelinesCache has no entry for %s", overlay)
			}
			var pvc struct {
				Metadata struct {
					Annotations map[string]string `yaml:"annotations"`
				} `yaml:"metadata"`
			}
			theOne(t, renderedObjects(t, overlay), "PersistentVolumeClaim", "memql-pipelines-cache").decode(t, &pvc)

			value, marked := pvc.Metadata.Annotations[bindsOnFirstUseAnnotation]
			switch {
			case want.bindsOnFirstUse && value != "true":
				t.Errorf("the %s cache claim does not carry %s: \"true\" (got %q, present=%v). Its class binds on "+
					"first use, so it stays Pending until a step mounts it, and Argo CD holds the whole "+
					"Application at Progressing for as long as no pipeline has run.",
					overlay, bindsOnFirstUseAnnotation, value, marked)
			case !want.bindsOnFirstUse && marked:
				t.Errorf("the %s cache claim carries %s: %q. On this overlay a Pending cache can mean its class "+
					"does not exist; the mark would report that as Healthy.", overlay, bindsOnFirstUseAnnotation, value)
			}
		})
	}
}

// upstreamArgoCDInstall is the remote resource deploy/argocd/bootstrap pulls.
var upstreamArgoCDInstall = regexp.MustCompile(`https://raw\.githubusercontent\.com/argoproj/argo-cd/[^/\s]+/manifests/install\.yaml`)

// upstreamStandIn is what the bootstrap's patches need of that remote install:
// the objects they target, as upstream ships them -- argocd-cm verbatim from
// the v2.13.3 install (no namespace, no data), argocd-repo-server cut to the
// container repo-server-exec-timeout.yaml names. The deploy gates never touch
// the network, so the render below swaps the URL for this file and keeps the
// bootstrap kustomization otherwise as it is. A new bootstrap patch on another
// upstream object fails that render naming the missing Id: add the object here.
const upstreamStandIn = `apiVersion: v1
kind: ConfigMap
metadata:
  labels:
    app.kubernetes.io/name: argocd-cm
    app.kubernetes.io/part-of: argocd
  name: argocd-cm
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: argocd-repo-server
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: argocd-repo-server
  template:
    metadata:
      labels:
        app.kubernetes.io/name: argocd-repo-server
    spec:
      containers:
        - name: argocd-repo-server
          image: quay.io/argoproj/argocd:v2.13.3
`

// TestTheArgoCDBootstrapReadsAMarkedUnboundClaimAsHealthy is the other half:
// the Argo CD the bootstrap installs carries the PersistentVolumeClaim health
// customization that reads the mark, in argocd-cm, in argocd's namespace.
//
// What a Go test cannot do is run the Lua. That was verified against the
// pinned release's own evaluator (`argocd admin settings resource-overrides
// health` in quay.io/argoproj/argocd:v2.13.3), which answers exactly as Argo
// CD's built-in check does for a Bound, Lost, unmarked Pending and phase-less
// claim, and Healthy for a marked Pending one. These assertions keep the
// script naming what that verification depended on.
func TestTheArgoCDBootstrapReadsAMarkedUnboundClaimAsHealthy(t *testing.T) {
	const healthKey = "resource.customizations.health.PersistentVolumeClaim"

	src := filepath.Join("..", "..", "argocd", "bootstrap")
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("reading %s: %v", src, err)
	}
	dir := t.TempDir()
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		body, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		if e.Name() == "kustomization.yaml" {
			if n := len(upstreamArgoCDInstall.FindAll(body, -1)); n != 1 {
				t.Fatalf("the bootstrap names the upstream install %d time(s), want exactly 1; this gate swaps "+
					"that one remote resource for upstreamStandIn", n)
			}
			body = upstreamArgoCDInstall.ReplaceAll(body, []byte("upstream-standin.yaml"))
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "upstream-standin.yaml"), []byte(upstreamStandIn), 0o644); err != nil {
		t.Fatal(err)
	}

	cm := theOne(t, renderedObjects(t, dir), "ConfigMap", "argocd-cm")
	if cm.Namespace != "argocd" {
		t.Errorf("argocd-cm renders in namespace %q, want argocd -- Argo CD reads its settings nowhere else", cm.Namespace)
	}
	var settings struct {
		Data map[string]string `yaml:"data"`
	}
	cm.decode(t, &settings)
	lua, ok := settings.Data[healthKey]
	if !ok {
		t.Fatalf("argocd-cm carries no %s, so Argo CD reads every Pending claim as Progressing and the local "+
			"Application never reports Healthy until a pipeline has run", healthKey)
	}
	for _, needle := range []string{
		// The mark, exactly as the local overlay writes it.
		`annotations["` + bindsOnFirstUseAnnotation + `"] == "true"`,
		`"binds when the first pipeline step mounts it"`,
		// Argo CD's own answer for every other claim: a customization replaces
		// the built-in check, so the script must give all of it.
		`phase == "Bound"`, `"Healthy"`,
		`phase == "Pending"`, `"Progressing"`,
		`phase == "Lost"`, `"Degraded"`,
		`"Unknown"`,
	} {
		if !strings.Contains(lua, needle) {
			t.Errorf("the %s script does not contain %s", healthKey, needle)
		}
	}
}

func orAbsent(s *string) string {
	if s == nil {
		return "<absent>"
	}
	return *s
}
