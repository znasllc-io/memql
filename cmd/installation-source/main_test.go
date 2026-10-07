package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/integrations/argocd"
)

func fixtureGit(t *testing.T, repository string, input io.Reader, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", repository}, args...)...)
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid",
		"GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid",
	}
	command.Stdin = input
	body, err := command.CombinedOutput()
	require.NoError(t, err, string(body))
	return strings.TrimSpace(string(body))
}

func collectorFixture(t *testing.T) (checkoutObjects, argocd.RenderSpec) {
	t.Helper()
	git, err := exec.LookPath("git")
	require.NoError(t, err, "collector qualification requires real Git")
	repository := t.TempDir()
	fixtureGit(t, repository, nil, "init", "--quiet", "--object-format=sha1")
	require.NoError(t, os.Mkdir(filepath.Join(repository, "overlay"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(repository, "overlay", "kustomization.yaml"), []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: [settings.yaml]\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(repository, "overlay", "settings.yaml"), []byte("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: settings}\ndata: {value: private-fixture-material}\n"), 0600))
	fixtureGit(t, repository, nil, "add", "overlay/kustomization.yaml", "overlay/settings.yaml")
	fixtureGit(t, repository, nil, "commit", "--quiet", "--no-gpg-sign", "-m", "source fixture")
	commit := fixtureGit(t, repository, nil, "rev-parse", "HEAD")
	spec := argocd.RenderSpec{
		Source:  json.RawMessage(`{"repoURL":"https://github.com/example/installation.git","path":"overlay","targetRevision":"` + commit + `"}`),
		AppName: "installation", AppLabelKey: "app.kubernetes.io/instance", Namespace: "memql", ProjectName: "installation",
		ProjectSourceRepos: []string{"https://github.com/example/installation.git"}, TrackingMethod: "annotation+label",
		InstallationID: "installation", KubeVersion: "1.32.13", APIVersions: []string{"v1/ConfigMap"},
	}
	return checkoutObjects{repository: repository, git: git}, spec
}

func TestCollectorCapturesCommittedObjectsIgnoringCheckoutAndReplacement(t *testing.T) {
	objects, spec := collectorFixture(t)
	output := filepath.Join(t.TempDir(), "source.tar")
	first, changed, err := capture(context.Background(), objects, spec, output)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, 2, first.Files)
	archive, err := os.ReadFile(output)
	require.NoError(t, err)
	closed, err := argocd.VerifySourceArchive(context.Background(), archive, spec)
	require.NoError(t, err)
	require.Equal(t, first.Digest, closed.Digest())
	info, err := os.Stat(output)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())

	// Git normally honors replacement refs. Neither those refs, dirty files,
	// nor inherited Git config/object-directory overrides may alter this proof.
	blob := fixtureGit(t, objects.repository, nil, "rev-parse", "HEAD:overlay/settings.yaml")
	replacement := fixtureGit(t, objects.repository, strings.NewReader("substituted bytes"), "hash-object", "-w", "--stdin")
	fixtureGit(t, objects.repository, nil, "replace", blob, replacement)
	require.Equal(t, "substituted bytes", fixtureGit(t, objects.repository, nil, "cat-file", "blob", blob))
	require.NoError(t, os.WriteFile(filepath.Join(objects.repository, "overlay", "settings.yaml"), []byte("dirty working tree"), 0600))
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.repositoryFormatVersion")
	t.Setenv("GIT_CONFIG_VALUE_0", "99999")
	secondPath := filepath.Join(t.TempDir(), "source.tar")
	second, changed, err := capture(context.Background(), objects, spec, secondPath)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, first, second)
	again, err := os.ReadFile(secondPath)
	require.NoError(t, err)
	require.Equal(t, archive, again)
	second, changed, err = capture(context.Background(), objects, spec, output)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, first, second)
	entries, err := os.ReadDir(filepath.Dir(output))
	require.NoError(t, err)
	require.Len(t, entries, 1, "successful and repeated collection must leave no temporary file")
}

func TestCollectorDoesNotReplaceExistingOutputOrLeakPartialFiles(t *testing.T) {
	objects, spec := collectorFixture(t)
	directory := t.TempDir()
	output := filepath.Join(directory, "source.tar")
	require.NoError(t, os.WriteFile(output, []byte("existing unrelated file"), 0600))
	_, changed, err := capture(context.Background(), objects, spec, output)
	require.Error(t, err)
	require.False(t, changed)
	prior, err := os.ReadFile(output)
	require.NoError(t, err)
	require.Equal(t, "existing unrelated file", string(prior))
	link := filepath.Join(directory, "linked.tar")
	require.NoError(t, os.Symlink(output, link))
	_, changed, err = capture(context.Background(), objects, spec, link)
	require.Error(t, err)
	require.False(t, changed)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, changed, err = capture(ctx, objects, spec, filepath.Join(directory, "cancelled.tar"))
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, changed)
	broken := objects
	broken.repository = filepath.Join(t.TempDir(), "missing")
	_, changed, err = capture(context.Background(), broken, spec, filepath.Join(directory, "failed.tar"))
	require.Error(t, err)
	require.False(t, changed)
	require.NotContains(t, err.Error(), broken.repository)
	entries, err := os.ReadDir(directory)
	require.NoError(t, err)
	require.Len(t, entries, 2, "failed captures must leave only the original file and link")
}

func TestCollectorCommandProducesOneContentFreeResult(t *testing.T) {
	objects, spec := collectorFixture(t)
	directory := t.TempDir()
	body, err := json.Marshal(spec)
	require.NoError(t, err)
	specPath := filepath.Join(directory, "spec.json")
	require.NoError(t, os.WriteFile(specPath, body, 0600))
	args := []string{"--repository=" + objects.repository, "--spec=" + specPath, "--output=" + filepath.Join(directory, "source.tar")}
	for attempt := 0; attempt < 2; attempt++ {
		var stdout, stderr bytes.Buffer
		require.Equal(t, 0, execute(context.Background(), args, &stdout, &stderr), stderr.String())
		require.NotContains(t, stdout.String()+stderr.String(), "private-fixture-material")
		var result struct {
			OK      bool          `json:"ok"`
			Changed bool          `json:"changed"`
			Result  captureResult `json:"result"`
		}
		d := json.NewDecoder(&stdout)
		require.NoError(t, d.Decode(&result))
		require.ErrorIs(t, d.Decode(new(any)), io.EOF)
		require.True(t, result.OK)
		require.Equal(t, attempt == 0, result.Changed)
		require.Equal(t, 2, result.Result.Files)
	}
	for name, invalid := range map[string]string{
		"unknown":    strings.TrimSuffix(string(body), "}") + `,"unexpected":true}`,
		"duplicate":  strings.TrimSuffix(string(body), "}") + `,"namespace":"other"}`,
		"case alias": strings.TrimSuffix(string(body), "}") + `,"Namespace":"other"}`,
		"trailing":   string(body) + `{}`,
		"wrong type": `[]`,
		"incomplete": `{}`,
		"oversized":  strings.Repeat(" ", (512<<10)+1),
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(specPath, []byte(invalid), 0600))
			var stdout, stderr bytes.Buffer
			require.Equal(t, 2, execute(context.Background(), args, &stdout, &stderr))
			require.True(t, json.Valid(stdout.Bytes()))
		})
	}
	var stdout, stderr bytes.Buffer
	require.Equal(t, 2, execute(context.Background(), []string{"--repository=x", "unexpected"}, &stdout, &stderr))
	require.True(t, json.Valid(stdout.Bytes()))
}

func TestCollectorsRacingForOneOutputConvergeWithoutLeftovers(t *testing.T) {
	objects, spec := collectorFixture(t)
	directory := t.TempDir()
	output := filepath.Join(directory, "source.tar")
	var results [2]captureResult
	var changed [2]bool
	var failures [2]error
	var wait sync.WaitGroup
	start := make(chan struct{})
	for i := range results {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			results[i], changed[i], failures[i] = capture(context.Background(), objects, spec, output)
		}()
	}
	close(start)
	wait.Wait()
	for _, err := range failures {
		require.NoError(t, err)
	}
	require.NotEqual(t, changed[0], changed[1], "exactly one collector installs the completed output")
	require.Equal(t, results[0], results[1])
	entries, err := os.ReadDir(directory)
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

func TestGitObjectReaderReapsProcessAfterBoundedReadAndCancellation(t *testing.T) {
	objects, _ := collectorFixture(t)
	blob := fixtureGit(t, objects.repository, strings.NewReader(strings.Repeat("x", 5<<20)), "hash-object", "-w", "--stdin")
	for _, cancelRead := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		reader, err := objects.OpenGitObject(ctx, "blob", blob)
		require.NoError(t, err)
		_, err = io.ReadFull(reader, make([]byte, 32))
		require.NoError(t, err)
		if cancelRead {
			cancel()
		}
		require.Error(t, reader.Close(), "a partially consumed producer must be terminated")
		cancel()
		require.NotNil(t, reader.(*objectProcess).cmd.ProcessState, "Close must reap the process")
	}
}
