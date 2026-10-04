// Package pipelines holds the gates for the pipelines substrate component
// (epic memql#5478, task memql#5492): the memql-pipelines namespace every
// pipeline step runs in, and the ConfigMap that points the workbench at it.
//
// These read the component's own files and need no renderer, so they run
// wherever Go does. What the files become inside an overlay -- the namespaces
// that survive, the values each overlay states -- is asserted by
// deploy/k8s/overlays/render_pipelines_test.go, which renders all three.
package pipelines

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/core/repowalk"
	"gopkg.in/yaml.v3"
)

const (
	stepsNamespace = "memql-pipelines"
	configMapName  = "memql-pipelines"
)

func componentDir(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	return filepath.Dir(self)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	// .../deploy/k8s/components/pipelines -> repo root
	return filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(componentDir(t)))))
}

type object struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Data map[string]string `yaml:"data"`
}

// componentObjects decodes every document of every file the component's
// kustomization lists under `resources:` -- the files that actually render,
// not whatever happens to sit in the directory.
func componentObjects(t *testing.T) []object {
	t.Helper()
	dir := componentDir(t)
	raw, err := os.ReadFile(filepath.Join(dir, "kustomization.yaml"))
	if err != nil {
		t.Fatalf("reading the component's kustomization: %v", err)
	}
	var k struct {
		Kind      string   `yaml:"kind"`
		Resources []string `yaml:"resources"`
	}
	if err := yaml.Unmarshal(raw, &k); err != nil {
		t.Fatalf("parsing the component's kustomization: %v", err)
	}
	if k.Kind != "Component" {
		t.Fatalf("kustomization.yaml is kind %q, want Component -- base composes nothing that carries its own namespace", k.Kind)
	}
	var out []object
	for _, file := range k.Resources {
		body, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		dec := yaml.NewDecoder(strings.NewReader(string(body)))
		for {
			var o object
			err := dec.Decode(&o)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("decoding %s: %v", file, err)
			}
			if o.Kind != "" {
				out = append(out, o)
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("the component lists no objects")
	}
	return out
}

// TestEveryObjectNamesTheNamespaceItBelongsTo is the component's half of the
// placement contract.
//
// The instance overlays set the mesh namespace with an unsetOnly
// NamespaceTransformer: it fills in every object that states no namespace, and
// fills it in with the MESH's. So an object of this component that forgets its
// `namespace: memql-pipelines` does not fail anywhere -- it lands beside the
// engine, where every one of them does harm: the LimitRange would put default
// CPU and memory LIMITS on every mesh container that sets only requests (all
// of base), the quota would cap the mesh's own Jobs, the Role would grant Jobs
// and Secrets where no step runs, and a NetworkPolicy selecting every pod with
// no ingress rule would cut the mesh off from itself.
//
// The ConfigMap is the reverse case: it belongs beside the workbench that
// reads it, so it must state NO namespace and let the overlay supply the
// mesh's, whatever that is called.
func TestEveryObjectNamesTheNamespaceItBelongsTo(t *testing.T) {
	var sawNamespace, sawConfigMap bool
	for _, o := range componentObjects(t) {
		id := o.Kind + "/" + o.Metadata.Name
		switch {
		case o.Kind == "Namespace":
			sawNamespace = true
			if o.Metadata.Name != stepsNamespace {
				t.Errorf("%s: the component's Namespace must be %s", id, stepsNamespace)
			}
		case o.Kind == "ConfigMap" && o.Metadata.Name == configMapName:
			sawConfigMap = true
			if o.Metadata.Namespace != "" {
				t.Errorf("%s states namespace %q. It belongs in the MESH namespace beside the workbench "+
					"that reads it, which the overlay's transformer supplies only when none is stated.",
					id, o.Metadata.Namespace)
			}
		case o.Metadata.Namespace != stepsNamespace:
			t.Errorf("%s states namespace %q, want %q. Without it the overlay's unsetOnly transformer "+
				"puts it in the MESH namespace, silently, where it acts on the engine instead of the steps.",
				id, o.Metadata.Namespace, stepsNamespace)
		}
	}
	if !sawNamespace || !sawConfigMap {
		t.Errorf("the component renders Namespace=%v ConfigMap/%s=%v; both are part of its contract",
			sawNamespace, configMapName, sawConfigMap)
	}
}

// TestTheCloneImageIsPinnedByDigest: the clone init container is handed the
// repository token, and a tag is whatever was pushed to it last, so whoever
// controls a mutable tag would control the token. The digest is measured with
// `docker buildx imagetools inspect <image>:<tag>` (the index digest), never
// typed.
func TestTheCloneImageIsPinnedByDigest(t *testing.T) {
	var image string
	for _, o := range componentObjects(t) {
		if o.Kind == "ConfigMap" && o.Metadata.Name == configMapName {
			image = o.Data["MEMQL_PIPELINES_CLONE_IMAGE"]
		}
	}
	ref, digest, ok := strings.Cut(image, "@sha256:")
	if !ok || ref == "" || len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
		t.Errorf("MEMQL_PIPELINES_CLONE_IMAGE is %q, want <image>:<tag>@sha256:<64 lowercase hex>", image)
	}
}

// TestTenantsDoNotComposeThePipelinesComponent keeps the README's claim true.
//
// memql-pipelines is one name per cluster. A tenant renders base under its own
// namespace with the plain `namespace:` field, so composing this component
// there would either fail the render ("namespace transformation produces ID
// conflict") or, worse, if someone "fixed" that, put every tenant on one
// cluster into one shared steps namespace, Role and quota.
func TestTenantsDoNotComposeThePipelinesComponent(t *testing.T) {
	root := repoRoot(t)
	var scanned int
	scan := func(dir string) {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if repowalk.SkipDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			scanned++
			if strings.Contains(string(body), "components/pipelines") {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s composes or names components/pipelines; tenants do not compose the pipelines "+
					"component (its README says why)", rel)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", dir, err)
		}
	}
	scan(filepath.Join(root, "deploy", "k8s", "components", "tenant"))
	examples, err := filepath.Glob(filepath.Join(root, "deploy", "k8s", "components", "examples", "tenant-*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, ex := range examples {
		scan(ex)
	}
	scan(filepath.Join(root, "scripts", "fleet"))
	// Not a silent pass: a walk that read nothing has checked nothing.
	if scanned == 0 || len(examples) == 0 {
		t.Fatalf("scanned %d file(s) across %d tenant example(s); the tenant tree moved and this gate is "+
			"watching nothing", scanned, len(examples))
	}
	t.Logf("scanned %d tenant file(s), %d tenant example(s)", scanned, len(examples))
}
