// Behaviour gate for install.binary (znasllc-io/memql#3360).
//
// install.binary is the only capability in the graph that puts a foreign
// executable on the machine. Everything that makes that safe is asserted here:
// it downloads exactly the artifact tool-pins.env names, it refuses anything
// whose sha256 does not match, --dry-run writes nothing, and it records whether
// the tool ALREADY existed outside the install dir -- the flag that stops a
// later uninstall from deleting the user's own k3d.
//
// The tests are hermetic. Pins point at file:// artifacts the test creates, so
// nothing here touches the network, and dest is always a t.TempDir(): the real
// ~/.memql is never written to.
package install

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type installResult struct {
	Tool             string `json:"tool"`
	Version          string `json:"version"`
	URL              string `json:"url"`
	SHA256           string `json:"sha256"`
	Dest             string `json:"dest"`
	Path             string `json:"path"`
	Installed        *bool  `json:"installed"`
	AlreadyInstalled *bool  `json:"alreadyInstalled"`
	PreExisting      *bool  `json:"preExisting"`
	DryRun           *bool  `json:"dryRun"`
}

//=============================================================================
// HARNESS
//=============================================================================

// installPATH builds an exclusive PATH carrying only what install-binary.sh and
// capability.sh genuinely need. Keeping it exclusive means no real k3d/kubectl
// on the runner can leak into a preExisting assertion.
func installPATH(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	linkReal(t, dir, "bash", "dirname", "tr", "mktemp", "mkdir", "chmod",
		"mv", "rm", "cp", "wc", "sha256sum")
	return dir
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// fakeArtifact writes a byte blob that stands in for a release binary and
// returns its file:// URL and digest.
func fakeArtifact(t *testing.T, name, body string) (url, digest string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	return "file://" + p, sha256Hex([]byte(body))
}

// writePins renders a pins manifest in the committed file's format.
func writePins(t *testing.T, entries map[string][3]string) string {
	t.Helper()
	var sb strings.Builder
	sb.WriteString("# test pins\n")
	for tool, triple := range entries {
		key := strings.ToUpper(tool)
		fmt.Fprintf(&sb, "\n%s_VERSION=%s\n%s_URL=%s\n%s_SHA256=%s\n",
			key, triple[0], key, triple[1], key, triple[2])
	}
	p := filepath.Join(t.TempDir(), "tool-pins.env")
	if err := os.WriteFile(p, []byte(sb.String()), 0o644); err != nil {
		t.Fatalf("write pins: %v", err)
	}
	return p
}

// runInstall executes install-binary.sh under an exclusive PATH and a HOME the
// test owns, so the default dest can never be the operator's real ~/.memql.
func runInstall(t *testing.T, pathDir, home string, args ...string) (capEnvelope, string, int) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{scriptPath(t, "install-binary.sh")}, args...)...)
	cmd.Env = []string{"PATH=" + pathDir, "HOME=" + home}
	cmd.Stdin = nil
	out, err := cmd.CombinedOutput()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run install-binary.sh: %v\noutput:\n%s", err, out)
	}
	line := lastJSONObject(string(out))
	if line == "" {
		t.Fatalf("install-binary.sh emitted no JSON envelope (exit %d)\noutput:\n%s", code, out)
	}
	var env capEnvelope
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		t.Fatalf("envelope is not valid JSON: %v\nline: %s", err, line)
	}
	return env, string(out), code
}

func decodeInstall(t *testing.T, env capEnvelope) installResult {
	t.Helper()
	var r installResult
	if err := json.Unmarshal(env.Result, &r); err != nil {
		t.Fatalf("result is not an install report: %v\nresult: %s", err, env.Result)
	}
	return r
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("readdir %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// k3dPins is the common fixture: one pinned tool backed by a file:// artifact.
func k3dPins(t *testing.T) (pins, digest, body string) {
	t.Helper()
	body = strings.Repeat("k3d-release-bytes\n", 64)
	url, digest := fakeArtifact(t, "k3d-linux-amd64", body)
	pins = writePins(t, map[string][3]string{"k3d": {"v5.9.0", url, digest}})
	return pins, digest, body
}

//=============================================================================
// TESTS
//=============================================================================

// TestUnknownToolIsBadParam pins the exit code the issue names. An unrecognized
// --tool is a malformed request (2), not a failed operation -- nothing was
// attempted, and no retry or prerequisite install will make "helm" appear in a
// manifest that does not pin it.
func TestUnknownToolIsBadParam(t *testing.T) {
	home := t.TempDir()
	pins, _, _ := k3dPins(t)
	dest := filepath.Join(t.TempDir(), "bin")

	env, out, code := runInstall(t, installPATH(t), home,
		"--tool=helm", "--pins="+pins, "--dest="+dest)
	if code != 2 {
		t.Errorf("unknown --tool exited %d, want 2 (bad param)\noutput:\n%s", code, out)
	}
	if env.OK {
		t.Error("envelope ok=true for an unknown tool")
	}
	if env.Error == nil || env.Error.Code != 2 {
		t.Errorf("envelope error.code should be 2; envelope: %s", out)
	}
	if env.Changed {
		t.Error("changed=true although nothing was attempted")
	}
	if names := dirEntries(t, dest); len(names) != 0 {
		t.Errorf("dest was populated on a rejected request: %v", names)
	}
}

// TestMissingToolParamIsBadParam -- --tool is required.
func TestMissingToolParamIsBadParam(t *testing.T) {
	home := t.TempDir()
	pins, _, _ := k3dPins(t)
	_, out, code := runInstall(t, installPATH(t), home, "--pins="+pins)
	if code != 2 {
		t.Errorf("missing --tool exited %d, want 2\noutput:\n%s", code, out)
	}
}

// TestDryRunWritesNothing is THE assertion of the dry-run path: it must be a
// plan, not a quieter install. Nothing lands in dest -- not the binary, not the
// directory, not a staging file.
func TestDryRunWritesNothing(t *testing.T) {
	home := t.TempDir()
	pins, digest, _ := k3dPins(t)
	dest := filepath.Join(t.TempDir(), "bin")

	env, out, code := runInstall(t, installPATH(t), home,
		"--tool=k3d", "--pins="+pins, "--dest="+dest, "--dry-run")
	if code != 0 {
		t.Fatalf("dry run exited %d, want 0\noutput:\n%s", code, out)
	}
	if env.Changed {
		t.Error("changed=true on a dry run -- a dry run mutates nothing")
	}
	r := decodeInstall(t, env)
	if r.DryRun == nil || !*r.DryRun {
		t.Errorf("dryRun=%v, want true", r.DryRun)
	}
	if r.Installed != nil && *r.Installed {
		t.Error("installed=true on a dry run")
	}
	if r.SHA256 != digest {
		t.Errorf("sha256 = %q, want the pinned digest %q -- a dry run still reports the plan", r.SHA256, digest)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("dry run created the dest directory %s (err=%v) -- it must write NOTHING", dest, err)
	}
	if names := dirEntries(t, dest); len(names) != 0 {
		t.Errorf("dry run wrote into dest: %v", names)
	}
}

// TestDryRunDefaultDestIsUnderHome documents the default without touching it.
func TestDryRunDefaultDestIsUnderHome(t *testing.T) {
	home := t.TempDir()
	pins, _, _ := k3dPins(t)

	env, out, code := runInstall(t, installPATH(t), home, "--tool=k3d", "--pins="+pins, "--dry-run")
	if code != 0 {
		t.Fatalf("dry run exited %d\noutput:\n%s", code, out)
	}
	r := decodeInstall(t, env)
	want := filepath.Join(home, ".memql", "bin")
	if r.Dest != want {
		t.Errorf("default dest = %q, want %q", r.Dest, want)
	}
	if names := dirEntries(t, home); len(names) != 0 {
		t.Errorf("dry run created %v under HOME", names)
	}
}

// TestInstallVerifiesAndPlaces is the happy path: verified bytes land in dest,
// executable, and the run reports changed=true.
func TestInstallVerifiesAndPlaces(t *testing.T) {
	home := t.TempDir()
	pins, digest, body := k3dPins(t)
	dest := filepath.Join(t.TempDir(), "bin")

	env, out, code := runInstall(t, installPATH(t), home,
		"--tool=k3d", "--pins="+pins, "--dest="+dest)
	if code != 0 {
		t.Fatalf("install exited %d, want 0\noutput:\n%s", code, out)
	}
	if !env.Changed {
		t.Error("changed=false although the tool was installed")
	}
	r := decodeInstall(t, env)
	if r.Installed == nil || !*r.Installed {
		t.Errorf("installed=%v, want true", r.Installed)
	}
	if r.Version != "v5.9.0" {
		t.Errorf("version = %q, want v5.9.0", r.Version)
	}
	if r.SHA256 != digest {
		t.Errorf("sha256 = %q, want %q", r.SHA256, digest)
	}

	binary := filepath.Join(dest, "k3d")
	if r.Path != binary {
		t.Errorf("path = %q, want %q", r.Path, binary)
	}
	got, err := os.ReadFile(binary)
	if err != nil {
		t.Fatalf("installed binary not readable: %v", err)
	}
	if string(got) != body {
		t.Error("installed bytes differ from the pinned artifact")
	}
	info, err := os.Stat(binary)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("installed binary is not executable (mode %v)", info.Mode().Perm())
	}
	if names := dirEntries(t, dest); len(names) != 1 {
		t.Errorf("dest holds %v -- a staging file was left behind", names)
	}
}

// TestDigestMismatchIsRefusedAndLeavesNothing is the reason the pins exist. When
// the bytes do not match the reviewed digest the artifact is discarded, not
// installed-with-a-warning, and dest is left exactly as it was.
func TestDigestMismatchIsRefusedAndLeavesNothing(t *testing.T) {
	home := t.TempDir()
	url, _ := fakeArtifact(t, "k3d-linux-amd64", strings.Repeat("tampered\n", 64))
	wrong := sha256Hex([]byte("what the pin claims this artifact is"))
	pins := writePins(t, map[string][3]string{"k3d": {"v5.9.0", url, wrong}})
	dest := filepath.Join(t.TempDir(), "bin")

	env, out, code := runInstall(t, installPATH(t), home,
		"--tool=k3d", "--pins="+pins, "--dest="+dest)
	if code != 5 {
		t.Errorf("digest mismatch exited %d, want 5 (operation failed)\noutput:\n%s", code, out)
	}
	if env.OK {
		t.Error("envelope ok=true for a digest mismatch")
	}
	if env.Changed {
		t.Error("changed=true although the artifact was rejected")
	}
	if _, err := os.Stat(filepath.Join(dest, "k3d")); !os.IsNotExist(err) {
		t.Errorf("a tool with a mismatched digest was installed anyway (stat err=%v)", err)
	}
	if names := dirEntries(t, dest); len(names) != 0 {
		t.Errorf("dest holds %v after a rejected download -- the staging file leaked", names)
	}
}

// TestPreExistingTracksToolsOutsideDest is the flag that protects the user's own
// tooling. preExisting=true means "this machine already had k3d before we ran",
// which is what a later uninstall reads to decide it must NOT remove it.
func TestPreExistingTracksToolsOutsideDest(t *testing.T) {
	pins, _, _ := k3dPins(t)

	t.Run("tool already on PATH outside dest", func(t *testing.T) {
		home := t.TempDir()
		pathDir := installPATH(t)
		writeStub(t, pathDir, "k3d", `echo "k3d version v5.0.0"`)
		dest := filepath.Join(t.TempDir(), "bin")

		r := decodeInstall(t, mustInstall(t, pathDir, home,
			"--tool=k3d", "--pins="+pins, "--dest="+dest, "--dry-run"))
		if r.PreExisting == nil || !*r.PreExisting {
			t.Errorf("preExisting=%v, want true -- the user's own k3d is on PATH", r.PreExisting)
		}
	})

	t.Run("no tool anywhere", func(t *testing.T) {
		home := t.TempDir()
		dest := filepath.Join(t.TempDir(), "bin")
		r := decodeInstall(t, mustInstall(t, installPATH(t), home,
			"--tool=k3d", "--pins="+pins, "--dest="+dest, "--dry-run"))
		if r.PreExisting == nil || *r.PreExisting {
			t.Errorf("preExisting=%v, want false -- nothing is on PATH", r.PreExisting)
		}
	})

	t.Run("tool on PATH but only inside dest", func(t *testing.T) {
		home := t.TempDir()
		pathDir := installPATH(t)
		dest := filepath.Join(t.TempDir(), "bin")
		if err := os.MkdirAll(dest, 0o755); err != nil {
			t.Fatalf("mkdir dest: %v", err)
		}
		writeStub(t, dest, "k3d", `echo "k3d version v5.9.0"`)

		// dest itself on PATH: this is the state after a previous install.
		r := decodeInstall(t, mustInstall(t, pathDir+":"+dest, home,
			"--tool=k3d", "--pins="+pins, "--dest="+dest, "--dry-run"))
		if r.PreExisting == nil || *r.PreExisting {
			t.Errorf("preExisting=%v, want false -- the only k3d is the one WE manage, "+
				"and mistaking it for the user's makes uninstall refuse to clean up", r.PreExisting)
		}
	})
}

// TestInstallIsIdempotent -- re-running against an already-correct binary is a
// successful no-op, not a re-download.
func TestInstallIsIdempotent(t *testing.T) {
	home := t.TempDir()
	pins, _, body := k3dPins(t)
	dest := filepath.Join(t.TempDir(), "bin")
	pathDir := installPATH(t)

	first := decodeInstall(t, mustInstall(t, pathDir, home, "--tool=k3d", "--pins="+pins, "--dest="+dest))
	if first.Installed == nil || !*first.Installed {
		t.Fatalf("first install did not install: %+v", first)
	}

	env, out, code := runInstall(t, pathDir, home, "--tool=k3d", "--pins="+pins, "--dest="+dest)
	if code != 0 {
		t.Fatalf("second install exited %d, want 0\noutput:\n%s", code, out)
	}
	if env.Changed {
		t.Error("changed=true on the second run -- an already-verified binary is a no-op")
	}
	r := decodeInstall(t, env)
	if r.AlreadyInstalled == nil || !*r.AlreadyInstalled {
		t.Errorf("alreadyInstalled=%v, want true", r.AlreadyInstalled)
	}
	got, err := os.ReadFile(filepath.Join(dest, "k3d"))
	if err != nil || string(got) != body {
		t.Errorf("the installed binary changed on an idempotent run (err=%v)", err)
	}
}

// TestMissingPinsManifestIsPrerequisite -- the manifest is an artifact the
// installer depends on, so its absence is 4 (prerequisite missing): regenerate
// it and retry. That is a genuinely actionable instruction, unlike a bad-param
// error which would suggest the caller's request was malformed.
func TestMissingPinsManifestIsPrerequisite(t *testing.T) {
	home := t.TempDir()
	dest := filepath.Join(t.TempDir(), "bin")
	missing := filepath.Join(t.TempDir(), "does-not-exist.env")

	env, out, code := runInstall(t, installPATH(t), home,
		"--tool=k3d", "--pins="+missing, "--dest="+dest)
	if code != 4 {
		t.Errorf("missing pins manifest exited %d, want 4 (prerequisite missing)\noutput:\n%s", code, out)
	}
	if env.Error == nil || env.Error.Code != 4 {
		t.Errorf("envelope error.code should be 4; envelope: %s", out)
	}
}

// TestDefaultPinsIsTheCommittedManifest -- with no --pins, the script reads the
// manifest committed next to it, so the digests in force are the reviewed ones.
func TestDefaultPinsIsTheCommittedManifest(t *testing.T) {
	home := t.TempDir()
	dest := filepath.Join(t.TempDir(), "bin")

	env, out, code := runInstall(t, installPATH(t), home, "--tool=k3d", "--dest="+dest, "--dry-run")
	if code != 0 {
		t.Fatalf("dry run against the committed pins exited %d\noutput:\n%s", code, out)
	}
	r := decodeInstall(t, env)
	pins := parsePins(t, pinsPath(t))
	if r.SHA256 != pins["K3D_SHA256"] {
		t.Errorf("default run used sha256 %q, want the committed %q", r.SHA256, pins["K3D_SHA256"])
	}
	if r.URL != pins["K3D_URL"] {
		t.Errorf("default run used url %q, want the committed %q", r.URL, pins["K3D_URL"])
	}
}

// TestUnwritableDestStillEmitsAnEnvelope guards the result guarantee across the
// staging-cleanup trap. install.binary needs an EXIT trap to remove its staging
// dir, which REPLACES the one cap_init installs -- the trap that promises a
// failure envelope even on an unexpected abort. Chaining the two is easy to get
// subtly wrong (an errexit shell will happily abandon the handler midway), and
// the symptom is silent: a non-zero exit with nothing on stdout, which every
// caller parsing the envelope reads as a crash with no reason.
//
// The pin here is file://, so this exercises the failure without the network.
func TestUnwritableDestStillEmitsAnEnvelope(t *testing.T) {
	home := t.TempDir()
	pins, _, _ := k3dPins(t)

	// dest's parent is a regular FILE, so creating the directory cannot work.
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	dest := filepath.Join(blocker, "bin")

	env, out, code := runInstall(t, installPATH(t), home,
		"--tool=k3d", "--pins="+pins, "--dest="+dest)
	if code == 0 {
		t.Fatalf("install into an unwritable dest exited 0\noutput:\n%s", out)
	}
	if env.OK {
		t.Errorf("envelope ok=true although the install could not complete\noutput:\n%s", out)
	}
	if env.Error == nil {
		t.Errorf("envelope carries no error block -- the caller gets a non-zero exit "+
			"with no reason\noutput:\n%s", out)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Errorf("dest %s exists after a failed install", dest)
	}
}

func mustInstall(t *testing.T, pathDir, home string, args ...string) capEnvelope {
	t.Helper()
	env, out, code := runInstall(t, pathDir, home, args...)
	if code != 0 {
		t.Fatalf("install exited %d, want 0\noutput:\n%s", code, out)
	}
	return env
}
