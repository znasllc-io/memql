// Package envregistry owns MemQL's environment-variable REGISTRY: the
// manifest that names every variable the platform knows about, the boot-time
// validation that refuses to start a node missing a required one, the
// domain derivations that compose a node's issuer / CORS origins / redirect
// URIs from MEMQL_DOMAIN, and the legacy-alias shim that bridges pre-rename
// spellings.
//
// It was called `genesis` until memql#3963, and the rename is the point. The
// old package was two unrelated things sharing a directory and a module: this
// registry, and the sealed-envelope machinery that read a `.env`, encrypted it
// under MEMQL_MASTER_KEY and autoloaded the result into a process's
// environment at boot. Only the second half was ever "genesis", and only the
// second half is being deleted (epic memql#3958) -- so the name went with it
// and the surviving half got one that says what it does.
//
// Consumers: app/config.go (boot validation), app/engine.go,
// component/memql/default_injector.go (first-deploy injection),
// cmd/envscan (the drift gate), component/frontdoor, and main.go's
// boot sequence.
//
// TWO COPIES OF manifest.yaml EXIST AND BOTH ARE REAL. scripts/secrets/manifest.yaml
// is the AUTHORED source of truth; the copy in this directory is the embedded
// snapshot compiled into every binary. scripts/secrets/sync-embedded-manifest.sh
// (`make env-registry-sync`) regenerates this one from that one, and
// TestEmbeddedManifestInSync fails CI on drift. Edit the authored copy.
package envregistry

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// EnvEntry is one parsed line: a key, its value, and the original
// source line number (used in validation error messages). Order is
// preserved as the file is read.
type EnvEntry struct {
	Name  string
	Value string
	Line  int
}

// ParseEnvFile reads a .env file from disk. Skips comments and blank
// lines; tolerates an optional `export ` prefix; strips matching
// single or double quotes around the value. Returns a typed error
// for malformed input (missing `=`, empty key).
func ParseEnvFile(path string) ([]EnvEntry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("envfile: open %s: %w", path, err)
	}
	defer f.Close()
	return parseEnv(f, path)
}

func parseEnv(r io.Reader, label string) ([]EnvEntry, error) {
	var out []EnvEntry
	scanner := bufio.NewScanner(r)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return nil, fmt.Errorf("envfile: %s:%d: line lacks '=': %q", label, lineNo, line)
		}
		name := strings.TrimSpace(line[:eq])
		value := line[eq+1:]
		if name == "" {
			return nil, fmt.Errorf("envfile: %s:%d: empty key", label, lineNo)
		}
		if len(value) >= 2 {
			first, last := value[0], value[len(value)-1]
			if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		out = append(out, EnvEntry{Name: name, Value: value, Line: lineNo})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("envfile: %s: scan: %w", label, err)
	}
	return out, nil
}
// ParseEnvReader was here, and was deleted in memql#4055 rather than
// unexported. It wrapped parseEnv, which ParseEnvFile already calls, so
// unexporting would have left an unreferenced private wrapper -- and the
// pre-release rule is to delete what is no longer needed rather than keep a
// shim. Its own doc comment set the retirement condition ("it becomes
// unreferenced again when the envelope goes, memql#3966"), epic memql#3958
// deleted the envelope, and the condition was met. parseEnv is the reader-shaped
// entry point a future caller should re-export from, not resurrect from git.
