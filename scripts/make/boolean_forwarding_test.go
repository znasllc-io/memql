package makehelp

// How a boolean make variable reaches a capability script.
//
// `$${PURGE:+--purge}` forwards a BARE flag whenever the variable is
// non-empty, so `make down PURGE=0` purged however the script read it: "0"
// is non-empty, and a bare --purge is true. A capability script reads its
// booleans with cap_bool, which interprets the value, so the recipe forwards
// the value itself: `$${PURGE:+--purge="$${PURGE}"}`.
//
// Recipes for scripts that are not capability scripts (release.sh,
// tag-submodules.sh) keep their own parsers, which take only the bare form,
// and are outside this rule.

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	reRecipeScript   = regexp.MustCompile(`\bbash[ \t]+(scripts/[A-Za-z0-9_./-]+\.sh)`)
	reBareForward    = regexp.MustCompile(`\$\$\{([A-Z_][A-Z0-9_]*):\+--([a-z][a-z0-9-]*)\}`)
	reSourcesCapLib  = regexp.MustCompile(`(?m)^[ \t]*(source|\.)[ \t]+[^\n]*lib/capability\.sh`)
	reCapSpecParamOf = func(name string) *regexp.Regexp {
		return regexp.MustCompile(`cap_spec_param(?:_required)?[ \t]+"` + regexp.QuoteMeta(name) + `"`)
	}
)

func TestCapabilityRecipesForwardBooleanValues(t *testing.T) {
	root := repoRoot(t)
	f, err := os.Open(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	capabilityLines := 0
	script := "" // the script the current (possibly continued) command runs
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if m := reRecipeScript.FindStringSubmatch(line); m != nil {
			script = m[1]
		}
		if script != "" {
			src, rerr := os.ReadFile(filepath.Join(root, script))
			if rerr == nil && reSourcesCapLib.Match(src) {
				capabilityLines++
				for _, m := range reBareForward.FindAllStringSubmatch(line, -1) {
					if !reCapSpecParamOf(m[2]).Match(src) {
						continue // not a param of this script; its own flag check refuses it
					}
					t.Errorf("Makefile:%d forwards %s to %s as a bare --%s, so %s=0 reads as true:\n    %s\n"+
						"Forward the value instead: $${%s:+--%s=\"$${%s}\"}.",
						n, m[1], script, m[2], m[1], strings.TrimSpace(line), m[1], m[2], m[1])
				}
			}
		}
		if !strings.HasSuffix(line, `\`) {
			script = ""
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if capabilityLines == 0 {
		t.Fatal("found no recipe line that runs a capability script -- the scan stopped matching, " +
			"and it is watching nothing")
	}
}

// TestMakeDownPurgeZeroKeepsTheKubeconfigContext drives the real recipe.
// Hermetic: stub k3d and kubectl on a PATH prefix log every call.
func TestMakeDownPurgeZeroKeepsTheKubeconfigContext(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not available")
	}
	root := repoRoot(t)
	run := func(t *testing.T, purge string) (purged bool, calls string) {
		t.Helper()
		dir := t.TempDir()
		stubs := map[string]string{
			"k3d": "#!/bin/sh\nprintf 'k3d %s\\n' \"$*\" >> \"$MAKE_DOWN_CALLS\"\n" +
				"if [ \"$1 $2\" = \"cluster list\" ]; then printf 'NAME SERVERS\\nmemql 1/1\\n'; fi\nexit 0\n",
			"kubectl": "#!/bin/sh\nprintf 'kubectl %s\\n' \"$*\" >> \"$MAKE_DOWN_CALLS\"\nexit 0\n",
		}
		for name, body := range stubs {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		log := filepath.Join(dir, "calls.log")
		cmd := exec.Command("make", "--no-print-directory", "-C", root, "down", "CLUSTER=memql", "PURGE="+purge)
		cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "MAKE_DOWN_CALLS="+log)
		var stdout, stderr strings.Builder
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("make down PURGE=%s: %v\n--- stdout ---\n%s--- stderr ---\n%s", purge, err, stdout.String(), stderr.String())
		}
		var env struct {
			Result struct {
				Purged bool `json:"purged"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &env); err != nil {
			t.Fatalf("stdout is not one envelope: %v\n%s", err, stdout.String())
		}
		b, _ := os.ReadFile(log)
		return env.Result.Purged, string(b)
	}

	if purged, calls := run(t, "0"); purged || strings.Contains(calls, "delete-context") {
		t.Errorf("make down PURGE=0 removed the kubeconfig context (purged=%v):\n%s", purged, calls)
	}
	if purged, calls := run(t, "1"); !purged || !strings.Contains(calls, "kubectl config delete-context k3d-memql") {
		t.Errorf("make down PURGE=1 kept the kubeconfig context (purged=%v):\n%s", purged, calls)
	}
}
