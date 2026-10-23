package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const containerTestDigest = "example.com/test@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestPipelineContainerMemoryComesFromLocalPolicy(t *testing.T) {
	for _, limit := range []int{0, 4096} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			fx := newPipelineFixture(t)
			allowLocalClones(t)
			policy, _ := pipelineTestPolicy(t, fmt.Sprintf("  container_memory_mib: %d\n", limit))
			argv := filepath.Join(t.TempDir(), "docker-argv")
			fakePipelineDocker(t, `case "$1" in
info) printf 'daemon-id\nlinux/arm64';;
create) printf '%s\n' "$@" > `+shellLiteral(argv)+`;;
image) echo linux/arm64;;
start) cat >/dev/null;;
inspect) printf '%s' '{"Status":"exited","Running":false,"ExitCode":0}';;
rm|ps) exit 0;;
esac`)
			result := decoded(t)(runPipelineStep(context.Background(), "", stepArgs(t, fx, "exit 0", map[string]any{
				"execution": "container", "platform": "linux/arm64", "image": containerTestDigest,
				// A cluster request cannot enlarge the machine's policy.
				"container_memory_mib": 32768,
			}), policy, nil, nil))
			if result.ExitCode != 0 {
				t.Fatalf("container exit %d", result.ExitCode)
			}
			args, err := os.ReadFile(argv)
			if err != nil {
				t.Fatal(err)
			}
			if limit == 0 {
				limit = 2048
			}
			for _, expected := range []string{fmt.Sprintf("--memory=%dm\n", limit), fmt.Sprintf("--memory-swap=%dm\n", limit), "--cpus=2\n", "--cap-drop=ALL\n"} {
				if !strings.Contains(string(args), expected) {
					t.Errorf("missing %q in Docker arguments: %s", expected, args)
				}
			}
		})
	}
}

func TestInvalidPipelineMemoryRefusesBeforeCheckout(t *testing.T) {
	fx := newPipelineFixture(t)
	allowLocalClones(t)
	for _, limit := range []int{-1, 255, 32769} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			policy, root := pipelineTestPolicy(t, fmt.Sprintf("  container_memory_mib: %d\n", limit))
			_, fail := runPipelineStep(context.Background(), "", stepArgs(t, fx, "exit 0", nil), policy, nil, nil)
			if fail == nil || fail.GetErrorCode() != "denied_by_policy" || !strings.Contains(fail.GetErrorMessage(), "container_memory_mib") {
				t.Fatalf("invalid memory was not explained/refused: %v", fail)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatal("checkout started with invalid memory policy")
			}
		})
	}
}

func TestPipelineExecutionRequiresAnHonestContract(t *testing.T) {
	fx := newPipelineFixture(t)
	allowLocalClones(t)
	cases := []map[string]any{
		{"execution": ""}, {"execution": "auto"}, {"platform": ""},
		{"platform": "windows/amd64"}, {"image": containerTestDigest},
		{"execution": "container", "platform": "linux/arm64", "image": "golang:latest"},
		{"execution": "container", "platform": "darwin/arm64", "image": containerTestDigest},
		{"services": []string{"postgres"}}, {"caches": []string{"go"}}, {"services": nil},
		{"needs": "docker"}, {"needs": []any{"unknown"}},
		{"execution": "container", "platform": "linux/arm64", "image": containerTestDigest, "needs": []any{"docker"}},
	}
	for _, extra := range cases {
		if _, err := parsePipelineStep(stepArgs(t, fx, "exit 0", extra)); err == nil {
			t.Errorf("accepted unsupported contract: %v", extra)
		}
	}
	wrong := "linux/amd64"
	if wrong == runtime.GOOS+"/"+runtime.GOARCH {
		wrong = "darwin/arm64"
	}
	req, err := parsePipelineStep(stepArgs(t, fx, "exit 0", map[string]any{"platform": wrong}))
	if err != nil {
		t.Fatal(err)
	}
	if err = checkPipelineRuntime(context.Background(), &req); err == nil {
		t.Fatal("wrong native platform admitted")
	}
}

func TestNativeDockerNeedChecksTheDaemonBeforeCheckout(t *testing.T) {
	fx := newPipelineFixture(t)
	allowLocalClones(t)
	fakePipelineDocker(t, "exit 1")
	policy, root := pipelineTestPolicy(t, "")
	_, fail := runPipelineStep(context.Background(), "", stepArgs(t, fx, "exit 0", map[string]any{"needs": []any{"docker"}}), policy, nil, nil)
	if fail == nil || fail.GetErrorCode() != "pipeline_runtime_unavailable" {
		t.Fatalf("failure: %v", fail)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("checkout started despite an unavailable Docker daemon")
	}
}

func TestContainerScriptKeepsValuesOutOfTheHostShell(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "injected")
	value := "line 1\n' ; touch " + marker + "; #\n$(touch " + marker + ")"
	req := pipelineStepRequest{command: `printf '%s' "$SECRET"`, secrets: map[string]string{"SECRET": value}}
	cmd := exec.Command("/bin/sh")
	cmd.Stdin = strings.NewReader(containerScript(req))
	out, err := cmd.Output()
	if err != nil || string(out) != value {
		t.Fatalf("round trip %q: %v", out, err)
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("secret became shell code")
	}
}

func fakePipelineDocker(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nmain() {\n"+body+"\n}\nmain \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	previous := dockerBinary
	dockerBinary = path
	t.Cleanup(func() { dockerBinary = previous })
	return path
}

func TestPipelineDockerHealthIsCheckedBeforeClone(t *testing.T) {
	fx := newPipelineFixture(t)
	allowLocalClones(t)
	for _, answer := range []string{"exit 1", "printf 'daemon-id\\nlinux/amd64'"} {
		t.Run(answer, func(t *testing.T) {
			fakePipelineDocker(t, answer)
			policy, root := pipelineTestPolicy(t, "")
			_, fail := runPipelineStep(context.Background(), "", stepArgs(t, fx, "exit 0", map[string]any{"execution": "container", "platform": "linux/arm64", "image": containerTestDigest}), policy, nil, nil)
			if fail == nil || fail.GetErrorCode() != "pipeline_runtime_unavailable" {
				t.Fatalf("failure: %v", fail)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatal("created workspace despite unavailable runtime")
			}
		})
	}
}

func TestPipelineDockerClientExitIsNotAReceipt(t *testing.T) {
	for _, scenario := range []struct{ name, state, cleanup, code string }{
		{"still running", `{"Status":"running","Running":true,"ExitCode":0}`, "exit 0", "pipeline_container_uncertain"},
		{"cleanup lost", `{"Status":"exited","Running":false,"ExitCode":0}`, "exit 1", "pipeline_cleanup_uncertain"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fakePipelineDocker(t, `case "$1" in
create) exit 0;;
image) echo linux/arm64;;
start) cat >/dev/null; exit 0;;
inspect) printf '%s' `+shellLiteral(scenario.state)+`;;
rm|ps) `+scenario.cleanup+`;;
esac`)
			reservation := testPipelineReservation(t)
			run := &pipelineRun{reservation: reservation, dir: pipelineTestWorkspace(t), req: pipelineStepRequest{execution: "container", dockerID: "daemon-id", platform: "linux/arm64", image: containerTestDigest, command: "exit 0"}, mask: newSecretMasker(nil), emit: func(bool, []byte) {}}
			_, fail := run.containerCommand(context.Background())
			if fail == nil || fail.GetErrorCode() != scenario.code {
				t.Fatalf("failure: %v", fail)
			}
		})
	}
}

// Opt-in against an EXISTING digest, never an implicit pull of a mutable tag.
// CI can require this by setting MEMQL_TEST_DOCKER_IMAGE. An unavailable daemon
// then fails rather than skipping, just as MEMQL_REQUIRE_DB does in the engine.
func TestPipelineContainerRealDocker(t *testing.T) {
	image := os.Getenv("MEMQL_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("set MEMQL_TEST_DOCKER_IMAGE to an existing pinned local Linux image")
	}
	fx := newPipelineFixture(t)
	allowLocalClones(t)
	policy, root := pipelineTestPolicy(t, "")
	t.Setenv("MEMQL_CONTAINER_HOST_ONLY", "must-not-enter-container")
	t.Setenv(workerTokenVariable, "worker-secret-must-not-enter-container")
	extra := map[string]any{"execution": "container", "platform": "linux/" + runtime.GOARCH, "image": image,
		"secrets": map[string]any{"SECRET": "secret'with\nmultiple-lines", "DOCKER_HOST": "this-must-not-configure-the-host-client"}, "artifacts": []string{"result.txt"}}
	var output recordedOutput
	command := `test "$(uname -s)" = Linux && test ! "${MEMQL_CONTAINER_HOST_ONLY+x}" && test ! "${MEMQL_WORKER_TOKEN+x}" && test "$(cat file.txt)" = second && printf '%s' "$SECRET" && printf '%s' "$SECRET" > result.txt`
	result := decoded(t)(runPipelineStep(context.Background(), "", stepArgs(t, fx, command, extra), policy, output.emit, output.artifact))
	if result.ExitCode != 0 {
		t.Fatalf("container exited %d: %s", result.ExitCode, output.text(true))
	}
	if files := untarGz(t, output.artifactBytes()); files["result.txt"] != "secret'with\nmultiple-lines" {
		t.Fatalf("artifact: %v", files)
	}
	if text := output.text(false); strings.Contains(text, "secret'with") || strings.Contains(text, "multiple-lines") {
		t.Fatal("secret leaked into output")
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("workspace cleanup: %v %v", entries, err)
	}

	// A replacement process reconciles a recorded attempt left behind by a
	// dead worker, without touching another container on the daemon.
	reservation, err := acquirePipelineReservation()
	if err != nil {
		t.Fatal(err)
	}
	interruptedDir, err := os.MkdirTemp(root, "step-")
	if err != nil {
		t.Fatal(err)
	}
	req, err := parsePipelineStep(stepArgs(t, fx, "exit 0", extra))
	if err != nil {
		t.Fatal(err)
	}
	if err = checkPipelineRuntime(context.Background(), &req); err != nil {
		t.Fatal(err)
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	orphan := "memql-step-" + hex.EncodeToString(nonce[:])
	if err = reservation.begin(pipelineAttemptRecord{Execution: "container", DockerID: req.dockerID, Container: orphan, Workspace: interruptedDir}); err != nil {
		t.Fatal(err)
	}
	if _, err = dockerOutput(context.Background(), "create", "--name", orphan, "--network=none", "--entrypoint=/bin/sh", image, "-c", "sleep 300"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = removePipelineContainer(ctx, orphan)
	})
	if _, err = dockerOutput(context.Background(), "start", orphan); err != nil {
		t.Fatal(err)
	}
	reservation.close()
	replacement, err := acquirePipelineReservation()
	if err != nil {
		t.Fatal(err)
	}
	if err = reconcilePipelineReservation(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	if replacement.dirty {
		t.Fatal("orphan retained after reconciliation")
	}
	replacement.close()
	if _, err = os.Stat(interruptedDir); !os.IsNotExist(err) {
		t.Fatal("orphan workspace retained")
	}
	// A deadline must end the CONTAINER, not only its attached Docker client.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var cancelled recordedOutput
	cancelled.onChunk = func(c recordedChunk) {
		if strings.Contains(c.data, "container-ready") {
			cancel()
		}
	}
	began := time.Now()
	_, fail := runPipelineStep(ctx, "", stepArgs(t, fx, "echo container-ready; sleep 300", extra), policy, cancelled.emit, cancelled.artifact)
	if fail == nil || fail.GetErrorCode() != "cancelled" {
		t.Fatalf("cancellation: %v", fail)
	}
	if time.Since(began) > 30*time.Second {
		t.Fatal("container cancellation was not bounded")
	}
	// Only our attempt names, obtained from this run's create output, may be
	// queried; do not claim or remove containers owned by another test/session.
	for _, text := range []string{output.text(false), cancelled.text(false)} {
		for _, line := range strings.Split(text, "\n") {
			if len(line) != 64 {
				continue
			}
			cmd := exec.Command("docker", "ps", "--all", "--filter", "id="+line, "--format", "{{.ID}}")
			out, err := cmd.Output()
			if err != nil || strings.TrimSpace(string(out)) != "" {
				t.Fatalf("container survived: %q %v", out, err)
			}
		}
	}
}

func isolatePipelineCapacity(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	previous := pipelineCapacityRoot
	pipelineCapacityRoot = func() (string, error) { return root, nil }
	t.Cleanup(func() { pipelineCapacityRoot = previous })
}
func testPipelineReservation(t *testing.T) *pipelineReservation {
	t.Helper()
	isolatePipelineCapacity(t)
	reservation, err := acquirePipelineReservation()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reservation.close)
	return reservation
}
