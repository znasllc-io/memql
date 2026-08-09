// Tests for scripts/install/seed-bootstrap.sh (capability
// install.seedBootstrap, znasllc-io/memql#3375).
//
// THE assertion this file exists for: AN INCOMPLETE BOOTSTRAP SET IS EXIT 2.
//
// Identity auto-bootstraps only when ALL five of domain / owner-email /
// owner-first-name / owner-last-name / registration-mode are present
// (component/identity/config.go: BootstrapConfig.HasAllRequired). Miss one and
// the whole set is inert -- and nothing anywhere says so. The Secret is written,
// kubectl shows four healthy keys, the cluster comes up green, and the operator
// lands on a login page for an account that was never created. A partial seed
// looks MORE finished than no seed at all, which is exactly why it must not be
// a warning: a warning scrolls past in a hundred lines of cluster bring-up and
// the failure surfaces twenty minutes later as "I can't sign in".
//
// The second assertion: THE API KEY NEVER APPEARS IN ARGV. argv is world-
// readable -- `ps`, /proc/<pid>/cmdline, shell history, and the capability
// runner's own log of the command it ran. So the key arrives as a file path and
// reaches kubectl through --from-file. The kubectl stub here records its full
// argv, and the test greps that recording for the key material.
//
// Hermetic: kubectl is a stub on a PATH prefix that records argv and answers
// cluster-info / get namespace; nothing touches a real cluster.
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

type sbEnvelope struct {
	OK         bool            `json:"ok"`
	Capability string          `json:"capability"`
	Changed    bool            `json:"changed"`
	Result     json.RawMessage `json:"result"`
	Error      *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type sbResult struct {
	Namespace         string `json:"namespace"`
	Secret            string `json:"secret"`
	Domain            string `json:"domain"`
	OwnerEmail        string `json:"ownerEmail"`
	RegistrationMode  string `json:"registrationMode"`
	ProviderKeyEnv    string `json:"providerKeyEnv"`
	BootstrapComplete bool   `json:"bootstrapComplete"`
	ProviderKeySeeded bool   `json:"providerKeySeeded"`
	KeyCount          int    `json:"keyCount"`
	DryRun            bool   `json:"dryRun"`
}

func sbScript(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	p := filepath.Join(filepath.Dir(thisFile), "seed-bootstrap.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("seed-bootstrap.sh not found at %s: %v", p, err)
	}
	return p
}

func sbRun(t *testing.T, extraEnv []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	cmd := exec.Command("bash", append([]string{sbScript(t)}, args...)...)
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Stdin = nil
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			t.Fatalf("run: %v", err)
		}
	}
	return out.String(), errb.String(), code
}

func sbParse(t *testing.T, stdout string) (sbEnvelope, sbResult) {
	t.Helper()
	line := strings.TrimSpace(stdout)
	if line == "" {
		t.Fatal("no envelope on stdout")
	}
	if strings.Contains(line, "\n") {
		t.Fatalf("stdout carried more than one line -- human logs belong on stderr:\n%s", line)
	}
	var env sbEnvelope
	if err := json.Unmarshal([]byte(line), &env); err != nil {
		t.Fatalf("stdout is not a JSON envelope: %v\n%s", err, line)
	}
	if env.Capability != "install.seedBootstrap" {
		t.Errorf("capability = %q, want install.seedBootstrap", env.Capability)
	}
	var res sbResult
	if err := json.Unmarshal(env.Result, &res); err != nil {
		t.Fatalf("result is not the expected object: %v\n%s", err, env.Result)
	}
	return env, res
}

// -----------------------------------------------------------------------
// A cluster made entirely of a stub
// -----------------------------------------------------------------------

type sbWorld struct {
	env     []string
	argvLog string
}

// sbNewWorld puts a stub kubectl on a PATH prefix. It appends its full argv to
// $STUB_ARGV, answers cluster-info / get namespace successfully, and echoes
// stdin back on `apply` so the pipeline behaves like the real thing.
func sbNewWorld(t *testing.T) sbWorld {
	t.Helper()
	bin := t.TempDir()
	argvLog := filepath.Join(t.TempDir(), "kubectl.argv")

	stub := `#!/usr/bin/env bash
printf '%s\n' "$*" >> "$STUB_ARGV"
# Drop leading --context <name> so the verb matching below is uniform.
if [[ "${1:-}" == "--context" ]]; then shift 2; fi
case "${1:-}" in
  cluster-info) echo "Kubernetes control plane is running"; exit 0 ;;
  get)          exit 0 ;;
  create)       echo "apiVersion: v1"; echo "kind: Secret"; exit 0 ;;
  apply)        cat > /dev/null; echo "secret/memql-bootstrap configured"; exit 0 ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(stub), 0o755); err != nil {
		t.Fatalf("write kubectl stub: %v", err)
	}
	return sbWorld{
		env: []string{
			"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"),
			"STUB_ARGV=" + argvLog,
		},
		argvLog: argvLog,
	}
}

func (w sbWorld) argv(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(w.argvLog)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read argv log: %v", err)
	}
	return string(b)
}

// sbCompleteArgs is a fully-formed invocation, so a negative test proves the
// refusal it names rather than a different missing param.
func sbCompleteArgs() []string {
	return []string{
		"--domain=local.znas.io",
		"--owner-email=ada@example.com",
		"--owner-first-name=Ada",
		"--owner-last-name=Lovelace",
		"--registration-mode=invite_only",
	}
}

// -----------------------------------------------------------------------
// THE assertion: an incomplete set is exit 2, not a warning
// -----------------------------------------------------------------------

func TestSeedBootstrapIncompleteSetIsExitTwo(t *testing.T) {
	required := []string{
		"--domain=local.znas.io",
		"--owner-email=ada@example.com",
		"--owner-first-name=Ada",
		"--owner-last-name=Lovelace",
		"--registration-mode=invite_only",
	}
	for i, omitted := range required {
		name := strings.SplitN(strings.TrimPrefix(omitted, "--"), "=", 2)[0]
		t.Run("missing "+name, func(t *testing.T) {
			w := sbNewWorld(t)
			var args []string
			for j, a := range required {
				if j != i {
					args = append(args, a)
				}
			}
			stdout, _, code := sbRun(t, w.env, args...)
			if code != 2 {
				t.Fatalf("omitting --%s exited %d, want 2 (bad param)\nstdout: %s", name, code, stdout)
			}
			env, _ := sbParse(t, stdout)
			if env.OK || env.Error == nil || env.Error.Code != 2 {
				t.Errorf("want ok=false error.code=2, got: %s", stdout)
			}
			if !strings.Contains(env.Error.Message, name) {
				t.Errorf("the refusal must name the missing field %q; got %q", name, env.Error.Message)
			}
			if env.Changed {
				t.Error("changed=true on a refusal")
			}
			// Nothing may have been written, and the cluster must not even
			// have been asked a question.
			if got := w.argv(t); got != "" {
				t.Errorf("kubectl was invoked despite the incomplete set:\n%s", got)
			}
		})
	}
}

// One re-run per missing field is the shape of a bad error message. Naming all
// of them at once is the difference between one fix and four.
func TestSeedBootstrapNamesEveryMissingFieldAtOnce(t *testing.T) {
	w := sbNewWorld(t)
	stdout, _, code := sbRun(t, w.env, "--domain=local.znas.io")
	if code != 2 {
		t.Fatalf("exit %d, want 2\nstdout: %s", code, stdout)
	}
	env, _ := sbParse(t, stdout)
	for _, want := range []string{"owner-email", "owner-first-name", "owner-last-name", "registration-mode"} {
		if !strings.Contains(env.Error.Message, want) {
			t.Errorf("the refusal omits %q -- an operator would re-run once per field; got %q", want, env.Error.Message)
		}
	}
}

// -----------------------------------------------------------------------
// THE second assertion: the key never appears in argv
// -----------------------------------------------------------------------

func TestSeedBootstrapKeyNeverAppearsInArgv(t *testing.T) {
	const secretKey = "sk-ant-DO-NOT-LEAK-0123456789"
	w := sbNewWorld(t)
	keyFile := filepath.Join(t.TempDir(), "anthropic.key")
	// Trailing newline on purpose: an editor adds one, and it is not part of
	// the key.
	if err := os.WriteFile(keyFile, []byte(secretKey+"\n"), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}

	args := append(sbCompleteArgs(), "--provider=anthropic", "--provider-key-file="+keyFile)
	stdout, stderr, code := sbRun(t, w.env, args...)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	env, res := sbParse(t, stdout)
	if !env.OK || !res.ProviderKeySeeded {
		t.Fatalf("the key was not seeded: %s", stdout)
	}
	if res.ProviderKeyEnv != "MEMQL_AI_ANTHROPIC_API_KEY" {
		t.Errorf("providerKeyEnv = %q, want MEMQL_AI_ANTHROPIC_API_KEY", res.ProviderKeyEnv)
	}

	// The whole point: kubectl's argv must carry a --from-file reference, never
	// the key itself.
	argv := w.argv(t)
	if strings.Contains(argv, secretKey) {
		t.Errorf("THE KEY IS IN ARGV -- readable via ps and /proc/<pid>/cmdline:\n%s", argv)
	}
	if !strings.Contains(argv, "--from-file=MEMQL_AI_ANTHROPIC_API_KEY=") {
		t.Errorf("the key was not passed via --from-file:\n%s", argv)
	}
	// Nor may it leak through the human-readable log or the envelope.
	if strings.Contains(stderr, secretKey) {
		t.Error("the key was printed to stderr")
	}
	if strings.Contains(stdout, secretKey) {
		t.Error("the key was printed into the result envelope")
	}
}

// The staged copy is scratch: it must not survive the run, or the key is left
// in a temp directory for the next person who looks.
func TestSeedBootstrapStagedKeyIsCleanedUp(t *testing.T) {
	w := sbNewWorld(t)
	keyFile := filepath.Join(t.TempDir(), "openai.key")
	if err := os.WriteFile(keyFile, []byte("sk-openai-xyz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := append(sbCompleteArgs(), "--provider=openai", "--provider-key-file="+keyFile)
	stdout, _, code := sbRun(t, w.env, args...)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s", code, stdout)
	}
	// Recover the staged path from kubectl's argv and confirm it is gone.
	argv := w.argv(t)
	const marker = "--from-file=MEMQL_AI_OPENAI_API_KEY="
	i := strings.Index(argv, marker)
	if i < 0 {
		t.Fatalf("no --from-file in argv:\n%s", argv)
	}
	staged := argv[i+len(marker):]
	if j := strings.IndexAny(staged, " \n"); j >= 0 {
		staged = staged[:j]
	}
	if _, err := os.Stat(staged); err == nil {
		t.Errorf("the staged key file survived the run at %s", staged)
	}
}

// -----------------------------------------------------------------------
// what actually lands in the Secret
// -----------------------------------------------------------------------

func TestSeedBootstrapWritesTheBootstrapEnvNames(t *testing.T) {
	w := sbNewWorld(t)
	args := append(sbCompleteArgs(), "--org-name=Acme", "--internal-domains=acme.com", "--internal-default-role=admin")
	stdout, stderr, code := sbRun(t, w.env, args...)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	env, res := sbParse(t, stdout)
	if !env.Changed {
		t.Error("changed=false after writing the Secret")
	}
	if !res.BootstrapComplete {
		t.Error("bootstrapComplete=false on a complete set")
	}

	argv := w.argv(t)
	for _, want := range []string{
		"MEMQL_IDENTITY_BOOTSTRAP_DOMAIN=local.znas.io",
		"MEMQL_IDENTITY_BOOTSTRAP_OWNER_EMAIL=ada@example.com",
		"MEMQL_IDENTITY_BOOTSTRAP_OWNER_FIRST_NAME=Ada",
		"MEMQL_IDENTITY_BOOTSTRAP_OWNER_LAST_NAME=Lovelace",
		"MEMQL_IDENTITY_BOOTSTRAP_REGISTRATION_MODE=invite_only",
		"MEMQL_IDENTITY_BOOTSTRAP_ORG_NAME=Acme",
		"MEMQL_IDENTITY_BOOTSTRAP_INTERNAL_DOMAINS=acme.com",
		"MEMQL_IDENTITY_BOOTSTRAP_INTERNAL_DEFAULT_ROLE=admin",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("the Secret was written without %s:\n%s", want, argv)
		}
	}
}

// An unset optional must be ABSENT, not empty: an empty env var reads as
// "configured, to nothing" and silently overrides whatever default the engine
// would have applied.
func TestSeedBootstrapOmitsUnsetOptionals(t *testing.T) {
	w := sbNewWorld(t)
	stdout, _, code := sbRun(t, w.env, sbCompleteArgs()...)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s", code, stdout)
	}
	argv := w.argv(t)
	for _, absent := range []string{
		"MEMQL_IDENTITY_BOOTSTRAP_ORG_NAME=",
		"MEMQL_IDENTITY_BOOTSTRAP_NOTIFY_EMAILS=",
		"MEMQL_IDENTITY_BOOTSTRAP_INTERNAL_DOMAINS=",
	} {
		if strings.Contains(argv, absent) {
			t.Errorf("unset optional %s was written as an empty value:\n%s", absent, argv)
		}
	}
}

// -----------------------------------------------------------------------
// the modes the engine itself refuses
// -----------------------------------------------------------------------

func TestSeedBootstrapRejectsBadModes(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{
			// identity's Config.Validate refuses this at boot; catching it here
			// turns a crash-looping node into one clear sentence.
			name: "domain_restricted with no allowlist",
			args: []string{
				"--domain=local.znas.io", "--owner-email=a@b.c",
				"--owner-first-name=A", "--owner-last-name=B",
				"--registration-mode=domain_restricted",
			},
		},
		{
			name: "unknown registration mode",
			args: []string{
				"--domain=local.znas.io", "--owner-email=a@b.c",
				"--owner-first-name=A", "--owner-last-name=B",
				"--registration-mode=freeforall",
			},
		},
		{
			name: "unknown internal default role",
			args: append(sbCompleteArgs(), "--internal-default-role=superuser"),
		},
		{
			name: "unknown provider",
			args: append(sbCompleteArgs(), "--provider=acme", "--provider-key-file=/dev/null"),
		},
		{
			name: "provider without a key file",
			args: append(sbCompleteArgs(), "--provider=anthropic"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := sbNewWorld(t)
			stdout, _, code := sbRun(t, w.env, tc.args...)
			if code != 2 {
				t.Fatalf("exit %d, want 2 (bad param)\nstdout: %s", code, stdout)
			}
			env, _ := sbParse(t, stdout)
			if env.OK || env.Error == nil || env.Error.Code != 2 {
				t.Errorf("want ok=false error.code=2, got: %s", stdout)
			}
		})
	}
}

func TestSeedBootstrapUnreadableKeyFileIsPrerequisite(t *testing.T) {
	w := sbNewWorld(t)
	args := append(sbCompleteArgs(), "--provider=anthropic", "--provider-key-file="+filepath.Join(t.TempDir(), "nope.key"))
	stdout, _, code := sbRun(t, w.env, args...)
	if code != 4 {
		t.Fatalf("exit %d, want 4 (prerequisite missing)\nstdout: %s", code, stdout)
	}
}

// -----------------------------------------------------------------------
// prerequisites + dry run
// -----------------------------------------------------------------------

func TestSeedBootstrapMissingKubectlIsPrerequisite(t *testing.T) {
	// A PATH carrying the shell utilities the capability library itself needs,
	// and nothing else -- so kubectl is genuinely absent rather than the whole
	// script failing to run.
	dir := raSanitizedBin(t)
	stdout, _, code := sbRun(t, []string{"PATH=" + dir, "STUB_ARGV=" + filepath.Join(t.TempDir(), "argv")},
		sbCompleteArgs()...)
	if code != 4 {
		t.Fatalf("exit %d, want 4 (prerequisite missing)\nstdout: %s", code, stdout)
	}
	env, _ := sbParse(t, stdout)
	if env.OK || env.Error == nil || env.Error.Code != 4 {
		t.Errorf("want ok=false error.code=4, got: %s", stdout)
	}
}

// A dry run must write nothing and say so -- including not asking the cluster,
// which may not exist yet when an operator is checking their inputs.
func TestSeedBootstrapDryRunWritesNothing(t *testing.T) {
	w := sbNewWorld(t)
	stdout, _, code := sbRun(t, w.env, append(sbCompleteArgs(), "--dry-run")...)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s", code, stdout)
	}
	env, res := sbParse(t, stdout)
	if env.Changed {
		t.Error("changed=true on a dry run")
	}
	if !res.DryRun {
		t.Error("result.dryRun=false on a dry run")
	}
	if got := w.argv(t); got != "" {
		t.Errorf("a dry run invoked kubectl:\n%s", got)
	}
}

func TestSeedBootstrapIsIdempotent(t *testing.T) {
	w := sbNewWorld(t)
	for i := 0; i < 2; i++ {
		stdout, _, code := sbRun(t, w.env, sbCompleteArgs()...)
		if code != 0 {
			t.Fatalf("run %d exited %d, want 0\nstdout: %s", i, code, stdout)
		}
	}
	// Both runs go through create|apply -- the idiom every seeder here uses.
	if n := strings.Count(w.argv(t), "apply -f -"); n != 2 {
		t.Errorf("expected two applies across two runs, got %d:\n%s", n, w.argv(t))
	}
}

func TestSeedBootstrapPrintSpec(t *testing.T) {
	stdout, _, code := sbRun(t, nil, "--print-spec")
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
	if spec.Capability != "install.seedBootstrap" {
		t.Errorf("capability = %q, want install.seedBootstrap", spec.Capability)
	}
	names := map[string]bool{}
	for _, p := range spec.Params {
		names[p.Name] = true
	}
	for _, want := range []string{
		"domain", "owner-email", "owner-first-name", "owner-last-name",
		"registration-mode", "provider", "provider-key-file",
	} {
		if !names[want] {
			t.Errorf("--print-spec omits the %q param", want)
		}
	}
}
