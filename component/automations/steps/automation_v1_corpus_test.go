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
// # Arms, before and after the flips
//
// Two arms run: the tree as it is (legacy), and the tree as `memqlmigrate
// --rewrite=expressions` and then `--rewrite=bodies` carry it (bodies). The
// goldens are written from the bodies arm, with -update, once the two agree,
// the legacy defects excepted (automationLegacyDefects: each checked, each
// mended only in the part that is defective). After the flips the tree is its
// own statement source, the legacy arm's line goes, and the bodies arm against
// the goldens is the statement form held to what the legacy build did --
// evidence only a run before the flips can gather, since the legacy executor
// goes with them. (The expressions flip alone -- the tree between the two
// flips -- is epic memql#5363's to measure: an arm for it here would hold the
// statement form's goldens to that epic's changes.)
//
// # What a run is compared on
//
// The calls it made and the events it published, in order; whether it was
// refused; its status; and its output. A logic call runs for real, each arm
// running its own build of the logic (the logic corpus holds those builds to
// one another) with the logic's construct calls answered by the same probe,
// so an automation reads what its logic really returns. That is also what
// lets a logic the bodies rewrite moves into its automation (a logic may not
// publish, D14) compare: the calls it made from inside the logic are the
// calls the moved statements make. Its return value, which the calling
// automation discarded and the moved statements now end the automation on,
// is not compared.

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
// -- and a logic call, which runs for real. Containers are built here, their
// children dispatched back to this registry.
type automationProbe struct {
	*probeRegistry
	fakes *probeFakes
}

func (r *automationProbe) Execute(ctx context.Context, step *automations.Step, stepCtx *automations.StepContext) (*automations.StepResult, error) {
	if r.probe == nil {
		return nil, fmt.Errorf("probe: step %q runs outside a run", step.ID)
	}
	switch step.Type {
	case automations.StepTypeForEach:
		return (&ForEachExecutor{Registry: r.real, Dispatch: r.Execute}).Execute(ctx, step, stepCtx)
	case automations.StepTypeParallel:
		return (&ParallelExecutor{Registry: r.real, Dispatch: r.Execute}).Execute(ctx, step, stepCtx)
	case automations.StepTypeSwitch:
		return (&SwitchExecutor{Registry: r.real, Dispatch: r.Execute}).Execute(ctx, step, stepCtx)
	case automations.StepTypeAction:
		if step.Action == nil {
			break
		}
		args, err := probeStepArgs(ctx, step, stepCtx, step.Action.Args, true)
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
		args, err := probeStepArgs(ctx, step, stepCtx, step.Automation.Args, false)
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

// probeStepArgs are a step's arguments as its real executor sends them: a v1
// step's evaluated (ResolveV1Map); a legacy step's resolved through
// resolveArgsRefs when legacyResolves, and as written otherwise.
func probeStepArgs(ctx context.Context, step *automations.Step, stepCtx *automations.StepContext, args map[string]any, legacyResolves bool) (map[string]any, error) {
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
	if legacyResolves {
		if resolved, err := resolveArgsRefs(in, stepCtx.Evaluator); err == nil {
			return resolved, nil
		}
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
// the engine, whose LogicRunner answers the logic's own steps from the probe
// (and whose one-`return` logic reach the probe through probeFakes).
func (r *automationProbe) runLogic(ctx context.Context, step *automations.Step, stepCtx *automations.StepContext) (*automations.StepResult, error) {
	var text string
	if step.Exprs != nil {
		args, err := stepCtx.Evaluator.ResolveV1Map(ctx, step.Function.Args)
		if err != nil {
			return nil, err
		}
		text = step.Function.Name + "(" + renderV1CallArgs(args) + ")"
	} else {
		args := step.Function.Args
		if len(args) > 0 {
			resolved, err := resolveArgsRefs(args, stepCtx.Evaluator)
			if err != nil {
				return nil, err
			}
			args = resolved
		}
		text = step.Function.Name + "(" + renderFunctionArgs(args) + ")"
	}
	res, err := r.engine.Execute(ctx, text)
	if err != nil {
		return nil, err
	}
	return &automations.StepResult{StepId: step.ID, Status: "success", Result: r.constructEnvelope(step.Function.Name, res)}, nil
}

// constructEnvelope is a one-`return` logic's result in the envelope
// production hands its caller when the logic returns a query or a mutation: a
// bundle of rows, which an automation reads with `decide.nodes()`. probeFakes
// stands a builtin in for the construct, and a builtin's result comes back as
// flat nodes; any other logic's result is its own.
func (r *automationProbe) constructEnvelope(logic string, res *memql.ExecuteResult) *memql.ExecuteResult {
	fn, ok := r.engine.Functions().Lookup(logic)
	if !ok || res == nil || fn.LogicSteps != nil || fn.LogicBody != nil {
		return res
	}
	call, ok := fn.Expr.(*memql.FunctionCallExpression)
	if !ok {
		return res
	}
	kind := r.kinds[call.Name]
	if kind != "query" && kind != "mutation" {
		return res
	}
	flat, has := res.FlatOutput()
	nodes, isNodes := flat.(map[string]memorynodes.MemoryNode)
	if !has || !isNodes {
		return res
	}
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	rows := make([]any, 0, len(ids))
	for _, id := range ids {
		var payload map[string]any
		_ = json.Unmarshal(nodes[id].Payload, &payload)
		rows = append(rows, map[string]any{"id": id, "concept": nodes[id].Concept, "payload": payload})
	}
	if kind == "mutation" && len(rows) == 1 {
		return stepResultFor(kind, rows[0]).(*memql.ExecuteResult)
	}
	return stepResultFor("query", rows).(*memql.ExecuteResult)
}

// ---------------------------------------------------------------------------
// the arms
// ---------------------------------------------------------------------------

// automationArm is one build of the tree's automations and the executor that
// runs them.
type automationArm struct {
	name   string
	legacy bool
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

// newAutomationArm loads the arm's automations through the boot walk in the
// arm's grammar and wires an executor over one engine, every logic of the
// tree rebuilt from the arm's source and upserted over the booted one, so a
// logic call runs the arm's build of it.
func newAutomationArm(t *testing.T, name string, v1 bool, pick func(*corpusSource) string, sources []*corpusSource) automationArm {
	t.Helper()
	eng := bootEmbeddedEngine(t)
	// A lazy handle satisfies the engine's setup; port 1 makes any database
	// read that escaped the probe fail loudly (see todayArm).
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

	saved := languageParser.DefaultOptions
	languageParser.DefaultOptions = languageParser.Options{ExpressionsV1: v1}
	defer func() { languageParser.DefaultOptions = saved }()
	loaded, err := automations.NewLoader(automations.LoaderOptions{Logger: logger}).LoadFromTree(armTree(t, sources, pick))
	require.NoErrorf(t, err, "%s arm: the tree's automations do not load", name)
	byName := make(map[string]*automations.Automation, len(loaded))
	for _, a := range loaded {
		byName[a.Name] = a
	}

	built := map[string]*memql.Function{}
	for _, f := range sources {
		for _, slice := range memql.ExtractFunctionSlices(pick(f)) {
			if slice.Kind != languageParser.FunctionTypeLogic {
				continue
			}
			fn, err := memql.BuildFunctionConstruct(pick(f), slice.Name, "unified:"+f.Path, memorynodes.DefaultRegistry())
			require.NoErrorf(t, err, "%s arm: %s does not build", name, slice.Name)
			require.NoError(t, eng.Functions().Upsert(fn))
			built[f.Path+" "+slice.Name] = fn
		}
	}

	reg := &automationProbe{probeRegistry: &probeRegistry{real: NewRegistry(), engine: eng, kinds: kinds}, fakes: &probeFakes{kinds: kinds}}
	reg.fakes.install(t, eng, built)
	eng.SetLogicRunner(automations.NewLogicRunner(eng, reg, logger))
	exec := automations.NewExecutor(automations.ExecutorOptions{
		Logger: logger, Engine: eng, EventBus: bus, StepRegistry: reg, SandboxRun: true,
	})
	return automationArm{name: name, legacy: !v1, byName: byName, reg: reg, exec: exec}
}

// run is one fixture's run in comparable form.
func (arm automationArm) run(fx automationFixture, now time.Time) logicRecord {
	a := arm.byName[fx.Name]
	if a == nil {
		return logicRecord{Refused: true, Result: "the arm has no automation " + fx.Name}
	}
	probe := &logicProbe{rows: fx.Rows, now: now}
	arm.reg.probe, arm.reg.fakes.probe = probe, probe
	defer func() { arm.reg.probe, arm.reg.fakes.probe = nil, nil }()
	var event *events.Event
	if fx.Event != nil {
		e := *fx.Event
		e.Payload = cloneMap(fx.Event.Payload)
		event = &e
	}
	exec, err := arm.exec.ExecuteWithEvent(context.Background(), a, "corpus", event)
	rec := logicRecord{Refused: err != nil, Calls: probe.calls}
	if exec != nil {
		result := map[string]any{"status": exec.Status}
		if !fx.MovedLogic {
			result["output"] = canonicalResult(exec.Output)
		}
		rec.Result = canonicalJSON(result)
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
	// MovedLogic: the automation calls a logic the bodies rewrite moves into
	// it, so its output is not compared.
	MovedLogic bool
}

// automationFixtures is every run of the corpus, read off the reference
// arm's build of each automation.
func automationFixtures(t *testing.T, autos map[string]*automations.Automation, moved map[string]bool) []automationFixture {
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
		callsMoved := false
		walkSteps(a.Steps, func(s *automations.Step) {
			if s.Function != nil && moved[s.Function.Name] {
				callsMoved = true
			}
		})
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
					Rows: rows, Label: v.label, MovedLogic: callsMoved,
				})
			}
		}
	}
	return out
}

func walkSteps(steps []*automations.Step, visit func(*automations.Step)) {
	for _, s := range steps {
		if s == nil {
			continue
		}
		visit(s)
		if s.ForEach != nil {
			walkSteps(s.ForEach.Do, visit)
		}
		if s.Parallel != nil {
			walkSteps(s.Parallel.Branches, visit)
		}
		if s.Block != nil {
			walkSteps(s.Block.Steps, visit)
		}
		if s.Switch != nil {
			cases := []*automations.SwitchCase{s.Switch.Default}
			for _, c := range s.Switch.Cases {
				cases = append(cases, c)
			}
			for _, c := range cases {
				if c != nil {
					walkSteps(append([]*automations.Step{c.Step}, c.Steps...), visit)
				}
			}
		}
	}
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

// movedLogic are the logic the bodies rewrite moves into their automations:
// declared in the tree, and no longer in its statement source.
func movedLogic(sources []*corpusSource) map[string]bool {
	out := map[string]bool{}
	for _, f := range sources {
		kept := map[string]bool{}
		for _, s := range memql.ExtractFunctionSlices(f.Bodies) {
			if s.Kind == languageParser.FunctionTypeLogic {
				kept[s.Name] = true
			}
		}
		for _, s := range memql.ExtractFunctionSlices(f.Current) {
			if s.Kind == languageParser.FunctionTypeLogic && !kept[s.Name] {
				out[s.Name] = true
			}
		}
	}
	return out
}

// TestAutomationCorpusRuns: every automation of the tree, run by every arm
// against every fixture, makes the calls and publishes the events its golden
// holds, in their order, and ends as the golden says -- and the arms agree,
// the legacy arm where it does not do what the body says excepted
// (automationLegacyDefects). With -update it writes the goldens from the
// bodies arm's runs instead, once they agree.
func TestAutomationCorpusRuns(t *testing.T) {
	sources := logicCorpusSources(t)
	moved := movedLogic(sources)
	// The first arm is the one the others are compared with. The flip deletes
	// the legacy arm's line: the tree then has no legacy source.
	arms := []automationArm{
		newAutomationArm(t, "legacy", false, func(f *corpusSource) string { return f.Current }, sources),
		newAutomationArm(t, "bodies", true, func(f *corpusSource) string { return f.Bodies }, sources),
	}
	legacyRuns := false
	for _, arm := range arms {
		legacyRuns = legacyRuns || arm.legacy
	}
	golden := len(arms) - 1 // the bodies arm: the statement form the goldens hold

	fixtures := automationFixtures(t, arms[0].byName, moved)
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
	mended := map[string]int{}
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
			m, err := compareAutomationRuns(fx, arms[0].legacy, records[0], arms[i].legacy, records[i])
			if m {
				mended[fx.Name]++
			}
			if err != nil {
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
			entry := map[string]any{"input": automationInput(fx), "outcome": records[golden].outcome()}
			if d, ok := automationLegacyDefects[fx.Name]; ok && d.applies(fx) && legacyRuns {
				entry["legacyDefect"] = d.why
			} else if prev, ok := goldens[file][key]; ok && !legacyRuns && jsonText(prev["outcome"]) == records[golden].String() && prev["legacyDefect"] != nil {
				// No legacy arm left to show the defect: an unchanged run keeps
				// the note an earlier -update wrote.
				entry["legacyDefect"] = prev["legacyDefect"]
			}
			gf.runs[key] = entry
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
			m, err := compareAutomationRuns(fx, arm.legacy, records[i], false, want)
			if m {
				mended[fx.Name]++
			}
			if err != nil {
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
	if legacyRuns {
		for name, d := range automationLegacyDefects {
			require.Positivef(t, mended[name], "the legacy defect of %s (%s) applies to no run: the entry is stale", name, d.why)
		}
	}
	require.Less(t, refused, len(fixtures)/2, "most runs must complete; a majority of refusals means the fixtures do not reach the bodies")

	if update {
		require.NoError(t, writeAutomationGoldens(written))
		t.Logf("wrote %d golden runs over %d automations to %s (%d refused)", len(fixtures), len(written), automationGoldenDir, refused)
		return
	}
	t.Logf("%d arm runs match the goldens over %d automations and %d fixtures (%d refused)", matched, len(arms[0].byName), len(fixtures), refused)
}

// ---------------------------------------------------------------------------
// legacy defects
// ---------------------------------------------------------------------------

// automationDefect is a run in which the LEGACY build does not do what the
// body says and the statement form does -- legacyDefect's counterpart for an
// automation's run. mend checks both halves (the defect in the legacy record,
// the value the body means in the other) and removes the defective part from
// both, so everything else the run did is still held to equality; a mend that
// finds no defect fails the gate.
type automationDefect struct {
	why     string
	applies func(fx automationFixture) bool
	mend    func(fx automationFixture, legacy, other *logicRecord) error
}

// automationLegacyDefects are the legacy defects the migration corrects, by
// automation. The first seven are the logic corpus's (logicLegacyDefects) as
// the automation that calls each shows them: every one sits in the calls and
// events the logic makes, which the moved statements make alike; the halves in
// a logic's return value do not apply, since that output is not compared.
var automationLegacyDefects = map[string]automationDefect{
	"onDelegationCreated": {
		why:     logicLegacyDefects["onDelegationCreated"].why,
		applies: func(automationFixture) bool { return true },
		mend: func(_ automationFixture, legacy, other *logicRecord) error {
			return mendEventField(legacy, other, "delegation.created", "timestamp", isInstant)
		},
	},
	"purgeExpiredSafetyClassifications": fromLogicDefect("purgeExpiredSafetyClassifications"),
	"purgeExpiredOutputScreenings":      fromLogicDefect("purgeExpiredOutputScreenings"),
	"auditEventRetentionSweep":          fromLogicDefect("auditEventRetentionSweep"),
	"accountDeletionReminder7Days":      fromLogicDefect("accountDeletionReminder7Days"),
	"accountDeletionReminder25Days":     fromLogicDefect("accountDeletionReminder25Days"),
	"conflictDetection": {
		why:     logicLegacyDefects["conflictDetection"].why,
		applies: func(fx automationFixture) bool { return fx.Event != nil && fx.Rows > 0 },
		mend: func(fx automationFixture, legacy, other *logicRecord) error {
			const topic = "data.conflicts.detected"
			if eventCall(legacy, topic) != nil {
				return fmt.Errorf("the legacy run published the conflict event")
			}
			ev := eventCall(other, topic)
			if ev == nil {
				return fmt.Errorf("the statement run did not publish the conflict event")
			}
			if n, _ := ev.Args["matchCount"].(float64); int(n) != fx.Rows {
				return fmt.Errorf("the statement run's event counts %v matches, want %d", ev.Args["matchCount"], fx.Rows)
			}
			if matches, _ := ev.Args["matches"].([]any); len(matches) != fx.Rows {
				return fmt.Errorf("the statement run's event carries %d matches, want %d", len(matches), fx.Rows)
			}
			other.Calls = withoutEvent(other.Calls, topic)
			return nil
		},
	},
	"bootstrapCluster": {
		why: "`?? []` and `?? 5432` fall back to the text `()` and the string \"5432\": the legacy argument renderer writes an empty list's " +
			"source and quotes the number, so createDatabase and createIdentityProvider are handed text",
		// Only a bff node refreshes the rows those two calls write.
		applies: func(fx automationFixture) bool { return fx.Label == "event bff" },
		mend: func(_ automationFixture, legacy, other *logicRecord) error {
			fields := map[string]map[string]struct{ legacy, other any }{
				"createDatabase":         {"extensions": {"()", []any{}}, "port": {"5432", float64(5432)}},
				"createIdentityProvider": {"acceptedAudiences": {"()", []any{}}},
			}
			found := 0
			for name, want := range fields {
				lc, oc := lastCall(legacy, name), lastCall(other, name)
				if lc == nil || oc == nil {
					continue // a run that does not refresh the rows makes no such call
				}
				for field, w := range want {
					if jsonText(lc.Args[field]) != jsonText(w.legacy) || jsonText(oc.Args[field]) != jsonText(w.other) {
						return fmt.Errorf("%s.%s is %v in the legacy run and %v in the statement run", name, field, lc.Args[field], oc.Args[field])
					}
					delete(lc.Args, field)
					delete(oc.Args, field)
					found++
				}
			}
			if found == 0 {
				return fmt.Errorf("neither run refreshed the database or identity provider row")
			}
			return nil
		},
	},
	"bringUpInstance": {
		why: "a sub-automation call hands over each argument's reference text rather than its value -- the legacy step passed its " +
			"args unresolved, which memql#5367 closed for a v1 step -- so provisionInstance and installInstance receive their own " +
			"argument names",
		applies: func(fx automationFixture) bool { return fx.Event != nil },
		mend: func(_ automationFixture, legacy, other *logicRecord) error {
			found := 0
			for i, lc := range legacy.Calls {
				if lc.Kind != "automation" || i >= len(other.Calls) || other.Calls[i].Kind != "automation" || other.Calls[i].Name != lc.Name {
					continue
				}
				oc := other.Calls[i]
				for field, v := range lc.Args {
					s, isText := v.(string)
					if !isText || s != field || jsonText(oc.Args[field]) == jsonText(v) {
						return fmt.Errorf("the legacy %s.%s is %v, not the argument's own name", lc.Name, field, v)
					}
					delete(lc.Args, field)
					delete(oc.Args, field)
					found++
				}
			}
			if found == 0 {
				return fmt.Errorf("the legacy run made no sub-automation call")
			}
			return nil
		},
	},
}

// fromLogicDefect is a logic's legacy defect as the automation that calls it
// shows it, for a defect whose mend reads only the calls and the row count.
func fromLogicDefect(logic string) automationDefect {
	d := logicLegacyDefects[logic]
	asLogic := func(fx automationFixture) logicFixture { return logicFixture{Name: logic, Rows: fx.Rows} }
	return automationDefect{
		why:     d.why,
		applies: func(fx automationFixture) bool { return d.applies(asLogic(fx)) },
		mend: func(fx automationFixture, legacy, other *logicRecord) error {
			return d.mend(asLogic(fx), legacy, other)
		},
	}
}

// compareAutomationRuns compares two runs of one fixture; when exactly one is
// the legacy arm's and the automation has a legacy defect that applies, the
// defect is checked and mended first (mended reports it).
func compareAutomationRuns(fx automationFixture, aLegacy bool, a logicRecord, bLegacy bool, b logicRecord) (mended bool, err error) {
	a, b = a.clone(), b.clone()
	if d, ok := automationLegacyDefects[fx.Name]; ok && aLegacy != bLegacy && d.applies(fx) {
		legacy, other := &a, &b
		if bLegacy {
			legacy, other = &b, &a
		}
		if err := d.mend(fx, legacy, other); err != nil {
			return false, fmt.Errorf("the legacy defect (%s) does not hold: %w", d.why, err)
		}
		mended = true
	}
	if a.String() != b.String() {
		return mended, fmt.Errorf("they differ")
	}
	return mended, nil
}
