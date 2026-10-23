package argocd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// This opt-in rehearsal reads committed objects from explicit local repositories.
// It never reads checkout files, fetches a ref, or contacts an installed cluster.
func TestSourceGraphOfCommittedProductOverlays(t *testing.T) {
	product := os.Getenv("MEMQL_ARGOCD_GRAPH_TEST_PRODUCT")
	if product == "" {
		t.Skip("set MEMQL_ARGOCD_GRAPH_TEST_PRODUCT, MEMQL_ARGOCD_GRAPH_TEST_ENGINE, MEMQL_ARGOCD_GRAPH_TEST_REPOSITORY and MEMQL_ARGOCD_GRAPH_TEST_COMMIT for a committed-product source rehearsal")
	}
	engine := os.Getenv("MEMQL_ARGOCD_GRAPH_TEST_ENGINE")
	repository := os.Getenv("MEMQL_ARGOCD_GRAPH_TEST_REPOSITORY")
	commit := os.Getenv("MEMQL_ARGOCD_GRAPH_TEST_COMMIT")
	require.NotEmpty(t, engine)
	require.True(t, canonicalGitHubSource(repository), "select the canonical product repository identity")
	require.True(t, commitSHA.MatchString(commit), "select one full immutable product commit")
	ctx := context.Background()
	type inventory struct {
		Source RenderSpec   `json:"source"`
		Edges  []SourceEdge `json:"edges"`
		Files  []SourceFile `json:"files"`
		Digest string       `json:"digest"`
	}
	var inventories []inventory
	capture := func(t *testing.T, objects GitObjects, spec SourceFragmentSpec) SourceFragment {
		t.Helper()
		var archive bytes.Buffer
		fragment, err := CaptureSourceFragmentArchive(ctx, objects, spec, &archive)
		require.NoError(t, err)
		fresh, err := VerifySourceFragmentArchive(ctx, archive.Bytes(), spec)
		require.NoError(t, err)
		require.Equal(t, fragment.Digest(), fresh.Digest())
		return fresh
	}
	for _, entry := range []struct {
		directory    string
		dependencies int
	}{{"deploy/k8s/instance", 3}, {"deploy/k8s/overlays/cloud", 0}} {
		t.Run(entry.directory, func(t *testing.T) {
			render := renderSpecFixture()
			var err error
			render.Source, err = json.Marshal(map[string]string{
				"repoURL":        repository,
					"path":           entry.directory,
				"targetRevision": commit,
			})
			require.NoError(t, err)
			spec := SourceFragmentSpec{Render: render, AllowedDependencies: []SourceRepository{{
				Repository: "https://github.com/znasllc-io/memql.git",
				Paths: []string{
					"deploy/k8s/overlays/cloud-entry",
					"deploy/k8s/components/pipeline-image-builder",
					"deploy/k8s/components/pipelines-capacity",
				},
			}}}
			root := capture(t, localTestGitObjects{product}, spec)
			require.Len(t, root.Dependencies(), entry.dependencies)
			dependencies := make([]SourceFragment, 0, len(root.Dependencies()))
			for _, dependency := range root.Dependencies() {
				dependencySpec, err := root.DependencySpec(dependency)
				require.NoError(t, err)
				dependencies = append(dependencies, capture(t, localTestGitObjects{engine}, dependencySpec))
			}
			closed, err := AssembleSourceClosure(ctx, root, dependencies)
			require.NoError(t, err)
			require.NotEmpty(t, closed.Files())
			again, err := AssembleSourceClosure(ctx, root, dependencies)
			require.NoError(t, err)
			require.Equal(t, closed.Digest(), again.Digest())
			inventories = append(inventories, inventory{Source: render, Edges: append([]SourceEdge(nil), root.inputs.edges...), Files: closed.Files(), Digest: closed.Digest()})
			t.Logf("verified committed product overlay: commit=%s dependencies=%d inputs=%d digest=%s", commit, len(dependencies), len(closed.Files()), closed.Digest())
		})
	}
	if directory := os.Getenv("MEMQL_ARGOCD_GRAPH_TEST_EVIDENCE"); directory != "" && !t.Failed() {
		// Metadata only, for the separate pinned-renderer comparison. A new
		// directory prevents an old or partial inventory from being reused.
		require.NoError(t, os.Mkdir(directory, 0700))
		body, err := json.MarshalIndent(inventories, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(directory, "closure.json"), append(body, '\n'), 0600))
	}
}
