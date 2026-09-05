// Package dslmount holds the gates for the DSL-delivery components:
// `dsl-mount`, the shared volume, and the two deliverers that write into it,
// `dsl-bundle` and `dsl-packages` (memql#4933).
//
// # Why these components had no test at all until now
//
// A Component cannot be rendered on its own -- it is only meaningful inside a
// Kustomization -- so testing one means committing the thing a consumer would
// write and building that. cnpg-db and tenant do exactly this and are the
// template; these three shipped without it, and every defect below survived
// review because reading three YAML files does not tell you what applying them
// together produces.
//
// Four of them, in the order a consumer meets them:
//
//  1. Both deliverers built the shared substrate for THEMSELVES -- the same
//     emptyDir, the same mount on container 0, the same MEMQL_DSL_PATH -- so
//     applying both rendered a Deployment with two volumes of one name, which
//     the API server refuses.
//  2. `dsl-bundle` added its init container at the BARE path, replacing the
//     whole list, while `dsl-packages` appended. One of the two listing orders
//     therefore deleted the package fetcher silently.
//  3. `dsl-packages` alone could not render at all: it appended to an
//     `initContainers` key no Deployment ships.
//  4. The USAGE both headers prescribed -- `components:` and `labels:` in one
//     kustomization -- selects nothing, because the label transformer runs
//     last. It renders, it applies, every pod is healthy, and none of them
//     has any of this.
//
// Each is invisible in a diff and loud in a render, which is what this file is.
package dslmount

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	// .../deploy/k8s/components/dsl-mount/component_test.go -> repo root
	return filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(self)))))
}

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

// renderExample builds a committed composition example. A missing one is a
// FAILURE rather than a skip: a component with no example is a component
// nothing renders, which is the state all three were in.
func renderExample(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(repoRoot(t), "deploy", "k8s", "components", "examples", name)
	if _, err := os.Stat(filepath.Join(dir, "kustomization.yaml")); err != nil {
		t.Fatalf("no composition example at %s (%v)", dir, err)
	}
	return renderDir(t, dir)
}

type container struct {
	Name  string `yaml:"name"`
	Image string `yaml:"image"`
	Env   []struct {
		Name  string `yaml:"name"`
		Value string `yaml:"value"`
	} `yaml:"env"`
	EnvFrom []struct {
		SecretRef *struct {
			Name string `yaml:"name"`
		} `yaml:"secretRef"`
		ConfigMapRef *struct {
			Name     string `yaml:"name"`
			Optional *bool  `yaml:"optional"`
		} `yaml:"configMapRef"`
	} `yaml:"envFrom"`
	VolumeMounts []struct {
		Name      string `yaml:"name"`
		MountPath string `yaml:"mountPath"`
	} `yaml:"volumeMounts"`
}

type deployment struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				InitContainers []container `yaml:"initContainers"`
				Containers     []container `yaml:"containers"`
				Volumes        []struct {
					Name string `yaml:"name"`
				} `yaml:"volumes"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

// deployments decodes rather than grepping. `strings.Contains(rendered,
// "dsl-tree")` cannot tell one volume from two, which is the whole question
// here.
func deployments(t *testing.T, rendered string) map[string]deployment {
	t.Helper()
	out := map[string]deployment{}
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	for {
		var d deployment
		err := dec.Decode(&d)
		if err != nil {
			break
		}
		if d.Kind == "Deployment" {
			out[d.Metadata.Name] = d
		}
	}
	if len(out) == 0 {
		t.Fatal("no Deployments in the rendered stream")
	}
	return out
}

// The five mesh node types that load product DSL.
var dslNodes = []string{"bff", "agent", "cognition", "planner", "workbench"}

func mountedNodes(t *testing.T, rendered string) map[string]deployment {
	t.Helper()
	all := deployments(t, rendered)
	out := map[string]deployment{}
	for _, n := range dslNodes {
		d, ok := all[n]
		if !ok {
			t.Fatalf("no %q Deployment in the render -- the example does not build the mesh it claims to", n)
		}
		out[n] = d
	}
	return out
}

func names(cs []container) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

// DEFECT 3. The packages component alone renders, and lands everything.
func TestPackagesAloneRendersAndMountsTheTree(t *testing.T) {
	for name, d := range mountedNodes(t, renderExample(t, "dsl-packages-only")) {
		spec := d.Spec.Template.Spec

		if got := names(spec.InitContainers); len(got) != 1 || got[0] != "dsl-packages-fetch" {
			t.Errorf("%s: initContainers = %v, want exactly [dsl-packages-fetch]", name, got)
		}
		if n := countVolume(spec.Volumes, "dsl-tree"); n != 1 {
			t.Errorf("%s: %d dsl-tree volumes, want exactly 1", name, n)
		}
		if !mountsTree(spec.Containers[0]) {
			t.Errorf("%s: the engine container does not mount the DSL tree", name)
		}
		if v, ok := env(spec.Containers[0], "MEMQL_DSL_PATH"); !ok || v != "/var/lib/memql/dsl" {
			t.Errorf("%s: MEMQL_DSL_PATH = %q (present=%v), want /var/lib/memql/dsl -- "+
				"without it the node reads no runtime DSL at all", name, v, ok)
		}
	}
}

// DEFECT 2 in Gap 2. The fetcher must be told WHERE to look. It shipped with
// `envFrom` on memql-secrets alone, which carries no container name anywhere
// in this repo, so it took the "no package pointer" branch on every instance
// and the node booted healthy with none of its packages' DSL.
func TestTheFetcherIsToldWhichBlobContainerToRead(t *testing.T) {
	for name, d := range mountedNodes(t, renderExample(t, "dsl-packages-only")) {
		fetch := findContainer(d.Spec.Template.Spec.InitContainers, "dsl-packages-fetch")
		if fetch == nil {
			t.Fatalf("%s: no dsl-packages-fetch init container", name)
		}
		var sawStorage bool
		for _, ref := range fetch.EnvFrom {
			if ref.ConfigMapRef != nil && ref.ConfigMapRef.Name == "memql-storage" {
				sawStorage = true
				if ref.ConfigMapRef.Optional == nil || !*ref.ConfigMapRef.Optional {
					t.Errorf("%s: the memql-storage ref is not optional -- a missing ConfigMap "+
						"should produce the fetcher's own sentence naming the variable, "+
						"not a pod wedged on a mount error", name)
				}
			}
		}
		if !sawStorage {
			t.Errorf("%s: the fetcher reads MEMQL_AZURE_BLOB_CONTAINER and nothing supplies it; "+
				"it will exit 0 having looked at nothing", name)
		}
	}
}

// DEFECTS 1 AND 2. Both deliverers on one node: one volume, one mount, one
// MEMQL_DSL_PATH, and BOTH init containers, bundle first.
func TestBothDeliverersComposeWithoutColliding(t *testing.T) {
	for name, d := range mountedNodes(t, renderExample(t, "dsl-bundle-and-packages")) {
		spec := d.Spec.Template.Spec

		got := names(spec.InitContainers)
		want := []string{"dsl-bundle-copy", "dsl-packages-fetch"}
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("%s: initContainers = %v, want %v in that order -- init containers run "+
				"in list order, so this IS the copy order: the product's bundle lands "+
				"first and packages add domains beside it", name, got, want)
		}
		if n := countVolume(spec.Volumes, "dsl-tree"); n != 1 {
			t.Errorf("%s: %d dsl-tree volumes, want exactly 1 -- a duplicate volume name is "+
				"rejected by the API server, and is what stopped these two composing", name, n)
		}
		if n := countMounts(spec.Containers[0], "dsl-tree"); n != 1 {
			t.Errorf("%s: %d dsl-tree mounts on the engine container, want exactly 1", name, n)
		}
		if n := countEnv(spec.Containers[0], "MEMQL_DSL_PATH"); n != 1 {
			t.Errorf("%s: MEMQL_DSL_PATH declared %d times, want exactly 1", name, n)
		}
	}
}

// DEFECT 4. The label has to be on before the selector reads it, and the
// example is what proves the documented shape works -- a render with no init
// container anywhere is the silent-success failure this whole file is about.
func TestTheExampleActuallySelectsSomething(t *testing.T) {
	rendered := renderExample(t, "dsl-packages-only")
	if !strings.Contains(rendered, "dsl-packages-fetch") {
		t.Fatal("the render contains no init container at all: the product-dsl label was " +
			"applied after the components that select on it, so every patch matched nothing. " +
			"It renders, it applies, and not one pod has any of this.")
	}
	// And it must NOT have been applied to a Deployment that is not a mesh
	// node: `redis` has no volumes key, so a blanket label fails the render --
	// which is the loud half of the same mistake.
	if d, ok := deployments(t, rendered)["redis"]; ok {
		if len(d.Spec.Template.Spec.InitContainers) != 0 {
			t.Error("redis carries a DSL init container; it does not load product DSL")
		}
	}
}

// The roll's RBAC must name workloads, not the namespace. A Role granting
// patch over every Deployment is the blast radius MEMQL_PACKAGES_ROLL_TARGETS
// exists to make somebody choose.
func TestTheRollRoleIsPinnedToNamedWorkloads(t *testing.T) {
	rendered := renderExample(t, "dsl-packages-only")

	type rule struct {
		APIGroups     []string `yaml:"apiGroups"`
		Resources     []string `yaml:"resources"`
		Verbs         []string `yaml:"verbs"`
		ResourceNames []string `yaml:"resourceNames"`
	}
	var found bool
	dec := yaml.NewDecoder(strings.NewReader(rendered))
	for {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Rules []rule `yaml:"rules"`
		}
		if dec.Decode(&doc) != nil {
			break
		}
		if doc.Kind != "Role" || doc.Metadata.Name != "memql-packages-roll" {
			continue
		}
		found = true
		for _, r := range doc.Rules {
			if len(r.ResourceNames) == 0 {
				t.Error("the roll Role names no resourceNames, so it grants patch over every " +
					"Deployment in the namespace")
			}
			for _, v := range r.Verbs {
				if v == "list" || v == "watch" || v == "*" {
					t.Errorf("verb %q defeats resourceNames -- RBAC ignores them for "+
						"collection verbs, so this silently grants the whole namespace", v)
				}
			}
		}
	}
	if !found {
		t.Fatal("no memql-packages-roll Role in the render")
	}
}

func countVolume(vols []struct {
	Name string `yaml:"name"`
}, name string) int {
	n := 0
	for _, v := range vols {
		if v.Name == name {
			n++
		}
	}
	return n
}

func countMounts(c container, name string) int {
	n := 0
	for _, m := range c.VolumeMounts {
		if m.Name == name {
			n++
		}
	}
	return n
}

func mountsTree(c container) bool { return countMounts(c, "dsl-tree") > 0 }

func countEnv(c container, key string) int {
	n := 0
	for _, e := range c.Env {
		if e.Name == key {
			n++
		}
	}
	return n
}

func env(c container, key string) (string, bool) {
	for _, e := range c.Env {
		if e.Name == key {
			return e.Value, true
		}
	}
	return "", false
}

func findContainer(cs []container, name string) *container {
	for i := range cs {
		if cs[i].Name == name {
			return &cs[i]
		}
	}
	return nil
}
