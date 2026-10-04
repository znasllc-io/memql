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
	"strconv"
	"strings"
	"sync"
	"testing"
)

// The scripts only ever run inside a pod, where nothing in this package's
// tests can reach them. So they are run here, under the same /bin/sh -c the
// containers use, started under a restrictive umask (077) as a container's
// process may be -- so a script that leans on the mask it inherits shows it.
// The step wrapper and the clone get /workspace pointed at a temporary
// directory, the one edit the harness makes, and it refuses to run if there is
// nothing to edit.

type shellRun struct {
	stdout, stderr string
	code           int
}

func runScript(t *testing.T, script, workspace string, env []string) shellRun {
	t.Helper()
	if !strings.Contains(script, "/workspace") {
		t.Fatal("the script no longer names /workspace; this harness points that path at a temporary directory")
	}
	// The script is a package constant and the substitute a t.TempDir() path,
	// single-quoted so a temporary directory with a space in it stays one word.
	quoted := "'" + strings.ReplaceAll(workspace, "'", `'"'"'`) + "'"
	return runShell(t, strings.ReplaceAll(script, "/workspace", quoted), env)
}

// runShell runs script exactly as given.
func runShell(t *testing.T, script string, env []string) shellRun {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh on this machine; the scripts are POSIX sh by contract")
	}
	cmd := exec.Command("/bin/sh", "-c", `umask 077 && exec /bin/sh -c "$1"`, "sh", script)
	cmd.Dir = t.TempDir() // a script must place itself; it does not inherit a directory
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

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return info.Mode().Perm()
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

// frameOf finds the artifact frame in a wrapper's stdout: the begin and end
// marker lines, WHOLE lines as the capture matches them, and the base64
// between them.
func frameOf(t *testing.T, stdout string) (before []string, payload string) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	begin := -1
	for i, line := range lines {
		if line == testMarker+" begin" {
			begin = i
			break
		}
	}
	if begin < 0 || lines[len(lines)-1] != testMarker+" end" {
		t.Fatalf("no whole-line frame in the wrapper's stdout -- the capture would not open it:\n%q", stdout)
	}
	return lines[:begin], strings.Join(lines[begin+1:len(lines)-1], "")
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStepWrapperFramesDeclaredArtifactsAfterTheCommand(t *testing.T) {
	requireTools(t, "tar", "gzip", "base64")
	workspace := t.TempDir()
	writeFiles(t, workspace, map[string]string{
		"coverage.out":  "mode: set\n",
		"reports/a.xml": "<a/>\n",
		"reports/b.xml": "<b/>\n",
		"notes.txt":     "not declared\n",
	})

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
	before, payload := frameOf(t, res.stdout)
	// Ruling R18: one blank line precedes the begin marker -- the price of
	// putting the marker on a line of its own whatever the command printed.
	if strings.Join(before, "|") != "running|" {
		t.Errorf("the lines before the frame are %q, want the command's output and one blank line", before)
	}

	tgz, err := base64.StdEncoding.DecodeString(payload)
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

// TestStepWrapperBeginsTheFrameOnALineOfItsOwn (the reviewer's IMPORTANT 2,
// ruling R18): a command whose last output has no trailing newline would glue
// the begin marker to its last words, the capture's whole-line match would
// never open the frame, and the base64 would flood the log while the
// artifacts were lost.
func TestStepWrapperBeginsTheFrameOnALineOfItsOwn(t *testing.T) {
	requireTools(t, "tar", "gzip", "base64")
	workspace := t.TempDir()
	writeFiles(t, workspace, map[string]string{"coverage.out": "mode: set\n"})
	res := runScript(t, stepWrapper, workspace, []string{
		"PATH=" + os.Getenv("PATH"),
		"MEMQL_STEP_COMMAND=printf 'last words'",
		"MEMQL_STEP_ARTIFACTS=coverage.out",
		"MEMQL_ARTIFACT_MARKER=" + testMarker,
	})
	if res.code != 0 {
		t.Fatalf("exit status = %d (stderr %q)", res.code, res.stderr)
	}
	before, payload := frameOf(t, res.stdout)
	if strings.Join(before, "|") != "last words" {
		t.Errorf("the lines before the frame are %q, want the command's unterminated last line alone", before)
	}
	tgz, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("the frame is not base64: %v", err)
	}
	if got := untar(t, tgz)["coverage.out"]; got != "mode: set\n" {
		t.Errorf("coverage.out archived as %q", got)
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
		if want := "\n" + testMarker + " begin\n" + testMarker + " end\n"; res.stdout != want {
			t.Errorf("command exit %s with no tar: stdout = %q, want an empty frame %q", status, res.stdout, want)
		}
	}
}

// TestStepWrapperLeavesTheCommandsUmaskAlone (the reviewer's IMPORTANT 3,
// ruling R13b): the user's command inherits whatever umask the wrapper runs
// under, and a world-writable default breaks tools that refuse files others can
// write -- OpenSSH a world-writable ~/.ssh/config, MySQL ~/.my.cnf. So the
// wrapper changes no umask; R13b shares the cache per uid instead.
func TestStepWrapperLeavesTheCommandsUmaskAlone(t *testing.T) {
	workspace := t.TempDir()
	res := runScript(t, stepWrapper, workspace, []string{
		"PATH=" + os.Getenv("PATH"),
		"MEMQL_STEP_COMMAND=umask && echo private > config",
		"MEMQL_ARTIFACT_MARKER=" + testMarker,
	})
	if res.code != 0 {
		t.Fatalf("exit status = %d (stderr %q)", res.code, res.stderr)
	}
	if got := strings.TrimSpace(res.stdout); got != "0077" {
		t.Errorf("the command ran under umask %q, want the 0077 the container started with", got)
	}
	if got := modeOf(t, filepath.Join(workspace, "config")); got != 0o600 {
		t.Errorf("a file the command wrote has mode %o, want 600", got)
	}
}

// TestStepWrapperGivesEachUIDItsOwnCacheTree (ruling R13b): steps of one owner
// share the owner's cache directory, but a root step and a uid-1000 step never
// share a tree inside it -- each uid writes its own, with its own umask -- so
// nothing has to be world-writable but the owner directory itself. The Job
// names the declared caches; the wrapper derives the paths from the uid it
// actually runs as.
func TestStepWrapperGivesEachUIDItsOwnCacheTree(t *testing.T) {
	requireTools(t, "id")
	uid := strconv.Itoa(os.Getuid())
	show := `printf '%s|%s|%s\n' "${GOMODCACHE-unset}" "${GOCACHE-unset}" "${npm_config_cache-unset}"`
	for _, tc := range []struct {
		caches, image, want string
	}{
		{"go npm", "", "/cache/go-u" + uid + "/mod|/cache/go-u" + uid + "/build|/cache/npm-u" + uid},
		{"go", "", "/cache/go-u" + uid + "/mod|/cache/go-u" + uid + "/build|unset"},
		{"npm", "", "unset|unset|/cache/npm-u" + uid},
		{"", "", "unset|unset|unset"},
		// An image's own setting survives a step that declares no cache.
		{"", "/image/own", "unset|/image/own|unset"},
	} {
		env := []string{
			"PATH=" + os.Getenv("PATH"),
			"MEMQL_STEP_COMMAND=" + show,
			"MEMQL_ARTIFACT_MARKER=" + testMarker,
		}
		if tc.caches != "" {
			env = append(env, "MEMQL_CACHES="+tc.caches)
		}
		if tc.image != "" {
			env = append(env, "GOCACHE="+tc.image)
		}
		res := runScript(t, stepWrapper, t.TempDir(), env)
		if res.code != 0 {
			t.Fatalf("caches %q: exit status = %d (stderr %q)", tc.caches, res.code, res.stderr)
		}
		if got := strings.TrimSpace(res.stdout); got != tc.want {
			t.Errorf("caches %q: the command saw %q, want %q", tc.caches, got, tc.want)
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
	writeFiles(t, src, map[string]string{"first.txt": "one\n", "sub/inner.txt": "inner\n"})
	git(t, src, "add", "first.txt", "sub/inner.txt")
	git(t, src, "commit", "-q", "-m", "one")
	sha := git(t, src, "rev-parse", "HEAD")
	writeFiles(t, src, map[string]string{"second.txt": "two\n"})
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

	// Ruling R13, kept by R13b for the clone: the clone runs as root and the
	// step as the step image's user, which need not match, so the checkout is
	// world-writable inside the pod's own scratch volume -- including the
	// repository itself, which a step's git commands write to.
	for path, want := range map[string]os.FileMode{
		filepath.Join(workspace, "first.txt"):        0o666,
		filepath.Join(workspace, "sub"):              0o777,
		filepath.Join(workspace, "sub", "inner.txt"): 0o666,
		filepath.Join(workspace, ".git"):             0o777,
	} {
		if got := modeOf(t, path); got != want {
			t.Errorf("%s has mode %o, want %o: a step running as another uid could not write it", path, got, want)
		}
	}
}

// TestCachePrepScriptPreparesTheOwnersCacheDirectory (rulings R15, R15b): the
// step mounts only its owner's directory of the cache claim, and kubelet would
// create a missing one root-owned with the claim ROOT's mode -- unwritable for
// a non-root step on a claim whose root is not world-writable. The cache-prep
// init container runs first and alone with the claim's root, and creates the
// directory world-writable, or opens up one created narrower before.
func TestCachePrepScriptPreparesTheOwnersCacheDirectory(t *testing.T) {
	prep := func(t *testing.T, ownerCache string) shellRun {
		return runShell(t, cachePrepScript, []string{"PATH=" + os.Getenv("PATH"), "OWNER_CACHE=" + ownerCache})
	}

	t.Run("a missing owner directory is created world-writable", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "owners", "0dde18ce172fff450b32bf41")
		if res := prep(t, dir); res.code != 0 {
			t.Fatalf("cache-prep exited %d\nstderr %s", res.code, res.stderr)
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
		if res := prep(t, dir); res.code != 0 {
			t.Fatalf("cache-prep exited %d\nstderr %s", res.code, res.stderr)
		}
		if got := modeOf(t, dir); got != 0o777 {
			t.Errorf("the existing owner directory kept mode %o, want 777", got)
		}
	})

	t.Run("a directory that cannot be prepared fails, naming it", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root, which writes through any mode: nothing here can refuse it")
		}
		root := t.TempDir()
		if err := os.Chmod(root, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
		dir := filepath.Join(root, "owners", "0dde18ce172fff450b32bf41")
		res := prep(t, dir)
		if res.code == 0 {
			t.Fatal("cache-prep succeeded with a directory it could not create; the step would meet an unwritable cache instead")
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
