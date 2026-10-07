package argocd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

// This test-only reader qualifies the codec against actual committed repository
// objects. It is not the production source-acquisition adapter: that adapter
// still needs its own authenticated transport and resource bounds.
type localTestGitObjects struct{ repository string }

func (g localTestGitObjects) OpenGitObject(ctx context.Context, kind, oid string) (io.ReadCloser, error) {
	command := exec.CommandContext(ctx, "git", "--no-replace-objects", "-C", g.repository, "cat-file", kind, oid)
	body, err := command.Output()
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

func TestSourceClosureOfCommittedInstallationOverlays(t *testing.T) {
	repository := os.Getenv("MEMQL_ARGOCD_SOURCE_TEST_REPOSITORY")
	if repository == "" {
		t.Skip("set MEMQL_ARGOCD_SOURCE_TEST_REPOSITORY and MEMQL_ARGOCD_SOURCE_TEST_COMMIT for a read-only committed-object rehearsal")
	}
	commit := os.Getenv("MEMQL_ARGOCD_SOURCE_TEST_COMMIT")
	require.True(t, commitSHA.MatchString(commit), "select one full immutable commit")
	for _, overlay := range []string{"local", "cloud"} {
		t.Run(overlay, func(t *testing.T) {
			spec := renderSpecFixture()
			var err error
			spec.Source, err = json.Marshal(map[string]string{
				"repoURL":        "https://github.com/znasllc-io/memql.git",
				"path":           "deploy/k8s/overlays/" + overlay,
				"targetRevision": commit,
			})
			require.NoError(t, err)
			closed, err := VerifySourceClosure(context.Background(), localTestGitObjects{repository}, spec)
			require.NoError(t, err)
			require.Greater(t, len(closed.Files()), 20)
			again, err := VerifySourceClosure(context.Background(), localTestGitObjects{repository}, spec)
			require.NoError(t, err)
			require.Equal(t, closed.Digest(), again.Digest())
			t.Logf("verified committed %s overlay: commit=%s inputs=%d digest=%s", overlay, commit, len(closed.Files()), closed.Digest())
		})
	}
}
