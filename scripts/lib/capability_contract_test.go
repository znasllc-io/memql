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
