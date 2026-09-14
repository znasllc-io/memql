package conformance

// scenarios_db_test.go -- the scenario suites (D23 of
// docs/superpowers/specs/2026-09-13-dsl-v1-language-freeze-program-design.md;
// epic memql#5370, task memql#5374).
//
// A scenario runs automations the product SHIPS, named from the embedded tree
// and never copied: a copy can pass while the shipped automation breaks. It
// writes its seed rows through the tree's own mutations, fires each automation
// the way its trigger would -- a schedule tick, or the very event a write
// published on the engine's bus -- and reads the rows back from the store.
//
// Two variants replay a scenario on fresh rows. dryRun runs one fire through
// the sandbox: every row must read as it did before, and the sandbox's
// manifest must hold the writes a live run makes, since a dry run that reached
// no write would pass the first check for the wrong reason. resume fails one
// step of one fire once and resumes the run from the journal it wrote: it must
// end with the rows and the capability calls an uninterrupted run ends with.
//
// The same scenarios run before and after epic 3's flip -- over the legacy
// bodies first, then over the statement bodies the tree migrates to -- so they
// hold the migration to what the product does to real rows.
//
// A scenario naming an automation or a mutation that no longer exists FAILS,
// with or without a database: a rename must red the scenario, never empty it.
// A scenario directory holds scenario.json and nothing the verdict runner
// reads (no expect.json, no .memql file).
//
// WHERE IT RUNS: beside the differential lane, in the mcp-conformance job
// (tryDB, harness_test.go). Without Postgres TestScenarios skips, and
// MEMQL_REQUIRE_DB=1 turns the skip into a failure. The database is shared, so
// every id a scenario seeds carries a tag unique to the run and the variant,
// and the seeded rows go when the test does.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	automationSteps "github.com/znasllc-io/memql/component/automations/steps"
	memoryNodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/memql"
)

// scenarioRoot is where the suites live, relative to this package.
var scenarioRoot = filepath.Join("2026", "scenarios")

// scenarioOwner is the actor a scenario's seeds write as.
const scenarioOwner = "user-scenario-owner"

// scenarioSuite is one scenarios/<suite>/scenario.json.
type scenarioSuite struct {
	Description string         `json:"description"`
	Scenarios   []scenarioCase `json:"scenarios"`
}

// scenarioCase is one scenario: rows, fires, and what the rows read after.
type scenarioCase struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Seed        []scenarioSeed `json:"seed"`
	Fire        []scenarioFire `json:"fire"`
	Expect      scenarioExpect `json:"expect"`
	// Actions answers the calls the scenario's actions dispatch, keyed as
	// scenarioDispatcher labels them: each capability script's result, which
	// the stub wraps in the envelope a script prints. An unlisted call
	// answers {}.
	Actions map[string]map[string]any `json:"actions"`
	DryRun  *scenarioDryRun           `json:"dryRun"`
	Resume  *scenarioResume           `json:"resume"`
}

// scenarioSeed is one write through a shipped mutation, as the scenario's
// owner unless As names another actor.
type scenarioSeed struct {
	Mutation string         `json:"mutation"`
	Args     map[string]any `json:"args"`
	As       *scenarioActor `json:"as"`
}

// scenarioActor is who a write runs as: a user id and a role.
type scenarioActor struct {
	UserID string `json:"userId"`
	Role   string `json:"role"`
}

// scenarioFire is one step after the seeds: a shipped automation run on a
// schedule tick or on an event the step before it published, or a write
// through a shipped mutation (Mutation, Args, As), which publishes events
// for the next step.
type scenarioFire struct {
	Automation string         `json:"automation"`
	Schedule   bool           `json:"schedule"`
	Event      *scenarioEvent `json:"event"`
	// Status is the run's expected status; "completed" when empty.
	Status string `json:"status"`

	Mutation string         `json:"mutation"`
	Args     map[string]any `json:"args"`
	As       *scenarioActor `json:"as"`
}

// scenarioEvent is the event a fire delivers: the graph event
// graph.node.<action>.<concept> the step before published for the row id (for
// the first fire, the seeds), or -- for a topic no write publishes -- an event
// the scenario publishes itself, Topic with Payload.
type scenarioEvent struct {
	Action  string `json:"action"`
	Concept string `json:"concept"`
	ID      any    `json:"id"`

	Topic   string         `json:"topic"`
	Payload map[string]any `json:"payload"`
}

// scenarioExpect is what the store and the capability stub hold after the
// fires.
type scenarioExpect struct {
	Rows []scenarioRow `json:"rows"`
	// Actions is every call the actions dispatched, in order, labelled as
	// scenarioDispatcher labels them.
	Actions []string `json:"actions"`
}

// scenarioRow is a row's latest version, named by its id or found by Where
// (the rows of the concept whose latest version holds those fields: Count of
// them, one by default). Its payload holds Payload's fields.
type scenarioRow struct {
	Concept string         `json:"concept"`
	ID      any            `json:"id"`
	Where   map[string]any `json:"where"`
	Count   *int           `json:"count"`
	Payload map[string]any `json:"payload"`
	// History is every version of a row named by id, oldest first, each
	// holding those fields.
	History []map[string]any `json:"history"`
	// Legacy is what the row reads while the tree's bodies are in the retired
	// forms, where a legacy defect makes it differ from what the body says.
	// Epic 3's flip moves the tree to statements, and then a Legacy entry is
	// refused: it is deleted with the defect it records.
	Legacy *scenarioLegacy `json:"legacy"`
}

// scenarioLegacy is a row's count under the legacy bodies, and the defect
// that makes it differ (the logic goldens' legacyDefect, in words).
type scenarioLegacy struct {
	Count  *int   `json:"count"`
	Defect string `json:"defect"`
}

// scenarioDryRun runs fire Fire through the sandbox. Writes is how many writes
// its manifest must hold.
type scenarioDryRun struct {
	Fire   int `json:"fire"`
	Writes int `json:"writes"`
}

// scenarioResume fails the FailAt-th step fire Fire's run asks for (counting
// from 1), once, then resumes the run from its journal.
type scenarioResume struct {
	Fire   int `json:"fire"`
	FailAt int `json:"failAt"`
}

// loadScenarioSuites reads every suite, refusing a directory that holds
// anything but scenario.json.
func loadScenarioSuites(t *testing.T) map[string]scenarioSuite {
	t.Helper()
	dirs, err := os.ReadDir(scenarioRoot)
	if err != nil {
		t.Fatalf("read %s: %v", scenarioRoot, err)
	}
	out := map[string]scenarioSuite{}
	for _, d := range dirs {
		if !d.IsDir() {
			continue // README.md
		}
		dir := filepath.Join(scenarioRoot, d.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.Name() != "scenario.json" {
				t.Errorf("%s holds %s: a scenario directory holds scenario.json and nothing the verdict runner reads", dir, e.Name())
			}
		}
		raw, err := os.ReadFile(filepath.Join(dir, "scenario.json"))
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		var s scenarioSuite
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&s); err != nil {
			t.Fatalf("%s/scenario.json: %v", dir, err)
		}
		if len(s.Scenarios) == 0 {
			t.Fatalf("%s/scenario.json holds no scenario", dir)
		}
		out[d.Name()] = s
	}
	if len(out) == 0 {
		t.Fatalf("no scenario suite under %s: the directory moved, and this test checked nothing", scenarioRoot)
	}
	return out
}

// scenarioFixtures is the engine and the shipped automations both scenario
// tests read, built once for this file: each costs a boot of the whole tree.
func scenarioFixtures(t *testing.T) (*Env, map[string]*automations.Automation) {
	t.Helper()
	scenarioFixturesOnce.Do(func() {
		scenarioEnv = newEnv(t)
		scenarioAutos = shippedAutomations(t)
	})
	if scenarioEnv == nil || scenarioAutos == nil {
		t.Fatal("the scenario fixtures failed to build (see the first test that asked for them)")
	}
	return scenarioEnv, scenarioAutos
}

var (
	scenarioFixturesOnce sync.Once
	scenarioEnv          *Env
	scenarioAutos        map[string]*automations.Automation
)

// shippedAutomations is every automation the embedded tree loads, by name.
func shippedAutomations(t *testing.T) map[string]*automations.Automation {
	t.Helper()
	loaded, err := automations.NewLoader(automations.LoaderOptions{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}).LoadFromUnifiedTree()
	if err != nil {
		t.Fatalf("load the shipped automations: %v", err)
	}
	out := make(map[string]*automations.Automation, len(loaded))
	for _, a := range loaded {
		out[a.Name] = a
	}
	return out
}

// TestScenarioFilesNameShippedConstructs: every automation a scenario fires
// and every mutation it seeds through is one the tree ships, and each
// scenario is well formed. It needs no database, so a rename reds on every
// lane.
func TestScenarioFilesNameShippedConstructs(t *testing.T) {
	suites := loadScenarioSuites(t)
	env, autos := scenarioFixtures(t)
	for suite, s := range suites {
		for _, sc := range s.Scenarios {
			where := suite + "/" + sc.Name
			checkScenarioShape(t, where, sc, autos, env.Eng)
		}
	}
}

// checkScenarioShape reports what makes a scenario unrunnable.
func checkScenarioShape(t *testing.T, where string, sc scenarioCase, autos map[string]*automations.Automation, eng *memql.MemQLEngine) {
	t.Helper()
	if sc.Name == "" || len(sc.Fire) == 0 || len(sc.Expect.Rows) == 0 {
		t.Errorf("%s: a scenario has a name, at least one fire and at least one expected row", where)
	}
	for i, r := range sc.Expect.Rows {
		if r.Concept == "" || (r.ID == nil) == (r.Where == nil) || (r.Count != nil && r.Where == nil) {
			t.Errorf("%s: expected row %d names a concept and either an id or a where (count goes with where)", where, i)
		}
		if r.History != nil && r.ID == nil {
			t.Errorf("%s: expected row %d's history belongs to a row named by id", where, i)
		}
		if l := r.Legacy; l != nil && (l.Defect == "" || l.Count == nil || r.Where == nil) {
			t.Errorf("%s: expected row %d's legacy entry is a where row's count under the legacy bodies, with the defect that explains it", where, i)
		}
	}
	// A scenario's bodies are all in the retired forms or all statements: the
	// tree migrates in one commit. Once they are statements, a legacy entry
	// records a defect that no longer exists.
	statements, legacy := 0, 0
	for _, f := range sc.Fire {
		if a := autos[f.Automation]; a != nil {
			if a.IsStatementBody() {
				statements++
			} else {
				legacy++
			}
		}
	}
	if statements > 0 && legacy > 0 {
		t.Errorf("%s: fires %d automations written in statements and %d in the retired forms; the tree migrates in one commit", where, statements, legacy)
	}
	if statements > 0 {
		for i, r := range sc.Expect.Rows {
			if r.Legacy != nil {
				t.Errorf("%s: expected row %d still carries a legacy entry, and its automations are statements now: delete it (%s)", where, i, r.Legacy.Defect)
			}
		}
	}
	isMutation := func(name string) bool {
		fn, err := eng.Functions().Get(name)
		return err == nil && fn != nil && fn.FunctionKind == "mutation"
	}
	for i, seed := range sc.Seed {
		if !isMutation(seed.Mutation) {
			t.Errorf("%s: seed %d names mutation %q, which the tree does not ship", where, i, seed.Mutation)
		}
	}
	for i, f := range sc.Fire {
		if f.Mutation != "" {
			if f.Automation != "" || f.Schedule || f.Event != nil {
				t.Errorf("%s: fire %d is a write or an automation, not both", where, i)
			}
			if !isMutation(f.Mutation) {
				t.Errorf("%s: fire %d writes through mutation %q, which the tree does not ship", where, i, f.Mutation)
			}
			continue
		}
		a := autos[f.Automation]
		if a == nil {
			t.Errorf("%s: fire %d names automation %q, which the tree does not ship", where, i, f.Automation)
			continue
		}
		switch {
		case f.Schedule == (f.Event != nil):
			t.Errorf("%s: fire %d is on a schedule or on an event, and exactly one", where, i)
		case f.Schedule && a.Schedule == "":
			t.Errorf("%s: fire %d ticks %s's schedule, and it has none", where, i, a.Name)
		case f.Event != nil && (f.Event.Topic == "") == (f.Event.Action == ""):
			t.Errorf("%s: fire %d's event is a graph event (action, concept, id) or a published one (topic, payload), and exactly one", where, i)
		case f.Event != nil && (a.Trigger == nil || a.Trigger.Event == ""):
			t.Errorf("%s: fire %d delivers an event to %s, which no event triggers", where, i, a.Name)
		case f.Event != nil && !events.Match(a.Trigger.Event, eventTopic(f.Event)):
			t.Errorf("%s: fire %d delivers %s, which %s's trigger (%s) does not take", where, i, eventTopic(f.Event), a.Name, a.Trigger.Event)
		}
	}
	if d := sc.DryRun; d != nil && (d.Fire < 0 || d.Fire >= len(sc.Fire) || sc.Fire[d.Fire].Automation == "" || d.Writes < 1) {
		t.Errorf("%s: dryRun names an automation fire of the scenario and the writes its manifest must hold (at least one)", where)
	}
	if r := sc.Resume; r != nil && (r.Fire < 0 || r.Fire >= len(sc.Fire) || sc.Fire[r.Fire].Automation == "" || r.FailAt < 1) {
		t.Errorf("%s: resume names an automation fire of the scenario and a step to fail, counting from 1", where)
	}
}

// eventTopic is the event's topic: the one it names, or the one a write of
// the named kind publishes.
func eventTopic(e *scenarioEvent) string {
	if e.Topic != "" {
		return e.Topic
	}
	return events.BuildTopicWithConcept("graph.node."+e.Action, e.Concept)
}

// TestScenarios runs every scenario over a real database, then its dryRun and
// resume variants.
func TestScenarios(t *testing.T) {
	suites := loadScenarioSuites(t)
	env, autos := scenarioFixtures(t)
	if !env.HasDB {
		if os.Getenv("MEMQL_REQUIRE_DB") == "1" {
			t.Fatal("the scenario suites need Postgres, and MEMQL_REQUIRE_DB=1 makes its absence a failure")
		}
		t.Skip("the scenario suites need Postgres (MEMQL_DATABASE_DSN); they run in the CI mcp-conformance job")
	}
	rig := newScenarioRig(t, env, autos)
	names := make([]string, 0, len(suites))
	for name := range suites {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, suite := range names {
		for _, sc := range suites[suite].Scenarios {
			sc := sc
			t.Run(suite+"/"+sc.Name, func(t *testing.T) {
				checkScenarioShape(t, suite+"/"+sc.Name, sc, rig.autos, env.Eng)
				if t.Failed() {
					return
				}
				rig.live(t, sc)
				if sc.DryRun != nil {
					rig.dryRun(t, sc)
				}
				if sc.Resume != nil {
					rig.resume(t, sc)
				}
			})
		}
	}
}

// scenarioRig is the engine a scenario runs against, the automations it can
// fire, and the graph events every write publishes.
type scenarioRig struct {
	env   *Env
	autos map[string]*automations.Automation
	quiet *slog.Logger
	run   string

	mu       sync.Mutex
	captured []events.Event
}

func newScenarioRig(t *testing.T, env *Env, autos map[string]*automations.Automation) *scenarioRig {
	t.Helper()
	r := &scenarioRig{
		env:   env,
		autos: autos,
		quiet: slog.New(slog.NewTextHandler(io.Discard, nil)),
		run:   strconv.FormatInt(time.Now().UnixNano()%1_000_000_000, 36),
	}
	bus := env.Eng.EventBus()
	if bus == nil {
		t.Fatal("the rig's engine has no event bus, so no write would publish the events a scenario fires on")
	}
	unsub := bus.Subscribe("graph.node.#", func(ev events.Event) {
		r.mu.Lock()
		r.captured = append(r.captured, ev)
		r.mu.Unlock()
	})
	t.Cleanup(unsub)
	return r
}

// variant is one replay of a scenario: its own tag, and so its own rows.
type variant struct {
	rig    *scenarioRig
	t      *testing.T
	sc     scenarioCase
	tag    string
	seeded [][2]string // (concept, id) of every row the seeds wrote
	found  [][2]string // (concept, id) of every row a where found
	calls  *scenarioDispatcher
	// window is how many events the bus had delivered when the step before
	// the current one began: an event fire takes an event published since.
	window int
	// legacy is set while the scenario's automations are in the retired forms.
	legacy bool
}

func (r *scenarioRig) variant(t *testing.T, sc scenarioCase, kind string) *variant {
	v := &variant{
		rig: r, t: t, sc: sc,
		tag:   r.run + "-" + kind,
		calls: &scenarioDispatcher{answers: sc.Actions},
	}
	for _, f := range sc.Fire {
		if a := r.autos[f.Automation]; a != nil && !a.IsStatementBody() {
			v.legacy = true
		}
	}
	t.Cleanup(v.cleanup)
	return v
}

// live seeds, fires every automation and checks the rows and the calls.
func (r *scenarioRig) live(t *testing.T, sc scenarioCase) {
	v := r.variant(t, sc, "live")
	v.seed()
	for i := range sc.Fire {
		v.step(i, v.registry(nil))
	}
	v.expectRows()
	v.expectActions()
}

// dryRun seeds, runs the fires before the dry one as a live run would, and
// runs that one through the sandbox: nothing is written, and the manifest
// holds the writes a live run makes.
func (r *scenarioRig) dryRun(t *testing.T, sc scenarioCase) {
	v := r.variant(t, sc, "dry")
	v.seed()
	for i := 0; i < sc.DryRun.Fire; i++ {
		v.step(i, v.registry(nil))
	}
	f := sc.Fire[sc.DryRun.Fire]
	before := v.snapshot()
	if len(sc.Seed) > 0 && len(before) == 0 {
		t.Fatalf("dryRun: the seeds wrote no row a snapshot reads (a seed id is written {\"$id\": ...}), so \"nothing changed\" would hold vacuously")
	}
	src, ok := memql.DSLConstructSource(r.quiet, "automation", f.Automation)
	if !ok {
		t.Fatalf("dryRun: the tree holds no source for automation %s", f.Automation)
	}
	req := memql.DryRunRequest{AutomationName: f.Automation, AutomationSource: src, Mode: memql.DryRunModeIsolated}
	if f.Event != nil {
		ev := v.event(f.Event)
		req.TriggerEvent = &memql.DryRunTriggerEvent{Topic: ev.Topic, Kind: ev.Kind.String(), Payload: ev.Payload}
	}
	start := time.Now()
	report, err := memql.RunBundleDryRun(context.Background(), r.env.Eng, req)
	if err != nil {
		t.Fatalf("dryRun %s: %v", f.Automation, err)
	}
	if !report.OK {
		t.Fatalf("dryRun %s did not complete: %s", f.Automation, report.FailureReason)
	}
	if got := len(report.SideEffectManifest.Mutations); got != sc.DryRun.Writes {
		t.Errorf("dryRun %s intercepted %d writes, and the scenario says a live run makes %d: %+v",
			f.Automation, got, sc.DryRun.Writes, report.SideEffectManifest.Mutations)
	}
	if after := v.snapshot(); !reflect.DeepEqual(before, after) {
		t.Errorf("dryRun %s changed a seeded row:\nbefore %v\nafter  %v", f.Automation, before, after)
	}
	written, err := r.env.DB.NewSelect().Model((*memoryNodes.MemoryNode)(nil)).
		Where(`"createdAt" >= ?`, start).Count(context.Background())
	if err != nil {
		t.Fatalf("count the rows written since the dry run began: %v", err)
	}
	if written != 0 {
		t.Errorf("dryRun %s: %d rows were written while it ran, and a dry run writes none", f.Automation, written)
	}
}

// resume seeds, fails one step of one fire once, resumes that run from its
// journal, and checks the rows and the calls an uninterrupted run ends with.
func (r *scenarioRig) resume(t *testing.T, sc scenarioCase) {
	v := r.variant(t, sc, "resume")
	v.seed()
	for i := range sc.Fire {
		if i != sc.Resume.Fire {
			v.step(i, v.registry(nil))
			continue
		}
		broken := &failOnce{at: sc.Resume.FailAt}
		ex := v.executor(v.registry(broken))
		a := r.autos[sc.Fire[i].Automation]
		first := v.execute(ex, i)
		if first.Status != "failed" || !broken.fired {
			t.Fatalf("resume: the run of %s did not fail at step %d (status %s, %d steps asked for)", a.Name, sc.Resume.FailAt, first.Status, broken.n)
		}
		journal, err := automations.LoadRunJournal(context.Background(), r.env.Eng, first.ID)
		if err != nil {
			t.Fatalf("resume: read the journal of %s's run: %v", a.Name, err)
		}
		// The injected failure comes before the step runs, so the step took
		// no effect and re-running it duplicates none: the case
		// AllowSideEffects is for. The capability order checked below is what
		// would show a duplicate.
		resumed, err := ex.ResumeFrom(context.Background(), journal, a, &automations.ResumeOptions{AllowSideEffects: true})
		if err != nil {
			t.Fatalf("resume %s: %v", a.Name, err)
		}
		if want := statusOf(sc.Fire[i]); resumed.Status != want {
			t.Fatalf("resume: %s's resumed run is %s, want %s: %s", a.Name, resumed.Status, want, resumed.Error)
		}
	}
	v.expectRows()
	v.expectActions()
}

// seed writes every seed row through its mutation.
func (v *variant) seed() {
	v.t.Helper()
	v.mark()
	for i, s := range v.sc.Seed {
		v.write(fmt.Sprintf("seed %d", i), s.Mutation, s.Args, s.As)
	}
}

// write calls a shipped mutation as the actor (the scenario's owner when
// nil), at internal origin, and notes the tagged ids it names as seeded rows.
func (v *variant) write(label, mutation string, rawArgs map[string]any, as *scenarioActor) {
	v.t.Helper()
	user, role := scenarioOwner, auth.RoleOwner
	if as != nil {
		if as.UserID != "" {
			user = as.UserID
		}
		if as.Role != "" {
			role = auth.Role(as.Role)
		}
	}
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: user, Role: role})
	ctx = auth.ContextWithToken(ctx, &auth.TokenInfo{Subject: user})
	ctx = auth.ContextWithInternalOrigin(ctx)
	args, _ := v.resolve(rawArgs).(map[string]any)
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+": "+renderScenarioArg(v.t, args[k]))
	}
	call := "mutation " + mutation + "(" + strings.Join(parts, ", ") + ")"
	if _, err := v.rig.env.Eng.Execute(ctx, call); err != nil {
		v.t.Fatalf("%s, %s: %v", label, call, err)
	}
	fn, _ := v.rig.env.Eng.Functions().Get(mutation)
	if fn != nil && fn.BoundConcept != "" {
		for _, k := range keys {
			if id, ok := args[k].(string); ok && strings.HasSuffix(id, "-"+v.tag) {
				v.seeded = append(v.seeded, [2]string{fn.BoundConcept, id})
			}
		}
	}
}

// step runs fire i: a write, or an automation to its expected status.
func (v *variant) step(i int, reg automations.StepExecutorRegistry) {
	v.t.Helper()
	f := v.sc.Fire[i]
	if f.Mutation != "" {
		v.mark()
		v.write(fmt.Sprintf("fire %d", i), f.Mutation, f.Args, f.As)
		return
	}
	exec := v.execute(v.executor(reg), i)
	if want := statusOf(f); exec.Status != want {
		v.t.Fatalf("fire %d: %s's run is %s, want %s: %s", i, f.Automation, exec.Status, want, exec.Error)
	}
}

// execute runs automation fire i once and returns its record, whatever its
// status.
func (v *variant) execute(ex *automations.Executor, i int) *automations.AutomationExecution {
	v.t.Helper()
	f := v.sc.Fire[i]
	a := v.rig.autos[f.Automation]
	var ev *events.Event
	if f.Event != nil {
		e := v.event(f.Event)
		ev = &e
	}
	// This run is the step the next event fire reads its event from.
	v.mark()
	var exec *automations.AutomationExecution
	if ev == nil {
		exec, _ = ex.Execute(context.Background(), a, "scenario:schedule")
	} else {
		exec, _ = ex.ExecuteWithEvent(context.Background(), a, "event:"+ev.Topic, ev)
	}
	if exec == nil {
		v.t.Fatalf("fire %d: %s left no execution record", i, a.Name)
	}
	return exec
}

func (v *variant) executor(reg automations.StepExecutorRegistry) *automations.Executor {
	ex := automations.NewExecutor(automations.ExecutorOptions{
		Logger:       v.rig.quiet,
		Engine:       v.rig.env.Eng,
		EventBus:     v.rig.env.Eng.EventBus(),
		StepRegistry: reg,
	})
	v.t.Cleanup(ex.Close)
	return ex
}

// registry is the production step registry, with actions dispatched to the
// variant's stub. A non-nil wrap fails a step once.
func (v *variant) registry(wrap *failOnce) automations.StepExecutorRegistry {
	reg := automationSteps.NewRegistry()
	reg.Register(automations.StepTypeAction, &automationSteps.ActionExecutor{Dispatcher: v.calls})
	if wrap == nil {
		return reg
	}
	wrap.inner = reg
	return wrap
}

func statusOf(f scenarioFire) string {
	if f.Status == "" {
		return "completed"
	}
	return f.Status
}

// mark notes where a step begins in the bus's delivered events.
func (v *variant) mark() {
	v.rig.mu.Lock()
	v.window = len(v.rig.captured)
	v.rig.mu.Unlock()
}

// event is the event a fire delivers: the scenario's own, or the graph event
// the step before published.
func (v *variant) event(e *scenarioEvent) events.Event {
	v.t.Helper()
	if e.Topic != "" {
		payload, _ := v.resolve(e.Payload).(map[string]any)
		return events.NewEvent(e.Topic, events.KindMessage, payload)
	}
	return v.awaitEvent(e)
}

// awaitEvent is the latest event for the named row the bus delivered since
// the step before began, waiting for the bus to deliver it.
func (v *variant) awaitEvent(e *scenarioEvent) events.Event {
	v.t.Helper()
	topic := eventTopic(e)
	id, _ := v.resolve(e.ID).(string)
	deadline := time.Now().Add(5 * time.Second)
	for {
		v.rig.mu.Lock()
		for i := len(v.rig.captured) - 1; i >= v.window; i-- {
			ev := v.rig.captured[i]
			if ev.Topic == topic && sameID(fmt.Sprint(ev.Payload["id"]), id) {
				v.rig.mu.Unlock()
				return ev
			}
		}
		v.rig.mu.Unlock()
		if time.Now().After(deadline) {
			v.t.Fatalf("the step before published no %s event for %s", topic, id)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// sameID reports whether a stored id -- bare or {concept}:{id} -- names id.
func sameID(stored, id string) bool {
	return stored == id || strings.HasSuffix(stored, ":"+id)
}

// expectRows checks every expected row's latest version.
func (v *variant) expectRows() {
	v.t.Helper()
	for _, want := range v.sc.Expect.Rows {
		if want.Where != nil {
			v.expectWhere(want)
			continue
		}
		id, _ := v.resolve(want.ID).(string)
		v.found = append(v.found, [2]string{want.Concept, id})
		got, ok := v.latest(want.Concept, id)
		if !ok {
			v.t.Errorf("%s %s: no row", want.Concept, id)
			continue
		}
		v.expectPayload(want, id, got)
		if want.History != nil {
			v.expectHistory(want, id)
		}
	}
}

// expectWhere finds the rows of the concept whose latest version holds the
// where fields: exactly Count of them (one by default), each holding Payload.
func (v *variant) expectWhere(want scenarioRow) {
	v.t.Helper()
	where := v.resolve(want.Where).(map[string]any)
	raw, err := json.Marshal(where)
	if err != nil {
		v.t.Fatal(err)
	}
	var nodes []memoryNodes.MemoryNode
	if err := v.rig.env.DB.NewSelect().Model(&nodes).
		Where("concept = ?", want.Concept).
		Where("payload @> ?::jsonb", string(raw)).
		Scan(context.Background()); err != nil {
		v.t.Fatalf("find %s where %s: %v", want.Concept, raw, err)
	}
	seen := map[string]bool{}
	var matched []string
	for _, n := range nodes {
		if seen[n.ID] {
			continue
		}
		seen[n.ID] = true
		latest, ok := v.latest(want.Concept, n.ID)
		if ok && holds(latest, where) {
			matched = append(matched, n.ID)
			v.found = append(v.found, [2]string{want.Concept, n.ID})
			v.expectPayload(want, n.ID, latest)
		}
	}
	count := 1
	if want.Count != nil {
		count = *want.Count
	}
	if l := want.Legacy; l != nil && v.legacy {
		count = *l.Count
		v.t.Logf("%s where %s: %d rows under the legacy bodies, not %d: %s", want.Concept, raw, count, *want.Count, l.Defect)
	}
	if len(matched) != count {
		v.t.Errorf("%s where %s: %d rows %v, want %d", want.Concept, raw, len(matched), matched, count)
	}
}

// expectPayload checks that a row holds the expected payload fields.
func (v *variant) expectPayload(want scenarioRow, id string, got map[string]any) {
	v.t.Helper()
	fields, _ := v.resolve(want.Payload).(map[string]any)
	for field, w := range fields {
		if g, present := got[field]; !present || !reflect.DeepEqual(jsonNormal(g), jsonNormal(w)) {
			v.t.Errorf("%s %s: %s is %v, want %v", want.Concept, id, field, got[field], w)
		}
	}
}

// expectHistory checks every version of a row, oldest first.
func (v *variant) expectHistory(want scenarioRow, id string) {
	v.t.Helper()
	var nodes []memoryNodes.MemoryNode
	if err := v.rig.env.DB.NewSelect().Model(&nodes).
		Where("concept = ?", want.Concept).
		Where("id IN (?)", bun.In([]string{id, want.Concept + ":" + id})).
		OrderExpr(`"createdAt" ASC`).
		Scan(context.Background()); err != nil {
		v.t.Fatalf("read the versions of %s %s: %v", want.Concept, id, err)
	}
	if len(nodes) != len(want.History) {
		v.t.Errorf("%s %s has %d versions, want %d", want.Concept, id, len(nodes), len(want.History))
		return
	}
	for i, n := range nodes {
		var payload map[string]any
		if err := json.Unmarshal(n.Payload, &payload); err != nil {
			v.t.Fatalf("%s %s version %d: %v", want.Concept, id, i+1, err)
		}
		if fields, _ := v.resolve(want.History[i]).(map[string]any); !holds(payload, fields) {
			v.t.Errorf("%s %s version %d is %v, want it to hold %v", want.Concept, id, i+1, payload, fields)
		}
	}
}

// holds reports whether a payload holds every field of where.
func holds(payload, where map[string]any) bool {
	for k, w := range where {
		if g, ok := payload[k]; !ok || !reflect.DeepEqual(jsonNormal(g), jsonNormal(w)) {
			return false
		}
	}
	return true
}

// expectActions checks the capabilities dispatched, in order.
func (v *variant) expectActions() {
	v.t.Helper()
	got := v.calls.dispatched()
	if want := v.sc.Expect.Actions; !reflect.DeepEqual(got, want) && (len(got) > 0 || len(want) > 0) {
		v.t.Errorf("capabilities dispatched %q, want %q", got, want)
	}
}

// latest reads a row's latest version from the store.
func (v *variant) latest(concept, id string) (map[string]any, bool) {
	var nodes []memoryNodes.MemoryNode
	err := v.rig.env.DB.NewSelect().Model(&nodes).
		Where("concept = ?", concept).
		Where("id IN (?)", bun.In([]string{id, concept + ":" + id})).
		OrderExpr(`"createdAt" DESC`).Limit(1).
		Scan(context.Background())
	if err != nil {
		v.t.Fatalf("read %s %s: %v", concept, id, err)
	}
	if len(nodes) == 0 {
		return nil, false
	}
	var payload map[string]any
	if err := json.Unmarshal(nodes[0].Payload, &payload); err != nil {
		v.t.Fatalf("%s %s: payload: %v", concept, id, err)
	}
	return payload, true
}

// snapshot is every seeded row's latest version, as stored.
func (v *variant) snapshot() map[string]string {
	out := map[string]string{}
	for _, s := range v.seeded {
		var nodes []memoryNodes.MemoryNode
		if err := v.rig.env.DB.NewSelect().Model(&nodes).
			Where("concept = ?", s[0]).
			Where("id IN (?)", bun.In([]string{s[1], s[0] + ":" + s[1]})).
			OrderExpr(`"createdAt" DESC`).Limit(1).
			Scan(context.Background()); err != nil {
			v.t.Fatalf("read %s %s: %v", s[0], s[1], err)
		}
		if len(nodes) == 1 {
			out[s[0]+" "+s[1]] = nodes[0].CreatedAt.String() + " " + string(nodes[0].Payload)
		}
	}
	return out
}

// cleanup deletes every version of the rows the variant seeded or found.
func (v *variant) cleanup() {
	if v.rig.env.DB == nil {
		return
	}
	for _, s := range append(v.seeded, v.found...) {
		_, _ = v.rig.env.DB.NewDelete().Model((*memoryNodes.MemoryNode)(nil)).
			Where("concept = ?", s[0]).
			Where("id IN (?)", bun.In([]string{s[1], s[0] + ":" + s[1]})).
			Exec(context.Background())
	}
}

// resolve replaces a scenario value's placeholders: {"$id": "x"} is id x
// under the variant's tag, and {"$ago": "PT10M"} the instant that long before
// now, in RFC3339.
func (v *variant) resolve(x any) any {
	switch x := x.(type) {
	case map[string]any:
		if len(x) == 1 {
			if id, ok := x["$id"].(string); ok {
				return id + "-" + v.tag
			}
			if d, ok := x["$ago"].(string); ok {
				dur, err := isoDuration(d)
				if err != nil {
					v.t.Fatalf("$ago %q: %v", d, err)
				}
				return time.Now().UTC().Add(-dur).Format(time.RFC3339)
			}
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = v.resolve(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = v.resolve(e)
		}
		return out
	}
	return x
}

var isoDurationRe = regexp.MustCompile(`^P(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?)?$`)

// isoDuration reads the ISO-8601 durations a scenario writes: days, hours,
// minutes and seconds.
func isoDuration(s string) (time.Duration, error) {
	m := isoDurationRe.FindStringSubmatch(s)
	if m == nil || s == "P" || s == "PT" {
		return 0, fmt.Errorf("not an ISO-8601 duration of days, hours, minutes and seconds")
	}
	var d time.Duration
	for i, unit := range []time.Duration{24 * time.Hour, time.Hour, time.Minute, time.Second} {
		if m[i+1] != "" {
			n, _ := strconv.Atoi(m[i+1])
			d += time.Duration(n) * unit
		}
	}
	return d, nil
}

// renderScenarioArg writes a JSON value as a call argument.
func renderScenarioArg(t *testing.T, v any) string {
	t.Helper()
	switch v := v.(type) {
	case string:
		return ast.QuoteString(v)
	case bool:
		return strconv.FormatBool(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case []any:
		items := make([]string, len(v))
		for i, e := range v {
			items[i] = renderScenarioArg(t, e)
		}
		return "[" + strings.Join(items, ", ") + "]"
	}
	t.Fatalf("a seed argument is a string, a number, a boolean or a list of them; got %T", v)
	return ""
}

// jsonNormal is v as a JSON round trip reads it, so a stored number and a
// scenario's compare equal.
func jsonNormal(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if json.Unmarshal(b, &out) != nil {
		return v
	}
	return out
}

// scenarioDispatcher answers every call with a capability script's envelope
// around the scenario's answer, and records the calls. A call is labelled by
// its capability, and a shell.script call by the script it names too
// ("shell.script:deploy.gate"), since every script action dispatches that one
// capability.
type scenarioDispatcher struct {
	answers map[string]map[string]any

	mu    sync.Mutex
	calls []string
}

func (d *scenarioDispatcher) Invoke(_ context.Context, capability string, args map[string]any) (any, error) {
	label := capability
	if script, ok := args["script"].(string); ok && script != "" {
		label += ":" + script
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, label)
	out := d.answers[label]
	if out == nil {
		out = map[string]any{}
	}
	return map[string]any{"ok": true, "changed": true, "result": out, "summary": "scenario"}, nil
}

func (d *scenarioDispatcher) dispatched() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.calls...)
}

// failOnce fails the at-th step it is asked to run, once, before the step
// runs, and delegates every other.
type failOnce struct {
	inner automations.StepExecutorRegistry
	at    int

	n     int
	fired bool
}

func (f *failOnce) Execute(ctx context.Context, step *automations.Step, stepCtx *automations.StepContext) (*automations.StepResult, error) {
	f.n++
	if !f.fired && f.n == f.at {
		f.fired = true
		now := time.Now()
		msg := fmt.Sprintf("scenario: step %d (%s) fails once", f.n, step.ID)
		return &automations.StepResult{StepId: step.ID, StartedAt: now, CompletedAt: now, Status: "failed", Error: msg}, fmt.Errorf("%s", msg)
	}
	return f.inner.Execute(ctx, step, stepCtx)
}
