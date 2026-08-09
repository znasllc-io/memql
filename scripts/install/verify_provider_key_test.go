// Tests for scripts/install/verify-provider-key.sh (capability
// install.verifyProviderKey, znasllc-io/memql#3364).
//
// The capability answers one question the installer cannot answer any other
// way: "is this AI-provider key actually good?" It answers it with a REAL
// authenticated call -- GET /v1/models -- which is authenticated, cheap, and
// spends no tokens.
//
// The assertion that matters, and the reason this test file exists, is the
// KEY HANDLING: an API key passed on a curl command line is visible in `ps`
// to every user on the box for the lifetime of the request. The script must
// therefore hand the key to curl through a 0600 config file and never through
// argv -- and it must not even ACCEPT a `--key=` flag, because that would put
// the key in the *script's* argv instead. The second assertion that matters is
// the exit code: a server that says "this key is bad" is a REFUSAL (exit 3),
// not an operational failure (exit 5); the installer branches on the
// difference.
//
// Hermetic: no call ever leaves the machine. Behavioural cases point
// --base-url at an httptest server; the argv/config-file cases run a stub
// `curl` on a PATH prefix that records its own argv and copies the config
// file it was handed.
package install

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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

// vpkKeyFile writes a key into a throwaway file and returns its path.
func vpkKeyFile(t *testing.T, key string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "provider.key")
	if err := os.WriteFile(p, []byte(key+"\n"), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}
	return p
}

// -----------------------------------------------------------------------
// Capability surface
// -----------------------------------------------------------------------

// TestVerifyProviderKeySpecHasNoKeyFlag is the structural half of the
// argv-exposure guarantee: the script must not declare a `--key` parameter at
// all. cap_parse_flags rejects undeclared flags, so *not declaring* `key` is
// what makes `--key=sk-...` impossible rather than merely discouraged.
func TestVerifyProviderKeySpecHasNoKeyFlag(t *testing.T) {
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
	for _, want := range []string{"provider", "key-file", "base-url"} {
		if !names[want] {
			t.Errorf("spec is missing the %q param; got %v", want, names)
		}
	}
	if names["key"] {
		t.Error("spec declares a `key` param -- the key must be read from a file, " +
			"never from a flag, because the script's own argv is world-readable in `ps`")
	}
}

// TestVerifyProviderKeyRejectsKeyFlag is the behavioural half: passing the key
// inline must be rejected outright (exit 2) rather than silently ignored.
func TestVerifyProviderKeyRejectsKeyFlag(t *testing.T) {
	stdout, _, code := vpkRun(t, nil, "--provider=openai", "--key=sk-inline-secret")
	if code != 2 {
		t.Fatalf("--key= exited %d, want 2 (bad param)\nstdout: %s", code, stdout)
	}
	env := vpkParse(t, stdout)
	if env.OK || env.Error == nil || env.Error.Code != 2 {
		t.Errorf("want ok=false error.code=2, got: %s", stdout)
	}
	if strings.Contains(stdout, "sk-inline-secret") {
		t.Errorf("the rejected key value was echoed back into the envelope: %s", stdout)
	}
}

// -----------------------------------------------------------------------
// THE assertion: the key never reaches curl's argv
// -----------------------------------------------------------------------

// vpkStubCurl installs a stub `curl` on a PATH prefix. The stub records its
// own argv, copies the --config file it was handed (the script deletes it on
// exit, so it must be captured mid-run) along with that file's mode, and
// replies with STUB_CODE.
func vpkStubCurl(t *testing.T) (binDir, argvFile, cfgCopy, cfgMode string) {
	t.Helper()
	binDir = t.TempDir()
	capture := t.TempDir()
	argvFile = filepath.Join(capture, "argv")
	cfgCopy = filepath.Join(capture, "config")
	cfgMode = filepath.Join(capture, "mode")

	stub := `#!/usr/bin/env bash
: > "$STUB_ARGV"
for a in "$@"; do printf '%s\n' "$a" >> "$STUB_ARGV"; done
cfg=""; out=""; prev=""
for a in "$@"; do
  case "$prev" in
    --config|-K) cfg="$a" ;;
    -o|--output) out="$a" ;;
  esac
  prev="$a"
done
if [[ -n "$cfg" && -f "$cfg" ]]; then
  cp "$cfg" "$STUB_CFG"
  { stat -c '%a' "$cfg" 2>/dev/null || stat -f '%Lp' "$cfg" 2>/dev/null; } > "$STUB_CFG_MODE"
fi
[[ -n "$out" ]] && printf '%s' "${STUB_BODY:-{\"data\":[]}}" > "$out"
printf '%s' "${STUB_CODE:-200}"
`
	p := filepath.Join(binDir, "curl")
	if err := os.WriteFile(p, []byte(stub), 0o755); err != nil {
		t.Fatalf("write stub curl: %v", err)
	}
	return binDir, argvFile, cfgCopy, cfgMode
}

func TestVerifyProviderKeyNeverPutsKeyInCurlArgv(t *testing.T) {
	const secret = "sk-ant-super-secret-argv-probe"
	binDir, argvFile, cfgCopy, cfgMode := vpkStubCurl(t)
	env := []string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"STUB_ARGV=" + argvFile,
		"STUB_CFG=" + cfgCopy,
		"STUB_CFG_MODE=" + cfgMode,
		"STUB_CODE=200",
	}
	stdout, stderr, code := vpkRun(t, env,
		"--provider=anthropic",
		"--key-file="+vpkKeyFile(t, secret),
		"--base-url=https://api.example.invalid",
	)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("stub curl was never invoked: %v", err)
	}
	if strings.Contains(string(argv), secret) {
		t.Errorf("THE KEY IS IN CURL'S ARGV -- visible in `ps` to every user on the box.\nargv:\n%s", argv)
	}

	cfg, err := os.ReadFile(cfgCopy)
	if err != nil {
		t.Fatalf("no curl --config file was handed to curl: %v\nargv:\n%s", err, argv)
	}
	if !strings.Contains(string(cfg), secret) {
		t.Errorf("the config file does not carry the key -- how was the call authenticated?\nconfig:\n%s", cfg)
	}
	mode, err := os.ReadFile(cfgMode)
	if err != nil {
		t.Fatalf("read config mode: %v", err)
	}
	if got := strings.TrimSpace(string(mode)); got != "600" {
		t.Errorf("curl config file mode = %q, want 600 -- the key sits in that file", got)
	}

	// Nothing on stdout or stderr may echo the key either.
	if strings.Contains(stdout, secret) || strings.Contains(stderr, secret) {
		t.Error("the key leaked into the script's own output")
	}
	// Verification is a read; it never mutates anything.
	res := vpkParse(t, stdout)
	if res.Changed {
		t.Error("changed=true for a read-only verification")
	}
}

// TestVerifyProviderKeyConfigFileIsCleanedUp asserts the 0600 file holding the
// key does not survive the run.
func TestVerifyProviderKeyConfigFileIsCleanedUp(t *testing.T) {
	binDir, argvFile, cfgCopy, cfgMode := vpkStubCurl(t)
	env := []string{
		"PATH=" + binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"STUB_ARGV=" + argvFile,
		"STUB_CFG=" + cfgCopy,
		"STUB_CFG_MODE=" + cfgMode,
		"STUB_CODE=200",
	}
	if _, _, code := vpkRun(t, env, "--provider=openai",
		"--key-file="+vpkKeyFile(t, "sk-cleanup"), "--base-url=https://api.example.invalid"); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatalf("stub curl never ran: %v", err)
	}
	var cfgPath string
	lines := strings.Split(strings.TrimSpace(string(argv)), "\n")
	for i, l := range lines {
		if (l == "--config" || l == "-K") && i+1 < len(lines) {
			cfgPath = lines[i+1]
		}
	}
	if cfgPath == "" {
		t.Fatalf("no --config in argv:\n%s", argv)
	}
	if _, err := os.Stat(cfgPath); err == nil {
		t.Errorf("the curl config file holding the key still exists after the run: %s", cfgPath)
	}
}

// -----------------------------------------------------------------------
// Behaviour against a real HTTP server
// -----------------------------------------------------------------------

// vpkServer starts an httptest server that records the last request and
// answers /v1/models with the configured status.
func vpkServer(t *testing.T, status int, body string) (url string, seen *http.Header, path *string) {
	t.Helper()
	var h http.Header
	var p string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h = r.Header.Clone()
		p = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &h, &p
}

func vpkRequireCurl(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available")
	}
}

// TestVerifyProviderKeyAcceptsGoodKey: a 200 from GET /v1/models is the
// success signal, and the request must actually hit /v1/models (the endpoint
// that authenticates without spending tokens).
func TestVerifyProviderKeyAcceptsGoodKey(t *testing.T) {
	vpkRequireCurl(t)
	for _, provider := range []string{"anthropic", "openai"} {
		t.Run(provider, func(t *testing.T) {
			url, hdr, path := vpkServer(t, 200, `{"data":[{"id":"m"}]}`)
			stdout, stderr, code := vpkRun(t, nil,
				"--provider="+provider,
				"--key-file="+vpkKeyFile(t, "sk-good-"+provider),
				"--base-url="+url,
			)
			if code != 0 {
				t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
			}
			env := vpkParse(t, stdout)
			if !env.OK {
				t.Errorf("ok=false for a 200 response: %s", stdout)
			}
			if v := vpkResultField(t, env, "valid"); v != true {
				t.Errorf("result.valid = %v, want true", v)
			}
			if v := vpkResultField(t, env, "httpStatus"); v != float64(200) {
				t.Errorf("result.httpStatus = %v, want 200", v)
			}
			if *path != "/v1/models" {
				t.Errorf("hit %q, want /v1/models -- the token-free authenticated probe", *path)
			}
			switch provider {
			case "anthropic":
				if got := hdr.Get("x-api-key"); got != "sk-good-anthropic" {
					t.Errorf("x-api-key = %q, want the key", got)
				}
				if hdr.Get("anthropic-version") == "" {
					t.Error("anthropic-version header missing -- the API rejects requests without it")
				}
			case "openai":
				if got := hdr.Get("Authorization"); got != "Bearer sk-good-openai" {
					t.Errorf("Authorization = %q, want Bearer <key>", got)
				}
			}
		})
	}
}

// TestVerifyProviderKeyRejectedKeyIsRefusal is the exit-code assertion: the
// server said the credential is bad. That is a refusal (3), not an operational
// failure (5) -- the installer re-prompts on 3 and reports an outage on 5.
func TestVerifyProviderKeyRejectedKeyIsRefusal(t *testing.T) {
	vpkRequireCurl(t)
	for _, status := range []int{401, 403} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			url, _, _ := vpkServer(t, status, `{"error":{"message":"invalid x-api-key"}}`)
			stdout, _, code := vpkRun(t, nil,
				"--provider=anthropic",
				"--key-file="+vpkKeyFile(t, "sk-bad"),
				"--base-url="+url,
			)
			if code != 3 {
				t.Fatalf("HTTP %d exited %d, want 3 (refused)\nstdout: %s", status, code, stdout)
			}
			env := vpkParse(t, stdout)
			if env.OK || env.Error == nil || env.Error.Code != 3 {
				t.Errorf("want ok=false error.code=3, got: %s", stdout)
			}
			if v := vpkResultField(t, env, "valid"); v != false {
				t.Errorf("result.valid = %v, want false", v)
			}
			if v := vpkResultField(t, env, "httpStatus"); v != float64(status) {
				t.Errorf("result.httpStatus = %v, want %d", v, status)
			}
		})
	}
}

// TestVerifyProviderKeyServerFaultIsOperationalFailure: a 500 says nothing
// about the key, so it must NOT be reported as a refusal.
func TestVerifyProviderKeyServerFaultIsOperationalFailure(t *testing.T) {
	vpkRequireCurl(t)
	url, _, _ := vpkServer(t, 500, `{"error":"boom"}`)
	stdout, _, code := vpkRun(t, nil,
		"--provider=openai",
		"--key-file="+vpkKeyFile(t, "sk-whatever"),
		"--base-url="+url,
	)
	if code != 5 {
		t.Fatalf("HTTP 500 exited %d, want 5 (operation failed)\nstdout: %s", code, stdout)
	}
	env := vpkParse(t, stdout)
	if env.Error == nil || env.Error.Code != 5 {
		t.Errorf("want error.code=5, got: %s", stdout)
	}
}

// TestVerifyProviderKeyUnreachableIsOperationalFailure: no server at all is
// also not the key's fault.
func TestVerifyProviderKeyUnreachableIsOperationalFailure(t *testing.T) {
	vpkRequireCurl(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead := srv.URL
	srv.Close() // nothing is listening on that port now

	stdout, _, code := vpkRun(t, nil,
		"--provider=openai",
		"--key-file="+vpkKeyFile(t, "sk-whatever"),
		"--base-url="+dead,
		"--timeout=3",
	)
	if code != 5 {
		t.Fatalf("unreachable base-url exited %d, want 5\nstdout: %s", code, stdout)
	}
}

// TestVerifyProviderKeyBadParams covers the invocation errors that must be
// exit 2 and must never reach the network.
func TestVerifyProviderKeyBadParams(t *testing.T) {
	good := vpkKeyFile(t, "sk-good")
	empty := filepath.Join(t.TempDir(), "empty.key")
	if err := os.WriteFile(empty, []byte("\n   \n"), 0o600); err != nil {
		t.Fatalf("write empty key file: %v", err)
	}

	cases := []struct {
		name string
		args []string
	}{
		{"no provider", []string{"--key-file=" + good}},
		{"unknown provider", []string{"--provider=hal9000", "--key-file=" + good}},
		{"no key file", []string{"--provider=openai"}},
		{"key file missing on disk", []string{"--provider=openai", "--key-file=/nope/nowhere.key"}},
		{"key file is empty", []string{"--provider=openai", "--key-file=" + empty}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, _, code := vpkRun(t, nil, tc.args...)
			if code != 2 {
				t.Fatalf("exit %d, want 2 (bad param)\nstdout: %s", code, stdout)
			}
			env := vpkParse(t, stdout)
			if env.OK || env.Error == nil || env.Error.Code != 2 {
				t.Errorf("want ok=false error.code=2, got: %s", stdout)
			}
		})
	}
}

// TestVerifyProviderKeyMissingCurlIsPrerequisite: no curl means the capability
// cannot run at all -- exit 4, distinct from "the key is bad".
func TestVerifyProviderKeyMissingCurlIsPrerequisite(t *testing.T) {
	binDir := vpkSanitizedBin(t, nil)
	stdout, _, code := vpkRun(t, []string{"PATH=" + binDir},
		"--provider=openai", "--key-file="+vpkKeyFile(t, "sk-x"))
	if code != 4 {
		t.Fatalf("exit %d, want 4 (prerequisite missing)\nstdout: %s", code, stdout)
	}
}

// vpkSanitizedBin builds a bin directory holding ONLY the shell utilities the
// capability library needs plus the named extras, so a test can prove what
// happens when a tool is genuinely absent (the runner's real PATH has curl,
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
