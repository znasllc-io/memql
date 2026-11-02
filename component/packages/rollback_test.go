package packages

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	rollbackTarget = "v1:platform:packageDeployment:old"
	priorPrefix    = "packages/acme/0123456789abcdef/"
	newerPrefix    = "packages/acme/ffffffffffffffff/"
)

// The surfaces a rollback moves answer a dead context with its error, as the
// engine, the blob upload and the Kubernetes client do. That is what stopped a
// rollback partway when its stream closed, and the pipeline's own fakes ignore it.
type liveStager struct {
	*fakeStager
	writeErr error
}

func (s liveStager) ReadActiveSet(ctx context.Context) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.fakeStager.ReadActiveSet(ctx)
}

func (s liveStager) WriteActiveSet(ctx context.Context, set map[string]string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.writeErr != nil {
		return s.writeErr
	}
	return s.fakeStager.WriteActiveSet(ctx, set)
}

type liveRoller struct{ *fakeRoller }

func (r liveRoller) Roll(ctx context.Context, reason string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.fakeRoller.Roll(ctx, reason)
}

type livePublisher struct{ *fakePublisher }

func (p livePublisher) RepointSite(ctx context.Context, siteId, bundleRef string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return p.fakePublisher.RepointSite(ctx, siteId, bundleRef)
}

// heldRepoint stops a rollback at its first site, as a slow write would.
type heldRepoint struct {
	livePublisher
	entered chan context.Context
	release chan struct{}
	// err, when set, is what the write answers once released.
	err error
}

func (p heldRepoint) RepointSite(ctx context.Context, siteId, bundleRef string) error {
	p.entered <- ctx
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.release:
		if p.err != nil {
			return p.err
		}
		return p.livePublisher.RepointSite(ctx, siteId, bundleRef)
	}
}

func (p heldRepoint) await(t *testing.T) context.Context {
	t.Helper()
	select {
	case ctx := <-p.entered:
		return ctx
	case <-time.After(5 * time.Second):
		t.Fatal("the rollback never reached its first site")
		return nil
	}
}

// rollbackEnding is one audit event, with the state of the context it was written
// on: the caller's is dead by then, and a budget's can be.
type rollbackEnding struct {
	event   RollbackAuditEvent
	ctxErr  error
	bounded bool
	actor   string
}

type rollbackAudit chan rollbackEnding

func (a rollbackAudit) Deploy(context.Context, DeployAuditEvent) {}

func (a rollbackAudit) Rollback(ctx context.Context, ev RollbackAuditEvent) {
	deadline, ok := ctx.Deadline()
	a <- rollbackEnding{
		event:   ev,
		ctxErr:  ctx.Err(),
		bounded: ok && time.Until(deadline) <= 30*time.Second,
		actor:   actorFromContext(ctx).UserId,
	}
}

type rollbackReturn struct {
	restored *RestoredState
	err      error
}

// rollbackCall is one rollback of a two-site package carrying DSL, from a caller
// who can leave, on a cluster currently on a newer prefix so the pointer has to
// move.
type rollbackCall struct {
	h        *harness
	ctx      context.Context
	leave    context.CancelFunc
	returned chan rollbackReturn
	req      RollbackRequest
	audit    rollbackAudit
	logs     *bytes.Buffer
}

func newRollbackCall(t *testing.T) *rollbackCall {
	t.Helper()
	h := newHarness(t, validPackage(), ownerPackage())
	h.engine.rows["query packageDeploymentById"] = []map[string]any{{
		"id":         rollbackTarget,
		"packageId":  "v1:platform:package:abc",
		"status":     StatusSucceeded,
		"dslVersion": priorPrefix,
		"deployables": []any{
			map[string]any{"name": "storefront", "siteId": "v1:platform:site:s1", "bundleRef": "blob://sites/s1/v1/"},
			map[string]any{"name": "docs", "siteId": "v1:platform:site:s2", "bundleRef": "blob://sites/s2/v1/"},
		},
	}}
	h.stager.active = map[string]string{"acme": newerPrefix}
	h.deps.Stager = liveStager{fakeStager: h.stager}
	h.deps.Roller = liveRoller{h.roller}
	h.deps.Publisher = livePublisher{h.publisher}
	audit := make(rollbackAudit, 2)
	h.deps.Auditor = audit
	logger, logs := capturedLog()
	h.deps.Logger = logger
	ctx, leave := context.WithCancel(callerCtx("v1:identity:user:owner"))
	t.Cleanup(leave)
	return &rollbackCall{
		h:        h,
		ctx:      ctx,
		leave:    leave,
		returned: make(chan rollbackReturn, 1),
		req:      RollbackRequest{PackageId: "v1:platform:package:abc", DeploymentId: rollbackTarget, Actor: mayDeployDsl()},
		audit:    audit,
		logs:     logs,
	}
}

func (r *rollbackCall) start() {
	go func() {
		restored, err := Rollback(r.ctx, r.h.deps, r.req)
		r.returned <- rollbackReturn{restored, err}
	}()
}

// holdAtFirstSite starts the rollback and returns once it is stopped at its first
// site, with the context it is running on.
func (r *rollbackCall) holdAtFirstSite(t *testing.T) (context.Context, chan struct{}) {
	t.Helper()
	held := heldRepoint{livePublisher: livePublisher{r.h.publisher}, entered: make(chan context.Context, 4), release: make(chan struct{})}
	r.h.deps.Publisher = held
	r.start()
	return held.await(t), held.release
}

func (r *rollbackCall) awaitReturn(t *testing.T) rollbackReturn {
	t.Helper()
	select {
	case call := <-r.returned:
		return call
	case <-time.After(5 * time.Second):
		t.Fatal("the call did not return")
		return rollbackReturn{}
	}
}

// awaitEnding is how a test sees a rollback end after its call has gone: the audit
// event is the last thing it does.
func (r *rollbackCall) awaitEnding(t *testing.T) rollbackEnding {
	t.Helper()
	select {
	case ending := <-r.audit:
		return ending
	case <-time.After(5 * time.Second):
		t.Fatal("the rollback recorded no ending")
		return rollbackEnding{}
	}
}

// logLine is the one line at this level, read after the rollback has ended.
func (r *rollbackCall) logLine(t *testing.T, level string) string {
	t.Helper()
	var found []string
	for _, line := range strings.Split(r.logs.String(), "\n") {
		if strings.Contains(line, "level="+level) {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want one %s line, got %d:\n%s", level, len(found), r.logs.String())
	}
	return found[0]
}

// The window this exists for: every site is back and the pointer has not moved.
// A connection dropped here used to leave the two apart, serving the old app on
// the new DSL with nothing left running to finish the job.
func TestACallerLeavingBetweenTheSitesAndThePointerLeavesTheRollbackRunning(t *testing.T) {
	r := newRollbackCall(t)
	r.h.publisher.onRepoint = func() {
		if len(r.h.publisher.repointed) == 2 {
			r.leave()
		}
	}
	rolled := make(chan struct{})
	r.h.roller.onRoll = func() { close(rolled) }
	r.start()

	select {
	case <-rolled:
	case <-time.After(5 * time.Second):
		t.Fatalf("the caller leaving stopped the rollback with the sites back and the DSL not: pointer %q, %d rolls",
			r.h.stager.active["acme"], r.h.roller.rolls)
	}
	if got := r.h.stager.active["acme"]; got != priorPrefix {
		t.Fatalf("the pointer must be back on the prior prefix, got %q", got)
	}
	if len(r.h.publisher.repointed) != 2 || r.h.roller.rolls != 1 {
		t.Fatalf("the rollback must reach its end once: repointed %v, %d rolls", r.h.publisher.repointed, r.h.roller.rolls)
	}
}

// The other half of the same lifetime: leaving ends the wait straight away, the
// rollback goes on under the caller's authority, and the reply claims nothing about
// an ending the caller will not see.
func TestACallerLeavingARollbackEndsOnlyTheWait(t *testing.T) {
	r := newRollbackCall(t)
	running, release := r.holdAtFirstSite(t)
	r.leave()

	call := r.awaitReturn(t)
	if running.Err() != nil {
		t.Fatalf("the caller leaving reached the rollback: %v", running.Err())
	}
	if !errors.Is(call.err, context.Canceled) {
		t.Fatalf("a caller that left is told so, got %v", call.err)
	}
	if call.restored == nil || call.restored.DeploymentId != rollbackTarget {
		t.Fatalf("the call must still name the rollback it leaves behind: %+v", call.restored)
	}
	if len(call.restored.Deployables) != 0 || call.restored.PointerMoved || call.restored.Rolled {
		t.Fatalf("the reply must claim nothing about how it ended: %+v", call.restored)
	}
	if actorFromContext(running).UserId != "v1:identity:user:owner" {
		t.Fatal("detaching from the caller lost the caller's authority")
	}

	close(release)
	ending := r.awaitEnding(t)
	if ending.event.Outcome != "success" {
		t.Fatalf("the rollback must reach its normal end: %+v", ending.event)
	}
	if got := r.h.stager.active["acme"]; got != priorPrefix || r.h.roller.rolls != 1 {
		t.Fatalf("pointer %q, %d rolls", got, r.h.roller.rolls)
	}
}

// A caller still waiting gets the rollback's own outcome, as it always did, and that
// includes how far a rollback that stopped had got.
func TestACallerThatStaysGetsTheRollbacksOutcome(t *testing.T) {
	t.Run("finished", func(t *testing.T) {
		r := newRollbackCall(t)
		r.start()
		call := r.awaitReturn(t)
		if call.err != nil || call.restored == nil {
			t.Fatalf("outcome=%+v err=%v", call.restored, call.err)
		}
		if len(call.restored.Deployables) != 2 || !call.restored.PointerMoved || !call.restored.Rolled {
			t.Fatalf("the reply must say what was put back: %+v", call.restored)
		}
	})
	t.Run("stopped at the pointer", func(t *testing.T) {
		r := newRollbackCall(t)
		refused := errors.New("the storage account said no")
		r.h.deps.Stager = liveStager{fakeStager: r.h.stager, writeErr: refused}
		r.start()
		call := r.awaitReturn(t)
		if !errors.Is(call.err, refused) {
			t.Fatalf("the caller must get the real error, got %v", call.err)
		}
		if call.restored == nil || len(call.restored.Deployables) != 2 || call.restored.PointerMoved || call.restored.Rolled {
			t.Fatalf("the reply must say how far it got: %+v", call.restored)
		}
	})
}

// A failure nobody is waiting for has to land somewhere a person can read it, as
// itself rather than as the connection that closed. The log line is what the Logs
// button on the attempt opens, and that button filters on the bare id. The trail is
// what is still there after the logs have expired.
func TestARollbackThatFailsAfterItsCallerLeftReportsTheRealError(t *testing.T) {
	r := newRollbackCall(t)
	r.h.roller.err = errors.New("the api server said no")
	_, release := r.holdAtFirstSite(t)
	r.leave()
	r.awaitReturn(t)
	close(release)

	ending := r.awaitEnding(t)
	ev := ending.event
	if ev.Outcome != "failure" || ev.FailureReason != "the api server said no" {
		t.Fatalf("the trail must carry the real failure: %+v", ev)
	}
	if len(ev.Sites) != 2 || !ev.PointerMoved || ev.Rolled {
		t.Fatalf("the trail must say how far it got: %+v", ev)
	}
	if ev.PackageId != r.req.PackageId || ev.DeploymentId != rollbackTarget || ev.DslVersion != priorPrefix {
		t.Fatalf("the trail must name what was being put back: %+v", ev)
	}
	if ending.ctxErr != nil || !ending.bounded || ending.actor != "v1:identity:user:owner" {
		t.Fatalf("the trail must be written on a live, bounded context, under the caller's authority: %+v", ending)
	}

	line := r.logLine(t, "ERROR")
	for _, want := range []string{"subject.id=old", "subject.concept=" + packageDeploymentConcept, "the api server said no"} {
		if !strings.Contains(line, want) {
			t.Fatalf("the log line must have %q: %s", want, line)
		}
	}
	if strings.Contains(line, context.Canceled.Error()) {
		t.Fatalf("the caller leaving must not be what the log reports: %s", line)
	}
}

// Detaching must not unbound it. A rollback that can land long after somebody gave
// up on it can land on top of whatever they did since.
func TestADetachedRollbackIsStillBounded(t *testing.T) {
	r := newRollbackCall(t)
	running, release := r.holdAtFirstSite(t)
	deadline, ok := running.Deadline()
	if !ok || time.Until(deadline) > rollbackBudget || time.Until(deadline) < rollbackBudget-time.Minute {
		t.Fatalf("deadline in %s, want about %s", time.Until(deadline), rollbackBudget)
	}
	close(release)
	r.awaitEnding(t)
}

// The budget is what stands where the caller's lifetime was. A stage that honours
// it ends the rollback, and the record says it ran out of time, on a context that
// outlives the budget.
func TestARollbackPastItsBudgetSaysSo(t *testing.T) {
	r := newRollbackCall(t)
	r.req.budget = 100 * time.Millisecond
	running, _ := r.holdAtFirstSite(t)
	select {
	case <-running.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the budget never ended the rollback")
	}

	call := r.awaitReturn(t)
	if !errors.Is(call.err, context.DeadlineExceeded) {
		t.Fatalf("the caller is told it ran out of time: %v", call.err)
	}
	ending := r.awaitEnding(t)
	if ending.event.Outcome != "failure" || !strings.Contains(ending.event.FailureReason, context.DeadlineExceeded.Error()) {
		t.Fatalf("the trail must say it ran out of time: %+v", ending.event)
	}
	if ending.ctxErr != nil || !ending.bounded {
		t.Fatalf("the trail must be written on a live, bounded context past the budget: %+v", ending)
	}
	if !strings.Contains(r.logLine(t, "ERROR"), context.DeadlineExceeded.Error()) {
		t.Fatalf("the log must say it ran out of time:\n%s", r.logs.String())
	}
}

// A refusal is a decision, and the D9 one is the same decision a deploy's audit
// records. Nothing has moved, so the trail says blocked and not failure.
func TestARollbackRefusedBeforeAnythingMovedIsOnTheTrail(t *testing.T) {
	r := newRollbackCall(t)
	r.req.Actor = plainUser()
	r.start()
	call := r.awaitReturn(t)
	if RefusalCode(call.err) != CodeDslRequiresAuthoring || call.restored != nil {
		t.Fatalf("outcome=%+v err=%v", call.restored, call.err)
	}
	ev := r.awaitEnding(t).event
	if ev.Outcome != "blocked" || !strings.HasPrefix(ev.FailureReason, CodeDslRequiresAuthoring+":") {
		t.Fatalf("the trail must say it was refused: %+v", ev)
	}
	if len(ev.Sites) != 0 || ev.PointerMoved || ev.Rolled {
		t.Fatalf("a refusal moves nothing: %+v", ev)
	}
}

// Until something has moved, the caller's lifetime still decides. Going before the
// last read is going before anything changed, so nothing starts behind them.
func TestACallerGoneBeforeAnythingMovedStopsTheRollback(t *testing.T) {
	r := newRollbackCall(t)
	r.leave()
	r.start()
	call := r.awaitReturn(t)
	if !errors.Is(call.err, context.Canceled) || call.restored != nil {
		t.Fatalf("outcome=%+v err=%v", call.restored, call.err)
	}
	if ev := r.awaitEnding(t).event; ev.Outcome != "failure" || len(ev.Sites) != 0 {
		t.Fatalf("the trail must say it stopped with nothing moved: %+v", ev)
	}
	if len(r.h.publisher.repointed) != 0 || r.h.stager.written != 0 || r.h.roller.rolls != 0 {
		t.Fatalf("nothing may have moved: %v, %d writes, %d rolls", r.h.publisher.repointed, r.h.stager.written, r.h.roller.rolls)
	}
}

// If the process dies nothing closes a rollback, so its lines are all that says how
// far it got. Each thing it moves gets one, in the order it moved them, under the
// subject the Logs button on the attempt reads.
func TestARollbackLeavesALineForEachThingItMoved(t *testing.T) {
	r := newRollbackCall(t)
	r.start()
	if call := r.awaitReturn(t); call.err != nil {
		t.Fatal(call.err)
	}
	r.awaitEnding(t)

	var steps []string
	for _, line := range strings.Split(strings.TrimSpace(r.logs.String()), "\n") {
		if !strings.Contains(line, "subject.id=old") {
			t.Fatalf("a line of this rollback is about something else: %s", line)
		}
		for _, step := range []string{"rolling back to this deployment", "site=v1:platform:site:s1", "site=v1:platform:site:s2", "moved the DSL pointer back", "asked for the roll"} {
			if strings.Contains(line, step) {
				steps = append(steps, step)
				// The store keeps debug lines only on a node set up to.
				if !strings.Contains(line, "level=INFO") {
					t.Fatalf("this line would not reach the store: %s", line)
				}
			}
		}
	}
	want := "rolling back to this deployment,site=v1:platform:site:s1,site=v1:platform:site:s2,moved the DSL pointer back,asked for the roll"
	if got := strings.Join(steps, ","); got != want {
		t.Fatalf("the lines must follow what moved:\n got %s\nwant %s\n%s", got, want, r.logs.String())
	}
}

// A rollback that finished says so in both places, because the person who pressed
// it may not have been there to read the reply. One with no DSL has no pointer and
// no roll to report.
func TestAFinishedRollbackIsOnTheTrailAndInItsLogs(t *testing.T) {
	for name, tc := range map[string]struct {
		dslVersion string
		moved      bool
	}{"with DSL": {priorPrefix, true}, "without DSL": {"", false}} {
		t.Run(name, func(t *testing.T) {
			r := newRollbackCall(t)
			r.h.engine.rows["query packageDeploymentById"][0]["dslVersion"] = tc.dslVersion
			r.start()
			if call := r.awaitReturn(t); call.err != nil {
				t.Fatal(call.err)
			}
			ev := r.awaitEnding(t).event
			if ev.Outcome != "success" || ev.FailureReason != "" {
				t.Fatalf("the trail must say it finished: %+v", ev)
			}
			if len(ev.Sites) != 2 || ev.PointerMoved != tc.moved || ev.Rolled != tc.moved {
				t.Fatalf("the trail must say what was put back: %+v", ev)
			}
			if !strings.Contains(r.logs.String(), "packages: rolled back to this deployment") {
				t.Fatalf("the logs must say it finished:\n%s", r.logs.String())
			}
		})
	}
}

// TestRollbackExecutesTheReversedOrder is the D6 law's other half, and the one
// that is easy to get symmetrically wrong: rolling back in the FORWARD order
// would leave new application code talking to old DSL for the width of the
// rollout, which is the exact window the ordering exists to close.
func TestRollbackExecutesTheReversedOrder(t *testing.T) {
	prior := map[string]any{
		"id":         "v1:platform:packageDeployment:old",
		"packageId":  "v1:platform:package:abc",
		"status":     StatusSucceeded,
		"dslVersion": "packages/acme/0123456789abcdef/",
		"deployables": []any{
			map[string]any{"name": "storefront", "siteId": "v1:platform:site:s1", "bundleRef": "blob://sites/s1/v1/"},
			map[string]any{"name": "docs", "siteId": "v1:platform:site:s2", "bundleRef": "blob://sites/s2/v1/"},
		},
	}
	h := newHarness(t, validPackage(), ownerPackage())
	h.engine.rows["query packageDeploymentById"] = []map[string]any{prior}
	// The cluster is currently on a NEWER prefix, so the pointer has to move.
	h.stager.active = map[string]string{"acme": "packages/acme/ffffffffffffffff/"}

	// Order is observed through a single log the fakes share.
	var order []string
	h.publisher.onRepoint = func() { order = append(order, "publish") }
	h.stager.onWrite = func() { order = append(order, "pointer") }
	h.roller.onRoll = func() { order = append(order, "roll") }

	res, err := Rollback(context.Background(), h.deps, RollbackRequest{
		PackageId:    "v1:platform:package:abc",
		DeploymentId: "v1:platform:packageDeployment:old",
		Actor:        mayDeployDsl(),
	})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}

	want := []string{"publish", "publish", "pointer", "roll"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("rollback must publish back FIRST, then move the pointer, then roll.\n got %v\nwant %v", order, want)
	}
	if !res.Rolled || res.DslVersion != "packages/acme/0123456789abcdef/" {
		t.Fatalf("restored state wrong: %+v", res)
	}
	if len(h.publisher.repointed) != 2 {
		t.Fatalf("both sites must be repointed: %v", h.publisher.repointed)
	}
	if got := h.stager.active["acme"]; got != "packages/acme/0123456789abcdef/" {
		t.Fatalf("the pointer must be back on the prior prefix, got %q", got)
	}
}

func TestRollbackRefusesADeploymentThatWasNeverLive(t *testing.T) {
	h := newHarness(t, validPackage(), ownerPackage())
	h.engine.rows["query packageDeploymentById"] = []map[string]any{{
		"id":        "v1:platform:packageDeployment:bad",
		"packageId": "v1:platform:package:abc",
		"status":    StatusRefused,
	}}
	if _, err := Rollback(context.Background(), h.deps, RollbackRequest{
		PackageId:    "v1:platform:package:abc",
		DeploymentId: "v1:platform:packageDeployment:bad",
		Actor:        mayDeployDsl(),
	}); err == nil {
		t.Fatal("a refused deployment carries no state to restore")
	}
}

func TestRollbackToADslVersionNeedsAuthoring(t *testing.T) {
	h := newHarness(t, validPackage(), ownerPackage())
	h.engine.rows["query packageDeploymentById"] = []map[string]any{{
		"id":         "v1:platform:packageDeployment:old",
		"packageId":  "v1:platform:package:abc",
		"status":     StatusSucceeded,
		"dslVersion": "packages/acme/0123456789abcdef/",
	}}
	_, err := Rollback(context.Background(), h.deps, RollbackRequest{
		PackageId:    "v1:platform:package:abc",
		DeploymentId: "v1:platform:packageDeployment:old",
		Actor:        plainUser(),
	})
	if RefusalCode(err) != CodeDslRequiresAuthoring {
		t.Fatalf("putting a DSL version back changes what the cluster can do; want %s, got %v",
			CodeDslRequiresAuthoring, err)
	}
	// Nothing may have moved.
	if len(h.publisher.repointed) != 0 || h.stager.written != 0 || h.roller.rolls != 0 {
		t.Fatalf("the gate must fire before anything moves")
	}
}
