package pipelines

import (
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// TestPipelinesImportNothingBeyondStdlib makes "a pure package with no engine
// and no database, the way component/work is built" (epic memql#5477, task
// memql#5489) checkable rather than promised.
//
// The design record's testing section rests on it: the manifest compiler, the
// affected-set computation and the event-to-mode table are tested on fixtures
// with nothing running. A package that could reach an engine, a database or
// the network is one nobody can check that way. `go list -deps` is the build
// graph, and the build graph has no opinions.
//
// The exemption list is EMPTY, and the YAML decoder is the reason it can be:
// the spec types carry yaml tags but import no YAML package, because
// component/packages decodes the whole memql-package.yaml with one strict
// decoder and hands this package the typed block. A second decoder here would
// be a second reading of one file.
func TestPipelinesImportNothingBeyondStdlib(t *testing.T) {
	const pkg = "github.com/znasllc-io/memql/component/pipelines"
	out, err := exec.Command("go", "list", "-deps", pkg).Output()
	if err != nil {
		t.Fatalf("go list -deps %s: %v", pkg, err)
	}
	var offenders []string
	for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		dep = strings.TrimSpace(dep)
		switch {
		case dep == "" || dep == pkg:
		case isStdlibPath(dep):
		default:
			offenders = append(offenders, dep)
		}
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("component/pipelines imports outside the standard library:\n  %s\n\n"+
			"This module's value is that its decisions can be checked without running "+
			"anything. If the import is genuinely needed, the code belongs in "+
			"component/pipelinerun, which may import anything.",
			strings.Join(offenders, "\n  "))
	}
}

// isStdlibPath reports whether an import path is in the standard library: a
// standard-library path's first segment carries no dot, a module path's does.
func isStdlibPath(p string) bool {
	first, _, _ := strings.Cut(p, "/")
	return !strings.Contains(first, ".")
}
