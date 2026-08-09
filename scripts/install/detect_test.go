// Behaviour gate for install.detect (znasllc-io/memql#3359).
//
// detect is the dependency inventory the rest of the installer plans against:
// what OS/arch are we on, which tools already exist, is the docker daemon
// actually up, are the ingress ports free, is there room on disk. It is the one
// capability in the graph that is READ-ONLY -- every later step decides what to
// do from its answer, so if detect ever mutated the machine the plan would be
// describing a system it had already changed.
//
// These tests are hermetic: `uname`, `df` and every probed tool are stubs on a
// PATH the test owns, so the result does not depend on what the runner happens
// to have installed, and nothing outside t.TempDir() is touched.
package install

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// detectTools is the tool graph the installer depends on. Every one of them
// must carry a `present` flag in the inventory -- a missing key is an unknown,
// and an unknown is not an inventory.
var detectTools = []string{"docker", "k3d", "kubectl", "git", "mkcert"}

// capEnvelope is the capability result envelope (the contract's result schema).
type capEnvelope struct {
	OK         bool            `json:"ok"`
	Capability string          `json:"capability"`
	Changed    bool            `json:"changed"`
	Result     json.RawMessage `json:"result"`
	Error      *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type toolProbe struct {
	Present *bool  `json:"present"`
	Path    string `json:"path"`
	Version string `json:"version"`
}

type detectResult struct {
	OS           string               `json:"os"`
	Arch         string               `json:"arch"`
	Supported    bool                 `json:"supported"`
	Tools        map[string]toolProbe `json:"tools"`
	DockerDaemon *bool                `json:"dockerDaemon"`
	Ports        map[string]bool      `json:"ports"`
	Disk         struct {
		Path   string `json:"path"`
		FreeMb int64  `json:"freeMb"`
	} `json:"disk"`
}

//=============================================================================
// HARNESS
//=============================================================================

func scriptPath(t *testing.T, name string) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	p := filepath.Join(wd, name)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("script %s not found: %v", name, err)
	}
	return p
}

func writeStub(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/usr/bin/env bash\n"+body), 0o755); err != nil {
		t.Fatalf("write stub %s: %v", name, err)
	}
}

// linkReal symlinks a real system binary into the stub dir. The stub PATH is
// exclusive, so anything the script (or capability.sh) genuinely needs from
// coreutils has to be linked in explicitly -- which also documents the script's
// true external dependency surface.
func linkReal(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, n := range names {
		real, err := exec.LookPath(n)
		if err != nil {
			t.Skipf("%s not available on this runner: %v", n, err)
		}
		if err := os.Symlink(real, filepath.Join(dir, n)); err != nil {
			t.Fatalf("symlink %s: %v", n, err)
		}
	}
}

// stubPATH builds an exclusive PATH directory containing a `uname` reporting
// the given kernel/machine, plus the coreutils detect and capability.sh need.
// No probed tool is present unless the caller adds one.
func stubPATH(t *testing.T, kernel, machine string) string {
	t.Helper()
	dir := t.TempDir()
	writeStub(t, dir, "uname", fmt.Sprintf(`case "${1:-}" in
  -m) echo %q ;;
  *)  echo %q ;;
esac
`, machine, kernel))
	// The exclusive PATH has to carry what the machinery itself needs:
	// `bash` because the stubs' /usr/bin/env shebang resolves it through PATH,
	// `dirname` for the SCRIPT_DIR idiom every capability script opens with,
	// `tr` for capability.sh's flag normalization, and `df` for the disk probe.
	linkReal(t, dir, "bash", "dirname", "tr", "df")
	return dir
}

// runDetect executes detect.sh with an exclusive PATH and a HOME the test owns.
func runDetect(t *testing.T, pathDir, home string, args ...string) (capEnvelope, string, int) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{scriptPath(t, "detect.sh")}, args...)...)
	cmd.Env = []string{"PATH=" + pathDir, "HOME=" + home}
	cmd.Stdin = nil
	out, err := cmd.CombinedOutput()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run detect.sh: %v\noutput:\n%s", err, out)
	}
	line := lastJSONObject(string(out))
	if line == "" {
		t.Fatalf("detect.sh emitted no JSON envelope (exit %d)\noutput:\n%s", code, out)
	}
	var env capEnvelope
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		t.Fatalf("envelope is not valid JSON: %v\nline: %s", err, line)
	}
	return env, string(out), code
}

func decodeDetect(t *testing.T, env capEnvelope) detectResult {
	t.Helper()
	var r detectResult
	if err := json.Unmarshal(env.Result, &r); err != nil {
		t.Fatalf("result is not a detect inventory: %v\nresult: %s", err, env.Result)
	}
	return r
}

func lastJSONObject(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "{") && strings.HasSuffix(l, "}") {
			return l
		}
	}
	return ""
}

// freePort returns a port with nothing listening on it.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return p
}

//=============================================================================
// TESTS
//=============================================================================

// TestDetectNeverChanges is THE assertion of #3359: detection must not mutate.
// changed=false on every path, and the HOME the test hands it stays untouched.
func TestDetectNeverChanges(t *testing.T) {
	home := t.TempDir()
	dir := stubPATH(t, "Linux", "x86_64")
	writeStub(t, dir, "docker", `case "$*" in --version) echo "Docker version 28.1.0, build abc";; *) exit 0;; esac`)

	for _, args := range [][]string{
		nil,
		{"--ports=" + fmt.Sprint(freePort(t))},
		{"--path=" + home},
	} {
		env, out, code := runDetect(t, dir, home, args...)
		if code != 0 {
			t.Fatalf("detect exited %d for args %v\noutput:\n%s", code, args, out)
		}
		if env.Changed {
			t.Errorf("changed=true for args %v -- detection must never mutate", args)
		}
	}

	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatalf("readdir home: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("detect created %d entries under HOME -- it must be read-only: %v", len(entries), entries)
	}
}

// TestDetectReportsEveryTool asserts a present flag for EVERY tool in the graph,
// and that an entirely bare machine still succeeds (missing tools are data, not
// an error -- deciding what to do about them is a later capability's job).
func TestDetectReportsEveryTool(t *testing.T) {
	home := t.TempDir()
	dir := stubPATH(t, "Linux", "x86_64")

	env, out, code := runDetect(t, dir, home)
	if code != 0 {
		t.Fatalf("detect on a bare machine exited %d, want 0\noutput:\n%s", code, out)
	}
	if !env.OK {
		t.Fatalf("envelope ok=false on a bare machine: %s", out)
	}
	r := decodeDetect(t, env)

	for _, tool := range detectTools {
		probe, ok := r.Tools[tool]
		if !ok {
			t.Errorf("tools.%s is missing -- every tool in the graph needs a present flag", tool)
			continue
		}
		if probe.Present == nil {
			t.Errorf("tools.%s has no `present` flag", tool)
			continue
		}
		if *probe.Present {
			t.Errorf("tools.%s reported present on an exclusive PATH with no tools on it", tool)
		}
	}
}

// TestDetectFindsInstalledTools drives the positive branch: a tool on PATH is
// present, its resolved path is reported, and its version is extracted.
func TestDetectFindsInstalledTools(t *testing.T) {
	home := t.TempDir()
	dir := stubPATH(t, "Linux", "x86_64")
	writeStub(t, dir, "k3d", `echo "k3d version v5.9.0"`)
	writeStub(t, dir, "git", `echo "git version 2.43.0"`)

	env, out, code := runDetect(t, dir, home)
	if code != 0 {
		t.Fatalf("detect exited %d\noutput:\n%s", code, out)
	}
	r := decodeDetect(t, env)

	for tool, wantVersion := range map[string]string{"k3d": "5.9.0", "git": "2.43.0"} {
		probe := r.Tools[tool]
		if probe.Present == nil || !*probe.Present {
			t.Errorf("tools.%s present=false but a stub is on PATH", tool)
			continue
		}
		if !strings.HasPrefix(probe.Path, dir) {
			t.Errorf("tools.%s path = %q, want a path under the stub dir %q", tool, probe.Path, dir)
		}
		if probe.Version != wantVersion {
			t.Errorf("tools.%s version = %q, want %q", tool, probe.Version, wantVersion)
		}
	}
	if probe := r.Tools["kubectl"]; probe.Present == nil || *probe.Present {
		t.Errorf("kubectl reported present but no stub was installed")
	}
}

// TestDockerDaemonIsSeparateFromDockerPresent locks the distinction the issue
// calls out: the docker BINARY existing and the docker DAEMON answering are two
// different facts. Collapsing them is how an installer decides everything is
// fine and then fails on the first `docker run`.
func TestDockerDaemonIsSeparateFromDockerPresent(t *testing.T) {
	home := t.TempDir()

	t.Run("binary present, daemon down", func(t *testing.T) {
		dir := stubPATH(t, "Linux", "x86_64")
		writeStub(t, dir, "docker", `case "${1:-}" in
  --version) echo "Docker version 28.1.0, build abc" ;;
  *) echo "Cannot connect to the Docker daemon" >&2; exit 1 ;;
esac`)
		r := decodeDetect(t, mustDetect(t, dir, home))
		if p := r.Tools["docker"].Present; p == nil || !*p {
			t.Fatalf("docker present=%v, want true (the stub binary is on PATH)", p)
		}
		if r.DockerDaemon == nil {
			t.Fatal("dockerDaemon is missing -- it must be a field of its own, not folded into docker.present")
		}
		if *r.DockerDaemon {
			t.Error("dockerDaemon=true although the docker stub refuses to connect")
		}
	})

	t.Run("binary present, daemon up", func(t *testing.T) {
		dir := stubPATH(t, "Linux", "x86_64")
		writeStub(t, dir, "docker", `case "${1:-}" in
  --version) echo "Docker version 28.1.0, build abc" ;;
  *) echo "28.1.0" ;;
esac`)
		r := decodeDetect(t, mustDetect(t, dir, home))
		if r.DockerDaemon == nil || !*r.DockerDaemon {
			t.Errorf("dockerDaemon=%v, want true (the stub answers)", r.DockerDaemon)
		}
	})

	t.Run("binary absent, daemon false", func(t *testing.T) {
		dir := stubPATH(t, "Linux", "x86_64")
		r := decodeDetect(t, mustDetect(t, dir, home))
		if p := r.Tools["docker"].Present; p == nil || *p {
			t.Fatalf("docker present=%v, want false", p)
		}
		if r.DockerDaemon == nil || *r.DockerDaemon {
			t.Errorf("dockerDaemon=%v, want false when docker is not installed", r.DockerDaemon)
		}
	})
}

// TestPortsAreTrueWhenFree pins the polarity. "80: true" has to mean the port is
// AVAILABLE; the opposite reading turns a clean machine into a blocked one.
func TestPortsAreTrueWhenFree(t *testing.T) {
	home := t.TempDir()
	dir := stubPATH(t, "Linux", "x86_64")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	busy := ln.Addr().(*net.TCPAddr).Port
	free := freePort(t)

	r := decodeDetect(t, mustDetect(t, dir, home, fmt.Sprintf("--ports=%d,%d", busy, free)))

	got, ok := r.Ports[fmt.Sprint(busy)]
	if !ok {
		t.Fatalf("port %d missing from the report: %+v", busy, r.Ports)
	}
	if got {
		t.Errorf("port %d reported free while a listener is bound to it", busy)
	}
	got, ok = r.Ports[fmt.Sprint(free)]
	if !ok {
		t.Fatalf("port %d missing from the report: %+v", free, r.Ports)
	}
	if !got {
		t.Errorf("port %d reported busy while nothing is listening -- ports are true-when-FREE", free)
	}
}

// TestDetectDefaultPortsAreTheIngressPair asserts the default probe covers the
// two ports the local cluster's front door binds.
func TestDetectDefaultPortsAreTheIngressPair(t *testing.T) {
	home := t.TempDir()
	dir := stubPATH(t, "Linux", "x86_64")
	r := decodeDetect(t, mustDetect(t, dir, home))
	for _, p := range []string{"80", "443"} {
		if _, ok := r.Ports[p]; !ok {
			t.Errorf("default port report is missing %s: %+v", p, r.Ports)
		}
	}
}

// TestUnsupportedOSIsRefusedNotMissing pins the exit code. An unsupported OS is
// a REFUSAL (3): the request is well-formed and every prerequisite question is
// moot. Reporting 4 would say "install something and retry", which is a lie --
// there is nothing to install that makes this epic support macOS.
func TestUnsupportedOSIsRefusedNotMissing(t *testing.T) {
	home := t.TempDir()
	dir := stubPATH(t, "Darwin", "arm64")

	env, out, code := runDetect(t, dir, home)
	if code != 3 {
		t.Errorf("unsupported OS exited %d, want 3 (refused, not 4/prerequisite-missing)\noutput:\n%s", code, out)
	}
	if env.OK {
		t.Error("envelope ok=true for an unsupported OS")
	}
	if env.Error == nil || env.Error.Code != 3 {
		t.Errorf("envelope error.code should be 3; envelope: %s", out)
	}
	if env.Changed {
		t.Error("changed=true on the refusal path -- detection must never mutate")
	}
}

// TestDetectReportsOSArchAndDisk covers the remaining inventory fields.
func TestDetectReportsOSArchAndDisk(t *testing.T) {
	home := t.TempDir()
	dir := stubPATH(t, "Linux", "x86_64")

	r := decodeDetect(t, mustDetect(t, dir, home, "--path="+home))
	if r.OS != "linux" {
		t.Errorf("os = %q, want %q (normalized lowercase)", r.OS, "linux")
	}
	if r.Arch != "amd64" {
		t.Errorf("arch = %q, want %q (normalized to the Go/OCI spelling)", r.Arch, "amd64")
	}
	if !r.Supported {
		t.Error("supported=false on linux/amd64")
	}
	if r.Disk.Path != home {
		t.Errorf("disk.path = %q, want %q", r.Disk.Path, home)
	}
	if r.Disk.FreeMb <= 0 {
		t.Errorf("disk.freeMb = %d, want a positive free-space reading", r.Disk.FreeMb)
	}
}

func mustDetect(t *testing.T, dir, home string, args ...string) capEnvelope {
	t.Helper()
	env, out, code := runDetect(t, dir, home, args...)
	if code != 0 {
		t.Fatalf("detect exited %d, want 0\noutput:\n%s", code, out)
	}
	return env
}
