package worker

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/znasllc-io/memql-cockpit/internal/worker/models"
)

// The worker's environment, as a command that is not the worker can see it.
//
// `memql worker models` runs in the owner's shell, and the worker does not: on
// Linux it runs under a systemd user unit whose environment is whatever
// EnvironmentFile=-~/.memql/worker.env loads, and nothing the shell exported. A
// ceiling read from the shell's OLLAMA_NUM_PARALLEL names a number the worker
// does not advertise, and the report exists to say what the worker advertises.
//
// So a command reads the worker's file instead, and when there is none it says
// the figure is its own. The worker's own log carries what it actually
// advertises -- announceModelCeilings, one line per served model, at start and
// after every reload -- and that is the record that cannot differ.

// workerEnvFileName is the file beside worker.yaml that the Linux installer
// creates and the unit loads (scripts/install/install-linux.sh and
// deploy/systemd/memql-worker.service both name it).
const workerEnvFileName = "worker.env"

// workerEnvFilePath is the file the worker's unit loads, found beside the
// worker.yaml the command was pointed at the way policy.yaml is, or empty
// where no unit loads one. That is every platform but Linux: a macOS
// LaunchAgent's plist sets HOME and PATH and names no environment file, so
// reading one there would describe an environment nothing starts the worker
// with.
func workerEnvFilePath(goos, configPath string) string {
	if goos != "linux" {
		return ""
	}
	return filepath.Join(filepath.Dir(configPath), workerEnvFileName)
}

// followWorkerEnvironment points a discoverer built for a COMMAND at the
// worker's environment instead of the command's own. It is for the commands
// that print the ceiling from the owner's shell. The worker's run paths do not
// call it: their process environment is the worker's.
func followWorkerEnvironment(d *models.Discoverer, configPath string) {
	d.ParallelEnv = workerParallelEnv(workerEnvFilePath(runtime.GOOS, configPath), os.Getenv)
}

// workerParallelEnv is models.Discoverer.ParallelEnv for a command: the
// worker's OLLAMA_NUM_PARALLEL from its environment file when there is one,
// otherwise the command's own and said to be.
//
// A file that exists is the whole of what the unit loads, so a variable it does
// not set is one the worker does not have, whatever the shell holds. A file
// that is missing or cannot be read says nothing about the worker, and the
// shell is then the only view left; the sentence does not claim it is the
// worker's.
//
// The file is read as it is NOW. The worker took its environment when it
// started, so an edit since then is not in the worker yet; restarting the
// worker is what puts it there.
func workerParallelEnv(envFile string, getenv func(string) string) func() (string, string) {
	return func() (string, string) {
		if envFile != "" {
			if text, err := os.ReadFile(envFile); err == nil {
				return parseEnvironmentFile(string(text))["OLLAMA_NUM_PARALLEL"],
					"the worker's environment file (" + homeRelative(envFile) + ")"
			}
		}
		return getenv("OLLAMA_NUM_PARALLEL"), "this command's environment (the worker's own may differ)"
	}
}

// parseEnvironmentFile reads the text of a systemd EnvironmentFile=: one
// NAME=value per line, blank lines and lines starting with # or ; skipped,
// whitespace around the name and the value dropped, a matching pair of single or
// double quotes around a value removed (and whitespace inside it kept), and the
// last assignment of a name wins. A line without an equals sign, or whose name
// systemd would refuse, is ignored.
//
// It is the cases an owner's worker.env contains, not all of systemd's syntax:
// a backslash at the end of a line does not continue it, and escapes inside a
// value are not interpreted.
func parseEnvironmentFile(text string) map[string]string {
	out := map[string]string{}
	// systemd ends a line at a newline or a carriage return.
	for _, line := range strings.FieldsFunc(text, func(r rune) bool { return r == '\n' || r == '\r' }) {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		name = strings.TrimSpace(name)
		if !ok || !validEnvName(name) {
			continue
		}
		out[name] = unquoteEnvValue(strings.TrimSpace(value))
	}
	return out
}

// unquoteEnvValue drops one matching pair of quotes around a value.
func unquoteEnvValue(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}

// validEnvName is a name systemd accepts for a variable: ASCII letters, digits
// and underscores, and not starting with a digit.
func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c == '_', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// homeRelative writes path the way an owner types it, with the home directory
// as ~, and unchanged when it is not under the home directory.
func homeRelative(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if rest, ok := strings.CutPrefix(path, strings.TrimRight(home, string(filepath.Separator))+string(filepath.Separator)); ok {
		return "~/" + rest
	}
	return path
}
