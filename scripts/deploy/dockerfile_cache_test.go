// Build-speed guardrail (build-speed #1506): the engine Dockerfile that
// scripts/deploy/aks-deploy.sh build_one() feeds to `az acr build` must keep
// its BuildKit cache mounts on every Go step. The module cache (/go/pkg/mod)
// and the Go build cache (/root/.cache/go-build) are what let a rebuild of an
// unchanged tree reuse downloaded modules + already-compiled packages instead
// of redoing both from scratch. They are easy to silently drop on the next
// Dockerfile edit, so these static assertions fail fast if a cache mount (or
// the `# syntax` directive that enables the mount syntax) goes missing.
//
// This is a string assertion, not a real image build -- the actual cache-reuse
// behaviour is verified by `docker buildx` / `az acr build` in CI/ops.
package deploy

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// repoRootDockerfile returns the path to the engine Dockerfile at the repo
// root (two directories up from scripts/deploy), which is the one build_one()
// uses for the engine (voice/identity) images.
func repoRootDockerfile(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	p := filepath.Join(filepath.Dir(thisFile), "..", "..", "Dockerfile")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("engine Dockerfile not found at %s: %v", p, err)
	}
	return p
}

// runBlocks collapses backslash-continued lines so a multi-line `RUN ... \`
// block reads as one logical line, then returns only the RUN instructions.
func runBlocks(t *testing.T, dockerfilePath string) []string {
	t.Helper()
	raw, err := os.ReadFile(dockerfilePath)
	if err != nil {
		t.Fatalf("read %s: %v", dockerfilePath, err)
	}
	joined := regexp.MustCompile(`\\\r?\n\s*`).ReplaceAllString(string(raw), " ")
	var runs []string
	for _, line := range strings.Split(joined, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "RUN ") {
			runs = append(runs, trimmed)
		}
	}
	return runs
}

const (
	goBuildCacheMount = "--mount=type=cache,target=/root/.cache/go-build"
	goModCacheMount   = "--mount=type=cache,target=/go/pkg/mod"
)

// invokesGo reports whether a RUN block compiles/fetches Go (and therefore
// benefits from the build + module caches).
func invokesGo(run string) bool {
	return strings.Contains(run, "go build") ||
		strings.Contains(run, "go mod download") ||
		strings.Contains(run, "go run github.com/a-h/templ")
}

func TestEngineDockerfileHasGoCacheMounts(t *testing.T) {
	df := repoRootDockerfile(t)
	runs := runBlocks(t, df)

	var goRuns int
	for _, run := range runs {
		if !invokesGo(run) {
			continue
		}
		goRuns++
		if !strings.Contains(run, goBuildCacheMount) {
			t.Errorf("Go RUN step missing build-cache mount %q:\n  %s", goBuildCacheMount, run)
		}
		if !strings.Contains(run, goModCacheMount) {
			t.Errorf("Go RUN step missing module-cache mount %q:\n  %s", goModCacheMount, run)
		}
	}
	// go mod download + templ generate + memql build + healthcheck build.
	if goRuns < 4 {
		t.Fatalf("expected >=4 Go RUN steps with cache mounts, found %d", goRuns)
	}
}

func TestEngineDockerfileDeclaresSyntaxDirective(t *testing.T) {
	raw, err := os.ReadFile(repoRootDockerfile(t))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	first := strings.TrimSpace(strings.SplitN(string(raw), "\n", 2)[0])
	if first != "# syntax=docker/dockerfile:1" {
		t.Errorf("Dockerfile must start with the dockerfile:1 syntax directive (enables --mount=type=cache); got %q", first)
	}
}
