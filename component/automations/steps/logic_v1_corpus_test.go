package steps

// logic_v1_corpus_test.go -- the logic-body corpus (memql#5367).
//
// Every logic construct of the tree is run against the same fixture inputs --
// argument variants, four actors, two row counts -- with every construct call
// answered by one probe, which records it. Each ARM runs the tree its own way,
// and every arm must return the same result and make the same calls, in the
// same order, as every other arm and as the GOLDENS: the agreed runs, one JSON
// file per construct under testdata/logic_corpus/<domain>/<name>.json.
//
// # Before and after the flip
//
// Before the tree is migrated to edition 2026, two arms run: the legacy build
// of the tree as it is, and the v1 build of the tree as `memqlmigrate
// --rewrite=expressions` migrates it (the codemod's own entry points, run in
// process). Their agreement is what the goldens were written from, with
// `go test -run TestLogicCorpusRuns -update`: the migration's no-change claim
// for logic bodies, at run time (the load-time half is component/memql's
// TestV1CorpusLogicBodiesBuild).
//
// After the flip the tree IS edition 2026 -- the flip migrates it and turns
// on langparser.DefaultOptions.ExpressionsV1 in one change -- so the v1 arm
// reads the tree's own files, with no codemod step (logicCorpusSources keys on
// DefaultOptions), and the legacy arm, which has no legacy source left to
// read, is deleted by deleting its line in TestLogicCorpusRuns. The v1 arm
// against the goldens is then the whole test.
//
// # Arms
//
// An arm is a name, the source it reads, and a run: a function from (source,
// fixture arguments, probe) to a result; the calls it made are the probe's
// record. Today's two go through the engine's own logic dispatch --
// engine.Execute of the construct's call, with the LogicRunner wired, so the
// engine decides between fn.Expr and fn.LogicSteps as it does in production.
// A runner that replaces them adds its arm to the list in TestLogicCorpusRuns
// and is held to the same goldens. Nothing but the two arms is specific to
// today's runners: a construct call's answer is given in the statement's own
// terms -- a query answers its rows (a []any of row maps), a builtin its
// result, a logic its return value, a mutation the row it wrote -- which each
// arm adapts to its runner (probeRegistry and probeFakes do it for today's).
// Calls are compared in order, the order the legacy topological sort ran
// them. Journal writes are not calls: only construct calls and published
// events are recorded.
//
// In ten constructs the legacy build does not do what the body says
// (logicLegacyDefects: each checked, each mended only in the part that is
// defective). The goldens hold what the body says, and name the defect where
// the legacy build differs.
//
// Two things the comparison does not pin, because a later runner may answer
// them differently without changing what a logic does: the envelope a result
// comes back in (canonicalResult reads an engine result as its flat output or
// its rows, so `return query x()` compares as the rows) and the text of an
// error (two refusals compare equal). Instants are masked: two runs a moment
// apart disagree on the clock.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/automations"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/events"
	memqlv1 "github.com/znasllc-io/memql/component/grpc/gen"
	"github.com/znasllc-io/memql/component/language/ast"
	"github.com/znasllc-io/memql/component/language/bodymigrate"
	languageParser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/memql/baseloader"
	"google.golang.org/protobuf/types/known/structpb"
)

// ---------------------------------------------------------------------------
// the probe
// ---------------------------------------------------------------------------

// probeCall is one call a run made: a construct call (query / mutation /
// builtin / logic), with the arguments the construct received, or a
// published event (kind "event", name the topic, args the payload).
type probeCall struct {
	Kind string         `json:"kind"`
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

// logicProbe answers every construct call of one run and records it. Its
// answers depend only on the call and the fixture's row count, so every arm
// is answered alike.
type logicProbe struct {
	rows  int       // rows a query answers with
	now   time.Time // the clock the canned rows are dated against
	calls []probeCall
}

// answer records a construct call and returns its value in the statement's
// terms.
func (p *logicProbe) answer(kind, name string, args map[string]any) any {
	p.calls = append(p.calls, probeCall{Kind: kind, Name: name, Args: args})
	switch kind {
	case "query":
		rows := make([]any, 0, p.rows)
		for i := 1; i <= p.rows; i++ {
			rows = append(rows, map[string]any{
				"id":      fmt.Sprintf("v1:probe:row:%s-%d", name, i),
				"concept": probeRowConcept,
				"payload": probeRowPayload(p.now, name, i, args),
			})
		}
		return rows
	case "mutation":
		id, _ := args["id"].(string)
		if id == "" {
			id = "v1:probe:row:" + name + "-written"
		}
		return map[string]any{"id": id, "concept": probeRowConcept, "payload": args}
	case "logic":
		return map[string]any{"logic": name}
	default: // builtin
		return map[string]any{"builtin": name, "args": args}
	}
}

// event records a published event.
func (p *logicProbe) event(topic string, payload map[string]any) {
	p.calls = append(p.calls, probeCall{Kind: "event", Name: topic, Args: payload})
}

const (
	probeRowConcept    = "v1:probe:row"
	probeAnswerConcept = "v1:probe:answer"
)

// probeRowPayload is one canned row: every field a logic body of the tree
// reads off a row, so a read that goes wrong in one arm reads a value in the
// other. The first row is a week into a deletion cooldown, the second far
// past it, so the reminder windows split them.
func probeRowPayload(now time.Time, name string, i int, args map[string]any) map[string]any {
	scheduled := now.Add(-(7*24 + 12) * time.Hour)
	if i > 1 {
		scheduled = now.Add(-(25*24 + 12) * time.Hour)
	}
	rank := 100
	switch args["slug"] {
	case "owner":
		rank = 400
	case "developer":
		rank = 300
	case "admin":
		rank = 200
	}
	return map[string]any{
		"name":                fmt.Sprintf("%s-%d", name, i),
		"value":               "7",
		"status":              "provisioned",
		"rank":                rank,
		"primaryEmail":        fmt.Sprintf("person%d@example.test", i),
		"deletionScheduledAt": scheduled.UTC().Format(time.RFC3339),
	}
}

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// corpusSource is one .memql file of the tree: as it is, and in edition 2026.
type corpusSource struct {
	Path string
	// Current is the file as the tree has it.
	Current string
	// V1 is the file in edition 2026: Current once the tree is migrated,
	// and the expressions codemod's rewrite of it before.
	V1 string
	// Bodies is V1 with its bodies written in statements (epic memql#5370):
	// V1 carried by the bodies rewrite (withBodies), which leaves a tree
	// already in statements unchanged.
	Bodies string
}

// logicFixture is one run: a logic construct, the arguments it is called
// with, the actor it runs as, and how many rows every query answers.
type logicFixture struct {
	Key   string // "<path> <name>"
	File  *corpusSource
	Name  string
	Args  map[string]any
	Role  string
	Rows  int
	Label string // the argument variant, for messages
}

// logicCorpusSources reads the tree the engine loads. The tree is written in
// the grammar the engine parses it with by default -- the flip migrates the
// tree and turns on langparser.DefaultOptions.ExpressionsV1 in one change --
// so once that is edition 2026 the files are their own v1 source; before, the
// v1 source is memqlmigrate --rewrite=expressions' output, from its own entry
// points (CollectPredicates over every file, then RewriteExpressions per
// file).
func logicCorpusSources(t *testing.T) []*corpusSource {
	t.Helper()
	files := baseloader.ReadAll(nil)
	require.NotEmpty(t, files, "the tree reads no .memql file")
	out := make([]*corpusSource, 0, len(files))
	if languageParser.DefaultOptions.ExpressionsV1 {
		for _, f := range files {
			out = append(out, &corpusSource{Path: f.Path, Current: f.Content, V1: f.Content})
		}
		return withBodies(t, out)
	}
	byPath := make(map[string][]byte, len(files))
	for _, f := range files {
		byPath[f.Path] = []byte(f.Content)
	}
	preds, err := languageParser.CollectPredicates(byPath)
	require.NoError(t, err)
	for _, f := range files {
		migrated, err := languageParser.RewriteExpressions([]byte(f.Content), preds)
		require.NoErrorf(t, err, "the codemod refuses %s", f.Path)
		out = append(out, &corpusSource{Path: f.Path, Current: f.Content, V1: string(migrated)})
	}
	return withBodies(t, out)
}

// withBodies sets every source's Bodies the way `memqlmigrate
// --rewrite=bodies` writes it: one rewrite over the whole tree's V1 text, its
// bare calls resolved over the tree. The tree is the engine's embedded one,
// so the index needs nothing beyond it.
func withBodies(t *testing.T, out []*corpusSource) []*corpusSource {
	t.Helper()
	files := make(map[string][]byte, len(out))
	for _, f := range out {
		files[f.Path] = []byte(f.V1)
	}
	changed, err := bodymigrate.RewriteTree(files, bodymigrate.IndexFiles(files))
	require.NoError(t, err, "the bodies rewrite refuses the tree")
	for _, f := range out {
		f.Bodies = f.V1
		if b, ok := changed[f.Path]; ok {
			f.Bodies = string(b)
		}
	}
	return out
}

// logicArgVariants are the argument sets a construct is called with beyond
// the schema's own (logicSchemaArgs): the inputs that take each branch of a
// decision.
var logicArgVariants = map[string][]map[string]any{
	"runIsTerminal":            {{"status": "succeeded"}, {"status": "abandoned"}, {"status": "running"}, {}},
	"requestRouteStatus":       {{"submitterRole": "owner"}, {"submitterRole": "admin"}, {"submitterRole": "writer"}, {"submitterRole": "reader"}, {}},
	"transitionEventKind":      {{"status": "needs_approval", "oldStatus": "draft"}, {"status": "queued", "oldStatus": "needs_approval"}, {"status": "rejected", "oldStatus": "queued"}, {"status": "changes_requested", "oldStatus": "queued"}, {"status": "queued", "oldStatus": "queued"}, {}},
	"deployGateGreen":          {{"gate": map[string]any{"passed": true}}, {"gate": map[string]any{"passed": false}}, {"gate": map[string]any{}}},
	"deployRollbackTarget":     {{"passed": true, "previousDeploymentId": "dep-1"}, {"passed": false, "previousDeploymentId": "dep-1"}, {"passed": false}, {}},
	"deployOutcomeLabel":       {{"passed": true}, {"passed": false}, {}},
	"installDependencyVerdict": {{"passed": true}, {"passed": false}, {}},
	"renderDiffVerdict":        {{"passed": true, "required": true}, {"passed": false, "required": true}, {"passed": false, "required": false}, {}},
	"nextDeploymentVersion":    {{"current": "1.2.3"}, {"current": "1.2.3", "bump": "minor"}},
	"governanceCanManagePrincipal": {
		{"targetUserId": "user-2", "targetRoleSlug": "user", "verb": "update"},
		{"targetUserId": "user-2", "targetRoleSlug": "owner", "verb": "delete"},
	},
	"governanceCanCreatePrincipal": {{"newRoleSlug": "user"}, {"newRoleSlug": "owner"}},
	"revokeExpiredDelegations":     {{"asOf": "2026-09-01T00:00:00Z"}},
}

// logicRoles are the actors every fixture runs as: the role gates of the tree
// decide on each of them differently.
var logicRoles = []string{"owner", "admin", "developer", "user"}

// probeEvent is the triggering event a logic that declares `event` is called
// with: every payload field a logic body of the tree reads.
func probeEvent(nodeType string) map[string]any {
	return map[string]any{
		"topic": "graph.node.created.v1:probe:thing",
		"kind":  "node.created",
		"payload": map[string]any{
			"id":               "v1:probe:thing:1",
			"status":           "succeeded",
			"partitionId":      "space-1",
			"recordType":       "contact",
			"naturalKeyField":  "email",
			"naturalKeyValue":  "someone@example.test",
			"identityId":       "identity-1",
			"identitySubject":  "subject-1",
			"identityType":     "passkey",
			"agentId":          "agent-1",
			"roleCeiling":      "user",
			"scopes":           []any{"read"},
			"createdBySubject": "subject-0",
			"node":             map[string]any{"type": nodeType, "id": "v1:cluster:node:n1"},
		},
	}
}

// logicSchemaArgs are the argument sets read off a construct's args block:
// every field, and the required ones only; a declared `event` takes the
// probe event under two node types.
func logicSchemaArgs(fn *memql.Function) []map[string]any {
	if fn.ArgsSchema == nil || len(fn.ArgsSchema.Fields) == 0 {
		return []map[string]any{{}}
	}
	var out []map[string]any
	for _, requiredOnly := range []bool{false, true} {
		for _, nodeType := range []string{"bff", "agent"} {
			args := map[string]any{}
			hasEvent := false
			for _, f := range fn.ArgsSchema.Fields {
				if f == nil || (requiredOnly && f.Optional) {
					continue
				}
				if f.Name == "event" {
					args["event"] = probeEvent(nodeType)
					hasEvent = true
					continue
				}
				args[f.Name] = logicFieldValue(f)
			}
			out = append(out, args)
			if !hasEvent {
				break
			}
		}
	}
	return out
}

func logicFieldValue(f *memql.FunctionArgsField) any {
	if len(f.Enum) > 0 {
		return f.Enum[0]
	}
	typ := strings.ToLower(f.Type)
	switch {
	case typ == "int" || typ == "integer" || typ == "number" || typ == "float":
		return float64(3)
	case typ == "bool" || typ == "boolean":
		return true
	case typ == "object":
		m := map[string]any{}
		for _, n := range f.Nested {
			if n != nil {
				m[n.Name] = logicFieldValue(n)
			}
		}
		return m
	case typ == "array" || strings.HasPrefix(typ, "[]"):
		return []any{"a"}
	case typ == "datetime" || typ == "date":
		return "2026-09-13T00:00:00Z"
	}
	return "s-" + f.Name
}

// ---------------------------------------------------------------------------
// the arms
// ---------------------------------------------------------------------------

// logicArm is one way of running a logic construct. run is called once per
// fixture with the source the arm reads; the probe is that fixture's.
type logicArm struct {
	name string
	// legacy marks the arm that runs the legacy grammar, whose defects
	// (logicLegacyDefects) are mended before it is compared.
	legacy bool
	source func(*corpusSource) string
	run    func(ctx context.Context, src, path, name string, args map[string]any, probe *logicProbe) (any, error)
	// moved reports a logic the arm's source no longer declares, because the
	// bodies rewrite moved it into the one automation that calls it (a logic
	// may not publish, D14). The arm has no run of it to compare.
	moved func(path, name string) bool
}

// todayArm runs the tree through today's runners: one engine per arm, booted
// on the embedded tree, every logic construct of the tree rebuilt from the
// arm's source with the arm's grammar through the loader's own entry point
// (memql.BuildFunctionConstruct) and upserted over the booted one, the
// LogicRunner wired with probeRegistry, and each run an engine.Execute of the
// construct's call -- so the engine picks fn.Expr or fn.LogicSteps as it does
// in production.
func todayArm(t *testing.T, name string, v1 bool, sources []*corpusSource) logicArm {
	t.Helper()
	pick := func(f *corpusSource) string { return f.Current }
	if v1 {
		pick = func(f *corpusSource) string { return f.V1 }
	}
	return engineArm(t, name, v1, false, pick, sources)
}

// bodiesArm runs the tree with its bodies in statements (corpusSource.Bodies):
// every logic builds to a statement body (fn.LogicBody) and each run reaches
// the LogicRunner's RunLogicBody through the engine, the dispatch production
// takes. Before the flip it holds the statement runtime to the goldens run for
// run; after it, the tree is its own statement source and this arm is the v1
// arm.
func bodiesArm(t *testing.T, sources []*corpusSource) logicArm {
	t.Helper()
	return engineArm(t, "bodies", true, true, func(f *corpusSource) string { return f.Bodies }, sources)
}

// engineArm is an arm through the engine: todayArm's and bodiesArm's.
// statements requires every logic to build as a statement body.
func engineArm(t *testing.T, name string, v1, statements bool, pick func(*corpusSource) string, sources []*corpusSource) logicArm {
	t.Helper()
	eng := bootEmbeddedEngine(t)
	// A lazy handle satisfies engine.Execute's setup; port 1 makes any
	// database read a construct call escaped the probe fail loudly instead
	// of borrowing a developer's database (workbench_teardown_arguments_test.go).
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

	// build builds one logic construct from a file's source in the arm's
	// grammar, through the loader's own entry point; a construct is built
	// once per source text.
	type builtKey struct{ path, name string }
	type builtEntry struct {
		src string
		fn  *memql.Function
	}
	cache := map[builtKey]builtEntry{}
	build := func(src, path, logic string) (*memql.Function, error) {
		key := builtKey{path, logic}
		if e, ok := cache[key]; ok && e.src == src {
			return e.fn, nil
		}
		saved := languageParser.DefaultOptions
		languageParser.DefaultOptions = languageParser.Options{ExpressionsV1: v1}
		fn, err := memql.BuildFunctionConstruct(src, logic, "unified:"+path, memorynodes.DefaultRegistry())
		languageParser.DefaultOptions = saved
		if err != nil {
			return nil, err
		}
		if fn.LogicSteps != nil && fn.LogicSteps.ExpressionsV1 != v1 {
			return nil, fmt.Errorf("%s arm: %s did not build in the arm's grammar", name, logic)
		}
		if statements && fn.LogicBody == nil {
			return nil, fmt.Errorf("%s arm: %s did not build as a statement body", name, logic)
		}
		cache[key] = builtEntry{src: src, fn: fn}
		return fn, nil
	}

	// The logic the arm's source still declares: the bodies rewrite moves a
	// publishing logic into its automation, and the arm has nothing to run
	// for it.
	declared := map[string]bool{}
	for _, f := range sources {
		for _, slice := range memql.ExtractFunctionSlices(pick(f)) {
			if slice.Kind == languageParser.FunctionTypeLogic {
				declared[f.Path+" "+slice.Name] = true
			}
		}
	}
	moved := func(path, logic string) bool { return !declared[path+" "+logic] }

	// Every logic construct of the tree builds, up front: a construct that
	// does not is the arm's failure, not one fixture's, and the builds name
	// the constructs a one-`return` body reaches through fn.Expr (probeFakes).
	built := map[string]*memql.Function{}
	var failures []string
	for _, f := range sources {
		for _, slice := range memql.ExtractFunctionSlices(f.Current) {
			if slice.Kind != languageParser.FunctionTypeLogic || moved(f.Path, slice.Name) {
				continue
			}
			fn, err := build(pick(f), f.Path, slice.Name)
			if err != nil {
				failures = append(failures, f.Path+" "+slice.Name+": "+err.Error())
				continue
			}
			built[f.Path+" "+slice.Name] = fn
		}
	}
	require.Emptyf(t, failures, "%s arm: %d logic constructs do not build:\n%s", name, len(failures), strings.Join(failures, "\n"))

	reg := &probeRegistry{real: NewRegistry(), engine: eng, kinds: kinds}
	eng.SetLogicRunner(automations.NewLogicRunner(eng, reg, slog.New(slog.NewTextHandler(io.Discard, nil))))
	fakes := &probeFakes{kinds: kinds}
	fakes.install(t, eng, built)

	return logicArm{
		name:   name,
		legacy: !v1,
		source: pick,
		run: func(ctx context.Context, src, path, logic string, args map[string]any, probe *logicProbe) (any, error) {
			fn, err := build(src, path, logic)
			if err != nil {
				return nil, err
			}
			if err := eng.Functions().Upsert(fn); err != nil {
				return nil, err
			}
			reg.probe, fakes.probe = probe, probe
			defer func() { reg.probe, fakes.probe = nil, nil }()
			return eng.Execute(ctx, logic+"("+renderV1NamedArgs(args)+")")
		},
		moved: moved,
	}
}

// probeFakes stands a probe in for every construct a one-`return` logic
// calls, which the engine reaches through fn.Expr rather than a step: each
// becomes a builtin, with the construct's own argument schema, whose executor
// asks the probe. So the call's arguments are validated as the real
// construct's would be, and what the probe records is what argument expansion
// handed it.
type probeFakes struct {
	kinds map[string]string
	probe *logicProbe
}

type probeIntegration struct{ caps []memql.IntegrationCapability }

func (p *probeIntegration) IntegrationName() string                     { return "logicprobe" }
func (p *probeIntegration) Capabilities() []memql.IntegrationCapability { return p.caps }

func (f *probeFakes) install(t *testing.T, eng *memql.MemQLEngine, built map[string]*memql.Function) {
	t.Helper()
	targets := map[string]bool{}
	for _, fn := range built {
		if call, ok := fn.Expr.(*memql.FunctionCallExpression); ok && fn.LogicSteps == nil {
			if _, known := f.kinds[call.Name]; known && f.kinds[call.Name] != "logic" {
				targets[call.Name] = true
			}
		}
	}
	names := make([]string, 0, len(targets))
	for n := range targets {
		names = append(names, n)
	}
	sort.Strings(names)
	integration := &probeIntegration{}
	for _, n := range names {
		real, ok := eng.Functions().Lookup(n)
		require.Truef(t, ok, "%s is called and not registered", n)
		kind := f.kinds[n]
		construct := n
		integration.caps = append(integration.caps, memql.IntegrationCapability{
			Name: construct,
			Handler: func(_ context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
				if f.probe == nil {
					return nil, fmt.Errorf("probe: %s called outside a run", construct)
				}
				return answerNodes(f.probe.answer(kind, construct, canonicalArgs(args)))
			},
		})
		require.NoError(t, eng.Functions().Upsert(&memql.Function{
			Name:         construct,
			Origin:       real.Origin,
			FunctionKind: memql.FunctionTypeBuiltin,
			Executor:     "integration.logicprobe." + construct,
			Enabled:      true,
			ArgsSchema:   real.ArgsSchema,
		}))
	}
	require.NoError(t, eng.RegisterIntegration(integration))
}

// answerNodes is a probe answer as a builtin executor's nodes: rows are rows,
// and any other answer is one node carrying it (canonicalResult reads it back).
func answerNodes(answer any) ([]memorynodes.MemoryNode, error) {
	if rows, ok := answer.([]any); ok {
		out := make([]memorynodes.MemoryNode, 0, len(rows))
		for _, r := range rows {
			m := r.(map[string]any)
			payload, err := json.Marshal(m["payload"])
			if err != nil {
				return nil, err
			}
			out = append(out, memorynodes.MemoryNode{ID: m["id"].(string), Concept: probeRowConcept, Type: "node", Payload: payload})
		}
		return out, nil
	}
	payload, err := json.Marshal(answer)
	if err != nil {
		return nil, err
	}
	return []memorynodes.MemoryNode{{ID: probeAnswerConcept + ":1", Concept: probeAnswerConcept, Type: "node", Payload: payload}}, nil
}

// probeRegistry is the LogicRunner's step registry for today's arms: every
// step runs on its real executor except a construct call, which the probe
// answers with the arguments the real executor would have sent -- rendered
// as it renders them and read back as the engine reads them -- and a
// published event, which runs for real and is recorded. Containers are built
// here with Dispatch pointed back at Execute, so a nested step re-enters it
// (child_dispatch.go).
type probeRegistry struct {
	real   *Registry
	engine *memql.MemQLEngine
	kinds  map[string]string
	probe  *logicProbe
}

func (r *probeRegistry) Execute(ctx context.Context, step *automations.Step, stepCtx *automations.StepContext) (*automations.StepResult, error) {
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
	case automations.StepTypeEvent:
		res, err := r.real.Execute(ctx, step, stepCtx)
		if err == nil && res != nil {
			if m, ok := res.Result.(map[string]any); ok {
				topic, _ := m["topic"].(string)
				payload, _ := m["payload"].(map[string]any)
				r.probe.event(topic, canonicalArgs(payload))
			}
		}
		return res, err
	case automations.StepTypeFunction:
		if step.Function == nil || (step.Exprs == nil && isExpressionBuiltinName(step.Function.Name)) {
			return r.real.Execute(ctx, step, stepCtx)
		}
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
		return r.call(step, text)
	case automations.StepTypeQuery:
		if step.Query == nil {
			return r.real.Execute(ctx, step, stepCtx)
		}
		if x := step.Exprs; x != nil {
			call, ok := ast.Unparen(x.Query).(*ast.CallExpr)
			if !ok || call.Kind == "" {
				return r.real.Execute(ctx, step, stepCtx) // evaluated in process
			}
			text, err := v1ConstructCallText(ctx, stepCtx.Evaluator, call)
			if err != nil {
				return nil, err
			}
			return r.call(step, text)
		}
		text, err := stepCtx.Evaluator.EvaluateStringForQuery(step.Query.Query)
		if err != nil {
			return nil, err
		}
		return r.call(step, text)
	case automations.StepTypeMutation, automations.StepTypeWebhook, automations.StepTypeAction,
		automations.StepTypeAutomation, automations.StepTypeEmitConceptCard:
		return nil, fmt.Errorf("probe: step %q is a %s step, which this corpus has no answer for -- teach probeRegistry to record it", step.ID, step.Type)
	}
	return r.real.Execute(ctx, step, stepCtx)
}

// call answers the construct call text a step would have sent to the engine.
func (r *probeRegistry) call(step *automations.Step, text string) (*automations.StepResult, error) {
	name, args, err := readCallText(text)
	if err != nil {
		return nil, fmt.Errorf("probe: step %q sends %q, which the engine does not read as a construct call: %w", step.ID, text, err)
	}
	kind, known := r.kinds[name]
	if !known {
		return nil, fmt.Errorf("probe: step %q calls %s, which the tree does not declare", step.ID, name)
	}
	answer := r.probe.answer(kind, name, args)
	return &automations.StepResult{StepId: step.ID, Status: "success", Result: stepResultFor(kind, answer)}, nil
}

// stepResultFor is a probe answer as the engine hands a step its result: a
// query's rows (and a mutation's row) as a bundle, anything else as the
// flat output a builtin's or a logic's result carries.
func stepResultFor(kind string, answer any) any {
	var rows []any
	switch kind {
	case "query":
		rows = answer.([]any)
	case "mutation":
		rows = []any{answer}
	default:
		return memql.NewResultWithOutput(answer)
	}
	bundle := &memqlv1.GraphBundle{}
	for _, r := range rows {
		m := r.(map[string]any)
		payload, _ := structpb.NewStruct(m["payload"].(map[string]any))
		id := m["id"].(string)
		bundle.Nodes = append(bundle.Nodes, &memqlv1.MemoryNode{Id: id, Concept: probeRowConcept, Payload: payload})
		bundle.RootIds = append(bundle.RootIds, id)
	}
	return &memql.ExecuteResult{Bundle: bundle}
}

// readCallText reads a construct call's text as the engine reads it: the
// language parser's expression, a call, whose arguments are literals. An
// argument that does not read back as a literal -- reference text the
// renderer let through -- is recorded as the node it became, so an arm that
// hands the engine an expression instead of a value differs from one that
// does not.
func readCallText(text string) (string, map[string]any, error) {
	parsed, err := languageParser.ParseExpression(text)
	if err != nil {
		return "", nil, err
	}
	call, ok := parsed.(*languageParser.FunctionCallExpr)
	if !ok {
		return "", nil, fmt.Errorf("parsed as %T", parsed)
	}
	args := make(map[string]any, len(call.Args))
	for k, v := range call.Args {
		args[k] = literalArg(v)
	}
	if len(args) == 1 {
		if inner, ok := args["0"].(map[string]any); ok {
			args = inner
		}
	}
	return call.Name, canonicalArgs(args), nil
}

func literalArg(v any) any {
	switch x := v.(type) {
	case *languageParser.LiteralExpr:
		return literalArg(x.Value)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = literalArg(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = literalArg(e)
		}
		return out
	case languageParser.ExpressionNode:
		return map[string]any{"$node": fmt.Sprintf("%T", x)}
	}
	return v
}

// ---------------------------------------------------------------------------
// comparison
// ---------------------------------------------------------------------------

// canonicalResult is a result in the terms every runner shares. An engine
// result is its flat output or, without one, its rows; a node is its id,
// concept and payload; a probe's non-row answer carried as a node is the
// answer; everything else is its JSON value.
func canonicalResult(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case *memql.ExecuteResult:
		if x == nil {
			return nil
		}
		if out, ok := x.FlatOutput(); ok {
			return canonicalResult(out)
		}
		if x.Bundle != nil {
			return canonicalResult(x.Bundle)
		}
		return nil
	case *memqlv1.GraphBundle:
		rows := make([]any, 0, len(x.GetNodes()))
		for _, n := range x.GetNodes() {
			rows = append(rows, map[string]any{"id": n.GetId(), "concept": n.GetConcept(), "payload": n.GetPayload().AsMap()})
		}
		return canonicalJSON(rows)
	case map[string]memorynodes.MemoryNode:
		ids := make([]string, 0, len(x))
		for id := range x {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		nodes := make([]memorynodes.MemoryNode, 0, len(ids))
		for _, id := range ids {
			nodes = append(nodes, x[id])
		}
		return canonicalResult(nodes)
	case []memorynodes.MemoryNode:
		if len(x) == 1 && x[0].Concept == probeAnswerConcept {
			var answer any
			_ = json.Unmarshal(x[0].Payload, &answer)
			return canonicalJSON(answer)
		}
		rows := make([]any, 0, len(x))
		for _, n := range x {
			var payload any
			_ = json.Unmarshal(n.Payload, &payload)
			rows = append(rows, map[string]any{"id": n.ID, "concept": n.Concept, "payload": payload})
		}
		return canonicalJSON(rows)
	}
	return canonicalJSON(v)
}

// canonicalJSON is v's JSON value, with every map that is a row reduced to
// its id, concept and payload.
func canonicalJSON(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("<unmarshalable %T: %v>", v, err)
	}
	var out any
	_ = json.Unmarshal(raw, &out)
	return reduceRows(out)
}

func reduceRows(v any) any {
	switch x := v.(type) {
	case map[string]any:
		if _, hasID := x["id"]; hasID {
			if _, hasPayload := x["payload"]; hasPayload {
				return map[string]any{"id": x["id"], "concept": x["concept"], "payload": reduceRows(x["payload"])}
			}
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = reduceRows(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = reduceRows(e)
		}
		return out
	}
	return v
}

// canonicalArgs is an argument map as JSON values.
func canonicalArgs(args map[string]any) map[string]any {
	if args == nil {
		return nil
	}
	out, _ := canonicalJSON(args).(map[string]any)
	return out
}

// logicTimestamp matches a rendered instant, which two runs a moment apart
// legitimately disagree on.
var logicTimestamp = regexp.MustCompile(`20[0-9]{2}-[0-9]{2}-[0-9]{2}T[0-9:.]+Z`)

// logicRecord is one arm's run in comparable form: whether it was refused,
// what it returned, and the calls it made.
type logicRecord struct {
	Refused bool        `json:"refused,omitempty"`
	Result  any         `json:"result"`
	Calls   []probeCall `json:"calls"`
}

func newLogicRecord(result any, err error, calls []probeCall) logicRecord {
	rec := logicRecord{Refused: err != nil, Calls: calls}
	if err == nil {
		rec.Result = canonicalResult(result)
	}
	return rec
}

// logicInstantMask is what an instant reads as in a compared record and in
// a golden.
const logicInstantMask = "<ts>"

// outcome is the record as the comparison reads it and a golden stores it:
// its JSON value, instants masked, every map's keys sorted.
func (r logicRecord) outcome() any {
	raw, _ := json.Marshal(r)
	var out any
	_ = json.Unmarshal([]byte(logicTimestamp.ReplaceAllString(string(raw), logicInstantMask)), &out)
	return out
}

// String is outcome as comparable text.
func (r logicRecord) String() string {
	raw, _ := json.Marshal(r.outcome())
	return string(raw)
}

// recordOf reads a golden outcome back as a record, for a mend to edit.
func recordOf(outcome any) logicRecord {
	raw, _ := json.Marshal(outcome)
	var out logicRecord
	_ = json.Unmarshal(raw, &out)
	return out
}

// clone is a deep copy, for a mend to edit.
func (r logicRecord) clone() logicRecord {
	raw, _ := json.Marshal(r)
	var out logicRecord
	_ = json.Unmarshal(raw, &out)
	return out
}

// ---------------------------------------------------------------------------
// legacy defects
// ---------------------------------------------------------------------------

// legacyDefect is a run in which the LEGACY build does not do what the body
// says and the v1 build does. mend checks both halves of that -- the defect in
// the legacy record, the value the body means in the other arm's -- and then
// removes the defective part from both, so everything else the run did is
// still held to equality. A mend that finds no defect fails the gate: the
// entry would otherwise outlive the defect it names.
type legacyDefect struct {
	why     string
	applies func(fx logicFixture) bool
	mend    func(fx logicFixture, legacy, other *logicRecord) error
}

// logicLegacyDefects are the legacy defects the migration corrects, by
// construct. Each was read off the legacy compiled body, not inferred from
// the difference.
var logicLegacyDefects = map[string]legacyDefect{
	"purgeExpiredSafetyClassifications": retentionObservation("safety.classification.retention.observed", "90"),
	"purgeExpiredOutputScreenings":      retentionObservation("safety.outputScreening.retention.observed", "90"),
	"auditEventRetentionSweep":          retentionObservation("identity.audit.retention.observed", "365"),
	"onDelegationCreated": {
		why:     "`now` in the event payload compiles to the text `timestamp()`, which the event step publishes as text",
		applies: func(logicFixture) bool { return true },
		mend: func(_ logicFixture, legacy, other *logicRecord) error {
			if err := mendEventField(legacy, other, "delegation.created", "timestamp", isInstant); err != nil {
				return err
			}
			// The logic returns the event step's result, which carries the
			// same payload.
			return mendResultPath(legacy, other, []string{"payload", "timestamp"}, isInstant)
		},
	},
	"accountDeletionReminder7Days":  deletionReminder(7, 1),
	"accountDeletionReminder25Days": deletionReminder(25, 2),
	"governanceCanManagePrincipal":  governanceDefect("rbacGovernPrincipal", []string{"actorUserId", "actorIsOwner", "actorRank", "targetRank"}, "targetRoleSlug"),
	"governanceCanCreatePrincipal":  governanceDefect("rbacCanCreatePrincipal", []string{"actorIsOwner", "actorRank", "newRank"}, "newRoleSlug"),
	"conflictDetection": {
		why: "the condition `! matchingConfirmed.empty()` evaluates false with matches in hand, so the conflict event is never published " +
			"(and its payload's `matchingConfirmed.count()`, `.nodes()` and `now` compile to text)",
		applies: func(fx logicFixture) bool { _, hasEvent := fx.Args["event"]; return hasEvent && fx.Rows > 0 },
		mend: func(fx logicFixture, legacy, other *logicRecord) error {
			if eventCall(legacy, "data.conflicts.detected") != nil || legacy.Result != nil {
				return fmt.Errorf("the legacy run published the conflict event")
			}
			ev := eventCall(other, "data.conflicts.detected")
			if ev == nil {
				return fmt.Errorf("the v1 run did not publish the conflict event")
			}
			if n, _ := ev.Args["matchCount"].(float64); int(n) != fx.Rows {
				return fmt.Errorf("the v1 event counts %v matches, want %d", ev.Args["matchCount"], fx.Rows)
			}
			if matches, _ := ev.Args["matches"].([]any); len(matches) != fx.Rows {
				return fmt.Errorf("the v1 event carries %d matches, want %d", len(matches), fx.Rows)
			}
			other.Calls = withoutEvent(other.Calls, "data.conflicts.detected")
			other.Result = nil
			return nil
		},
	},
	"transitionEventKind": {
		why: "the legacy compiler turns `old == st` into `old == \"st\"`, a comparison with the literal text, so an unchanged status reads as a transition",
		applies: func(fx logicFixture) bool {
			st, _ := fx.Args["status"].(string)
			return st != "" && fx.Args["oldStatus"] == st
		},
		mend: func(_ logicFixture, legacy, other *logicRecord) error {
			if s, _ := legacy.Result.(string); s == "" {
				return fmt.Errorf("the legacy run returned %v for an unchanged status", legacy.Result)
			}
			if other.Result != "" {
				return fmt.Errorf("the v1 run returned %v for an unchanged status, want \"\"", other.Result)
			}
			legacy.Result = ""
			return nil
		},
	},
}

// retentionObservation is the defect of the three retention sweeps: the
// observation event's payload expressions compile to strings, which the
// event step publishes as their text.
func retentionObservation(topic, defaultDays string) legacyDefect {
	return legacyDefect{
		why: "the event payload's expressions -- `rows.count()`, `retentionDays.first().payload.value ?? \"" + defaultDays +
			"\"` and `now` -- compile to strings the event step publishes as text",
		applies: func(logicFixture) bool { return true },
		mend: func(fx logicFixture, legacy, other *logicRecord) error {
			days := defaultDays
			if fx.Rows > 0 {
				days = "7" // the probe's globalVariable row
			}
			for field, want := range map[string]func(any) bool{
				"candidateCount": func(v any) bool { n, ok := v.(float64); return ok && int(n) == fx.Rows },
				"retentionDays":  func(v any) bool { return v == days },
				"timestamp":      isInstant,
			} {
				if err := mendEventField(legacy, other, topic, field, want); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// deletionReminder is the defect of the two deletion-reminder sweeps: the
// reminder window -- `addDuration(item.payload.deletionScheduledAt, "PnD") <
// now && ...` -- compiles to a condition the string evaluator reads as false,
// so no reminder is ever published. The probe's rows sit one inside each
// window: row 1 half a day into the 7-day one, row 2 into the 25-day one.
func deletionReminder(days, row int) legacyDefect {
	return legacyDefect{
		why: fmt.Sprintf("the %d-day window condition compiles to one the string evaluator reads as false, so no reminder is published "+
			"(and the payload's `now` compiles to the text `timestamp()`)", days),
		applies: func(fx logicFixture) bool { return fx.Rows >= row },
		mend: func(_ logicFixture, legacy, other *logicRecord) error {
			const topic = "identity.deletion.reminder"
			if eventCall(legacy, topic) != nil {
				return fmt.Errorf("the legacy run published a reminder")
			}
			var reminders []probeCall
			for _, c := range other.Calls {
				if c.Kind == "event" && c.Name == topic {
					reminders = append(reminders, c)
				}
			}
			if len(reminders) != 1 {
				return fmt.Errorf("the v1 run published %d reminders, want the one row inside the window", len(reminders))
			}
			got := reminders[0].Args
			if want := fmt.Sprintf("v1:probe:row:usersInDeletionCooldown-%d", row); got["userId"] != want {
				return fmt.Errorf("the v1 reminder is for %v, want %s", got["userId"], want)
			}
			if n, _ := got["milestoneDays"].(float64); int(n) != days || !isInstant(got["timestamp"]) {
				return fmt.Errorf("the v1 reminder is %v", got)
			}
			other.Calls = withoutEvent(other.Calls, topic)
			return nil
		},
	}
}

// governanceDefect is the defect of the two rbac governance logics: the
// actor's fields compile to `arg("actor.role")` / `arg("actor.isClusterOwner")`
// text, which the first statement passes to roleBySlug as a slug and the
// return passes to the governance builtin unevaluated -- so the Go core never
// sees the actor's rank or owner bit.
func governanceDefect(builtin string, actorFields []string, targetSlugArg string) legacyDefect {
	return legacyDefect{
		why: "`actor.role` and `actor.isClusterOwner` compile to `arg(...)` text: roleBySlug is asked for the slug `arg(\"actor.role\")` " +
			"and " + builtin + " receives the actor's fields unevaluated",
		applies: func(logicFixture) bool { return true },
		mend: func(fx logicFixture, legacy, other *logicRecord) error {
			if len(legacy.Calls) == 0 || len(other.Calls) == 0 || legacy.Calls[0].Name != "roleBySlug" || other.Calls[0].Name != "roleBySlug" {
				return fmt.Errorf("the first call is not the actor's roleBySlug")
			}
			if legacy.Calls[0].Args["slug"] != `arg("actor.role")` {
				return fmt.Errorf("the legacy actor slug is %v", legacy.Calls[0].Args["slug"])
			}
			if other.Calls[0].Args["slug"] != fx.Role {
				return fmt.Errorf("the v1 actor slug is %v, want %q", other.Calls[0].Args["slug"], fx.Role)
			}
			legacy.Calls[0].Args["slug"], other.Calls[0].Args["slug"] = "<actor role>", "<actor role>"

			target, _ := fx.Args[targetSlugArg].(string)
			want := map[string]any{
				"actorUserId":  "user-corpus",
				"actorIsOwner": fx.Role == "owner",
			}
			if fx.Rows > 0 {
				rank := func(slug string) any {
					return probeRowPayload(time.Time{}, "", 1, map[string]any{"slug": slug})["rank"]
				}
				want["actorRank"] = float64(rank(fx.Role).(int))
				want["targetRank"] = float64(rank(target).(int))
				want["newRank"] = float64(rank(target).(int))
			}
			for _, rec := range []*logicRecord{legacy, other} {
				call := lastCall(rec, builtin)
				if call == nil {
					return fmt.Errorf("no %s call", builtin)
				}
				for _, f := range actorFields {
					v, present := call.Args[f]
					if rec == legacy {
						// Unevaluated: the engine reads the text back as an
						// expression, never as a value.
						if _, isNode := v.(map[string]any)["$node"]; present && !isNode {
							return fmt.Errorf("the legacy %s %s is the value %v", builtin, f, v)
						}
					} else {
						w, wanted := want[f]
						if wanted != present || (present && w != v) {
							return fmt.Errorf("the v1 %s %s is %v (present %v), want %v (present %v)", builtin, f, v, present, w, wanted)
						}
					}
					delete(call.Args, f)
					if args, ok := rec.Result.(map[string]any)["args"].(map[string]any); ok {
						delete(args, f)
					}
				}
			}
			return nil
		},
	}
}

// isInstant reports whether v is an instant -- or the mask a golden stores
// one as.
func isInstant(v any) bool {
	s, ok := v.(string)
	return ok && (s == logicInstantMask || (logicTimestamp.MatchString(s) && logicTimestamp.FindString(s) == s))
}

func eventCall(rec *logicRecord, topic string) *probeCall {
	for i := range rec.Calls {
		if rec.Calls[i].Kind == "event" && rec.Calls[i].Name == topic {
			return &rec.Calls[i]
		}
	}
	return nil
}

func lastCall(rec *logicRecord, name string) *probeCall {
	for i := len(rec.Calls) - 1; i >= 0; i-- {
		if rec.Calls[i].Name == name {
			return &rec.Calls[i]
		}
	}
	return nil
}

func withoutEvent(calls []probeCall, topic string) []probeCall {
	out := make([]probeCall, 0, len(calls))
	for _, c := range calls {
		if c.Kind == "event" && c.Name == topic {
			continue
		}
		out = append(out, c)
	}
	return out
}

// mendEventField checks one field of a published event's payload: the
// legacy value is expression text, the other arm's satisfies want. It then
// removes the field from both.
func mendEventField(legacy, other *logicRecord, topic, field string, want func(any) bool) error {
	lev, oev := eventCall(legacy, topic), eventCall(other, topic)
	if lev == nil || oev == nil {
		return fmt.Errorf("an arm did not publish %s", topic)
	}
	if s, _ := lev.Args[field].(string); !strings.Contains(s, "(") {
		return fmt.Errorf("the legacy %s.%s is %v, not expression text", topic, field, lev.Args[field])
	}
	if !want(oev.Args[field]) {
		return fmt.Errorf("the v1 %s.%s is %v", topic, field, oev.Args[field])
	}
	delete(lev.Args, field)
	delete(oev.Args, field)
	return nil
}

// mendResultPath checks a value under a result map the same way.
func mendResultPath(legacy, other *logicRecord, path []string, want func(any) bool) error {
	at := func(rec *logicRecord) (map[string]any, bool) {
		m, ok := rec.Result.(map[string]any)
		for _, p := range path[:len(path)-1] {
			if !ok {
				return nil, false
			}
			m, ok = m[p].(map[string]any)
		}
		return m, ok
	}
	lm, lok := at(legacy)
	om, ook := at(other)
	leaf := path[len(path)-1]
	if !lok || !ook {
		return fmt.Errorf("a result has no %s", strings.Join(path, "."))
	}
	if s, _ := lm[leaf].(string); !strings.Contains(s, "(") {
		return fmt.Errorf("the legacy result %s is %v, not expression text", strings.Join(path, "."), lm[leaf])
	}
	if !want(om[leaf]) {
		return fmt.Errorf("the v1 result %s is %v", strings.Join(path, "."), om[leaf])
	}
	delete(lm, leaf)
	delete(om, leaf)
	return nil
}

// ---------------------------------------------------------------------------
// goldens
// ---------------------------------------------------------------------------

// updateLogicGoldens rewrites the goldens from the runs, once every arm
// agrees with every other.
var updateLogicGoldens = flag.Bool("update", false, "rewrite "+logicGoldenDir+" from TestLogicCorpusRuns' runs; every arm must agree")

// logicGoldenDir holds one golden file per logic construct,
// <domain>/<name>.json: its construct, and its runs by run key, each with
// the run's input, its outcome as the comparison reads it (logicRecord.outcome)
// and, where the legacy build does not do what the body says, the defect.
const logicGoldenDir = "testdata/logic_corpus"

// goldenFileFor is the golden file of a fixture's construct.
func goldenFileFor(fx logicFixture) string {
	domain, _, _ := strings.Cut(fx.File.Path, "/")
	return filepath.Join(logicGoldenDir, domain, fx.Name+".json")
}

// goldenRunKey names one run within its construct's golden file.
func goldenRunKey(fx logicFixture) string {
	return fmt.Sprintf("%s | actor %s | %d rows", fx.Label, fx.Role, fx.Rows)
}

// goldenInput is what a run is called with, as a golden stores it.
func goldenInput(fx logicFixture) any {
	return jsonValue(map[string]any{"args": fx.Args, "actor": fx.Role, "rows": fx.Rows})
}

// jsonValue is v's JSON value; jsonText is its text, every map's keys sorted.
func jsonValue(v any) any {
	raw, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(raw, &out)
	return out
}

func jsonText(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

// logicGoldenRuns is the golden set: file -> run key -> run.
type logicGoldenRuns map[string]map[string]map[string]any

// readLogicGoldens reads every golden file. The layout is fixed --
// <domain>/<name>.json -- so it is read a directory at a time.
func readLogicGoldens() (logicGoldenRuns, error) {
	out := logicGoldenRuns{}
	domains, err := os.ReadDir(logicGoldenDir)
	if err != nil {
		return nil, err
	}
	for _, d := range domains {
		if !d.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(logicGoldenDir, d.Name()))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
				continue
			}
			path := filepath.Join(logicGoldenDir, d.Name(), f.Name())
			raw, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			var file struct {
				Runs map[string]map[string]any `json:"runs"`
			}
			if err := json.Unmarshal(raw, &file); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			out[path] = file.Runs
		}
	}
	return out, nil
}

// goldenFile is one construct's golden file as -update writes it.
type goldenFile struct {
	construct string
	runs      map[string]any
}

// writeLogicGoldens replaces the golden set with files: indented, keys
// sorted, `<` and `>` as themselves, so a change reads as a line diff.
func writeLogicGoldens(files map[string]*goldenFile) error {
	if err := os.RemoveAll(logicGoldenDir); err != nil {
		return err
	}
	for path, gf := range files {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]any{"construct": gf.construct, "runs": gf.runs}); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// the gate
// ---------------------------------------------------------------------------

// compareRuns compares two runs of one fixture. When exactly one of them is
// the legacy arm's and the construct has a legacy defect that applies, the
// defect is checked and mended first (mended reports it).
func compareRuns(fx logicFixture, aLegacy bool, a logicRecord, bLegacy bool, b logicRecord) (mended bool, err error) {
	a, b = a.clone(), b.clone()
	if d, ok := logicLegacyDefects[fx.Name]; ok && aLegacy != bLegacy && d.applies(fx) {
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

// TestLogicCorpusRuns: every logic construct of the tree, run by every arm
// against every fixture, returns what its golden holds and makes the calls
// the golden holds, in their order -- and every arm agrees with every other,
// the legacy arm where it does not do what the body says excepted
// (logicLegacyDefects). With -update it writes the goldens from the runs
// instead, once they agree.
func TestLogicCorpusRuns(t *testing.T) {
	sources := logicCorpusSources(t)
	// The first arm is the one the others are compared with. The flip deletes
	// the legacy arm's line: the tree then has no legacy source, and the v1
	// arm against the goldens is the whole test. The bodies arm runs the
	// statement form of every body (epic memql#5370) until the bodies flip
	// makes it the v1 arm's own.
	arms := []logicArm{
		todayArm(t, "legacy", false, sources),
		todayArm(t, "v1", true, sources),
		bodiesArm(t, sources),
	}
	legacyRuns := false
	golden := -1 // the arm an -update writes the goldens from
	for i, arm := range arms {
		legacyRuns = legacyRuns || arm.legacy
		if golden < 0 && !arm.legacy {
			golden = i
		}
	}

	fixtures := logicFixtures(t, sources)
	require.Greater(t, len(fixtures), 300, "dozens of logic constructs times their variants; a small count means the fixtures went blind")

	update := *updateLogicGoldens
	goldens, readErr := readLogicGoldens()
	switch {
	case update:
		require.GreaterOrEqualf(t, golden, 0, "-update writes what the bodies say, which no legacy arm is: add an arm that is not legacy")
	default:
		require.NoErrorf(t, readErr, "the logic corpus has no goldens to compare with in %s -- write them with `go test -run TestLogicCorpusRuns -update`", logicGoldenDir)
	}

	now := time.Now().UTC()
	written := map[string]*goldenFile{}
	taken := map[string]bool{}
	mended := map[string]int{}
	constructs := map[string]bool{}
	moved := map[string]map[string]string{} // arm -> golden file -> the construct it has no run of
	matched, refused := 0, 0
	var diffs []string
	for _, fx := range fixtures {
		ctx := auth.ContextWithInternalOrigin(auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "user-corpus", Role: auth.Role(fx.Role)}))
		records := make([]logicRecord, len(arms))
		ran := make([]bool, len(arms))
		for i, arm := range arms {
			if arm.moved != nil && arm.moved(fx.File.Path, fx.Name) {
				if moved[arm.name] == nil {
					moved[arm.name] = map[string]string{}
				}
				moved[arm.name][goldenFileFor(fx)] = fx.Key
				continue
			}
			probe := &logicProbe{rows: fx.Rows, now: now}
			res, err := arm.run(ctx, arm.source(fx.File), fx.File.Path, fx.Name, fx.Args, probe)
			records[i], ran[i] = newLogicRecord(res, err, probe.calls), true
		}
		file, key := goldenFileFor(fx), goldenRunKey(fx)
		where := fmt.Sprintf("%s [%s]", fx.Key, key)
		constructs[fx.Key] = true

		agreed := true
		for i := 1; i < len(arms); i++ {
			if !ran[0] || !ran[i] {
				continue
			}
			m, err := compareRuns(fx, arms[0].legacy, records[0], arms[i].legacy, records[i])
			if m {
				mended[fx.Name]++
			}
			if err != nil {
				agreed = false
				diffs = append(diffs, fmt.Sprintf("%s: the %s and %s arms: %v\n    %s: %s\n    %s: %s",
					where, arms[0].name, arms[i].name, err, arms[0].name, records[0], arms[i].name, records[i]))
			}
		}

		if update {
			if !agreed {
				continue
			}
			entry := map[string]any{"input": goldenInput(fx), "outcome": records[golden].outcome()}
			if d, ok := logicLegacyDefects[fx.Name]; ok && d.applies(fx) && legacyRuns {
				entry["legacyDefect"] = d.why
			} else if prev, ok := goldens[file][key]; ok && !legacyRuns && jsonText(prev["outcome"]) == records[golden].String() && prev["legacyDefect"] != nil {
				// No legacy arm left to show the defect: an unchanged run keeps
				// the note an earlier -update wrote.
				entry["legacyDefect"] = prev["legacyDefect"]
			}
			gf := written[file]
			if gf == nil {
				gf = &goldenFile{construct: fx.Key, runs: map[string]any{}}
				written[file] = gf
			}
			if _, dup := gf.runs[key]; dup {
				diffs = append(diffs, where+": two fixtures share one golden run key")
			}
			gf.runs[key] = entry
			if records[golden].Refused {
				refused++
			}
			continue
		}

		entry, ok := goldens[file][key]
		if !ok {
			diffs = append(diffs, where+": no golden run -- if the fixtures changed on purpose, rewrite the goldens with -update")
			continue
		}
		taken[file+"\x00"+key] = true
		if got, want := jsonText(goldenInput(fx)), jsonText(entry["input"]); got != want {
			diffs = append(diffs, fmt.Sprintf("%s: the fixture is not the golden's input\n    golden:  %s\n    fixture: %s", where, want, got))
			continue
		}
		want := recordOf(entry["outcome"])
		if want.Refused {
			refused++
		}
		for i, arm := range arms {
			if !ran[i] {
				continue
			}
			m, err := compareRuns(fx, arm.legacy, records[i], false, want)
			if m {
				mended[fx.Name]++
			}
			if err != nil {
				diffs = append(diffs, fmt.Sprintf("%s: the %s arm and the golden: %v\n    golden: %s\n    %s: %s",
					where, arm.name, err, want, arm.name, records[i]))
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
		// An arm skips only a logic the bodies rewrite moved into its
		// automation, and the rewrite moves only a logic that publishes: its
		// goldens publish in some run.
		for arm, files := range moved {
			for file, k := range files {
				if !goldensPublish(goldens[file]) {
					diffs = append(diffs, fmt.Sprintf("%s: the %s arm has no run of it, and no golden run of it publishes: the rewrite moved a logic that may stay one", k, arm))
				}
			}
		}
	}
	sort.Strings(diffs)
	require.Emptyf(t, diffs, "%d runs differ:\n%s", len(diffs), strings.Join(diffs, "\n"))
	if legacyRuns {
		for name, d := range logicLegacyDefects {
			require.Positivef(t, mended[name], "the legacy defect of %s (%s) applies to no run: the entry is stale", name, d.why)
		}
	}
	require.Less(t, refused, len(fixtures)/2, "most runs must return a result; a majority of refusals means the fixtures do not reach the bodies")

	if update {
		require.NoError(t, writeLogicGoldens(written))
		t.Logf("wrote %d golden runs over %d logic constructs to %s (%d refused, %d legacy-defect mends)", len(fixtures), len(written), logicGoldenDir, refused, sumInts(mended))
		return
	}
	t.Logf("%d arm runs match the goldens over %d logic constructs and %d fixtures (%d refused, %d legacy-defect mends)", matched, len(constructs), len(fixtures), refused, sumInts(mended))
	for _, arm := range arms {
		if files := moved[arm.name]; len(files) > 0 {
			keys := make([]string, 0, len(files))
			for _, k := range files {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			t.Logf("the %s arm has no run of %d logic the bodies rewrite moved into their automations: %s", arm.name, len(keys), strings.Join(keys, ", "))
		}
	}
}

// goldensPublish reports whether one construct's golden runs publish an event
// in any run.
func goldensPublish(runs map[string]map[string]any) bool {
	for _, run := range runs {
		for _, c := range recordOf(run["outcome"]).Calls {
			if c.Kind == "event" {
				return true
			}
		}
	}
	return false
}

func sumInts(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// logicFixtures is every run of the corpus: each logic construct of the tree,
// under its argument variants, every actor and both row counts.
func logicFixtures(t *testing.T, sources []*corpusSource) []logicFixture {
	t.Helper()
	var out []logicFixture
	for _, f := range sources {
		for _, slice := range memql.ExtractFunctionSlices(f.Current) {
			if slice.Kind != languageParser.FunctionTypeLogic {
				continue
			}
			// The tree's own build, in the grammar the tree is written in:
			// only its args block is read, which both grammars read alike.
			fn, err := memql.BuildFunctionConstruct(f.Current, slice.Name, "unified:"+f.Path, memorynodes.DefaultRegistry())
			require.NoError(t, err)
			variants := logicSchemaArgs(fn)
			labels := make([]string, len(variants))
			for i := range variants {
				labels[i] = fmt.Sprintf("schema args %d", i)
			}
			for i, v := range logicArgVariants[slice.Name] {
				variants = append(variants, v)
				labels = append(labels, fmt.Sprintf("variant %d", i))
			}
			for i, args := range variants {
				for _, role := range logicRoles {
					for _, rows := range []int{2, 0} {
						out = append(out, logicFixture{
							Key: f.Path + " " + slice.Name, File: f, Name: slice.Name,
							Args: args, Role: role, Rows: rows, Label: labels[i],
						})
					}
				}
			}
		}
	}
	return out
}
