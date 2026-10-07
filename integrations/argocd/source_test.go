package argocd

import (
	"bytes"
	"context"
	"crypto/sha1" // Fixture encoding for Git's existing object protocol.
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type sourceFixtureFile struct{ mode, body string }
type objectFixture map[string][]byte

func (objects objectFixture) OpenGitObject(_ context.Context, kind, oid string) (io.ReadCloser, error) {
	body, found := objects[kind+"/"+oid]
	if !found {
		return nil, fmt.Errorf("fixture object unavailable")
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

func (objects objectFixture) add(kind string, body []byte) string {
	hash := sha1.New()
	_, _ = fmt.Fprintf(hash, "%s %d\x00", kind, len(body))
	_, _ = hash.Write(body)
	oid := hex.EncodeToString(hash.Sum(nil))
	objects[kind+"/"+oid] = append([]byte(nil), body...)
	return oid
}

func sourceFilesFixture() map[string]sourceFixtureFile {
	return map[string]sourceFixtureFile{
		".gitattributes": {"100644", "* text=auto\n*.sh text eol=lf\n"},
		"overlay/kustomization.yaml": {"100644", `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources: [../base]
components: [../component]
patches:
  - path: patch.yaml
  - patch: |
      apiVersion: apps/v1
      kind: Deployment
      metadata: {name: engine}
      spec: {replicas: 3}
configMapGenerator:
  - name: settings
    files: [settings=config.txt]
    envs: [vars.env]
transformers: [namespace.yaml]
configurations: [fields.yaml]
`},
		"base/kustomization.yaml":      {"100644", "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: [deployment.yaml]\n"},
		"base/deployment.yaml":         {"100644", "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: engine}\n"},
		"component/kustomization.yaml": {"100644", "apiVersion: kustomize.config.k8s.io/v1alpha1\nkind: Component\nresources: [service.yaml]\n"},
		"component/service.yaml":       {"100644", "apiVersion: v1\nkind: Service\nmetadata: {name: engine}\n"},
		"overlay/patch.yaml":           {"100644", "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: engine}\nspec: {replicas: 2}\n"},
		"overlay/config.txt":           {"100755", "fixture contents that must not appear in a source summary"},
		"overlay/vars.env":             {"100644", "# frozen values\nENABLED=true\nEMPTY=\n"},
		"overlay/namespace.yaml":       {"100644", "apiVersion: builtin\nkind: NamespaceTransformer\nmetadata: {name: scope, namespace: memql}\nunsetOnly: true\n"},
		"overlay/fields.yaml":          {"100644", "nameReference: []\n"},
		"unrelated.txt":                {"100644", "not a render input"},
	}
}

// Build genuine Git tree/commit byte encodings. The verifier consumes no path
// map from this fixture; it must independently traverse the verified objects.
func sourceObjectsFixture(t *testing.T, files map[string]sourceFixtureFile) (objectFixture, RenderSpec) {
	t.Helper()
	objects := objectFixture{}
	var tree func(string) string
	tree = func(prefix string) string {
		entries := map[string]gitEntry{}
		for p, file := range files {
			if !strings.HasPrefix(p, prefix) {
				continue
			}
			rest := strings.TrimPrefix(p, prefix)
			name, _, subdir := strings.Cut(rest, "/")
			if subdir {
				entries[name] = gitEntry{mode: "40000", name: name}
				continue
			}
			oid := strings.Repeat("a", 40)
			if file.mode != "160000" {
				oid = objects.add("blob", []byte(file.body))
			}
			entries[name] = gitEntry{mode: file.mode, name: name, oid: oid}
		}
		ordered := make([]gitEntry, 0, len(entries))
		for name, entry := range entries {
			if entry.mode == "40000" {
				entry.oid = tree(prefix + name + "/")
			}
			ordered = append(ordered, entry)
		}
		sort.Slice(ordered, func(i, j int) bool {
			a, b := ordered[i].name, ordered[j].name
			if ordered[i].mode == "40000" {
				a += "/"
			}
			if ordered[j].mode == "40000" {
				b += "/"
			}
			return a < b
		})
		var body bytes.Buffer
		for _, entry := range ordered {
			fmt.Fprintf(&body, "%s %s\x00", entry.mode, entry.name)
			raw, err := hex.DecodeString(entry.oid)
			require.NoError(t, err)
			body.Write(raw)
		}
		return objects.add("tree", body.Bytes())
	}
	root := tree("")
	commit := objects.add("commit", []byte("tree "+root+"\nauthor Fixture <fixture@example.invalid> 1 +0000\ncommitter Fixture <fixture@example.invalid> 1 +0000\n\nsource fixture\n"))
	spec := renderSpecFixture()
	var source map[string]any
	require.NoError(t, json.Unmarshal(spec.Source, &source))
	source["path"], source["targetRevision"] = "overlay", commit
	var err error
	spec.Source, err = json.Marshal(source)
	require.NoError(t, err)
	return objects, spec
}

func TestSourceClosureVerifiesGitObjectsAndCompleteInputs(t *testing.T) {
	files := sourceFilesFixture()
	objects, spec := sourceObjectsFixture(t, files)
	closed, err := VerifySourceClosure(context.Background(), objects, spec)
	require.NoError(t, err)
	require.Regexp(t, `^memql-id:[a-f0-9]{64}$`, closed.Digest())
	require.Len(t, closed.Files(), len(files)-1)
	for _, file := range closed.Files() {
		require.NotEqual(t, "unrelated.txt", file.Path)
		require.Regexp(t, `^[a-f0-9]{40}$`, file.Blob)
		require.Regexp(t, `^memql-id:[a-f0-9]{64}$`, file.ContentDigest)
	}
	second, err := VerifySourceClosure(context.Background(), objects, spec)
	require.NoError(t, err)
	require.Equal(t, closed.Digest(), second.Digest())
	spec.Source[0] = 'x'
	projection := closed.Files()
	projection[0].Path = "mutated"
	copySpec := closed.Spec()
	copySpec.Source[0] = 'x'
	require.NotEqual(t, "mutated", closed.Files()[0].Path)
	require.Equal(t, byte('{'), closed.Spec().Source[0])
	require.Equal(t, "ArgoCD source files=11 digest="+closed.Digest(), fmt.Sprintf("%+v", closed))
	require.Equal(t, fmt.Sprintf("%+v", closed), fmt.Sprintf("%#v", closed))

	files["overlay/config.txt"] = sourceFixtureFile{"100755", "changed committed input"}
	objects, spec = sourceObjectsFixture(t, files)
	changed, err := VerifySourceClosure(context.Background(), objects, spec)
	require.NoError(t, err)
	require.NotEqual(t, closed.Digest(), changed.Digest())
}

func TestSourceClosureRejectsSubstitutedObjectBodies(t *testing.T) {
	for _, kind := range []string{"commit", "tree", "blob"} {
		t.Run(kind, func(t *testing.T) {
			objects, spec := sourceObjectsFixture(t, sourceFilesFixture())
			for key, body := range objects {
				if strings.HasPrefix(key, kind+"/") {
					objects[key] = append(body, 'x')
				}
			}
			closed, err := VerifySourceClosure(context.Background(), objects, spec)
			require.ErrorContains(t, err, "does not match its requested identity")
			require.Empty(t, closed.Digest())
		})
	}
}

func TestSourceClosureRefusesOpenOrAmbiguousInputs(t *testing.T) {
	for name, change := range map[string]func(map[string]sourceFixtureFile){
		"remote base": func(f map[string]sourceFixtureFile) {
			p := f["overlay/kustomization.yaml"]
			p.body = strings.Replace(p.body, "../base", "https://example.invalid/base?ref=main", 1)
			f["overlay/kustomization.yaml"] = p
		},
		"repository escape": func(f map[string]sourceFixtureFile) {
			p := f["overlay/kustomization.yaml"]
			p.body = strings.Replace(p.body, "../base", "../../outside", 1)
			f["overlay/kustomization.yaml"] = p
		},
		"control in input path": func(f map[string]sourceFixtureFile) {
			p := f["overlay/kustomization.yaml"]
			p.body = strings.Replace(p.body, "patch.yaml", `"patch\u0001.yaml"`, 1)
			f["overlay/kustomization.yaml"] = p
			f["overlay/patch\x01.yaml"] = f["overlay/patch.yaml"]
		},
		"deep patch path": func(f map[string]sourceFixtureFile) {
			p := f["overlay/kustomization.yaml"]
			p.body = strings.Replace(p.body, "patch.yaml", strings.Repeat("nested/", 129)+"patch.yaml", 1)
			f["overlay/kustomization.yaml"] = p
		},
		"strategic patch path disguised as inline": func(f map[string]sourceFixtureFile) {
			p := f["overlay/kustomization.yaml"]
			p.body += "patchesStrategicMerge:\n  - |\n    /outside/patch\n"
			f["overlay/kustomization.yaml"] = p
		},
		"strategic brace path disguised as inline": func(f map[string]sourceFixtureFile) {
			p := f["overlay/kustomization.yaml"]
			p.body += "patchesStrategicMerge: ['{not: a-resource}']\n"
			f["overlay/kustomization.yaml"] = p
		},
		"directory generator source": func(f map[string]sourceFixtureFile) {
			p := f["overlay/kustomization.yaml"]
			p.body = strings.Replace(p.body, "settings=config.txt", "settings=../base", 1)
			f["overlay/kustomization.yaml"] = p
		},
		"missing file": func(f map[string]sourceFixtureFile) { delete(f, "overlay/patch.yaml") },
		"file symlink": func(f map[string]sourceFixtureFile) {
			f["overlay/config.txt"] = sourceFixtureFile{"120000", "/etc/passwd"}
		},
		"directory symlink": func(f map[string]sourceFixtureFile) {
			delete(f, "base/kustomization.yaml")
			delete(f, "base/deployment.yaml")
			f["base"] = sourceFixtureFile{"120000", "/outside"}
		},
		"submodule": func(f map[string]sourceFixtureFile) {
			delete(f, "base/kustomization.yaml")
			delete(f, "base/deployment.yaml")
			f["base"] = sourceFixtureFile{"160000", ""}
		},
		"submodule configuration": func(f map[string]sourceFixtureFile) {
			f[".gitmodules"] = sourceFixtureFile{"100644", "[submodule x]\n"}
		},
		"checkout filter": func(f map[string]sourceFixtureFile) {
			f["overlay/.gitattributes"] = sourceFixtureFile{"100644", "* filter=dynamic\n"}
		},
		"attribute macro": func(f map[string]sourceFixtureFile) {
			f[".gitattributes"] = sourceFixtureFile{"100644", "[attr]dynamic filter=external\n"}
		},
		"global Argo source override": func(f map[string]sourceFixtureFile) {
			f["overlay/.argocd-source.yaml"] = sourceFixtureFile{"100644", "plugin: {name: dynamic}\n"}
		},
		"named Argo source override": func(f map[string]sourceFixtureFile) {
			f["overlay/.argocd-source-installation.yaml"] = sourceFixtureFile{"100644", "kustomize: {images: [mutable:latest]}\n"}
		},
		"custom generator": func(f map[string]sourceFixtureFile) {
			p := f["overlay/kustomization.yaml"]
			p.body += "generators: [execute.yaml]\n"
			f["overlay/kustomization.yaml"] = p
		},
		"helm": func(f map[string]sourceFixtureFile) {
			p := f["overlay/kustomization.yaml"]
			p.body += "helmCharts: [{name: remote}]\n"
			f["overlay/kustomization.yaml"] = p
		},
		"unknown file-bearing feature": func(f map[string]sourceFixtureFile) {
			p := f["overlay/kustomization.yaml"]
			p.body += "replacements: [{path: external.yaml}]\n"
			f["overlay/kustomization.yaml"] = p
		},
		"host env fallback": func(f map[string]sourceFixtureFile) {
			f["overlay/vars.env"] = sourceFixtureFile{"100644", "HOST_SECRET\n"}
		},
		"unqualified transformer": func(f map[string]sourceFixtureFile) {
			f["overlay/namespace.yaml"] = sourceFixtureFile{"100644", "apiVersion: example/v1\nkind: ExecTransformer\nmetadata: {name: exec}\n"}
		},
		"duplicate key": func(f map[string]sourceFixtureFile) {
			p := f["overlay/kustomization.yaml"]
			p.body += "resources: [other]\n"
			f["overlay/kustomization.yaml"] = p
		},
		"alias": func(f map[string]sourceFixtureFile) {
			p := f["overlay/kustomization.yaml"]
			p.body += "commonLabels: &labels {x: y}\ncommonAnnotations: *labels\n"
			f["overlay/kustomization.yaml"] = p
		},
		"multiple documents": func(f map[string]sourceFixtureFile) {
			p := f["overlay/kustomization.yaml"]
			p.body += "---\nkind: Kustomization\n"
			f["overlay/kustomization.yaml"] = p
		},
		"multiple settings": func(f map[string]sourceFixtureFile) { f["overlay/Kustomization"] = f["overlay/kustomization.yaml"] },
		"recursion": func(f map[string]sourceFixtureFile) {
			p := f["base/kustomization.yaml"]
			p.body = strings.Replace(p.body, "deployment.yaml", "../overlay", 1)
			f["base/kustomization.yaml"] = p
		},
		"component kind mismatch": func(f map[string]sourceFixtureFile) {
			p := f["component/kustomization.yaml"]
			p.body = strings.Replace(p.body, "kind: Component", "kind: Kustomization", 1)
			f["component/kustomization.yaml"] = p
		},
	} {
		t.Run(name, func(t *testing.T) {
			files := sourceFilesFixture()
			change(files)
			objects, spec := sourceObjectsFixture(t, files)
			closed, err := VerifySourceClosure(context.Background(), objects, spec)
			require.Error(t, err)
			require.Empty(t, closed.Digest())
		})
	}
}

type objectReaderFunc func(context.Context, string, string) (io.ReadCloser, error)

func (f objectReaderFunc) OpenGitObject(ctx context.Context, kind, oid string) (io.ReadCloser, error) {
	return f(ctx, kind, oid)
}

func TestSourceClosureBoundsReadsAndRedactsProviderErrors(t *testing.T) {
	_, spec := sourceObjectsFixture(t, sourceFilesFixture())
	reader := objectReaderFunc(func(context.Context, string, string) (io.ReadCloser, error) {
		return nil, fmt.Errorf("private credential in provider diagnostic")
	})
	_, err := VerifySourceClosure(context.Background(), reader, spec)
	require.EqualError(t, err, "Git source object could not be read")
	reader = objectReaderFunc(func(context.Context, string, string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(strings.Repeat("x", maxSourceObject+1))), nil
	})
	_, err = VerifySourceClosure(context.Background(), reader, spec)
	require.ErrorContains(t, err, "byte bound")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reader = objectReaderFunc(func(context.Context, string, string) (io.ReadCloser, error) {
		t.Fatal("canceled proof read an object")
		return nil, nil
	})
	_, err = VerifySourceClosure(ctx, reader, spec)
	require.ErrorIs(t, err, context.Canceled)
	objects, spec := sourceObjectsFixture(t, sourceFilesFixture())
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	reader = objectReaderFunc(func(ctx context.Context, kind, oid string) (io.ReadCloser, error) {
		body, err := objects.OpenGitObject(ctx, kind, oid)
		cancel()
		return body, err
	})
	_, err = VerifySourceClosure(ctx, reader, spec)
	require.ErrorIs(t, err, context.Canceled, "a completed read must not hide cancellation")
}

func TestSourceClosureRejectsMalformedButCorrectlyHashedTrees(t *testing.T) {
	entry := func(mode, name string) []byte {
		return append([]byte(mode+" "+name+"\x00"), bytes.Repeat([]byte{1}, 20)...)
	}
	for name, tree := range map[string][]byte{
		"truncated identity": []byte("100644 file\x00short"),
		"missing separator":  []byte("100644 file"),
		"unsupported mode":   entry("100600", "file"),
		"escape name":        entry("100644", ".."),
		"path name":          entry("100644", "dir/file"),
		"control name":       entry("100644", "file\x7f"),
		"duplicate name":     append(entry("100644", "file"), entry("40000", "file")...),
	} {
		t.Run(name, func(t *testing.T) {
			objects, spec := sourceObjectsFixture(t, sourceFilesFixture())
			root := objects.add("tree", tree)
			commit := objects.add("commit", []byte("tree "+root+"\n\nmalformed tree fixture\n"))
			var source map[string]any
			require.NoError(t, json.Unmarshal(spec.Source, &source))
			source["targetRevision"] = commit
			var err error
			spec.Source, err = json.Marshal(source)
			require.NoError(t, err)
			closed, err := VerifySourceClosure(context.Background(), objects, spec)
			require.Error(t, err)
			require.NotContains(t, err.Error(), "does not match its requested identity", "the valid object hash must not hide malformed tree entries")
			require.Empty(t, closed.Digest())
		})
	}
}

func TestSourceClosureBoundsAggregateReadsAndObjectCount(t *testing.T) {
	for _, bound := range []string{"bytes", "objects"} {
		t.Run(bound, func(t *testing.T) {
			files := sourceFilesFixture()
			settings := files["overlay/kustomization.yaml"]
			settings.body += "patchesJson6902:\n"
			count := maxSourceCount
			if bound == "bytes" {
				count = maxSourceBytes/(maxSourceObject-32) + 1
			}
			for i := 0; i < count; i++ {
				name := fmt.Sprintf("extra-%04d.yaml", i)
				body := fmt.Sprintf("unique: %d\n", i)
				if bound == "bytes" {
					body += strings.Repeat("#", maxSourceObject-32-len(body))
				}
				files["overlay/"+name] = sourceFixtureFile{"100644", body}
				settings.body += "  - path: " + name + "\n"
			}
			files["overlay/kustomization.yaml"] = settings
			objects, spec := sourceObjectsFixture(t, files)
			closed, err := VerifySourceClosure(context.Background(), objects, spec)
			require.ErrorContains(t, err, "bound")
			require.Empty(t, closed.Digest())
		})
	}
}
