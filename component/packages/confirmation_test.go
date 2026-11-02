package packages

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/edge"
	"github.com/znasllc-io/memql/component/memql"
)

func TestBackgroundConfirmationRefusesBeforeClaim(t *testing.T) {
	for _, name := range []string{"unconfirmed", "automatic", "missing id", "missing report", "missing snapshot", "different snapshot", "cancel requested", "no database gate", "claim write unavailable"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, spaOnlyPackage(), ownerPackage())
			rep, err := Analyze(spaOnlyPackage(), Options{SourceVersion: "sha-abc123"})
			if err != nil {
				t.Fatal(err)
			}
			row := parkedRun("saved", "abc", map[string]any{"snapshotArtifactId": "snapshot", "report": reportMap(t, rep)})
			h.engine.rows["query packageDeploymentById"] = []map[string]any{row}
			req := DeployRequest{PackageId: "abc", DeploymentId: "saved", Confirmed: true, Actor: plainUser()}
			switch name {
			case "unconfirmed":
				req.Confirmed = false
			case "automatic":
				req.Automatic = true
			case "missing id":
				req.DeploymentId = ""
			case "missing report":
				delete(row, "report")
			case "missing snapshot":
				delete(row, "snapshotArtifactId")
			case "different snapshot":
				req.FromDeploymentId = "different"
			case "cancel requested":
				row["cancelRequested"] = true
			case "no database gate":
				h.deps.Store.deploymentGate = nil
			case "claim write unavailable":
				h.engine.fail = map[string]error{"mutation claimPackageDeploymentConfirmation": errors.New("fixture unavailable")}
			}
			if _, err := StartConfirmation(context.Background(), h.deps, req); err == nil {
				t.Fatal("invalid/unknown acceptance launched execution")
			}
			if name != "claim write unavailable" && h.engine.sawStatement("mutation claimPackageDeploymentConfirmation") {
				t.Fatal("invalid request claimed the attempt")
			}
			if len(h.builder.built) != 0 || len(h.publisher.published) != 0 || h.fetcher.repoFetches != 0 {
				t.Fatal("refused confirmation performed work")
			}
		})
	}
}

func TestConfirmationBudgetRetainsEachSelectedBuildAndAsset(t *testing.T) {
	t.Setenv(BuildTimeoutEnv, "7200")
	report := &Report{Deployables: []DeployableReport{
		{Name: "one"}, {Name: "two", Assets: []ManifestAsset{{Path: "asset"}}},
		{Name: "prebuilt", Prebuilt: true}, {Name: "skipped"},
	}}
	got, err := confirmationTimeout(report, map[string]Placement{"skipped": {Skip: true}})
	want := 30*time.Minute + 2*(2*time.Hour+time.Minute) + sourceDownloadTimeout
	if err != nil || got != want {
		t.Fatalf("budget=%s want=%s err=%v", got, want, err)
	}
	t.Setenv(BuildTimeoutEnv, "1")
	got, err = confirmationTimeout(report, map[string]Placement{"one": {Skip: true}, "two": {Skip: true}, "skipped": {Skip: true}})
	if err != nil || got != 30*time.Minute {
		t.Fatalf("prebuilt budget=%s %v", got, err)
	}
}

type cancelSource struct {
	*fakeFetcher
	cancel context.CancelFunc
}

func (f cancelSource) FetchRepo(ctx context.Context, _ RepoSource, _ Limits) (*SourceSnapshot, error) {
	f.cancel()
	return nil, ctx.Err()
}

type terminalContextEngine struct {
	Engine
	closed  bool
	actor   string
	bounded bool
}

func (e *terminalContextEngine) Execute(ctx context.Context, q string) (*memql.ExecuteResult, error) {
	if strings.HasPrefix(q, "mutation closePackageDeployment(") {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		e.closed = true
		e.actor = actorFromContext(ctx).UserId
		deadline, ok := ctx.Deadline()
		e.bounded = ok && time.Until(deadline) > 0 && time.Until(deadline) <= 30*time.Second
	}
	return e.Engine.Execute(ctx, q)
}

// An UNCONFIRMED call without background is the one that stays on its request:
// nobody has said yes, so nothing outlives the person who asked. Its row still
// closes, on a context that outlives them.
func TestACallerLeavingAnUnconfirmedRunStillRecordsABoundedOutcome(t *testing.T) {
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	e := &terminalContextEngine{Engine: h.engine}
	h.deps.Store.engine = e
	ctx, cancel := context.WithCancel(callerCtx("v1:identity:user:someone"))
	defer cancel()
	h.deps.Fetcher = cancelSource{fakeFetcher: h.fetcher, cancel: cancel}
	out, err := Deploy(ctx, h.deps, DeployRequest{PackageId: "abc", Actor: actorFromContext(ctx)})
	if !errors.Is(err, context.Canceled) || out == nil || out.Status != StatusFailed {
		t.Fatalf("outcome=%+v err=%v", out, err)
	}
	if !e.closed || !e.bounded || e.actor != actorFromContext(ctx).UserId {
		t.Fatalf("terminal context lost auth/lifetime: %+v", e)
	}
	if len(h.builder.built) != 0 || len(h.publisher.published) != 0 {
		t.Fatal("cancelled request reached effects")
	}
}

// heldBuild stops a run in the middle of its build, which is where a confirmed
// deploy spends its minutes and so where a dropped connection lands.
type heldBuild struct {
	entered chan context.Context
	release chan struct{}
	err     error
	// ignoresContext is a stage stuck on something its context cannot reach.
	ignoresContext bool
}

func newHeldBuild() *heldBuild {
	return &heldBuild{entered: make(chan context.Context, 4), release: make(chan struct{})}
}

func (b *heldBuild) Build(ctx context.Context, _ BuildRun, _ *SourceSnapshot, _ DeployableReport) (BuildResult, error) {
	b.entered <- ctx
	if b.ignoresContext {
		<-b.release
		return BuildResult{}, ctx.Err()
	}
	select {
	case <-ctx.Done():
		return BuildResult{}, ctx.Err()
	case <-b.release:
		if b.err != nil {
			return BuildResult{}, b.err
		}
		return BuildResult{Bundle: edge.Bundle{"index.html": []byte("<!doctype html>")}}, nil
	}
}

func (b *heldBuild) await(t *testing.T) context.Context {
	t.Helper()
	select {
	case ctx := <-b.entered:
		return ctx
	case <-time.After(5 * time.Second):
		t.Fatal("the confirmed run never reached its build")
		return nil
	}
}

type deployCall struct {
	out *DeployOutcome
	err error
}

func awaitDeployCall(t *testing.T, returned <-chan deployCall) deployCall {
	t.Helper()
	select {
	case call := <-returned:
		return call
	case <-time.After(5 * time.Second):
		t.Fatal("the call did not return")
		return deployCall{}
	}
}

// confirmedCall is one synchronous confirmation, held mid-build, with the
// caller still attached. Rows are the durable link to a Cancel from another
// replica, and the audit channel is how a test sees the run end after the call
// has gone.
type confirmedCall struct {
	h        *harness
	rows     *sharedAnalysisRows
	closing  *terminalContextEngine
	build    *heldBuild
	audit    analysisAudit
	building context.Context
	leave    context.CancelFunc
	returned chan deployCall
}

func confirmMidBuild(t *testing.T, build *heldBuild) *confirmedCall {
	t.Helper()
	return confirmMidBuildWithin(t, build, 0)
}

func confirmMidBuildWithin(t *testing.T, build *heldBuild, budget time.Duration) *confirmedCall {
	t.Helper()
	t.Setenv(HeartbeatIntervalEnv, "1")
	h := newHarness(t, spaOnlyPackage(), ownerPackage())
	rows := &sharedAnalysisRows{recordingEngine: h.engine}
	closing := &terminalContextEngine{Engine: rows}
	h.deps.Store = &store{engine: closing, deploymentGate: offlineDeploymentGate}
	h.deps.Builder = build
	audit := make(analysisAudit, 1)
	h.deps.Auditor = audit
	requestCtx, leave := context.WithCancel(callerCtx("v1:identity:user:someone"))
	t.Cleanup(leave)
	c := &confirmedCall{h: h, rows: rows, closing: closing, build: build, audit: audit, leave: leave, returned: make(chan deployCall, 1)}
	go func() {
		out, err := Deploy(requestCtx, h.deps, DeployRequest{
			PackageId:           "v1:platform:package:abc",
			Actor:               plainUser(),
			Confirmed:           true,
			Placements:          firstDeployPlacements(),
			confirmationTimeout: budget,
		})
		c.returned <- deployCall{out, err}
	}()
	c.building = build.await(t)
	return c
}

func (c *confirmedCall) cancelFromAnotherReplica(t *testing.T) {
	t.Helper()
	other := NewIntegration(c.rows, discardLogger())
	other.depsOnce.Do(func() {
		other.deps = &Deps{Store: &store{engine: c.rows, deploymentGate: offlineDeploymentGate}, Logger: discardLogger()}
	})
	if _, err := other.handleCancelDeployment(callerCtx("v1:identity:user:someone"), map[string]any{"packageId": "abc", "deploymentId": "v1:platform:packageDeployment:fixed1"}, 0); err != nil {
		t.Fatal(err)
	}
}

func closeStatements(e *recordingEngine) []string {
	var closes []string
	for _, q := range e.statements() {
		if strings.HasPrefix(q, "mutation closePackageDeployment(") {
			closes = append(closes, q)
		}
	}
	return closes
}

// The incident this answers: a missed websocket ping cancelled the stream's
// context, that context was the run's, and a build that was going fine died of
// it. Saying yes at the gate hands the run to the cluster, so the caller going
// away ends the WAIT and nothing else.
func TestACallerLeavingMidBuildLeavesTheConfirmedDeployRunning(t *testing.T) {
	c := confirmMidBuild(t, newHeldBuild())
	c.leave()

	call := awaitDeployCall(t, c.returned)
	if !errors.Is(call.err, context.Canceled) {
		t.Fatalf("a caller that left is told so, got %v", call.err)
	}
	if call.out == nil || call.out.DeploymentId == "" {
		t.Fatalf("the call must still name the run it leaves behind: %+v", call.out)
	}
	if c.building.Err() != nil {
		t.Fatalf("the caller leaving reached the build: %v", c.building.Err())
	}
	if actorFromContext(c.building).UserId != "v1:identity:user:someone" {
		t.Fatal("detaching from the caller lost the caller's authority")
	}

	close(c.build.release)
	if event := awaitAnalysis(t, c.audit); event.Status != StatusSucceeded {
		t.Fatalf("the run must reach its normal end: %+v", event)
	}
	closes := closeStatements(c.h.engine)
	if len(closes) != 1 || !strings.Contains(closes[0], `status: "`+StatusSucceeded+`"`) {
		t.Fatalf("the row must close succeeded, once: %v", closes)
	}
	if len(c.h.publisher.published) == 0 {
		t.Fatal("the run reached its end without publishing")
	}
}

// The other half of the lifetime: a caller still waiting gets the run's own
// outcome, exactly as a synchronous call always has.
func TestACallerThatStaysGetsTheConfirmedRunsOutcome(t *testing.T) {
	c := confirmMidBuild(t, newHeldBuild())
	close(c.build.release)
	call := awaitDeployCall(t, c.returned)
	if call.err != nil || call.out == nil || call.out.Status != StatusSucceeded {
		t.Fatalf("outcome=%+v err=%v", call.out, call.err)
	}
}

// Detaching keeps the one stop that was always a person's decision. The durable
// flag is what stops the run, from any replica, whether or not the caller that
// confirmed it is still there.
func TestCancelStillStopsAConfirmedDeploy(t *testing.T) {
	for name, callerLeft := range map[string]bool{"caller still waiting": false, "caller gone": true} {
		t.Run(name, func(t *testing.T) {
			c := confirmMidBuild(t, newHeldBuild())
			if callerLeft {
				c.leave()
				awaitDeployCall(t, c.returned)
				if c.building.Err() != nil {
					t.Fatalf("the caller leaving reached the build: %v", c.building.Err())
				}
			}
			c.cancelFromAnotherReplica(t)
			if event := awaitAnalysis(t, c.audit); event.Status != StatusCancelled {
				t.Fatalf("Cancel must stop the run as cancelled: %+v", event)
			}
			if c.building.Err() == nil {
				t.Fatal("Cancel must reach the build in flight")
			}
			closes := closeStatements(c.h.engine)
			if len(closes) != 1 || !strings.Contains(closes[0], `status: "`+StatusCancelled+`"`) {
				t.Fatalf("the row must close cancelled, once: %v", closes)
			}
			if len(c.h.publisher.published) != 0 {
				t.Fatal("a cancelled run published")
			}
			if !callerLeft {
				call := awaitDeployCall(t, c.returned)
				if RefusalCode(call.err) != CodeDeploymentCancelled || call.out == nil || call.out.Status != StatusCancelled {
					t.Fatalf("a waiting caller is told it was cancelled: %+v %v", call.out, call.err)
				}
			}
		})
	}
}

// A run that really fails after its caller left closes its own row with what
// went wrong, and does not leave it for the sweep to describe.
func TestAConfirmedDeployThatFailsAfterItsCallerLeftClosesItsOwnRow(t *testing.T) {
	build := newHeldBuild()
	build.err = refuseScoped(CodeDeployableBuildFailed, "docs", "npm ci exited 1")
	c := confirmMidBuild(t, build)
	c.leave()
	awaitDeployCall(t, c.returned)

	close(build.release)
	if event := awaitAnalysis(t, c.audit); event.Status != StatusRefused || event.FailureReason != CodeDeployableBuildFailed {
		t.Fatalf("the audit must carry the real failure: %+v", event)
	}
	closes := closeStatements(c.h.engine)
	if len(closes) != 1 || !strings.Contains(closes[0], "npm ci exited 1") {
		t.Fatalf("the row must close with the build's own error: %v", closes)
	}
	if strings.Contains(closes[0], "context canceled") {
		t.Fatalf("the caller leaving must not be what the row reports: %s", closes[0])
	}
	if !c.closing.closed || !c.closing.bounded || c.closing.actor != "v1:identity:user:someone" {
		t.Fatalf("the close must run bounded, under the caller's authority: %+v", c.closing)
	}
}

func heartbeatsWritten(e *recordingEngine) int {
	n := 0
	for _, q := range e.statements() {
		if strings.HasPrefix(q, "mutation heartbeatPackageDeployment(") {
			n++
		}
	}
	return n
}

// The sweep reads a stopped heartbeat as a stopped run, so the heartbeat has to
// live as long as the run does and not as long as the call that started it.
func TestAConfirmedDeployKeepsItsHeartbeatAfterItsCallerLeft(t *testing.T) {
	c := confirmMidBuild(t, newHeldBuild())
	c.leave()
	awaitDeployCall(t, c.returned)
	left := heartbeatsWritten(c.h.engine)
	deadline := time.Now().Add(5 * time.Second)
	for heartbeatsWritten(c.h.engine) <= left {
		if time.Now().After(deadline) {
			t.Fatal("the heartbeat stopped with the caller, so the sweep would close a healthy run")
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(c.build.release)
	awaitAnalysis(t, c.audit)
}

// The budget is what stands where the caller's lifetime used to. A stage that
// honours it ends the run, and the row says the run ran out of time.
func TestAConfirmedRunPastItsBudgetClosesItsOwnRow(t *testing.T) {
	c := confirmMidBuildWithin(t, newHeldBuild(), 100*time.Millisecond)
	call := awaitDeployCall(t, c.returned)
	if !strings.Contains(call.err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("the caller is told the run ran out of time: %v", call.err)
	}
	closes := closeStatements(c.h.engine)
	if len(closes) != 1 || !strings.Contains(closes[0], context.DeadlineExceeded.Error()) {
		t.Fatalf("the row must say the run ran out of time: %v", closes)
	}
	if !c.closing.closed || !c.closing.bounded {
		t.Fatalf("the close must run on a live, bounded context past the budget: %+v", c.closing)
	}
}

// And a stage that ignores it cannot keep the row alive forever: the heartbeat
// ends with the budget, which is what lets the sweep close the row.
func TestAStageStuckPastTheBudgetStopsTheHeartbeat(t *testing.T) {
	build := newHeldBuild()
	build.ignoresContext = true
	c := confirmMidBuildWithin(t, build, 1500*time.Millisecond)
	<-c.building.Done()
	settled := heartbeatsWritten(c.h.engine)
	// Long enough for two beats of a live heartbeat. One beat is forgiven: a tick
	// that landed as the budget ran out can still be on its way.
	time.Sleep(2200 * time.Millisecond)
	if got := heartbeatsWritten(c.h.engine); got > settled+1 {
		t.Fatalf("the heartbeat outlived the budget: %d beats, then %d", settled, got)
	}
	close(build.release)
	awaitDeployCall(t, c.returned)
}

// Detaching must not unbound the run. A confirmation with a saved report gets
// the budget that report asks for, and one with none gets a budget that covers
// what the package declares and leaves room for what it has yet to declare.
func TestADetachedConfirmationIsStillBounded(t *testing.T) {
	t.Setenv(BuildTimeoutEnv, "60")
	t.Run("no saved report", func(t *testing.T) {
		c := confirmMidBuild(t, newHeldBuild())
		deadline, ok := c.building.Deadline()
		want := confirmationTimeoutWithoutReport(ownerPackage())
		if !ok || time.Until(deadline) > want || time.Until(deadline) < want-time.Minute {
			t.Fatalf("deadline in %s, want about %s", time.Until(deadline), want)
		}
		close(c.build.release)
		awaitDeployCall(t, c.returned)
	})
	t.Run("a saved report", func(t *testing.T) {
		h := newHarness(t, spaOnlyPackage(), ownerPackage())
		build := newHeldBuild()
		h.deps.Builder = build
		rep, err := Analyze(spaOnlyPackage(), Options{SourceVersion: "sha-abc123"})
		if err != nil {
			t.Fatal(err)
		}
		h.engine.rows["query packageDeploymentById"] = []map[string]any{
			parkedRun("saved", "abc", map[string]any{"report": reportMap(t, rep)}),
		}
		returned := make(chan deployCall, 1)
		go func() {
			out, err := Deploy(context.Background(), h.deps, DeployRequest{PackageId: "abc", DeploymentId: "saved", Confirmed: true, Actor: plainUser(), Placements: firstDeployPlacements()})
			returned <- deployCall{out, err}
		}()
		building := build.await(t)
		want, err := confirmationTimeout(rep, firstDeployPlacements())
		if err != nil {
			t.Fatal(err)
		}
		deadline, ok := building.Deadline()
		if !ok || time.Until(deadline) > want || time.Until(deadline) < want-time.Minute {
			t.Fatalf("deadline in %s, want about %s", time.Until(deadline), want)
		}
		close(build.release)
		awaitDeployCall(t, returned)
	})
}

func TestAConfirmationWithoutAReportIsBudgetedForWhatItDeclaresAndMore(t *testing.T) {
	t.Setenv(BuildTimeoutEnv, "60")
	perBuild := 2 * time.Minute
	if got, want := confirmationTimeoutWithoutReport(ownerPackage()), 30*time.Minute+unreportedConfirmationBuilds*perBuild; got != want {
		t.Fatalf("an undeclared source: got %s, want %s", got, want)
	}
	declares := make([]any, 0, unreportedConfirmationBuilds+2)
	for range unreportedConfirmationBuilds + 2 {
		declares = append(declares, map[string]any{"name": "app"})
	}
	pkg := ownerPackage()
	pkg["declares"] = declares
	if got, want := confirmationTimeoutWithoutReport(pkg), 30*time.Minute+(unreportedConfirmationBuilds+2)*perBuild; got != want {
		t.Fatalf("a source declaring more: got %s, want %s", got, want)
	}
}
