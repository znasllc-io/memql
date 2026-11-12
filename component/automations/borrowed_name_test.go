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
//   - the name reaches the engine's automation, in the loaded set, through
//     LoadByName and through the scheduler's registry, whatever the order the
//     two were loaded in;
//   - the node still starts: the other copy is left out, not a load refusal.

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

// Loaded the way a node loads it: a mounted runtime domain declares an
// automation under a name the engine's own tree also declares. The node loads
// (no refusal), the name belongs to the engine's automation alone, and that
// automation keeps the principal.
func TestABorrowedNameLeavesTheEnginesAutomationAndItsPrincipal(t *testing.T) {
	mountBorrowedDomain(t, borrowedName)

	loaded, err := NewLoader(LoaderOptions{}).LoadAll()
	if err != nil {
		t.Fatalf("a runtime domain holding an engine automation's name must not refuse the load: %v", err)
	}
	var named []*Automation
	for _, a := range loaded {
		if a != nil && a.Name == borrowedName {
			named = append(named, a)
		}
	}
	for _, a := range named {
		if !isEngineCopy(a) {
			ac := runAs(t, a)
			t.Errorf("the load kept %s from %s, which runs as %q (role %q, cluster owner %v): the name is the engine's own automation's",
				a.Name, a.Origin, ac.UserId, ac.Role, ac.IsClusterOwner())
		}
	}
	if len(named) != 1 || !isEngineCopy(named[0]) {
		t.Fatalf("%d automations named %s loaded, want exactly the engine's", len(named), borrowedName)
	}
	if ac := runAs(t, named[0]); ac.UserId != "system:maintenance:"+borrowedName || !ac.IsClusterOwner() {
		t.Errorf("the engine's %s ran as %q (cluster owner %v), want its maintenance principal", borrowedName, ac.UserId, ac.IsClusterOwner())
	}

	// A call by name reaches the engine's automation.
	byName, err := NewLoader(LoaderOptions{}).LoadByName(borrowedName)
	if err != nil || !isEngineCopy(byName) {
		t.Fatalf("LoadByName(%s) = %v, %v; want the engine's", borrowedName, byName, err)
	}

	// And so does the scheduler's registry, which is what a trigger by name,
	// a sub-automation call and a resume read.
	s, err := NewScheduler(SchedulerOptions{Loader: NewLoader(LoaderOptions{}), Engine: &memql.MemQLEngine{}, StepRegistry: &actorProbe{}})
	if err != nil {
		t.Fatalf("NewScheduler: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	defer s.Stop(context.Background())
	select {
	case <-s.Ready():
	case <-time.After(30 * time.Second):
		t.Fatal("scheduler did not become ready")
	}
	if got := s.LookupAutomation(borrowedName); got == nil || !isEngineCopy(got) {
		origin := "<none>"
		if got != nil {
			origin = got.Origin
		}
		t.Fatalf("the scheduler registered %s from %s, want the engine's", borrowedName, origin)
	}
}

// A runtime domain's automation with a name of its own still loads: the rule is
// about the engine's names, not about runtime domains.
func TestARuntimeDomainsOwnAutomationStillLoads(t *testing.T) {
	const own = "zzBorrowedNameControlSweep"
	mountBorrowedDomain(t, own)
	loaded, err := NewLoader(LoaderOptions{}).LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	for _, a := range loaded {
		if a != nil && a.Name == own {
			if ac := runAs(t, a); ac.UserId != "system:automation:"+own || ac.IsClusterOwner() {
				t.Errorf("%s ran as %q (cluster owner %v), want the ordinary system actor", own, ac.UserId, ac.IsClusterOwner())
			}
			return
		}
	}
	t.Fatalf("%s did not load from the runtime domain", own)
}

// Whichever was read first, the engine's automation keeps its name, and the
// other is the one left out. Two automations of other layers that share a name
// are not this rule's business.
func TestTheEnginesAutomationKeepsItsNameWhateverTheOrder(t *testing.T) {
	engine := &Automation{Name: borrowedName, Origin: "unified:campaigns/automations.memql:" + borrowedName, Source: memqldsl.SourceEngine}
	other := &Automation{Name: borrowedName, Origin: "unified:" + borrowedDomain + "/automations.memql:" + borrowedName, Source: memqldsl.SourceRuntime}
	pack := &Automation{Name: borrowedName, Origin: "unified:zzpack/automations.memql:" + borrowedName, Source: memqldsl.SourcePack}
	own := &Automation{Name: "zzOwnName", Source: memqldsl.SourceRuntime}
	for _, order := range [][]*Automation{{engine, other, pack, own}, {own, pack, other, engine}} {
		kept, left := engineNamesWin(order)
		if len(kept) != 2 || len(left) != 2 {
			t.Fatalf("kept %d and left out %d, want the engine's and the unrelated one kept", len(kept), len(left))
		}
		for _, a := range kept {
			if a != engine && a != own {
				t.Errorf("kept %s (source %s)", a.Origin, a.Source)
			}
		}
		for _, a := range left {
			if a.Source == memqldsl.SourceEngine {
				t.Errorf("left out the engine's own automation")
			}
		}
	}
	a, b := &Automation{Name: "zzShared", Source: memqldsl.SourceRuntime}, &Automation{Name: "zzShared", Source: memqldsl.SourcePack}
	if kept, left := engineNamesWin([]*Automation{a, b}); len(kept) != 2 || len(left) != 0 {
		t.Errorf("two automations of other layers sharing a name: kept %d, left out %d; want both kept", len(kept), len(left))
	}
}

// The loader records the layer each automation was read from: the engine's
// tree for the engine's, the runtime layer for a mounted domain's.
func TestTheLoaderRecordsWhereAnAutomationCameFrom(t *testing.T) {
	const own = "zzBorrowedNameSourceSweep"
	mountBorrowedDomain(t, own)
	loaded, err := NewLoader(LoaderOptions{}).LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	seen := 0
	for _, a := range loaded {
		switch {
		case a.Name == own:
			seen++
			if a.Source != memqldsl.SourceRuntime {
				t.Errorf("%s from %s recorded source %s, want runtime", a.Name, a.Origin, a.Source)
			}
		case isEngineCopy(a) && a.Name == borrowedName:
			seen++
			if a.Source != memqldsl.SourceEngine {
				t.Errorf("%s from %s recorded source %s, want engine", a.Name, a.Origin, a.Source)
			}
		}
	}
	if seen != 2 {
		t.Fatalf("found %d of the two automations this test reads", seen)
	}
	// A snapshot keeps it.
	for _, a := range loaded {
		if a.Name == own {
			snap, err := NewLoader(LoaderOptions{}).Snapshot(a)
			if err != nil || snap.Source != a.Source {
				t.Fatalf("Snapshot: source %v, error %v; want %s", snap, err, a.Source)
			}
		}
	}
}
