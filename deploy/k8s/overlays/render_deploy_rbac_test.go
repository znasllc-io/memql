// Render gates on the deploy console's in-cluster RBAC (memql#4257).
//
// component/deploycontrol runs on the identity node and, in a cluster, reaches
// ArgoCD through the Kubernetes API with a ServiceAccount rather than through a
// kubectl binary the distroless image does not contain. Three things have to be
// true for that to work, and the third is the one that fails silently.
//
//  1. the identity pod runs as `memql-deploy` (not `default`);
//  2. `memql-deploy` holds read-only Rollouts / AnalysisRuns in the mesh
//     namespace;
//  3. `memql-deploy` holds get+patch on ONE named Application IN ARGOCD'S
//     NAMESPACE -- and that grant cannot be shipped in deploy/k8s/base.
//
// WHY (3) IS A GATE AND NOT A COMMENT. It was in the base first. Every overlay
// sets `namespace: memql`, and kustomize's namespace transformer rewrites
// metadata.namespace on every namespaced resource it accumulates -- including
// one that states `argocd` explicitly. Measured on all three overlays: the Role
// and its RoleBinding both came out in `memql`. That is not an error at any
// point an operator would see. It applies cleanly, it binds cleanly, and it
// grants nothing; the first symptom is a repair failing with a 403 that names a
// ServiceAccount which looks correctly bound in every manifest.
//
// So the Application grant lives in deploy/argocd/apps/deploy-console-rbac.yaml,
// rendered by the app-of-apps root with `directory:` rather than kustomize, and
// this file asserts BOTH halves of that arrangement: the argocd objects must be
// absent from every overlay, and present, correctly namespaced, and REGISTERED
// IN THE INCLUDE GLOB in the operator tree. The glob matters on its own --
// root.yaml's own header says a manifest whose filename is not listed "does not
// fail, it silently does not exist".
package overlays

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	deploySA        = "memql-deploy"
	deployArgoRole  = "memql-deploy-argocd"
	deployMeshRole  = "memql-deploy-rollouts"
	argoRBACFile    = "../../argocd/apps/deploy-console-rbac.yaml"
	argoRootAppFile = "../../argocd/apps/root.yaml"
	// argoCDNamespace is where ArgoCD itself lives -- deliberately NOT
	// cloudNamespace, and the whole point of this file.
	argoCDNamespace = "argocd"
)

// rbacOverlays are the overlays an operator reconciles a cluster from. Listed
// rather than discovered, for the reason meshDeployments is listed: the failure
// this catches is an overlay arriving without the wiring, and a discovered list
// would grow with the tree and assert nothing.
var rbacOverlays = []string{"cloud", "cloud-entry", "local"}

// rbacResource is the slice of an RBAC document these gates read.
type rbacResource struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Rules []struct {
		APIGroups     []string `yaml:"apiGroups"`
		Resources     []string `yaml:"resources"`
		ResourceNames []string `yaml:"resourceNames"`
		Verbs         []string `yaml:"verbs"`
	} `yaml:"rules"`
	Subjects []struct {
		Kind      string `yaml:"kind"`
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"subjects"`
	RoleRef struct {
		Kind string `yaml:"kind"`
		Name string `yaml:"name"`
	} `yaml:"roleRef"`
	Spec struct {
		Template struct {
			Spec struct {
				ServiceAccountName string `yaml:"serviceAccountName"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

func parseRBAC(t *testing.T, rendered string) []rbacResource {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	var out []rbacResource
	for i := 0; ; i++ {
		var r rbacResource
		err := dec.Decode(&r)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decoding document %d: %v", i+1, err)
		}
		if r.Kind == "" {
			continue
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		t.Fatal("parsed zero documents")
	}
	return out
}

// TestIdentityRunsAsTheDeployServiceAccount covers (1) and (2), and the
// negative half of (3).
func TestIdentityRunsAsTheDeployServiceAccount(t *testing.T) {
	for _, overlay := range rbacOverlays {
		t.Run(overlay, func(t *testing.T) {
			docs := parseRBAC(t, render(t, overlay))
			if len(docs) < len(meshDeployments) {
				t.Fatalf("rendered only %d documents -- coverage floor", len(docs))
			}

			var sawSA, sawIdentityBinding, sawMeshRole, sawMeshBinding bool
			for _, d := range docs {
				switch {
				case d.Kind == "ServiceAccount" && d.Metadata.Name == deploySA:
					sawSA = true
					if d.Metadata.Namespace != cloudNamespace {
						t.Errorf("ServiceAccount %s lands in %q, want %q", deploySA, d.Metadata.Namespace, cloudNamespace)
					}
				case d.Kind == "Deployment" && d.Metadata.Name == "identity":
					if got := d.Spec.Template.Spec.ServiceAccountName; got != deploySA {
						t.Errorf("the identity pod runs as %q, want %q -- without it the deploy "+
							"substrate presents the default ServiceAccount, which holds nothing "+
							"on argoproj.io and 403s on the first repair", got, deploySA)
					} else {
						sawIdentityBinding = true
					}
				case d.Kind == "Role" && d.Metadata.Name == deployMeshRole:
					sawMeshRole = true
					assertReadOnlyRolloutRole(t, d)
				case d.Kind == "RoleBinding" && d.Metadata.Name == deployMeshRole:
					sawMeshBinding = true
					assertBindsDeploySA(t, d)

				// THE RELOCATION TRAP. If the argocd-namespace grant is ever
				// moved back into deploy/k8s/base, this is where it surfaces --
				// as a Role named `memql-deploy-argocd` that the namespace
				// transformer has quietly moved into the mesh namespace.
				case d.Metadata.Name == deployArgoRole:
					t.Errorf("%s/%s is rendered by the %s overlay, in namespace %q. The grant on "+
						"ArgoCD's Application must NOT be composed through an overlay: the "+
						"`namespace:` transformer rewrites it to %q, where it applies cleanly, "+
						"binds cleanly and grants nothing. It belongs in %s, which the app-of-apps "+
						"root renders with `directory:` (memql#4257).",
						d.Kind, d.Metadata.Name, overlay, d.Metadata.Namespace, cloudNamespace, argoRBACFile)
				}
			}
			for what, ok := range map[string]bool{
				"the memql-deploy ServiceAccount":    sawSA,
				"serviceAccountName on identity":     sawIdentityBinding,
				"the read-only rollouts Role":        sawMeshRole,
				"the read-only rollouts RoleBinding": sawMeshBinding,
			} {
				if !ok {
					t.Errorf("%s does not render", what)
				}
			}
		})
	}
}

// assertReadOnlyRolloutRole pins the mesh-namespace grant to reads. The
// substrate refuses the rollout promote/abort verb by name, so a write grant
// here would be a privilege issued for a call that is never made.
func assertReadOnlyRolloutRole(t *testing.T, d rbacResource) {
	t.Helper()
	allowed := map[string]bool{"get": true, "list": true}
	for _, rule := range d.Rules {
		for _, v := range rule.Verbs {
			if !allowed[v] {
				t.Errorf("%s grants %q on %v. The substrate makes no write call in this "+
					"namespace -- promote/abort is a kubectl plugin it refuses by name -- so a "+
					"write verb here is privilege issued for a call that is never made.",
					d.Metadata.Name, v, rule.Resources)
			}
		}
		if len(rule.APIGroups) != 1 || rule.APIGroups[0] != "argoproj.io" {
			t.Errorf("%s grants apiGroups %v, want exactly [argoproj.io]", d.Metadata.Name, rule.APIGroups)
		}
	}
}

func assertBindsDeploySA(t *testing.T, d rbacResource) {
	t.Helper()
	if len(d.Subjects) != 1 {
		t.Errorf("%s binds %d subjects, want exactly 1", d.Metadata.Name, len(d.Subjects))
		return
	}
	s := d.Subjects[0]
	if s.Kind != "ServiceAccount" || s.Name != deploySA || s.Namespace != cloudNamespace {
		t.Errorf("%s binds %s/%s in %q, want ServiceAccount/%s in %q",
			d.Metadata.Name, s.Kind, s.Name, s.Namespace, deploySA, cloudNamespace)
	}
}

// TestArgoApplicationGrantIsOperatorAppliedAndRegistered covers the positive
// half of (3): the grant exists in the operator tree, states argocd, is pinned
// to one Application by name, and is LISTED IN THE INCLUDE GLOB.
func TestArgoApplicationGrantIsOperatorAppliedAndRegistered(t *testing.T) {
	raw, err := os.ReadFile(argoRBACFile)
	if err != nil {
		t.Fatalf("reading %s: %v -- it is the grant that makes repair possible in a cluster", argoRBACFile, err)
	}
	docs := parseRBAC(t, string(raw))

	var sawRole, sawBinding bool
	for _, d := range docs {
		if d.Metadata.Namespace != argoCDNamespace {
			t.Errorf("%s/%s states namespace %q, want %q", d.Kind, d.Metadata.Name, d.Metadata.Namespace, argoCDNamespace)
		}
		switch d.Kind {
		case "Role":
			sawRole = true
			if len(d.Rules) != 1 {
				t.Fatalf("the Role carries %d rules, want exactly 1", len(d.Rules))
			}
			r := d.Rules[0]
			// resourceNames is the whole point: ArgoCD's namespace holds every
			// installation's Application, so an unpinned patch grant reaches a
			// neighbour's.
			if len(r.ResourceNames) != 1 || r.ResourceNames[0] != "memql" {
				t.Errorf("the Role's resourceNames is %v, want exactly [memql]. Unpinned, this "+
					"token could patch any Application in ArgoCD's namespace.", r.ResourceNames)
			}
			allowed := map[string]bool{"get": true, "patch": true}
			for _, v := range r.Verbs {
				if !allowed[v] {
					t.Errorf("the Role grants %q; repair is a get and two merge patches, nothing else", v)
				}
			}
		case "RoleBinding":
			sawBinding = true
			assertBindsDeploySA(t, d)
			if d.RoleRef.Kind != "Role" || d.RoleRef.Name != deployArgoRole {
				t.Errorf("the RoleBinding points at %s/%s, want Role/%s", d.RoleRef.Kind, d.RoleRef.Name, deployArgoRole)
			}
		}
	}
	if !sawRole || !sawBinding {
		t.Fatalf("%s carries Role=%v Binding=%v; both are required", argoRBACFile, sawRole, sawBinding)
	}

	// THE INCLUDE GLOB IS THE REGISTRATION. root.yaml's own header says an
	// unlisted manifest "does not fail, it silently does not exist" -- which for
	// an RBAC grant means a repair that 403s with every file in the repository
	// looking correct.
	root, err := os.ReadFile(argoRootAppFile)
	if err != nil {
		t.Fatalf("reading %s: %v", argoRootAppFile, err)
	}
	if !strings.Contains(string(root), "deploy-console-rbac.yaml") {
		t.Errorf("%s does not list deploy-console-rbac.yaml in its directory.include glob, so the "+
			"app-of-apps never renders it and the grant silently does not exist", argoRootAppFile)
	}
}
