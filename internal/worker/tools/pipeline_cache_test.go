package tools

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPipelineCachesSeparateEnrollmentScopePlatformAndImage(t *testing.T) {
	base := pipelineStepRequest{cacheHome: "first-cluster", cacheScope: strings.Repeat("a", 64), cloneURL: "https://github.com/owner/repo.git", platform: "linux/arm64", image: containerTestDigest}
	identities := map[string]bool{pipelineCacheIdentity(base): true}
	for _, change := range []func(*pipelineStepRequest){
		func(r *pipelineStepRequest) { r.cacheHome = "second-cluster" },
		func(r *pipelineStepRequest) { r.cacheScope = strings.Repeat("b", 64) },
		func(r *pipelineStepRequest) { r.cloneURL = "https://other.example/owner/repo.git" },
		func(r *pipelineStepRequest) { r.platform = "linux/amd64" },
		func(r *pipelineStepRequest) { r.image += "another" },
	} {
		req := base
		change(&req)
		key := pipelineCacheIdentity(req)
		if !pipelineCacheHash.MatchString(key) || identities[key] {
			t.Fatalf("cache boundary collided: %+v", req)
		}
		identities[key] = true
	}
	if pipelineCacheIdentity(base) != pipelineCacheIdentity(base) {
		t.Fatal("cache not reusable")
	}
}

func TestPipelineCacheRetentionAndLinks(t *testing.T) {
	reservation := testPipelineReservation(t)
	if reservation.dirty {
		t.Fatal("dirty fixture")
	}
	oldLimit := pipelineCacheRetainedBytes
	pipelineCacheRetainedBytes = 10
	t.Cleanup(func() { pipelineCacheRetainedBytes = oldLimit })
	req := pipelineStepRequest{cacheHome: "cluster", cacheScope: strings.Repeat("a", 64), image: containerTestDigest}
	if err := preparePipelineCache(&req); err != nil {
		t.Fatal(err)
	}
	old := req.cacheDir
	if err := os.WriteFile(filepath.Join(old, "file"), []byte("12345678"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(old, time.Unix(1, 0), time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	req.cacheScope = strings.Repeat("b", 64)
	if err := preparePipelineCache(&req); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(req.cacheDir, "file"), []byte("12345678"), 0400); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "keep")
	if err := os.WriteFile(outside, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(old, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(old, time.Unix(1, 0), time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	if err := prunePipelineCaches(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(old); !os.IsNotExist(err) {
		t.Fatal("old cache retained above budget")
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "keep" {
		t.Fatal("cache cleanup followed link")
	}
	root, _ := pipelineCachesRoot()
	link := filepath.Join(root, strings.Repeat("c", 64))
	if err := os.Symlink(filepath.Dir(outside), link); err != nil {
		t.Fatal(err)
	}
	if err := prunePipelineCaches(); err == nil {
		t.Fatal("cache-root link accepted")
	}
	if err := ensurePipelineCacheDirectory(link); err == nil {
		t.Fatal("mounted a link into the worker home")
	}
}

func TestPipelineCachesRequireScopeAndLocalEnrollment(t *testing.T) {
	fx := newPipelineFixture(t)
	allowLocalClones(t)
	base := map[string]any{"execution": "container", "platform": "linux/" + runtime.GOARCH, "image": containerTestDigest, "caches": []any{"go"}, "cacheScope": strings.Repeat("a", 64)}
	for _, edit := range []map[string]any{
		{"cacheScope": ""}, {"cacheScope": "../../owner"}, {"caches": []any{"unknown"}}, {"caches": []any{"go", "go"}}, {"env": map[string]any{"GOCACHE": "/host"}}, {"secrets": map[string]any{"GOMODCACHE": "secret"}},
	} {
		extra := map[string]any{}
		for k, v := range base {
			extra[k] = v
		}
		for k, v := range edit {
			extra[k] = v
		}
		if _, err := parsePipelineStep(stepArgs(t, fx, "exit 0", extra)); err == nil {
			t.Fatalf("unsafe cache declaration admitted: %v", edit)
		}
	}
	policy, root := pipelineTestPolicy(t, "")
	_, fail := runPipelineStep(context.Background(), "", stepArgs(t, fx, "exit 0", base), policy, nil, nil)
	if fail == nil || fail.GetErrorCode() != "bad_request" {
		t.Fatalf("unbound cluster got a cache: %v", fail)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("unbound request cloned")
	}
}

func TestPipelineCachesRealDocker(t *testing.T) {
	image := os.Getenv("MEMQL_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("set MEMQL_TEST_DOCKER_IMAGE to require persistent container cache tests")
	}
	fx := newPipelineFixture(t)
	allowLocalClones(t)
	policy, _ := pipelineTestPolicy(t, "")
	extra := map[string]any{"execution": "container", "platform": "linux/" + runtime.GOARCH, "image": image, "caches": []any{"go", "npm"}, "cacheScope": strings.Repeat("a", 64)}
	ctx := context.WithValue(context.Background(), pipelineCacheHomeKey{}, "first-cluster")
	var output recordedOutput
	run := func(ctx context.Context, command string) {
		t.Helper()
		result := decoded(t)(runPipelineStep(ctx, "", stepArgs(t, fx, command, extra), policy, output.emit, output.artifact))
		if result.ExitCode != 0 {
			t.Fatalf("cache command exited %d: %s %s", result.ExitCode, output.text(false), output.text(true))
		}
	}
	run(ctx, `test "$GOCACHE" = /cache/go/build && test "$GOMODCACHE" = /cache/go/mod && test "$npm_config_cache" = /cache/npm && mkdir -p "$GOCACHE" && echo retained > "$GOCACHE/proof"`)
	run(ctx, `test "$(cat "$GOCACHE/proof")" = retained`)
	run(context.WithValue(context.Background(), pipelineCacheHomeKey{}, "second-cluster"), `test ! -e "$GOCACHE/proof"`)
	extra["cacheScope"] = strings.Repeat("b", 64)
	run(ctx, `test ! -e "$GOCACHE/proof"`)
}
