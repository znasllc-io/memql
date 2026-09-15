package automations

// loop_check_test.go -- where the static loop graph runs (memql#5381, D-J):
// the loader's check after its walk (report-only for now), StaticGraph, and
// the authored scheduler's check at activation.

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/znasllc-io/memql/component/events"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/dslfs"
)

// forgeFunctions are forge's routing constructs as the loader builds them.
func forgeFunctions(t *testing.T) *memql.FunctionRegistry {
	return functionsFromTree(t, map[string][]string{
		"forge/mutations.memql": {"advanceRequest", "recordRequestEvent"},
		"forge/logic.memql":     {"requestRouteStatus"},
	})
}

// loopTree is a one-domain DSL tree holding src as its automations file.
func loopTree(src string) fstest.MapFS {
	line := dslfs.Manifest{Language: languageParser.LanguageVersion, Edition: languageParser.Edition}
	return fstest.MapFS{
		"loopcheck/" + dslfs.ManifestFile: {Data: []byte(line.Render())},
		"loopcheck/automations.memql":     {Data: []byte(src)},
	}
}

// selfUpdating advances the request it fires on, with no filter.
const selfUpdating = `@trigger(event="node.updated", concept="v1:forge:request")
automation reAdvance {
  args {
    id any
  }
  advance := mutation advanceRequest(requestId: args.id, status: "queued")
}
`

func captureLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

func TestCheckLoops_AProblemOnTheAutomationsFile(t *testing.T) {
	t.Setenv(memql.AllowSkipsEnvVar, "")
	logger, logs := captureLogger()
	l := NewLoader(LoaderOptions{Logger: logger, Functions: forgeFunctions(t)})
	loaded, err := l.LoadFromTree(loopTree(selfUpdating))
	// REPORT-ONLY: the load is not refused for the cycle yet, and says so.
	if err != nil {
		t.Fatalf("the loop check is report-only, yet the load was refused: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("loaded %d automations, want 1", len(loaded))
	}
	if !strings.Contains(logs.String(), "static loop analysis: problem (report-only") || !strings.Contains(logs.String(), "[loop_cycle]") {
		t.Errorf("the problem was not logged:\n%s", logs.String())
	}
	// The node's loader re-walks the tree on every LoadByName; the check is
	// logged by its first load only.
	if _, err := l.LoadFromTree(loopTree(selfUpdating)); err != nil {
		t.Fatalf("second load: %v", err)
	}
	if n := strings.Count(logs.String(), "static loop analysis: problem"); n != 1 {
		t.Errorf("the problem was logged %d times over two loads, want once:\n%s", n, logs.String())
	}

	g, problems := l.checkLoops(loaded)
	if len(problems) != 1 {
		t.Fatalf("problems = %+v, want one", problems)
	}
	p := problems[0]
	if p.Path != "loopcheck/automations.memql" || p.Name != "reAdvance" || p.Phase != "loops" {
		t.Errorf("problem = %+v, want loopcheck/automations.memql:reAdvance [loops]", p)
	}
	if !strings.HasPrefix(p.Err, "automation cycle not covered by @loop: reAdvance -> reAdvance") || !strings.HasSuffix(p.Err, "[loop_cycle]") {
		t.Errorf("err = %s", p.Err)
	}
	if g.Coverage.Resolved != 1 || g.Coverage.Unresolved != 0 {
		t.Errorf("coverage = %+v, want the one call resolved", g.Coverage)
	}
	// The strict gate's rendering, which the flip will print at boot.
	if got := formatAutomationProblems(problems); !strings.Contains(got, "loopcheck/automations.memql:reAdvance [loops] automation cycle not covered by @loop") {
		t.Errorf("rendered as:\n%s", got)
	}
}

// Without a function registry the check does not run -- and the load says
// so, rather than reporting a tree with no cycles.
func TestCheckLoops_NotRunWithoutFunctions(t *testing.T) {
	t.Setenv(memql.AllowSkipsEnvVar, "")
	logger, logs := captureLogger()
	l := NewLoader(LoaderOptions{Logger: logger})
	if _, err := l.LoadFromTree(loopTree(selfUpdating)); err != nil {
		t.Fatalf("load: %v", err)
	}
	if !strings.Contains(logs.String(), "static loop analysis not run: the loader has no function registry") {
		t.Errorf("the load did not say the check did not run:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), "[loop_cycle]") {
		t.Errorf("a check that did not run reported a problem:\n%s", logs.String())
	}
	if _, err := l.StaticGraph(); err == nil || !strings.Contains(err.Error(), "no function registry") {
		t.Errorf("StaticGraph without a function registry = %v, want a refusal", err)
	}
}

// A @loop on an automation in no cycle is not a problem, but is logged.
func TestCheckLoops_LoopInNoCycleWarns(t *testing.T) {
	logger, logs := captureLogger()
	l := NewLoader(LoaderOptions{Logger: logger, Functions: forgeFunctions(t)})
	a := withLoop(graphAutomation(t, `@trigger(event="node.created", concept="v1:forge:requestEvent")
automation settle {
  args {
    id any
  }
  advance := mutation advanceRequest(requestId: args.id, status: "queued")
}`))
	g, problems := l.checkLoops([]*Automation{a})
	l.logLoopCheck(g, problems)
	if len(problems) != 0 {
		t.Fatalf("problems = %+v, want none", problems)
	}
	if !strings.Contains(logs.String(), "@loop on an automation in no cycle") {
		t.Errorf("the lone @loop was not logged:\n%s", logs.String())
	}
}

func TestAutomationFile(t *testing.T) {
	for origin, want := range map[string]string{
		"unified:forge/automations.memql:routeRequest":     "forge/automations.memql",
		"unified:shop:v2/automations.memql:onOrderCreated": "shop:v2/automations.memql",
		"authored:v1:identity:user:alice:mine":             "authored:v1:identity:user:alice:mine",
	} {
		name := origin[strings.LastIndex(origin, ":")+1:]
		if got := automationFile(origin, name); got != want {
			t.Errorf("automationFile(%q) = %q, want %q", origin, got, want)
		}
	}
}

// authoredForgeScheduler is an authored scheduler whose loader carries forge's
// constructs, judging candidates beside a shipped routeRequest in the shape it
// had before its first-version filter (unfilteredRouteRequest) -- itself a
// cycle, which no candidate may be refused for merely sitting beside.
func authoredForgeScheduler(t *testing.T, logger *slog.Logger, withFunctions bool) *AuthoredScheduler {
	t.Helper()
	opts := LoaderOptions{Logger: logger}
	if withFunctions {
		opts.Functions = forgeFunctions(t)
	}
	s, err := NewAuthoredScheduler(AuthoredSchedulerOptions{
		Logger:   logger,
		Loader:   NewLoader(opts),
		EventBus: events.NewBus(),
		Run:      func(context.Context, *Automation, *events.Event) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Stop)
	s.shipped = graphAutomations(t, unfilteredRouteRequest)
	s.shippedLoaded = true
	return s
}

func authoredLoopConstruct(name, src string) *memql.AuthoredConstruct {
	return &memql.AuthoredConstruct{OwnerUserId: "v1:identity:user:alice", Kind: "automation", Name: name, Version: 1, Source: src}
}

func TestAuthoredActivate_RefusesACandidateThatClosesACycle(t *testing.T) {
	s := authoredForgeScheduler(t, nil, true)

	err := s.Activate(authoredLoopConstruct("reAdvance", selfUpdating))
	if err == nil {
		t.Fatal("a self-updating authored automation activated")
	}
	for _, want := range []string{"authored:v1:identity:user:alice:reAdvance", "automation cycle not covered by @loop: reAdvance -> reAdvance", "[loop_cycle]"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%v", want, err)
		}
	}
	if s.IsActive("v1:identity:user:alice", "reAdvance") {
		t.Error("a refused automation was wired anyway")
	}

	// A candidate that closes a cycle THROUGH a shipped automation: it fires
	// on the requestEvent routeRequest records, and advances the request
	// routeRequest fires on.
	err = s.Activate(authoredLoopConstruct("reRoute", `@trigger(event="node.created", concept="v1:forge:requestEvent")
automation reRoute {
  args {
    requestId any
  }
  advance := mutation advanceRequest(requestId: args.requestId, status: "queued")
}
`))
	if err == nil || !strings.Contains(err.Error(), "[loop_cycle]") || !strings.Contains(err.Error(), "routeRequest") {
		t.Fatalf("a candidate closing a cycle with routeRequest = %v, want a loop_cycle refusal naming routeRequest", err)
	}
}

func TestAuthoredActivate_AdmitsACandidateBesideAShippedCycle(t *testing.T) {
	s := authoredForgeScheduler(t, nil, true)
	err := s.Activate(authoredLoopConstruct("routeOnce", `@trigger(event="node.created", concept="v1:forge:request")
@filter(row => args.firstVersion == true)
automation routeOnce {
  args {
    firstVersion bool
    id           any
  }
  advance := mutation advanceRequest(requestId: args.id, status: "queued")
}
`))
	if err != nil {
		t.Fatalf("a first-version automation closes no cycle, and routeRequest's own is not its to answer for: %v", err)
	}
	if !s.IsActive("v1:identity:user:alice", "routeOnce") {
		t.Error("routeOnce was not wired")
	}
}

// A scheduler whose loader holds no function registry cannot judge a
// candidate: it activates, and logs that the check did not run.
func TestAuthoredActivate_NoFunctionsLogsNotRun(t *testing.T) {
	logger, logs := captureLogger()
	s := authoredForgeScheduler(t, logger, false)
	if err := s.Activate(authoredLoopConstruct("reAdvance", selfUpdating)); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if !strings.Contains(logs.String(), "static loop analysis not run for an authored automation") {
		t.Errorf("the scheduler did not say the check did not run:\n%s", logs.String())
	}
}
