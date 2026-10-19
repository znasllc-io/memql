package worker

import (
	"go/ast"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
)

// workerenv_test.go pins how `memql worker models` learns the worker's
// OLLAMA_NUM_PARALLEL. The command runs in the owner's shell; the worker runs
// under a unit that loads ~/.memql/worker.env. Reading the shell named a ceiling
// the worker does not advertise.

func TestParseEnvironmentFile(t *testing.T) {
	tests := []struct {
		name string
		text string
		want map[string]string
	}{
		{"nothing", "", map[string]string{}},
		{"one assignment", "OLLAMA_NUM_PARALLEL=2\n", map[string]string{"OLLAMA_NUM_PARALLEL": "2"}},
		{"no trailing newline", "A=1", map[string]string{"A": "1"}},
		{"blank lines and both comment marks", "\n# a comment\n; another\n   \nA=1\n  # indented comment\n", map[string]string{"A": "1"}},
		{"a commented-out assignment is not one", "#OLLAMA_NUM_PARALLEL=2\n;OLLAMA_NUM_PARALLEL=3\n", map[string]string{}},
		{"a line without an equals sign is ignored", "OLLAMA_NUM_PARALLEL\nA=1\n", map[string]string{"A": "1"}},
		{"an empty name is ignored", "=2\nA=1\n", map[string]string{"A": "1"}},
		{"the last assignment wins", "A=1\nB=5\nA=2\n", map[string]string{"A": "2", "B": "5"}},
		{"an empty assignment is an assignment", "A=1\nA=\n", map[string]string{"A": ""}},
		{"double quotes are stripped", "A=\"2\"\n", map[string]string{"A": "2"}},
		{"single quotes are stripped", "A='2'\n", map[string]string{"A": "2"}},
		{"whitespace around the name and value is stripped", "  A =  2  \n", map[string]string{"A": "2"}},
		{"whitespace inside quotes is kept", "A=\" 2 \"\n", map[string]string{"A": " 2 "}},
		{"mismatched quotes are not a pair", "A=\"2'\n", map[string]string{"A": "\"2'"}},
		{"one lone quote is a value", "A=\"\n", map[string]string{"A": "\""}},
		{"the value may hold an equals sign", "A=b=c\n", map[string]string{"A": "b=c"}},
		{"windows line ends", "A=1\r\nB=2\r\n", map[string]string{"A": "1", "B": "2"}},
		// systemd refuses a name with a space in it; `export` is shell syntax,
		// and a unit does not read a shell.
		{"export is not stripped", "export A=1\nB=2\n", map[string]string{"B": "2"}},
		{"a name may not start with a digit", "1A=1\nA1=2\n", map[string]string{"A1": "2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseEnvironmentFile(tt.text); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseEnvironmentFile(%q) = %q, want %q", tt.text, got, tt.want)
			}
		})
	}
}

// The unit loads the file on Linux, and only there: a LaunchAgent's plist sets
// HOME and PATH and names no environment file.
func TestWorkerEnvFilePath(t *testing.T) {
	config := filepath.Join("/home", "op", ".memql", "worker.yaml")
	if got, want := workerEnvFilePath("linux", config), filepath.Join("/home", "op", ".memql", "worker.env"); got != want {
		t.Errorf("linux: %q, want %q", got, want)
	}
	for _, goos := range []string{"darwin", "windows", "freebsd"} {
		if got := workerEnvFilePath(goos, config); got != "" {
			t.Errorf("%s: %q, want none: nothing starts the worker from a file there", goos, got)
		}
	}
}

// ceilingFor asks a discoverer that follows the worker's environment (as the
// CLI's does) for the ceiling and where it came from.
func ceilingFor(envFile string) (int, string) {
	d := &models.Discoverer{ParallelEnv: workerParallelEnv(envFile, os.Getenv)}
	return d.ParallelSource(0)
}

func writeWorkerEnv(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "worker.env")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestTheCLIReadsTheWorkersEnvironmentFileNotItsOwnShell is the finding: the
// worker's OLLAMA_NUM_PARALLEL is the one its unit loads from worker.env, and
// the shell the owner typed the command into can hold another.
func TestTheCLIReadsTheWorkersEnvironmentFileNotItsOwnShell(t *testing.T) {
	t.Setenv("OLLAMA_NUM_PARALLEL", "2")
	path := writeWorkerEnv(t, "# the worker's slots\nOLLAMA_NUM_PARALLEL=4\n")
	n, source := ceilingFor(path)
	want := "OLLAMA_NUM_PARALLEL in the worker's environment file (" + path + ")"
	if n != 4 || source != want {
		t.Errorf("ceiling = %d from %q, want 4 from %q: the shell's 2 is not the worker's", n, source, want)
	}
}

func TestTheEnvironmentFileIsNamedWithTheHomeAsATilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".memql", "worker.env")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("OLLAMA_NUM_PARALLEL=4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, source := ceilingFor(path)
	if want := "OLLAMA_NUM_PARALLEL in the worker's environment file (~/.memql/worker.env)"; source != want {
		t.Errorf("source = %q, want %q", source, want)
	}
}

// An environment file that exists is the whole of what the unit loads for the
// worker, so a variable it does not set is one the worker does not have --
// whatever the shell holds. The owner's own file is empty, which is exactly
// this case.
func TestAnEmptyEnvironmentFileMeansTheWorkerHasNoSuchVariable(t *testing.T) {
	t.Setenv("OLLAMA_NUM_PARALLEL", "2")
	for name, text := range map[string]string{
		"empty":                       "",
		"a comment only":              "# nothing here yet\n",
		"other variables":             "MEMQL_WORKER_TOKEN=abc\n",
		"the variable set to nothing": "OLLAMA_NUM_PARALLEL=\n",
	} {
		t.Run(name, func(t *testing.T) {
			n, source := ceilingFor(writeWorkerEnv(t, text))
			if n != 1 || source != "the default" {
				t.Errorf("ceiling = %d from %q, want 1 from the default: the shell's 2 must not leak in", n, source)
			}
		})
	}
}

// With no file there is nothing to read the worker's environment from (a
// Mac, or a machine the installer never touched), and the shell is the only
// view left. It is used, and it is not claimed to be the worker's.
func TestWithoutAnEnvironmentFileTheCLIFallsBackToItsOwnAndSaysSo(t *testing.T) {
	t.Setenv("OLLAMA_NUM_PARALLEL", "2")
	const want = "OLLAMA_NUM_PARALLEL in this command's environment (the worker's own may differ)"
	for name, path := range map[string]string{
		"no file":                 filepath.Join(t.TempDir(), "worker.env"),
		"a platform with no file": "",
		"a path that is a folder": t.TempDir(),
		"a path under a non-dir":  filepath.Join(writeWorkerEnv(t, ""), "worker.env"),
	} {
		t.Run(name, func(t *testing.T) {
			n, source := ceilingFor(path)
			if n != 2 || source != want {
				t.Errorf("ceiling = %d from %q, want 2 from %q", n, source, want)
			}
		})
	}

	t.Run("and with nothing in the shell either", func(t *testing.T) {
		t.Setenv("OLLAMA_NUM_PARALLEL", "")
		n, source := ceilingFor("")
		if n != 1 || source != "the default" {
			t.Errorf("ceiling = %d from %q, want 1 from the default", n, source)
		}
	})
}

// TestFollowWorkerEnvironmentSetsTheSeam. The platform decision lives in
// workerEnvFilePath, which takes the platform; this only checks that the
// discoverer a command is handed leaves the process environment behind.
func TestFollowWorkerEnvironmentSetsTheSeam(t *testing.T) {
	d := newDiscoverer()
	if d.ParallelEnv != nil {
		t.Fatal("a discoverer starts as the worker's own: it reads the process environment")
	}
	followWorkerEnvironment(d, filepath.Join(t.TempDir(), "worker.yaml"))
	if d.ParallelEnv == nil {
		t.Error("followWorkerEnvironment left the discoverer reading the process environment")
	}
}

// TestOnlyTheCommandsThatAreNotTheWorkerFollowItsEnvironment. `memql worker
// models` and the closing block of `memql worker setup --inference` print the
// ceiling from a process that is not the worker, so they read the worker's
// file. The worker's own run paths ARE the worker: their os.Getenv is the
// environment, and reading the file there would be a second reading of it.
func TestOnlyTheCommandsThatAreNotTheWorkerFollowItsEnvironment(t *testing.T) {
	_, files := workerSources(t)
	calls := func(name string) int {
		n := 0
		ast.Inspect(files[name], func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "followWorkerEnvironment" {
					n++
				}
			}
			return true
		})
		return n
	}
	for _, file := range []string{"models_cmd.go", "inference_cmd.go"} {
		if calls(file) == 0 {
			t.Errorf("%s prints the ceiling from the owner's shell but never calls followWorkerEnvironment", file)
		}
	}
	for _, file := range []string{"cli.go", "pair.go", "modelinventory.go", "fleet.go", "loop.go"} {
		if n := calls(file); n != 0 {
			t.Errorf("%s runs as the worker, whose process environment is already its own, but calls followWorkerEnvironment %d time(s)", file, n)
		}
	}
}
