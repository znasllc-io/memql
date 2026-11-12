package automations

// borrowed_name_test.go -- the cluster's maintenance principal is the engine's
// own (component/auth/maintenance_actor.go), and an automation's NAME is not
// where it comes from.
//
// A runtime domain -- what MEMQL_DSL_PATH mounts, and so what every package
// staged through Deployables is -- declares an automation that carries the name
// of one of the engine's maintenance automations. What must hold:
//
//   - it does not run as the maintenance principal: it runs as the ordinary
//     system actor of an automation that is not the engine's;
//   - the engine's own automation of that name still does;

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

// borrowedName is a listed maintenance automation of the engine's own tree
// (dsl/campaigns/automations.memql).
const borrowedName = "emailStatisticsRollupSweep"

// borrowedDomain is the runtime domain the tests mount.
const borrowedDomain = "zzborrowedname"

func borrowedSource(name string) string {
	return "use campaigns.queries.{ recentSendJobs }\n\n" +
		"/// A runtime domain's automation.\n" +
		"@trigger(schedule=\"0 0 3 * * *\")\n" +
		"automation " + name + " {\n" +
		"  jobs := query recentSendJobs()\n" +
		"}\n"
}

// mountBorrowedDomain mounts a runtime domain the way a node mounts a staged
// package: a directory under MEMQL_DSL_PATH, through MountRuntimeDomainsFromEnv.
func mountBorrowedDomain(t *testing.T, automationName string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, borrowedDomain)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memql.toml"), []byte("memql = \"1.0\"\nedition = \"2026\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "automations.memql"), []byte(borrowedSource(automationName)), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEMQL_DSL_PATH", root)
	if got := memqldsl.MountRuntimeDomainsFromEnv(nil); len(got) != 1 || got[0] != borrowedDomain {
		t.Fatalf("mounted %v, want [%s]", got, borrowedDomain)
	}
	t.Cleanup(func() { memqldsl.UnregisterTree(borrowedDomain) })
	if _, err := memql.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts: %v", err)
	}
}

// actorProbe records the principal each step ran as.
type actorProbe struct {
	mu     sync.Mutex
	actors []*auth.AccessContext
}

func (p *actorProbe) Execute(ctx context.Context, step *Step, _ *StepContext) (*StepResult, error) {
	ac, _ := auth.AccessFromContext(ctx)
	p.mu.Lock()
	p.actors = append(p.actors, ac)
	p.mu.Unlock()
	now := time.Now()
	return &StepResult{StepId: step.ID, Status: "completed", Result: map[string]any{}, StartedAt: now, CompletedAt: now}, nil
}

// runAs runs a and answers the principal its one step ran as.
func runAs(t *testing.T, a *Automation) *auth.AccessContext {
	t.Helper()
	probe := &actorProbe{}
	e := NewExecutor(ExecutorOptions{StepRegistry: probe})
	defer e.Close()
	if _, err := e.Execute(context.Background(), a, "manual"); err != nil {
		t.Fatalf("run %s (%s): %v", a.Name, a.Origin, err)
	}
	if len(probe.actors) != 1 || probe.actors[0] == nil {
		t.Fatalf("run %s (%s): recorded %v, want one step's principal", a.Name, a.Origin, probe.actors)
	}
	return probe.actors[0]
}

func isEngineCopy(a *Automation) bool {
	return strings.HasPrefix(a.Origin, "unified:campaigns/")
}

// An automation that is not the engine's runs as the ordinary system actor,
// whatever name it carries: for every name on the list, the principal it runs
// as is `system:automation:<name>` with the reader role, never the
// maintenance principal.
func TestAnAutomationThatIsNotTheEnginesRunsAsTheOrdinaryActor(t *testing.T) {
	for _, name := range auth.MaintenanceAutomationNames() {
		a, err := NewLoader(LoaderOptions{}).CompileSource(
			"@template\nautomation "+name+" {\n  run := logic probeLogic()\n}\n", borrowedDomain+"/automations.memql")
		if err != nil {
			t.Fatalf("compile %s: %v", name, err)
		}
		ac := runAs(t, a)
		if ac.IsClusterOwner() || strings.HasPrefix(ac.UserId, "system:maintenance:") {
			t.Errorf("%s from %s ran as %q with role %q, a cluster owner: the maintenance principal is the engine's own automation's, never a name's",
				name, a.Origin, ac.UserId, ac.Role)
			continue
		}
		if ac.UserId != "system:automation:"+name || ac.Role != auth.RoleReader {
			t.Errorf("%s from %s ran as %q with role %q, want the ordinary system actor", name, a.Origin, ac.UserId, ac.Role)
		}
	}
}
