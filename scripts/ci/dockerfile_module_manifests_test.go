// Static guard: every nested module's manifest is COPYed into the Docker
// dependency layer, before `go mod download` runs (znasllc-io/memql#3240).
//
// # The failure this exists to prevent
//
// Both Dockerfiles use the standard layer-caching split: `COPY go.mod go.sum`,
// then `go mod download`, then `COPY . .`. That split is what keeps an
// unchanged dependency set from re-downloading on every source edit.
//
// The moment the root go.mod `replace`s a NESTED module by relative path, that
// split breaks: `go mod download` has to read the nested go.mod to resolve the
// replace, and the nested go.mod does not exist yet -- `COPY . .` is two steps
// later. The build dies with
//
//	reading component/bus/gen/go.mod: no such file or directory
//
// # Why a guard and not a fixed Dockerfile
//
// The fix is one COPY line per module, and memql#3228 adds roughly 29 modules
// across five more tasks. "Remember to add a COPY line" is exactly the kind of
// instruction that holds for the first three and then does not.
//
// It is also poorly placed to be caught by eye: the root Dockerfile builds
// every production engine image but is NOT exercised by the pull_request lanes
// (only cmd/deploy-gate-check's is). memql#3240 discovered this because the
// deploy-gate lane happened to fail; the root Dockerfile was broken in exactly
// the same way and silently, and would have surfaced at the next release.
//
// So the assertion walks from go.work -- the one place every module is
// enumerated -- outwards to the Dockerfiles, which is the direction that can
// see an omission.
package ci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dockerfilesNeedingModuleManifests are the Dockerfiles that build Go binaries
// out of the root module and therefore resolve its replace directives.
var dockerfilesNeedingModuleManifests = []string{
	"Dockerfile",
	"cmd/deploy-gate-check/Dockerfile",
}

func TestDockerfilesCopyEveryNestedModuleManifest(t *testing.T) {
	root := repoRootForModuleManifests(t)

	modules := nestedModuleDirsFromGoWork(t, root)
	if len(modules) == 0 {
		t.Fatal("go.work declared no nested modules; this guard reads go.work as the " +
			"module inventory, so an empty result means the parse broke rather than " +
			"that the tree is single-module")
	}

	for _, df := range dockerfilesNeedingModuleManifests {
		path := filepath.Join(root, df)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", df, err)
		}
		body := string(raw)

		beforeDownload, found := bodyBeforeModDownload(body)
		if !found {
			// No dependency-cache layer, so no ordering hazard to guard.
			continue
		}

		for _, mod := range modules {
			// The manifest must be named ANYWHERE before `go mod download`.
			// Matching on the directory keeps this agnostic to whether the
			// author wrote one COPY per module or grouped several.
			want := mod + "/go.mod"
			if !strings.Contains(beforeDownload, want) {
				t.Errorf("%s: %q is not COPYed before `go mod download`.\n\n"+
					"The root go.mod `replace`s it by relative path, so `go mod download` "+
					"must be able to read it -- `COPY . .` is too late and the build fails "+
					"with \"reading %s: no such file or directory\".\n\n"+
					"Add, next to the others:\n"+
					"    COPY %s/go.mod %s/go.sum ./%s/\n",
					df, want, want, mod, mod, mod)
			}
		}
	}
}

// bodyBeforeModDownload returns everything preceding the first `go mod
// download` INSTRUCTION, and whether one was found.
//
// Comment lines are skipped deliberately. A plain strings.Index over the whole
// file matches the prose in the comment that explains why the COPY lines are
// there -- which sits ABOVE them, so every manifest reads as missing and the
// guard fails on a correct Dockerfile. That is exactly how this function's
// first version behaved.
func bodyBeforeModDownload(body string) (string, bool) {
	offset := 0
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") &&
			strings.Contains(line, "go mod download") {
			return body[:offset], true
		}
		offset += len(line) + 1
	}
	return "", false
}

// nestedModuleDirsFromGoWork returns every `use` entry in go.work except the
// root module, repo-relative and slash-separated.
func nestedModuleDirsFromGoWork(t *testing.T, root string) []string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(root, "go.work"))
	if err != nil {
		t.Fatalf("read go.work: %v", err)
	}

	var dirs []string
	inUse := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "//") || line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "use ("):
			inUse = true
			continue
		case inUse && line == ")":
			inUse = false
			continue
		case strings.HasPrefix(line, "use "):
			// Single-entry form: `use ./foo`.
			dirs = appendModuleDir(dirs, strings.TrimSpace(strings.TrimPrefix(line, "use ")))
			continue
		}
		if inUse {
			dirs = appendModuleDir(dirs, line)
		}
	}
	return dirs
}

func appendModuleDir(dirs []string, entry string) []string {
	entry = strings.Trim(entry, `"`)
	entry = strings.TrimPrefix(entry, "./")
	if entry == "" || entry == "." {
		return dirs
	}
	return append(dirs, entry)
}

// repoRootForModuleManifests walks up from the test's working directory to the
// directory holding go.work.
func repoRootForModuleManifests(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("walked to the filesystem root without finding go.work")
		}
		dir = parent
	}
}
