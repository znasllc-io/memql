package procedure

import (
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// allowedNonStdlib is the exemption list, and it is EMPTY.
//
// The design record allows this module the standard library and
// component/work. It turned out to need only the first: every goalSignature
// the pipeline groups by is read off a row by integrations/procedure rather
// than derived here, so nothing imports the work spine and `go mod tidy`
// removes a require that says otherwise. The narrower truth is what ships,
// and it is a stronger claim than the record asked for.
//
// TO WIDEN IT: if a function here ever needs work.GoalSignature -- computing a
// signature rather than being handed one -- add
// "github.com/znasllc-io/memql/component/work" here AND the require to
// go.mod, in the same change. That is the ONE import the record permits; a
// second entry is a different decision and belongs in a different review.
var allowedNonStdlib = map[string]bool{}

// TestProcedureImportsNothingBeyondStdlibAndWork is what makes "induction
// spends no model" checkable rather than promised (design record
// docs/superpowers/specs/2026-09-13-app-session-recording-and-learning-program-design.md,
// decision D6).
//
// The claim this epic rests on is that a recorded corpus becomes a
// parameterized construct with NO provider call. A package that could reach a
// provider, an engine or a database is one nobody can check without running
// them. `go list -deps` is the build graph, and the build graph has no
// opinions.
//
// # Why a module and not a package boundary
//
// component/proving answers the same question with a package boundary plus a
// gate, and its own comment explains why: a nested module fires roughly twelve
// repo-wide gates. That answer is not available here. integrations/procedure
// must import this code, `integrations` is its own module, and it does not
// require the root module -- so a root-module package is unreachable from it
// with GOWORK=off, which is exactly the mode the module-boundaries lane runs.
// The twelve-gate tax is paid for that reason and no other.
//
// # What this deliberately does NOT catch
//
// A function in this module that is WRONG. Purity is a claim about reach, not
// about correctness; the golden tests beside it are the correctness half, and
// neither substitutes for the other.
func TestProcedureImportsNothingBeyondStdlibAndWork(t *testing.T) {
	const pkg = "github.com/znasllc-io/memql/component/procedure"
	out, err := exec.Command("go", "list", "-deps", pkg).Output()
	if err != nil {
		t.Fatalf("go list -deps %s: %v", pkg, err)
	}
	var offenders []string
	for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		dep = strings.TrimSpace(dep)
		switch {
		case dep == "" || dep == pkg:
		case allowedNonStdlib[dep]:
		case isStdlibPath(dep):
		default:
			offenders = append(offenders, dep)
		}
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("component/procedure imports outside the standard library:\n  %s\n\n"+
			"This module's whole value is that its decisions can be checked without running "+
			"anything -- no engine, no database, no provider. If the import is genuinely "+
			"needed, the code belongs in integrations/procedure, which may import anything.",
			strings.Join(offenders, "\n  "))
	}
}

// isStdlibPath reports whether an import path is in the standard library. The
// heuristic is the usual one and it is exact enough: a standard-library path's
// first segment carries no dot, because a module path's does.
func isStdlibPath(p string) bool {
	first, _, _ := strings.Cut(p, "/")
	return !strings.Contains(first, ".")
}
