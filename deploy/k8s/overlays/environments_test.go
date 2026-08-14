// Package overlays holds the gates that are about the RELATIONSHIP between the
// environment overlays rather than about any one of them.
//
// WHY A TEST AND NOT A REVIEW. The whole claim of epic memql#3748 is that
// staging and production are the same system with different values in it -- one
// cluster, one ArgoCD, one base, two namespaces. That claim decays silently.
// A patch added to one overlay and not the other, a node that stops mounting
// the environment ConfigMap, a search path that loses `public` and takes
// TimescaleDB with it: none of those break a build, and the first two make the
// two environments diverge in SHAPE while every file still looks reasonable on
// its own. Rendering both and comparing is the difference between believing
// they are twins and knowing it.
package overlays

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// environments is the closed set this epic creates, paired with what each is
// expected to carry. Listed rather than discovered from the directory: the
// failure worth catching is a THIRD environment arriving and nobody deciding
// what its schema should be, which discovery would wave through.
var environments = []struct {
	dir        string // overlay directory, relative to this file
	namespace  string // every namespaced resource must land here
	label      string // MEMQL_ENVIRONMENT, matching v1:cluster:node.environment's enum
	schema     string // the FIRST schema on the search path -- where every write lands
	replicas   int    // the committed count for a request-serving mesh node
	argoApp    string // the ArgoCD Application that reconciles it
	autoSynced bool   // whether that Application carries an `automated:` block
}{
	{
		dir: "prod", namespace: "memql-prod", label: "production",
		schema: "memql_prod", replicas: 2,
		argoApp: "memql-prod", autoSynced: false,
	},
	{
		dir: "staging", namespace: "memql-staging", label: "staging",
		schema: "memql_staging", replicas: 2,
		argoApp: "memql-staging", autoSynced: true,
	},
}

// meshDeployments are the node types both environments run. Listed for the same
// reason the local overlay's render gate lists them: the failure this catches is
// a node type arriving and the wiring not covering it, and a discovered list
// would grow with the tree and assert nothing.
var meshDeployments = []string{
	"identity", "bff", "cognition", "agent", "planner",
	"workbench", "mcp", "voice", "voice-agent", "edge",
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
	t.Skip("neither kustomize nor kubectl is installed; cannot render the overlays")
	return ""
}

// resource is the slice of a rendered document these gates reason about.
type resource struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Data map[string]string `yaml:"data"`
	Spec struct {
		Replicas *int `yaml:"replicas"`
		Template struct {
			Spec struct {
				Containers []struct {
					Name    string `yaml:"name"`
					EnvFrom []struct {
						ConfigMapRef *struct {
							Name     string `yaml:"name"`
							Optional *bool  `yaml:"optional"`
						} `yaml:"configMapRef"`
					} `yaml:"envFrom"`
				} `yaml:"containers"`
			} `yaml:"spec"`
		} `yaml:"template"`
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

// TestEachEnvironmentLandsWhollyInItsOwnNamespace is the isolation the whole
// design rests on at the Kubernetes layer.
//
// Kustomize's namespace transformer is doing the work: it rewrites
// metadata.namespace on every namespaced resource AND metadata.name on the
// Namespace object base/namespace.yaml declares, which is why one base can
// serve two environments with no per-environment manifest. The risk it carries
// is a resource that names a namespace somewhere the transformer does not
// reach -- and one such survivor means two environments sharing a workload
// while their manifests look separate.
func TestEachEnvironmentLandsWhollyInItsOwnNamespace(t *testing.T) {
	for _, env := range environments {
		t.Run(env.dir, func(t *testing.T) {
			var sawNamespaceObject bool
			for _, r := range parse(t, render(t, env.dir)) {
				if r.Kind == "Namespace" {
					sawNamespaceObject = true
					if r.Metadata.Name != env.namespace {
						t.Errorf("the Namespace object is named %q, want %q", r.Metadata.Name, env.namespace)
					}
					continue
				}
				// A cluster-scoped resource legitimately carries none.
				if r.Metadata.Namespace == "" {
					continue
				}
				if r.Metadata.Namespace != env.namespace {
					t.Errorf("%s/%s lands in namespace %q, want %q -- a resource the namespace transformer did not reach means the two environments share it",
						r.Kind, r.Metadata.Name, r.Metadata.Namespace, env.namespace)
				}
			}
			if !sawNamespaceObject {
				t.Errorf("no Namespace object rendered; the overlay is relying on the namespace existing out of band")
			}
		})
	}
}

// TestNothingAddressesAnEnvironmentByName guards the property one base is
// standing on.
//
// Every intra-mesh address in base is a BARE Service name -- `identity:50061`,
// `bff-active:50058`, `workbench=workbench:50060` -- which cluster DNS resolves
// inside the pod's own namespace. That is the entire reason the namespace
// transformer is sufficient: rewrite the metadata and two independent meshes
// fall out, with no per-environment manifest anywhere.
//
// A fully-qualified name would break it silently and in the worst direction.
// `identity.memql.svc.cluster.local` in base does not fail to resolve -- it
// resolves, to the OTHER environment's identity node, so staging's mesh would
// quietly verify its tokens against production's JWKS while every manifest
// still looked correct. Nothing about the symptom would point here.
func TestNothingAddressesAnEnvironmentByName(t *testing.T) {
	for i, env := range environments {
		other := environments[(i+1)%len(environments)]
		t.Run(env.dir, func(t *testing.T) {
			for n, line := range strings.Split(render(t, env.dir), "\n") {
				if strings.Contains(line, "svc.cluster.local") {
					t.Errorf("line %d fully-qualifies a cluster DNS name, which pins it to one namespace and "+
						"resolves to the other environment's node rather than failing:\n  %s", n+1, strings.TrimSpace(line))
				}
				if strings.Contains(line, other.namespace) {
					t.Errorf("line %d names %s inside %s's manifests:\n  %s",
						n+1, other.namespace, env.dir, strings.TrimSpace(line))
				}
			}
		})
	}
}

// TestBothEnvironmentsRenderTheSameSystem is the acceptance criterion in test
// form: "Both Applications reconcile from the same base; a diff of the two
// overlays contains only values."
//
// It compares the SET of (kind, name) pairs, which is what "the same system"
// means operationally -- the same workloads, services, configmaps and jobs. It
// deliberately does NOT compare their contents: the contents are exactly where
// the values differ, and asserting on them would be asserting the two
// environments are identical rather than that they are the same shape.
func TestBothEnvironmentsRenderTheSameSystem(t *testing.T) {
	inventory := func(dir string) []string {
		var out []string
		for _, r := range parse(t, render(t, dir)) {
			// The Namespace's NAME is a value here, uniquely: it is the one
			// object whose identity the environment changes.
			if r.Kind == "Namespace" {
				out = append(out, "Namespace")
				continue
			}
			out = append(out, r.Kind+"/"+r.Metadata.Name)
		}
		sort.Strings(out)
		return out
	}

	a, b := inventory(environments[0].dir), inventory(environments[1].dir)
	if strings.Join(a, "\n") == strings.Join(b, "\n") {
		return
	}

	in := func(set []string, want string) bool {
		i := sort.SearchStrings(set, want)
		return i < len(set) && set[i] == want
	}
	for _, r := range a {
		if !in(b, r) {
			t.Errorf("%s renders %s and %s does not -- the two environments have diverged in SHAPE, which is the drift one base exists to prevent",
				environments[0].dir, r, environments[1].dir)
		}
	}
	for _, r := range b {
		if !in(a, r) {
			t.Errorf("%s renders %s and %s does not -- the two environments have diverged in SHAPE, which is the drift one base exists to prevent",
				environments[1].dir, r, environments[0].dir)
		}
	}
}

// TestEnvironmentConfigMapCarriesTheBoundary checks the two values that ARE the
// environment, and checks the search path against the properties that were
// measured rather than assumed (component/database/search_path.go).
//
// The `public` assertions are the design's safety margin, so they get a test
// that would fail if either half were ever dropped:
//
//   - `public` must NOT be first. An unset or mistyped search path falls back
//     to `"$user", public`, so an environment living there is what a
//     configuration slip silently resolves to. parseSearchPath refuses it at
//     boot; this refuses it at review.
//   - `public` must be PRESENT. The TimescaleDB extension's functions live in
//     it, and a path without it fails the first create_hypertable call with an
//     error naming neither the search path nor the fix.
func TestEnvironmentConfigMapCarriesTheBoundary(t *testing.T) {
	for _, env := range environments {
		t.Run(env.dir, func(t *testing.T) {
			var found bool
			for _, r := range parse(t, render(t, env.dir)) {
				if r.Kind != "ConfigMap" || r.Metadata.Name != "memql-environment" {
					continue
				}
				found = true

				if got := r.Data["MEMQL_ENVIRONMENT"]; got != env.label {
					t.Errorf("MEMQL_ENVIRONMENT is %q, want %q (the v1:cluster:node.environment enum, stamped at registration)", got, env.label)
				}

				raw := r.Data["MEMQL_DB_SEARCH_PATH"]
				var schemas []string
				for _, part := range strings.Split(raw, ",") {
					if s := strings.TrimSpace(part); s != "" {
						schemas = append(schemas, s)
					}
				}
				if len(schemas) == 0 {
					t.Fatalf("MEMQL_DB_SEARCH_PATH is %q, which names no schema", raw)
				}
				if schemas[0] != env.schema {
					t.Errorf("the first schema on the search path is %q, want %q -- the first schema is where every unqualified write lands", schemas[0], env.schema)
				}
				if schemas[0] == "public" {
					t.Error("public may not be the first schema: an unset or mistyped search path falls back to \"$user\", public, so an environment living there is what a configuration slip silently resolves to")
				}
				var hasPublic bool
				for _, s := range schemas {
					if s == "public" {
						hasPublic = true
					}
				}
				if !hasPublic {
					t.Errorf("MEMQL_DB_SEARCH_PATH is %q and does not include public -- the TimescaleDB extension's functions live there, and the migrations call them by bare name", raw)
				}
			}
			if !found {
				t.Error("no memql-environment ConfigMap rendered; this environment has no boundary and its nodes would fall back to \"$user\", public")
			}
		})
	}
}

// TestEveryNodeReadsTheEnvironmentConfigMap is the same failure mode the local
// overlay's domain gate exists for, one layer down: the wiring is declared once
// per node in base, and a node that does not carry it silently runs on the
// default search path -- which is to say, in the wrong environment's data, or
// in an empty `public` where the first query fails.
//
// `optional: true` is asserted too, and it is not cosmetic. It is what lets the
// SAME base serve a single-environment cluster that ships no such ConfigMap --
// every local one. Drop it and `make up` breaks with CreateContainerConfigError
// on every pod.
func TestEveryNodeReadsTheEnvironmentConfigMap(t *testing.T) {
	for _, env := range environments {
		t.Run(env.dir, func(t *testing.T) {
			byName := map[string]resource{}
			for _, r := range parse(t, render(t, env.dir)) {
				if r.Kind == "Deployment" {
					byName[r.Metadata.Name] = r
				}
			}
			for _, node := range meshDeployments {
				r, ok := byName[node]
				if !ok {
					t.Errorf("no Deployment named %s in the rendered overlay", node)
					continue
				}
				var mounted bool
				for _, c := range r.Spec.Template.Spec.Containers {
					for _, ef := range c.EnvFrom {
						if ef.ConfigMapRef == nil || ef.ConfigMapRef.Name != "memql-environment" {
							continue
						}
						mounted = true
						if ef.ConfigMapRef.Optional == nil || !*ef.ConfigMapRef.Optional {
							t.Errorf("%s mounts memql-environment without optional:true; that breaks every single-environment cluster, which ships no such ConfigMap", node)
						}
					}
				}
				if !mounted {
					t.Errorf("%s does not mount the memql-environment ConfigMap, so it runs on the default search path -- which is either another environment's data or an empty public", node)
				}
			}
		})
	}
}

// TestCommittedReplicaCountsAreStatedNotInherited checks the counts, and checks
// that both overlays state them.
//
// Stating a value in both places is what makes the acceptance criterion true --
// a count inherited from base here and overridden there is a STRUCTURAL
// difference wearing a number's clothes, and the diff of the two overlays would
// no longer be "only values".
func TestCommittedReplicaCountsAreStatedNotInherited(t *testing.T) {
	// voice-agent is a room dispatcher rather than a request-serving mesh node:
	// one replica serves up to MEMQL_VOICE_MAX_ROOMS rooms, so its count does
	// not track the rest of the mesh in either environment.
	const dispatcher = "voice-agent"

	for _, env := range environments {
		t.Run(env.dir, func(t *testing.T) {
			byName := map[string]resource{}
			for _, r := range parse(t, render(t, env.dir)) {
				if r.Kind == "Deployment" {
					byName[r.Metadata.Name] = r
				}
			}
			for _, node := range meshDeployments {
				r, ok := byName[node]
				if !ok {
					continue // reported by the ConfigMap gate above
				}
				if r.Spec.Replicas == nil {
					t.Errorf("%s declares no replica count", node)
					continue
				}
				want := env.replicas
				if node == dispatcher && want > 1 {
					want = 1
				}
				if *r.Spec.Replicas != want {
					t.Errorf("%s has %d replicas, want %d", node, *r.Spec.Replicas, want)
				}
			}
		})
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

// TestEachEnvironmentHasAnApplicationPointedAtItsOwnOverlay wires the two
// halves together: an overlay nothing reconciles is a directory, and an
// Application pointed at the wrong overlay deploys staging into production's
// namespace without any file looking wrong.
//
// The /spec/replicas exclusion is checked because it is load-bearing rather
// than tidy. `make scale N=0 ENV=staging` is what makes a second environment
// cost storage instead of compute, and with selfHeal on and that field in the
// diff, ArgoCD scales it straight back up within a reconcile interval. The
// acceptance criterion is "scales to zero and back WITHOUT MANUAL REPAIR", and
// this field is the entire difference between that and suspending auto-sync
// every time.
func TestEachEnvironmentHasAnApplicationPointedAtItsOwnOverlay(t *testing.T) {
	for _, env := range environments {
		t.Run(env.argoApp, func(t *testing.T) {
			app := readApplication(t, env.argoApp)

			if app.Kind != "Application" {
				t.Errorf("kind is %q, want Application", app.Kind)
			}
			if app.Metadata.Name != env.argoApp {
				t.Errorf("name is %q, want %q", app.Metadata.Name, env.argoApp)
			}
			if app.Spec.Project != "memql" {
				t.Errorf("project is %q, want memql -- the AppProject is what bounds the blast radius", app.Spec.Project)
			}
			if want := "deploy/k8s/overlays/" + env.dir; app.Spec.Source.Path != want {
				t.Errorf("source path is %q, want %q", app.Spec.Source.Path, want)
			}
			if app.Spec.Destination.Namespace != env.namespace {
				t.Errorf("destination namespace is %q, want %q", app.Spec.Destination.Namespace, env.namespace)
			}
			// ONE cluster is the point of the epic. A second API server here
			// would be the second estate this change exists to collapse.
			if app.Spec.Destination.Server != "https://kubernetes.default.svc" {
				t.Errorf("destination server is %q, want the in-cluster API server -- both environments live in ONE cluster", app.Spec.Destination.Server)
			}
			if got := len(app.Spec.SyncPolicy.Automated) > 0; got != env.autoSynced {
				t.Errorf("auto-sync is %v, want %v", got, env.autoSynced)
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
				t.Error("the Application does not exclude /spec/replicas from drift detection, so selfHeal reverts every `make scale` and scaling to zero needs manual repair")
			}
		})
	}
}

// TestTheAppOfAppsRendersBothApplications is the silent-failure gate.
//
// `directory.include` in root.yaml is a hardcoded brace list, so a manifest
// dropped into deploy/argocd/apps is NOT rendered until its filename appears
// there. Nothing fails: the Application simply does not exist, the namespace is
// never reconciled, and the only symptom is an environment that is not running.
// The AppProject is the same shape of trap from the other end -- an Application
// whose destination namespace is missing from `destinations` is rejected at
// sync time rather than at review time.
func TestTheAppOfAppsRendersBothApplications(t *testing.T) {
	root, err := os.ReadFile(filepath.Join("..", "..", "argocd", "apps", "root.yaml"))
	if err != nil {
		t.Fatalf("reading root.yaml: %v", err)
	}
	project, err := os.ReadFile(filepath.Join("..", "..", "argocd", "apps", "project.yaml"))
	if err != nil {
		t.Fatalf("reading project.yaml: %v", err)
	}

	for _, env := range environments {
		if !strings.Contains(string(root), env.argoApp+".yaml") {
			t.Errorf("root.yaml's include glob does not name %s.yaml, so the app-of-apps never renders it and %s is simply not deployed",
				env.argoApp, env.dir)
		}
		if !strings.Contains(string(project), "namespace: "+env.namespace) {
			t.Errorf("the memql AppProject does not permit namespace %s, so %s is rejected at sync",
				env.namespace, env.argoApp)
		}
	}
}
