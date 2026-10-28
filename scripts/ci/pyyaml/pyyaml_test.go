package pyyaml

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A real python3 runs the probe here; only yaml is shadowed, by a module on
// PYTHONPATH that fails the way an absent PyYAML does. That is what proves the
// probe names the interpreter BEFORE the import that fails, which a stub
// interpreter cannot show.
func TestProblemNamesTheModuleTheInterpreterAndTheFix(t *testing.T) {
	shadow := t.TempDir()
	writeFile(t, filepath.Join(shadow, "yaml.py"), "raise ModuleNotFoundError(\"No module named 'yaml'\")\n", 0o644)
	t.Setenv("PYTHONPATH", shadow)

	got := problem()
	for _, want := range []string{
		"cannot import yaml",
		"PyYAML",
		"(Python 3.",
		"ModuleNotFoundError: No module named 'yaml'",
		"make install-deps",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message lacks %q:\n%s", want, got)
		}
	}
}

// The interpreter's own answer is what the message reports: the executable
// and version it printed, the last line of its error, and the PATH entry that
// was asked -- the one exec.Command("python3", ...) resolves.
func TestProblemReportsTheInterpreterThatAnswered(t *testing.T) {
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "python3"), `#!/bin/sh
echo /opt/example/bin/python3.15
echo 3.15.0
echo 'Traceback (most recent call last):' >&2
echo '  File "<string>", line 4, in <module>' >&2
echo "ModuleNotFoundError: No module named 'yaml'" >&2
exit 1
`, 0o755)
	t.Setenv("PATH", bin)

	got := problem()
	for _, want := range []string{
		"/opt/example/bin/python3.15 (Python 3.15.0)",
		"found on PATH as " + filepath.Join(bin, "python3"),
		"error:       ModuleNotFoundError: No module named 'yaml'\n",
		"make install-deps",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("message lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "Traceback") {
		t.Errorf("message repeats the traceback instead of its last line:\n%s", got)
	}
}

// A python3 that cannot start writes no traceback; the message still says why.
func TestProblemNamesAnInterpreterThatCannotStart(t *testing.T) {
	bin := t.TempDir()
	writeFile(t, filepath.Join(bin, "python3"), "not an executable format\n", 0o755)
	t.Setenv("PATH", bin)

	got := problem()
	for _, want := range []string{"found on PATH as " + filepath.Join(bin, "python3"), "exec format error", "make install-deps"} {
		if !strings.Contains(got, want) {
			t.Errorf("message lacks %q:\n%s", want, got)
		}
	}
}

func TestProblemIsEmptyWhenYAMLImports(t *testing.T) {
	shadow := t.TempDir()
	writeFile(t, filepath.Join(shadow, "yaml.py"), "", 0o644)
	t.Setenv("PYTHONPATH", shadow)

	if got := problem(); got != "" {
		t.Fatalf("an interpreter that imports yaml was reported:\n%s", got)
	}
}

func TestProblemNamesAMissingPython3(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	got := problem()
	for _, want := range []string{"python3 is not on PATH", "PyYAML", "make install-deps"} {
		if !strings.Contains(got, want) {
			t.Errorf("message lacks %q:\n%s", want, got)
		}
	}
}

func writeFile(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}
