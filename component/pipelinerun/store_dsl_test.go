package pipelinerun

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/pipelines"
)

// store_dsl_test.go -- the production Store, against a RECORDING engine.
//
// A recording engine parses nothing and gates nothing, so three things are
// asserted here that it would otherwise wave through: every rendered call
// PARSES (through the real front end, with hostile values in every string
// position); every call names a construct and arguments dsl/pipelines
// actually declares (read from the .memql files themselves); and every call
// runs under the authority its kind requires. Whether the rows LAND is
// store_db_test.go's question.

type recordedCall struct {
	query  string
	origin auth.CallOrigin
	actor  *auth.AccessContext
	fresh  bool
}

type recordingEngine struct {
	mu      sync.Mutex
	calls   []recordedCall
	answers map[string]any // construct name -> OutputPayload
}

func newRecordingEngine() *recordingEngine { return &recordingEngine{answers: map[string]any{}} }

func (e *recordingEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	ac, _ := auth.AccessFromContext(ctx)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, recordedCall{query: query, origin: auth.OriginFromContext(ctx), actor: ac, fresh: memql.FreshReadFromContext(ctx)})
	if answer, ok := e.answers[constructOf(query)]; ok {
		return memql.NewResultWithOutput(answer), nil
	}
	return memql.NewResultWithOutput([]any{}), nil
}

func (e *recordingEngine) recorded() []recordedCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]recordedCall(nil), e.calls...)
}

// awkward goes in every string position: quotes, a backslash, a newline, a
// carriage return, a tab, a NUL, non-ASCII, and a brace that would end an
// object literal early.
const awkward = "quote\" back\\slash new\nline cr\r tab\t nul\x00 é {brace} [bracket] key: value"

// everyStoreCall drives every Store method once with values, returning a
// name per call for the failure message.
func everyStoreCall(s Store, value string) []struct {
	name string
	run  func() error
} {
	ctx := context.Background()
	now := time.Date(2026, 10, 3, 12, 0, 0, 123, time.UTC)
	p := Pipeline{
		ID: value, OwnerUserID: value, AccountID: value, PackageID: value, Name: value, Repository: value,
		DefaultBranch: value, InstallationID: 51234567, CredentialID: value, Delivery: value,
		Compute: pipelines.Compute(value), SecretNames: []string{value, "B"}, ChannelIDs: []string{value},
	}
	r := Run{
		ID: value, OwnerUserID: value, AccountID: value, PipelineID: value, Repository: value, SHA: value,
		Mode: pipelines.Mode(value), Event: pipelines.Event(value), RunKey: value, Attempt: 2, Trigger: value,
		RerunOf: value, DeliveryID: value, PullRequest: 42, HeadBranch: value, BaseSHA: value, Title: value,
		Version: value, Status: value, Conclusion: value, RefusalCode: value, RefusalMessage: value,
		RefusalScope: value, CheckRunID: 30431907812, CheckRunState: value,
		Notes: []Note{{Code: value, Message: value}}, QueuedAt: now, FinishedAt: now, DurationMs: 1234,
	}
	heads := map[string]string{"branch:" + value: value, "pr:12": value}
	// An exponent spelling, and a NaN no JSON can carry: the NaN is dropped
	// rather than refusing the whole write.
	timings := map[string]float64{value: 1.5, "example.test/a": 0.0000001, "b": 1e21, "nan": math.NaN()}
	stages := []StageSummary{{Name: value, Status: value, DurationMs: 9, Steps: 3, Failed: 1}}
	return []struct {
		name string
		run  func() error
	}{
		{"PackageForCaller", func() error { _, err := s.PackageForCaller(ctx, value); return err }},
		{"PipelineForOwner", func() error { _, err := s.PipelineForOwner(ctx, value); return err }},
		{"PipelineForPackage", func() error { _, err := s.PipelineForPackage(ctx, value); return err }},
		{"PipelinesForOwner", func() error { _, err := s.PipelinesForOwner(ctx); return err }},
		{"RunForOwner", func() error { _, err := s.RunForOwner(ctx, value); return err }},
		{"RunsForOwner (one pipeline)", func() error { _, err := s.RunsForOwner(ctx, value); return err }},
		{"RunsForOwner (every pipeline)", func() error { _, err := s.RunsForOwner(ctx, ""); return err }},
		{"InboundDelivery", func() error { _, err := s.InboundDelivery(ctx, value); return err }},
		{"PipelinesForRepository", func() error { _, err := s.PipelinesForRepository(ctx, value); return err }},
		{"PipelinesPolled", func() error { _, err := s.PipelinesPolled(ctx); return err }},
		{"PipelineByID", func() error { _, err := s.PipelineByID(ctx, value); return err }},
		{"PipelinesActive", func() error { _, err := s.PipelinesActive(ctx); return err }},
		{"WorkSteps", func() error { _, err := s.WorkSteps(ctx, value); return err }},
		{"RunsForKey", func() error { _, err := s.RunsForKey(ctx, value); return err }},
		{"RunsForPipelineSHA", func() error { _, err := s.RunsForPipelineSHA(ctx, value, value); return err }},
		{"RunByCheckRun", func() error { _, err := s.RunByCheckRun(ctx, value, 30431907812); return err }},
		{"RunsUnfinished", func() error { _, err := s.RunsUnfinished(ctx); return err }},
		{"RunsUnfinishedForPullRequest", func() error { _, err := s.RunsUnfinishedForPullRequest(ctx, value, 42); return err }},
		{"RunsFinalCheckRunUnavailable", func() error { _, err := s.RunsFinalCheckRunUnavailable(ctx, now); return err }},
		{"RunByID", func() error { _, err := s.RunByID(ctx, value); return err }},
		{"CreatePipeline", func() error { return s.CreatePipeline(ctx, p) }},
		{"UpdatePipeline (every field)", func() error {
			return s.UpdatePipeline(ctx, value, value, PipelinePatch{
				Name: ptr(value), DefaultBranch: ptr(value), Delivery: ptr(value), Compute: ptr(pipelines.Compute(value)),
				Status: ptr(value), SecretNames: ptr([]string{value}), ChannelIDs: ptr([]string{}),
				Heads: ptr(heads), Timings: ptr(timings), TimingsRunID: ptr(value), TimingsUpdatedAt: ptr(now),
			})
		}},
		{"UpdatePipeline (clearing)", func() error {
			return s.UpdatePipeline(ctx, value, value, PipelinePatch{Heads: ptr(map[string]string{}), SecretNames: ptr([]string(nil))})
		}},
		{"CreateRun", func() error { return s.CreateRun(ctx, r) }},
		{"UpdateRun (every field)", func() error {
			return s.UpdateRun(ctx, value, value, RunPatch{
				Status: ptr(value), Conclusion: ptr(value), RefusalCode: ptr(value), RefusalMessage: ptr(value),
				RefusalScope: ptr(value), CheckRunID: ptr(int64(30431907812)), CheckRunState: ptr(value),
				Notes: ptr([]Note{{Code: value, Message: value}}), WorkRunID: ptr(value), WorkGoalID: ptr(value),
				DriverNodeID: ptr(value), DriverHeartbeatAt: ptr(now), CancelRequested: ptr(true), CancelledBy: ptr(value),
				Stages: ptr(stages), StartedAt: ptr(now), FinishedAt: ptr(now), DurationMs: ptr(int64(5)),
			})
		}},
		{"UpdateRun (clearing)", func() error {
			return s.UpdateRun(ctx, value, value, RunPatch{
				Conclusion: ptr(""), CheckRunID: ptr(int64(0)), Notes: ptr([]Note{}), CancelRequested: ptr(false),
			})
		}},
	}
}

func TestEveryRenderedCallParses(t *testing.T) {
	engine := newRecordingEngine()
	store := NewDSLStore(engine)
	before := 0
	for _, c := range everyStoreCall(store, awkward) {
		if err := c.run(); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		calls := engine.recorded()
		if len(calls) == before {
			t.Fatalf("%s rendered no call -- the case checks nothing", c.name)
		}
		for _, call := range calls[before:] {
			if _, err := langparser.ParseExpression(call.query); err != nil {
				t.Errorf("%s: THE REAL PARSER REFUSES THE RENDERED CALL\n  %s\n  --> %v\n"+
					"A recording engine never parses, so this is the only test that can see it; in production every such call fails at execute time.",
					c.name, call.query, err)
			}
			if strings.Contains(call.query, "null") {
				t.Errorf("%s renders `null`, a retired spelling the parser refuses: %s", c.name, call.query)
			}
		}
		before = len(calls)
	}
}

// TestEveryCallNamesAConstructAndArgumentsTheDSLDeclares reads dsl/pipelines
// (and Deployables' packageById, and the work spine's workStepsForRun) and
// holds every rendered call to it: the construct exists, of the kind the call
// says, and every argument the call passes is one the construct's args block
// declares.
func TestEveryCallNamesAConstructAndArgumentsTheDSLDeclares(t *testing.T) {
	declared := map[string]dslConstruct{}
	for _, file := range []string{
		"../../dsl/pipelines/queries.memql", "../../dsl/pipelines/mutations.memql",
		"../../dsl/platform/queries.memql", "../../dsl/work/queries.memql",
	} {
		for name, c := range readDSLConstructs(t, file) {
			declared[name] = c
		}
	}
	engine := newRecordingEngine()
	store := NewDSLStore(engine)
	for _, c := range everyStoreCall(store, "value") {
		if err := c.run(); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
	seen := map[string]bool{}
	checked := 0
	for _, call := range engine.recorded() {
		kind, _, _ := strings.Cut(call.query, " ")
		name := constructOf(call.query)
		seen[name] = true
		d, ok := declared[name]
		if !ok {
			t.Errorf("%s %s: no such construct in dsl/pipelines or dsl/platform", kind, name)
			continue
		}
		if d.kind != kind {
			t.Errorf("%s is a %s in the DSL, called as a %s", name, d.kind, kind)
		}
		for _, arg := range topLevelArgs(call.query) {
			checked++
			if !d.args[arg] {
				t.Errorf("%s(%s:): the construct declares no such argument (it declares %v)", name, arg, keys(d.args))
			}
		}
	}
	// The checker reports its own coverage: an argument scan that found
	// nothing would pass every call above and check nothing.
	if checked < 80 {
		t.Errorf("only %d arguments were checked against the DSL; the scan is not reading the calls", checked)
	}
	if args := declared[mCreatePipelineRun].args; !args["runKey"] || !args["checkRunState"] || len(args) < 20 {
		t.Errorf("the DSL reader did not read createPipelineRun's args block: %v", keys(args))
	}
	for _, want := range []string{
		qPipelinesForOwner, qPipelineForOwner, qPipelineForPackage, qPipelineRunsForOwner, qPipelineRunForOwner,
		qPipelinesForRepository, qPipelinesPolled, qPipelineByID, qPipelineRunsForKey, qPipelineRunsForPipelineSha,
		qPipelineRunByCheckRun, qPipelineRunsUnfinished, qPipelineRunsUnfinishedForPullRequest, qPipelineRunsCheckRunLost,
		qPipelineRunByID, qPipelinesActive, qWorkStepsForRun,
		mCreatePipeline, mUpdatePipeline, mCreatePipelineRun, mUpdatePipelineRun, qPackageByID, qInboundRequestByID,
	} {
		if !seen[want] {
			t.Errorf("%s is named in store_dsl.go and no Store method calls it", want)
		}
	}
}

// TestEachCallRunsUnderItsAuthority is the authorization shape: the
// person-facing reads under the caller, unstamped; the server-only reads as
// the pipelines system actor with internal origin; every write as the row's
// owner with internal origin -- and an unknown owner refused before anything
// is executed.
func TestEachCallRunsUnderItsAuthority(t *testing.T) {
	engine := newRecordingEngine()
	store := NewDSLStore(engine)
	caller := personCtx(ownerID)

	if _, err := store.RunForOwner(caller, "r1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RunsForKey(caller, "k"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateRun(caller, "v1:identity:user:"+ownerID, "r1", RunPatch{Status: ptr(StatusInProgress)}); err != nil {
		t.Fatal(err)
	}
	calls := engine.recorded()
	if len(calls) != 3 {
		t.Fatalf("calls = %d", len(calls))
	}

	person := calls[0]
	if person.origin.IsInternal() || person.actor == nil || person.actor.UserId != ownerID {
		t.Errorf("a person-facing read runs under the caller, unstamped: origin %v actor %+v", person.origin, person.actor)
	}

	system := calls[1]
	if !system.origin.IsInternal() {
		t.Errorf("a server-only read is stamped internal")
	}
	if system.actor == nil || system.actor.UserId != "system:pipelines" || !system.actor.IsClusterOwner() || !system.actor.Synthetic {
		t.Errorf("a server-only read is the pipelines system actor (a synthetic cluster owner), not the caller: %+v", system.actor)
	}

	write := calls[2]
	if !write.origin.IsInternal() || write.actor == nil || write.actor.UserId != "v1:identity:user:"+ownerID || write.actor.Synthetic {
		t.Errorf("a write runs as the owner, stamped internal: origin %v actor %+v", write.origin, write.actor)
	}

	if err := store.CreateRun(caller, Run{ID: "r2", RunKey: "k"}); err == nil {
		t.Errorf("a run with no owner must not be written")
	}
	if err := store.UpdatePipeline(caller, "  ", "p1", PipelinePatch{Status: ptr(PipelineActive)}); err == nil {
		t.Errorf("an update with no owner must not be written")
	}
	if n := len(engine.recorded()); n != 3 {
		t.Errorf("a refused write reached the engine: %d calls", n)
	}

	// The stamp never escapes its call: the caller's context is untouched.
	if auth.OriginFromContext(caller).IsInternal() {
		t.Errorf("the caller's context came back stamped")
	}
}

// A connect restates the source's organization AS IT IS NOW, empty included:
// createPipeline is a read-merge insert, so an accountId left out keeps the
// previous connection's -- and through the account tier, that organization's
// members keep reading and writing a pipeline whose source no longer belongs
// to them.
func TestAConnectWritesTheSourcesAccountEvenWhenItIsEmpty(t *testing.T) {
	engine := newRecordingEngine()
	store := NewDSLStore(engine)
	p := Pipeline{
		ID: "p1", OwnerUserID: "v1:identity:user:u1", PackageID: "k1", Name: "shop", Repository: repoName,
		InstallationID: 7, CredentialID: "c1", Delivery: DeliveryWebhook,
	}
	if err := store.CreatePipeline(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	p.AccountID = "v1:accounts:account:a1"
	if err := store.CreatePipeline(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	calls := engine.recorded()
	if len(calls) != 2 {
		t.Fatalf("calls = %d", len(calls))
	}
	if !strings.Contains(calls[0].query, `accountId: ""`) {
		t.Errorf("an untied source writes its pipeline's accountId empty: %s", calls[0].query)
	}
	if !strings.Contains(calls[1].query, `accountId: "a1"`) {
		t.Errorf("a tied source writes its organization, bare: %s", calls[1].query)
	}
}

func TestTheRenderedCallsSayWhatTheRowsAre(t *testing.T) {
	engine := newRecordingEngine()
	store := NewDSLStore(engine)
	ctx := context.Background()
	queued := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	if err := store.CreateRun(ctx, Run{
		ID: "v1:pipelines:run:r1", OwnerUserID: "v1:identity:user:u1", PipelineID: "v1:pipelines:pipeline:p1",
		Repository: "Acme/Shop", SHA: strings.ToUpper(shaA), Mode: pipelines.ModeAffected, Event: pipelines.EventPullRequest,
		RunKey: "acme/shop@" + shaA + ":affected:pull_request", Attempt: 1, Trigger: TriggerWebhook, Status: StatusQueued,
		CheckRunState: CheckRunRefused, Notes: []Note{{Code: pipelines.CodeCheckPermission, Message: "m"}}, QueuedAt: queued,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RunsForOwner(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RunByCheckRun(ctx, "Acme/Shop", 42); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdatePipeline(ctx, "v1:identity:user:u1", "p1", PipelinePatch{Heads: ptr(map[string]string{"pr:2": shaB, "branch:main": shaA})}); err != nil {
		t.Fatal(err)
	}
	calls := engine.recorded()
	// RenderCall's form: arguments in sorted order, values JSON-encoded.
	want := []string{
		`mutation createPipelineRun(attempt: 1, checkRunState: "refused", event: "pull_request", mode: "affected", notes: [{"code":"pipeline_check_permission_missing","message":"m"}], pipelineId: "p1", queuedAt: "2026-10-03T12:00:00Z", repository: "acme/shop", runId: "r1", runKey: "acme/shop@` + shaA + `:affected:pull_request", sha: "` + shaA + `", status: "queued", trigger: "webhook")`,
		`query pipelineRunsForOwner()`,
		`query pipelineRunByCheckRun(checkRunId: "42", repository: "acme/shop")`,
		`mutation updatePipeline(heads: {"branch:main":"` + shaA + `","pr:2":"` + shaB + `"}, pipelineId: "p1")`,
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %d, want %d", len(calls), len(want))
	}
	for i, w := range want {
		if calls[i].query != w {
			t.Errorf("call %d:\n got %s\nwant %s", i, calls[i].query, w)
		}
	}
}

func TestRowsAreReadBackBareAndTyped(t *testing.T) {
	engine := newRecordingEngine()
	engine.answers[qPipelineRunByID] = []any{map[string]any{
		"id":                "v1:pipelines:run:r1",
		"ownerUserId":       "v1:identity:user:u1",
		"accountId":         "v1:accounts:account:a1",
		"pipelineId":        "v1:pipelines:pipeline:p1",
		"repository":        "Acme/Shop",
		"sha":               strings.ToUpper(shaA),
		"mode":              "affected",
		"event":             "pull_request",
		"runKey":            "k",
		"attempt":           float64(3),
		"pullRequest":       float64(42),
		"checkRunId":        "30431907812",
		"checkRunState":     "written",
		"cancelRequested":   true,
		"workGoalId":        "v1:work:goal:g1",
		"workRunId":         "w1",
		"driverNodeId":      "agent-a",
		"driverHeartbeatAt": "2026-10-03T12:00:30Z",
		"queuedAt":          "2026-10-03T12:00:00.5Z",
		"durationMs":        float64(1500),
		"notes":             []any{map[string]any{"code": "c", "message": "m"}},
		"stages":            []any{map[string]any{"name": "checks", "status": "Passed", "durationMs": float64(9), "steps": float64(2), "failed": float64(0)}},
	}}
	engine.answers[qPipelineByID] = []any{map[string]any{
		"payload": map[string]any{
			"ownerUserId":    "v1:identity:user:u1",
			"packageId":      "v1:platform:package:k1",
			"credentialId":   "v1:platform:sourceCredential:c1",
			"installationId": "51234567",
			"heads":          map[string]any{"branch:main": shaA},
			"timings":        map[string]any{"acme.test/a": float64(1.5)},
			"secretNames":    []any{"A", "B"},
			"status":         "active",
		},
		"id": "v1:pipelines:pipeline:p1",
	}}
	store := NewDSLStore(engine)

	r, err := store.RunByID(context.Background(), "r1")
	if err != nil || r == nil {
		t.Fatalf("RunByID: %v %v", r, err)
	}
	if r.ID != "r1" || r.PipelineID != "p1" || r.AccountID != "a1" || r.WorkGoalID != "g1" {
		t.Errorf("ids are read back bare: %+v", r)
	}
	if r.OwnerUserID != "v1:identity:user:u1" {
		t.Errorf("the owner is kept as stored -- it is what authority is borrowed under: %q", r.OwnerUserID)
	}
	if r.Repository != "acme/shop" || r.SHA != shaA {
		t.Errorf("repository and sha are lower-cased: %q %q", r.Repository, r.SHA)
	}
	if r.Attempt != 3 || r.PullRequest != 42 || r.CheckRunID != 30431907812 || r.DurationMs != 1500 || !r.CancelRequested {
		t.Errorf("numbers and flags: %+v", r)
	}
	if r.QueuedAt.IsZero() || r.DriverHeartbeatAt.IsZero() {
		t.Errorf("datetimes: %v %v", r.QueuedAt, r.DriverHeartbeatAt)
	}
	if len(r.Notes) != 1 || r.Notes[0].Code != "c" || len(r.Stages) != 1 || r.Stages[0].Steps != 2 {
		t.Errorf("notes %+v stages %+v", r.Notes, r.Stages)
	}

	p, err := store.PipelineByID(context.Background(), "p1")
	if err != nil || p == nil {
		t.Fatalf("PipelineByID: %v %v", p, err)
	}
	if p.ID != "p1" || p.PackageID != "k1" || p.CredentialID != "c1" || p.InstallationID != 51234567 {
		t.Errorf("a nested payload is lifted and its ids bared: %+v", p)
	}
	if p.Compute != pipelines.ComputeCluster {
		t.Errorf("an absent compute reads as cluster: %q", p.Compute)
	}
	if p.Heads["branch:main"] != shaA || p.Timings["acme.test/a"] != 1.5 || len(p.SecretNames) != 2 {
		t.Errorf("heads %v timings %v secrets %v", p.Heads, p.Timings, p.SecretNames)
	}
}

// A resumed driver reads its predecessor's step rows through the work spine's
// own server-only read, as the pipelines system actor, with the BARE run id
// the journal addresses them by; what it keeps of each is what workStepFull
// projects -- the call the driver declared and the receipt's fields.
func TestWorkStepsAreReadBackForAResume(t *testing.T) {
	engine := newRecordingEngine()
	engine.answers[qWorkStepsForRun] = []any{map[string]any{
		"id":           "v1:work:step:s1",
		"key":          "tests.go#2",
		"seq":          float64(3),
		"status":       "skipped",
		"attempt":      float64(1),
		"durationMs":   float64(1200),
		"errorCode":    pipelines.CodeStageBlocked,
		"errorMessage": "Not run: stage checks failed.",
		"call":         map[string]any{"construct": "pipeline", "name": "go", "stage": "tests", "packages": []any{"acme.test/a", "acme.test/b"}},
		"result":       map[string]any{"reason": "Not run: stage checks failed."},
	}}
	steps, err := NewDSLStore(engine).WorkSteps(context.Background(), "v1:work:run:w1")
	if err != nil || len(steps) != 1 {
		t.Fatalf("WorkSteps: %+v %v", steps, err)
	}
	want := WorkStep{
		Key: "tests.go#2", Seq: 3, Status: WorkStepSkipped, Attempt: 1, DurationMs: 1200,
		ErrorCode: pipelines.CodeStageBlocked, ErrorMessage: "Not run: stage checks failed.",
		Stage: "tests", Name: "go", Packages: []string{"acme.test/a", "acme.test/b"}, Reason: "Not run: stage checks failed.",
	}
	got := steps[0]
	if got.Key != want.Key || got.Seq != want.Seq || got.Status != want.Status || got.Attempt != want.Attempt ||
		got.DurationMs != want.DurationMs || got.ErrorCode != want.ErrorCode || got.ErrorMessage != want.ErrorMessage ||
		got.Stage != want.Stage || got.Name != want.Name || got.Reason != want.Reason || !slices.Equal(got.Packages, want.Packages) {
		t.Errorf("step = %+v, want %+v", got, want)
	}
	calls := engine.recorded()
	if len(calls) != 1 || calls[0].query != `query workStepsForRun(runId: "w1")` {
		t.Fatalf("calls = %+v", calls)
	}
	if !calls[0].origin.IsInternal() || calls[0].actor == nil || calls[0].actor.UserId != "system:pipelines" {
		t.Errorf("the read is the pipelines system actor's, stamped internal: %+v", calls[0].actor)
	}

	if none, err := NewDSLStore(engine).WorkSteps(context.Background(), "  "); err != nil || none != nil || len(engine.recorded()) != 1 {
		t.Errorf("no work run is no read: %v %v", none, err)
	}
}

// The trigger's delivery is read back through inboundRequestFull, as the
// pipelines system actor, by the row's bare id -- and its body exactly as it
// was staged: the signature covered every byte, the whitespace included.
func TestAStagedDeliveryIsReadBackAsStaged(t *testing.T) {
	engine := newRecordingEngine()
	const body = "  {\"action\":\"opened\"}\n"
	engine.answers[qInboundRequestByID] = []any{map[string]any{
		"id": "v1:platform:inboundRequest:in1", "source": "github", "signatureVerified": true,
		"body": body, "headersJson": `{"x-github-event":"pull_request"}`, "status": "received",
	}}
	d, err := NewDSLStore(engine).InboundDelivery(context.Background(), "v1:platform:inboundRequest:in1")
	if err != nil || d == nil {
		t.Fatalf("InboundDelivery: %+v %v", d, err)
	}
	if d.ID != "in1" || d.Source != "github" || !d.SignatureVerified || d.Body != body || d.HeadersJSON != `{"x-github-event":"pull_request"}` {
		t.Errorf("delivery = %+v", d)
	}
	calls := engine.recorded()
	if len(calls) != 1 || calls[0].query != `query inboundRequestById(requestId: "in1")` {
		t.Fatalf("calls = %+v", calls)
	}
	if !calls[0].origin.IsInternal() || calls[0].actor == nil || calls[0].actor.UserId != "system:pipelines" {
		t.Errorf("the read is the pipelines system actor's, stamped internal: %+v", calls[0].actor)
	}

	engine.answers[qInboundRequestByID] = []any{map[string]any{"id": "in2", "source": "github", "body": "{}"}}
	if d, _ := NewDSLStore(engine).InboundDelivery(context.Background(), "in2"); d == nil || d.SignatureVerified {
		t.Errorf("an absent signatureVerified reads as false: %+v", d)
	}
	if none, err := NewDSLStore(engine).InboundDelivery(context.Background(), " "); err != nil || none != nil || len(engine.recorded()) != 2 {
		t.Errorf("no delivery id is no read: %v %v", none, err)
	}
}

// What a push supersedes is read by the pipeline's BARE id and the pull
// request's NUMBER -- an int, as createPipelineRun writes it -- as the
// pipelines system actor, stamped internal; and a pull request numbered 0 or
// less is no pull request, so nothing is read for it (a read of 0 would ask
// for every run that carries no number).
func TestAPullRequestsUnfinishedRunsAreReadByItsNumber(t *testing.T) {
	engine := newRecordingEngine()
	engine.answers[qPipelineRunsUnfinishedForPullRequest] = []any{map[string]any{
		"id": "v1:pipelines:run:r1", "pipelineId": "v1:pipelines:pipeline:p1", "pullRequest": float64(42),
		"mode": "affected", "status": "in_progress",
	}}
	store := NewDSLStore(engine)

	runs, err := store.RunsUnfinishedForPullRequest(personCtx(ownerID), "v1:pipelines:pipeline:p1", 42)
	if err != nil || len(runs) != 1 || runs[0].ID != "r1" || runs[0].PullRequest != 42 || runs[0].Mode != pipelines.ModeAffected {
		t.Fatalf("RunsUnfinishedForPullRequest: %+v %v", runs, err)
	}
	calls := engine.recorded()
	if len(calls) != 1 || calls[0].query != `query pipelineRunsUnfinishedForPullRequest(pipelineId: "p1", pullRequest: 42)` {
		t.Fatalf("calls = %+v", calls)
	}
	if !calls[0].origin.IsInternal() || calls[0].actor == nil || calls[0].actor.UserId != "system:pipelines" {
		t.Errorf("the read is the pipelines system actor's, stamped internal, whoever asked: %+v", calls[0].actor)
	}
	for _, none := range []int{0, -1} {
		if got, err := store.RunsUnfinishedForPullRequest(context.Background(), "p1", none); err != nil || got != nil {
			t.Errorf("pull request %d: %+v %v", none, got, err)
		}
	}
	if n := len(engine.recorded()); n != 1 {
		t.Errorf("no pull request is no read: %d calls", n)
	}
}

// ---------------------------------------------------------------------------
// Reading the DSL
// ---------------------------------------------------------------------------

type dslConstruct struct {
	kind string
	args map[string]bool
}

var constructHead = regexp.MustCompile(`^(query|mutation)\s+\w+\s+(\w+)\s*\{`)

// readDSLConstructs reads the queries and mutations a .memql file declares,
// with the names in each one's args block.
func readDSLConstructs(t *testing.T, path string) map[string]dslConstruct {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	out := map[string]dslConstruct{}
	lines := strings.Split(string(raw), "\n")
	for i := 0; i < len(lines); i++ {
		m := constructHead.FindStringSubmatch(strings.TrimSpace(lines[i]))
		if m == nil {
			continue
		}
		c := dslConstruct{kind: m[1], args: map[string]bool{}}
		depth := strings.Count(lines[i], "{") - strings.Count(lines[i], "}")
		inArgs := false
		for j := i + 1; j < len(lines) && depth > 0; j++ {
			line := strings.TrimSpace(lines[j])
			if strings.HasPrefix(line, "args {") || strings.HasPrefix(line, "args{") {
				inArgs = true
			} else if inArgs && line == "}" {
				inArgs = false
			} else if inArgs && line != "" && !strings.HasPrefix(line, "//") {
				c.args[strings.Fields(line)[0]] = true
			}
			depth += strings.Count(lines[j], "{") - strings.Count(lines[j], "}")
		}
		out[m[2]] = c
	}
	if len(out) == 0 {
		t.Fatalf("%s declares no query or mutation -- this reader is broken", path)
	}
	return out
}

// topLevelArgs are the argument names of a rendered call, skipping anything
// inside a string, an object or a list.
func topLevelArgs(query string) []string {
	open := strings.IndexByte(query, '(')
	if open < 0 {
		return nil
	}
	var (
		out   []string
		depth int
		inStr bool
		esc   bool
		start = open + 1
	)
	body := query[:len(query)-1] // drop the closing paren
	for i := open + 1; i < len(body); i++ {
		c := body[i]
		switch {
		case inStr:
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
		case c == '"':
			inStr = true
		case c == '{' || c == '[':
			depth++
		case c == '}' || c == ']':
			depth--
		case c == ':' && depth == 0:
			name := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(body[start:i]), ","))
			out = append(out, name)
		case c == ',' && depth == 0:
			start = i + 1
		}
	}
	return out
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
