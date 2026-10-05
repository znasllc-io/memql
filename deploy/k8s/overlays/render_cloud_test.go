// Package overlays holds the render gates on the cloud overlay -- the ones
// about the overlay's relationship to the base and to ArgoCD, rather than about
// any one manifest in it.
//
// WHY A TEST AND NOT A REVIEW. Each of these decays silently. A replica count
// that stops being stated and starts being inherited, a namespace a resource
// carries that the transformer did not reach, an Application filename that
// never makes it into root.yaml's brace list: none of those break a build,
// none fail to reconcile, and the first symptom of the last one is a cluster
// that is simply not running. Rendering the overlay and reading it is the
// difference between believing the shape and knowing it.
//
// This file replaces environments_test.go (533 lines), which asserted that TWO
// overlays rendered the same system. Epic memql#3943 removed "environment" as a
// product concept, so there is one cloud overlay and nothing to compare it to;
// what survives here is every assertion that was about the overlay itself
// rather than about the pair.
package overlays

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	// cloudOverlay is the one cloud overlay, and cloudNamespace is where all of
	// it must land.
	cloudOverlay   = "cloud"
	cloudNamespace = "memql"
	// cloudApp is the ArgoCD Application that reconciles it.
	cloudApp = "memql"
	// cloudReplicas is the committed count for a request-serving mesh node.
	cloudReplicas = 2
)

// meshDeployments are the node types the cluster runs. Listed for the same
// reason the local overlay's render gate lists them: the failure this catches is
// a node type arriving and the wiring not covering it, and a discovered list
// would grow with the tree and assert nothing.
var meshDeployments = []string{
	"identity", "bff", "agent", "planner",
	"workbench", "mcp", "edge",
}

// render builds an overlay with whichever renderer the machine has.
func render(t *testing.T, dir string) string {
	t.Helper()
	for _, cmd := range [][]string{
		{"kustomize", "build", dir},
		{"kubectl", "kustomize", dir},
	} {
		if _, err := exec.LookPath(cmd[0]); err != nil {
			continue
		}
		out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s failed: %v\n%s", strings.Join(cmd, " "), err, out)
		}
		return string(out)
	}
	t.Skip("neither kustomize nor kubectl is installed; cannot render the overlay")
	return ""
}

// resource is the slice of a rendered document these gates reason about.
//
// The Service fields (Type, ExternalTrafficPolicy, LoadBalancerSourceRanges)
// are the LoadBalancer-only surface a hold on a module removes; they decode to
// their zero values on every other kind.
type resource struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name        string            `yaml:"name"`
		Namespace   string            `yaml:"namespace"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
	Data map[string]string `yaml:"data"`
	Spec struct {
		Replicas                 *int     `yaml:"replicas"`
		Type                     string   `yaml:"type"`
		ExternalTrafficPolicy    string   `yaml:"externalTrafficPolicy"`
		LoadBalancerSourceRanges []string `yaml:"loadBalancerSourceRanges"`
	} `yaml:"spec"`
}

func parse(t *testing.T, rendered string) []resource {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	var out []resource
	for {
		var r resource
		err := dec.Decode(&r)
		if errors.Is(err, io.EOF) {
			break
		}
		// A mid-stream decode error is FATAL rather than a break. Stopping
		// quietly at the first bad document would leave every gate below
		// asserting about a truncated prefix of the overlay -- they would all
		// pass, having checked a fraction of it.
		if err != nil {
			t.Fatalf("decoding document %d of the rendered overlay: %v", len(out)+1, err)
		}
		if r.Kind == "" {
			continue
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		t.Fatal("the rendered overlay parsed to zero resources")
	}
	return out
}

// TestTheCloudOverlayLandsWhollyInOneNamespace is the isolation the design
// rests on at the Kubernetes layer -- with ONE deliberate exception.
//
// The mesh namespace is the overlay's NamespaceTransformer
// (namespace-transformer.yaml, `unsetOnly: true`), which fills in every
// resource that states no namespace; base states `memql` on everything it
// renders, which is why the base needs no namespace-bearing manifest of its
// own. The risk is a resource that states a namespace of its own, which
// unsetOnly keeps: it lands outside the mesh, where the bare Service names
// every intra-mesh address uses stop resolving, and unless the AppProject
// permits that namespace the sync refuses it.
//
// THE EXCEPTION is the pipelines substrate (epic memql#5478, task #5492): a
// second namespace, memql-pipelines, rendered on purpose by
// deploy/k8s/components/pipelines for the Jobs pipeline steps run as. Exactly
// two Namespace objects are accepted, and only the objects wantPipelinesObjects
// places in memql-pipelines may land there; anything else outside memql still
// fails.
func TestTheCloudOverlayLandsWhollyInOneNamespace(t *testing.T) {
	assertTheMeshNamespaceAndThePipelinesOne(t, cloudOverlay)
}

// assertTheMeshNamespaceAndThePipelinesOne is the body of both instance
// overlays' one-namespace gates (TestCloudEntryLandsWhollyInOneNamespace is
// the other), so the exception is stated once.
func assertTheMeshNamespaceAndThePipelinesOne(t *testing.T, overlay string) {
	t.Helper()
	var namespaces []string
	for _, r := range parse(t, render(t, overlay)) {
		if r.Kind == "Namespace" {
			namespaces = append(namespaces, r.Metadata.Name)
			continue
		}
		// A cluster-scoped resource legitimately carries none.
		if r.Metadata.Namespace == "" || r.Metadata.Namespace == cloudNamespace {
			continue
		}
		if r.Metadata.Namespace == pipelinesNamespace &&
			wantPipelinesObjects[r.Kind+"/"+r.Metadata.Name] == pipelinesNamespace {
			continue
		}
		t.Errorf("%s/%s lands in namespace %q; the %s overlay renders into %q and, for the pipelines "+
			"substrate's own objects alone, %q -- anything else is outside the mesh it was meant for",
			r.Kind, r.Metadata.Name, r.Metadata.Namespace, overlay, cloudNamespace, pipelinesNamespace)
	}
	slices.Sort(namespaces)
	if want := []string{cloudNamespace, pipelinesNamespace}; !slices.Equal(namespaces, want) {
		t.Errorf("the %s overlay renders Namespace objects %v, want exactly %v -- a namespace that does not "+
			"render is one the overlay relies on existing out of band", overlay, namespaces, want)
	}
}

// TestNothingFullyQualifiesAClusterDNSName guards the property the base is
// standing on.
//
// Every intra-mesh address in base is a BARE Service name -- `identity:50061`,
// `bff-active:50058`, `workbench=workbench:50060` -- which cluster DNS resolves
// inside the pod's own namespace. That is the entire reason the namespace
// transformer is sufficient: rewrite the metadata and a self-contained mesh
// falls out, with no namespace named in any manifest that does not own it.
//
// A fully-qualified name would break it silently. `identity.memql.svc.cluster.
// local` in base pins the mesh to one namespace, and if a second install ever
// shared the cluster it would RESOLVE -- to the other install's identity node,
// so one mesh would quietly verify its tokens against the other's JWKS while
// every manifest still looked correct. Nothing about the symptom would point
// here.
func TestNothingFullyQualifiesAClusterDNSName(t *testing.T) {
	for n, line := range strings.Split(render(t, cloudOverlay), "\n") {
		if strings.Contains(line, "svc.cluster.local") {
			t.Errorf("line %d fully-qualifies a cluster DNS name, which pins the mesh to one namespace "+
				"and would resolve across an install boundary rather than failing:\n  %s", n+1, strings.TrimSpace(line))
		}
	}
}

// TestCommittedReplicaCountsAreStatedNotInherited is the property the overlay's
// own comment claims, checked.
//
// An INHERITED count is a number nobody decided. It reconciles to whatever base
// happens to say, which is fine until base moves for an unrelated reason and
// the cluster silently changes width. Stating it in the overlay makes the width
// a reviewed value; this asserts every mesh node actually carries one.
func TestCommittedReplicaCountsAreStatedNotInherited(t *testing.T) {
	byName := map[string]resource{}
	for _, r := range parse(t, render(t, cloudOverlay)) {
		if r.Kind == "Deployment" {
			byName[r.Metadata.Name] = r
		}
	}
	for _, node := range meshDeployments {
		r, ok := byName[node]
		if !ok {
			t.Errorf("%s does not render; the overlay is missing a node type the mesh needs", node)
			continue
		}
		if r.Spec.Replicas == nil {
			t.Errorf("%s declares no replica count", node)
			continue
		}
		want := cloudReplicas
		if *r.Spec.Replicas != want {
			t.Errorf("%s has %d replicas, want %d", node, *r.Spec.Replicas, want)
		}
	}
}

// TestNoEnvironmentConfigMapIsRendered is the post-epic assertion.
//
// The `memql-environment` ConfigMap carried MEMQL_ENVIRONMENT, the schema
// search path and the front-door host label -- the three values that made a
// namespace an environment. Epic memql#3943 deleted all three, so its presence
// here would mean one of them came back through a file nobody re-read.
func TestNoEnvironmentConfigMapIsRendered(t *testing.T) {
	for _, r := range parse(t, render(t, cloudOverlay)) {
		if r.Kind == "ConfigMap" && r.Metadata.Name == "memql-environment" {
			t.Errorf("the memql-environment ConfigMap rendered with data %v; epic memql#3943 removed "+
				"the environment concept and every value it carried", r.Data)
		}
	}
}

// argoApplication is the slice of an ArgoCD Application these gates read.
type argoApplication struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Project string `yaml:"project"`
		Source  struct {
			RepoURL string `yaml:"repoURL"`
			Path    string `yaml:"path"`
		} `yaml:"source"`
		Destination struct {
			Server    string `yaml:"server"`
			Namespace string `yaml:"namespace"`
		} `yaml:"destination"`
		SyncPolicy struct {
			Automated map[string]any `yaml:"automated"`
		} `yaml:"syncPolicy"`
		IgnoreDifferences []struct {
			Group        string   `yaml:"group"`
			Kind         string   `yaml:"kind"`
			JSONPointers []string `yaml:"jsonPointers"`
		} `yaml:"ignoreDifferences"`
	} `yaml:"spec"`
}

func readApplication(t *testing.T, name string) argoApplication {
	t.Helper()
	path := filepath.Join("..", "..", "argocd", "apps", name+".yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var app argoApplication
	if err := yaml.Unmarshal(b, &app); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return app
}

// TestTheApplicationPointsAtTheCloudOverlay wires the two halves together: an
// overlay nothing reconciles is a directory, and an Application pointed at the
// wrong path deploys something else without any file looking wrong.
//
// The /spec/replicas exclusion is checked because it is load-bearing rather
// than tidy. `make scale N=<n>` writes that field directly, and with selfHeal
// on and the field in the diff ArgoCD would scale it straight back within a
// reconcile interval -- leaving the operator doing manual repair after every
// scale.
func TestTheApplicationPointsAtTheCloudOverlay(t *testing.T) {
	app := readApplication(t, cloudApp)

	if app.Kind != "Application" {
		t.Errorf("kind is %q, want Application", app.Kind)
	}
	if app.Metadata.Name != cloudApp {
		t.Errorf("name is %q, want %q", app.Metadata.Name, cloudApp)
	}
	if app.Spec.Project != "memql" {
		t.Errorf("project is %q, want memql -- the AppProject is what bounds the blast radius", app.Spec.Project)
	}
	if want := "deploy/k8s/overlays/" + cloudOverlay; app.Spec.Source.Path != want {
		t.Errorf("source path is %q, want %q", app.Spec.Source.Path, want)
	}
	if app.Spec.Destination.Namespace != cloudNamespace {
		t.Errorf("destination namespace is %q, want %q", app.Spec.Destination.Namespace, cloudNamespace)
	}
	if app.Spec.Destination.Server != "https://kubernetes.default.svc" {
		t.Errorf("destination server is %q, want the in-cluster API server", app.Spec.Destination.Server)
	}
	// Manual sync is deliberate: this is the highest-stakes surface and an
	// operator triggers every sync explicitly (deploy/argocd/README.md).
	if len(app.Spec.SyncPolicy.Automated) > 0 {
		t.Error("the Application carries an `automated:` block; sync is manual by design until a couple of clean GitOps deploys have happened")
	}

	var ignoresReplicas bool
	for _, ig := range app.Spec.IgnoreDifferences {
		if ig.Kind != "Deployment" {
			continue
		}
		for _, p := range ig.JSONPointers {
			if p == "/spec/replicas" {
				ignoresReplicas = true
			}
		}
	}
	if !ignoresReplicas {
		t.Error("the Application does not exclude /spec/replicas from drift detection, so selfHeal would revert every `make scale`")
	}
}

// TestTheAppOfAppsRendersTheApplication is the silent-failure gate.
//
// `directory.include` in root.yaml is a hardcoded brace list, so a manifest
// dropped into deploy/argocd/apps is NOT rendered until its filename appears
// there. Nothing fails: the Application simply does not exist, the namespace is
// never reconciled, and the only symptom is a cluster that is not running. The
// AppProject is the same shape of trap from the other end -- an Application
// whose destination namespace is missing from `destinations` is rejected at
// sync time rather than at review time. The Application renders into TWO
// namespaces since the pipelines substrate (memql#5492), so both must be
// permitted: memql for the mesh, memql-pipelines for the Jobs steps run as.
func TestTheAppOfAppsRendersTheApplication(t *testing.T) {
	root, err := os.ReadFile(filepath.Join("..", "..", "argocd", "apps", "root.yaml"))
	if err != nil {
		t.Fatalf("reading root.yaml: %v", err)
	}
	project, err := os.ReadFile(filepath.Join("..", "..", "argocd", "apps", "project.yaml"))
	if err != nil {
		t.Fatalf("reading project.yaml: %v", err)
	}

	if !strings.Contains(string(root), cloudApp+".yaml") {
		t.Errorf("root.yaml's include glob does not name %s.yaml, so the app-of-apps never renders it and the cluster is simply not deployed", cloudApp)
	}
	// Parsed, not substring-matched: "namespace: memql" is a substring of
	// "namespace: memql-pipelines", so a text check for the mesh namespace
	// could no longer fail once the second one is listed.
	var appProject struct {
		Spec struct {
			Destinations []struct {
				Server    string `yaml:"server"`
				Namespace string `yaml:"namespace"`
			} `yaml:"destinations"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(project, &appProject); err != nil {
		t.Fatalf("parsing project.yaml: %v", err)
	}
	permitted := map[string]bool{}
	for _, d := range appProject.Spec.Destinations {
		if d.Server == "https://kubernetes.default.svc" {
			permitted[d.Namespace] = true
		}
	}
	for _, ns := range []string{cloudNamespace, pipelinesNamespace} {
		if !permitted[ns] {
			t.Errorf("the memql AppProject does not permit namespace %s on the in-cluster API server, so the %s "+
				"Application's objects there are rejected at sync", ns, cloudApp)
		}
	}
	// The retired second Application must not linger in either file: an include
	// naming a file that does not exist is silently skipped by ArgoCD, and a
	// permitted namespace nothing reconciles is a standing grant.
	for _, retired := range []string{"memql-prod", "memql-staging"} {
		if strings.Contains(string(root), retired+".yaml") {
			t.Errorf("root.yaml still includes %s.yaml, which epic memql#3943 removed", retired)
		}
		if strings.Contains(string(project), "namespace: "+retired) {
			t.Errorf("the memql AppProject still permits namespace %s, which epic memql#3943 removed", retired)
		}
	}
}
