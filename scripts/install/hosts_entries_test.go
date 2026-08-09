// Package install holds tests for the local-install capability scripts.
package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// hosts_entries_test.go -- znasllc-io/memql#3361.
//
// scripts/install/hosts-entries.sh writes the memQL front-door hostnames into
// the system hosts file inside a delimited `# BEGIN memql` block. It is the
// only script in the install substrate that edits a file the operator owns and
// that the rest of the machine depends on, so the bar is higher than "it
// works": an uninstall must give the operator back the EXACT bytes they had.
//
// That is what these tests are about. `add` then `remove` is asserted to
// restore the fixture byte for byte -- not "equivalent content", not "the
// hostnames are gone", but a byte comparison -- across the awkward shapes a
// real hosts file takes: no trailing newline, blank lines, an operator block
// appended AFTER ours, an empty file. A round-trip that silently normalises a
// missing trailing newline is a corrupted /etc/hosts on someone's laptop.
//
// Everything here drives an INJECTED --hosts-file under t.TempDir(). No test
// in this file may name /etc/hosts; TestHostsEntriesTestsNeverNameRealHostsFile
// keeps that honest for future edits.

//=============================================================================
// HARNESS
//=============================================================================

// hostsEnvelope is the capability result envelope (the contract's schema).
type hostsEnvelope struct {
	OK         bool           `json:"ok"`
	Capability string         `json:"capability"`
	Changed    bool           `json:"changed"`
	Result     map[string]any `json:"result"`
	Error      *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func hostsRepoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

func hostsScript(t *testing.T) string {
	t.Helper()
	return filepath.Join(hostsRepoRoot(t), "scripts", "install", "hosts-entries.sh")
}

// hostsExitCode extracts the process exit code from an exec error (-1 unknown).
func hostsExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// hostsLastJSONLine returns the last line of out that looks like a JSON object.
func hostsLastJSONLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "{") && strings.HasSuffix(l, "}") {
			return l
		}
	}
	return ""
}

// hostsRun invokes the script with stdin closed and returns the parsed
// envelope (from stdout only -- stdout must be pure JSON), the exit code and
// the combined output for diagnostics.
func hostsRun(t *testing.T, args ...string) (hostsEnvelope, int, string) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{hostsScript(t)}, args...)...)
	cmd.Stdin = nil
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := hostsExitCode(err)
	combined := "--- stdout ---\n" + stdout.String() + "--- stderr ---\n" + stderr.String()

	var env hostsEnvelope
	line := hostsLastJSONLine(stdout.String())
	if line == "" {
		return env, code, combined
	}
	if jerr := json.Unmarshal([]byte(line), &env); jerr != nil {
		t.Fatalf("stdout is not a valid JSON envelope: %v\nline: %s\n%s", jerr, line, combined)
	}
	return env, code, combined
}

// hostsFixture writes content to a fresh temp hosts file and returns its path.
func hostsFixture(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return p
}

func hostsRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

const (
	hostsConfirmAdd    = "add-memql-hosts"
	hostsConfirmRemove = "remove-memql-hosts"
)

// hostsFixtures are the awkward real-world shapes a hosts file takes. Every
// one of them must survive an add/remove round trip byte for byte.
var hostsFixtures = []struct {
	name    string
	content string
}{
	{"trailing newline", "127.0.0.1\tlocalhost\n::1\tlocalhost\n"},
	{"no trailing newline", "127.0.0.1\tlocalhost\n::1\tip6-localhost"},
	{"empty file", ""},
	{"blank lines and comments", "# operator notes\n\n127.0.0.1 localhost\n\n\n# tail comment\n"},
	{"only a comment, unterminated", "# nothing here"},
	{"trailing blank line", "127.0.0.1 localhost\n\n"},
}

//=============================================================================
// THE ASSERTION THAT MATTERS: add -> remove restores the file byte for byte
//=============================================================================

func TestHostsEntriesAddRemoveRoundTripIsByteExact(t *testing.T) {
	for _, f := range hostsFixtures {
		f := f
		t.Run(f.name, func(t *testing.T) {
			path := hostsFixture(t, f.content)

			env, code, out := hostsRun(t, "--action=add", "--hosts-file="+path, "--confirm="+hostsConfirmAdd)
			if code != 0 || !env.OK {
				t.Fatalf("add failed (exit %d): %s", code, out)
			}
			if !env.Changed {
				t.Errorf("add reported changed=false on a file with no block: %s", out)
			}
			added := hostsRead(t, path)
			if added == f.content {
				t.Fatalf("add did not modify the file")
			}
			if !strings.Contains(added, "cockpit.local.znas.io") {
				t.Errorf("added file is missing the front-door hostname:\n%q", added)
			}
			if n := strings.Count(added, "# BEGIN memql"); n != 1 {
				t.Errorf("want exactly 1 begin marker, got %d:\n%q", n, added)
			}
			if n := strings.Count(added, "# END memql"); n != 1 {
				t.Errorf("want exactly 1 end marker, got %d:\n%q", n, added)
			}
			// The operator's own bytes must still be in there verbatim.
			if !strings.Contains(added, f.content) {
				t.Errorf("operator content not preserved verbatim in:\n%q", added)
			}

			env, code, out = hostsRun(t, "--action=remove", "--hosts-file="+path, "--confirm="+hostsConfirmRemove)
			if code != 0 || !env.OK {
				t.Fatalf("remove failed (exit %d): %s", code, out)
			}
			if !env.Changed {
				t.Errorf("remove reported changed=false but a block was present: %s", out)
			}
			got := hostsRead(t, path)
			if got != f.content {
				t.Errorf("remove did not restore the file byte for byte\n got: %q\nwant: %q", got, f.content)
			}
		})
	}
}

// A block the operator has content AFTER must still round-trip: the block is
// rewritten in place, not moved to the end of the file.
func TestHostsEntriesRoundTripWithContentAfterTheBlock(t *testing.T) {
	original := "127.0.0.1 localhost\n"
	path := hostsFixture(t, original)

	if _, code, out := hostsRun(t, "--action=add", "--hosts-file="+path, "--confirm="+hostsConfirmAdd); code != 0 {
		t.Fatalf("add failed (exit %d): %s", code, out)
	}
	// The operator appends their own lines below our block.
	afterBlock := "\n# operator added this later\n10.0.0.5 nas.local\n"
	withTail := hostsRead(t, path) + afterBlock
	if err := os.WriteFile(path, []byte(withTail), 0o644); err != nil {
		t.Fatalf("write tail: %v", err)
	}

	// Re-adding with different hostnames must rewrite the block IN PLACE.
	env, code, out := hostsRun(t, "--action=add", "--hosts-file="+path,
		"--hostnames=one.local.znas.io,two.local.znas.io", "--confirm="+hostsConfirmAdd)
	if code != 0 || !env.OK {
		t.Fatalf("re-add failed (exit %d): %s", code, out)
	}
	updated := hostsRead(t, path)
	if !strings.HasSuffix(updated, afterBlock) {
		t.Errorf("operator's trailing content was not preserved at the end:\n%q", updated)
	}
	if strings.Contains(updated, "cockpit.local.znas.io") {
		t.Errorf("stale hostname survived the rewrite:\n%q", updated)
	}
	if n := strings.Count(updated, "# BEGIN memql"); n != 1 {
		t.Errorf("want exactly 1 begin marker after rewrite, got %d:\n%q", n, updated)
	}

	env, code, out = hostsRun(t, "--action=remove", "--hosts-file="+path, "--confirm="+hostsConfirmRemove)
	if code != 0 || !env.OK {
		t.Fatalf("remove failed (exit %d): %s", code, out)
	}
	// The blank line the operator typed above their own entries is THEIRS --
	// removing our block must leave it exactly where they put it.
	want := original + afterBlock
	if got := hostsRead(t, path); got != want {
		t.Errorf("remove did not restore operator content byte for byte\n got: %q\nwant: %q", got, want)
	}
}

//=============================================================================
// IDEMPOTENCY
//=============================================================================

func TestHostsEntriesSecondAddIsUnchangedAndLeavesOneBlock(t *testing.T) {
	path := hostsFixture(t, "127.0.0.1 localhost\n")

	env, code, out := hostsRun(t, "--action=add", "--hosts-file="+path, "--confirm="+hostsConfirmAdd)
	if code != 0 || !env.OK || !env.Changed {
		t.Fatalf("first add: exit %d changed %v: %s", code, env.Changed, out)
	}
	first := hostsRead(t, path)

	env, code, out = hostsRun(t, "--action=add", "--hosts-file="+path, "--confirm="+hostsConfirmAdd)
	if code != 0 || !env.OK {
		t.Fatalf("second add failed (exit %d): %s", code, out)
	}
	if env.Changed {
		t.Errorf("second add reported changed=true; the block was already correct: %s", out)
	}
	second := hostsRead(t, path)
	if second != first {
		t.Errorf("second add rewrote the file\n got: %q\nwant: %q", second, first)
	}
	if n := strings.Count(second, "# BEGIN memql"); n != 1 {
		t.Errorf("want exactly 1 begin marker after two adds, got %d:\n%q", n, second)
	}
	if n := strings.Count(second, "# END memql"); n != 1 {
		t.Errorf("want exactly 1 end marker after two adds, got %d:\n%q", n, second)
	}
}

func TestHostsEntriesRemoveWithoutBlockIsUnchanged(t *testing.T) {
	original := "127.0.0.1 localhost\n"
	path := hostsFixture(t, original)

	env, code, out := hostsRun(t, "--action=remove", "--hosts-file="+path, "--confirm="+hostsConfirmRemove)
	if code != 0 || !env.OK {
		t.Fatalf("remove failed (exit %d): %s", code, out)
	}
	if env.Changed {
		t.Errorf("remove of an absent block reported changed=true: %s", out)
	}
	if got := hostsRead(t, path); got != original {
		t.Errorf("remove touched a file with no block\n got: %q\nwant: %q", got, original)
	}
}

//=============================================================================
// CONFIRMATION -- no phrase is exit 3, and never a silent edit
//=============================================================================

func TestHostsEntriesWithoutConfirmationRefusesAndDoesNotEdit(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"add, no confirm", []string{"--action=add"}},
		{"add, wrong phrase", []string{"--action=add", "--confirm=yes"}},
		{"add, the remove phrase", []string{"--action=add", "--confirm=" + hostsConfirmRemove}},
		{"remove, no confirm", []string{"--action=remove"}},
		{"remove, the add phrase", []string{"--action=remove", "--confirm=" + hostsConfirmAdd}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			original := "127.0.0.1 localhost\n"
			path := hostsFixture(t, original)
			args := append([]string{"--hosts-file=" + path}, tc.args...)

			env, code, out := hostsRun(t, args...)
			if code != 3 {
				t.Errorf("exit %d, want 3 (refused): %s", code, out)
			}
			if env.OK {
				t.Errorf("envelope ok=true for a refused run: %s", out)
			}
			if env.Error == nil || env.Error.Code != 3 {
				t.Errorf("envelope should carry error.code=3: %s", out)
			}
			if got := hostsRead(t, path); got != original {
				t.Errorf("file edited without confirmation\n got: %q\nwant: %q", got, original)
			}
		})
	}
}

//=============================================================================
// PARAMETER VALIDATION + PREREQUISITES
//=============================================================================

func TestHostsEntriesBadParamsExitTwo(t *testing.T) {
	path := hostsFixture(t, "127.0.0.1 localhost\n")
	cases := []struct {
		name string
		args []string
	}{
		{"missing action", []string{"--confirm=" + hostsConfirmAdd}},
		{"unknown action", []string{"--action=install", "--confirm=" + hostsConfirmAdd}},
		{"empty hostnames", []string{"--action=add", "--hostnames= ", "--confirm=" + hostsConfirmAdd}},
		{"wildcard hostname", []string{"--action=add", "--hostnames=*.local.znas.io", "--confirm=" + hostsConfirmAdd}},
		{"hostname with a space", []string{"--action=add", "--hostnames=a.local b/c", "--confirm=" + hostsConfirmAdd}},
		{"bad ip", []string{"--action=add", "--ip=not an ip", "--confirm=" + hostsConfirmAdd}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			env, code, out := hostsRun(t, append([]string{"--hosts-file=" + path}, tc.args...)...)
			if code != 2 {
				t.Errorf("exit %d, want 2 (bad param): %s", code, out)
			}
			if env.OK || env.Error == nil || env.Error.Code != 2 {
				t.Errorf("envelope should carry ok=false error.code=2: %s", out)
			}
		})
	}
}

func TestHostsEntriesMissingHostsFileIsExitFour(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-hosts")
	env, code, out := hostsRun(t, "--action=add", "--hosts-file="+path, "--confirm="+hostsConfirmAdd)
	if code != 4 {
		t.Errorf("exit %d, want 4 (prerequisite missing): %s", code, out)
	}
	if env.OK || env.Error == nil || env.Error.Code != 4 {
		t.Errorf("envelope should carry ok=false error.code=4: %s", out)
	}
}

// An unwritable hosts file is the ordinary "you forgot sudo" case. It must be
// an honest prerequisite failure, not a stack trace or a partial write.
func TestHostsEntriesUnwritableFileIsExitFour(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: file permissions do not restrict writes")
	}
	original := "127.0.0.1 localhost\n"
	path := hostsFixture(t, original)
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	env, code, out := hostsRun(t, "--action=add", "--hosts-file="+path, "--confirm="+hostsConfirmAdd)
	if code != 4 {
		t.Errorf("exit %d, want 4 (prerequisite missing -- needs elevation): %s", code, out)
	}
	if env.OK || env.Error == nil || env.Error.Code != 4 {
		t.Errorf("envelope should carry ok=false error.code=4: %s", out)
	}
	if got := hostsRead(t, path); got != original {
		t.Errorf("read-only file was modified\n got: %q", got)
	}
}

// A hand-corrupted block (BEGIN with no END) must fail loudly rather than
// silently swallowing the rest of the operator's file.
func TestHostsEntriesUnterminatedBlockIsExitFive(t *testing.T) {
	original := "127.0.0.1 localhost\n# BEGIN memql\n127.0.0.1 cockpit.local.znas.io\n"
	path := hostsFixture(t, original)

	env, code, out := hostsRun(t, "--action=remove", "--hosts-file="+path, "--confirm="+hostsConfirmRemove)
	if code != 5 {
		t.Errorf("exit %d, want 5 (operation failed): %s", code, out)
	}
	if env.OK || env.Error == nil || env.Error.Code != 5 {
		t.Errorf("envelope should carry ok=false error.code=5: %s", out)
	}
	if got := hostsRead(t, path); got != original {
		t.Errorf("corrupt file was modified\n got: %q\nwant: %q", got, original)
	}
}

//=============================================================================
// THE FRONT DOOR + THE RESULT ENVELOPE
//=============================================================================

// The default hostname set is the local stack's front door. If a hostname is
// added to deploy/k8s/overlays/local, it belongs here too.
func TestHostsEntriesDefaultHostnamesAreTheFrontDoor(t *testing.T) {
	path := hostsFixture(t, "127.0.0.1 localhost\n")
	env, code, out := hostsRun(t, "--action=add", "--hosts-file="+path, "--confirm="+hostsConfirmAdd)
	if code != 0 || !env.OK {
		t.Fatalf("add failed (exit %d): %s", code, out)
	}
	got := hostsRead(t, path)
	for _, h := range []string{"cockpit.local.znas.io", "identity.local.znas.io", "local.znas.io"} {
		if !strings.Contains(got, "127.0.0.1 "+h+"\n") {
			t.Errorf("front-door hostname %q missing from:\n%q", h, got)
		}
	}
	if env.Capability != "install.hostsEntries" {
		t.Errorf("capability %q, want install.hostsEntries", env.Capability)
	}
	if env.Result["hostsFile"] != path {
		t.Errorf("result.hostsFile = %v, want %q", env.Result["hostsFile"], path)
	}
	if env.Result["action"] != "add" {
		t.Errorf("result.action = %v, want add", env.Result["action"])
	}
	if env.Result["blockPresent"] != true {
		t.Errorf("result.blockPresent = %v, want true", env.Result["blockPresent"])
	}
}

func TestHostsEntriesCustomIPAndHostnames(t *testing.T) {
	path := hostsFixture(t, "")
	env, code, out := hostsRun(t, "--action=add", "--hosts-file="+path,
		"--ip=10.1.2.3", "--hostnames=a.example.test b.example.test", "--confirm="+hostsConfirmAdd)
	if code != 0 || !env.OK {
		t.Fatalf("add failed (exit %d): %s", code, out)
	}
	got := hostsRead(t, path)
	for _, want := range []string{"10.1.2.3 a.example.test\n", "10.1.2.3 b.example.test\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing entry %q in:\n%q", want, got)
		}
	}
	if strings.Contains(got, "cockpit.local.znas.io") {
		t.Errorf("default hostnames leaked into a custom run:\n%q", got)
	}
}

//=============================================================================
// SAFETY RAILS ON THE TESTS THEMSELVES
//=============================================================================

// The default is /etc/hosts, so a test that forgets --hosts-file would edit
// the machine running the suite. Assert (a) the script really does default to
// /etc/hosts, and (b) no test in this file ever names it.
func TestHostsEntriesDefaultsToEtcHosts(t *testing.T) {
	b, err := os.ReadFile(hostsScript(t))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	if !strings.Contains(string(b), `DEFAULT_HOSTS_FILE="/etc/hosts"`) {
		t.Errorf("script must declare readonly DEFAULT_HOSTS_FILE as the system hosts path")
	}
}

func TestHostsEntriesTestsNeverNameRealHostsFile(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(hostsRepoRoot(t), "scripts", "install", "hosts_entries_test.go"))
	if err != nil {
		t.Fatalf("read test file: %v", err)
	}
	// The quoted literal appears exactly once by design: in the
	// default-declaration assertion above. Any other occurrence is a test that
	// may be editing the runner's real hosts file.
	// The needle is assembled at runtime so this guard does not count itself.
	needle := strconv.Quote("/etc" + "/hosts")
	if n := strings.Count(string(b), needle); n != 1 {
		t.Errorf("the quoted system-hosts literal appears %d times in this test file, want 1 "+
			"(only the default-declaration assertion) -- a test must never target the "+
			"runner's real hosts file", n)
	}
}
