package pipelinesteps

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// The two scripts only ever run inside a pod, where nothing in this package's
// tests can reach them. So they are run here, under the same /bin/sh -c the
// containers use, with /workspace pointed at a temporary directory -- the one
// edit the harness makes, and it refuses to run if there is nothing to edit.

type shellRun struct {
	stdout, stderr string
	code           int
}

func runScript(t *testing.T, script, workspace string, env []string) shellRun {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh on this machine; the scripts are POSIX sh by contract")
	}
	if !strings.Contains(script, "/workspace") {
		t.Fatal("the script no longer names /workspace; this harness points that path at a temporary directory")
	}
	// The script is a package constant and the substitute a t.TempDir() path,
	// single-quoted so a temporary directory with a space in it stays one word.
	quoted := "'" + strings.ReplaceAll(workspace, "'", `'"'"'`) + "'"
	local := strings.ReplaceAll(script, "/workspace", quoted)
	// Started under a restrictive umask, as a container's process may be, so a
	// script that relies on the mask it inherits shows it here.
	cmd := exec.Command("/bin/sh", "-c", `umask 077 && exec /bin/sh -c "$1"`, "sh", local)
	cmd.Dir = t.TempDir() // the script must place itself; it does not inherit the workspace
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		return shellRun{stdout.String(), stderr.String(), exit.ExitCode()}
	case err != nil:
		t.Fatalf("running the script: %v", err)
	}
	return shellRun{stdout.String(), stderr.String(), 0}
}

func requireTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not on PATH; this test runs the script for real", tool)
		}
	}
}

const testMarker = "::memql-artifacts::0123456789abcdef"

func TestStepWrapperExitsWithTheCommandsOwnStatus(t *testing.T) {
	workspace := t.TempDir()
	res := runScript(t, stepWrapper, workspace, []string{
		"PATH=" + os.Getenv("PATH"),
		"MEMQL_STEP_COMMAND=pwd; echo to-stderr >&2; exit 3",
		"MEMQL_ARTIFACT_MARKER=" + testMarker,
	})
	if res.code != 3 {
		t.Errorf("exit status = %d, want the command's own 3 (stderr %q)", res.code, res.stderr)
	}
	if got := strings.TrimSpace(res.stdout); got != workspace {
		t.Errorf("the command ran in %q, want the workspace %q", got, workspace)
	}
	if !strings.Contains(res.stderr, "to-stderr") {
		t.Errorf("stderr = %q, want the command's own stderr passed through", res.stderr)
	}
	if strings.Contains(res.stdout, testMarker) {
		t.Errorf("stdout carries an artifact frame with no artifacts declared:\n%s", res.stdout)
	}
}

func TestStepWrapperFramesDeclaredArtifactsAfterTheCommand(t *testing.T) {
	requireTools(t, "tar", "gzip", "base64")
	workspace := t.TempDir()
	for name, body := range map[string]string{
		"coverage.out":  "mode: set\n",
		"reports/a.xml": "<a/>\n",
		"reports/b.xml": "<b/>\n",
		"notes.txt":     "not declared\n",
	} {
		path := filepath.Join(workspace, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// A FAILED step still frames its artifacts -- a failing run's reports are
	// the ones somebody needs -- and still exits with its own status.
	res := runScript(t, stepWrapper, workspace, []string{
		"PATH=" + os.Getenv("PATH"),
		"MEMQL_STEP_COMMAND=echo running; exit 2",
		"MEMQL_STEP_ARTIFACTS=coverage.out reports/*.xml",
		"MEMQL_ARTIFACT_MARKER=" + testMarker,
	})
	if res.code != 2 {
		t.Fatalf("exit status = %d, want the command's own 2 (stderr %q)", res.code, res.stderr)
	}
	lines := strings.Split(strings.TrimRight(res.stdout, "\n"), "\n")
	if len(lines) < 4 || lines[0] != "running" || lines[1] != testMarker+" begin" || lines[len(lines)-1] != testMarker+" end" {
		t.Fatalf("stdout is not the command's output followed by one frame:\n%s", res.stdout)
	}

	tgz, err := base64.StdEncoding.DecodeString(strings.Join(lines[2:len(lines)-1], ""))
	if err != nil {
		t.Fatalf("the frame is not base64: %v", err)
	}
	files := untar(t, tgz)
	var names []string
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	if want := []string{"coverage.out", "reports/a.xml", "reports/b.xml"}; strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("archived %q, want exactly the declared paths and glob matches %q", names, want)
	}
	if got := files["coverage.out"]; got != "mode: set\n" {
		t.Errorf("coverage.out archived as %q", got)
	}
}

// TestStepWrapperClearsTheUmaskBeforeTheCommandRuns: one cache volume serves
// steps running as root and steps running as an image's own non-root user, so
// a file one step leaves there has to stay writable by the next, whatever uid
// that one runs as (ruling R13). The wrapper's FIRST statement clears the
// umask, before anything it runs -- the command included -- creates a file.
func TestStepWrapperClearsTheUmaskBeforeTheCommandRuns(t *testing.T) {
	if first, _, _ := strings.Cut(stepWrapper, "\n"); first != "umask 0000" {
		t.Errorf("the wrapper's first statement is %q, want umask 0000", first)
	}
	workspace, cache := t.TempDir(), t.TempDir()
	res := runScript(t, stepWrapper, workspace, []string{
		"PATH=" + os.Getenv("PATH"),
		"CACHE_DIR=" + cache,
		`MEMQL_STEP_COMMAND=umask && mkdir -p "$CACHE_DIR/go/mod" && echo cached > "$CACHE_DIR/go/mod/entry" && echo built > out.bin`,
		"MEMQL_ARTIFACT_MARKER=" + testMarker,
	})
	if res.code != 0 {
		t.Fatalf("exit status = %d (stderr %q)", res.code, res.stderr)
	}
	if got := strings.TrimSpace(res.stdout); got != "0000" {
		t.Errorf("the command ran under umask %q, want 0000: the mask the container started with leaked through", got)
	}
	for path, want := range map[string]os.FileMode{
		filepath.Join(cache, "go"):                 0o777,
		filepath.Join(cache, "go", "mod"):          0o777,
		filepath.Join(cache, "go", "mod", "entry"): 0o666,
		filepath.Join(workspace, "out.bin"):        0o666,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("the command did not create %s: %v", path, err)
			continue
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s has mode %o, want %o: a step running as another uid could not write it", path, got, want)
		}
	}
}

// Review Focus 3: an image with no tar or base64 cannot frame its artifacts,
// and that must cost the artifacts, never the step's outcome.
func TestStepWrapperWithoutTarKeepsTheStepsStatus(t *testing.T) {
	nothing := t.TempDir() // a PATH on which no tool exists
	for _, status := range []string{"0", "4"} {
		res := runScript(t, stepWrapper, t.TempDir(), []string{
			"PATH=" + nothing,
			"MEMQL_STEP_COMMAND=exit " + status,
			"MEMQL_STEP_ARTIFACTS=coverage.out",
			"MEMQL_ARTIFACT_MARKER=" + testMarker,
		})
		if want := map[string]int{"0": 0, "4": 4}[status]; res.code != want {
			t.Errorf("command exit %s with no tar: wrapper exited %d, want %d", status, res.code, want)
		}
		if want := testMarker + " begin\n" + testMarker + " end\n"; res.stdout != want {
			t.Errorf("command exit %s with no tar: stdout = %q, want an empty frame %q", status, res.stdout, want)
		}
	}
}

func untar(t *testing.T, tgz []byte) map[string]string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		t.Fatalf("the frame is not gzip: %v", err)
	}
	tr := tar.NewReader(zr)
	files := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return files
		}
		if err != nil {
			t.Fatalf("the frame is not a tar: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[hdr.Name] = string(body)
	}
}

// gitEnv is a git environment that ignores the machine's own configuration: a
// developer's insteadOf rewrite or credential helper must not decide what the
// clone script does.
func gitEnv(t *testing.T) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(gitEnv(t),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestCloneScriptChecksOutTheSHAAndNothingAfterIt(t *testing.T) {
	requireTools(t, "git")
	src := t.TempDir()
	git(t, src, "init", "-q", ".")
	if err := os.WriteFile(filepath.Join(src, "first.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "inner.txt"), []byte("inner\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "add", "first.txt", "sub/inner.txt")
	git(t, src, "commit", "-q", "-m", "one")
	sha := git(t, src, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(src, "second.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, src, "add", "second.txt")
	git(t, src, "commit", "-q", "-m", "two")

	workspace := t.TempDir()
	res := runScript(t, cloneScript, workspace, append(gitEnv(t), "CLONE_URL=file://"+src, "SHA="+sha))
	if res.code != 0 {
		t.Fatalf("clone exited %d\nstdout %s\nstderr %s", res.code, res.stdout, res.stderr)
	}
	if head := git(t, workspace, "rev-parse", "HEAD"); head != sha {
		t.Errorf("checked out %s, want the SHA %s rather than the branch tip", head, sha)
	}
	if _, err := os.Stat(filepath.Join(workspace, "first.txt")); err != nil {
		t.Errorf("first.txt missing from the checkout: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "second.txt")); err == nil {
		t.Error("second.txt is in the checkout: it was committed after the SHA")
	}
	if shallow := git(t, workspace, "rev-parse", "--is-shallow-repository"); shallow != "true" {
		t.Errorf("the checkout is not shallow (%s): a full clone of a large repository is minutes per step", shallow)
	}
	if !strings.Contains(res.stdout, "memql: checked out "+sha) {
		t.Errorf("stdout = %q, want the line naming the SHA", res.stdout)
	}

	// Ruling R13: the clone runs as its image's user and the step as the step
	// image's, which need not match, so the checkout is world-writable inside
	// the pod's own scratch volume -- including the repository itself, which a
	// step's git commands write to.
	for path, want := range map[string]os.FileMode{
		filepath.Join(workspace, "first.txt"):        0o666,
		filepath.Join(workspace, "sub"):              0o777,
		filepath.Join(workspace, "sub", "inner.txt"): 0o666,
		filepath.Join(workspace, ".git"):             0o777,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("%s missing from the checkout: %v", path, err)
			continue
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s has mode %o, want %o: a step running as another uid could not write it", path, got, want)
		}
	}
}

// TestCloneScriptPreparesTheOwnersCacheDirectory (ruling R15): the step mounts
// only its owner's directory of the cache claim, and kubelet creates a missing
// subPath root-owned with the claim ROOT's mode -- so on a claim whose root is
// not world-writable, a non-root step would get a cache it cannot write. The
// clone runs first and mounts the claim's root, so it creates the owner's
// directory itself, world-writable, and opens up one created narrower before.
func TestCloneScriptPreparesTheOwnersCacheDirectory(t *testing.T) {
	requireTools(t, "git")
	src := t.TempDir()
	git(t, src, "init", "-q", ".")
	git(t, src, "commit", "-q", "--allow-empty", "-m", "one")
	sha := git(t, src, "rev-parse", "HEAD")
	clone := func(t *testing.T, ownerCache string) shellRun {
		env := append(gitEnv(t), "CLONE_URL=file://"+src, "SHA="+sha)
		if ownerCache != "" {
			env = append(env, "OWNER_CACHE="+ownerCache)
		}
		return runScript(t, cloneScript, t.TempDir(), env)
	}
	modeOf := func(t *testing.T, path string) os.FileMode {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		return info.Mode().Perm()
	}

	t.Run("a missing owner directory is created world-writable", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "owners", "0dde18ce172fff450b32bf41")
		if res := clone(t, dir); res.code != 0 {
			t.Fatalf("clone exited %d\nstderr %s", res.code, res.stderr)
		}
		if got := modeOf(t, dir); got != 0o777 {
			t.Errorf("the owner's cache directory has mode %o, want 777: a non-root step could not write it", got)
		}
	})

	t.Run("an owner directory made narrower before is opened up", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "owners", "0dde18ce172fff450b32bf41")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if res := clone(t, dir); res.code != 0 {
			t.Fatalf("clone exited %d\nstderr %s", res.code, res.stderr)
		}
		if got := modeOf(t, dir); got != 0o777 {
			t.Errorf("the existing owner directory kept mode %o, want 777", got)
		}
	})

	t.Run("a step with no cache prepares nothing and still clones", func(t *testing.T) {
		if res := clone(t, ""); res.code != 0 {
			t.Fatalf("clone without a cache exited %d: the preparation must be skipped, not attempted\nstderr %s", res.code, res.stderr)
		}
	})

	t.Run("a directory that cannot be prepared fails the clone, naming it", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root, which writes through any mode: nothing here can refuse it")
		}
		root := t.TempDir()
		if err := os.Chmod(root, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
		dir := filepath.Join(root, "owners", "0dde18ce172fff450b32bf41")
		res := clone(t, dir)
		if res.code == 0 {
			t.Fatal("the clone succeeded with a cache directory it could not create; the step would meet an unwritable cache instead")
		}
		if !strings.Contains(res.stderr, dir) {
			t.Errorf("stderr = %q, want a line naming %s", res.stderr, dir)
		}
	})
}

// TestCloneScriptSendsTheTokenAsBasicAuthAndNeverPrintsIt: GitHub accepts an
// installation token as the password of user x-access-token. The header is
// what a private clone stands on, and the token must not reach the log the
// clone's output becomes.
func TestCloneScriptSendsTheTokenAsBasicAuthAndNeverPrintsIt(t *testing.T) {
	requireTools(t, "git", "base64", "tr")

	fetch := func(t *testing.T, token string) ([]string, shellRun) {
		var mu sync.Mutex
		var seen []string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen = append(seen, r.Header.Get("Authorization"))
			mu.Unlock()
			http.Error(w, "no such repository", http.StatusNotFound)
		}))
		defer srv.Close()
		env := append(gitEnv(t), "CLONE_URL="+srv.URL+"/acme/widget.git", "SHA="+testSHA)
		if token != "" {
			env = append(env, "GIT_TOKEN="+token)
		}
		res := runScript(t, cloneScript, t.TempDir(), env)
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...), res
	}

	t.Run("with a token", func(t *testing.T) {
		// Long enough that base64 wraps its output (76 columns): the header
		// must still be one line, which is what the script's tr is for.
		token := plantedClone + strings.Repeat("x", 80)
		seen, res := fetch(t, token)
		if res.code == 0 {
			t.Fatal("the clone succeeded against a server that has no repository")
		}
		if len(seen) == 0 {
			t.Fatalf("git never reached the server\nstderr %s", res.stderr)
		}
		want := "basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
		for i, got := range seen {
			if got != want {
				t.Errorf("request %d Authorization = %q, want %q", i, got, want)
			}
		}
		output := res.stdout + res.stderr
		if strings.Contains(output, token) || strings.Contains(output, strings.TrimPrefix(want, "basic ")) {
			t.Errorf("the clone's output carries the token:\n%s", output)
		}
	})

	t.Run("anonymous", func(t *testing.T) {
		seen, res := fetch(t, "")
		if len(seen) == 0 {
			t.Fatalf("git never reached the server\nstderr %s", res.stderr)
		}
		for i, got := range seen {
			if got != "" {
				t.Errorf("request %d carried Authorization %q with no token", i, got)
			}
		}
	})
}
