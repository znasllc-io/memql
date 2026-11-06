package tools

import (
	"context"
	"github.com/znasllc-io/memql/cockpit/internal/worker/capacity"
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
	release, err := preparePipelineCache(&req)
	if err != nil {
		t.Fatal(err)
	}
	// Each build gives its cache back when it ends; only then is it counted
	// against the budget, and only then can it be removed.
	release()
	old := req.cacheDir
	if err := os.WriteFile(filepath.Join(old, "file"), []byte("12345678"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(old, time.Unix(1, 0), time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	req.cacheScope = strings.Repeat("b", 64)
	if release, err = preparePipelineCache(&req); err != nil {
		t.Fatal(err)
	}
	release()
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
	pipelineHostMemory = capacity.ReadSystemHeadroom
	pipelineHostCPUs = runtime.NumCPU
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

// A cache a running step mounts is neither measured nor removed by another
// step's prune, even when it is the oldest; once its step ends it is counted
// and can go.
func TestPipelineCachePruneLeavesACacheARunningStepMounts(t *testing.T) {
	testPipelineReservation(t)
	oldLimit := pipelineCacheRetainedBytes
	pipelineCacheRetainedBytes = 10
	t.Cleanup(func() { pipelineCacheRetainedBytes = oldLimit })
	prepare := func(scope string, age int64) (string, func()) {
		t.Helper()
		req := pipelineStepRequest{cacheHome: "cluster", cacheScope: strings.Repeat(scope, 64), image: containerTestDigest}
		release, err := preparePipelineCache(&req)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(req.cacheDir, "file"), []byte("12345678"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(req.cacheDir, time.Unix(age, 0), time.Unix(age, 0)); err != nil {
			t.Fatal(err)
		}
		return req.cacheDir, release
	}
	mounted, releaseMounted := prepare("a", 1)
	older, releaseOlder := prepare("b", 2)
	releaseOlder()
	newer, releaseNewer := prepare("c", 3)
	releaseNewer()
	if err := prunePipelineCaches(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mounted); err != nil {
		t.Fatalf("a mounted cache was pruned: %v", err)
	}
	if _, err := os.Stat(older); !os.IsNotExist(err) {
		t.Fatalf("the oldest idle cache survived over budget: %v", err)
	}
	if _, err := os.Stat(newer); err != nil {
		t.Fatalf("a cache within budget was pruned: %v", err)
	}
	releaseMounted()
	releaseMounted() // a release is idempotent
	if err := prunePipelineCaches(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mounted); !os.IsNotExist(err) {
		t.Fatalf("an idle cache over budget survived: %v", err)
	}
}

// An interrupted attempt in any slot may still have a container writing into a
// cache no running step counts: retention waits until it is reconciled.
func TestPipelineCachePruneWaitsForAnInterruptedSlot(t *testing.T) {
	testPipelineReservation(t)
	oldLimit := pipelineCacheRetainedBytes
	pipelineCacheRetainedBytes = 1
	t.Cleanup(func() { pipelineCacheRetainedBytes = oldLimit })
	req := pipelineStepRequest{cacheHome: "cluster", cacheScope: strings.Repeat("d", 64), image: containerTestDigest}
	release, err := preparePipelineCache(&req)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if err := os.WriteFile(filepath.Join(req.cacheDir, "file"), []byte("12345678"), 0600); err != nil {
		t.Fatal(err)
	}
	root, _ := pipelineCapacityRoot()
	slot := capacity.PipelineSlotFile(root, 3)
	if err := os.WriteFile(slot, []byte(`{"execution":"container"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := prunePipelineCaches(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(req.cacheDir); err != nil {
		t.Fatalf("a cache was pruned beside an interrupted attempt: %v", err)
	}
	if err := os.Truncate(slot, 0); err != nil {
		t.Fatal(err)
	}
	if err := prunePipelineCaches(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(req.cacheDir); !os.IsNotExist(err) {
		t.Fatalf("retention did not resume after reconciliation: %v", err)
	}
}
