// Tests for scripts/install/remove-artifact.sh (capability
// install.removeArtifact, znasllc-io/memql#3367).
//
// Uninstall is the half of an installer that can hurt someone. The install
// receipt records, per artifact, whether the installer CREATED it or merely
// found it already there -- and the second case is the dangerous one: a
// developer who had mkcert, or a /etc/hosts entry, or a k3d cluster before
// this installer ever ran does not lose it because they uninstalled memQL.
//
// THE assertion this file exists for: `--pre-existing=true` is an
// UNCONDITIONAL EXIT 3, enforced AT THE POINT OF ACTION. Not in the executor
// that reads the receipt, not once at the top of argument parsing -- inside
// each removal path, immediately before the mutation. That way an executor
// bug, a hand-edited receipt, and a direct shell invocation all hit the same
// wall, and the wall is the last thing between the flag and the damage. The
// test therefore asserts refusal for EVERY kind and, separately, that nothing
// was touched.
//
// The second assertion: removing something already gone is a successful no-op
// with changed=false. An uninstall that fails because the artifact is missing
// makes a partially-completed install impossible to clean up.
//
// Hermetic: every external tool (k3d, docker, mkcert) is a stub on a PATH
// prefix that records its argv, and every filesystem target lives in t.TempDir().
package install

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type raEnvelope struct {
	OK         bool            `json:"ok"`
	Capability string          `json:"capability"`
	Changed    bool            `json:"changed"`
	Result     json.RawMessage `json:"result"`
	Error      *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type raResult struct {
	Kind    string `json:"kind"`
	Target  string `json:"target"`
	Removed bool   `json:"removed"`
	Detail  string `json:"detail"`
	Count   int    `json:"count"`
}

func raScript(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	p := filepath.Join(filepath.Dir(thisFile), "remove-artifact.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("remove-artifact.sh not found at %s: %v", p, err)
	}
	return p
}

func raRun(t *testing.T, extraEnv []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	cmd := exec.Command("bash", append([]string{raScript(t)}, args...)...)
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Stdin = nil
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			t.Fatalf("run: %v", err)
		}
	}
	return out.String(), errb.String(), code
}

func raParse(t *testing.T, stdout string) (raEnvelope, raResult) {
	t.Helper()
	line := strings.TrimSpace(stdout)
	if line == "" {
		t.Fatal("no envelope on stdout")
	}
	if strings.Contains(line, "\n") {
		t.Fatalf("stdout carried more than one line -- human logs belong on stderr:\n%s", line)
	}
	var env raEnvelope
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		t.Fatalf("stdout is not a JSON envelope: %v\n%s", err, line)
	}
	if env.Capability != "install.removeArtifact" {
		t.Errorf("capability = %q, want install.removeArtifact", env.Capability)
	}
	var res raResult
	if err := json.Unmarshal(env.Result, &res); err != nil {
		t.Fatalf("result is not the expected object: %v\n%s", err, env.Result)
	}
	return env, res
}

// -----------------------------------------------------------------------
// A machine made entirely of stubs and temp dirs
// -----------------------------------------------------------------------

type raWorld struct {
	env     []string
	argvDir string
	caroot  string
}

// raNewWorld installs stub k3d / docker / mkcert on a PATH prefix. Each stub
// appends its argv to $STUB_ARGV_DIR/<name>, so a test can prove a tool was
// never reached (the file simply does not exist).
func raNewWorld(t *testing.T, clusters, images string) raWorld {
	t.Helper()
	bin := t.TempDir()
	state := t.TempDir()
	argvDir := filepath.Join(state, "argv")
	if err := os.MkdirAll(argvDir, 0o755); err != nil {
		t.Fatalf("mkdir argv: %v", err)
	}
	clusterFile := filepath.Join(state, "clusters")
	if err := os.WriteFile(clusterFile, []byte(clusters), 0o600); err != nil {
		t.Fatalf("write clusters: %v", err)
	}
	imageFile := filepath.Join(state, "images")
	if err := os.WriteFile(imageFile, []byte(images), 0o600); err != nil {
		t.Fatalf("write images: %v", err)
	}
	caroot := filepath.Join(state, "caroot")
	if err := os.MkdirAll(caroot, 0o755); err != nil {
		t.Fatalf("mkdir caroot: %v", err)
	}

	stubs := map[string]string{
		"k3d": `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$STUB_ARGV_DIR/k3d"
if [[ "$1" == "cluster" && "$2" == "list" ]]; then cat "$STUB_K3D_CLUSTERS"; fi
exit 0
`,
		"docker": `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$STUB_ARGV_DIR/docker"
if [[ "$1" == "images" ]]; then cat "$STUB_DOCKER_IMAGES"; fi
exit 0
`,
		"mkcert": `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$STUB_ARGV_DIR/mkcert"
if [[ "$1" == "-CAROOT" ]]; then printf '%s\n' "$STUB_CAROOT"; fi
exit 0
`,
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write stub %s: %v", name, err)
		}
	}
	return raWorld{
		env: []string{
			"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
			"STUB_ARGV_DIR=" + argvDir,
			"STUB_K3D_CLUSTERS=" + clusterFile,
			"STUB_DOCKER_IMAGES=" + imageFile,
			"STUB_CAROOT=" + caroot,
		},
		argvDir: argvDir,
		caroot:  caroot,
	}
}

// ran reports whether a stubbed tool was invoked at all.
func (w raWorld) ran(t *testing.T, tool string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(w.argvDir, tool))
	return err == nil
}

// argv returns everything a stubbed tool was invoked with.
func (w raWorld) argv(t *testing.T, tool string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(w.argvDir, tool))
	if err != nil {
		t.Fatalf("stub %s was never invoked: %v", tool, err)
	}
	return string(b)
}

// raSeedCA writes a mkcert CA root into the world's CAROOT.
func (w raWorld) seedCA(t *testing.T) {
	t.Helper()
	for _, f := range []string{"rootCA.pem", "rootCA-key.pem"} {
		if err := os.WriteFile(filepath.Join(w.caroot, f), []byte("stub ca\n"), 0o600); err != nil {
			t.Fatalf("seed CA: %v", err)
		}
	}
}

// raHostsFile writes a hosts file carrying a memQL-marked block plus lines
// that must survive untouched.
func raHostsFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hosts")
	body := strings.Join([]string{
		"127.0.0.1\tlocalhost",
		"10.1.2.3\tsome-corporate-thing.internal",
		"# BEGIN memql",
		"127.0.0.1 cockpit.local.znas.io",
		"127.0.0.1 identity.local.znas.io",
		"# END memql",
		"::1\tip6-localhost",
	}, "\n") + "\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write hosts: %v", err)
	}
	return p
}

func raBinaryFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "memql-cockpit")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	return p
}

// raCheckoutDir builds something that looks like install.cloneStack's --dest:
// a directory with a .git inside it.
func raCheckoutDir(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(filepath.Join(p, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir checkout: %v", err)
	}
	if err := os.WriteFile(filepath.Join(p, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	return p
}

const raClusterList = "NAME    SERVERS   AGENTS   LOADBALANCER\nmemql   1/1       0/0      true\n"
const raImageList = "memql-bff:dev\nmemql-agent:dev\npostgres:16\n"

// -----------------------------------------------------------------------
// THE assertion: --pre-existing=true is an unconditional refusal
// -----------------------------------------------------------------------

// raKindArgs returns a fully-formed, would-otherwise-succeed invocation for
// each kind, so the refusal test proves the refusal -- not a missing param.
func raKindArgs(t *testing.T, w raWorld) map[string][]string {
	t.Helper()
	return map[string][]string{
		"binary":       {"--kind=binary", "--path=" + raBinaryFile(t)},
		"checkout":     {"--kind=checkout", "--path=" + raCheckoutDir(t)},
		"hostsEntries": {"--kind=hostsEntries", "--path=" + raHostsFile(t)},
		"mkcertCA":     {"--kind=mkcertCA", "--caroot=" + w.caroot},
		"stack":        {"--kind=stack", "--cluster=memql"},
		"images":       {"--kind=images", "--image-prefix=memql"},
	}
}

// TestRemoveArtifactPreExistingRefusesEveryKind is the core guarantee. It is
// deliberately exhaustive across kinds: the refusal must live at the point of
// action in each removal path, so adding a kind without the guard fails here.
func TestRemoveArtifactPreExistingRefusesEveryKind(t *testing.T) {
	for kind, args := range raKindArgs(t, raNewWorld(t, raClusterList, raImageList)) {
		kind, args := kind, args
		t.Run(kind, func(t *testing.T) {
			w := raNewWorld(t, raClusterList, raImageList)
			w.seedCA(t)
			// Re-point the caroot arg at this subtest's world.
			for i, a := range args {
				if strings.HasPrefix(a, "--caroot=") {
					args[i] = "--caroot=" + w.caroot
				}
			}
			stdout, _, code := raRun(t, w.env, append(append([]string{}, args...), "--pre-existing=true")...)
			if code != 3 {
				t.Fatalf("kind=%s with --pre-existing=true exited %d, want 3 (refused)\nstdout: %s",
					kind, code, stdout)
			}
			env, _ := raParse(t, stdout)
			if env.OK || env.Error == nil || env.Error.Code != 3 {
				t.Errorf("want ok=false error.code=3, got: %s", stdout)
			}
			if env.Changed {
				t.Error("changed=true on a refusal -- the guard fired too late")
			}
			if !strings.Contains(strings.ToLower(env.Error.Message), "pre-existing") {
				t.Errorf("the refusal must say why; got %q", env.Error.Message)
			}
			// Nothing may have been touched.
			for _, tool := range []string{"k3d", "docker", "mkcert"} {
				if w.ran(t, tool) {
					t.Errorf("%s was invoked despite the refusal: %s", tool, w.argv(t, tool))
				}
			}
		})
	}
}

// TestRemoveArtifactPreExistingLeavesFilesAlone: the filesystem kinds must be
// byte-identical after a refusal.
func TestRemoveArtifactPreExistingLeavesFilesAlone(t *testing.T) {
	w := raNewWorld(t, raClusterList, raImageList)

	bin := raBinaryFile(t)
	if _, _, code := raRun(t, w.env, "--kind=binary", "--path="+bin, "--pre-existing=true"); code != 3 {
		t.Fatalf("binary refusal exited %d, want 3", code)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Errorf("a pre-existing binary was deleted anyway: %v", err)
	}

	hosts := raHostsFile(t)
	before, err := os.ReadFile(hosts)
	if err != nil {
		t.Fatalf("read hosts: %v", err)
	}
	if _, _, code := raRun(t, w.env, "--kind=hostsEntries", "--path="+hosts, "--pre-existing=true"); code != 3 {
		t.Fatalf("hostsEntries refusal exited %d, want 3", code)
	}
	after, err := os.ReadFile(hosts)
	if err != nil {
		t.Fatalf("read hosts: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("a pre-existing hosts file was edited anyway:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestRemoveArtifactPreExistingTruthiness: the flag arrives from a JSON
// receipt, a shell, or an executor, in whatever spelling each of those uses.
// Anything that is not plainly false must refuse -- a garbled value is a
// reason to stop, not a reason to delete.
func TestRemoveArtifactPreExistingTruthiness(t *testing.T) {
	refuse := []string{"true", "TRUE", "True", "1", "yes", "maybe", "null"}
	proceed := []string{"false", "FALSE", "0", "no", ""}

	for _, v := range refuse {
		t.Run("refuse/"+v, func(t *testing.T) {
			w := raNewWorld(t, raClusterList, raImageList)
			bin := raBinaryFile(t)
			_, _, code := raRun(t, w.env, "--kind=binary", "--path="+bin, "--pre-existing="+v)
			if code != 3 {
				t.Errorf("--pre-existing=%q exited %d, want 3", v, code)
			}
			if _, err := os.Stat(bin); err != nil {
				t.Errorf("--pre-existing=%q deleted the file", v)
			}
		})
	}
	for _, v := range proceed {
		t.Run("proceed/"+v, func(t *testing.T) {
			w := raNewWorld(t, raClusterList, raImageList)
			bin := raBinaryFile(t)
			args := []string{"--kind=binary", "--path=" + bin}
			if v != "" {
				args = append(args, "--pre-existing="+v)
			}
			_, _, code := raRun(t, w.env, args...)
			if code != 0 {
				t.Errorf("--pre-existing=%q exited %d, want 0", v, code)
			}
			if _, err := os.Stat(bin); err == nil {
				t.Errorf("--pre-existing=%q did not remove the file", v)
			}
		})
	}
}

// TestRemoveArtifactBarePreExistingFlagRefuses: `--pre-existing` with no value
// parses as "1", which must refuse rather than fall through as unset.
func TestRemoveArtifactBarePreExistingFlagRefuses(t *testing.T) {
	w := raNewWorld(t, raClusterList, raImageList)
	bin := raBinaryFile(t)
	_, _, code := raRun(t, w.env, "--kind=binary", "--path="+bin, "--pre-existing")
	if code != 3 {
		t.Fatalf("bare --pre-existing exited %d, want 3", code)
	}
	if _, err := os.Stat(bin); err != nil {
		t.Error("bare --pre-existing deleted the file")
	}
}

// -----------------------------------------------------------------------
// Per-kind removal, and the idempotent no-op
// -----------------------------------------------------------------------

func TestRemoveArtifactBinary(t *testing.T) {
	w := raNewWorld(t, raClusterList, raImageList)
	bin := raBinaryFile(t)

	stdout, stderr, code := raRun(t, w.env, "--kind=binary", "--path="+bin)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	env, res := raParse(t, stdout)
	if !env.Changed {
		t.Error("changed=false after actually deleting the binary")
	}
	if !res.Removed {
		t.Error("result.removed=false after actually deleting the binary")
	}
	if _, err := os.Stat(bin); err == nil {
		t.Error("the binary is still on disk")
	}

	// Second run: already gone is a no-op, not a failure.
	stdout, _, code = raRun(t, w.env, "--kind=binary", "--path="+bin)
	if code != 0 {
		t.Fatalf("removing an absent binary exited %d, want 0 (idempotent)\nstdout: %s", code, stdout)
	}
	env, res = raParse(t, stdout)
	if env.Changed {
		t.Error("changed=true when there was nothing to remove")
	}
	if res.Removed {
		t.Error("result.removed=true when there was nothing to remove")
	}
}

// TestRemoveArtifactCheckout covers the one removal path that recurses. The
// interesting cases are the refusals: a recursive delete driven by a path out
// of a receipt is the shape of an uninstall that eats a home directory, so the
// path has to earn the deletion by being a real checkout.
func TestRemoveArtifactCheckout(t *testing.T) {
	w := raNewWorld(t, raClusterList, raImageList)
	dir := raCheckoutDir(t)

	stdout, stderr, code := raRun(t, w.env, "--kind=checkout", "--path="+dir)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	env, res := raParse(t, stdout)
	if !env.Changed || !res.Removed || res.Kind != "checkout" {
		t.Errorf("want changed+removed for kind=checkout, got: %s", stdout)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("the checkout is still on disk")
	}

	// Already gone is a no-op, like every other kind.
	stdout, _, code = raRun(t, w.env, "--kind=checkout", "--path="+dir)
	if code != 0 {
		t.Fatalf("removing an absent checkout exited %d, want 0\nstdout: %s", code, stdout)
	}
	if env, res = raParse(t, stdout); env.Changed || res.Removed {
		t.Errorf("changed/removed set with nothing to remove: %s", stdout)
	}
}

func TestRemoveArtifactCheckoutRefusesNonCheckouts(t *testing.T) {
	w := raNewWorld(t, raClusterList, raImageList)

	// A directory with no .git is not the thing we cloned.
	plain := filepath.Join(t.TempDir(), "notacheckout")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plain, "important.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, _, code := raRun(t, w.env, "--kind=checkout", "--path="+plain)
	if code != 3 {
		t.Fatalf("exit %d, want 3 (refused: no .git)\nstdout: %s", code, stdout)
	}
	if _, err := os.Stat(filepath.Join(plain, "important.txt")); err != nil {
		t.Error("the refusal deleted something anyway")
	}

	// $HOME itself, reached the long way round, must not resolve into a target.
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	stdout, _, code = raRun(t, append(w.env, "HOME="+home),
		"--kind=checkout", "--path="+filepath.Join(home, "sub", ".."))
	if code != 3 {
		t.Fatalf("exit %d, want 3 (refused: resolves to $HOME)\nstdout: %s", code, stdout)
	}
}

func TestRemoveArtifactHostsEntries(t *testing.T) {
	w := raNewWorld(t, raClusterList, raImageList)
	hosts := raHostsFile(t)

	stdout, stderr, code := raRun(t, w.env, "--kind=hostsEntries", "--path="+hosts)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	env, _ := raParse(t, stdout)
	if !env.Changed {
		t.Error("changed=false after removing the marked block")
	}
	got, err := os.ReadFile(hosts)
	if err != nil {
		t.Fatalf("read hosts: %v", err)
	}
	body := string(got)
	for _, gone := range []string{"cockpit.local.znas.io", "identity.local.znas.io", "BEGIN memql", "END memql"} {
		if strings.Contains(body, gone) {
			t.Errorf("%q survived the removal:\n%s", gone, body)
		}
	}
	// Everything the installer did NOT write must be untouched -- /etc/hosts
	// is a shared file and other people's entries are not ours to delete.
	for _, kept := range []string{"localhost", "some-corporate-thing.internal", "ip6-localhost"} {
		if !strings.Contains(body, kept) {
			t.Errorf("%q was collateral damage:\n%s", kept, body)
		}
	}

	// Idempotent.
	stdout, _, code = raRun(t, w.env, "--kind=hostsEntries", "--path="+hosts)
	if code != 0 {
		t.Fatalf("second run exited %d, want 0\nstdout: %s", code, stdout)
	}
	env, _ = raParse(t, stdout)
	if env.Changed {
		t.Error("changed=true when the block was already gone")
	}
}

func TestRemoveArtifactMkcertCA(t *testing.T) {
	w := raNewWorld(t, raClusterList, raImageList)
	w.seedCA(t)

	stdout, stderr, code := raRun(t, w.env, "--kind=mkcertCA", "--caroot="+w.caroot)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	env, _ := raParse(t, stdout)
	if !env.Changed {
		t.Error("changed=false after uninstalling the CA")
	}
	if !strings.Contains(w.argv(t, "mkcert"), "-uninstall") {
		t.Errorf("mkcert -uninstall was never run; argv: %s", w.argv(t, "mkcert"))
	}
	if _, err := os.Stat(filepath.Join(w.caroot, "rootCA.pem")); err == nil {
		t.Error("rootCA.pem survived")
	}

	// No CA on disk: nothing to do.
	stdout, _, code = raRun(t, w.env, "--kind=mkcertCA", "--caroot="+w.caroot)
	if code != 0 {
		t.Fatalf("second run exited %d, want 0\nstdout: %s", code, stdout)
	}
	env, _ = raParse(t, stdout)
	if env.Changed {
		t.Error("changed=true when there was no CA to remove")
	}
}

func TestRemoveArtifactStack(t *testing.T) {
	w := raNewWorld(t, raClusterList, raImageList)
	stdout, stderr, code := raRun(t, w.env, "--kind=stack", "--cluster=memql")
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	env, _ := raParse(t, stdout)
	if !env.Changed {
		t.Error("changed=false after deleting the cluster")
	}
	if !strings.Contains(w.argv(t, "k3d"), "cluster delete memql") {
		t.Errorf("k3d cluster delete was never run; argv: %s", w.argv(t, "k3d"))
	}

	// A cluster that is not there.
	w2 := raNewWorld(t, "NAME SERVERS AGENTS LOADBALANCER\n", raImageList)
	stdout, _, code = raRun(t, w2.env, "--kind=stack", "--cluster=memql")
	if code != 0 {
		t.Fatalf("absent cluster exited %d, want 0 (idempotent)\nstdout: %s", code, stdout)
	}
	env, _ = raParse(t, stdout)
	if env.Changed {
		t.Error("changed=true when the cluster did not exist")
	}
	if strings.Contains(w2.argv(t, "k3d"), "cluster delete") {
		t.Error("k3d cluster delete was run against a cluster that does not exist")
	}
}

func TestRemoveArtifactImages(t *testing.T) {
	w := raNewWorld(t, raClusterList, raImageList)
	stdout, stderr, code := raRun(t, w.env, "--kind=images", "--image-prefix=memql")
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	env, res := raParse(t, stdout)
	if !env.Changed {
		t.Error("changed=false after removing images")
	}
	if res.Count != 2 {
		t.Errorf("count = %d, want 2 (memql-bff, memql-agent)", res.Count)
	}
	argv := w.argv(t, "docker")
	for _, want := range []string{"memql-bff:dev", "memql-agent:dev"} {
		if !strings.Contains(argv, want) {
			t.Errorf("%s was not removed; argv: %s", want, argv)
		}
	}
	// Someone else's images are not ours to delete.
	if strings.Contains(argv, "postgres:16") {
		t.Errorf("an unrelated image was removed; argv: %s", argv)
	}

	// Nothing matching: no-op.
	w2 := raNewWorld(t, raClusterList, "postgres:16\nredis:7\n")
	stdout, _, code = raRun(t, w2.env, "--kind=images", "--image-prefix=memql")
	if code != 0 {
		t.Fatalf("no matching images exited %d, want 0\nstdout: %s", code, stdout)
	}
	env, _ = raParse(t, stdout)
	if env.Changed {
		t.Error("changed=true when no image matched")
	}
}

// -----------------------------------------------------------------------
// Invocation errors and prerequisites
// -----------------------------------------------------------------------

func TestRemoveArtifactBadParams(t *testing.T) {
	w := raNewWorld(t, raClusterList, raImageList)
	cases := []struct {
		name string
		args []string
	}{
		{"no kind", []string{}},
		{"unknown kind", []string{"--kind=universe"}},
		{"binary without a path", []string{"--kind=binary"}},
		{"hostsEntries without a path", []string{"--kind=hostsEntries", "--path="}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, _, code := raRun(t, w.env, tc.args...)
			if code != 2 {
				t.Fatalf("exit %d, want 2 (bad param)\nstdout: %s", code, stdout)
			}
			env, _ := raParse(t, stdout)
			if env.OK || env.Error == nil || env.Error.Code != 2 {
				t.Errorf("want ok=false error.code=2, got: %s", stdout)
			}
		})
	}
}

// TestRemoveArtifactDestIsAcceptedAsPath keeps a receipt that spells the
// target `dest` working, since both spellings show up in install records.
func TestRemoveArtifactDestIsAcceptedAsPath(t *testing.T) {
	w := raNewWorld(t, raClusterList, raImageList)
	bin := raBinaryFile(t)
	stdout, _, code := raRun(t, w.env, "--kind=binary", "--dest="+bin)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s", code, stdout)
	}
	if _, err := os.Stat(bin); err == nil {
		t.Error("--dest did not remove the binary")
	}
}

func TestRemoveArtifactMissingToolIsPrerequisite(t *testing.T) {
	dir := raSanitizedBin(t)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"stack without k3d", []string{"--kind=stack", "--cluster=memql"}},
		{"images without docker", []string{"--kind=images", "--image-prefix=memql"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, _, code := raRun(t, []string{"PATH=" + dir}, tc.args...)
			if code != 4 {
				t.Fatalf("exit %d, want 4 (prerequisite missing)\nstdout: %s", code, stdout)
			}
		})
	}
}

func TestRemoveArtifactPrintSpec(t *testing.T) {
	stdout, _, code := raRun(t, nil, "--print-spec")
	if code != 0 {
		t.Fatalf("--print-spec exited %d\n%s", code, stdout)
	}
	var spec struct {
		Capability string `json:"capability"`
		Params     []struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &spec); err != nil {
		t.Fatalf("spec is not JSON: %v\n%s", err, stdout)
	}
	if spec.Capability != "install.removeArtifact" {
		t.Errorf("capability = %q, want install.removeArtifact", spec.Capability)
	}
	names := map[string]bool{}
	for _, p := range spec.Params {
		names[p.Name] = true
	}
	for _, want := range []string{"kind", "pre-existing", "path", "dest"} {
		if !names[want] {
			t.Errorf("spec is missing the %q param; got %v", want, names)
		}
	}
}

// raSanitizedBin builds a bin directory holding ONLY the shell utilities the
// capability library needs, so the missing-tool cases are genuinely missing
// (the runner has docker and mkcert on its real PATH).
func raSanitizedBin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"bash", "tr", "grep", "sed", "cat", "head", "tail",
		"mktemp", "chmod", "rm", "awk", "cut", "sort", "wc", "printf", "mkdir", "dirname", "xargs"} {
		src, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if err := os.Symlink(src, filepath.Join(dir, name)); err != nil && !os.IsExist(err) {
			t.Fatalf("symlink %s: %v", name, err)
		}
	}
	return dir
}
