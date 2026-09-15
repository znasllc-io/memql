package steps

// automation_v1_corpus_test.go -- the automation corpus (epic memql#5370, task
// memql#5372).
//
// Every automation of the tree is run in each of its builds against the same
// fixtures -- a triggering event built from its trigger and its args block, the
// invocation event a run by name gets, or none for a scheduled run, and two
// row counts -- with every construct call,
// action and sub-automation answered by one probe that records it (the logic
// corpus's logicProbe). Each ARM loads the whole tree its own way through the
// boot walk (automations.Loader.LoadFromTree) and runs each automation on an
// Executor whose step registry answers from the probe. Every arm must make the
// same calls, in the same order, publish the same events and end the same way
// as every other arm and as the GOLDENS: one JSON file per automation under
// testdata/automation_corpus/<domain>/<name>.json.
//
// # The arm, before and after the flips
//
// Before the flips two arms ran: the tree as it was (legacy), and the tree as
// `memqlmigrate --rewrite=expressions` and then `--rewrite=bodies` carried it
// (bodies). The goldens were written from the bodies arm, with -update, once
// the two agreed, run for run. Since the bodies flip (epic memql#5370) the
// tree is its own statement source and one arm runs it, so the goldens are
// the statement form held to what the legacy build did -- evidence only a run
// before the flips could gather, since the legacy executor went with them.
// A runner that replaces this one adds its arm to the list in
// TestAutomationCorpusRuns and is held to the same goldens.
//
// # What a run is compared on
//
// The calls it made and the events it published, in order; whether it was
// refused; its status; and its output. A logic call runs for real, the arm
// running its own build of the logic (the logic corpus holds that build to
// its goldens) with the logic's construct calls answered by the same probe,
// so an automation reads what its logic really returns.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/automations"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	memqldsl "github.com/znasllc-io/memql/dsl"
)

const automationGoldenDir = "testdata/automation_corpus"

// ---------------------------------------------------------------------------
// the registry
// ---------------------------------------------------------------------------

// automationProbe is the automation corpus's step registry: the logic
// corpus's probeRegistry, plus what only an automation's steps reach -- an
// action and a sub-automation call, answered by the probe, which records them
// -- and a logic call, which runs for real. A parallel is built here, its
// branches dispatched back to this registry; a `for` and a block run their
// lists on the executor's sequence runner, whose registry this is.
type automationProbe struct {
	*probeRegistry
}

func (r *automationProbe) Execute(ctx context.Context, step *automations.Step, stepCtx *automations.StepContext) (*automations.StepResult, error) {
	if r.probe == nil {
		return nil, fmt.Errorf("probe: step %q runs outside a run", step.ID)
	}
	switch step.Type {
	case automations.StepTypeParallel:
		return (&ParallelExecutor{Registry: r.real, Dispatch: r.Execute}).Execute(ctx, step, stepCtx)
	case automations.StepTypeAction:
		if step.Action == nil {
			break
		}
		args, err := probeStepArgs(ctx, step, stepCtx, step.Action.Args)
		if err != nil {
			return nil, err
		}
		r.probe.calls = append(r.probe.calls, probeCall{Kind: "action", Name: step.Action.Ref, Args: canonicalArgs(args)})
		return &automations.StepResult{StepId: step.ID, Status: "success", Result: probeActionAnswer(step.Action.Ref)}, nil
	case automations.StepTypeAutomation:
		if step.Automation == nil {
			break
		}
		// The real executor hands a legacy step's arguments over as written and
		// a v1 step's as values (steps/automation.go); so does the probe.
		args, err := probeStepArgs(ctx, step, stepCtx, step.Automation.Args)
		if err != nil {
			return nil, err
		}
		name := strings.TrimSpace(step.Automation.Name)
		r.probe.calls = append(r.probe.calls, probeCall{Kind: "automation", Name: name, Args: canonicalArgs(args)})
		return &automations.StepResult{StepId: step.ID, Status: "success", Result: &automations.AutomationExecution{
			AutomationName: name, Status: "completed", Output: map[string]any{"automation": name},
		}}, nil
	case automations.StepTypeFunction:
		if step.Function != nil && r.kinds[step.Function.Name] == "logic" {
			return r.runLogic(ctx, step, stepCtx)
		}
	}
	return r.probeRegistry.Execute(ctx, step, stepCtx)
}

// probeStepArgs are a step's arguments as its real executor sends them:
// evaluated (ResolveV1Map), every step being prepared since epic 2's flip.
func probeStepArgs(ctx context.Context, step *automations.Step, stepCtx *automations.StepContext, args map[string]any) (map[string]any, error) {
	in := make(map[string]any, len(args))
	for k, v := range args {
		in[k] = v
	}
	if stepCtx.Evaluator == nil || len(in) == 0 {
		return in, nil
	}
	if step.Exprs != nil {
		return stepCtx.Evaluator.ResolveV1Map(ctx, in)
	}
	return in, nil
}

// probeActionAnswer is every action's step result, in the shape the real
// ActionExecutor returns one: an authored action's record of the call around
// a capability script's envelope (steps/action.go executeAuthored), whose
// innermost `result` the statement form binds and the legacy form climbs to.
func probeActionAnswer(ref string) map[string]any {
	return map[string]any{
		"authored": true, "ref": ref, "capability": "probe." + ref, "resultFingerprint": "probe",
		"result": map[string]any{
			"ok": true, "changed": true,
			"result": map[string]any{
				"action": ref, "passed": true, "status": "succeeded", "version": "1.2.3",
				"deploymentId": "dep-2", "previousDeploymentId": "dep-1",
			},
		},
	}
}

// runLogic runs a logic call for real: the call its step would send, through
// the engine, whose LogicRunner answers the logic's own statements from the
// probe.
func (r *automationProbe) runLogic(ctx context.Context, step *automations.Step, stepCtx *automations.StepContext) (*automations.StepResult, error) {
	args, err := stepCtx.Evaluator.ResolveV1Map(ctx, step.Function.Args)
	if err != nil {
		return nil, err
	}
	res, err := r.engine.Execute(ctx, step.Function.Name+"("+renderV1CallArgs(args)+")")
	if err != nil {
		return nil, err
	}
	return &automations.StepResult{StepId: step.ID, Status: "success", Result: res}, nil
}

// ---------------------------------------------------------------------------
// the arms
// ---------------------------------------------------------------------------

// automationArm is one build of the tree's automations and the executor that
// runs them.
type automationArm struct {
	name   string
	byName map[string]*automations.Automation
	reg    *automationProbe
	exec   *automations.Executor
}

// armTree is the embedded tree with each .memql file replaced by the arm's
// source of it.
func armTree(t *testing.T, sources []*corpusSource, pick func(*corpusSource) string) fstest.MapFS {
	t.Helper()
	byPath := make(map[string]string, len(sources))
	for _, f := range sources {
		byPath[f.Path] = pick(f)
	}
	src := memqldsl.Tree()
	out := fstest.MapFS{}
	err := fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if s, ok := byPath[p]; ok {
			out[p] = &fstest.MapFile{Data: []byte(s)}
			return nil
		}
		b, err := fs.ReadFile(src, p)
		if err != nil {
			return err
		}
		out[p] = &fstest.MapFile{Data: b}
		return nil
	})
	require.NoError(t, err)
	return out
}

// newAutomationArm loads the arm's automations through the boot walk from
// the arm's source, and wires an executor over one engine, every logic of the
// tree rebuilt from that source and upserted over the booted one, so a logic
// call runs the arm's build of it.
func newAutomationArm(t *testing.T, name string, pick func(*corpusSource) string, sources []*corpusSource) automationArm {
	t.Helper()
	eng := bootEmbeddedEngine(t)
	// A lazy handle satisfies the engine's setup; port 1 makes any database
	// read that escaped the probe fail loudly (see engineArm).
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(
		pgdriver.WithDSN("postgres://unused:unused@127.0.0.1:1/unused?sslmode=disable"))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	eng.SetDatabaseGetter(func() *bun.DB { return db })
	bus := events.NewBus()
	t.Cleanup(bus.Close)
	eng.SetEventBus(bus)
	kinds := map[string]string{}
	for _, fn := range eng.Functions().Snapshot() {
		if fn != nil {
			kinds[fn.Name] = strings.ToLower(strings.TrimSpace(fn.FunctionKind))
		}
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	loaded, err := automations.NewLoader(automations.LoaderOptions{Logger: logger}).LoadFromTree(armTree(t, sources, pick))
	require.NoErrorf(t, err, "%s arm: the tree's automations do not load", name)
	byName := make(map[string]*automations.Automation, len(loaded))
	for _, a := range loaded {
		byName[a.Name] = a
	}

	for _, f := range sources {
		for _, slice := range memql.ExtractFunctionSlices(pick(f)) {
			if slice.Kind != languageParser.FunctionTypeLogic {
				continue
			}
			fn, err := memql.BuildFunctionConstruct(pick(f), slice.Name, "unified:"+f.Path, memorynodes.DefaultRegistry())
			require.NoErrorf(t, err, "%s arm: %s does not build", name, slice.Name)
			require.NoError(t, eng.Functions().Upsert(fn))
		}
	}

	reg := &automationProbe{probeRegistry: &probeRegistry{real: NewRegistry(), engine: eng, kinds: kinds}}
	eng.SetLogicRunner(automations.NewLogicRunner(eng, reg, logger))
	exec := automations.NewExecutor(automations.ExecutorOptions{
		Logger: logger, Engine: eng, EventBus: bus, StepRegistry: reg, SandboxRun: true,
	})
	return automationArm{name: name, byName: byName, reg: reg, exec: exec}
}

// run is one fixture's run in comparable form.
func (arm automationArm) run(fx automationFixture, now time.Time) logicRecord {
	a := arm.byName[fx.Name]
	if a == nil {
		return logicRecord{Refused: true, Result: "the arm has no automation " + fx.Name}
	}
	probe := &logicProbe{rows: fx.Rows, now: now}
	arm.reg.probe = probe
	defer func() { arm.reg.probe = nil }()
	var event *events.Event
	if fx.Event != nil {
		e := *fx.Event
		e.Payload = cloneMap(fx.Event.Payload)
		event = &e
	}
	exec, err := arm.exec.ExecuteWithEvent(context.Background(), a, "corpus", event)
	rec := logicRecord{Refused: err != nil, Calls: probe.calls}
	if exec != nil {
		rec.Result = canonicalJSON(map[string]any{"status": exec.Status, "output": canonicalResult(exec.Output)})
	}
	return rec
}

func cloneMap(m map[string]any) map[string]any {
	raw, _ := json.Marshal(m)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// automationFixture is one run: an automation, the event it fires on (nil for
// a scheduled or manual run) and how many rows every query answers.
type automationFixture struct {
	Key   string // "<path> <name>"
	Path  string
	Name  string
	Event *events.Event
	Rows  int
	Label string
}

// automationFixtures is every run of the corpus, read off the reference
// arm's build of each automation.
func automationFixtures(t *testing.T, autos map[string]*automations.Automation) []automationFixture {
	t.Helper()
	names := make([]string, 0, len(autos))
	for n := range autos {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []automationFixture
	for _, n := range names {
		a := autos[n]
		p := strings.TrimPrefix(a.Origin, "unified:")
		if i := strings.LastIndexByte(p, ':'); i >= 0 {
			p = p[:i]
		}
		type variant struct {
			label string
			event *events.Event
		}
		var variants []variant
		switch {
		case a.Trigger != nil && a.Trigger.Event != "":
			for _, nodeType := range []string{"bff", "agent"} {
				variants = append(variants, variant{"event " + nodeType, automationEvent(a, nodeType)})
			}
		case a.Schedule == "":
			// Run only by name, from a sub-automation step or run_automation.
			variants = append(variants, variant{"invocation", automationInvocation(a)})
		default:
			variants = append(variants, variant{"no event", nil})
		}
		for _, v := range variants {
			for _, rows := range []int{2, 0} {
				out = append(out, automationFixture{
					Key: p + " " + a.Name, Path: p, Name: a.Name, Event: v.event,
					Rows: rows, Label: v.label,
				})
			}
		}
	}
	return out
}

// automationEvent is the event an automation fires on: its trigger's topic,
// and a payload holding every field the tree's bodies read (probeEvent) with
// each field the automation's args block declares set to a value it accepts.
func automationEvent(a *automations.Automation, nodeType string) *events.Event {
	topic := a.Trigger.Event
	segs := strings.Split(topic, ".")
	for i, s := range segs {
		if s == "*" || s == "#" {
			segs[i] = "probe"
		}
	}
	topic = strings.Join(segs, ".")
	payload := cloneMap(probeEvent(nodeType)["payload"].(map[string]any))
	if a.Args != nil {
		for _, f := range a.Args.Fields {
			if f == nil {
				continue
			}
			if v, ok := payload[f.Name]; ok && (len(f.Enum) == 0 || enumHas(f.Enum, v)) {
				continue
			}
			payload[f.Name] = argsFieldValue(f)
		}
	}
	e := &events.Event{Topic: topic, Payload: payload, Timestamp: time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)}
	switch {
	case strings.Contains(topic, "node.created"):
		e.Kind = events.KindNodeCreated
	case strings.Contains(topic, "node.updated"):
		e.Kind = events.KindNodeUpdated
	case strings.Contains(topic, "node.deleted"):
		e.Kind = events.KindNodeDeleted
	}
	return e
}

// automationInvocation is the event an automation run by name fires on: the
// synthetic one Scheduler.TriggerAutomationWithArgs builds, whose payload is
// the call's args -- here a value for each field the args block declares.
func automationInvocation(a *automations.Automation) *events.Event {
	args := map[string]any{}
	if a.Args != nil {
		for _, f := range a.Args.Fields {
			if f != nil {
				args[f.Name] = argsFieldValue(f)
			}
		}
	}
	return &events.Event{Topic: "automation.invocation." + a.Name, Kind: events.KindUnspecified, Payload: args,
		Timestamp: time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)}
}

func enumHas(enum []any, v any) bool {
	for _, e := range enum {
		if fmt.Sprint(e) == fmt.Sprint(v) {
			return true
		}
	}
	return false
}

// argsFieldValue is a value an automation's args field accepts.
func argsFieldValue(f *automations.ArgsField) any {
	if len(f.Enum) > 0 {
		return f.Enum[0]
	}
	switch typ := strings.ToLower(f.Type); {
	case typ == "int" || typ == "integer" || typ == "number" || typ == "float":
		return float64(3)
	case typ == "bool" || typ == "boolean":
		return true
	case typ == "object":
		return map[string]any{}
	case typ == "array" || strings.HasPrefix(typ, "[]"):
		return []any{"a"}
	}
	return "s-" + f.Name
}

// ---------------------------------------------------------------------------
// goldens
// ---------------------------------------------------------------------------

func automationGoldenFile(fx automationFixture) string {
	domain, _, _ := strings.Cut(fx.Path, "/")
	return filepath.Join(automationGoldenDir, domain, fx.Name+".json")
}

func automationRunKey(fx automationFixture) string {
	return fmt.Sprintf("%s | %d rows", fx.Label, fx.Rows)
}

func automationInput(fx automationFixture) any {
	in := map[string]any{"rows": fx.Rows}
	if fx.Event != nil {
		in["event"] = map[string]any{"topic": fx.Event.Topic, "kind": fx.Event.Kind.String(), "payload": fx.Event.Payload}
	}
	return jsonValue(in)
}

func readAutomationGoldens() (logicGoldenRuns, error) {
	out := logicGoldenRuns{}
	domains, err := os.ReadDir(automationGoldenDir)
	if err != nil {
		return nil, err
	}
	for _, d := range domains {
		if !d.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(automationGoldenDir, d.Name()))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if f.IsDir() || path.Ext(f.Name()) != ".json" {
				continue
			}
			p := filepath.Join(automationGoldenDir, d.Name(), f.Name())
			raw, err := os.ReadFile(p)
			if err != nil {
				return nil, err
			}
			var file struct {
				Runs map[string]map[string]any `json:"runs"`
			}
			if err := json.Unmarshal(raw, &file); err != nil {
				return nil, fmt.Errorf("%s: %w", p, err)
			}
			out[p] = file.Runs
		}
	}
	return out, nil
}

func writeAutomationGoldens(files map[string]*goldenFile) error {
	if err := os.RemoveAll(automationGoldenDir); err != nil {
		return err
	}
	for p, gf := range files {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]any{"automation": gf.construct, "runs": gf.runs}); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// the gate
// ---------------------------------------------------------------------------

// TestAutomationCorpusRuns: every automation of the tree, run by every arm
// against every fixture, makes the calls and publishes the events its golden
// holds, in their order, and ends as the golden says -- and the arms agree.
// With -update it writes the goldens from the bodies arm's runs instead,
// once they agree.
func TestAutomationCorpusRuns(t *testing.T) {
	sources := logicCorpusSources(t)
	// The first arm is the one the others are compared with. Since the
	// bodies flip the tree is its own statement source, and one arm runs it.
	arms := []automationArm{
		newAutomationArm(t, "bodies", func(f *corpusSource) string { return f.Bodies }, sources),
	}
	golden := len(arms) - 1 // the bodies arm: the statement form the goldens hold

	fixtures := automationFixtures(t, arms[0].byName)
	require.Greater(t, len(fixtures), 100, "dozens of automations times their fixtures; a small count means the fixtures went blind")
	for _, arm := range arms[1:] {
		require.Equalf(t, len(arms[0].byName), len(arm.byName), "the %s arm loads %d automations, the %s arm %d", arms[0].name, len(arms[0].byName), arm.name, len(arm.byName))
	}

	update := *updateLogicGoldens
	goldens, readErr := readAutomationGoldens()
	if !update {
		require.NoErrorf(t, readErr, "the automation corpus has no goldens in %s -- write them with `go test -run TestAutomationCorpusRuns -update`", automationGoldenDir)
	}

	now := time.Now().UTC()
	written := map[string]*goldenFile{}
	taken := map[string]bool{}
	var diffs []string
	matched, refused := 0, 0
	for _, fx := range fixtures {
		records := make([]logicRecord, len(arms))
		for i, arm := range arms {
			records[i] = arm.run(fx, now)
		}
		file, key := automationGoldenFile(fx), automationRunKey(fx)
		where := fmt.Sprintf("%s [%s]", fx.Key, key)

		agreed := true
		for i := 1; i < len(arms); i++ {
			if err := compareAutomationRuns(records[0], records[i]); err != nil {
				agreed = false
				diffs = append(diffs, fmt.Sprintf("%s: the %s and %s arms: %v\n    %s: %s\n    %s: %s",
					where, arms[0].name, arms[i].name, err, arms[0].name, records[0], arms[i].name, records[i]))
			}
		}
		if records[golden].Refused {
			refused++
		}

		if update {
			if !agreed {
				continue
			}
			gf := written[file]
			if gf == nil {
				gf = &goldenFile{construct: fx.Key, runs: map[string]any{}}
				written[file] = gf
			}
			if _, dup := gf.runs[key]; dup {
				diffs = append(diffs, where+": two fixtures share one golden run key")
			}
			gf.runs[key] = map[string]any{"input": automationInput(fx), "outcome": records[golden].outcome()}
			continue
		}

		entry, ok := goldens[file][key]
		if !ok {
			diffs = append(diffs, where+": no golden run -- if the fixtures changed on purpose, rewrite the goldens with -update")
			continue
		}
		taken[file+"\x00"+key] = true
		if got, want := jsonText(automationInput(fx)), jsonText(entry["input"]); got != want {
			diffs = append(diffs, fmt.Sprintf("%s: the fixture is not the golden's input\n    golden:  %s\n    fixture: %s", where, want, got))
			continue
		}
		want := recordOf(entry["outcome"])
		for i, arm := range arms {
			if err := compareAutomationRuns(records[i], want); err != nil {
				diffs = append(diffs, fmt.Sprintf("%s: the %s arm and the golden: %v\n    golden: %s\n    %s: %s", where, arm.name, err, want, arm.name, records[i]))
				continue
			}
			matched++
		}
	}
	if !update {
		for file, runs := range goldens {
			for key := range runs {
				if !taken[file+"\x00"+key] {
					diffs = append(diffs, fmt.Sprintf("%s [%s]: a golden run no fixture makes -- if the fixtures changed on purpose, rewrite the goldens with -update", file, key))
				}
			}
		}
	}
	sort.Strings(diffs)
	require.Emptyf(t, diffs, "%d runs differ:\n%s", len(diffs), strings.Join(diffs, "\n"))
	require.Less(t, refused, len(fixtures)/2, "most runs must complete; a majority of refusals means the fixtures do not reach the bodies")

	if update {
		require.NoError(t, writeAutomationGoldens(written))
		t.Logf("wrote %d golden runs over %d automations to %s (%d refused)", len(fixtures), len(written), automationGoldenDir, refused)
		return
	}
	t.Logf("%d arm runs match the goldens over %d automations and %d fixtures (%d refused)", matched, len(arms[0].byName), len(fixtures), refused)
}

// compareAutomationRuns compares two runs of one fixture.
func compareAutomationRuns(a, b logicRecord) error {
	if a.String() != b.String() {
		return fmt.Errorf("they differ")
	}
	return nil
}
