package k3d

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// dev_prewarm_frontend_test.go -- znasllc-io/memql#3873.
//
// WHAT HAPPENED. `make dev` failed partway through a multi-image build:
//
//	#2 resolve image config for docker-image://docker.io/docker/dockerfile:1
//	#2 ERROR: failed to do request:
//	   Head "https://registry-1.docker.io/v2/docker/dockerfile/manifests/1":
//	   net/http: timeout awaiting response headers
//
// Seven of eight engine images had already built and imported. Nothing in this
// repository was broken -- `Dockerfile:1` is `# syntax=docker/dockerfile:1`,
// which makes BuildKit resolve an EXTERNAL frontend image from Docker Hub
// before any of this repository's code is compiled.
//
// THE FACT THE FIX TURNS ON, measured rather than assumed. The issue explicitly
// declined to ship this fix because it could not verify that BuildKit's docker
// driver consults the local image store for a `# syntax=` reference -- and an
// unverified robustness fix is worse than an open issue. It does:
//
//	$ docker tag docker/dockerfile:1 unreachable-registry.invalid/probe:v0
//	$ # syntax=unreachable-registry.invalid/probe:v0
//	$ docker build ...
//	=> succeeded in 0.228s
//
// `.invalid` is a reserved TLD that cannot resolve, so a build that succeeds
// against it made no round trip. Pre-pulling the frontend is therefore
// SUFFICIENT to prevent the failure, not merely hopeful.
//
// WHAT THESE TESTS PIN, since the measurement above lives in a commit message
// and cannot re-run itself:
//
//   - the pulled reference is READ FROM the Dockerfile, so a second copy of it
//     cannot drift and leave the pre-warm pulling an image the build never uses
//   - a frontend already in the store is NOT re-pulled, because `make dev` is
//     the inner loop and a network call on every invocation is a cost the cold
//     case does not justify
//   - a FAILED pull does not abort the run, and says so naming Docker Hub --
//     the complaint was a failure that named Docker Hub after seven images had
//     built, so the warning has to arrive before the first one

// prewarmFakeDocker stands in for `docker`. FAKE_HAVE_IMAGE decides whether the
// frontend is already in the store; FAKE_PULL_FAILS makes the pull fail. Every
// invocation is appended to $FAKE_CALLS so the test can assert on what actually
// ran rather than on the function's own log lines.
const prewarmFakeDocker = `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$FAKE_CALLS"
case "$1" in
  image)
    [ -n "${FAKE_HAVE_IMAGE:-}" ] && exit 0
    exit 1 ;;
  pull)
    [ -n "${FAKE_PULL_FAILS:-}" ] && exit 1
    exit 0 ;;
esac
exit 0
`

// runPrewarm sources dev.sh and calls prewarm_build_frontend against a fake
// docker, with REPO_ROOT pointed at a synthetic tree carrying `syntaxLine` as
// its Dockerfile and a passthrough retry.sh. Returns the calls the fake docker
// saw, the combined output, and the exit code.
func runPrewarm(t *testing.T, syntaxLine string, haveImage, pullFails bool) (calls []string, out string, code int) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	root := repoRoot(t)
	tmp := t.TempDir()

	if err := os.WriteFile(filepath.Join(tmp, "docker"), []byte(prewarmFakeDocker), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}
	// A synthetic REPO_ROOT: the Dockerfile the ref is read from, plus a
	// retry.sh that simply runs the command. The real retry.sh is covered by
	// its own tests; what matters here is that the pull goes THROUGH it.
	fakeRoot := filepath.Join(tmp, "root")
	if err := os.MkdirAll(filepath.Join(fakeRoot, "scripts", "ci"), 0o755); err != nil {
		t.Fatalf("mkdir fake root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fakeRoot, "Dockerfile"),
		[]byte(syntaxLine+"\nFROM scratch\n"), 0o644); err != nil {
		t.Fatalf("write fake Dockerfile: %v", err)
	}
	retryStub := "#!/usr/bin/env bash\n" +
		"printf 'RETRY-USED\\n' >&2\n" +
		"while [[ \"$1\" != \"--\" && $# -gt 0 ]]; do shift; done\n" +
		"shift\n" +
		"\"$@\"\n"
	if err := os.WriteFile(filepath.Join(fakeRoot, "scripts", "ci", "retry.sh"),
		[]byte(retryStub), 0o755); err != nil {
		t.Fatalf("write retry stub: %v", err)
	}

	callsFile := filepath.Join(tmp, "calls")
	harness := filepath.Join(tmp, "harness.sh")
	body := "#!/usr/bin/env bash\n" +
		"set -uo pipefail\n" +
		"source \"" + filepath.Join(root, "scripts", "k3d", "dev.sh") + "\"\n" +
		"REPO_ROOT=" + fakeRoot + "\n" +
		"prewarm_build_frontend\n" +
		"echo \"PREWARM-EXIT=$?\"\n"
	if err := os.WriteFile(harness, []byte(body), 0o755); err != nil {
		t.Fatalf("write harness: %v", err)
	}

	cmd := exec.Command("bash", harness)
	cmd.Dir = root
	env := append(os.Environ(),
		"PATH="+tmp+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_CALLS="+callsFile,
	)
	if haveImage {
		env = append(env, "FAKE_HAVE_IMAGE=1")
	}
	if pullFails {
		env = append(env, "FAKE_PULL_FAILS=1")
	}
	cmd.Env = env

	b, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("harness failed: %v\n%s", err, b)
	}
	out = string(b)

	if raw, readErr := os.ReadFile(callsFile); readErr == nil {
		for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
			if line != "" {
				calls = append(calls, line)
			}
		}
	}
	t.Logf("exit=%d calls=%v output:\n%s", code, calls, out)
	return calls, out, code
}

func joined(calls []string) string { return strings.Join(calls, " | ") }

// TestPrewarmReadsTheRefFromTheDockerfile is the drift guard, and the reason
// the ref is not a constant in dev.sh.
//
// A hardcoded second copy of `docker/dockerfile:1` would fail SILENTLY when the
// directive changed: the pre-warm would pull an image the build does not use,
// leaving the cold-store failure exactly as it was while appearing to have run.
func TestPrewarmReadsTheRefFromTheDockerfile(t *testing.T) {
	calls, _, code := runPrewarm(t, "# syntax=example.test/custom-frontend:9.9", false, false)

	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if !strings.Contains(joined(calls), "pull example.test/custom-frontend:9.9") {
		t.Errorf("the pre-warm did not pull the ref the Dockerfile declares.\n"+
			"A hardcoded copy of the frontend reference drifts silently -- it would pull "+
			"an image the build never resolves, and the cold-store failure would survive "+
			"a fix that looks like it ran (memql#3873).\ncalls: %s", joined(calls))
	}
}

// TestPrewarmMatchesTheRealDockerfile keeps the extraction honest against the
// file that actually ships. The test above proves the ref is read from A
// Dockerfile; this proves the reader handles THIS one.
func TestPrewarmMatchesTheRealDockerfile(t *testing.T) {
	root := repoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	first, _, _ := strings.Cut(string(raw), "\n")
	if !strings.HasPrefix(first, "# syntax=") {
		t.Skipf("the root Dockerfile no longer carries a syntax directive (%q) -- "+
			"nothing external to pre-warm, which is the state dropping it would leave", first)
	}
	want := strings.TrimSpace(strings.TrimPrefix(first, "# syntax="))

	calls, _, code := runPrewarm(t, first, false, false)
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if !strings.Contains(joined(calls), "pull "+want) {
		t.Errorf("the pre-warm does not pull %q, the frontend the real Dockerfile "+
			"declares\ncalls: %s", want, joined(calls))
	}
}

// TestPrewarmSkipsThePullWhenTheFrontendIsAlreadyLocal.
//
// `make dev` is the inner loop, run many times an hour. The measurement in this
// file's header is what makes the skip safe: with the image in the store,
// BuildKit makes no round trip, so there is nothing a fresh pull would protect.
func TestPrewarmSkipsThePullWhenTheFrontendIsAlreadyLocal(t *testing.T) {
	calls, out, code := runPrewarm(t, "# syntax=docker/dockerfile:1", true, false)

	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if strings.Contains(joined(calls), "pull") {
		t.Errorf("the pre-warm pulled a frontend that is already in the local image "+
			"store. This runs on every `make dev`; a network call the cold case does "+
			"not need is a tax on the inner loop.\ncalls: %s", joined(calls))
	}
	if !strings.Contains(out, "already in the local image store") {
		t.Errorf("the run does not say it skipped, so an operator watching a slow "+
			"`make dev` cannot tell the pre-warm from a hang\n%s", out)
	}
}

// TestPrewarmPullsThroughRetryNotBareDocker.
//
// The pull is the NETWORK operation, and retry.sh exists for exactly that. The
// alternative considered in the issue -- wrapping the whole `docker build` --
// is the anti-pattern retry.sh's own header warns about: it would retry a Go
// compile failure, burning three full builds to report the third.
func TestPrewarmPullsThroughRetryNotBareDocker(t *testing.T) {
	_, out, code := runPrewarm(t, "# syntax=docker/dockerfile:1", false, false)

	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if !strings.Contains(out, "RETRY-USED") {
		t.Errorf("the pull did not go through retry.sh, so a single slow moment on "+
			"registry-1.docker.io still fails the run -- which is the whole "+
			"complaint (memql#3873)\n%s", out)
	}
}

// TestPrewarmFailureWarnsAndContinues is the arm that decides what a cold,
// UNREACHABLE Docker Hub does to the run.
//
// It must not abort: if the frontend is present but `pull` failed for an
// unrelated reason, the build works, and refusing here would break a working
// setup. What it must do is SAY SO, before the first image builds -- the
// original complaint was a failure that named Docker Hub after seven of eight
// images had already been built and imported.
func TestPrewarmFailureWarnsAndContinues(t *testing.T) {
	_, out, code := runPrewarm(t, "# syntax=docker/dockerfile:1", false, true)

	if code != 0 {
		t.Errorf("exit %d after a failed pre-pull. A pre-warm that ABORTS the run is "+
			"worse than the problem: the build still succeeds when the frontend is "+
			"cached, and this would refuse it.\n%s", code, out)
	}
	if !strings.Contains(out, "Docker Hub") {
		t.Errorf("the warning does not name Docker Hub, so the operator gets the same "+
			"unattributable failure the issue is about -- just earlier\n%s", out)
	}
	if !strings.Contains(out, "memql#3873") {
		t.Errorf("the warning does not name the issue, so a reader has the symptom and "+
			"no route to the explanation\n%s", out)
	}
}

// TestPrewarmIsANoOpWithoutASyntaxDirective.
//
// Dropping `# syntax=` was the other candidate fix (it removes the network
// dependency outright, at the cost of pinning the Dockerfile's feature set to
// the oldest builder in use). If somebody takes that route later, this must
// quietly do nothing rather than fail on an empty ref.
func TestPrewarmIsANoOpWithoutASyntaxDirective(t *testing.T) {
	calls, _, code := runPrewarm(t, "# not a syntax directive", false, false)

	if code != 0 {
		t.Errorf("exit %d for a Dockerfile with no syntax directive -- there is no "+
			"external frontend to resolve, so there is nothing to warm and nothing "+
			"to fail\n", code)
	}
	if strings.Contains(joined(calls), "pull") {
		t.Errorf("the pre-warm pulled something for a Dockerfile that declares no "+
			"frontend\ncalls: %s", joined(calls))
	}
}
