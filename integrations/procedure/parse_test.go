package procedure

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	concept "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/ast"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/work"
)

// parse_test.go -- every statement this package hands the engine, run through
// the REAL MemQL front end (no database).
//
// A recording fake accepts any string, so the whole package can be green
// while nothing is written: the object-literal wrapper the parser refuses,
// Go's %q escapes, a map rendered as its Go spelling -- each has shipped in
// this tree before with every test passing. So the drivers below run the
// PRODUCTION paths against the recording engine and parse whatever those
// paths actually produced, twice over: the syntax (ParseExpression) and the
// resolution (a DSL-loaded engine's Parse, which checks that the construct
// exists and that its arguments are declared).

// parseCallArgs parses one rendered call and returns its arguments, numbers as
// float64 and nil as nil -- the shapes a decoded JSON document has, so a test
// can compare what went in with what the parser read back.
func parseCallArgs(t *testing.T, stmt string) map[string]any {
	t.Helper()
	call := strings.TrimSpace(stmt)
	for _, kind := range []string{"mutation ", "query ", "builtin "} {
		call = strings.TrimPrefix(call, kind)
	}
	expr, err := langparser.ParseExpression(call)
	if err != nil {
		t.Fatalf("the real parser REFUSED a call this package renders:\n\t%s\n%v", truncateForLog(stmt), err)
	}
	fn, ok := expr.(*langparser.FunctionCallExpr)
	if !ok {
		t.Fatalf("call parsed as %T, not a function call: %s", expr, truncateForLog(stmt))
	}
	out, _ := normalizeParsed(fn.Args).(map[string]any)
	if out == nil {
		out = map[string]any{}
	}
	return out
}

func normalizeParsed(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = normalizeParsed(e)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = normalizeParsed(e)
		}
		return out
	case int64:
		return float64(t)
	case int:
		return float64(t)
	case *ast.NilExpr:
		return nil
	}
	return v
}

func truncateForLog(s string) string {
	if len(s) > 400 {
		return s[:400] + "..."
	}
	return s
}

// realDSLEngine loads the embedded DSL tree and initialises an engine with no
// database, which is all parse and resolve need. Built per test: a package
// fixture that mutates the concept registry leaks into every later test.
func realDSLEngine(t *testing.T) *memqlengine.MemQLEngine {
	t.Helper()
	if _, err := memqlengine.LoadUnifiedConcepts(nil); err != nil {
		t.Fatalf("LoadUnifiedConcepts (the dsl/ tree): %v", err)
	}
	eng, err := memqlengine.New(nil)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	eng.Logger = slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	if err := eng.Init(concept.DefaultRegistry()); err != nil {
		t.Fatalf("engine init: %v", err)
	}
	return eng
}

// everyRenderedStatement drives every production path that renders a
// statement and returns what they actually handed the engine.
func everyRenderedStatement(t *testing.T) []recordedCall {
	t.Helper()
	var out []recordedCall
	collect := func(eng *fakeEngine) {
		out = append(out, eng.recorded()...)
	}

	// A first lift through the completion trigger -- the path the automation
	// takes: it borrows the run's owner and reads the run through the owned
	// read.
	eng := newFakeEngine()
	seedCorpus(t, eng, twoRecordings()...)
	eng.reply("workRunForOwner", twoRecordings()[1].runRow())
	i := newTestIntegration(eng)
	i.SetCompiler(&passingGate{})
	if _, err := i.handleLearnFromRun(triggerCtx(), map[string]any{"runId": twoRecordings()[1].runId, "ownerUserId": testOwner}, 0); err != nil {
		t.Fatalf("learnFromRun: %v", err)
	}
	collect(eng)
	maintenance := auth.ContextWithAccess(context.Background(), auth.MaintenanceActor("mineProcedureCorpusAcrossAutomations"))

	// A re-lift of a changed procedure, a candidate re-entering, and the
	// person's own read path.
	stored := storedConstruct(t, eng, "shadow")
	relift := newFakeEngine()
	seedCorpus(t, relift, twoRecordings()...)
	stored["procedureHash"] = "sha256:an-older-version"
	relift.reply("procedureConstructByName", stored)
	relift.reply("workRunForOwner", twoRecordings()[0].runRow())
	j := newTestIntegration(relift)
	j.SetCompiler(&passingGate{})
	if _, err := j.learnFromRun(auth.ContextWithUserActor(context.Background(), testOwner), twoRecordings()[0].runId, LevelAction); err != nil {
		t.Fatalf("learnFromRun as the owner: %v", err)
	}
	collect(relift)

	// The sweep over every owner.
	sweep := newFakeEngine()
	seedCorpus(t, sweep, twoRecordings()...)
	sweep.reply("usersForSeedSweep", map[string]any{"id": testOwner})
	sweep.reply("workRunsForOwner", twoRecordings()[0].runRow())
	k := newTestIntegration(sweep)
	k.SetCompiler(&passingGate{})
	if _, err := k.handleMineCorpus(maintenance, map[string]any{"ownerUserId": ""}, 0); err != nil {
		t.Fatalf("mineCorpus sweep: %v", err)
	}
	collect(sweep)
	return out
}

// TestEveryStatementTheLiftRendersParses is the direct guard for the class of
// defect a recording fake cannot see: every call a lift renders -- reads,
// @serverOnly writes, the procedure payload as an object of objects with
// dotted keys -- must be one the real parser accepts AND one a DSL-loaded
// engine resolves to a declared construct with declared arguments.
//
// And the property the stamp rule turns on is checked from the REGISTRY, not
// restated: every construct the loaded tree declares @serverOnly must have been
// called under internal origin, or it is refused in production with one WARN
// and the row is never written.
func TestEveryStatementTheLiftRendersParses(t *testing.T) {
	assertStatementsParseAndResolve(t, everyRenderedStatement(t), []string{
		"workRunForOwner", "workRunsForOwnerGoalSignature", "workStepsForOwnerRun",
		"workObservationsForOwnerRun", "libraryFileById", "workGoalForOwner", "procedureConstructByName",
		"createAuthoringBundle", "createAuthoringConstruct", "recordProcedure", "recordBundleValidation",
		"recordConstructLadder", "recordConstructGoalSignature", "usersForSeedSweep", "workRunsForOwner",
	})
}

// assertStatementsParseAndResolve holds every recorded call to the real front
// end and the loaded registry, and every @serverOnly one to internal origin.
// want names the constructs the drivers must have rendered, so a driver that
// silently stopped reaching one cannot turn the guard vacuous.
func assertStatementsParseAndResolve(t *testing.T, calls []recordedCall, want []string) {
	t.Helper()
	seen := map[string]bool{}
	for _, c := range calls {
		seen[c.Name()] = true
	}
	for _, name := range want {
		if !seen[name] {
			t.Errorf("no driver rendered %s, so nothing here parses it", name)
		}
	}

	eng := realDSLEngine(t)
	serverOnly := 0
	for _, c := range calls {
		parseCallArgs(t, c.Query)
		fn, err := eng.Functions().Get(c.Name())
		if err != nil || fn == nil {
			t.Errorf("%s is not in the function registry: %v -- a call to a construct no .memql file declares fails at EXECUTE", c.Name(), err)
			continue
		}
		if fn.ServerOnly {
			serverOnly++
			if !c.Internal {
				t.Errorf("%s is @serverOnly and was called WITHOUT internal origin; in production it is refused with one WARN: %s",
					c.Name(), truncateForLog(c.Query))
			}
		}
		_, perr := eng.Parse(c.Query)
		if perr == nil {
			continue
		}
		// Parse is ctx-free and defaults to CLIENT origin, so a @serverOnly
		// READ is refused here while working in production behind the stamp.
		// The assertion inverts for exactly that refusal: the construct must
		// genuinely be server-only, so the arm cannot hide a different fault.
		if strings.Contains(perr.Error(), "is server-only and cannot be called by a client") && fn.ServerOnly {
			continue
		}
		t.Errorf("the engine refused a statement this package renders:\n  %s\n  --> %v", truncateForLog(c.Query), perr)
	}
	if serverOnly == 0 {
		t.Fatal("no @serverOnly construct was called, so the stamp assertion above checked nothing")
	}
}

// TestLiteralRendersAMapAsAnObject is gap G9. The renderer fell through to a
// QUOTED STRING for a map, so the Gate 1 report went in as the text
// "map[gate1Ran:true ...]" where the concept declares an object. A map must
// render as an object literal, recursively, keys sorted -- and a key that is
// not a plain name (a dotted hole id, a hyphenated tool, a keyword) must come
// back as the same key. The whole value is read back through the real parser
// and compared, because a literal that merely parses can still say something
// else.
func TestLiteralRendersAMapAsAnObject(t *testing.T) {
	in := map[string]any{
		"gate1Ran":       true,
		"count":          float64(3),
		"exitCode":       float64(-1),
		"ratio":          0.25,
		"s0.command.3":   "a hole id is a key",
		"docker-compose": "2.27.0",
		"if":             "a keyword is a key",
		"nested":         map[string]any{"list": []any{float64(1), "two", map[string]any{"deep": false}}},
		"empty":          map[string]any{},
		"awkward":        "O'Brien \"quoted\" <tag> back\\slash tab\there\nnewline",
		"strings":        []string{"a", "b"},
		"dropped":        nil,
	}
	rendered := "mutation x(v: " + literal(in) + ")"
	if !strings.HasPrefix(literal(in), "{") {
		t.Fatalf("a map rendered as %s, not an object literal", literal(in))
	}
	got := parseCallArgs(t, rendered)["v"]
	want := map[string]any{}
	for k, v := range in {
		if v == nil {
			continue // a nil is dropped, never rendered
		}
		want[k] = v
	}
	want["strings"] = []any{"a", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the parser read back\n  %#v\nfrom %s,\nwant\n  %#v", got, rendered, want)
	}
	if !strings.Contains(literal(map[string]any{"b": 1, "a": 2}), "a: 2, b: 1") {
		t.Errorf("keys must be sorted: %s", literal(map[string]any{"b": 1, "a": 2}))
	}
	// A TYPED nil map is "said nothing", like an untyped nil: dropped from a
	// call, never rendered as an empty object that would overwrite a field.
	var none map[string]any
	if c := call("recordConstructLadder", map[string]any{"constructId": "c", "distinctBindings": none}); strings.Contains(c, "distinctBindings") {
		t.Errorf("a typed nil map was rendered: %s", c)
	}
	// And an EMPTY map is a value: it is how a cleared streak is written.
	if c := call("recordConstructLadder", map[string]any{"constructId": "c", "distinctBindings": map[string]any{}}); !strings.Contains(c, "distinctBindings: {}") {
		t.Errorf("an empty map must render as {}: %s", c)
	}
}

// TestRecordBundleValidationCallTextParses: the call the G9 defect broke,
// driven through the production writer and read back through the parser. Its
// validationReport must arrive as an OBJECT carrying the gate's verdict.
func TestRecordBundleValidationCallTextParses(t *testing.T) {
	eng := newFakeEngine()
	i := newTestIntegration(eng)
	if err := i.recordValidation(context.Background(), "b1", failingGate{}.CompileBundle(
		[]memqlengine.SandboxConstruct{{Kind: "automation", Name: "p"}}), true, false); err != nil {
		t.Fatalf("recordValidation: %v", err)
	}
	args := argsOf(t, eng.callTo(t, "recordBundleValidation"))
	report, ok := args["validationReport"].(map[string]any)
	if !ok {
		t.Fatalf("validationReport arrived as %T (%v), want an object", args["validationReport"], args["validationReport"])
	}
	if report["gate1Ran"] != true || report["reRunnable"] != false || report["ok"] != false {
		t.Fatalf("validationReport = %v, want the gate's verdict", report)
	}
	if diags, _ := report["diagnostics"].([]any); len(diags) != 1 {
		t.Fatalf("diagnostics = %v, want the one failure", report["diagnostics"])
	}
	if args["status"] != "failed" {
		t.Errorf("status = %v, want failed", args["status"])
	}
}

// everyReplayStatement drives every production path epic memql#5408's runner
// adds -- serving a goal, a divergence handed to the app, a refused start,
// a shadow comparison that proposes the promotion, a resumed replay, the
// promotion decision and both sweeps -- and returns what they handed the
// engine. The rows these statements carry are the widest this package writes:
// outcomes holding lists of objects and prose with backticks and quotes,
// binding digests keyed by dotted hole ids, an approval's options and
// evidence.
func everyReplayStatement(t *testing.T) []recordedCall {
	t.Helper()
	var out []recordedCall

	// Served from the statement, then resumed-and-finished, then a
	// divergence handed to the app.
	served := newReplayWorld(t, "trusted")
	if _, err := served.i.handleProcedureReplay(inGoalRun(replayOwner), map[string]any{"constructId": served.constructId}, 0); err != nil {
		t.Fatalf("serve: %v", err)
	}
	if _, err := served.i.handleProcedureReplay(inGoalRun(replayOwner), map[string]any{"constructId": served.constructId}, 0); err != nil {
		t.Fatalf("serve again: %v", err)
	}
	out = append(out, served.eng.recorded()...)

	diverged := newReplayWorld(t, "trusted")
	diverged.d.alter["step1"] = func(r *DispatchResult) { r.Observation.Contents[0].Digest = strings.Repeat("0", 64) }
	diverged.serve(t, ReplayTrusted, map[string]any{"file": goalFile})
	out = append(out, diverged.eng.recorded()...)

	refused := newReplayWorld(t, "trusted")
	refused.p.observed.Tools["mkdir"] = "9.3"
	refused.serve(t, ReplayTrusted, map[string]any{"file": goalFile})
	out = append(out, refused.eng.recorded()...)

	// The stops the ladder does not count: a target that stopped answering,
	// a resume that found a step in flight on a machine, and a replay whose
	// version was re-lifted under it.
	unavailable := newReplayWorld(t, "trusted")
	unavailable.d.alter["step1"] = func(r *DispatchResult) {
		yes := true
		*r = DispatchResult{Observation: work.StepObservation{IsError: &yes}, Unavailable: true,
			Output: map[string]any{"errorCode": "worker_disconnected", "errorMessage": "the stream dropped"}}
	}
	unavailable.serve(t, ReplayTrusted, map[string]any{"file": goalFile})
	out = append(out, unavailable.eng.recorded()...)

	interrupted, _ := machineReplayWorld(t)
	interruptedAfterStep0(t, interrupted, servedReq(interrupted), "running")
	if _, err := interrupted.i.Replay(context.Background(), servedReq(interrupted)); err != nil {
		t.Fatalf("resume: %v", err)
	}
	out = append(out, interrupted.eng.recorded()...)

	replaced := newReplayWorld(t, "trusted")
	replaced.d.alter["step0"] = func(*DispatchResult) { reliftTo(replaced, "shadow", "sha256:the-relifted-version") }
	replaced.serve(t, ReplayTrusted, map[string]any{"file": goalFile})
	out = append(out, replaced.eng.recorded()...)

	// Two shadow comparisons, the second proposing the promotion, through
	// ShadowCompare's own read of the recording.
	shadow := shadowWorld(t, recording1("c.txt", testNow.Add(-2*time.Minute)), recording1("d.txt", testNow.Add(-time.Minute)))
	shadow.policy(work.LadderPolicy{ShadowMatches: 2, DistinctBindings: 2, CanaryMatches: 1, FailuresToDemote: 2, InsufficientToDemote: 1, RetireAfterDays: 30})
	for _, runId := range []string{"v1:work:run:rec-c", "v1:work:run:rec-d"} {
		if _, err := shadow.i.ShadowCompare(personCtx(testOwner), runId); err != nil {
			t.Fatalf("ShadowCompare: %v", err)
		}
	}
	if len(shadow.eng.callsTo("createWorkApproval")) != 1 {
		t.Fatalf("the shadow driver raised %d approvals, want the one this guard parses", len(shadow.eng.callsTo("createWorkApproval")))
	}
	out = append(out, shadow.eng.recorded()...)

	decide := promotionWorld(t, "approved", "")
	decideAsCluster(t, decide)
	out = append(out, decide.eng.recorded()...)

	sweeps := newSweepWorld(t, 100)
	sweeps.rows[testOwner] = []map[string]any{
		procedureOn("over", "trusted", map[string]any{"failures": float64(3)}),
		procedureOn("stale", "shadow", map[string]any{"lastReplayAt": testNow.Add(-90 * 24 * time.Hour).Format(timeLayout)}),
	}
	sweepAsCluster(t, sweeps, SweepDemotion)
	sweepAsCluster(t, sweeps, SweepRetirement)
	out = append(out, sweeps.eng.recorded()...)
	return out
}

// TestEveryStatementTheReplayRendersParses is TestEveryStatementTheLiftRendersParses
// for the certification ladder's paths: the real parser, the loaded registry,
// and internal origin on every @serverOnly construct.
func TestEveryStatementTheReplayRendersParses(t *testing.T) {
	assertStatementsParseAndResolve(t, everyReplayStatement(t), []string{
		"authoringConstructById", "ladderPolicyCurrent", "workRunForOwner", "workGoalForOwner",
		"createWorkRun", "updateWorkRun", "createWorkStep", "updateWorkStep", "workStepsForOwnerRun",
		"recordConstructLadder", "recordConstructReliability", "createWorkApproval",
		"procedureConstructsForGoalSignature", "workObservationsForOwnerRun", "libraryFileById",
		"workApprovalById", "usersForSeedSweep", "learnedProceduresForOwner", "workDescriptionGuidance",
	})
}
