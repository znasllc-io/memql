package pipelinerun

import (
	"context"
	"errors"
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
	// byCall answers one exact rendered call, ahead of answers: how a test hands
	// two calls to one construct two different rows (one present, one absent).
	byCall map[string]any
}

func newRecordingEngine() *recordingEngine {
	return &recordingEngine{answers: map[string]any{}, byCall: map[string]any{}}
}

func (e *recordingEngine) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	ac, _ := auth.AccessFromContext(ctx)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, recordedCall{query: query, origin: auth.OriginFromContext(ctx), actor: ac, fresh: memql.FreshReadFromContext(ctx)})
	if answer, ok := e.byCall[query]; ok {
		return memql.NewResultWithOutput(answer), nil
	}
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
	ch := Channel{
		ID: value, OwnerUserID: value, AccountID: value, Name: value, Kind: value, SecretRef: value,
		Recipients: []string{value, "ops@example.test"},
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
		{"ChannelsForOwner", func() error { _, err := s.ChannelsForOwner(ctx); return err }},
		{"ChannelForOwnerByName", func() error { _, err := s.ChannelForOwnerByName(ctx, value, value); return err }},
		{"PreviousRuns", func() error { _, err := s.PreviousRuns(ctx, value, pipelines.Event(value)); return err }},
		{"OutboundStatuses", func() error { _, err := s.OutboundStatuses(ctx, []string{value, "second"}); return err }},
		{"LibraryFileNames", func() error { _, err := s.LibraryFileNames(ctx, value, []string{value, "second"}); return err }},
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
		{"CreateChannel (every field)", func() error { return s.CreateChannel(ctx, ch) }},
		{"CreateChannel (a Discord channel)", func() error {
			return s.CreateChannel(ctx, Channel{ID: value, OwnerUserID: value, Name: value, Kind: "discord", SecretRef: value})
		}},
		{"UpdateChannel (every field)", func() error {
			return s.UpdateChannel(ctx, value, value, ChannelPatch{
				Name: ptr(value), Kind: ptr(value), SecretRef: ptr(value), Status: ptr(value), Recipients: ptr([]string{value}),
			})
		}},
		{"UpdateChannel (clearing)", func() error {
			return s.UpdateChannel(ctx, value, value, ChannelPatch{SecretRef: ptr(""), Recipients: ptr([]string(nil))})
		}},
		{"StageNotification (to a secret)", func() error {
			return s.StageNotification(ctx, NotificationRequest{
				RequestID: value, Medium: "webhook", TargetSecret: value, Subject: value, Body: value, DedupeKey: value, RequestedBy: value,
			})
		}},
		{"StageNotification (a plain row)", func() error {
			return s.StageNotification(ctx, NotificationRequest{
				RequestID: value, Medium: "email", Target: value, Subject: value, Body: value, DedupeKey: value, RequestedBy: value,
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
// (and Deployables' packageById, the work spine's workStepsForRun, the outbound
// seam's staging and by-id read, and the Library's libraryFileById) and holds
// every rendered call to it: the construct exists, of the kind the call says,
// and every argument the call passes is one the construct's args block
// declares.
func TestEveryCallNamesAConstructAndArgumentsTheDSLDeclares(t *testing.T) {
	declared := map[string]dslConstruct{}
	for _, file := range []string{
		"../../dsl/pipelines/queries.memql", "../../dsl/pipelines/mutations.memql",
		"../../dsl/platform/queries.memql", "../../dsl/platform/mutations.memql",
		"../../dsl/work/queries.memql", "../../dsl/library/queries.memql",
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
			t.Errorf("%s %s: no such construct in the DSL files this test reads", kind, name)
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
		qPipelineChannelsForOwner, qPipelineChannelForOwnerByName, qPipelineRunsForPipelineEvent, mCreatePipelineChannel, mUpdatePipelineChannel,
		mStageServerOutboundRequest, mStageOutboundRequestToSecret, qOutboundRequestByID, qLibraryFileByID,
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

// refusedChannelAndOutboundCalls are calls no Store may act on, each for its own
// reason: a read or a write on a person's behalf names the person, and a
// notification names where it goes and what it says. The DSL store refuses them
// BEFORE it renders anything, and the fake store refuses them the same way, so
// a test that runs over the fake is not more forgiving than production.
func refusedChannelAndOutboundCalls(s Store) []struct {
	name string
	run  func() error
} {
	ctx := context.Background()
	return []struct {
		name string
		run  func() error
	}{
		{"a channel read for nobody", func() error { _, err := s.ChannelForOwnerByName(ctx, "  ", "releases"); return err }},
		{"a channel read for nobody, with nothing to find", func() error { _, err := s.ChannelForOwnerByName(ctx, "", ""); return err }},
		{"Library names for nobody", func() error { _, err := s.LibraryFileNames(ctx, "", []string{"f1"}); return err }},
		{"Library names for nobody, none asked", func() error { _, err := s.LibraryFileNames(ctx, "", nil); return err }},
		{"a channel written for nobody", func() error {
			return s.CreateChannel(ctx, Channel{ID: "c1", Name: "releases", Kind: "email", Recipients: []string{"a@example.test"}})
		}},
		{"a channel with no id", func() error {
			return s.CreateChannel(ctx, Channel{ID: "  ", OwnerUserID: "v1:identity:user:u1", Name: "releases", Kind: "email", Recipients: []string{"a@example.test"}})
		}},
		{"a channel updated for nobody", func() error {
			return s.UpdateChannel(ctx, " ", "c1", ChannelPatch{Status: ptr("archived")})
		}},
		{"a secret notification that is not a webhook", func() error {
			return s.StageNotification(ctx, NotificationRequest{RequestID: "pn1", Medium: "email", TargetSecret: "DISCORD_RELEASES", Body: "x"})
		}},
		{"a secret notification naming no medium", func() error {
			return s.StageNotification(ctx, NotificationRequest{RequestID: "pn1", TargetSecret: "DISCORD_RELEASES", Body: "x"})
		}},
		{"a secret notification that also names a target", func() error {
			return s.StageNotification(ctx, NotificationRequest{
				RequestID: "pn1", Medium: "webhook", TargetSecret: "DISCORD_RELEASES", Target: "https://example.test/hook", Body: "x",
			})
		}},
		{"a notification with no request id", func() error {
			return s.StageNotification(ctx, NotificationRequest{Medium: "webhook", TargetSecret: "DISCORD_RELEASES", Body: "x"})
		}},
		{"a notification saying nothing", func() error {
			return s.StageNotification(ctx, NotificationRequest{RequestID: "pn1", Medium: "webhook", TargetSecret: "DISCORD_RELEASES", Body: "  "})
		}},
		{"a plain notification naming no target", func() error {
			return s.StageNotification(ctx, NotificationRequest{RequestID: "pn2", Medium: "email", Body: "x"})
		}},
		{"a plain notification naming no medium", func() error {
			return s.StageNotification(ctx, NotificationRequest{RequestID: "pn2", Target: "a@example.test", Body: "x"})
		}},
	}
}

// TestTheChannelAndOutboundCallsRunUnderTheirAuthority is the authorization
// shape of what the notify stage reads and writes. A person's channels are read
// under the caller. A channel by name and a Library file are read under their
// OWNER's borrowed authority -- not the caller's, and unstamped, because the
// owner conjunct of the construct decides the rows and the borrowed actor
// reads exactly what that person could. The previous runs, the staging and the
// delivery status are the pipelines system actor's, stamped internal: no person
// owns an outbound row or speaks for a driver. The channel's writes are the
// owner's, stamped. And a call that names nobody, or a notification that
// cannot be sent as written, is refused before anything is executed.
func TestTheChannelAndOutboundCallsRunUnderTheirAuthority(t *testing.T) {
	engine := newRecordingEngine()
	store := NewDSLStore(engine)
	caller := personCtx(otherID)
	owner := "v1:identity:user:" + ownerID

	one := func(name string, run func() error) recordedCall {
		t.Helper()
		before := len(engine.recorded())
		if err := run(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		calls := engine.recorded()[before:]
		if len(calls) != 1 {
			t.Fatalf("%s made %d calls, want exactly 1: %v", name, len(calls), calls)
		}
		return calls[0]
	}
	asSystem := func(c recordedCall) bool {
		return c.origin.IsInternal() && c.actor != nil && c.actor.UserId == "system:pipelines" && c.actor.IsClusterOwner() && c.actor.Synthetic
	}
	asOwner := func(c recordedCall, stamped bool) bool {
		return c.origin.IsInternal() == stamped && c.actor != nil && c.actor.UserId == owner && !c.actor.Synthetic
	}

	if c := one("ChannelsForOwner", func() error { _, err := store.ChannelsForOwner(caller); return err }); c.origin.IsInternal() ||
		c.actor == nil || c.actor.UserId != otherID {
		t.Errorf("a person's own channels are read under the caller, unstamped: %+v origin %v", c.actor, c.origin)
	}
	if c := one("ChannelForOwnerByName", func() error { _, err := store.ChannelForOwnerByName(caller, owner, "releases"); return err }); !asOwner(c, false) {
		t.Errorf("a channel by name is read under its OWNER's borrowed authority, not the caller's, and unstamped: %+v origin %v", c.actor, c.origin)
	}
	if c := one("LibraryFileNames", func() error { _, err := store.LibraryFileNames(caller, owner, []string{"f1"}); return err }); !asOwner(c, false) {
		t.Errorf("a Library file is read under its owner's borrowed authority, unstamped: %+v origin %v", c.actor, c.origin)
	}
	if c := one("PreviousRuns", func() error { _, err := store.PreviousRuns(caller, "p1", pipelines.EventPush); return err }); !asSystem(c) {
		t.Errorf("the previous runs are a server-only read, as the system actor: %+v origin %v", c.actor, c.origin)
	}
	// A delivery's state is moved by the outbound worker on whichever replica
	// claimed the row, so the status read is FRESH by itself: no caller has to
	// remember, and one that did not ask still gets the database's answer.
	if c := one("OutboundStatuses", func() error { _, err := store.OutboundStatuses(caller, []string{"pn1"}); return err }); !asSystem(c) || !c.fresh {
		t.Errorf("a delivery's status is a server-only read, as the system actor, and always fresh: %+v origin %v fresh %v", c.actor, c.origin, c.fresh)
	}
	// The other reads a driver decides on are fresh when the CALLER marks them,
	// and the store hands the mark on rather than dropping it -- and adds none.
	for name, read := range map[string]func(context.Context) error{
		"ChannelForOwnerByName": func(ctx context.Context) error {
			_, err := store.ChannelForOwnerByName(ctx, owner, "releases")
			return err
		},
		"PreviousRuns": func(ctx context.Context) error {
			_, err := store.PreviousRuns(ctx, "p1", pipelines.EventPush)
			return err
		},
	} {
		if c := one(name, func() error { return read(caller) }); c.fresh {
			t.Errorf("%s is fresh only when its caller says so: the store marked a plain call", name)
		}
		if c := one(name+" (fresh)", func() error { return read(memql.ContextWithFreshRead(caller)) }); !c.fresh {
			t.Errorf("%s dropped its caller's fresh mark", name)
		}
	}
	for _, n := range []NotificationRequest{
		{RequestID: "pn1", Medium: "webhook", TargetSecret: "DISCORD_RELEASES", Body: "{}"},
		{RequestID: "pn2", Medium: "email", Target: "a@example.test", Body: "text"},
	} {
		if c := one("StageNotification "+n.RequestID, func() error { return store.StageNotification(caller, n) }); !asSystem(c) {
			t.Errorf("staging is a write as the system actor with internal origin, because the row has no owner to borrow: %+v origin %v", c.actor, c.origin)
		}
	}
	if c := one("CreateChannel", func() error {
		return store.CreateChannel(caller, Channel{ID: "c1", OwnerUserID: owner, Name: "releases", Kind: "discord", SecretRef: "DISCORD_RELEASES"})
	}); !asOwner(c, true) {
		t.Errorf("a channel is written as its owner, stamped: %+v origin %v", c.actor, c.origin)
	}
	if c := one("UpdateChannel", func() error {
		return store.UpdateChannel(caller, owner, "c1", ChannelPatch{Status: ptr("archived")})
	}); !asOwner(c, true) {
		t.Errorf("a channel is changed as its owner, stamped: %+v origin %v", c.actor, c.origin)
	}

	before := len(engine.recorded())
	for _, c := range refusedChannelAndOutboundCalls(store) {
		if err := c.run(); err == nil {
			t.Errorf("%s was not refused", c.name)
		}
	}
	if n := len(engine.recorded()) - before; n != 0 {
		t.Errorf("a refused call reached the engine: %d calls", n)
	}
	// A name nobody typed asks nothing: there is no channel to find.
	if none, err := store.ChannelForOwnerByName(caller, owner, "  "); err != nil || none != nil || len(engine.recorded()) != before {
		t.Errorf("no name is no read: %v %v", none, err)
	}
	if auth.OriginFromContext(caller).IsInternal() {
		t.Errorf("the stamp never escapes its call: the caller's context came back stamped")
	}
}

// TestTheFakeStoreRefusesWhatTheDSLStoreRefuses keeps the fake the notify
// stage's tests run over exactly as strict as production about who a call is
// for and what a notification says.
func TestTheFakeStoreRefusesWhatTheDSLStoreRefuses(t *testing.T) {
	for name, store := range map[string]Store{"dsl": NewDSLStore(newRecordingEngine()), "fake": newMemStore()} {
		for _, c := range refusedChannelAndOutboundCalls(store) {
			if err := c.run(); err == nil {
				t.Errorf("the %s store did not refuse %s", name, c.name)
			}
		}
		if none, err := store.ChannelForOwnerByName(context.Background(), "v1:identity:user:"+ownerID, " "); err != nil || none != nil {
			t.Errorf("the %s store: no name is no channel, and no error: %v %v", name, none, err)
		}
	}
}

// failingEngine answers every call with an error: an engine that cannot reach
// its storage.
type failingEngine struct{ err error }

func (e failingEngine) Execute(context.Context, string) (*memql.ExecuteResult, error) {
	return nil, e.err
}

// A read that fails is an error. The notify stage concludes from a delivery's
// row alone, and a failure it could mistake for a row that is not there -- or,
// worse, for one that is `sent` -- is a notification reported delivered that
// never was.
func TestAFailedReadIsAnErrorNeverAnAbsentRow(t *testing.T) {
	down := errors.New("storage is down")
	store := NewDSLStore(failingEngine{err: down})
	ctx := context.Background()

	if got, err := store.OutboundStatuses(ctx, []string{"pn1", "pn2"}); !errors.Is(err, down) || got != nil {
		t.Errorf("OutboundStatuses: %+v %v", got, err)
	}
	if got, err := store.ChannelForOwnerByName(ctx, "v1:identity:user:u1", "releases"); !errors.Is(err, down) || got != nil {
		t.Errorf("ChannelForOwnerByName: %+v %v", got, err)
	}
	if got, err := store.LibraryFileNames(ctx, "v1:identity:user:u1", []string{"f1"}); !errors.Is(err, down) || got != nil {
		t.Errorf("LibraryFileNames: %+v %v", got, err)
	}
	if got, err := store.PreviousRuns(ctx, "p1", pipelines.EventPush); !errors.Is(err, down) || got != nil {
		t.Errorf("PreviousRuns: %+v %v", got, err)
	}
	if err := store.StageNotification(ctx, NotificationRequest{RequestID: "pn1", Medium: "email", Target: "a@example.test", Body: "x"}); !errors.Is(err, down) {
		t.Errorf("StageNotification: %v", err)
	}
}

// A blank key asks nothing. A pipeline or an event that is not there is not a
// question, so PreviousRuns answers nothing without asking the engine; and a
// blank id in a status poll is an empty entry in its place -- the notify stage
// reads the answer by position -- with no read made for it, and the real ids
// beside it read as usual.
func TestABlankKeyAsksNothing(t *testing.T) {
	ctx := context.Background()
	engine := newRecordingEngine()
	for name, store := range map[string]Store{"dsl": NewDSLStore(engine), "fake": newMemStore()} {
		before := len(engine.recorded())
		for _, c := range []struct {
			pipeline string
			event    pipelines.Event
		}{{"", pipelines.EventPush}, {"  ", pipelines.EventPush}, {"p1", ""}, {"p1", "  "}, {"", ""}} {
			if got, err := store.PreviousRuns(ctx, c.pipeline, c.event); err != nil || got != nil {
				t.Errorf("the %s store: PreviousRuns(%q, %q) = %+v, %v; want nothing", name, c.pipeline, c.event, got, err)
			}
		}
		if n := len(engine.recorded()) - before; n != 0 {
			t.Errorf("the %s store: a blank pipeline or event reached the engine: %d calls", name, n)
		}

		got, err := store.OutboundStatuses(ctx, []string{"", "  ", "pn1"})
		if err != nil || len(got) != 3 {
			t.Fatalf("the %s store: OutboundStatuses: %+v %v", name, got, err)
		}
		if got[0] != (OutboundStatus{}) || got[1] != (OutboundStatus{}) || got[2].ID != "pn1" {
			t.Errorf("the %s store: a blank id is an empty entry in its place, the real id beside it: %+v", name, got)
		}
		if name == "dsl" {
			calls := engine.recorded()[before:]
			if len(calls) != 1 || calls[0].query != `query outboundRequestById(requestId: "pn1")` {
				t.Errorf("a blank id asks nothing, and the real one is read once: %v", calls)
			}
		}
	}
}

// The fake the notify stage's tests run over keeps the rules the DSL keeps:
// a channel is created `active` whatever the value says, its by-name read is the
// OWNER's whoever the context carries (and the newest of two rows sharing a name
// answers), an update names the fields it changes, and an outbound row is
// pending when staged, moved only by the worker, and -- @createOnly -- still
// where the worker left it after a second stage at its id.
func TestTheFakeStoreKeepsTheChannelAndOutboxRules(t *testing.T) {
	s := newMemStore()
	ctx := context.Background()
	const owner = "v1:identity:user:" + ownerID
	ok := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	ok(s.CreateChannel(ctx, Channel{ID: "c1", OwnerUserID: owner, Name: "releases", Kind: "discord", SecretRef: "DISCORD_RELEASES", Status: "archived"}))
	ok(s.CreateChannel(ctx, Channel{ID: "c2", OwnerUserID: owner, Name: "ops", Kind: "email", Recipients: []string{"a@example.test"}}))
	ok(s.CreateChannel(ctx, Channel{ID: "c3", OwnerUserID: owner, Name: "releases", Kind: "discord", SecretRef: "DISCORD_OTHER"}))
	if c, _ := s.channel("c1"); c.Status != "active" {
		t.Errorf("a channel is created active, whatever the value says: %q", c.Status)
	}
	if got, err := s.ChannelForOwnerByName(ctx, owner, "releases"); err != nil || got == nil || got.ID != "c3" {
		t.Errorf("the owner's channel is read with nobody on the context, the newest of two sharing a name: %+v %v", got, err)
	}
	if got, _ := s.ChannelForOwnerByName(ctx, "user-bob", "releases"); got != nil {
		t.Errorf("another owner's borrowed read finds nobody else's channel: %+v", got)
	}
	if mine, _ := s.ChannelsForOwner(personCtx(ownerID)); len(mine) != 3 || mine[0].Name != "ops" {
		t.Errorf("the caller's own channels, by name: %+v", mine)
	}
	if theirs, _ := s.ChannelsForOwner(personCtx(otherID)); len(theirs) != 0 {
		t.Errorf("a stranger lists none: %+v", theirs)
	}
	ok(s.UpdateChannel(ctx, owner, "c2", ChannelPatch{Status: ptr("archived")}))
	ok(s.UpdateChannel(ctx, owner, "c2", ChannelPatch{Recipients: ptr([]string{})}))
	if c, _ := s.channel("c2"); c.Status != "archived" || c.Name != "ops" || c.Kind != "email" || len(c.Recipients) != 0 {
		t.Errorf("an update changes what it names and an empty list clears: %+v", c)
	}
	// Production does not check the owner of an update (see the port), so the
	// fake must not either: a test that passed on the fake's refusal would hide a
	// caller that never made the owner-scoped read.
	ok(s.UpdateChannel(ctx, "user-bob", "c2", ChannelPatch{Status: ptr("active")}))
	if c, _ := s.channel("c2"); c.Status != "active" {
		t.Errorf("the owner an update names is attribution, as in production: %+v", c)
	}
	if err := s.UpdateChannel(ctx, owner, "c-nothing", ChannelPatch{Status: ptr("active")}); err == nil {
		t.Errorf("a channel that is not there is an error, as in production")
	}

	n := NotificationRequest{RequestID: "v1:platform:outboundRequest:pn1", Medium: "webhook", TargetSecret: "DISCORD_RELEASES", Body: "first"}
	ok(s.StageNotification(ctx, n))
	if got, _ := s.OutboundStatuses(ctx, []string{"pn1", "pn9"}); len(got) != 2 || got[0].ID != "pn1" || got[0].Status != "pending" || got[1].Status != "" {
		t.Errorf("a staged row is pending and an unstaged one has no status: %+v", got)
	}
	s.setOutbound("pn1", "sent", 1, "")
	n.Body = "second"
	ok(s.StageNotification(ctx, n))
	rows := s.outboundRows()
	if len(rows) != 1 || rows[0].State.Status != "sent" || rows[0].Request.Body != "second" || !rows[0].State.SentAt.Equal(testNow) {
		t.Errorf("@createOnly: a second stage refreshes the body and leaves the delivery state: %+v", rows)
	}
	if staged := s.stagedNotifications(); len(staged) != 2 {
		t.Errorf("both stage calls are on record: %+v", staged)
	}

	s.addFile(owner, "v1:library:file:f1", "tests.log")
	if names, _ := s.LibraryFileNames(ctx, owner, []string{"f1", "f2"}); len(names) != 1 || names["f1"] != "tests.log" {
		t.Errorf("an owner's file is named, under either spelling of its id: %v", names)
	}
	if names, _ := s.LibraryFileNames(ctx, "user-bob", []string{"f1"}); len(names) != 0 {
		t.Errorf("another owner's file is not there to read: %v", names)
	}
}

// The calls themselves, as the engine receives them: the construct, the
// arguments in RenderCall's sorted order, each bare id bare, an empty optional
// field left out of a create (the row is new, so absent is the truth) and an
// empty list SENT by an update (it is how a read-merge write clears one).
func TestTheChannelAndOutboundCallsSayWhatTheRowsAre(t *testing.T) {
	engine := newRecordingEngine()
	store := NewDSLStore(engine)
	ctx := context.Background()
	const owner = "v1:identity:user:u1"
	ok := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	ok(store.CreateChannel(ctx, Channel{
		ID: "v1:pipelines:channel:c1", OwnerUserID: owner, AccountID: "v1:accounts:account:a1", Name: "ops", Kind: "email",
		Recipients: []string{"a@example.test", "b@example.test"},
	}))
	ok(store.CreateChannel(ctx, Channel{ID: "c2", OwnerUserID: owner, Name: "releases", Kind: "discord", SecretRef: "DISCORD_RELEASES"}))
	ok(store.UpdateChannel(ctx, owner, "v1:pipelines:channel:c1", ChannelPatch{Status: ptr("archived"), Recipients: ptr([]string{})}))
	ok(store.UpdateChannel(ctx, owner, "c2", ChannelPatch{Name: ptr("deploys"), SecretRef: ptr("DISCORD_DEPLOYS")}))
	_, err := store.ChannelsForOwner(ctx)
	ok(err)
	_, err = store.ChannelForOwnerByName(ctx, owner, " releases ")
	ok(err)
	_, err = store.PreviousRuns(ctx, "v1:pipelines:pipeline:p1", pipelines.EventRelease)
	ok(err)
	ok(store.StageNotification(ctx, NotificationRequest{
		RequestID: "v1:platform:outboundRequest:pn1", Medium: "webhook", TargetSecret: "DISCORD_RELEASES", Body: `{"username":"MemQL"}`,
		DedupeKey: "pn1", RequestedBy: "pipelines:notify:r1",
	}))
	ok(store.StageNotification(ctx, NotificationRequest{
		RequestID: "pn2", Medium: "email", Target: "a@example.test", Subject: "memql - Release v1 passed", Body: "text",
	}))
	_, err = store.OutboundStatuses(ctx, []string{"v1:platform:outboundRequest:pn1", "pn2"})
	ok(err)
	_, err = store.LibraryFileNames(ctx, owner, []string{"v1:library:file:f1", "f1", " f2 "})
	ok(err)

	want := []string{
		`mutation createPipelineChannel(accountId: "a1", channelId: "c1", kind: "email", name: "ops", recipients: ["a@example.test","b@example.test"])`,
		`mutation createPipelineChannel(channelId: "c2", kind: "discord", name: "releases", secretRef: "DISCORD_RELEASES")`,
		`mutation updatePipelineChannel(channelId: "c1", recipients: [], status: "archived")`,
		`mutation updatePipelineChannel(channelId: "c2", name: "deploys", secretRef: "DISCORD_DEPLOYS")`,
		`query pipelineChannelsForOwner()`,
		`query pipelineChannelForOwnerByName(name: "releases")`,
		`query pipelineRunsForPipelineEvent(event: "release", pipelineId: "p1")`,
		`mutation stageOutboundRequestToSecret(body: "{\"username\":\"MemQL\"}", dedupeKey: "pn1", requestId: "pn1", requestedBy: "pipelines:notify:r1", targetSecret: "DISCORD_RELEASES")`,
		`mutation stageServerOutboundRequest(body: "text", medium: "email", requestId: "pn2", subject: "memql - Release v1 passed", target: "a@example.test")`,
		`query outboundRequestById(requestId: "pn1")`,
		`query outboundRequestById(requestId: "pn2")`,
		// One read per FILE, however its id is spelled.
		`query libraryFileById(fileId: "f1")`,
		`query libraryFileById(fileId: "f2")`,
	}
	calls := engine.recorded()
	if len(calls) != len(want) {
		t.Fatalf("calls = %d, want %d:\n%v", len(calls), len(want), calls)
	}
	for i, w := range want {
		if calls[i].query != w {
			t.Errorf("call %d:\n got %s\nwant %s", i, calls[i].query, w)
		}
	}
}

// What comes back is read the way every other row is: ids bare, the owner as
// stored (it is what authority is borrowed under), the recipients trimmed, a
// delivery's status exactly as the worker left it, and a row that is not there
// an ANSWER -- an outbound status with no status -- rather than an error.
func TestChannelAndOutboundRowsAreReadBack(t *testing.T) {
	engine := newRecordingEngine()
	engine.answers[qPipelineChannelForOwnerByName] = []any{map[string]any{
		"id": "v1:pipelines:channel:c1", "ownerUserId": "v1:identity:user:u1", "accountId": "v1:accounts:account:a1",
		"name": "ops", "kind": "email", "secretRef": "", "status": "active",
		"recipients": []any{" a@example.test ", "b@example.test", ""},
	}}
	engine.answers[qPipelineChannelsForOwner] = []any{
		map[string]any{"id": "v1:pipelines:channel:c2", "ownerUserId": "v1:identity:user:u1", "name": "releases", "kind": "discord", "secretRef": "DISCORD_RELEASES", "status": "archived"},
		map[string]any{"payload": map[string]any{"ownerUserId": "v1:identity:user:u1", "name": "ops", "kind": "email", "status": "active"}, "id": "v1:pipelines:channel:c1"},
	}
	engine.answers[qPipelineRunsForPipelineEvent] = []any{
		map[string]any{"id": "v1:pipelines:run:r2", "pipelineId": "v1:pipelines:pipeline:p1", "event": "push", "status": "completed", "conclusion": "failure", "queuedAt": "2026-10-03T12:05:00Z"},
		map[string]any{"id": "v1:pipelines:run:r1", "pipelineId": "v1:pipelines:pipeline:p1", "event": "push", "status": "completed", "conclusion": "success", "queuedAt": "2026-10-03T12:00:00Z"},
	}
	engine.byCall[`query outboundRequestById(requestId: "pn1")`] = []any{map[string]any{
		"id": "v1:platform:outboundRequest:pn1", "status": "sent", "attempts": float64(1), "sentAt": "2026-10-03T12:00:05Z",
	}}
	engine.byCall[`query outboundRequestById(requestId: "pn2")`] = []any{map[string]any{
		"id": "v1:platform:outboundRequest:pn2", "status": "retrying", "attempts": float64(3), "lastError": "webhook: status 502",
	}}
	engine.byCall[`query libraryFileById(fileId: "f1")`] = []any{map[string]any{"id": "v1:library:file:f1", "name": "tests.log"}}
	engine.byCall[`query libraryFileById(fileId: "f2")`] = []any{map[string]any{"id": "v1:library:file:f2", "name": "tests.xml"}}
	store := NewDSLStore(engine)
	ctx := context.Background()

	ch, err := store.ChannelForOwnerByName(ctx, "v1:identity:user:u1", "ops")
	if err != nil || ch == nil {
		t.Fatalf("ChannelForOwnerByName: %+v %v", ch, err)
	}
	if ch.ID != "c1" || ch.AccountID != "a1" || ch.OwnerUserID != "v1:identity:user:u1" || ch.Name != "ops" || ch.Kind != "email" ||
		ch.Status != "active" || ch.SecretRef != "" || !slices.Equal(ch.Recipients, []string{"a@example.test", "b@example.test"}) {
		t.Errorf("channel = %+v", ch)
	}
	list, err := store.ChannelsForOwner(ctx)
	if err != nil || len(list) != 2 || list[0].ID != "c2" || list[0].SecretRef != "DISCORD_RELEASES" || list[0].Status != "archived" ||
		list[1].ID != "c1" || list[1].Name != "ops" || list[1].OwnerUserID != "v1:identity:user:u1" {
		t.Errorf("channels = %+v %v", list, err)
	}
	engine.answers[qPipelineChannelForOwnerByName] = []any{}
	if none, err := store.ChannelForOwnerByName(ctx, "v1:identity:user:u1", "nobody-has-this"); err != nil || none != nil {
		t.Errorf("a name nothing carries reads nothing: %+v %v", none, err)
	}

	runs, err := store.PreviousRuns(ctx, "p1", pipelines.EventPush)
	if err != nil || len(runs) != 2 || runs[0].ID != "r2" || runs[0].Conclusion != ConclusionFailure || runs[0].PipelineID != "p1" ||
		runs[1].ID != "r1" || runs[1].Event != pipelines.EventPush || runs[1].QueuedAt.IsZero() {
		t.Errorf("previous runs, as the server hands them (newest first): %+v %v", runs, err)
	}

	got, err := store.OutboundStatuses(ctx, []string{"v1:platform:outboundRequest:pn1", "pn2", "pn3"})
	if err != nil || len(got) != 3 {
		t.Fatalf("OutboundStatuses: %+v %v", got, err)
	}
	if got[0].ID != "pn1" || got[0].Status != "sent" || got[0].Attempts != 1 || !got[0].SentAt.Equal(time.Date(2026, 10, 3, 12, 0, 5, 0, time.UTC)) || got[0].LastError != "" {
		t.Errorf("a delivered row: %+v", got[0])
	}
	if got[1].ID != "pn2" || got[1].Status != "retrying" || got[1].Attempts != 3 || got[1].LastError != "webhook: status 502" || !got[1].SentAt.IsZero() {
		t.Errorf("a row still being retried keeps what the worker left on it: %+v", got[1])
	}
	if got[2].ID != "pn3" || got[2].Status != "" || got[2].Attempts != 0 {
		t.Errorf("a row nothing staged is an entry with no status, one entry per id asked, in order: %+v", got[2])
	}

	names, err := store.LibraryFileNames(ctx, "v1:identity:user:u1", []string{"v1:library:file:f1", " f2 ", "f1", " ", "f3"})
	if err != nil {
		t.Fatalf("LibraryFileNames: %v", err)
	}
	if len(names) != 3 || names["v1:library:file:f1"] != "tests.log" || names["f1"] != "tests.log" || names["f2"] != "tests.xml" {
		t.Errorf("names are keyed by each id as the caller spelled it, trimmed, and a file the owner cannot read is simply absent: %v", names)
	}
	if _, padded := names[" f2 "]; padded {
		t.Errorf("a key is trimmed: %v", names)
	}
	// One read per call above: two by name, the list, the previous runs, three
	// statuses, and the three FILES (f1 once, however it was spelled; the blank
	// asks nothing).
	if n := len(engine.recorded()); n != 10 {
		t.Errorf("calls = %d, want 10", n)
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
