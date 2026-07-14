// Conformance gate for the capability-script contract
// (docs/internal/design/capability-script-contract.md, znasllc-io/memql#2221).
//
// A "capability script" is any shell script under scripts/ that sources
// scripts/lib/capability.sh -- i.e. it opts into the contract. This test
// discovers every such script and enforces, both statically and dynamically,
// that it honours the contract:
//
//   - sources scripts/lib/capability.sh and calls cap_init;
//   - is non-interactive (no `read -p` / `read -rp` / `select` prompt);
//   - is syntactically valid bash;
//   - answers --print-spec with a valid JSON descriptor whose "capability"
//     matches its cap_init id;
//   - does not block when run with stdin closed.
//
// New capability scripts are picked up automatically -- no registry to update.
// A script that is NOT a capability backend (a pure status reporter, a dev
// convenience) simply does not source capability.sh and is skipped here; but
// anything a DSL `action` resolves to MUST be a capability script.
package lib

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// repoRoot resolves the repository root from this test file's location
// (scripts/lib -> ../..).
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

// capabilityScripts walks scripts/ and returns every .sh that sources
// capability.sh (the opt-in marker for the contract).
func capabilityScripts(t *testing.T) []string {
	t.Helper()
	root := repoRoot(t)
	scriptsDir := filepath.Join(root, "scripts")
	var found []string
	err := filepath.Walk(scriptsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".sh") {
			return nil
		}
		// Never treat the library itself as a capability script.
		if filepath.Base(path) == "capability.sh" {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if bytes.Contains(b, []byte("lib/capability.sh")) {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk scripts/: %v", err)
	}
	return found
}

// interactivePrompt matches a blocking `read` prompt or a `select` loop, which
// the contract (rule 3) forbids. We match `read` with -p (prompt) or with no
// input source, and bare `select NAME in`. `while ... read -r line` style data
// reads (which consume an already-redirected stream, not the tty) are allowed.
var (
	reReadPrompt = regexp.MustCompile(`(?m)^\s*read\s+(-[a-zA-Z]*\s+)*-p\b`)
	reSelect     = regexp.MustCompile(`(?m)^\s*select\s+\w+\s+in\b`)
)

func TestCapabilityScriptsAreNonInteractive(t *testing.T) {
	scripts := capabilityScripts(t)
	if len(scripts) == 0 {
		t.Fatal("no capability scripts found -- the discovery glob is broken")
	}
	for _, s := range scripts {
		s := s
		t.Run(rel(t, s), func(t *testing.T) {
			b, err := os.ReadFile(s)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			src := string(b)
			if reReadPrompt.MatchString(src) {
				t.Errorf("interactive `read -p` prompt found -- capability scripts must be "+
					"non-interactive; replace the confirmation with a --confirm=<phrase> param "+
					"and cap_confirm_or_die (contract rule 3). File: %s", s)
			}
			if reSelect.MatchString(src) {
				t.Errorf("interactive `select` menu found -- capability scripts must be "+
					"non-interactive (contract rule 3). File: %s", s)
			}
		})
	}
}

// TestDeployScriptsAreNonInteractive is the BROADER non-interactivity gate.
// The capability-script contract (rule 3) requires every deploy/ops script --
// not only the capability-converted ones -- to be non-interactive, because any
// of them may be driven by an automation or a CI runner. This scans the deploy
// surface for blocking prompts so a future `read -p` regression fails CI.
func TestDeployScriptsAreNonInteractive(t *testing.T) {
	root := repoRoot(t)
	dirs := []string{"scripts/k3d", "scripts/deploy", "scripts/staging", "scripts/release"}
	for _, d := range dirs {
		full := filepath.Join(root, d)
		if _, err := os.Stat(full); os.IsNotExist(err) {
			continue
		}
		err := filepath.Walk(full, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".sh") {
				return nil
			}
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			src := string(b)
			if reReadPrompt.MatchString(src) || reSelect.MatchString(src) {
				t.Errorf("interactive prompt found in %s -- deploy/ops scripts must be "+
					"non-interactive (capability-script contract rule 3); replace the "+
					"confirmation with an explicit --confirm=<phrase> param", rel(t, path))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", d, err)
		}
	}
}

func TestCapabilityScriptsCallCapInit(t *testing.T) {
	reInit := regexp.MustCompile(`(?m)^\s*cap_init\s+`)
	for _, s := range capabilityScripts(t) {
		s := s
		t.Run(rel(t, s), func(t *testing.T) {
			b, err := os.ReadFile(s)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if !reInit.MatchString(string(b)) {
				t.Errorf("script sources capability.sh but never calls cap_init <id> <summary> "+
					"-- the result-guarantee trap is not installed. File: %s", s)
			}
		})
	}
}

func TestCapabilityScriptsAreValidBash(t *testing.T) {
	for _, s := range capabilityScripts(t) {
		s := s
		t.Run(rel(t, s), func(t *testing.T) {
			out, err := exec.Command("bash", "-n", s).CombinedOutput()
			if err != nil {
				t.Errorf("bash -n failed: %v\n%s", err, out)
			}
		})
	}
}

// TestCapabilityScriptsPrintSpec runs each script with --print-spec and a
// closed stdin, asserting it (a) does not block, (b) emits a valid descriptor,
// and (c) the descriptor's capability id matches its cap_init declaration.
func TestCapabilityScriptsPrintSpec(t *testing.T) {
	reInitID := regexp.MustCompile(`(?m)^\s*cap_init\s+"([^"]+)"`)
	for _, s := range capabilityScripts(t) {
		s := s
		t.Run(rel(t, s), func(t *testing.T) {
			b, err := os.ReadFile(s)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			m := reInitID.FindStringSubmatch(string(b))
			if m == nil {
				t.Skipf("cap_init id not a string literal; skipping descriptor check")
				return
			}
			wantID := m[1]

			out, err := runWithTimeout(t, 30*time.Second, s, "--print-spec")
			if err != nil {
				t.Fatalf("--print-spec failed (exit/err %v); output:\n%s", err, out)
			}
			line := lastJSONLine(out)
			if line == "" {
				t.Fatalf("--print-spec emitted no JSON line; output:\n%s", out)
			}
			var spec struct {
				Capability string `json:"capability"`
				Summary    string `json:"summary"`
				Params     []struct {
					Name string `json:"name"`
				} `json:"params"`
			}
			if err := json.Unmarshal([]byte(line), &spec); err != nil {
				t.Fatalf("--print-spec is not valid JSON: %v\nline: %s", err, line)
			}
			if spec.Capability != wantID {
				t.Errorf("descriptor capability %q != cap_init id %q", spec.Capability, wantID)
			}
		})
	}
}

// runWithTimeout runs the script with stdin closed (the contract requires it
// not block) and a hard timeout (it must not hang).
func runWithTimeout(t *testing.T, d time.Duration, script string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Stdin = nil // closed stdin
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return buf.String(), err
	case <-time.After(d):
		_ = cmd.Process.Kill()
		t.Fatalf("script %s blocked > %s with stdin closed -- not non-interactive", script, d)
		return buf.String(), nil
	}
}

// lastJSONLine returns the last line of out that parses as a JSON object.
func lastJSONLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.HasPrefix(l, "{") && strings.HasSuffix(l, "}") {
			return l
		}
	}
	return ""
}

func rel(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.Rel(repoRoot(t), p)
	if err != nil {
		return p
	}
	return r
}

// TestCapParamPrecedence locks cap_param's resolution order to what the contract
// (and capability.sh / capability-script-contract.md) advertise:
//
//	--flag=value  >  stdin JSON (opt-in via --params-stdin)  >  default
//
// Critically it asserts there is NO environment tier: a plain env var named like
// the param is ignored, and resolution falls through to the default. This is the
// claim znasllc-io/memql#2382 corrected in the docs; the test keeps doc == code.
func TestCapParamPrecedence(t *testing.T) {
	root := repoRoot(t)
	lib := filepath.Join(root, "scripts", "lib", "capability.sh")
	if _, err := os.Stat(lib); err != nil {
		t.Fatalf("capability.sh not found at %s: %v", lib, err)
	}

	// Harness sources the library and prints the resolved value of `foo` so the
	// test can assert precedence. It never calls cap_ok, so the EXIT trap stays
	// quiet on a clean (exit 0) run. `foo` must be declared via cap_spec_param
	// -- cap_parse_flags rejects undeclared flags (exit 2, #2508).
	harness := filepath.Join(t.TempDir(), "capparam_harness.sh")
	body := "#!/usr/bin/env bash\n" +
		"set -euo pipefail\n" +
		"source \"" + lib + "\"\n" +
		"cap_init \"test.capParamPrecedence\" \"cap_param precedence harness\"\n" +
		"cap_spec_param \"foo\" \"precedence probe param\"\n" +
		"cap_parse_flags \"$@\"\n" +
		"printf 'RESULT=%s\\n' \"$(cap_param foo defval)\"\n"
	if err := os.WriteFile(harness, []byte(body), 0o755); err != nil {
		t.Fatalf("write harness: %v", err)
	}

	cases := []struct {
		name  string
		args  []string
		stdin string
		env   []string // extra KEY=VAL entries appended to the process env
		want  string
	}{
		{
			name:  "flag beats stdin and default",
			args:  []string{"--params-stdin", "--foo=flagval"},
			stdin: `{"foo":"stdinval"}`,
			want:  "flagval",
		},
		{
			name:  "stdin beats default",
			args:  []string{"--params-stdin"},
			stdin: `{"foo":"stdinval"}`,
			want:  "stdinval",
		},
		{
			name: "default when neither flag nor stdin",
			want: "defval",
		},
		{
			// The corrected claim: cap_param has NO env tier. A plain env var
			// named like the param must not be consulted -- resolution falls
			// through to the default.
			name: "no env tier -- env var named like the param is ignored",
			env:  []string{"FOO=envval", "CAP_ARG_FOO_SHOULD_NOT_MATTER=x"},
			want: "defval",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("bash", append([]string{harness}, tc.args...)...)
			cmd.Env = append(os.Environ(), tc.env...)
			if tc.stdin != "" {
				cmd.Stdin = strings.NewReader(tc.stdin)
			} else {
				cmd.Stdin = nil // closed stdin
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("harness failed: %v\noutput:\n%s", err, out)
			}
			got := parseResult(string(out))
			if got != tc.want {
				t.Errorf("cap_param resolved %q, want %q\noutput:\n%s", got, tc.want, out)
			}
		})
	}
}

// parseResult extracts the value printed after the last "RESULT=" marker.
func parseResult(out string) string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(strings.TrimSpace(lines[i]), "RESULT="); ok {
			return v
		}
	}
	return ""
}

// envelope is the capability result envelope (the contract's result schema).
type envelope struct {
	OK         bool            `json:"ok"`
	Capability string          `json:"capability"`
	Changed    bool            `json:"changed"`
	Result     json.RawMessage `json:"result"`
	Error      *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// exitCode extracts the process exit code from an exec error (-1 if unknown).
func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// TestCapabilityScriptsRejectUnknownFlags runs EVERY capability script with a
// flag no script declares and asserts the contract's documented behavior
// (znasllc-io/memql#2508): exit 2 plus a failure envelope whose error.code is
// 2. Before the fix, cap_parse_flags silently accepted any --name / --name=v,
// so a typoed flag fell through to its default -- dangerous for capability
// scripts whose params gate destructive behavior. The rejection fires inside
// cap_parse_flags, before any script work runs, so this is side-effect-free.
func TestCapabilityScriptsRejectUnknownFlags(t *testing.T) {
	for _, s := range capabilityScripts(t) {
		s := s
		t.Run(rel(t, s), func(t *testing.T) {
			cmd := exec.Command("bash", s, "--zz-undeclared-conformance-probe=1")
			cmd.Stdin = nil // closed stdin
			var buf bytes.Buffer
			cmd.Stdout = &buf
			cmd.Stderr = &buf
			done := make(chan error, 1)
			if err := cmd.Start(); err != nil {
				t.Fatalf("start: %v", err)
			}
			go func() { done <- cmd.Wait() }()
			var runErr error
			select {
			case runErr = <-done:
			case <-time.After(30 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatalf("script blocked > 30s on an unknown flag")
			}
			out := buf.String()
			if runErr == nil {
				t.Fatalf("unknown flag accepted (exit 0) -- cap_parse_flags must reject "+
					"undeclared flags with exit 2 (contract, #2508). Output:\n%s", out)
			}
			if code := exitCode(runErr); code != 2 {
				t.Errorf("unknown flag exited %d, want 2 (bad invocation). Output:\n%s", code, out)
			}
			line := lastJSONLine(out)
			if line == "" {
				t.Fatalf("no JSON failure envelope emitted for the unknown flag. Output:\n%s", out)
			}
			var env envelope
			if err := json.Unmarshal([]byte(line), &env); err != nil {
				t.Fatalf("failure envelope is not valid JSON: %v\nline: %s", err, line)
			}
			if env.OK {
				t.Errorf("envelope ok=true for a rejected flag; want ok=false")
			}
			if env.Error == nil || env.Error.Code != 2 {
				t.Errorf("envelope error.code != 2 for a rejected flag; envelope: %s", line)
			}
		})
	}
}

// TestCapParseFlagsUnknownFlagSemantics locks the library-level rejection
// semantics (#2508): declared flags and the built-in meta flags parse; any
// undeclared flag -- bare or --k=v form -- fails with exit 2 and a failure
// envelope naming the flag.
func TestCapParseFlagsUnknownFlagSemantics(t *testing.T) {
	root := repoRoot(t)
	lib := filepath.Join(root, "scripts", "lib", "capability.sh")

	harness := filepath.Join(t.TempDir(), "unknownflag_harness.sh")
	body := "#!/usr/bin/env bash\n" +
		"set -euo pipefail\n" +
		"source \"" + lib + "\"\n" +
		"cap_init \"test.unknownFlag\" \"unknown-flag rejection harness\"\n" +
		"cap_spec_param \"declared\" \"a declared param\"\n" +
		"cap_parse_flags \"$@\"\n" +
		"printf 'RESULT=%s\\n' \"$(cap_param declared defval)\"\n"
	if err := os.WriteFile(harness, []byte(body), 0o755); err != nil {
		t.Fatalf("write harness: %v", err)
	}

	cases := []struct {
		name     string
		args     []string
		wantExit int    // 0 = success expected
		wantVal  string // asserted only on success
	}{
		{name: "declared flag parses", args: []string{"--declared=x"}, wantExit: 0, wantVal: "x"},
		{name: "declared bare flag parses", args: []string{"--declared"}, wantExit: 0, wantVal: "1"},
		{name: "meta params-stdin needs no declaration", args: []string{"--params-stdin"}, wantExit: 0, wantVal: "defval"},
		{name: "undeclared k=v flag exits 2", args: []string{"--produkt=x"}, wantExit: 2},
		{name: "undeclared bare flag exits 2", args: []string{"--produkt"}, wantExit: 2},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("bash", append([]string{harness}, tc.args...)...)
			cmd.Stdin = nil
			out, err := cmd.CombinedOutput()
			if tc.wantExit == 0 {
				if err != nil {
					t.Fatalf("harness failed: %v\noutput:\n%s", err, out)
				}
				if got := parseResult(string(out)); got != tc.wantVal {
					t.Errorf("resolved %q, want %q\noutput:\n%s", got, tc.wantVal, out)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected exit %d, got success\noutput:\n%s", tc.wantExit, out)
			}
			if code := exitCode(err); code != tc.wantExit {
				t.Errorf("exit %d, want %d\noutput:\n%s", code, tc.wantExit, out)
			}
			line := lastJSONLine(string(out))
			var env envelope
			if line == "" || json.Unmarshal([]byte(line), &env) != nil {
				t.Fatalf("no valid failure envelope\noutput:\n%s", out)
			}
			if env.OK || env.Error == nil || env.Error.Code != 2 {
				t.Errorf("envelope should carry ok=false error.code=2; got: %s", line)
			}
			if !strings.Contains(env.Error.Message, "--produkt") {
				t.Errorf("error message should name the rejected flag; got: %s", env.Error.Message)
			}
		})
	}
}

// TestCapParamStdinMultiParam is the regression test for the stdin drain bug
// (znasllc-io/memql#2508, second defect): each "$(cap_param ...)" subshell used
// to re-read stdin, and the first read drained it, so a two-param stdin JSON
// object like {"product":...,"product-org":...} resolved only its first-read
// key. The fix buffers stdin ONCE in the parent shell (cap_init on the env
// opt-in; cap_parse_flags on --params-stdin), so every subshell inherits the
// same captured JSON. This harness mirrors the original repro, including a
// dashed param name.
func TestCapParamStdinMultiParam(t *testing.T) {
	root := repoRoot(t)
	lib := filepath.Join(root, "scripts", "lib", "capability.sh")

	harness := filepath.Join(t.TempDir(), "stdinmulti_harness.sh")
	body := "#!/usr/bin/env bash\n" +
		"set -euo pipefail\n" +
		"source \"" + lib + "\"\n" +
		"cap_init \"test.stdinMulti\" \"multi-param stdin harness\"\n" +
		"cap_spec_param \"product\" \"first param\"\n" +
		"cap_spec_param \"product-org\" \"second param (dashed)\"\n" +
		"cap_spec_param \"absent\" \"third param, not in the JSON\"\n" +
		"cap_parse_flags \"$@\"\n" +
		"printf 'PRODUCT=%s\\n' \"$(cap_param product)\"\n" +
		"printf 'ORG=%s\\n' \"$(cap_param product-org)\"\n" +
		"printf 'ABSENT=%s\\n' \"$(cap_param absent fallback)\"\n"
	if err := os.WriteFile(harness, []byte(body), 0o755); err != nil {
		t.Fatalf("write harness: %v", err)
	}

	run := func(t *testing.T, args []string, extraEnv []string) map[string]string {
		t.Helper()
		cmd := exec.Command("bash", append([]string{harness}, args...)...)
		cmd.Env = append(os.Environ(), extraEnv...)
		cmd.Stdin = strings.NewReader(`{"product":"demo","product-org":"demo-org"}`)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("harness failed: %v\noutput:\n%s", err, out)
		}
		got := map[string]string{}
		for _, l := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
			if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
				got[k] = v
			}
		}
		return got
	}

	assert := func(t *testing.T, got map[string]string) {
		t.Helper()
		if got["PRODUCT"] != "demo" {
			t.Errorf("product resolved %q, want %q", got["PRODUCT"], "demo")
		}
		if got["ORG"] != "demo-org" {
			t.Errorf("product-org resolved %q, want %q -- the stdin buffer was "+
				"drained by the first cap_param read (#2508)", got["ORG"], "demo-org")
		}
		if got["ABSENT"] != "fallback" {
			t.Errorf("absent param resolved %q, want default %q", got["ABSENT"], "fallback")
		}
	}

	t.Run("flag opt-in --params-stdin", func(t *testing.T) {
		assert(t, run(t, []string{"--params-stdin"}, nil))
	})
	t.Run("env opt-in CAP_PARAMS_STDIN=1", func(t *testing.T) {
		assert(t, run(t, nil, []string{"CAP_PARAMS_STDIN=1"}))
	})
}
