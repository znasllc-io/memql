// Package cnpgdb holds the gates for the reusable database component (epic
// memql#3842, task memql#3851).
//
// The component is the seam a client cluster today -- and a MemQL Cloud tenant
// later -- composes its database from. Its failure modes are the ones a
// component has rather than a manifest: a consumer that forgets to supply a
// required value, and a preset whose numbers stopped matching what it is
// documented to sell.
package cnpgdb

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// placeholder is the destinationPath the component ships because the CRD
// requires the field and there is no sane default for it.
const placeholder = "PATCH-ME-IN-THE-OVERLAY"

func repoRoot(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	// .../deploy/k8s/components/cnpg-db/component_test.go -> repo root
	return filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(self)))))
}

// renderDir builds a kustomization directory with whichever renderer is present.
func renderDir(t *testing.T, dir string) string {
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
	t.Skip("neither kustomize nor kubectl is installed")
	return ""
}

// renderPreset renders the committed composition example for a preset ("" for
// the no-preset one).
//
// A Component cannot be built on its own -- it is only meaningful inside a
// Kustomization -- so exercising a preset means building the thing a consumer
// would write. Those live in deploy/k8s/components/examples/ rather than in a
// temp directory or a Go `testdata/` dir next to this file, and neither
// alternative is available: kustomize refuses an ABSOLUTE component path
// ("new root ... cannot be absolute"), and it detects a CYCLE when the example
// sits inside the component it references ("candidate root ... contains visited
// root ..."). Committing them also makes them the usage documentation, which
// being rendered here keeps honest.
func renderPreset(t *testing.T, preset string) string {
	t.Helper()
	name := "cnpg-db-base"
	if preset != "" {
		name = "cnpg-db-" + preset
	}
	dir := filepath.Join(repoRoot(t), "deploy", "k8s", "components", "examples", name)
	if _, err := os.Stat(filepath.Join(dir, "kustomization.yaml")); err != nil {
		t.Fatalf("no composition example at %s (%v) -- a preset with no example is a preset nothing renders", dir, err)
	}
	return renderDir(t, dir)
}

type clusterDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Instances  int    `yaml:"instances"`
		ImageName  string `yaml:"imageName"`
		EnablePDB  *bool  `yaml:"enablePDB"`
		PostgreSQL struct {
			Parameters map[string]string `yaml:"parameters"`
		} `yaml:"postgresql"`
		Storage struct {
			Size string `yaml:"size"`
		} `yaml:"storage"`
		WalStorage struct {
			Size string `yaml:"size"`
		} `yaml:"walStorage"`
		Resources struct {
			Requests map[string]string `yaml:"requests"`
		} `yaml:"resources"`
	} `yaml:"spec"`
}

// findCluster returns the CNPG Cluster in a rendered stream, if there is one.
//
// It decodes rather than grepping, because `strings.Contains(rendered, "kind:
// Cluster")` is true of ClusterRole, ClusterRoleBinding and ClusterIssuer -- and
// the cloud overlays' front doors carry a ClusterIssuer, so the substring test
// says "this overlay has a database" about every one of them.
func findCluster(t *testing.T, rendered string) (clusterDoc, bool) {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	for {
		var d clusterDoc
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			return clusterDoc{}, false
		}
		if err != nil {
			t.Fatalf("decoding rendered manifests: %v", err)
		}
		if d.Kind == "Cluster" {
			return d, true
		}
	}
}

func clusterFrom(t *testing.T, rendered string) clusterDoc {
	t.Helper()
	c, ok := findCluster(t, rendered)
	if !ok {
		t.Fatal("no CNPG Cluster in the rendered manifests")
	}
	return c
}

// TestComponentRendersTheFourResources is the shape assertion: whatever an
// overlay does with values, composing this component must yield the same four
// kinds, because "one shape everywhere" is the entire claim.
func TestComponentRendersTheFourResources(t *testing.T) {
	rendered := renderPreset(t, "")

	for _, want := range []string{
		"kind: Cluster",
		"kind: Database",
		"kind: ObjectStore",
		"kind: ScheduledBackup",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the component does not render %q", strings.TrimPrefix(want, "kind: "))
		}
	}
}

// TestPresetsMatchTheirDocumentedTiers checks the numbers against what the tier
// catalog sells.
//
// A preset is a PROMISE with a price attached. "mid gives you two instances and
// 128 GiB" silently becoming something else is not a rendering bug -- it is a
// deployment that does not match what a customer bought, and nothing about the
// running cluster would say so.
func TestPresetsMatchTheirDocumentedTiers(t *testing.T) {
	for _, tc := range []struct {
		preset    string
		instances int
		pdb       bool
		data, wal string
		cpu, mem  string
		maxConns  string
	}{
		{"entry", 1, false, "32Gi", "16Gi", "1", "4Gi", "200"},
		{"mid", 2, true, "128Gi", "32Gi", "2", "8Gi", "400"},
		{"top", 3, true, "256Gi", "64Gi", "4", "16Gi", "400"},
	} {
		t.Run(tc.preset, func(t *testing.T) {
			c := clusterFrom(t, renderPreset(t, tc.preset))

			if c.Spec.Instances != tc.instances {
				t.Errorf("instances = %d, want %d", c.Spec.Instances, tc.instances)
			}
			// THE HA TOGGLE IS TWO VALUES, and the second is the one that gets
			// forgotten: raising instances while leaving the component's
			// single-instance `enablePDB: false` inherited means a node drain
			// can take the primary and its replica together -- exactly what the
			// second instance was bought to prevent. At one instance the
			// opposite is true: a PDB there blocks every drain forever.
			if c.Spec.EnablePDB == nil {
				t.Fatalf("%s does not state enablePDB", tc.preset)
			}
			if *c.Spec.EnablePDB != tc.pdb {
				t.Errorf("enablePDB = %v, want %v (instances=%d)", *c.Spec.EnablePDB, tc.pdb, tc.instances)
			}
			if c.Spec.Storage.Size != tc.data {
				t.Errorf("storage = %s, want %s", c.Spec.Storage.Size, tc.data)
			}
			if c.Spec.WalStorage.Size != tc.wal {
				t.Errorf("walStorage = %s, want %s", c.Spec.WalStorage.Size, tc.wal)
			}
			if got := c.Spec.Resources.Requests["cpu"]; got != tc.cpu {
				t.Errorf("cpu request = %s, want %s", got, tc.cpu)
			}
			if got := c.Spec.Resources.Requests["memory"]; got != tc.mem {
				t.Errorf("memory request = %s, want %s", got, tc.mem)
			}
			if got := c.Spec.PostgreSQL.Parameters["max_connections"]; got != tc.maxConns {
				t.Errorf("max_connections = %s, want %s", got, tc.maxConns)
			}
		})
	}
}

// TestOptionalPoolerStillComposes renders the opt-in pooler path.
//
// No overlay composes it today -- self-hosting removed the per-tier connection
// ceiling that made a pooler mandatory, so it ships READY rather than ENABLED.
// That is exactly the condition under which a component rots: nothing builds
// it, so nothing notices when a CRD field it uses is renamed or removed, and
// the first person to need it discovers that under load.
//
// Also asserts the two properties that make it safe to put in the path at all.
func TestOptionalPoolerStillComposes(t *testing.T) {
	rendered := renderPreset(t, "mid-pooler")

	if !strings.Contains(rendered, "kind: Pooler") {
		t.Fatal("the pooler example renders no Pooler")
	}
	// The database is still there: a pooler REPLACES nothing.
	if _, ok := findCluster(t, rendered); !ok {
		t.Error("composing the pooler dropped the Cluster")
	}
	// TRANSACTION mode is the whole reason MEMORY_NODES_DATABASE_DIRECT_DSN
	// exists. Session mode would make the pooler pointless (one server
	// connection per client); transaction mode is what makes the DSN split
	// load-bearing rather than decorative.
	if !strings.Contains(rendered, "poolMode: transaction") {
		t.Error("the pooler is not in transaction mode; in session mode it pools nothing worth pooling")
	}
	// Pointed at the primary. A pooler on `ro` would be a Service nothing
	// dials, since read-replica routing is out of scope for this epic.
	if !strings.Contains(rendered, "type: rw") {
		t.Error("the pooler is not type: rw")
	}
}

// TestNoOverlayShipsThePlaceholderBackupDestination is the required-value gate.
//
// `destinationPath` is a REQUIRED field on the ObjectStore CRD, so the component
// cannot omit it and leave the consumer to notice -- it ships a placeholder
// instead. An overlay that fails to patch it does not error: it archives
// nowhere, while the Cluster reports Ready and the pods look healthy. That is
// the exact failure mode the whole backup layer exists to prevent, so it is
// worth a gate that reads every real overlay.
func TestNoOverlayShipsThePlaceholderBackupDestination(t *testing.T) {
	root := repoRoot(t)
	overlays, err := filepath.Glob(filepath.Join(root, "deploy", "k8s", "overlays", "*"))
	if err != nil {
		t.Fatalf("globbing overlays: %v", err)
	}

	var checked int
	for _, dir := range overlays {
		if _, err := os.Stat(filepath.Join(dir, "kustomization.yaml")); err != nil {
			continue // not an overlay (the shared _test.go files live here too)
		}
		name := filepath.Base(dir)
		rendered := renderDir(t, dir)
		if !strings.Contains(rendered, "kind: ObjectStore") {
			continue // this overlay does not compose the database yet
		}
		checked++
		if strings.Contains(rendered, placeholder) {
			t.Errorf("the %s overlay renders the component's placeholder destinationPath (%q). "+
				"It would archive nowhere -- with no error, a Ready Cluster and healthy pods. "+
				"Patch spec/configuration/destinationPath in that overlay.", name, placeholder)
		}
	}

	// A gate that checked nothing must say so rather than pass quietly: if no
	// overlay composes the component, this test proves the component is unused,
	// not that every consumer is correct.
	if checked == 0 {
		t.Skip("no overlay composes the cnpg-db component yet; nothing to check")
	}
	t.Logf("checked %d overlay(s) composing the database component", checked)
}

// TestComponentImageIsPinnedByEveryConsumer guards a SILENT no-op.
//
// Kustomize's `images:` transformer knows the container paths in core workload
// kinds and nothing else, so a CustomResource naming its image elsewhere is
// invisible to it. CNPG puts it at `spec/imageName`. The component ships
// kustomizeconfig/images.yaml to teach the transformer that path -- and without
// it an overlay's perfectly reasonable-looking `images:` entry does nothing at
// all: the render succeeds, `imageName` comes out as a bare `memql-db` with no
// tag, and the failure lands at apply (CNPG refuses an unparseable tag) or at
// pull (`memql-db:latest`, which does not exist).
func TestComponentImageIsPinnedByEveryConsumer(t *testing.T) {
	root := repoRoot(t)

	// The component itself must ship the transformer config, or no consumer can
	// pin anything.
	cfg := filepath.Join(root, "deploy", "k8s", "components", "cnpg-db", "kustomizeconfig", "images.yaml")
	if _, err := os.Stat(cfg); err != nil {
		t.Fatalf("the component does not ship kustomizeconfig/images.yaml (%v); every overlay's `images:` "+
			"entry for memql-db is then a silent no-op", err)
	}

	// Unpinned in the component -- that is deliberate, so a consumer that
	// forgets fails to pull rather than running some other Postgres.
	if c := clusterFrom(t, renderPreset(t, "")); strings.Contains(c.Spec.ImageName, ":") {
		t.Errorf("the component pins an image tag (%q). It must stay unpinned so an overlay that forgets "+
			"its `images:` entry fails loudly rather than running whatever that tag happens to be.", c.Spec.ImageName)
	}

	overlays, _ := filepath.Glob(filepath.Join(root, "deploy", "k8s", "overlays", "*"))
	for _, dir := range overlays {
		if _, err := os.Stat(filepath.Join(dir, "kustomization.yaml")); err != nil {
			continue
		}
		c, ok := findCluster(t, renderDir(t, dir))
		if !ok {
			continue // this overlay does not compose the database yet
		}
		if !strings.Contains(c.Spec.ImageName, ":") && !strings.Contains(c.Spec.ImageName, "@") {
			t.Errorf("the %s overlay renders imageName=%q with no tag or digest. Its `images:` entry is not "+
				"reaching spec/imageName -- check that the cnpg-db component's configurations: block is intact.",
				filepath.Base(dir), c.Spec.ImageName)
		}
	}
}
