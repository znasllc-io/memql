package k3d

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// bringup_media_test.go -- `make up` starts the local calls media server last, and
// MEDIA=0 skips it (scripts/media/local-up.sh is make media-up).
//
// bringup.sh is far too heavy to drive end to end, so, as bringup_front_door_tls_test.go
// does, this sources it, replaces each heavy step with one that only records its name,
// and runs the real main(). The media step itself runs for real, against a fake
// local-up.sh: what is under test is when bringup calls it, what it hands it, and what a
// failure does to the bring-up's own answer.

const bringupFakeMediaUp = `#!/usr/bin/env bash
printf 'media-up %s\n' "$*" >> "$BRINGUP_STEPS"
printf 'INFO:  fake media server log line\n' >&2
if [ -n "$FAKE_MEDIA_UP_FAILS" ]; then
  printf '{"ok":false,"capability":"media.localUp","changed":false,"result":{},"error":{"code":4,"message":"Docker is required but not found"}}\n'
  exit 4
fi
printf '{"ok":true,"capability":"media.localUp","changed":true,"result":{"publicUrl":"wss://media.memql.localhost:7443"},"error":null}\n'
`

type bringupRun struct {
	steps  []string
	env    envelopeOf
	code   int
	stdout string
}

type envelopeOf struct {
	OK     bool           `json:"ok"`
	Result map[string]any `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// mediaOpts is how the fake media-up behaves: it fails, or it is not there at all (a tree
// without scripts/media, as the editor's installer stages bringup.sh).
type mediaOpts struct {
	fails, absent bool
}

func runBringupMain(t *testing.T, opts mediaOpts, args ...string) bringupRun {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	root := repoRoot(t)
	tmp := t.TempDir()
	steps := filepath.Join(tmp, "steps.log")
	fake := filepath.Join(tmp, "local-up.sh")
	if !opts.absent {
		if err := os.WriteFile(fake, []byte(bringupFakeMediaUp), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var stub strings.Builder
	stub.WriteString("#!/usr/bin/env bash\nset -euo pipefail\n")
	stub.WriteString("source \"" + filepath.Join(root, "scripts", "k3d", "bringup.sh") + "\"\n")
	for _, step := range []string{
		"refuse_clean_slate_below_minimum", "nuke", "bringup",
		"wait_for_deployments_created", "rebuild_images", "run_migration",
	} {
		stub.WriteString("function " + step + "() { printf '%s\\n' " + step + " >> \"$BRINGUP_STEPS\"; }\n")
	}
	stub.WriteString("function wait_for_healthy() { printf 'wait_for_healthy\\n' >> \"$BRINGUP_STEPS\"; " +
		"HEALTHY=true; FRONT_DOOR_TLS=true; FRONT_DOOR_TLS_DETAIL=checked; }\n")
	stub.WriteString("MEDIA_UP_SCRIPT=\"" + fake + "\"\n")
	stub.WriteString("main \"$@\"\n")
	harness := filepath.Join(tmp, "harness.sh")
	if err := os.WriteFile(harness, []byte(stub.String()), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", append([]string{harness}, args...)...)
	cmd.Dir = root
	cmd.Stdin = nil
	failFlag := ""
	if opts.fails {
		failFlag = "1"
	}
	cmd.Env = append(os.Environ(), "BRINGUP_STEPS="+steps, "FAKE_MEDIA_UP_FAILS="+failFlag,
		"CLUSTER=", "NAMESPACE=", "MEDIA=")
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	run := bringupRun{stdout: stdout.String()}
	if ee, ok := err.(*exec.ExitError); ok {
		run.code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("harness: %v\n%s", err, stderr.String())
	}
	t.Logf("exit=%d\nstdout: %s\nstderr:\n%s", run.code, stdout.String(), stderr.String())
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &run.env); err != nil {
		t.Fatalf("stdout is not exactly one envelope (%v): %q", err, stdout.String())
	}
	if b, err := os.ReadFile(steps); err == nil {
		run.steps = strings.Fields(strings.ReplaceAll(string(b), "\n", " | "))
	}
	return run
}

func stepNames(steps []string) string { return strings.Join(steps, " ") }

func TestBringupStartsTheMediaServerLast(t *testing.T) {
	run := runBringupMain(t, mediaOpts{})
	if run.code != 0 || !run.env.OK {
		t.Fatalf("bring-up failed: exit %d, %+v", run.code, run.env.Error)
	}
	got := stepNames(run.steps)
	if !strings.HasSuffix(got, "wait_for_healthy | media-up --cluster=memql --namespace=memql |") {
		t.Errorf("the media server did not start last, after the mesh was healthy, with the cluster and namespace: %s", got)
	}
	if run.env.Result["mediaServer"] != "started" {
		t.Errorf("mediaServer = %v, want started", run.env.Result["mediaServer"])
	}
	if strings.Count(run.stdout, `"capability"`) != 1 {
		t.Errorf("the media server's own envelope leaked onto bring-up's stdout: %s", run.stdout)
	}
}

func TestBringupWithMediaZeroSkipsTheMediaServer(t *testing.T) {
	run := runBringupMain(t, mediaOpts{}, "--media=0")
	if run.code != 0 || !run.env.OK {
		t.Fatalf("bring-up failed: exit %d, %+v", run.code, run.env.Error)
	}
	if strings.Contains(stepNames(run.steps), "media-up") {
		t.Errorf("MEDIA=0 still started the media server: %s", stepNames(run.steps))
	}
	if run.env.Result["mediaServer"] != "skipped" {
		t.Errorf("mediaServer = %v, want skipped", run.env.Result["mediaServer"])
	}
}

func TestBringupPassesTheDomainToTheMediaServer(t *testing.T) {
	run := runBringupMain(t, mediaOpts{}, "--domain=lab.example.test")
	if !strings.Contains(stepNames(run.steps), "media-up --cluster=memql --namespace=memql --domain=lab.example.test") {
		t.Errorf("the media server was not given the bring-up's domain: %s", stepNames(run.steps))
	}
}

// A media server that does not start fails the bring-up, AFTER every other step has run:
// the cluster is up, and the answer says so, which step did not finish, and the two ways
// on (start it again once fixed, or bring up without it).
func TestBringupFailsWhenTheMediaServerDoesNotStart(t *testing.T) {
	run := runBringupMain(t, mediaOpts{fails: true})
	if run.code != 5 || run.env.OK || run.env.Error == nil || run.env.Error.Code != 5 {
		t.Fatalf("a media server that did not start exited %d with %+v, want a code-5 failure", run.code, run.env.Error)
	}
	for _, want := range []string{"make media-up", "MEDIA=0", "Docker is required"} {
		if !strings.Contains(run.env.Error.Message, want) {
			t.Errorf("the failure does not say %q: %s", want, run.env.Error.Message)
		}
	}
	if !strings.Contains(stepNames(run.steps), "rebuild_images | run_migration | wait_for_healthy | media-up") {
		t.Errorf("the bring-up did not finish its other steps first: %s", stepNames(run.steps))
	}
	if run.env.Result["mediaServer"] != "failed" || run.env.Result["healthy"] != true {
		t.Errorf("mediaServer=%v healthy=%v, want failed and true", run.env.Result["mediaServer"], run.env.Result["healthy"])
	}
}

// The editor's installer stages bringup.sh without scripts/media, so there the step is
// skipped, said so, and nothing else changes.
func TestBringupWithoutTheMediaScriptSkipsTheStep(t *testing.T) {
	run := runBringupMain(t, mediaOpts{absent: true})
	if run.code != 0 || !run.env.OK {
		t.Fatalf("a tree without scripts/media failed the bring-up: exit %d, %+v", run.code, run.env.Error)
	}
	if run.env.Result["mediaServer"] != "unavailable" {
		t.Errorf("mediaServer = %v, want unavailable", run.env.Result["mediaServer"])
	}
}
