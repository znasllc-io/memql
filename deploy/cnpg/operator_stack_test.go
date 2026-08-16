// Package cnpg holds the static guards for the database operator stack --
// CloudNativePG, the Barman Cloud plugin, and the cert-manager install they
// depend on (epic memql#3842, task memql#3845).
//
// # Why these are tests
//
// Every property guarded here fails SILENTLY or DESTRUCTIVELY, and none of them
// fails at review:
//
//   - An Application whose filename is missing from root.yaml's `directory.include`
//     brace list is not rendered. Nothing errors; the app simply does not exist,
//     and the only symptom is a database operator that was never installed.
//   - The Barman plugin's manifest declares Certificate and Issuer objects, so
//     if the CNPG Application syncs before cert-manager's CRDs are served it
//     does not degrade -- it fails to apply. The sync-waves are the only thing
//     ordering them in the cloud, and reordering two annotations looks like
//     nothing.
//   - `prune: true` on an operator that owns CRDs deletes the CRDs, and with
//     `clusters.postgresql.cnpg.io` goes every Cluster in the estate and the
//     data behind it. This is the one entry here whose failure mode is not
//     "quietly broken" but "quietly gone".
package cnpg

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// cnpgRepoRoot resolves the repository root from this file's location.
func cnpgRepoRoot(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate the repository root")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(self)))
}

func cnpgRead(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(cnpgRepoRoot(t), rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return b
}

// operatorApplication is the slice of an ArgoCD Application these guards read.
type operatorApplication struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name        string            `yaml:"name"`
		Annotations map[string]string `yaml:"annotations"`
	} `yaml:"metadata"`
	Spec struct {
		Project string `yaml:"project"`
		Source  struct {
			RepoURL        string `yaml:"repoURL"`
			Path           string `yaml:"path"`
			TargetRevision string `yaml:"targetRevision"`
		} `yaml:"source"`
		Destination struct {
			Server    string `yaml:"server"`
			Namespace string `yaml:"namespace"`
		} `yaml:"destination"`
		SyncPolicy struct {
			Automated *struct {
				Prune    bool `yaml:"prune"`
				SelfHeal bool `yaml:"selfHeal"`
			} `yaml:"automated"`
			SyncOptions []string `yaml:"syncOptions"`
		} `yaml:"syncPolicy"`
	} `yaml:"spec"`
}

// operatorApps is the closed set this task installs, paired with what each must
// carry. Listed rather than discovered: the failure worth catching is a THIRD
// operator arriving and nobody deciding where it sits in the ordering, which
// discovery would wave through.
var operatorApps = []struct {
	file      string // manifest filename under deploy/argocd/apps
	name      string
	path      string // the directory it reconciles
	namespace string
	wave      int // sync-wave; lower syncs first
}{
	{file: "cert-manager.yaml", name: "cert-manager", path: "deploy/cert-manager/install", namespace: "cert-manager", wave: -2},
	{file: "cnpg-operator.yaml", name: "cnpg-operator", path: "deploy/cnpg/install", namespace: "cnpg-system", wave: -1},
}

func loadOperatorApp(t *testing.T, file string) operatorApplication {
	t.Helper()
	var app operatorApplication
	if err := yaml.Unmarshal(cnpgRead(t, filepath.Join("deploy", "argocd", "apps", file)), &app); err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}
	return app
}

// TestOperatorApplicationsAreRegisteredInTheAppOfApps is the silent-failure gate.
//
// `directory.include` in root.yaml is a hardcoded brace list. A manifest sitting
// in that directory but absent from the list is NOT rendered -- the Application
// does not exist, no namespace is reconciled, and the only symptom is that the
// database operator was never installed. There is nothing red to click on.
func TestOperatorApplicationsAreRegisteredInTheAppOfApps(t *testing.T) {
	root := string(cnpgRead(t, "deploy/argocd/apps/root.yaml"))

	for _, app := range operatorApps {
		if !strings.Contains(root, app.file) {
			t.Errorf("root.yaml's directory.include does not name %s, so the app-of-apps never renders it "+
				"and %s is simply not installed -- with no failing sync to look at", app.file, app.name)
		}
	}
}

// TestCertManagerSyncsBeforeCloudNativePG guards the ordering the Barman plugin
// depends on.
//
// The plugin's manifest declares cert-manager Certificate and Issuer objects for
// its mTLS with the operator. Without those CRDs already served the CNPG
// Application fails to apply outright. Two annotations are the only thing
// expressing that in the cloud.
func TestCertManagerSyncsBeforeCloudNativePG(t *testing.T) {
	waves := map[string]int{}
	for _, want := range operatorApps {
		app := loadOperatorApp(t, want.file)

		raw, ok := app.Metadata.Annotations["argocd.argoproj.io/sync-wave"]
		if !ok {
			t.Errorf("%s carries no argocd.argoproj.io/sync-wave annotation; nothing orders it against the rest of the stack", want.name)
			continue
		}
		got, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			t.Errorf("%s has a non-numeric sync-wave %q", want.name, raw)
			continue
		}
		if got != want.wave {
			t.Errorf("%s has sync-wave %d, want %d", want.name, got, want.wave)
		}
		waves[want.name] = got
	}

	cm, okCM := waves["cert-manager"]
	pg, okPG := waves["cnpg-operator"]
	if okCM && okPG && cm >= pg {
		t.Errorf("cert-manager syncs at wave %d and cnpg-operator at %d, so cert-manager is not first. "+
			"The Barman Cloud plugin declares Certificate and Issuer objects; without cert-manager's CRDs "+
			"already served, the CNPG Application does not degrade -- it fails to apply.", cm, pg)
	}
}

// TestOperatorApplicationsDoNotAutoPrune is the destructive-failure gate.
//
// Pruning an operator that owns CRDs deletes the CRDs. For CNPG that is
// `clusters.postgresql.cnpg.io`: every Cluster object in the estate goes with
// it, and the operator then tears down the StatefulSets and PVCs behind them.
// A momentary render failure must not be able to do that.
//
// selfHeal is asserted ON in the same breath, because the two are easy to
// confuse and they carry opposite risks -- selfHeal reverts drift in the
// controller Deployment, which is wanted; prune deletes what the manifest stops
// declaring, which here is the data.
func TestOperatorApplicationsDoNotAutoPrune(t *testing.T) {
	for _, want := range operatorApps {
		app := loadOperatorApp(t, want.file)

		if app.Spec.SyncPolicy.Automated == nil {
			t.Errorf("%s declares no automated sync policy; the stack would need a manual sync on every change", want.name)
			continue
		}
		if app.Spec.SyncPolicy.Automated.Prune {
			t.Errorf("%s has prune:true. This operator owns CRDs -- pruning them deletes every object of those kinds. "+
				"For cnpg-operator that is every Cluster in the estate, and the PVCs behind them. "+
				"Removing an operator must be a deliberate act, not something a failed render can do.", want.name)
		}
		if !app.Spec.SyncPolicy.Automated.SelfHeal {
			t.Errorf("%s has selfHeal:false, so hand-edits to the controller Deployment persist unreverted", want.name)
		}
	}
}

// TestOperatorApplicationsPointWhereTheyClaimTo checks the wiring that would
// otherwise be verified only by deploying it.
func TestOperatorApplicationsPointWhereTheyClaimTo(t *testing.T) {
	for _, want := range operatorApps {
		t.Run(want.name, func(t *testing.T) {
			app := loadOperatorApp(t, want.file)

			if app.Kind != "Application" {
				t.Errorf("kind is %q, want Application", app.Kind)
			}
			if app.Metadata.Name != want.name {
				t.Errorf("name is %q, want %q", app.Metadata.Name, want.name)
			}
			if app.Spec.Source.Path != want.path {
				t.Errorf("source path is %q, want %q", app.Spec.Source.Path, want.path)
			}
			if app.Spec.Destination.Namespace != want.namespace {
				t.Errorf("destination namespace is %q, want %q", app.Spec.Destination.Namespace, want.namespace)
			}
			// ONE cluster, as everywhere else in this estate.
			if app.Spec.Destination.Server != "https://kubernetes.default.svc" {
				t.Errorf("destination server is %q, want the in-cluster API server", app.Spec.Destination.Server)
			}
			// `default` rather than `memql` is deliberate and explained in each
			// manifest's header: an operator installs CRDs and ClusterRoles by
			// nature, which the memql AppProject exists to forbid to the mesh.
			// Asserted so that "fixing" it to memql -- which looks tidier --
			// fails here rather than at sync time in a cluster.
			if app.Spec.Project != "default" {
				t.Errorf("project is %q, want default. The memql AppProject's near-empty clusterResourceWhitelist "+
					"forbids the CRDs and ClusterRoles an operator install is made of; moving it there rejects the sync.",
					app.Spec.Project)
			}
			// CNPG's and cert-manager's CRDs exceed the last-applied-configuration
			// annotation size limit, so a client-side apply fails on
			// metadata.annotations being too long.
			var ssa bool
			for _, opt := range app.Spec.SyncPolicy.SyncOptions {
				if opt == "ServerSideApply=true" {
					ssa = true
				}
			}
			if !ssa {
				t.Error("the Application does not set ServerSideApply=true; these CRDs are too large for the " +
					"last-applied-configuration annotation and a client-side apply fails on annotation size")
			}
		})
	}
}

// TestOperatorInstallsArePinnedToExactVersions guards the reproducibility that
// makes an operator upgrade a scheduled event rather than a surprise.
//
// An operator upgrade rolls every database pod in the cluster. A floating ref
// means that happens whenever upstream cuts a release, at whatever hour ArgoCD
// next reconciles.
func TestOperatorInstallsArePinnedToExactVersions(t *testing.T) {
	installs := map[string][]string{
		"deploy/cnpg/install/kustomization.yaml": {
			"/cloudnative-pg/releases/download/v1.30.0/cnpg-1.30.0.yaml",
			"/plugin-barman-cloud/releases/download/v0.14.0/manifest.yaml",
		},
		"deploy/cert-manager/install/kustomization.yaml": {
			"/cert-manager/releases/download/v1.21.1/cert-manager.yaml",
		},
	}

	for file, wantRefs := range installs {
		body := string(cnpgRead(t, file))

		for _, ref := range wantRefs {
			if !strings.Contains(body, ref) {
				t.Errorf("%s no longer pins %s -- if this is an intentional bump, update this guard and "+
					"read deploy/cnpg/README.md first: an operator upgrade rolls every database pod.", file, ref)
			}
		}

		// A floating ref alongside the pinned ones would defeat the point while
		// leaving the assertions above satisfied.
		for _, floating := range []string{"/latest/download/", "refs/heads/main", "/master/", "@main"} {
			if strings.Contains(body, floating) {
				t.Errorf("%s contains a floating reference (%q). An operator upgrade rolls every database pod; "+
					"it must happen when an operator decides, not when upstream cuts a release.", file, floating)
			}
		}

		// A `namespace:` transformer here would rewrite metadata.namespace on
		// every resource in manifests that are self-namespacing and full of
		// cross-namespace references (webhook service refs, RBAC subjects). The
		// damage surfaces as a webhook that never answers, not as an apply error.
		for _, line := range strings.Split(body, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "namespace:") && !strings.HasPrefix(trimmed, "#") {
				t.Errorf("%s sets a kustomize `namespace:` transformer (%q). These upstream manifests declare "+
					"their own namespaces and reference themselves across namespaces; rewriting that surfaces "+
					"as a webhook that never answers rather than as an apply error.", file, trimmed)
			}
		}
	}
}
