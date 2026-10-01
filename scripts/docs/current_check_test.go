package docs

// current_check_test.go -- the weekly docs-current check (memql#5714).
//
// The REAL script runs against a fake `gh` and a fake `curl` on PATH (the
// shape scripts/ci/ruleset_drift_test.go uses), so what is measured is what the
// script concludes from each answer. The case that matters most is the seeded
// one: a newest release with no docs asset must FAIL, because that is the state
// every release from v0.20.22 to v0.23.5 was in while everything stayed green.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCheckGH answers the two gh calls the script makes.
//
//	FAKE_TAGS        newline-separated tag names `gh api .../tags` returns
//	FAKE_ASSETS      newline-separated asset names of the release
//	FAKE_NO_RELEASE  non-empty: `gh release view` fails, as for a tag with no Release
const fakeCheckGH = `#!/usr/bin/env bash
printf 'gh %s\n' "$*" >> "$FAKE_LOG"
case "$1" in
  api) printf '%s\n' "$FAKE_TAGS"; exit 0 ;;
  release)
    if [[ -n "${FAKE_NO_RELEASE:-}" ]]; then echo "release not found" >&2; exit 1; fi
    if [[ -n "$FAKE_ASSETS" ]]; then printf '%s\n' "$FAKE_ASSETS"; fi
    exit 0 ;;
esac
echo "fake gh: unhandled call: $*" >&2
exit 1
`

// fakeCheckCurl writes FAKE_PAGE to the -o file, or fails like `curl -f` on a
// 404 when FAKE_CURL_FAIL is set.
const fakeCheckCurl = `#!/usr/bin/env bash
printf 'curl %s\n' "$*" >> "$FAKE_LOG"
if [[ -n "${FAKE_CURL_FAIL:-}" ]]; then echo "curl: (22) The requested URL returned error: 404" >&2; exit 22; fi
out=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    *) shift ;;
  esac
done
printf '%s' "$FAKE_PAGE" > "$out"
`

// metaPage renders a docs page carrying the version meta the site layout emits.
func metaPage(version string) string {
	return `<!doctype html><html><head><meta charset="utf-8"><meta name="memql-docs-version" content="` +
		version + `"><title>Docs</title></head><body>docs</body></html>`
}

type checkRun struct {
	code   int
	stdout string
	stderr string
	log    []string
}

func (r checkRun) envelope(t *testing.T) (map[string]any, map[string]any, string) {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(r.stdout)), &env); err != nil {
		t.Fatalf("stdout is not one JSON envelope: %v\nstdout:\n%s\nstderr:\n%s", err, r.stdout, r.stderr)
	}
	result, _ := env["result"].(map[string]any)
	errBlock, _ := env["error"].(map[string]any)
	msg, _ := errBlock["message"].(string)
	return env, result, msg
}

func scriptUnderTest(t *testing.T) string {
	return filepath.Join(thisDir(t), "current-check.sh")
}

// runCheck runs the real script with the fakes on PATH and fakeEnv set.
func runCheck(t *testing.T, fakeEnv map[string]string, args ...string) checkRun {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "gh"), fakeCheckGH, 0o755)
	writeFile(t, filepath.Join(bin, "curl"), fakeCheckCurl, 0o755)
	return execCheck(t, bash, bin+string(os.PathListSeparator)+os.Getenv("PATH"), bin, fakeEnv, args...)
}

func execCheck(t *testing.T, bash, path, bin string, fakeEnv map[string]string, args ...string) checkRun {
	t.Helper()
	logPath := filepath.Join(bin, "calls.log")
	cmd := exec.Command(bash, append([]string{scriptUnderTest(t)}, args...)...)
	env := []string{"PATH=" + path, "HOME=" + bin, "TMPDIR=" + bin, "FAKE_LOG=" + logPath}
	for k, v := range fakeEnv {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	code := 0
	if ee, ok := runErr.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if runErr != nil {
		t.Fatalf("running current-check.sh: %v", runErr)
	}
	r := checkRun{code: code, stdout: stdout.String(), stderr: stderr.String()}
	if logged, err := os.ReadFile(logPath); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(logged)), "\n") {
			if line != "" {
				r.log = append(r.log, line)
			}
		}
	}
	t.Logf("exit=%d\nstdout:\n%s\nstderr:\n%s\ncalls:\n  %s", code, r.stdout, r.stderr, strings.Join(r.log, "\n  "))
	return r
}

// currentFakes is the all-green state for the newest tag v0.23.5.
func currentFakes() map[string]string {
	return map[string]string{
		"FAKE_TAGS":   "v0.23.5\nv0.23.4\nv0.22.12",
		"FAKE_ASSETS": "docs-0.23.5.tgz\nsbom.spdx.json",
		"FAKE_PAGE":   metaPage("0.23.5"),
	}
}

func assertNotCurrent(t *testing.T, r checkRun, mention string) (map[string]any, string) {
	t.Helper()
	if r.code != 5 {
		t.Fatalf("exit = %d, want 5 (not current)", r.code)
	}
	env, result, msg := r.envelope(t)
	if env["ok"] != false {
		t.Fatalf("ok = %v, want false", env["ok"])
	}
	if !strings.Contains(msg, mention) {
		t.Errorf("the failure does not mention %q: %s", mention, msg)
	}
	return result, msg
}

// TestCurrentCheckFailsOnAMissingAsset is the seeded failure: the newest
// release exists and carries no docs bundle -- the live state of every release
// from v0.20.22 to v0.23.5. The site agreeing with the tag must not rescue it.
func TestCurrentCheckFailsOnAMissingAsset(t *testing.T) {
	fakes := currentFakes()
	fakes["FAKE_ASSETS"] = "docs-0.23.4.tgz\nsbom.spdx.json"
	r := runCheck(t, fakes, "--repo=acme/widget")
	result, _ := assertNotCurrent(t, r, "no docs-0.23.5.tgz asset")
	if result["assetPresent"] != false {
		t.Errorf("assetPresent = %v, want false", result["assetPresent"])
	}
	if result["siteVersion"] != "0.23.5" {
		t.Errorf("siteVersion = %v, want 0.23.5 -- the site check must still run and report", result["siteVersion"])
	}
}

// TestCurrentCheckFailsOnAReleaseWithNoAssetsAtAll: an empty asset list is the
// commonest form of the missing bundle.
func TestCurrentCheckFailsOnAReleaseWithNoAssetsAtAll(t *testing.T) {
	fakes := currentFakes()
	fakes["FAKE_ASSETS"] = ""
	r := runCheck(t, fakes, "--repo=acme/widget")
	assertNotCurrent(t, r, "no docs-0.23.5.tgz asset")
}

// TestCurrentCheckFailsWhenTheTagHasNoRelease: a tag with no Release fired no
// workflow, so it has no bundle.
func TestCurrentCheckFailsWhenTheTagHasNoRelease(t *testing.T) {
	fakes := currentFakes()
	fakes["FAKE_NO_RELEASE"] = "1"
	r := runCheck(t, fakes, "--repo=acme/widget")
	assertNotCurrent(t, r, "no readable GitHub Release")
}

// TestCurrentCheckFailsOnADifferentSiteVersion: the bundle exists and the site
// has not picked it up -- the sync half of the pipeline stalled.
func TestCurrentCheckFailsOnADifferentSiteVersion(t *testing.T) {
	fakes := currentFakes()
	fakes["FAKE_PAGE"] = metaPage("0.21.25")
	r := runCheck(t, fakes, "--repo=acme/widget")
	result, _ := assertNotCurrent(t, r, "serves the docs of 0.21.25")
	if result["assetPresent"] != true {
		t.Errorf("assetPresent = %v, want true", result["assetPresent"])
	}
	if result["siteVersion"] != "0.21.25" {
		t.Errorf("siteVersion = %v, want 0.21.25", result["siteVersion"])
	}
}

// TestCurrentCheckFailsWhenTheMetaIsAbsent: a site that does not say which
// release it documents cannot be shown current, so an absent meta fails rather
// than skipping.
func TestCurrentCheckFailsWhenTheMetaIsAbsent(t *testing.T) {
	fakes := currentFakes()
	fakes["FAKE_PAGE"] = `<!doctype html><html><head><title>Docs</title></head><body>docs</body></html>`
	r := runCheck(t, fakes, "--repo=acme/widget")
	assertNotCurrent(t, r, "no memql-docs-version meta")
}

func TestCurrentCheckFailsWhenTheSiteIsUnreachable(t *testing.T) {
	fakes := currentFakes()
	fakes["FAKE_CURL_FAIL"] = "1"
	r := runCheck(t, fakes, "--repo=acme/widget", "--siteUrl=https://docs.example.test/docs/")
	assertNotCurrent(t, r, "could not fetch https://docs.example.test/docs/")
}

// TestCurrentCheckReportsBothFailures: one failure must not hide the other,
// or fixing the first reveals the second a week later.
func TestCurrentCheckReportsBothFailures(t *testing.T) {
	fakes := currentFakes()
	fakes["FAKE_ASSETS"] = ""
	fakes["FAKE_PAGE"] = metaPage("0.21.25")
	r := runCheck(t, fakes, "--repo=acme/widget")
	_, msg := assertNotCurrent(t, r, "no docs-0.23.5.tgz asset")
	if !strings.Contains(msg, "serves the docs of 0.21.25") {
		t.Errorf("the site failure was hidden behind the asset failure: %s", msg)
	}
}

// TestCurrentCheckPassesWhenCurrent is the positive control, across the meta
// spellings a renderer can produce. Without it every failure above is satisfied
// by a script that fails everything.
func TestCurrentCheckPassesWhenCurrent(t *testing.T) {
	for name, page := range map[string]string{
		"name first":        metaPage("0.23.5"),
		"content first":     `<html><head><meta content="0.23.5" name="memql-docs-version" /></head></html>`,
		"single quotes":     `<html><head><meta name='memql-docs-version' content='0.23.5'></head></html>`,
		"one of many metas": `<meta name="description" content="9.9.9"><meta name="memql-docs-version" content="0.23.5"><meta name="x" content="1.1.1">`,
	} {
		t.Run(name, func(t *testing.T) {
			fakes := currentFakes()
			fakes["FAKE_PAGE"] = page
			r := runCheck(t, fakes, "--repo=acme/widget")
			if r.code != 0 {
				t.Fatalf("a current site failed the check: exit %d", r.code)
			}
			env, result, _ := r.envelope(t)
			if env["ok"] != true || result["tag"] != "v0.23.5" || result["assetPresent"] != true || result["siteVersion"] != "0.23.5" {
				t.Fatalf("the envelope does not describe a current site: %s", r.stdout)
			}
		})
	}
}

// TestCurrentCheckPicksTheNumericNewestReleaseTag: v0.9.2 sorts after v0.17.0
// as a string, and the other refs are not engine releases at all. A wrong pick
// would check the docs of a release nobody is on.
func TestCurrentCheckPicksTheNumericNewestReleaseTag(t *testing.T) {
	fakes := map[string]string{
		"FAKE_TAGS": strings.Join([]string{
			"v0.9.2", "v0.17.0", "memql-sdk-core-v0.40.0", "component/bus/gen/v0.99.0",
			"0.14.0", "v0.18.0-rc1", "v0.17.0-backup", "nightly", "v0.16.9",
		}, "\n"),
		"FAKE_ASSETS": "docs-0.17.0.tgz",
		"FAKE_PAGE":   metaPage("0.17.0"),
	}
	r := runCheck(t, fakes, "--repo=acme/widget")
	if r.code != 0 {
		t.Fatalf("exit %d, want 0", r.code)
	}
	_, result, _ := r.envelope(t)
	if result["tag"] != "v0.17.0" {
		t.Fatalf("newest tag = %v, want v0.17.0", result["tag"])
	}
	var viewed bool
	for _, call := range r.log {
		if strings.HasPrefix(call, "gh release view v0.17.0 ") {
			viewed = true
		}
	}
	if !viewed {
		t.Errorf("the assets of v0.17.0 were never read: %v", r.log)
	}
}

// TestCurrentCheckNeedsGhAndCurl: exit 4 names the missing tool. PATH is built
// from the few tools the script needs before that check, so an installed gh or
// curl on the machine running the test cannot mask the absence.
func TestCurrentCheckNeedsGhAndCurl(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	for _, tc := range []struct {
		name    string
		fakes   []string
		missing string
	}{
		{"no gh", []string{"curl"}, "gh is not installed"},
		{"no curl", []string{"gh"}, "curl is not installed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			for _, tool := range []string{"dirname", "tr"} {
				real, err := exec.LookPath(tool)
				if err != nil {
					t.Skipf("%s not available", tool)
				}
				if err := os.Symlink(real, filepath.Join(bin, tool)); err != nil {
					t.Fatal(err)
				}
			}
			for _, fake := range tc.fakes {
				body := fakeCheckGH
				if fake == "curl" {
					body = fakeCheckCurl
				}
				writeFile(t, filepath.Join(bin, fake), body, 0o755)
			}
			r := execCheck(t, bash, bin, bin, currentFakes(), "--repo=acme/widget")
			if r.code != 4 {
				t.Fatalf("exit = %d, want 4 (prerequisite missing)", r.code)
			}
			if _, _, msg := r.envelope(t); !strings.Contains(msg, tc.missing) {
				t.Errorf("the failure does not say %q: %s", tc.missing, msg)
			}
		})
	}
}

// TestCurrentCheckRefusesABadRepo: exit 2 before any network call.
func TestCurrentCheckRefusesABadRepo(t *testing.T) {
	r := runCheck(t, currentFakes(), "--repo=not-a-repo")
	if r.code != 2 {
		t.Fatalf("exit = %d, want 2", r.code)
	}
	if len(r.log) != 0 {
		t.Errorf("a bad parameter still reached the network: %v", r.log)
	}
}
