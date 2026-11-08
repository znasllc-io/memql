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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/core/repowalk"
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
// sourcesCapabilityLib matches an actual `source .../lib/capability.sh`
// STATEMENT, not a mention of the path.
//
// It was `bytes.Contains(b, "lib/capability.sh")`, which is an opt-in test that
// a comment can pass. scripts/ci/bash32-smoke.sh names the path in its own
// prose and in the pattern it greps for, so a substring rule classified the CI
// reporter as a capability and these tests EXECUTED it -- whereupon it ran its
// own discovery from the wrong working directory and reported two failures
// about a corpus it could not see. The same loose rule fork-bombed that script
// when it discovered itself, so this is the second time one substring match
// has been wrong; both now require the statement.
//
// scripts/ci/bash32-smoke.sh applies the identical rule in shell, which is what
// lets its header claim the two cover one corpus.
var sourcesCapabilityLib = regexp.MustCompile(`(?m)^[ \t]*(source|\.)[ \t]+[^\n]*lib/capability\.sh`)

func capabilityScripts(t *testing.T) []string {
	t.Helper()
	root := repoRoot(t)
	scriptsDir := filepath.Join(root, "scripts")
	var found []string
	err := filepath.Walk(scriptsDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && repowalk.SkipDir(info.Name()) {
			return filepath.SkipDir
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
		if sourcesCapabilityLib.Match(b) {
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
	dirs := []string{"scripts/k3d", "scripts/deploy", "scripts/release", "scripts/docs"}
	for _, d := range dirs {
		full := filepath.Join(root, d)
		if _, err := os.Stat(full); os.IsNotExist(err) {
			continue
		}
		err := filepath.Walk(full, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() && repowalk.SkipDir(info.Name()) {
				return filepath.SkipDir
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
	// test can assert precedence. It ends on cap_ok like any capability: an exit
	// of 0 with no envelope is reported as a failure (TestCapExitZeroWithoutAResultIsAFailure).
	// `foo` must be declared via cap_spec_param -- cap_parse_flags rejects
	// undeclared flags (exit 2, #2508).
	harness := filepath.Join(t.TempDir(), "capparam_harness.sh")
	body := "#!/usr/bin/env bash\n" +
		"set -euo pipefail\n" +
		"source \"" + lib + "\"\n" +
		"cap_init \"test.capParamPrecedence\" \"cap_param precedence harness\"\n" +
		"cap_spec_param \"foo\" \"precedence probe param\"\n" +
		"cap_parse_flags \"$@\"\n" +
		"printf 'RESULT=%s\\n' \"$(cap_param foo defval)\"\n" +
		"cap_ok\n"
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
		"printf 'RESULT=%s\\n' \"$(cap_param declared defval)\"\n" +
		"cap_ok\n"
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
		// A bare declared flag resolves to "true", NOT "1". This expectation was
		// the assertion that CERTIFIED znasllc-io/memql#4629: the parser was
		// tested in isolation against the one value no consumer accepted, so the
		// gate stayed green while every `--dryRun` ran the destructive branch.
		// Changed deliberately -- see TestCapBoolPairsParserAndConsumer, which
		// tests the parser and a consumer together so this cannot recur.
		{name: "declared bare flag parses as true", args: []string{"--declared"}, wantExit: 0, wantVal: "true"},
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
		"printf 'ABSENT=%s\\n' \"$(cap_param absent fallback)\"\n" +
		"cap_ok\n"
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

// TestCapBoolPairsParserAndConsumer is the gate for znasllc-io/memql#4629 /
// #4631: a bare `--dryRun` used to parse to the string "1" while every
// consumer compared against "true", so the guard was skipped and the
// destructive branch ran. The safety flag failed OPEN.
//
// Nothing caught it because nothing tested the parser and a consumer
// TOGETHER -- TestCapParseFlagsUnknownFlagSemantics asserted the parser's
// value in isolation, and asserted the harmful one. This test closes that
// gap by driving a fixture script end to end: a `dryRun` param, a side
// effect guarded by cap_bool, and an assertion that every truthy spelling
// reaches the SAME branch.
//
// It fails against the pre-fix parser (bare --dryRun took the destructive
// branch), which is the property that makes it a gate rather than a
// restatement.
func TestCapBoolPairsParserAndConsumer(t *testing.T) {
	root := repoRoot(t)
	lib := filepath.Join(root, "scripts", "lib", "capability.sh")

	harness := filepath.Join(t.TempDir(), "capbool_harness.sh")
	body := "#!/usr/bin/env bash\n" +
		"set -euo pipefail\n" +
		"source \"" + lib + "\"\n" +
		"cap_init \"test.capBool\" \"cap_bool pairing harness\"\n" +
		"cap_spec_param \"dryRun\" \"report without acting\"\n" +
		"cap_parse_flags \"$@\"\n" +
		// The shape every capability script uses: a destructive branch the
		// boolean is supposed to guard.
		"if cap_bool dryRun; then\n" +
		"    printf 'RESULT=%s\\n' \"dry\"\n" +
		"else\n" +
		"    printf 'RESULT=%s\\n' \"destructive\"\n" +
		"fi\n" +
		"cap_ok '{}'\n"
	if err := os.WriteFile(harness, []byte(body), 0o755); err != nil {
		t.Fatalf("write harness: %v", err)
	}

	cases := []struct {
		name     string
		args     []string
		wantExit int
		want     string
	}{
		// The three spellings that must be INDISTINGUISHABLE. The first is
		// the one a human types and the only one that was broken.
		{name: "bare flag is truthy", args: []string{"--dryRun"}, want: "dry"},
		{name: "explicit true is truthy", args: []string{"--dryRun=true"}, want: "dry"},
		{name: "legacy 1 is truthy", args: []string{"--dryRun=1"}, want: "dry"},
		{name: "yes is truthy", args: []string{"--dryRun=yes"}, want: "dry"},
		{name: "case is ignored", args: []string{"--dryRun=TRUE"}, want: "dry"},

		// And the falsey spellings must reach the other branch.
		{name: "omitted is falsey", args: nil, want: "destructive"},
		{name: "explicit false is falsey", args: []string{"--dryRun=false"}, want: "destructive"},
		{name: "zero is falsey", args: []string{"--dryRun=0"}, want: "destructive"},
		{name: "empty is falsey", args: []string{"--dryRun="}, want: "destructive"},

		// A value that is neither is REFUSED rather than silently taking a
		// branch. cap_bool cannot know a flag's polarity, so it cannot pick a
		// safe default for a typo; exit 2 (bad param) is the only answer that
		// fails closed regardless of which branch is the dangerous one.
		{name: "typo is refused, not guessed", args: []string{"--dryRun=ture"}, wantExit: 2},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("bash", append([]string{harness}, tc.args...)...)
			cmd.Stdin = nil
			out, err := cmd.CombinedOutput()

			if tc.wantExit != 0 {
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
				if env.OK || env.Error == nil || env.Error.Code != tc.wantExit {
					t.Errorf("envelope should carry ok=false error.code=%d; got: %s", tc.wantExit, line)
				}
				return
			}

			if err != nil {
				t.Fatalf("harness failed: %v\noutput:\n%s", err, out)
			}
			got := parseResult(string(out))
			if got != tc.want {
				t.Errorf("took the %q branch, want %q -- a boolean flag whose spelling changes "+
					"which branch runs is the #4629 defect\noutput:\n%s", got, tc.want, out)
			}
		})
	}
}

// reCapParamStringCompare matches a capability script comparing a value
// against a bare boolean string literal -- `[[ "$DRY_RUN" == "true" ]]`,
// `[ "$KEEP" = "1" ]`, and the negated forms. That hand-rolled comparison IS
// the #4629 defect: it is the half of the pair that disagreed with the
// parser, and re-introducing one anywhere re-opens the class.
// Capture group 1 is the variable NAME being compared, so the finding is
// attributed to that variable and not to some unrelated one that happens to
// share the line.
var reCapParamStringCompare = regexp.MustCompile(
	`\$\{?([A-Za-z_][A-Za-z0-9_]*)\}?"?\s*[!=]=\s*"?(?:true|false|1|yes|no)"?`)

// TestNoCapabilityScriptHandRollsABooleanComparison keeps #4629 closed as a
// CLASS rather than as an instance. The parser fix alone leaves the next
// boolean param to rediscover the same disagreement; this sweep is what makes
// cap_bool the only sanctioned reader.
//
// It only inspects variables that were sourced from cap_param/cap_flag, since
// a script comparing some unrelated local against "true" is not this bug.
func TestNoCapabilityScriptHandRollsABooleanComparison(t *testing.T) {
	scripts := capabilityScripts(t)
	if len(scripts) == 0 {
		t.Fatal("no capability scripts discovered")
	}
	// var name -> sourced from cap_param/cap_flag, e.g. DRY_RUN="$(cap_param dryRun ...)"
	reSourced := regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)=\"?\$\((cap_param|cap_flag)\s`)

	for _, path := range scripts {
		path := path
		rel, _ := filepath.Rel(repoRoot(t), path)
		t.Run(rel, func(t *testing.T) {
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			src := string(b)

			sourced := map[string]bool{}
			for _, m := range reSourced.FindAllStringSubmatch(src, -1) {
				sourced[m[1]] = true
			}
			if len(sourced) == 0 {
				return
			}

			for i, line := range strings.Split(src, "\n") {
				for _, m := range reCapParamStringCompare.FindAllStringSubmatch(line, -1) {
					name := m[1]
					if !sourced[name] {
						continue
					}
					t.Errorf("%s:%d compares %s (a cap_param value) against a boolean string literal:\n"+
						"    %s\n"+
						"Use `if cap_bool <param>; then` instead. A hand-rolled comparison is what\n"+
						"made a bare --dryRun fail open (znasllc-io/memql#4629): the parser and the\n"+
						"consumer disagreed on the spelling of true.",
						rel, i+1, name, strings.TrimSpace(line))
				}
			}
		})
	}
}

type booleanReadFinding struct {
	line int    // one-based
	text string // the trimmed source line
	why  string
}

var (
	reSpecParam = regexp.MustCompile(`cap_spec_param(?:_required)?[ \t]+"([^"]+)"[ \t]+"((?:[^"\\]|\\.)*)"`)
	// A param is DECLARED a flag when a parenthetical in its description
	// names the word: "(flag)", "(flag; default: report only)", "(flag,
	// forwarded to up.sh)", "(immutability override; flag)". Every boolean
	// the corpus read with cap_flag said so; "flagged" is not the word.
	reFlagMarker = regexp.MustCompile(`\([^()]*\bflag\b[^()]*\)`)
	reParamRead  = regexp.MustCompile(`\b(cap_flag|cap_param|cap_bool_str|cap_bool)[ \t]+"?([A-Za-z0-9][A-Za-z0-9_-]*)"?`)
	// The one placement where cap_bool_str's exit 2 stops the script: the
	// whole right-hand side of a plain assignment, optionally after a
	// `local NAME;` declaration on the same line.
	reBoolStrAssign = regexp.MustCompile(`^(?:local[ \t]+[A-Za-z_][A-Za-z0-9_ \t]*;[ \t]*)?([A-Za-z_][A-Za-z0-9_]*)="?\$\(cap_bool_str[ \t].*\)"?(?:[ \t]+#.*)?$`)
	reCapBoolSubst  = regexp.MustCompile("(?:\\$\\(|`)[ \t]*(?:![ \t]*)?cap_bool(?:[ \t;)`]|$)")
)

// booleanReadFindings returns every boolean read in a capability script's
// source that can fail open or lose its refusal. Full-line comments are not
// code. The rules, each a measured failure:
//
//  1. A param declared a flag (reFlagMarker) is read only with cap_bool or
//     cap_bool_str. cap_flag and cap_param hand back the raw string, and
//     "false" is non-empty.
//  2. cap_flag reads only a param the script also resolves with cap_param: a
//     presence probe for a VALUE param ("was --revision passed?"). Any other
//     cap_flag read is a boolean read in disguise, whatever its description
//     says -- four memql-znas scripts declared `apply` with no marker.
//  3. cap_bool_str is the whole right-hand side of a plain assignment. Under
//     `local`, `export`, a [[ ]] test or an argument, bash discards the
//     substitution's exit 2 and a mistyped value reads as false.
//  4. cap_bool never runs inside a command substitution, where its refusal is
//     captured as output instead of stopping the script.
//  5. A variable assigned from cap_bool_str is never tested for emptiness:
//     it holds "true" or "false", so `-n` is always true. That is the mistake
//     a conversion makes when it changes the read and not the test.
func booleanReadFindings(src string) []booleanReadFinding {
	declared := map[string]bool{}
	for _, m := range reSpecParam.FindAllStringSubmatch(src, -1) {
		if reFlagMarker.MatchString(m[2]) {
			declared[m[1]] = true
		}
	}

	type paramRead struct {
		line      int
		text, fn  string
		paramName string
	}
	var (
		findings []booleanReadFinding
		reads    []paramRead
		code     []bashCodeLine      // the non-comment lines
		valued   = map[string]bool{} // resolved with cap_param somewhere
		boolStr  = map[string]bool{} // variables holding a cap_bool_str value
	)
	for i, raw := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		code = append(code, bashCodeLine{text: trimmed, number: i + 1})
		for _, m := range reParamRead.FindAllStringSubmatch(trimmed, -1) {
			reads = append(reads, paramRead{line: i + 1, text: trimmed, fn: m[1], paramName: m[2]})
			if m[1] == "cap_param" {
				valued[m[2]] = true
			}
		}
		if strings.Contains(trimmed, "cap_bool_str") {
			m := reBoolStrAssign.FindStringSubmatch(trimmed)
			if m != nil && strings.Count(trimmed, "cap_bool_str") == 1 {
				boolStr[m[1]] = true
			} else {
				findings = append(findings, booleanReadFinding{i + 1, trimmed,
					"cap_bool_str is not the whole right-hand side of a plain assignment, so its refusal of a " +
						"non-boolean is lost: `local`, `export`, a [[ ]] test and an argument all discard the " +
						"substitution's exit 2, and a mistyped value reads as false. Write NAME=\"$(cap_bool_str " +
						"<param>)\" as its own statement, or branch with `if cap_bool <param>; then`."})
			}
		}
		if reCapBoolSubst.MatchString(trimmed) {
			findings = append(findings, booleanReadFinding{i + 1, trimmed,
				"cap_bool runs inside a command substitution, where its refusal of a non-boolean is captured " +
					"as output instead of stopping the script. Branch with `if cap_bool <param>; then`, or take " +
					"the value with NAME=\"$(cap_bool_str <param>)\"."})
		}
	}

	for _, r := range reads {
		switch {
		case declared[r.paramName] && (r.fn == "cap_flag" || r.fn == "cap_param"):
			findings = append(findings, booleanReadFinding{r.line, r.text, fmt.Sprintf(
				"--%s is declared a flag (its description says so) but is read with %s, which returns the raw "+
					"string: --%s=false is non-empty, so an emptiness test takes it as true. Read it with "+
					"`if cap_bool %s; then`.", r.paramName, r.fn, r.paramName, r.paramName)})
		case r.fn == "cap_flag" && !valued[r.paramName]:
			findings = append(findings, booleanReadFinding{r.line, r.text, fmt.Sprintf(
				"cap_flag %s reads a param this script never resolves with cap_param. cap_flag answers one "+
					"question, whether a VALUE param arrived as a flag, beside that param's cap_param read; "+
					"anything else is a boolean read in disguise, and --%s=false is a non-empty string. Read a "+
					"boolean with `if cap_bool %s; then` (and say \"(flag)\" in its description), a value with "+
					"cap_param.", r.paramName, r.paramName, r.paramName)})
		}
	}

	for name := range boolStr {
		emptiness := regexp.MustCompile(`-[nz][ \t]+"?\$\{?` + name + `\b|\$\{` + name + `:?\+`)
		for _, c := range code {
			if emptiness.MatchString(c.text) {
				findings = append(findings, booleanReadFinding{c.number, c.text, fmt.Sprintf(
					"%s holds cap_bool_str's \"true\" or \"false\", both non-empty, so this emptiness test is "+
						"constant. Compare it: [[ \"$%s\" == \"true\" ]].", name, name)})
			}
		}
	}

	sort.Slice(findings, func(i, j int) bool { return findings[i].line < findings[j].line })
	return findings
}

// TestCapabilityScriptsReadBooleansWithCapBool is the gate for boolean reads
// that fail open (booleanReadFindings has the rules).
//
// cap_flag returns the raw parsed value, and a boolean read through it and
// tested for emptiness takes --x=false as true. That shipped in thirteen
// scripts. Measured, by the regression test beside each:
// normalize-node-ids.sh --apply=false --confirm=normalize-node-ids rewrote
// stored ids; k3d/down.sh --purge=false removed the kubeconfig context;
// magic-link.sh, enrolment-link.sh and recovery-key.sh passed --local=false
// through their gates to kubectl, against whatever context it pointed at; and
// verify-frontdoor.sh --report-only=false, the exact argv the DSL action
// renders for `reportOnly: false`, exited 0 over a failing front door. Read
// from the code, not run: bringup.sh --clean=false took the clean-slate path,
// which deletes the cluster and its database.
//
// TestNoCapabilityScriptHandRollsABooleanComparison catches the `== "true"`
// half of the same mistake; this catches the read itself and the emptiness
// tests that went with it.
func TestCapabilityScriptsReadBooleansWithCapBool(t *testing.T) {
	scripts := capabilityScripts(t)
	if len(scripts) == 0 {
		t.Fatal("no capability scripts discovered")
	}
	declaredFlags := 0
	for _, path := range scripts {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		src := string(b)
		for _, m := range reSpecParam.FindAllStringSubmatch(src, -1) {
			if reFlagMarker.MatchString(m[2]) {
				declaredFlags++
			}
		}
		for _, f := range booleanReadFindings(src) {
			t.Errorf("%s:%d: %s\n    %s", rel(t, path), f.line, f.why, f.text)
		}
	}
	if declaredFlags == 0 {
		t.Fatal("found no param declared a flag -- either every \"(flag)\" description went or the scan " +
			"stopped matching, and either way rule 1 is watching nothing")
	}
}

// TestBooleanReadFindingsCatchEachShape drives booleanReadFindings over one
// fixture per shape, so the corpus sweep below cannot pass by matching
// nothing. Each bad shape is one measured way for a boolean to fail open or
// lose its refusal; each good one is a reader the conversions rely on.
func TestBooleanReadFindingsCatchEachShape(t *testing.T) {
	const declarations = `cap_spec_param "apply" "perform the rewrite (flag; default: report only)"
cap_spec_param "force" "overwrite an existing lockfile (immutability override; flag)"
cap_spec_param "quiet" "log failures only"
cap_spec_param "domain" "the domain"
cap_spec_param "internal-domains" "domains whose users are flagged internal"
`
	cases := []struct {
		name string
		body string
		want string // substring of the one expected finding; "" = no finding
	}{
		{"a declared flag read with cap_flag", `APPLY="$(cap_flag apply)"`, "declared a flag"},
		{"a declared flag read with cap_param", `FORCE="$(cap_param force "${FORCE_ENV:-}")"`, "declared a flag"},
		{"an undeclared boolean read with cap_flag", `[[ -n "$(cap_flag quiet)" ]] || log_sample`, "never resolves with cap_param"},
		{"a value probe beside its cap_param read", "DOMAIN=\"$(cap_param domain \"\")\"\n[[ -z \"$(cap_flag domain)\" ]] && DOMAIN=fallback", ""},
		{"flagged as a word is not a flag marker", `INTERNAL="$(cap_param internal-domains "")"`, ""},
		{"the branch form", `if cap_bool apply; then APPLY=1; else APPLY=""; fi`, ""},
		{"the negated branch form", `if ! cap_bool apply; then cap_fail 3 "refused"; fi`, ""},
		{"the value form", `APPLY="$(cap_bool_str apply)"`, ""},
		{"the value form after a local declaration", `local apply; apply="$(cap_bool_str apply false)"`, ""},
		{"the value form compared to true", "APPLY=\"$(cap_bool_str apply)\"\nif [[ \"$APPLY\" == \"true\" ]]; then run; fi", ""},
		{"local discards the refusal", `local APPLY="$(cap_bool_str apply)"`, "plain assignment"},
		{"export discards the refusal", `export APPLY="$(cap_bool_str apply)"`, "plain assignment"},
		{"a test discards the refusal", `if [[ "$(cap_bool_str apply)" == "true" ]]; then run; fi`, "plain assignment"},
		{"an argument discards the refusal", `cap_result_set_raw applied "$(cap_bool_str apply)"`, "plain assignment"},
		{"cap_bool inside a substitution", `APPLY="$(cap_bool apply && echo true || echo false)"`, "command substitution"},
		{"an emptiness test of a cap_bool_str value", "APPLY=\"$(cap_bool_str apply)\"\nif [[ -n \"$APPLY\" ]]; then run; fi", "emptiness"},
		{"a pass-through of a cap_bool_str value", "APPLY=\"$(cap_bool_str apply)\"\nrun ${APPLY:+--apply}", "emptiness"},
		{"a longer name sharing the prefix is another variable", "APPLY=\"$(cap_bool_str apply)\"\n[[ -n \"$APPLY_LOG\" ]] && run", ""},
		{"a comment is not a read", `# APPLY="$(cap_flag apply)"`, ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			findings := booleanReadFindings(declarations + tc.body + "\n")
			if tc.want == "" {
				for _, f := range findings {
					t.Errorf("unexpected finding at line %d: %s\n    %s", f.line, f.why, f.text)
				}
				return
			}
			if len(findings) != 1 {
				t.Fatalf("want exactly one finding containing %q, got %d: %+v", tc.want, len(findings), findings)
			}
			if !strings.Contains(findings[0].why, tc.want) {
				t.Errorf("finding does not say %q:\n    %s", tc.want, findings[0].why)
			}
		})
	}
}

// capBoolHarness writes a capability script that sources the runtime,
// declares a boolean `dryRun` and a value `foo`, parses its flags and then
// runs body, and returns its path.
func capBoolHarness(t *testing.T, body string) string {
	t.Helper()
	lib := filepath.Join(repoRoot(t), "scripts", "lib", "capability.sh")
	harness := filepath.Join(t.TempDir(), "capbool_reader_harness.sh")
	script := "#!/usr/bin/env bash\n" +
		"set -euo pipefail\n" +
		"source \"" + lib + "\"\n" +
		"cap_init \"test.capBoolReader\" \"boolean reader harness\"\n" +
		"cap_spec_param \"dryRun\" \"report without acting (flag)\"\n" +
		"cap_spec_param \"foo\" \"a value param\"\n" +
		"cap_parse_flags \"$@\"\n" +
		body
	if err := os.WriteFile(harness, []byte(script), 0o755); err != nil {
		t.Fatalf("write harness: %v", err)
	}
	return harness
}

// TestCapBoolStrRefusalNamesTheValue pins what a mistyped boolean tells the
// person who typed it when the script reads it with cap_bool_str.
//
// cap_bool_str runs inside a command substitution, so its refusal envelope is
// captured into the variable and never printed. The assignment's exit 2 still
// stops the script, and the EXIT trap then reports only that the run "aborted
// (exit 2)". Measured on bash 3.2.57 and 5.3.15: nothing said which flag or
// which value. stderr is the one stream a substitution does not capture, so
// the refusal names both there.
func TestCapBoolStrRefusalNamesTheValue(t *testing.T) {
	harness := capBoolHarness(t, "DRY=\"$(cap_bool_str dryRun)\"\ncap_info \"dry=${DRY}\"\ncap_ok\n")
	cmd := exec.Command("bash", harness, "--dryRun=ture")
	cmd.Stdin = nil
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	diag := "--- stdout ---\n" + stdout.String() + "--- stderr ---\n" + stderr.String()

	if code := exitCode(err); code != 2 {
		t.Fatalf("exit %d, want 2 (bad param)\n%s", code, diag)
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout must carry exactly one envelope, got %d lines\n%s", len(lines), diag)
	}
	var env envelope
	if json.Unmarshal([]byte(lines[0]), &env) != nil || env.OK || env.Error == nil || env.Error.Code != 2 {
		t.Fatalf("want one failure envelope with error.code=2\n%s", diag)
	}
	for _, want := range []string{"--dryRun", "'ture'"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr does not name %s: a refused boolean must say which flag and which value\n%s", want, diag)
		}
	}
}

// TestCapParamsStdinIsABoolean holds the runtime's own boolean flag to the
// rule it sets every script. _cap_load_stdin read --params-stdin with cap_flag
// and tested it for emptiness, so --params-stdin=false READ stdin. That flag
// exists to keep a capability from consuming a stream it was not handed.
func TestCapParamsStdinIsABoolean(t *testing.T) {
	harness := capBoolHarness(t, "printf 'RESULT=%s\\n' \"$(cap_param foo defval)\"\ncap_ok\n")
	cases := []struct {
		name     string
		args     []string
		want     string
		wantExit int
	}{
		{name: "bare flag reads stdin", args: []string{"--params-stdin"}, want: "stdinval"},
		{name: "true reads stdin", args: []string{"--params-stdin=true"}, want: "stdinval"},
		{name: "false reads no stdin", args: []string{"--params-stdin=false"}, want: "defval"},
		{name: "zero reads no stdin", args: []string{"--params-stdin=0"}, want: "defval"},
		{name: "a typo is refused, not guessed", args: []string{"--params-stdin=ture"}, wantExit: 2},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("bash", append([]string{harness}, tc.args...)...)
			cmd.Stdin = strings.NewReader(`{"foo":"stdinval"}`)
			out, err := cmd.CombinedOutput()
			if tc.wantExit != 0 {
				if code := exitCode(err); code != tc.wantExit {
					t.Fatalf("exit %d, want %d\noutput:\n%s", code, tc.wantExit, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("harness failed: %v\noutput:\n%s", err, out)
			}
			if got := parseResult(string(out)); got != tc.want {
				t.Errorf("foo resolved %q, want %q\noutput:\n%s", got, tc.want, out)
			}
		})
	}
}

// TestCapProgressWritesOneStderrLine pins `cap_progress`, the structured
// progress line a long capability writes while it works.
//
// Its contract has two readers that must agree: a human at a terminal, who just
// sees the line, and the editor's runner (editors/vscode/src/install/runner.ts),
// which recognises the `::memql-progress::` prefix, turns the line into a phase
// under the progress bar and keeps it out of the step's log. So the shape is
// asserted byte for byte, and so is where it goes: STDERR only, because stdout
// belongs to the one result envelope. And because reporting progress must never
// be able to fail a capability, a malformed count still exits 0 and still
// reports the label.
func TestCapProgressWritesOneStderrLine(t *testing.T) {
	root := repoRoot(t)
	lib := filepath.Join(root, "scripts", "lib", "capability.sh")

	harness := filepath.Join(t.TempDir(), "capprogress_harness.sh")
	body := "#!/usr/bin/env bash\n" +
		"set -euo pipefail\n" +
		"source \"" + lib + "\"\n" +
		"cap_init \"test.capProgress\" \"cap_progress harness\"\n" +
		"cap_progress \"$@\"\n" +
		"cap_ok\n"
	if err := os.WriteFile(harness, []byte(body), 0o755); err != nil {
		t.Fatalf("write harness: %v", err)
	}

	cases := []struct {
		name   string
		args   []string
		stderr string
	}{
		{"with a count", []string{"Starting services", "5", "9"}, "::memql-progress:: 5/9 Starting services\n"},
		{"without a count", []string{"Installing ArgoCD"}, "::memql-progress:: - Installing ArgoCD\n"},
		{"zero done is a count", []string{"Building images", "0", "3"}, "::memql-progress:: 0/3 Building images\n"},
		{"a malformed count drops the count, not the line", []string{"Importing images", "two", "3"}, "::memql-progress:: - Importing images\n"},
		{"a zero total is no count", []string{"Importing images", "0", "0"}, "::memql-progress:: - Importing images\n"},
		{"half a count is no count", []string{"Importing images", "2"}, "::memql-progress:: - Importing images\n"},
		{"an empty label reports nothing", []string{""}, ""},
		// Leading zeros are decimal. Read as octal, "08" is an arithmetic error
		// bash prints into the step's log on its way to dropping the count.
		{"leading zeros are decimal", []string{"Starting services", "08", "09"}, "::memql-progress:: 8/9 Starting services\n"},
		{"a count too large to be one is no count", []string{"Building images", "99999999999", "9"}, "::memql-progress:: - Building images\n"},
		{"a line break in the label cannot split the line", []string{"Starting\r\nservices", "1", "2"}, "::memql-progress:: 1/2 Starting  services\n"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("bash", append([]string{harness}, tc.args...)...)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("cap_progress failed the script: %v\nstderr:\n%s", err, stderr.String())
			}
			if got := stderr.String(); got != tc.stderr {
				t.Errorf("stderr = %q, want %q", got, tc.stderr)
			}
			if strings.Contains(stdout.String(), "memql-progress") {
				t.Errorf("a progress line reached stdout, which belongs to the result envelope:\n%s", stdout.String())
			}
			var env envelope
			if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &env); err != nil || !env.OK {
				t.Errorf("stdout is not exactly one ok envelope (%v):\n%s", err, stdout.String())
			}
		})
	}
}

// noResultHarness writes a capability script that sources the runtime, calls
// cap_init and then runs body, and returns its path.
func noResultHarness(t *testing.T, body string) string {
	t.Helper()
	lib := filepath.Join(repoRoot(t), "scripts", "lib", "capability.sh")
	harness := filepath.Join(t.TempDir(), "noresult_harness.sh")
	script := "#!/usr/bin/env bash\n" +
		"set -euo pipefail\n" +
		"source \"" + lib + "\"\n" +
		"cap_init \"test.noResult\" \"a run that never reports its own result\"\n" +
		body
	if err := os.WriteFile(harness, []byte(script), 0o755); err != nil {
		t.Fatalf("write harness: %v", err)
	}
	return harness
}

// assertReportedAsFailure runs harness under bashPath and asserts the one
// outcome the contract allows for a run that never emitted its own result:
// exit 1, and exactly one envelope on stdout saying that no result was given.
// wantStderr, when set, is bash's own error, which must still reach stderr --
// it is the only place the cause is named.
func assertReportedAsFailure(t *testing.T, bashPath, harness, wantStderr string) {
	t.Helper()
	cmd := exec.Command(bashPath, harness)
	cmd.Stdin = nil
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	diag := "--- stdout ---\n" + stdout.String() + "--- stderr ---\n" + stderr.String()

	if err == nil {
		t.Fatalf("exited 0 having never called cap_ok or cap_fail: a run that never said it succeeded "+
			"was reported as a success\n%s", diag)
	}
	if code := exitCode(err); code != 1 {
		t.Errorf("exit %d, want 1 (the contract's code for an unexpected failure)\n%s", code, diag)
	}
	if strings.TrimSpace(stdout.String()) == "" {
		t.Fatalf("no envelope on stdout\n%s", diag)
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout must carry exactly one envelope, got %d lines\n%s", len(lines), diag)
	}
	var env envelope
	if err := json.Unmarshal([]byte(lines[0]), &env); err != nil {
		t.Fatalf("stdout is not a JSON envelope: %v\n%s", err, diag)
	}
	if env.OK || env.Error == nil || env.Error.Code != 1 {
		t.Fatalf("want ok=false and error.code=1, got: %s", lines[0])
	}
	if env.Capability != "test.noResult" {
		t.Errorf("envelope names capability %q, want %q", env.Capability, "test.noResult")
	}
	if !strings.Contains(env.Error.Message, "without an explicit result") || !strings.Contains(env.Error.Message, "cap_ok") {
		t.Errorf("the message must say no result was given and name cap_ok; got: %s", env.Error.Message)
	}
	if wantStderr != "" && !strings.Contains(stderr.String(), wantStderr) {
		t.Errorf("stderr lost bash's own %q, which is the only place the cause is named\n%s", wantStderr, diag)
	}
}

// TestCapExitZeroWithoutAResultIsAFailure pins what the EXIT trap does when a
// script ends with status 0 but never emitted an envelope.
//
// It used to do nothing. _cap_on_exit spoke only for a NON-zero status, on the
// theory that a zero meant a clean end -- but a success path MUST call cap_ok,
// so a zero with no envelope means the script never got as far as saying it
// succeeded. Trusting that zero is what let bash 3.2's expansion deaths
// (TestCapBash32ExpansionDeathIsAFailure) exit 0 and report nothing at all.
//
// Neither case here needs bash 3.2, so this is the test every CI lane runs.
func TestCapExitZeroWithoutAResultIsAFailure(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"an exit 0 before cap_ok", "cap_info \"did the work\"\nexit 0\ncap_ok\n"},
		{"the end of the script without cap_ok", "cap_info \"did the work\"\n"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			assertReportedAsFailure(t, "bash", noResultHarness(t, tc.body), "")
		})
	}
}

// bash32 returns a bash 3.2 to run under, or skips. Stock macOS ships one as
// /bin/bash and always will; no CI runner here has one outside the
// bash32-smoke container.
func bash32(t *testing.T) string {
	t.Helper()
	candidates := []string{"/bin/bash"}
	if p, err := exec.LookPath("bash"); err == nil {
		candidates = append(candidates, p)
	}
	for _, c := range candidates {
		out, err := exec.Command(c, "-c", `printf '%s' "$BASH_VERSION"`).Output()
		if err == nil && strings.HasPrefix(string(out), "3.2.") {
			return c
		}
	}
	t.Skipf("no bash 3.2 to run under (probed %s); stock macOS ships one as /bin/bash",
		strings.Join(candidates, ", "))
	return ""
}

// TestCapBash32ExpansionDeathIsAFailure runs the defect behind the rule above
// on the interpreter that produces it.
//
// Under `set -e` -- which every capability script sets -- stock macOS bash
// 3.2.57 ends a script that hits a fatal expansion error with the status of
// the last command that ran, which under `set -e` is almost always 0. Bash 4+
// exits 1 for the same line. Measured, both ways. So on a Mac a bash 4
// construct, or any unbound variable, made a capability exit 0 with nothing on
// stdout: a product repo's argocd-sync.sh printed its compare and then did
// exactly that.
//
// Skipped where there is no bash 3.2, which is every CI runner here: the
// bash32-smoke lane (scripts/ci/bash32-smoke.sh) runs the unbound-variable
// shape under the real interpreter instead.
func TestCapBash32ExpansionDeathIsAFailure(t *testing.T) {
	bash := bash32(t)
	cases := []struct {
		name   string
		body   string
		stderr string
	}{
		{
			// The shape that shipped: a bash 4 case modification inside a
			// predicate an `if` calls.
			name: "a bad substitution in a function an if calls",
			body: "function is_x() {\n" +
				"    case \"${1,,}\" in\n" +
				"        x) return 0 ;;\n" +
				"    esac\n" +
				"    return 1\n" +
				"}\n" +
				"if is_x X; then cap_info \"matched\"; fi\n" +
				"cap_ok\n",
			stderr: "bad substitution",
		},
		{
			// The commoner one: any unbound variable under `set -u`.
			name:   "an unbound variable",
			body:   "cap_info \"value: ${never_set}\"\ncap_ok\n",
			stderr: "unbound variable",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			assertReportedAsFailure(t, bash, noResultHarness(t, tc.body), tc.stderr)
		})
	}
}

// TestCapabilityTrapsHandCapOnExitTheRealStatus keeps capability.sh's result
// guarantee in force wherever a capability script installs an EXIT trap of its
// own. A trap replaces the one cap_init installs, so cleanup can drop the
// guarantee in two ways, and both shipped.
//
// REPLACED. `trap 'rm -rf "$WORK"' EXIT` removes the guarantee outright: from
// that line on, an abort reaches the caller with nothing on stdout.
// scripts/deploy/render-diff.sh and scripts/deploy/seed-instance-secrets.sh did
// that, with cleanup traps of their own.
//
// MISFED. `trap 'cleanup_tmp; _cap_on_exit' EXIT` reads naturally and is still
// wrong: _cap_on_exit reads the status from $?, and by the time it runs, $? is
// cleanup_tmp's, not the script's. scripts/install/hosts-entries.sh composed
// its trap that way precisely so an abort would still emit an envelope
// (memql#3562), and an abort there still emitted nothing: the runtime saw
// cleanup's 0 and took it for a clean end. Now that a 0 with no result is
// reported as a failure, the same composition would report an abort as an
// exit 0 and blame the wrong thing.
//
// THE RULE. Every EXIT trap reaches _cap_on_exit. Inline, a trap that names
// _cap_on_exit names nothing else. Anything that has to run first goes in a
// handler function, defined in the script, that captures $? as its first
// statement, turns errexit off, and restores the status with `(exit "$rc")`
// immediately before calling _cap_on_exit -- _install_on_exit in
// scripts/install/install-binary.sh is the reference. The `set +e` is not
// optional: under errexit, `(exit "$rc")` is itself a failing command whenever
// the status is non-zero, so the handler is abandoned there -- on exactly the
// aborts it exists for -- before _cap_on_exit runs. Measured on bash 3.2 and 5.
// Clearing the trap (`trap - EXIT`) is refused as well, except inside a
// handler, where it only stops the handler running twice.
func TestCapabilityTrapsHandCapOnExitTheRealStatus(t *testing.T) {
	inlineTrap := regexp.MustCompile(`^trap[ \t]+(?:'([^']*)'|"([^"]*)")[ \t]+EXIT\b`)
	namedTrap := regexp.MustCompile(`^trap[ \t]+'?([A-Za-z_][A-Za-z0-9_]*)'?[ \t]+EXIT\b`)
	clearsTrap := regexp.MustCompile(`^trap[ \t]+(-|''|"")[ \t]+EXIT\b`)
	identifier := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	captures := regexp.MustCompile(`^local[ \t]+([A-Za-z_][A-Za-z0-9_]*)=\$\?$`)
	errexitOff := regexp.MustCompile(`^set[ \t]+\+e([ \t;]|$)`)
	callsCapOnExit := regexp.MustCompile(`^_cap_on_exit([ \t;]|$)`)
	const repair = "Repair: put the cleanup in a handler function that starts `local rc=$?`, runs " +
		"`set +e`, and runs `(exit \"$rc\")` right before _cap_on_exit, as _install_on_exit in " +
		"scripts/install/install-binary.sh does, and install that with `trap <handler> EXIT`."

	scripts := capabilityScripts(t)
	if len(scripts) == 0 {
		t.Fatal("no capability scripts found -- the discovery glob is broken")
	}
	checked := 0
	for _, path := range scripts {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		lines := strings.Split(string(body), "\n")

		// Every function's code lines, by name, and the function each code
		// line belongs to.
		funcs := map[string][]bashCodeLine{}
		enclosing := map[int]string{}
		for _, fn := range bashFunctions(string(body)) {
			funcs[fn.name] = fn.code
			for _, c := range fn.code {
				enclosing[c.number] = fn.name
			}
		}
		handlers := map[string]bool{}
		for _, raw := range lines {
			if m := namedTrap.FindStringSubmatch(strings.TrimSpace(raw)); m != nil {
				handlers[m[1]] = true
			}
		}

		for i, raw := range lines {
			trimmed := strings.TrimSpace(raw)
			where := rel(t, path) + ":" + strconv.Itoa(i+1)
			if clearsTrap.MatchString(trimmed) {
				if !handlers[enclosing[i+1]] {
					t.Errorf("%s: this clears the EXIT trap, and with it capability.sh's guarantee of "+
						"an envelope on every exit:\n    %s\nOnly an EXIT handler may clear it, to "+
						"stop itself running twice.", where, trimmed)
				}
				continue
			}
			if m := inlineTrap.FindStringSubmatch(trimmed); m != nil {
				inline := strings.TrimSpace(m[1] + m[2])
				switch {
				case identifier.MatchString(inline):
					// _cap_on_exit itself, or a named handler, judged below.
				case strings.Contains(inline, "_cap_on_exit"):
					t.Errorf("%s: an EXIT trap runs other commands with _cap_on_exit:\n    %s\n"+
						"_cap_on_exit reads the script's status from $?, which the commands before it "+
						"have replaced -- an abort is then reported as an exit 0, or not at all.\n%s",
						where, trimmed, repair)
					continue
				default:
					t.Errorf("%s: an EXIT trap replaces capability.sh's result guarantee:\n    %s\n"+
						"From this line on, an abort reaches the caller with nothing on stdout.\n%s",
						where, trimmed, repair)
					continue
				}
			}
			m := namedTrap.FindStringSubmatch(trimmed)
			if m == nil || m[1] == "_cap_on_exit" {
				continue
			}
			code, defined := funcs[m[1]]
			if !defined {
				t.Errorf("%s: the EXIT handler %s() is not a multi-line function defined in this "+
					"script, so nothing can check that it reaches _cap_on_exit.\n%s", where, m[1], repair)
				continue
			}
			call := -1
			for j, c := range code {
				if callsCapOnExit.MatchString(c.text) {
					call = j
					break
				}
			}
			if call < 0 {
				t.Errorf("%s: the EXIT handler %s() never calls _cap_on_exit, so it replaces "+
					"capability.sh's result guarantee: an abort reaches the caller with nothing on "+
					"stdout.\n%s", where, m[1], repair)
				continue
			}
			checked++
			first := captures.FindStringSubmatch(code[0].text)
			if first == nil || call == 0 {
				t.Errorf("%s: the EXIT handler %s() calls _cap_on_exit but does not begin by capturing "+
					"the script's status (`local rc=$?`); it begins:\n    %s", where, m[1], code[0].text)
				continue
			}
			restore := regexp.MustCompile(`^\(exit[ \t]+"?\$\{?` + first[1] + `\}?"?\)([ \t]+#.*)?$`)
			if !restore.MatchString(code[call-1].text) {
				t.Errorf("%s: the EXIT handler %s() does not restore the status it captured right "+
					"before calling _cap_on_exit -- `(exit \"$%s\")` must be the line before it; "+
					"found:\n    %s", where, m[1], first[1], code[call-1].text)
				continue
			}
			off := false
			for _, c := range code[1 : call-1] {
				off = off || errexitOff.MatchString(c.text)
			}
			if !off {
				t.Errorf("%s: the EXIT handler %s() restores the status without `set +e` first. "+
					"Under errexit, `(exit \"$%s\")` fails whenever the status is non-zero, and the "+
					"handler is abandoned there -- on every abort -- before _cap_on_exit runs.",
					where, m[1], first[1])
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no EXIT handler that calls _cap_on_exit -- either every script stopped " +
			"composing its trap or this scan stopped matching, and either way it is watching nothing")
	}
	t.Logf("checked %d EXIT handler(s) across %d capability script(s)", checked, len(scripts))
}

// TestCapSourcedScriptLeavesTheShellsExitAlone pins where the exit-0 rule
// stops: at the capability's own process.
//
// A capability script that a shell SOURCES -- a test harness exercising its
// functions, as scripts/k3d's tests do with up.sh and dev.sh -- still installs
// the trap from its top-level cap_init. But the shell that sourced it is not
// the capability, and its exit 0 is its own: holding those shells to cap_ok
// turned 79 assertions across scripts/k3d red the moment the rule landed. An
// abort is still reported either way.
func TestCapSourcedScriptLeavesTheShellsExitAlone(t *testing.T) {
	lib := filepath.Join(repoRoot(t), "scripts", "lib", "capability.sh")
	dir := t.TempDir()
	script := filepath.Join(dir, "sourced_capability.sh")
	body := "#!/usr/bin/env bash\n" +
		"set -euo pipefail\n" +
		"source \"" + lib + "\"\n" +
		"cap_init \"test.sourced\" \"a capability a test harness sources\"\n" +
		"function greet() { cap_info \"hello\"; }\n" +
		"function main() { greet; cap_ok; }\n" +
		"if [[ \"${BASH_SOURCE[0]}\" == \"$0\" ]]; then\n" +
		"    main \"$@\"\n" +
		"fi\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	harness := func(name, tail string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("source \""+script+"\"\ngreet\n"+tail), 0o755); err != nil {
			t.Fatalf("write harness: %v", err)
		}
		return p
	}

	cases := []struct {
		name  string
		argv  []string
		abort bool
	}{
		{"a harness file that sources it and ends", []string{harness("ends.sh", "")}, false},
		{"bash -c that sources it and ends", []string{"-c", "source \"$1\"; greet", "bash", script}, false},
		{"a harness that sources it and aborts", []string{harness("aborts.sh", "false\n")}, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("bash", tc.argv...)
			cmd.Stdin = nil
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()
			diag := "--- stdout ---\n" + stdout.String() + "--- stderr ---\n" + stderr.String()

			if !tc.abort {
				if err != nil {
					t.Fatalf("a shell that sourced a capability and ended cleanly exited %d -- the "+
						"exit-0 rule reached past the capability into its caller\n%s", exitCode(err), diag)
				}
				if strings.TrimSpace(stdout.String()) != "" {
					t.Errorf("stdout should be empty, the shell never ran the capability\n%s", diag)
				}
				return
			}
			if code := exitCode(err); code != 1 {
				t.Fatalf("exit %d, want 1 from the abort\n%s", code, diag)
			}
			var env envelope
			line := strings.TrimSpace(stdout.String())
			if json.Unmarshal([]byte(line), &env) != nil || env.OK || env.Error == nil ||
				!strings.Contains(env.Error.Message, "aborted (exit 1)") {
				t.Errorf("an abort must still be reported, as exactly one envelope\n%s", diag)
			}
		})
	}
}
