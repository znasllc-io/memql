package lib

// scripts/lib/bool_flag.sh is the boolean reader for the scripts that parse
// their own flags rather than sourcing capability.sh (release.sh,
// tag-submodules.sh). A make variable reaches both kinds of script the same
// way, `$${VAR:+--flag="$${VAR}"}`, so both must mean the same thing by it:
// this pins bool_flag to cap_bool, value for value.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBoolFlagAgreesWithCapBool(t *testing.T) {
	root := repoRoot(t)
	lib := filepath.Join(root, "scripts", "lib")
	dir := t.TempDir()

	capHarness := filepath.Join(dir, "cap.sh")
	if err := os.WriteFile(capHarness, []byte("#!/usr/bin/env bash\n"+
		"set -euo pipefail\n"+
		"source \""+filepath.Join(lib, "capability.sh")+"\"\n"+
		"cap_init \"test.boolFlagParity\" \"parity harness\"\n"+
		"cap_spec_param \"x\" \"the flag under test (flag)\"\n"+
		"cap_parse_flags \"$@\"\n"+
		"if cap_bool x; then printf 'RESULT=true\\n'; else printf 'RESULT=false\\n'; fi\n"+
		"cap_ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	flagHarness := filepath.Join(dir, "flag.sh")
	if err := os.WriteFile(flagHarness, []byte("#!/usr/bin/env bash\n"+
		"set -uo pipefail\n"+
		"source \""+filepath.Join(lib, "bool_flag.sh")+"\"\n"+
		"v=\"$(bool_flag --x \"$1\")\" || { printf 'RESULT=refused rc=%s\\n' \"$?\"; exit 0; }\n"+
		"printf 'RESULT=%s\\n' \"$v\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	answer := func(t *testing.T, harness string, args ...string) string {
		t.Helper()
		out, err := exec.Command("bash", append([]string{harness}, args...)...).CombinedOutput()
		if code := exitCode(err); code == 2 && harness == capHarness {
			return "refused rc=2"
		} else if err != nil {
			t.Fatalf("%s %v: %v\n%s", filepath.Base(harness), args, err, out)
		}
		return parseResult(string(out))
	}

	for _, v := range []string{
		"", "true", "TRUE", "True", "1", "yes", "Y", "y", "on", "ON",
		"false", "FALSE", "0", "no", "N", "n", "off", "Off",
		"ture", "2", "maybe", " true", "yes please",
	} {
		v := v
		t.Run("value "+strings.ReplaceAll(v, " ", "_"), func(t *testing.T) {
			want := answer(t, capHarness, "--x="+v)
			got := answer(t, flagHarness, v)
			if got != want {
				t.Errorf("bool_flag reads %q as %s, cap_bool as %s -- a make variable would mean one "+
					"thing to a capability script and another to release.sh", v, got, want)
			}
		})
	}
}
