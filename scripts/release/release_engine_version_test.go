package release

// release_engine_version_test.go -- release-engine.sh refuses a stale VERSION
// (memql#5714).
//
// The REAL script runs against a fake `gh` on PATH (the shape
// scripts/ci/ruleset_drift_test.go uses), so what is measured is what the
// script does with each answer rather than what a reimplementation of it would
// do. The fake logs every argv it receives; the assertions that matter are
// about that log -- above all, that `gh release create` is never reached when
// VERSION disagrees, because a refusal after the create would be no refusal.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeEngineGH answers every gh call release-engine.sh makes.
//
//	FAKE_RELEASE_STATE   absent (default) | draft | published, before any create
//	FAKE_TAG_EXISTS      non-empty: the tag already exists
//	FAKE_HEAD_SHA        the default branch's head (default headsha)
//	FAKE_VERSION         the raw content of VERSION
//	FAKE_VERSION_ABSENT  non-empty: the root listing does not name VERSION
//
// `release create` leaves a marker so the re-read after it reports the release
// as published, which is what the real API does.
const fakeEngineGH = `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$FAKE_GH_LOG"
case "$1" in
  auth) exit 0 ;;
  release)
    case "$2" in
      view)
        state="${FAKE_RELEASE_STATE:-absent}"
        if [[ -f "$FAKE_STATE_DIR/created" ]]; then state=published; fi
        case "$state" in
          absent) echo "release not found" >&2; exit 1 ;;
          draft) printf '{"isDraft":true,"url":"https://github.test/draft","publishedAt":null}\n'; exit 0 ;;
          published) printf '{"isDraft":false,"url":"https://github.test/releases/%s","publishedAt":"2026-01-01T00:00:00Z"}\n' "$3"; exit 0 ;;
        esac ;;
      create)
        : > "$FAKE_STATE_DIR/created"
        printf 'https://github.test/releases/%s\n' "$3"
        exit 0 ;;
    esac ;;
  run)
    printf '[{"databaseId":7,"url":"https://github.test/run/7","event":"workflow_dispatch","status":"queued","conclusion":null,"createdAt":"2026-01-01T00:00:05Z"}]\n'
    exit 0 ;;
  api)
    shift
    path=""
    while [[ $# -gt 0 ]]; do
      case "$1" in
        -H|--jq) shift 2 ;;
        *) path="$1"; shift ;;
      esac
    done
    case "$path" in
      */git/matching-refs/tags/*)
        if [[ -n "${FAKE_TAG_EXISTS:-}" ]]; then printf 'refs/tags/%s\n' "${path##*/}"; fi
        exit 0 ;;
      */commits/*) printf '%s\n' "${FAKE_HEAD_SHA:-headsha}"; exit 0 ;;
      */contents/VERSION\?ref=*) printf '%s' "$FAKE_VERSION"; exit 0 ;;
      */contents\?ref=*)
        if [[ -n "${FAKE_VERSION_ABSENT:-}" ]]; then printf 'README.md\ngo.mod\n'; else printf 'README.md\nVERSION\ngo.mod\n'; fi
        exit 0 ;;
      repos/*) printf 'main\n'; exit 0 ;;
    esac ;;
esac
echo "fake gh: unhandled call: $*" >&2
exit 1
`

// engineRun is one execution of the real script.
type engineRun struct {
	code   int
	stdout string
	stderr string
	ghLog  []string
}

// envelope decodes the one JSON line on stdout.
func (r engineRun) envelope(t *testing.T) map[string]any {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(r.stdout)), &env); err != nil {
		t.Fatalf("stdout is not one JSON envelope: %v\nstdout:\n%s\nstderr:\n%s", err, r.stdout, r.stderr)
	}
	return env
}

// calls returns the logged gh invocations containing substr.
func (r engineRun) calls(substr string) []string {
	var out []string
	for _, line := range r.ghLog {
		if strings.Contains(line, substr) {
			out = append(out, line)
		}
	}
	return out
}

func runReleaseEngine(t *testing.T, fakeEnv map[string]string, args ...string) engineRun {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available; release-engine.sh requires it")
	}
	script := filepath.Join(repoRoot(t), "scripts", "release", "release-engine.sh")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(fakeEngineGH), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	logPath := filepath.Join(dir, "gh.log")

	cmd := exec.Command("bash", append([]string{script}, args...)...)
	env := []string{
		"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + dir,
		"FAKE_GH_LOG=" + logPath,
		"FAKE_STATE_DIR=" + dir,
	}
	for k, v := range fakeEnv {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running release-engine.sh: %v", err)
	}
	logged, _ := os.ReadFile(logPath)
	r := engineRun{code: code, stdout: stdout.String(), stderr: stderr.String()}
	for _, line := range strings.Split(strings.TrimSpace(string(logged)), "\n") {
		if line != "" {
			r.ghLog = append(r.ghLog, line)
		}
	}
	t.Logf("exit=%d\nstdout:\n%s\nstderr:\n%s\ngh calls:\n  %s", code, r.stdout, r.stderr, strings.Join(r.ghLog, "\n  "))
	return r
}

// assertRefused checks the refusal contract: exit 3, ok=false, and no release
// created.
func assertRefused(t *testing.T, r engineRun, mention string) {
	t.Helper()
	if r.code != 3 {
		t.Fatalf("exit = %d, want 3 (refused)", r.code)
	}
	env := r.envelope(t)
	if env["ok"] != false {
		t.Fatalf("envelope ok = %v, want false", env["ok"])
	}
	errBlock, _ := env["error"].(map[string]any)
	if msg, _ := errBlock["message"].(string); !strings.Contains(msg, mention) {
		t.Errorf("the refusal does not mention %q: %v", mention, errBlock)
	}
	if created := r.calls("release create"); len(created) != 0 {
		t.Fatalf("a refused run still created a release: %v", created)
	}
}

// TestReleaseEngineRefusesAStaleVersion is the lag itself: VERSION still names
// the previous release. Refused on a real run AND on a dry run, because a dry
// run that passed and a real run that then refused would make the dry run a
// promise it cannot keep.
func TestReleaseEngineRefusesAStaleVersion(t *testing.T) {
	for _, dry := range []string{"false", "true"} {
		t.Run("dryRun="+dry, func(t *testing.T) {
			r := runReleaseEngine(t, map[string]string{"FAKE_VERSION": "1.2.2\n"},
				"--version=v1.2.3", "--repo=acme/widget", "--pollSeconds=0", "--dryRun="+dry)
			assertRefused(t, r, "reads '1.2.2'")
			result, _ := r.envelope(t)["result"].(map[string]any)
			if result["versionFile"] != "1.2.2" {
				t.Errorf("result.versionFile = %v, want 1.2.2 -- the refusal must report what it read", result["versionFile"])
			}
		})
	}
}

// TestReleaseEngineRefusesAMissingVersion covers the file being absent at the
// commit, which the root listing tells apart from a failed call.
func TestReleaseEngineRefusesAMissingVersion(t *testing.T) {
	r := runReleaseEngine(t, map[string]string{"FAKE_VERSION_ABSENT": "1"},
		"--version=1.2.3", "--repo=acme/widget", "--pollSeconds=0")
	assertRefused(t, r, "no VERSION file")
	if reads := r.calls("contents/VERSION"); len(reads) != 0 {
		t.Errorf("the file was fetched although the listing said it is absent: %v", reads)
	}
}

// TestReleaseEngineChecksVersionAtTheCommitItTags pins the --target half. With
// no tag and no targetSha, gh release create would tag whatever the default
// branch points at WHEN IT RUNS; the script reads the head once, checks VERSION
// there, and tags exactly that sha.
func TestReleaseEngineChecksVersionAtTheCommitItTags(t *testing.T) {
	r := runReleaseEngine(t, map[string]string{"FAKE_VERSION": "1.2.3\n", "FAKE_HEAD_SHA": "headsha9"},
		"--version=1.2.3", "--repo=acme/widget", "--pollSeconds=0")
	if r.code != 0 {
		t.Fatalf("a matching VERSION was refused: exit %d", r.code)
	}
	if reads := r.calls("contents/VERSION?ref=headsha9"); len(reads) != 1 {
		t.Fatalf("VERSION was not read at the default branch's head: %v", r.ghLog)
	}
	created := r.calls("release create v1.2.3")
	if len(created) != 1 || !strings.Contains(created[0], "--target headsha9") {
		t.Fatalf("the release was not created at the sha VERSION was read at: %v", created)
	}
}

// TestReleaseEngineChecksAGivenTargetSha: an explicit targetSha is both the
// commit checked and the commit tagged.
func TestReleaseEngineChecksAGivenTargetSha(t *testing.T) {
	r := runReleaseEngine(t, map[string]string{"FAKE_VERSION": "1.2.3\n"},
		"--version=1.2.3", "--repo=acme/widget", "--pollSeconds=0", "--targetSha=cafe123")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0", r.code)
	}
	if reads := r.calls("contents/VERSION?ref=cafe123"); len(reads) != 1 {
		t.Fatalf("VERSION was not read at the given targetSha: %v", r.ghLog)
	}
	if heads := r.calls("/commits/"); len(heads) != 0 {
		t.Errorf("the default branch was read although a targetSha was given: %v", heads)
	}
	created := r.calls("release create v1.2.3")
	if len(created) != 1 || !strings.Contains(created[0], "--target cafe123") {
		t.Fatalf("the release was not created at the given targetSha: %v", created)
	}
}

// TestReleaseEngineChecksAnExistingTagAtTheTag: when the tag exists, the tag IS
// the commit, and no --target is passed that could disagree with it.
func TestReleaseEngineChecksAnExistingTagAtTheTag(t *testing.T) {
	r := runReleaseEngine(t, map[string]string{"FAKE_VERSION": "1.2.3\n", "FAKE_TAG_EXISTS": "1"},
		"--version=1.2.3", "--repo=acme/widget", "--pollSeconds=0")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0", r.code)
	}
	if reads := r.calls("contents/VERSION?ref=v1.2.3"); len(reads) != 1 {
		t.Fatalf("VERSION was not read at the existing tag: %v", r.ghLog)
	}
	created := r.calls("release create v1.2.3")
	if len(created) != 1 || strings.Contains(created[0], "--target") {
		t.Fatalf("an existing tag was released with a --target: %v", created)
	}
}

// TestReleaseEnginePublishedReleaseSkipsTheVersionCheck: a published release is
// history. Re-running only waits for its build, so a VERSION that has moved on
// since must not block that.
func TestReleaseEnginePublishedReleaseSkipsTheVersionCheck(t *testing.T) {
	r := runReleaseEngine(t, map[string]string{"FAKE_VERSION": "9.9.9\n", "FAKE_RELEASE_STATE": "published"},
		"--version=1.2.3", "--repo=acme/widget", "--pollSeconds=0")
	if r.code != 0 {
		t.Fatalf("a published release was refused over VERSION: exit %d", r.code)
	}
	if reads := r.calls("/contents"); len(reads) != 0 {
		t.Errorf("VERSION was read for a release that already exists: %v", reads)
	}
	if created := r.calls("release create"); len(created) != 0 {
		t.Errorf("an existing release was recreated: %v", created)
	}
}
