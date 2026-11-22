// Tests for scripts/install/verify-provider-key.sh (capability
// install.verifyProviderKey, znasllc-io/memql#3364, rewritten for
// epic memql#5088).
//
// WHAT THE CAPABILITY ANSWERS NOW. It used to answer "is this AI-provider key
// actually good?" with an authenticated GET /v1/models. There is no key any
// more: both vendors are reached by workload identity federation, and the
// credential is a projected Kubernetes token that exists only inside a pod. So
// the question became "is this CLUSTER's credential good?", and the only thing
// that can answer it is the pod holding the token --
// `kubectl exec <deploy> -- memql provider-auth check --provider=<vendor>`.
//
// The assertions that matter are therefore:
//
//  1. THE KEY SURFACE IS GONE AND CANNOT COME BACK. `--key-file` and
//     `--base-url` are undeclared, so cap_parse_flags refuses them (exit 2)
//     rather than ignoring them. A silently ignored `--key-file` would let a
//     caller believe a key was verified when nothing of the sort happened.
//  2. THE THREE-WAY EXIT CODE. A vendor that RAN the check and said no is a
//     REFUSAL (3); a check that could not be RUN is an operational failure
//     (5); no kubectl at all is a missing prerequisite (4). The installer
//     branches on the difference, and `kubectl exec` collapses 3 and 5 into
//     "non-zero" unless they are told apart deliberately.
//  3. THE VENDOR REACHES THE POD. `--provider` must appear in the exec'd
//     command, or an OpenAI check would silently verify Anthropic.
//
// Hermetic: no call ever leaves the machine and no cluster is contacted. Every
// behavioural case runs a stub `kubectl` on a PATH prefix that records its own
// argv and exits with a configured status.
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

// vpkEnvelope is the capability result envelope (the contract's result schema).
type vpkEnvelope struct {
	OK         bool            `json:"ok"`
	Capability string          `json:"capability"`
	Changed    bool            `json:"changed"`
	Result     json.RawMessage `json:"result"`
	Error      *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// vpkScript resolves the absolute path of the script under test.
func vpkScript(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	p := filepath.Join(filepath.Dir(thisFile), "verify-provider-key.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("verify-provider-key.sh not found at %s: %v", p, err)
	}
	return p
}

// vpkRun executes the script with stdin closed, keeping stdout and stderr
// SEPARATE -- the contract says stdout carries exactly one JSON envelope and
// nothing else, so the tests parse stdout directly.
func vpkRun(t *testing.T, extraEnv []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	cmd := exec.Command("bash", append([]string{vpkScript(t)}, args...)...)
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Stdin = nil
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	code = 0
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

// vpkParse parses the single JSON envelope the script writes to stdout.
func vpkParse(t *testing.T, stdout string) vpkEnvelope {
	t.Helper()
	line := strings.TrimSpace(stdout)
	if line == "" {
		t.Fatalf("no envelope on stdout")
	}
	if strings.Contains(line, "\n") {
		t.Fatalf("stdout carried more than one line -- human logs belong on stderr:\n%s", line)
	}
	var env vpkEnvelope
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		t.Fatalf("stdout is not a JSON envelope: %v\nstdout: %s", err, line)
	}
	if env.Capability != "install.verifyProviderKey" {
		t.Errorf("capability = %q, want install.verifyProviderKey", env.Capability)
	}
	return env
}

// vpkResultField pulls one field out of the envelope's result object.
func vpkResultField(t *testing.T, env vpkEnvelope, key string) any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(env.Result, &m); err != nil {
		t.Fatalf("result is not an object: %v (%s)", err, env.Result)
	}
	return m[key]
}

// -----------------------------------------------------------------------
// Capability surface
// -----------------------------------------------------------------------

// TestVerifyProviderSpecIsFederationOnly pins the declared parameter surface.
//
// The absent names are the load-bearing half. cap_parse_flags rejects any flag
// the script did not declare, so NOT declaring `key`, `key-file` or `base-url`
// is what makes a key impossible to pass rather than merely pointless -- and
// --print-spec is the surface a caller reads to discover that without running
// anything.
func TestVerifyProviderSpecIsFederationOnly(t *testing.T) {
	stdout, _, code := vpkRun(t, nil, "--print-spec")
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
	if spec.Capability != "install.verifyProviderKey" {
		t.Errorf("capability = %q, want install.verifyProviderKey", spec.Capability)
	}
	names := map[string]bool{}
	for _, p := range spec.Params {
		names[p.Name] = true
	}
	for _, want := range []string{"provider", "federation-deploy", "namespace", "memql-binary"} {
		if !names[want] {
			t.Errorf("spec is missing the %q param; got %v", want, names)
		}
	}
	for _, gone := range []string{"key", "key-file", "base-url", "anthropic-version"} {
		if names[gone] {
			t.Errorf("spec still declares %q -- there is no vendor API key anywhere in the product, "+
				"and a key path that still parses is a key path that can be reintroduced", gone)
		}
	}
}

// TestVerifyProviderRejectsEveryKeyFlag is the behavioural half: the retired
// key flags must be REFUSED (exit 2), never quietly ignored. An ignored
// --key-file would let an installer report a verified key having verified
// nothing.
func TestVerifyProviderRejectsEveryKeyFlag(t *testing.T) {
	for _, flag := range []string{"--key=sk-inline-secret", "--key-file=/tmp/nope.key", "--base-url=http://127.0.0.1:1"} {
		t.Run(flag, func(t *testing.T) {
			stdout, _, code := vpkRun(t, nil, "--provider=openai", "--federation-deploy=agent", flag)
			if code != 2 {
				t.Fatalf("%s exited %d, want 2 (bad param)\nstdout: %s", flag, code, stdout)
			}
			env := vpkParse(t, stdout)
			if env.OK || env.Error == nil || env.Error.Code != 2 {
				t.Errorf("want ok=false error.code=2, got: %s", stdout)
			}
			if strings.Contains(stdout, "sk-inline-secret") {
				t.Errorf("the rejected value was echoed back into the envelope: %s", stdout)
			}
		})
	}
}

// -----------------------------------------------------------------------
// The federation probe
// -----------------------------------------------------------------------

// vpkStubKubectl installs a stub `kubectl` on a PATH prefix. It records its own
// argv and exits with STUB_RC, printing STUB_OUT.
func vpkStubKubectl(t *testing.T) (binDir, argvFile string) {
	t.Helper()
	binDir = t.TempDir()
	argvFile = filepath.Join(t.TempDir(), "argv")

	stub := `#!/usr/bin/env bash
: > "$STUB_ARGV"
for a in "$@"; do printf '%s\n' "$a" >> "$STUB_ARGV"; done
printf '%s\n' "${STUB_OUT:-provider-auth check: ok}"
exit "${STUB_RC:-0}"
`
	p := filepath.Join(binDir, "kubectl")
	if err := os.WriteFile(p, []byte(stub), 0o755); err != nil {
		t.Fatalf("write stub kubectl: %v", err)
	}
	return binDir, argvFile
}

func vpkStubEnv(binDir, argvFile, rc, out string) []string {
	return []string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"STUB_ARGV=" + argvFile,
		"STUB_RC=" + rc,
		"STUB_OUT=" + out,
	}
}

// TestVerifyProviderFederationAcceptedIsSuccess covers the happy path for both
// vendors, and asserts the one wire detail that cannot be checked any other
// way: the vendor reaches the pod as --provider. Without it an OpenAI check
// would run the binary's default and report Anthropic's answer under OpenAI's
// name -- green, and about the wrong vendor.
func TestVerifyProviderFederationAcceptedIsSuccess(t *testing.T) {
	for _, provider := range []string{"anthropic", "openai"} {
		t.Run(provider, func(t *testing.T) {
			binDir, argvFile := vpkStubKubectl(t)
			stdout, stderr, code := vpkRun(t, vpkStubEnv(binDir, argvFile, "0", "exchange: ok"),
				"--provider="+provider,
				"--federation-deploy=agent",
				"--namespace=memql",
			)
			if code != 0 {
				t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
			}
			env := vpkParse(t, stdout)
			if !env.OK {
				t.Errorf("ok=false for an accepted credential: %s", stdout)
			}
			if v := vpkResultField(t, env, "valid"); v != true {
				t.Errorf("result.valid = %v, want true", v)
			}
			if v := vpkResultField(t, env, "credential"); v != "federation" {
				t.Errorf("result.credential = %v, want \"federation\"", v)
			}
			if v := vpkResultField(t, env, "provider"); v != provider {
				t.Errorf("result.provider = %v, want %q", v, provider)
			}
			if v := vpkResultField(t, env, "deployment"); v != "memql/agent" {
				t.Errorf("result.deployment = %v, want \"memql/agent\"", v)
			}
			// Verification is a read; it never mutates anything.
			if env.Changed {
				t.Error("changed=true for a read-only verification")
			}

			argv, err := os.ReadFile(argvFile)
			if err != nil {
				t.Fatalf("stub kubectl was never invoked: %v", err)
			}
			got := string(argv)
			for _, want := range []string{"exec", "-n", "memql", "deploy/agent", "provider-auth", "check", "--provider=" + provider} {
				if !strings.Contains(got, want) {
					t.Errorf("kubectl argv is missing %q:\n%s", want, got)
				}
			}
		})
	}
}

// TestVerifyProviderFederationRefusedIsRefusal: the pod RAN the check and the
// vendor said no. `provider-auth check` uses 1 for that, and it must surface as
// exit 3 -- the installer re-does the console steps on 3 and reports an outage
// on 5.
func TestVerifyProviderFederationRefusedIsRefusal(t *testing.T) {
	binDir, argvFile := vpkStubKubectl(t)
	stdout, _, code := vpkRun(t, vpkStubEnv(binDir, argvFile, "1", "exchange: DENIED match_audience"),
		"--provider=openai", "--federation-deploy=agent")
	if code != 3 {
		t.Fatalf("a refused exchange exited %d, want 3\nstdout: %s", code, stdout)
	}
	env := vpkParse(t, stdout)
	if env.OK || env.Error == nil || env.Error.Code != 3 {
		t.Errorf("want ok=false error.code=3, got: %s", stdout)
	}
	if v := vpkResultField(t, env, "valid"); v != false {
		t.Errorf("result.valid = %v, want false", v)
	}
	// The vendor's own words are the whole value of the check; dropping them
	// leaves the operator with a bare exit code and nothing to act on.
	detail, _ := vpkResultField(t, env, "detail").(string)
	if !strings.Contains(detail, "match_audience") {
		t.Errorf("result.detail = %q, want it to carry what the check actually said", detail)
	}
}

// TestVerifyProviderFederationUnrunnableIsOperationalFailure: anything other
// than 0 or 1 means the check never ran (no such Deployment, no cluster, exec
// refused), which says nothing about the credential.
func TestVerifyProviderFederationUnrunnableIsOperationalFailure(t *testing.T) {
	binDir, argvFile := vpkStubKubectl(t)
	stdout, _, code := vpkRun(t, vpkStubEnv(binDir, argvFile, "127", "Error from server (NotFound): deployments.apps agent not found"),
		"--provider=anthropic", "--federation-deploy=agent")
	if code != 5 {
		t.Fatalf("an unrunnable check exited %d, want 5\nstdout: %s", code, stdout)
	}
	env := vpkParse(t, stdout)
	if env.Error == nil || env.Error.Code != 5 {
		t.Errorf("want error.code=5, got: %s", stdout)
	}
}

// TestVerifyProviderNotVerifiedYetIsNotARefusal: the check RAN and could not
// say. The engine in the pod has recorded no sign-in yet, or is waiting for its
// next identity token. `provider-auth check` exits 4 for that. It must not
// surface as 3: an installer told "refused" re-does the console steps for a
// node that only needed another moment.
func TestVerifyProviderNotVerifiedYetIsNotARefusal(t *testing.T) {
	binDir, argvFile := vpkStubKubectl(t)
	stdout, _, code := vpkRun(t, vpkStubEnv(binDir, argvFile, "4", "provider-auth check: NOT VERIFIED YET: the engine in this pod has recorded no anthropic sign-in"),
		"--provider=anthropic", "--federation-deploy=agent")
	if code != 5 {
		t.Fatalf("an unverified sign-in exited %d, want 5\nstdout: %s", code, stdout)
	}
	env := vpkParse(t, stdout)
	if env.Error == nil || env.Error.Code != 5 {
		t.Fatalf("want error.code=5, got: %s", stdout)
	}
	if !strings.Contains(env.Error.Message, "not verified") {
		t.Errorf("the message reads as a failure to run the check, not as an open question: %q", env.Error.Message)
	}
	if strings.Contains(env.Error.Message, "could not run") {
		t.Errorf("the check DID run; the message must not say it could not: %q", env.Error.Message)
	}
	if v := vpkResultField(t, env, "valid"); v != false {
		t.Errorf("result.valid = %v, want false", v)
	}
}

// TestVerifyProviderNotSetUpIsNeverSuccess: the check RAN and the engine in the
// pod said it holds no account at the vendor. `provider-auth check` exits 3 for
// that (it used to exit 0, which this script read as "Federation verified" for
// a pod with no ids at all). The installer runs this step only after it has
// written federation ids, so a pod that holds none is a configuration that did
// not arrive: exit 3, with words that say so, and never a failure to run.
func TestVerifyProviderNotSetUpIsNeverSuccess(t *testing.T) {
	binDir, argvFile := vpkStubKubectl(t)
	stdout, _, code := vpkRun(t, vpkStubEnv(binDir, argvFile, "3",
		"provider-auth check: NOT SET UP: the vendor is not set up on this node: no federation ids are set for this vendor on this node"),
		"--provider=anthropic", "--federation-deploy=agent")
	if code != 3 {
		t.Fatalf("a pod with no federation ids exited %d, want 3\nstdout: %s", code, stdout)
	}
	env := vpkParse(t, stdout)
	if env.OK || env.Error == nil || env.Error.Code != 3 {
		t.Fatalf("want ok=false error.code=3, got: %s", stdout)
	}
	if !strings.Contains(env.Error.Message, "not set up") {
		t.Errorf("the message does not say the vendor is not set up in that pod: %q", env.Error.Message)
	}
	for _, wrong := range []string{"could not run", "did not accept", "was refused"} {
		if strings.Contains(env.Error.Message, wrong) {
			t.Errorf("the message says %q, which is not what happened: %q", wrong, env.Error.Message)
		}
	}
	if v := vpkResultField(t, env, "valid"); v != false {
		t.Errorf("result.valid = %v, want false", v)
	}
}

// TestVerifyProviderBadParams covers the invocation errors that must be exit 2
// and must never reach a cluster.
func TestVerifyProviderBadParams(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no provider", []string{"--federation-deploy=agent"}},
		{"unknown provider", []string{"--provider=hal9000", "--federation-deploy=agent"}},
		{"no federation deploy", []string{"--provider=openai"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binDir, argvFile := vpkStubKubectl(t)
			stdout, _, code := vpkRun(t, vpkStubEnv(binDir, argvFile, "0", "unused"), tc.args...)
			if code != 2 {
				t.Fatalf("exit %d, want 2 (bad param)\nstdout: %s", code, stdout)
			}
			env := vpkParse(t, stdout)
			if env.OK || env.Error == nil || env.Error.Code != 2 {
				t.Errorf("want ok=false error.code=2, got: %s", stdout)
			}
			if _, err := os.Stat(argvFile); err == nil {
				t.Error("a bad param reached kubectl; the check must be refused before anything runs")
			}
		})
	}
}

// TestVerifyProviderMissingKubectlIsPrerequisite: no kubectl means the
// capability cannot run at all -- exit 4, distinct from "the credential is
// bad".
func TestVerifyProviderMissingKubectlIsPrerequisite(t *testing.T) {
	binDir := vpkSanitizedBin(t, nil)
	stdout, _, code := vpkRun(t, []string{"PATH=" + binDir},
		"--provider=openai", "--federation-deploy=agent")
	if code != 4 {
		t.Fatalf("exit %d, want 4 (prerequisite missing)\nstdout: %s", code, stdout)
	}
}

// vpkSanitizedBin builds a bin directory holding ONLY the shell utilities the
// capability library needs plus the named extras, so a test can prove what
// happens when a tool is genuinely absent (the runner's real PATH has kubectl,
// docker, mkcert and friends installed).
func vpkSanitizedBin(t *testing.T, extras []string) string {
	t.Helper()
	dir := t.TempDir()
	base := []string{"bash", "tr", "grep", "sed", "cat", "head", "tail", "mktemp",
		"chmod", "rm", "awk", "cut", "sort", "printf", "stat", "cp", "mkdir", "dirname"}
	for _, name := range append(base, extras...) {
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
