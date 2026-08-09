// Tests for scripts/install/magic-link.sh (capability install.magicLink,
// znasllc-io/memql#3366).
//
// A freshly installed cluster has no way in until the cluster owner claims
// ownership, and that claim arrives as a magic link. On a local cluster there
// is no mailbox: identity's dev-mode escape hatch logs the link at INFO
// (component/identity/emailsender/sender.go), so the installer's only route to
// a first login is to read it back out of the identity pod's logs.
//
// The two assertions this file exists for:
//
//  1. --local IS MANDATORY (exit 3 without it). kubectl talks to whatever
//     context was last used -- possibly staging, possibly production. Scraping
//     an authentication credential out of pod logs must be a decision the
//     operator states out loud, not something that happens because a kubeconfig
//     was pointing somewhere. The script additionally PINS --context rather
//     than inheriting the ambient one, so the affirmation is mechanically
//     backed instead of merely asserted.
//
//  2. THE LAST LINK WINS. Magic links are single-use. A pod that restarted, or
//     an operator who clicked once already, leaves spent links earlier in the
//     log. Handing back the first match hands back a link that is guaranteed
//     not to work.
//
// Hermetic: every case drives a stub `kubectl` on a PATH prefix. No real
// cluster is contacted, and no kubeconfig on the runner is read.
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

type mlEnvelope struct {
	OK         bool            `json:"ok"`
	Capability string          `json:"capability"`
	Changed    bool            `json:"changed"`
	Result     json.RawMessage `json:"result"`
	Error      *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type mlResult struct {
	Link       string `json:"link"`
	Namespace  string `json:"namespace"`
	Target     string `json:"target"`
	Context    string `json:"context"`
	Candidates int    `json:"candidates"`
}

func mlScript(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	p := filepath.Join(filepath.Dir(thisFile), "magic-link.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("magic-link.sh not found at %s: %v", p, err)
	}
	return p
}

func mlRun(t *testing.T, extraEnv []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	cmd := exec.Command("bash", append([]string{mlScript(t)}, args...)...)
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

func mlParse(t *testing.T, stdout string) (mlEnvelope, mlResult) {
	t.Helper()
	line := strings.TrimSpace(stdout)
	if line == "" {
		t.Fatal("no envelope on stdout")
	}
	if strings.Contains(line, "\n") {
		t.Fatalf("stdout carried more than one line -- human logs belong on stderr:\n%s", line)
	}
	var env mlEnvelope
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		t.Fatalf("stdout is not a JSON envelope: %v\n%s", err, line)
	}
	if env.Capability != "install.magicLink" {
		t.Errorf("capability = %q, want install.magicLink", env.Capability)
	}
	if env.Changed {
		t.Error("changed=true for a read-only log scrape")
	}
	var res mlResult
	if err := json.Unmarshal(env.Result, &res); err != nil {
		t.Fatalf("result is not the expected object: %v\n%s", err, env.Result)
	}
	return env, res
}

// -----------------------------------------------------------------------
// Stub kubectl
// -----------------------------------------------------------------------

// mlStubKubectl installs a stub `kubectl` that records its argv and replays a
// canned log stream. It returns the env prefix and the argv-capture path (the
// file does NOT exist until the stub actually runs, which is how a test proves
// kubectl was never reached).
func mlStubKubectl(t *testing.T, logs string, rc string) (env []string, argvFile string) {
	t.Helper()
	dir := t.TempDir()
	capture := t.TempDir()
	argvFile = filepath.Join(capture, "argv")
	logFile := filepath.Join(capture, "logs")
	if err := os.WriteFile(logFile, []byte(logs), 0o600); err != nil {
		t.Fatalf("write log fixture: %v", err)
	}
	stub := `#!/usr/bin/env bash
: > "$STUB_ARGV"
for a in "$@"; do printf '%s\n' "$a" >> "$STUB_ARGV"; done
if [[ "${STUB_KUBECTL_RC:-0}" != "0" ]]; then
  echo "Error from server (NotFound): stub failure" >&2
  exit "$STUB_KUBECTL_RC"
fi
cat "$STUB_LOGS"
`
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(stub), 0o755); err != nil {
		t.Fatalf("write stub kubectl: %v", err)
	}
	return []string{
		"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"STUB_ARGV=" + argvFile,
		"STUB_LOGS=" + logFile,
		"STUB_KUBECTL_RC=" + rc,
	}, argvFile
}

// mlLogLine renders the slog INFO record identity actually emits in dev mode.
func mlLogLine(email, token string) string {
	return `{"time":"2026-08-08T10:00:00Z","level":"INFO","msg":"identity: DEV magic link (also sent via configured email)",` +
		`"to":"` + email + `","link":"https://identity.local.znas.io/auth/complete?ml=` + token + `"}`
}

func mlArgv(t *testing.T, argvFile string) []string {
	t.Helper()
	b, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("stub kubectl was never invoked: %v", err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// -----------------------------------------------------------------------
// THE assertion: --local is mandatory
// -----------------------------------------------------------------------

// TestMagicLinkRefusesWithoutLocal: without the explicit affirmation, the
// script must refuse (3) BEFORE it shells out. kubectl points at whatever
// context was last used; reading an auth credential out of logs is not
// something to do by accident against staging.
func TestMagicLinkRefusesWithoutLocal(t *testing.T) {
	env, argvFile := mlStubKubectl(t, mlLogLine("owner@example.com", "TOK"), "0")
	stdout, _, code := mlRun(t, env)
	if code != 3 {
		t.Fatalf("exit %d, want 3 (refused)\nstdout: %s", code, stdout)
	}
	e, _ := mlParse(t, stdout)
	if e.OK || e.Error == nil || e.Error.Code != 3 {
		t.Errorf("want ok=false error.code=3, got: %s", stdout)
	}
	if !strings.Contains(strings.ToLower(e.Error.Message), "local") {
		t.Errorf("the refusal must say what is missing; got %q", e.Error.Message)
	}
	if _, err := os.Stat(argvFile); err == nil {
		t.Error("kubectl ran despite the refusal -- the gate must fire before any cluster contact")
	}
}

// TestMagicLinkPinsTheContext: the affirmation is backed mechanically. The
// script passes an explicit --context instead of inheriting whatever the
// kubeconfig currently points at.
func TestMagicLinkPinsTheContext(t *testing.T) {
	env, argvFile := mlStubKubectl(t, mlLogLine("owner@example.com", "TOK"), "0")
	stdout, stderr, code := mlRun(t, env, "--local")
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	argv := mlArgv(t, argvFile)
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--context=k3d-memql") {
		t.Errorf("kubectl was not pinned to the local context; argv: %v", argv)
	}
	if !strings.Contains(joined, "logs") {
		t.Errorf("kubectl was not asked for logs; argv: %v", argv)
	}
	_, res := mlParse(t, stdout)
	if res.Context != "k3d-memql" {
		t.Errorf("result.context = %q, want k3d-memql", res.Context)
	}
	if res.Namespace != "memql" {
		t.Errorf("result.namespace = %q, want memql", res.Namespace)
	}
	if !strings.Contains(res.Target, "identity") {
		t.Errorf("result.target = %q, want the identity workload", res.Target)
	}
}

// TestMagicLinkContextIsOverridable keeps the pin from becoming a cage for an
// operator whose local cluster is named differently.
func TestMagicLinkContextIsOverridable(t *testing.T) {
	env, argvFile := mlStubKubectl(t, mlLogLine("owner@example.com", "TOK"), "0")
	if _, _, code := mlRun(t, env, "--local", "--context=k3d-other", "--namespace=ns2"); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	joined := strings.Join(mlArgv(t, argvFile), " ")
	if !strings.Contains(joined, "--context=k3d-other") {
		t.Errorf("--context override ignored; argv: %s", joined)
	}
	if !strings.Contains(joined, "ns2") {
		t.Errorf("--namespace override ignored; argv: %s", joined)
	}
}

// -----------------------------------------------------------------------
// THE assertion: the last link wins
// -----------------------------------------------------------------------

// TestMagicLinkLastLinkWins is the whole reason this is not `grep -m1`. Magic
// links are single-use; the earlier ones in the log are spent (an operator
// already clicked, or the pod restarted mid-install). Returning anything but
// the newest hands the operator a link that cannot work.
func TestMagicLinkLastLinkWins(t *testing.T) {
	logs := strings.Join([]string{
		mlLogLine("owner@example.com", "SPENT-ONE"),
		`{"time":"2026-08-08T10:00:05Z","level":"INFO","msg":"identity: boot complete"}`,
		mlLogLine("owner@example.com", "SPENT-TWO"),
		mlLogLine("owner@example.com", "FRESHEST"),
	}, "\n") + "\n"

	env, _ := mlStubKubectl(t, logs, "0")
	stdout, stderr, code := mlRun(t, env, "--local")
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	_, res := mlParse(t, stdout)
	if !strings.Contains(res.Link, "ml=FRESHEST") {
		t.Errorf("returned %q -- the LAST link must win; the earlier ones are spent", res.Link)
	}
	if strings.Contains(res.Link, "SPENT") {
		t.Errorf("returned a spent link: %s", res.Link)
	}
	if res.Candidates != 3 {
		t.Errorf("candidates = %d, want 3 -- the operator should see how many were in the log", res.Candidates)
	}
}

// TestMagicLinkFiltersByEmail: several people may have hit /login against the
// same dev cluster. --email narrows to the one the installer cares about, and
// the last link FOR THAT ADDRESS wins.
func TestMagicLinkFiltersByEmail(t *testing.T) {
	logs := strings.Join([]string{
		mlLogLine("owner@example.com", "OWNER-OLD"),
		mlLogLine("someone@else.test", "OTHER-NEW"),
		mlLogLine("owner@example.com", "OWNER-NEW"),
		mlLogLine("someone@else.test", "OTHER-NEWEST"),
	}, "\n") + "\n"

	env, _ := mlStubKubectl(t, logs, "0")
	stdout, _, code := mlRun(t, env, "--local", "--email=owner@example.com")
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s", code, stdout)
	}
	_, res := mlParse(t, stdout)
	if !strings.Contains(res.Link, "ml=OWNER-NEW") {
		t.Errorf("returned %q, want the newest link for owner@example.com", res.Link)
	}
}

// -----------------------------------------------------------------------
// Failure modes
// -----------------------------------------------------------------------

// TestMagicLinkNoLinkInLogs: an empty log window is an operational failure
// with an actionable message, not a silent success with an empty link.
func TestMagicLinkNoLinkInLogs(t *testing.T) {
	env, _ := mlStubKubectl(t, `{"level":"INFO","msg":"identity: boot complete"}`+"\n", "0")
	stdout, _, code := mlRun(t, env, "--local")
	if code != 5 {
		t.Fatalf("exit %d, want 5 (operation failed)\nstdout: %s", code, stdout)
	}
	e, _ := mlParse(t, stdout)
	if e.OK || e.Error == nil || e.Error.Code != 5 {
		t.Errorf("want ok=false error.code=5, got: %s", stdout)
	}
}

func TestMagicLinkKubectlFailureIsOperational(t *testing.T) {
	env, _ := mlStubKubectl(t, "", "1")
	stdout, _, code := mlRun(t, env, "--local")
	if code != 5 {
		t.Fatalf("exit %d, want 5\nstdout: %s", code, stdout)
	}
}

func TestMagicLinkMissingKubectlIsPrerequisite(t *testing.T) {
	dir := mlSanitizedBin(t)
	stdout, _, code := mlRun(t, []string{"PATH=" + dir}, "--local")
	if code != 4 {
		t.Fatalf("exit %d, want 4 (prerequisite missing)\nstdout: %s", code, stdout)
	}
}

func TestMagicLinkPrintSpec(t *testing.T) {
	stdout, _, code := mlRun(t, nil, "--print-spec")
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
	if spec.Capability != "install.magicLink" {
		t.Errorf("capability = %q, want install.magicLink", spec.Capability)
	}
	names := map[string]bool{}
	for _, p := range spec.Params {
		names[p.Name] = true
	}
	for _, want := range []string{"local", "namespace", "context", "target"} {
		if !names[want] {
			t.Errorf("spec is missing the %q param; got %v", want, names)
		}
	}
}

// mlSanitizedBin builds a bin directory holding ONLY the shell utilities the
// capability library needs, so the missing-kubectl case is genuinely missing
// (the runner has kubectl on its real PATH).
func mlSanitizedBin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"bash", "tr", "grep", "sed", "cat", "head", "tail",
		"mktemp", "chmod", "rm", "awk", "cut", "sort", "wc", "printf", "mkdir", "dirname"} {
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
