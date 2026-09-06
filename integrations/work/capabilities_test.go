package work

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
)

// TestCapabilityNamesMatchTheDSL is the gate that makes the seven builtins in
// dsl/work/builtins.memql actually reachable.
//
// A capability the DSL names and the registry LACKS is not a quiet no-op: it
// is a boot-time resolution failure on every node type, because
// AuditIntegrationExecutors resolves every enabled integration.* builtin
// against the registry at Init and strict boot refuses the report. And a
// capability the registry holds that the DSL does not name is dead code that
// nothing can call.
//
// So the assertion is SET EQUALITY, read out of the .memql file rather than
// restated here -- a list written twice is a list that disagrees.
func TestCapabilityNamesMatchTheDSL(t *testing.T) {
	declared := executorsDeclaredInDSL(t)
	if len(declared) == 0 {
		t.Fatal("found no integration.work.* executors in dsl/work/builtins.memql, so this test asserts nothing")
	}

	registered := map[string]bool{}
	for _, c := range (&Integration{}).Capabilities() {
		if registered[c.Name] {
			t.Errorf("capability %q is registered twice", c.Name)
		}
		registered[c.Name] = true
		if strings.TrimSpace(c.Description) == "" {
			t.Errorf("capability %q has no description; it is what the functions() introspection shows", c.Name)
		}
		if c.Handler == nil {
			t.Errorf("capability %q has no handler", c.Name)
		}
	}

	for name := range declared {
		if !registered[name] {
			t.Errorf("dsl/work/builtins.memql declares @executor(\"integration.work.%s\") and nothing registers it -- strict boot refuses a builtin whose executor does not resolve, on EVERY node type", name)
		}
	}
	for name := range registered {
		if !declared[name] {
			t.Errorf("integration.work.%s is registered and no builtin declares it; nothing can call it", name)
		}
	}
}

// TestRegistrationNameIsTheLiteral. The module-taxonomy gate finds plug-in
// registrations by scanning source for the STRING LITERAL, because a computed
// name could not be classified at PR time -- so the literal and the constant
// must agree or the classification is about a name nothing registers.
func TestRegistrationNameIsTheLiteral(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(packageDir(t), "integration.go"))
	if err != nil {
		t.Fatalf("read integration.go: %v", err)
	}
	want := `memql.RegisterPlugin("` + integrationName + `"`
	if !strings.Contains(string(src), want) {
		t.Errorf("integration.go does not register the literal %q; the taxonomy gate scans for it and integrationName is %q", want, integrationName)
	}
}

// TestSweepAutomationsAreOnTheMaintenanceList.
//
// Both sweeps read every owner's rows, and the composite tier admits that only
// to a cluster owner. The scheduled automations clear the handler floor
// BECAUSE component/auth/maintenance_actor.go names them; drop either entry
// and the automation's default RoleReader actor is refused by the very sweep
// it exists to run, every night, with one line in one replica's log as the
// only sign.
//
// The names are read out of dsl/work/automations.memql rather than restated,
// so a renamed automation fails here rather than silently losing its principal.
func TestSweepAutomationsAreOnTheMaintenanceList(t *testing.T) {
	names := automationNamesInDSL(t)
	if len(names) != 2 {
		t.Fatalf("expected 2 automations in dsl/work/automations.memql, found %v", names)
	}
	for _, name := range names {
		if !auth.IsMaintenanceAutomation(name) {
			t.Errorf("automation %q is not on component/auth/maintenance_actor.go's list; under the default reader actor its reads answer ZERO ROWS AND NO ERROR, and a sweep that does nothing looks exactly like a cluster with nothing to do", name)
		}
	}
}

// ---------------------------------------------------------------------------

var executorPattern = regexp.MustCompile(`@executor\("integration\.work\.([A-Za-z0-9_]+)"\)`)

func executorsDeclaredInDSL(t *testing.T) map[string]bool {
	t.Helper()
	src := readRepoFile(t, filepath.Join("dsl", "work", "builtins.memql"))
	out := map[string]bool{}
	for _, m := range executorPattern.FindAllStringSubmatch(src, -1) {
		out[m[1]] = true
	}
	return out
}

var automationPattern = regexp.MustCompile(`(?m)^automation\s+([A-Za-z0-9_]+)\s*\{`)

func automationNamesInDSL(t *testing.T) []string {
	t.Helper()
	src := readRepoFile(t, filepath.Join("dsl", "work", "automations.memql"))
	var out []string
	for _, m := range automationPattern.FindAllStringSubmatch(src, -1) {
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

// readRepoFile reads a path relative to the repository root.
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	dir := packageDir(t)
	for range 6 {
		candidate := filepath.Join(dir, rel)
		if raw, err := os.ReadFile(candidate); err == nil {
			return string(raw)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not find %s walking up from %s", rel, packageDir(t))
	return ""
}

func packageDir(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate the package directory")
	}
	return filepath.Dir(self)
}
