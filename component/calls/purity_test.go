package calls

import (
	"os/exec"
	"sort"
	"strings"
	"testing"
)

// TestPackageImportsOnlyTheStandardLibrary makes "the call rules decide
// without running anything" checkable. Admission, queue order, the host-left
// countdown and schedule validation are consulted on hot paths and from
// tests; a package that could reach an engine, a database or a clock could
// not be checked without them. `go list -deps` is the build graph and has no
// opinions. It checks reach, not correctness; the table tests beside it are
// the correctness half.
func TestPackageImportsOnlyTheStandardLibrary(t *testing.T) {
	const pkg = "github.com/znasllc-io/memql/component/calls"
	out, err := exec.Command("go", "list", "-deps", pkg).Output()
	if err != nil {
		t.Fatalf("go list -deps %s: %v", pkg, err)
	}
	var offenders []string
	for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		dep = strings.TrimSpace(dep)
		if dep == "" || dep == pkg {
			continue
		}
		first, _, _ := strings.Cut(dep, "/")
		if strings.Contains(first, ".") {
			offenders = append(offenders, dep)
		}
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("component/calls imports outside the standard library:\n  %s\n\n"+
			"If the import is genuinely needed, the code belongs in an integration, "+
			"which may import anything.", strings.Join(offenders, "\n  "))
	}
}
