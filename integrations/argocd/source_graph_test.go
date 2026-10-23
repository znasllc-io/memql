package argocd

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func sourceGraphFixture(t *testing.T) (objectFixture, SourceFragmentSpec, map[string]objectFixture) {
	t.Helper()
	versions := map[string]objectFixture{}
	refs := []string{}
	for _, value := range []string{"old", "new"} {
		objects, spec := sourceObjectsFixture(t, map[string]sourceFixtureFile{
			"overlay/kustomization.yaml": {"100644", "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: [value.yaml]\n"},
			"overlay/value.yaml":         {"100644", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: " + value + "}\n"},
		})
		var source map[string]any
		require.NoError(t, json.Unmarshal(spec.Source, &source))
		commit := stringAt(source, "targetRevision")
		versions[commit] = objects
		refs = append(refs, "https://github.com/example/engine//overlay?ref="+commit+"&timeout=120s&submodules=false")
	}
	objects, render := sourceObjectsFixture(t, map[string]sourceFixtureFile{
		"overlay/kustomization.yaml": {"100644", "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n - " + refs[0] + "\n - " + refs[1] + "\n - " + refs[0] + "\n"},
	})
	var source map[string]any
	require.NoError(t, json.Unmarshal(render.Source, &source))
	source["repoURL"] = "https://github.com/example/product.git"
	render.Source, _ = json.Marshal(source)
	return objects, SourceFragmentSpec{Render: render, AllowedDependencies: []SourceRepository{{Repository: "https://github.com/example/engine.git", Paths: []string{"overlay"}}}}, versions
}
func captureGraphFixture(t *testing.T, objects objectFixture, spec SourceFragmentSpec) (SourceFragment, []byte) {
	t.Helper()
	var archive bytes.Buffer
	captured, err := CaptureSourceFragmentArchive(context.Background(), objects, spec, &archive)
	require.NoError(t, err)
	verified, err := VerifySourceFragmentArchive(context.Background(), archive.Bytes(), spec)
	require.NoError(t, err)
	require.Equal(t, captured.Digest(), verified.Digest())
	return verified, archive.Bytes()
}
func graphDependencyFixtures(t *testing.T, root SourceFragment, versions map[string]objectFixture) []SourceFragment {
	t.Helper()
	dependencies := []SourceFragment{}
	for _, dependency := range root.Dependencies() {
		spec, err := root.DependencySpec(dependency)
		require.NoError(t, err)
		fragment, _ := captureGraphFixture(t, versions[dependency.Revision], spec)
		dependencies = append(dependencies, fragment)
	}
	return dependencies
}

func TestSourceGraphBindsDifferentCommitsAndDeduplicatesAcquisition(t *testing.T) {
	objects, spec, versions := sourceGraphFixture(t)
	root, archive := captureGraphFixture(t, objects, spec)
	require.Len(t, root.Dependencies(), 2)
	dependencies := graphDependencyFixtures(t, root, versions)
	closed, err := AssembleSourceClosure(context.Background(), root, dependencies)
	require.NoError(t, err)
	require.Len(t, closed.Files(), 5)
	commits := map[string]bool{}
	for _, file := range closed.Files() {
		require.NotEmpty(t, file.Repository)
		require.NotEmpty(t, file.Revision)
		if file.Repository == spec.AllowedDependencies[0].Repository {
			commits[file.Revision] = true
		}
	}
	require.Len(t, commits, 2)
	require.NotEqual(t, root.Digest(), closed.Digest())
	dependencies[0], dependencies[1] = dependencies[1], dependencies[0]
	reordered, err := AssembleSourceClosure(context.Background(), root, dependencies)
	require.NoError(t, err)
	require.Equal(t, closed.Digest(), reordered.Digest())
	legacy, err := VerifySourceArchive(context.Background(), archive, spec.Render)
	require.Error(t, err)
	require.Empty(t, legacy.Digest(), "partial archive must never manufacture a closed proof")
	copy := root.Spec()
	copy.AllowedDependencies[0].Paths[0] = "other"
	require.Equal(t, "overlay", root.Spec().AllowedDependencies[0].Paths[0])
}
func TestSourceGraphRequiresExactCompleteProofAndAggregateBounds(t *testing.T) {
	objects, spec, versions := sourceGraphFixture(t)
	root, _ := captureGraphFixture(t, objects, spec)
	deps := graphDependencyFixtures(t, root, versions)
	for name, fragments := range map[string][]SourceFragment{"missing": deps[:1], "extra": append(append([]SourceFragment{}, deps...), deps[0]), "duplicate": {deps[0], deps[0]}, "root": {root, deps[1]}} {
		t.Run(name, func(t *testing.T) {
			closed, err := AssembleSourceClosure(context.Background(), root, fragments)
			require.Error(t, err)
			require.Empty(t, closed.Digest())
		})
	}
	for name, mutate := range map[string]func(*SourceFragment){"bytes": func(f *SourceFragment) { f.inputs.bytes = maxSourceBytes }, "objects": func(f *SourceFragment) { f.inputs.objects = maxSourceCount }, "settings": func(f *SourceFragment) { f.inputs.settings = 256 }} {
		t.Run(name, func(t *testing.T) {
			copies := append([]SourceFragment{}, deps...)
			mutate(&copies[0])
			closed, err := AssembleSourceClosure(context.Background(), root, copies)
			require.Error(t, err)
			require.Empty(t, closed.Digest())
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := AssembleSourceClosure(ctx, root, deps)
	require.Error(t, err)
	altered := append([]SourceFragment{}, deps...)
	changed := altered[0].Spec()
	changed.Render.Namespace = "other"
	altered[0].spec, _ = json.Marshal(changed)
	_, err = AssembleSourceClosure(context.Background(), root, altered)
	require.Error(t, err)
	_, err = root.DependencySpec(SourceDependency{Repository: "https://github.com/example/engine.git", Revision: strings.Repeat("a", 40), Path: "overlay"})
	require.Error(t, err)
}
func TestSourceDependencyReferenceRequiresExactQualifiedCommit(t *testing.T) {
	allowed := []SourceRepository{{Repository: "https://github.com/example/engine.git", Paths: []string{"overlay"}}}
	valid := "https://github.com/example/engine//overlay?ref=" + strings.Repeat("a", 40) + "&timeout=120s&submodules=false"
	dependency, err := ParseSourceDependency(valid, false, allowed)
	require.NoError(t, err)
	require.Equal(t, allowed[0].Repository, dependency.Repository)
	invalid := []string{
		strings.Replace(valid, strings.Repeat("a", 40), "v0.30.9", 1), strings.Replace(valid, strings.Repeat("a", 40), "main", 1), strings.Replace(valid, strings.Repeat("a", 40), strings.Repeat("a", 39), 1),
		valid + "&ref=" + strings.Repeat("b", 40), valid + "&other=true", valid + "#fragment", strings.Replace(valid, "120s", "5m", 1), strings.Replace(valid, "false", "true", 1),
		strings.Replace(valid, "github.com", "example.com", 1), strings.Replace(valid, "github.com", "user@github.com", 1), strings.Replace(valid, "github.com", "github.com:443", 1),
		strings.Replace(valid, "//overlay", "//../overlay", 1), strings.Replace(valid, "//overlay", "//other", 1), strings.Replace(valid, "example/engine", "example/other", 1), strings.Replace(valid, "overlay", "%6fverlay", 1), strings.Replace(valid, "https:", "http:", 1), "git::" + valid,
	}
	for _, reference := range invalid {
		_, err := ParseSourceDependency(reference, false, allowed)
		require.Error(t, err, reference)
	}
	// Pinned Kustomize v5.4.3 searches for _git/ as a substring before //,
	// even inside a directory, organization or repository name. Each binding
	// below is otherwise admitted, so the syntax gate must reject the marker.
	for _, marker := range []struct {
		name, repository, directory string
	}{
		{"path segment", "https://github.com/example/engine.git", "foo/_git/repository"},
		{"path suffix", "https://github.com/example/engine.git", "foo_git/repository"},
		{"organization suffix", "https://github.com/example_git/engine.git", "overlay"},
		{"repository suffix", "https://github.com/example/engine_git.git", "overlay"},
	} {
		t.Run(marker.name, func(t *testing.T) {
			reference := strings.TrimSuffix(marker.repository, ".git") + "//" + marker.directory + "?ref=" + strings.Repeat("a", 40) + "&timeout=120s&submodules=false"
			bindings := []SourceRepository{{Repository: marker.repository, Paths: []string{marker.directory}}}
			require.NoError(t, ValidateSourceRepositories(bindings))
			_, err := ParseSourceDependencyReference(reference, false)
			require.Error(t, err)
			_, err = ParseSourceDependency(reference, false, bindings)
			require.Error(t, err)
		})
	}
	_, err = ParseSourceDependency(valid, false, nil)
	require.Error(t, err)
	_, err = ParseSourceDependencyReference(strings.Replace(valid, "//overlay", "//other", 1), false)
	require.NoError(t, err, "syntax-only codec grants no acquisition")
}
func TestDependencyFragmentRejectsSecondRemoteHop(t *testing.T) {
	objects, spec, _ := sourceGraphFixture(t)
	spec.Dependency = true
	spec.AllowedDependencies = append(spec.AllowedDependencies, SourceRepository{Repository: "https://github.com/example/product.git", Paths: []string{"overlay"}})
	var archive bytes.Buffer
	fragment, err := CaptureSourceFragmentArchive(context.Background(), objects, spec, &archive)
	require.Error(t, err)
	require.Empty(t, fragment.Digest())
}
