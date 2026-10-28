package packages

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/deploycontrol"
	"github.com/znasllc-io/memql/component/edge"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/azureblob"
)

type continuationStager struct {
	*fakeStager
	mu   sync.Mutex
	etag int
	lost bool
}

func (s *continuationStager) ReadActiveSetVersion(context.Context) (ActiveSetVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, _ := json.Marshal(s.active)
	return ActiveSetVersion{Exists: true, ETag: fmt.Sprintf("\"%d\"", s.etag), Data: raw, Domains: maps.Clone(s.active)}, nil
}
func (s *continuationStager) CompareAndSwapActiveSet(ctx context.Context, before ActiveSetVersion, next map[string]string) (ActiveSetVersion, error) {
	s.mu.Lock()
	if before.ETag != fmt.Sprintf("\"%d\"", s.etag) {
		s.mu.Unlock()
		return ActiveSetVersion{}, azureblob.ErrPackageActiveSetChanged
	}
	s.active = maps.Clone(next)
	s.etag++
	s.written++
	lost := s.lost
	s.mu.Unlock()
	if lost {
		return ActiveSetVersion{}, azureblob.ErrBlobCommitUncertain
	}
	return s.ReadActiveSetVersion(ctx)
}

type continuationRoller struct {
	mu             sync.Mutex
	applied, ready bool
	calls          int
	lost           bool
}

func (r *continuationRoller) Roll(context.Context, string) error {
	return errors.New("legacy rollout must not be used")
}
func (r *continuationRoller) PrepareRoll(_ context.Context, marker string) ([]deploycontrol.PackageRolloutIntent, error) {
	return []deploycontrol.PackageRolloutIntent{{Namespace: "test", Name: "bff", UID: "fixed-owned-bff", Marker: marker, BeforeTemplateSHA256: strings.Repeat("1", 64), AfterTemplateSHA256: strings.Repeat("2", 64), Replicas: 2}}, nil
}
func (r *continuationRoller) ObserveRoll(context.Context, deploycontrol.PackageRolloutIntent) (deploycontrol.PackageRolloutObservation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return deploycontrol.PackageRolloutObservation{Applied: r.applied, Ready: r.applied && r.ready}, nil
}
func (r *continuationRoller) ApplyRoll(context.Context, deploycontrol.PackageRolloutIntent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.applied = true
	if r.lost {
		return errors.New("lost rollout reply")
	}
	return nil
}

type continuationPublisher struct {
	SitePublisher
	writer                  packageSiteWriter
	prepared, published     atomic.Int32
	lost                    atomic.Bool
	corrupt                 atomic.Bool
	stager                  *continuationStager
	driftDuringVerification atomic.Bool
}

func (p *continuationPublisher) PrepareBundle(_ context.Context, site string, b edge.Bundle, target edge.Target) (PreparedSiteBundle, error) {
	p.prepared.Add(1)
	site = memql.BareShortId(site)
	sum := sha256.Sum256(b["index.html"])
	return PreparedSiteBundle{SiteID: site, Target: target, Version: "v111111111111", BundleRef: "blob://sites/" + site + "/v111111111111/", Files: []PreparedSiteFile{{Name: "index.html", Receipt: azureblob.VerifiedBlob{Size: int64(len(b["index.html"])), SHA256: hex.EncodeToString(sum[:]), ETag: "\"owned-output\""}}}}, nil
}
func (p *continuationPublisher) PublishPrepared(ctx context.Context, b PreparedSiteBundle, expected ActiveSetVersion) (PublishResult, error) {
	if p.corrupt.Load() {
		return PublishResult{}, errors.New("retained output changed")
	}
	if p.driftDuringVerification.Swap(false) {
		p.stager.mu.Lock()
		p.stager.etag++
		p.stager.mu.Unlock()
	}
	active, err := p.stager.ReadActiveSetVersion(ctx)
	if err != nil || !sameActiveVersion(active, expected) {
		return PublishResult{}, fmt.Errorf("active pointer changed during verification")
	}
	p.published.Add(1)
	if err := p.writer.PointPrepared(ctx, b); err != nil {
		return PublishResult{}, err
	}
	if p.lost.Swap(false) {
		return PublishResult{}, errors.New("lost committed publication reply")
	}
	return PublishResult{SiteId: b.SiteID, Version: b.Version, BundleRef: b.BundleRef}, nil
}

type continuationFixture struct {
	base       *confirmationFixture
	stager     *continuationStager
	roller     *continuationRoller
	publishers [2]*continuationPublisher
}

func newContinuationFixture(t *testing.T) *continuationFixture {
	return newContinuationFixtureSource(t, validPackage())
}

func newContinuationFixtureSource(t *testing.T, source fs.FS) *continuationFixture {
	t.Helper()
	f := newConfirmationFixtureSource(t, source)
	c := &continuationFixture{base: f, stager: &continuationStager{fakeStager: f.h.stager, etag: 1}, roller: &continuationRoller{}}
	for index, d := range f.deps {
		d.Stager = c.stager
		d.Roller = c.roller
		d.Roles = func(context.Context, string) (auth.Role, error) { return auth.RoleOwner, nil }
		p := &continuationPublisher{SitePublisher: f.h.publisher, stager: c.stager, writer: packageSiteWriter{publisher: &enginePublisher{engine: d.Store.engine, store: d.Store}}}
		d.Publisher = p
		c.publishers[index] = p
	}
	t.Cleanup(func() {
		db, _ := f.deps[0].Store.continuationDB()
		if db != nil {
			_, _ = db.ExecContext(context.Background(), `DELETE FROM package_deployment_continuations WHERE deployment_id=$1`, memql.BareShortId(f.run))
		}
	})
	return c
}
func (c *continuationFixture) start(t *testing.T) {
	t.Helper()
	f := c.base
	out, err := Deploy(f.ctx, f.deps[0], DeployRequest{PackageId: f.pkg, DeploymentId: f.run, Confirmed: true, Actor: actorFromContext(f.ctx), Placements: map[string]Placement{"storefront": {Skip: true}}})
	if err != nil || out == nil || out.Status != StatusRolling {
		t.Fatalf("checkpoint: %+v %v", out, err)
	}
	r, err := f.deps[1].Store.readContinuation(f.ctx, f.run)
	if err != nil || r == nil || !r.State.PointerAttempted || r.State.PointerConfirmed == nil {
		t.Fatalf("checkpoint not persisted: %+v %v", r, err)
	}
}
func (c *continuationFixture) resume(t *testing.T) {
	t.Helper()
	f := c.base
	owned, err := f.deps[1].continueDeployment(f.ctx, f.run, f.pkg)
	if err != nil || !owned {
		t.Fatalf("resume: owned=%v %v", owned, err)
	}
}
func (c *continuationFixture) row(t *testing.T) map[string]any {
	t.Helper()
	row, err := c.base.deps[1].Store.deploymentById(memql.ContextWithFreshRead(c.base.ctx), c.base.run)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func TestPackageContinuationSelfRollMovesToAnotherReplica(t *testing.T) {
	for _, clusterOwned := range []bool{false, true} {
		t.Run(fmt.Sprintf("clusterOwned=%v", clusterOwned), func(t *testing.T) {

			c := newContinuationFixture(t)
			f := c.base
			if clusterOwned {
				// Legacy cluster-owned source fixture: only the stored owner is empty;
				// the original requester and the real account/site authorization stay.
				db, err := f.deps[0].Store.continuationDB()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(f.ctx, `UPDATE "MemoryNodes" SET payload=jsonb_set(payload,'{ownerUserId}','""'::jsonb) WHERE id=$1`, f.pkg); err != nil {
					t.Fatal(err)
				}
				f.ctx = memql.ContextWithFreshRead(f.ctx)
			}
			c.start(t)
			if c.publishers[0].published.Load() != 0 || len(f.h.builder.built) != 1 || c.roller.calls != 1 {
				t.Fatal("effects crossed the rollout readiness gate")
			}
			// Replica B has an independent engine cache and no original goroutine/source
			// bytes. Its only handoff is the native PG row and retained output receipts.
			f.h.fetcher.err = errors.New("a continuation must never fetch or rebuild")
			c.roller.ready = true
			c.resume(t)
			if rowString(c.row(t), "status") != StatusSucceeded || c.publishers[1].published.Load() != 1 || c.roller.calls != 1 || len(f.h.builder.built) != 1 {
				t.Fatal("same-attempt continuation did not finish exactly once")
			}
			r, err := f.deps[1].Store.readContinuation(f.ctx, f.run)
			if err != nil || r == nil || r.Phase != "completed" || len(r.Receipts) != 1 {
				t.Fatalf("missing native receipt: %+v %v", r, err)
			}
			if clusterOwned && (r.Selection.OwnerUserID != "" || r.Selection.RequestedBy != memql.BareShortId(f.owner)) {
				t.Fatal("cluster-owned source changed requester authority")
			}
			owned, err := f.deps[1].continueDeployment(f.ctx, f.run, f.pkg)
			if err != nil || owned || c.publishers[1].published.Load() != 1 {
				t.Fatal("terminal attempt was replayed")
			}

		})
	}
}

func TestPackageContinuationUnknownRollOnlyObserves(t *testing.T) {
	c := newContinuationFixture(t)
	c.roller.lost = true
	c.start(t)
	c.resume(t)
	if c.roller.calls != 1 || c.publishers[1].published.Load() != 0 {
		t.Fatal("uncertain rollout was replayed or prematurely published")
	}
	c.roller.ready = true
	c.resume(t)
	if rowString(c.row(t), "status") != StatusSucceeded || c.roller.calls != 1 {
		t.Fatal("observed rollout did not complete")
	}
}
func TestPackageContinuationCommittedPublicationLostReplyDoesNotReplay(t *testing.T) {
	c := newContinuationFixture(t)
	c.start(t)
	c.roller.ready = true
	c.publishers[1].lost.Store(true)
	c.resume(t)
	if rowString(c.row(t), "status") == StatusSucceeded {
		t.Fatal("unknown reply must remain pending until receipt observation")
	}
	c.resume(t)
	if rowString(c.row(t), "status") != StatusSucceeded || c.publishers[1].published.Load() != 1 {
		t.Fatal("committed publication was replayed")
	}
}
func TestPackageContinuationRefusesChangedInputsAndRevocation(t *testing.T) {
	for _, what := range []string{"source", "requester", "pointer", "output", "pointer-during-verification"} {
		t.Run(what, func(t *testing.T) {
			c := newContinuationFixture(t)
			c.start(t)
			c.roller.ready = true
			f := c.base
			switch what {
			case "source":
				err := f.deps[0].Store.setPackageStatus(f.ctx, f.pkg, "archived")
				if err != nil {
					t.Fatal(err)
				}
			case "requester":
				f.deps[1].Roles = func(context.Context, string) (auth.Role, error) { return auth.RoleReader, nil }
			case "pointer":
				c.stager.etag++
			case "output":
				c.publishers[1].corrupt.Store(true)
			case "pointer-during-verification":
				c.publishers[1].driftDuringVerification.Store(true)
			}
			c.resume(t)
			if c.publishers[1].published.Load() != 0 || rowString(c.row(t), "status") == StatusSucceeded {
				t.Fatal("changed selection or authority published")
			}
		})
	}
}

func selectedPublication(t *testing.T) (*continuationFixture, *continuationRecord, context.Context, string) {
	t.Helper()
	c := newContinuationFixture(t)
	c.start(t)
	f := c.base
	r, err := f.deps[0].Store.claimContinuation(f.ctx, f.run)
	if err != nil || r == nil {
		t.Fatalf("claim: %+v %v", r, err)
	}
	if err := f.deps[0].Store.saveContinuation(f.ctx, r, "publishing"); err != nil {
		t.Fatal(err)
	}
	p := r.Selection.Publications[0]
	ctx := memql.ContextWithPackagePublicationFence(memql.ContextWithRowVersionFence(f.ctx, "v1:platform:site", p.SiteID, p.SiteVersion), r.Selection.DeploymentID, r.SHA, r.Lease, p.Key)
	q, err := edge.PointVersionStatement(p.SiteID, edge.Target(p.Target), p.BundleRef, "")
	if err != nil {
		t.Fatal(err)
	}
	return c, r, ctx, q
}
func TestPackagePublicationFenceRefusesActorLeaseSelectionAndWitness(t *testing.T) {
	for _, change := range []string{"actor", "lease", "selection", "witness", "target"} {
		t.Run(change, func(t *testing.T) {
			c, r, ctx, q := selectedPublication(t)
			f := c.base
			p := r.Selection.Publications[0]
			switch change {
			case "actor":
				ctx = auth.ContextWithUserActor(ctx, "foreign")
			case "lease":
				ctx = memql.ContextWithPackagePublicationFence(ctx, r.Selection.DeploymentID, r.SHA, "foreign", p.Key)
			case "selection":
				ctx = memql.ContextWithPackagePublicationFence(ctx, r.Selection.DeploymentID, strings.Repeat("0", 64), r.Lease, p.Key)
			case "witness":
				ctx = memql.ContextWithPackagePublicationFence(memql.ContextWithRowVersionFence(f.ctx, "v1:platform:site", p.SiteID, p.SiteVersion.Add(-time.Microsecond)), r.Selection.DeploymentID, r.SHA, r.Lease, p.Key)
			case "target":
				q, _ = edge.PointVersionStatement(p.SiteID, edge.TargetCandidate, p.BundleRef, "")
			}
			if _, err := f.deps[1].Store.engine.Execute(ctx, q); err == nil {
				t.Fatal("invalid publication fence accepted")
			}
			latest, err := f.deps[0].Store.readContinuation(f.ctx, f.run)
			if err != nil || len(latest.Receipts) != 0 {
				t.Fatal("refused mutation recorded receipt")
			}
		})
	}
}
func TestPackagePublicationReceiptAndSiteRollbackTogether(t *testing.T) {
	c, r, ctx, q := selectedPublication(t)
	f := c.base
	db, _ := f.deps[0].Store.continuationDB()
	p := r.Selection.Publications[0]
	before, err := f.deps[0].Store.siteById(memql.ContextWithFreshRead(f.ctx), p.SiteID)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("package_receipt_fail_%d", time.Now().UnixNano())
	// Owned disposable DB: failure occurs after the engine created its site
	// version, while recording the coupled receipt in that SAME transaction.
	function := fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.deployment_id = '%s' AND NEW.publications <> OLD.publications THEN RAISE EXCEPTION 'injected receipt failure'; END IF; RETURN NEW; END $$`, name, r.Selection.DeploymentID)
	if _, err := db.ExecContext(f.ctx, function); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(f.ctx, fmt.Sprintf(`CREATE TRIGGER %s BEFORE UPDATE ON package_deployment_continuations FOR EACH ROW EXECUTE FUNCTION %s()`, name, name)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON package_deployment_continuations`, name))
		_, _ = db.ExecContext(context.Background(), fmt.Sprintf(`DROP FUNCTION IF EXISTS %s()`, name))
	})
	if _, err := f.deps[1].Store.engine.Execute(ctx, q); err == nil {
		t.Fatal("injected receipt failure did not refuse")
	}
	after, err := f.deps[1].Store.siteById(memql.ContextWithFreshRead(f.ctx), p.SiteID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("site escaped rolled-back receipt transaction")
	}
	latest, err := f.deps[0].Store.readContinuation(f.ctx, f.run)
	if err != nil || len(latest.Receipts) != 0 {
		t.Fatal("receipt escaped rolled-back site transaction")
	}
}
func TestPackageContinuationReplacedLeaseFencesLateOwner(t *testing.T) {
	c, r, oldCtx, q := selectedPublication(t)
	f := c.base
	db, _ := f.deps[0].Store.continuationDB()
	if _, err := db.ExecContext(f.ctx, `UPDATE package_deployment_continuations SET lease_until='epoch' WHERE deployment_id=$1`, r.Selection.DeploymentID); err != nil {
		t.Fatal(err)
	}
	replacement, err := f.deps[1].Store.claimContinuation(f.ctx, f.run)
	if err != nil || replacement == nil || replacement.Lease == r.Lease {
		t.Fatal("second replica did not take expired lease")
	}
	if _, err := f.deps[0].Store.engine.Execute(oldCtx, q); err == nil {
		t.Fatal("late original owner published after replacement")
	}
	if err := f.deps[0].Store.saveContinuation(f.ctx, r, "completed"); err == nil {
		t.Fatal("late original owner closed replacement")
	}
}
func TestPackageContinuationCancelAndSweepPreserveCheckpoint(t *testing.T) {
	c := newContinuationFixture(t)
	c.start(t)
	f := c.base
	if _, err := f.integration(1).handleCancelDeployment(f.ctx, map[string]any{"deploymentId": f.run}, 0); err == nil {
		t.Fatal("cancel interrupted a committed rollout checkpoint")
	}
	if _, err := SweepAbandoned(f.ctx, f.deps[1]); err != nil {
		t.Fatal(err)
	}
	if IsTerminal(rowString(c.row(t), "status")) {
		t.Fatal("sweep abandoned resumable selected rollout")
	}
	c.roller.ready = true
	c.resume(t)
	if rowString(c.row(t), "status") != StatusSucceeded {
		t.Fatal("checkpoint failed to finish after sweep")
	}
}

func TestPackageContinuationCompetingReplicaClaimsSelectOneOwner(t *testing.T) {
	c := newContinuationFixture(t)
	c.start(t)
	f := c.base
	start := make(chan struct{})
	results := make(chan *continuationRecord, 2)
	errs := make(chan error, 2)
	for _, d := range f.deps {
		go func(d *Deps) { <-start; r, err := d.Store.claimContinuation(f.ctx, f.run); results <- r; errs <- err }(d)
	}
	close(start)
	claims := 0
	for i := 0; i < 2; i++ {
		if r := <-results; r != nil {
			claims++
			defer f.deps[0].Store.releaseContinuation(f.ctx, r)
		}
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if claims != 1 {
		t.Fatalf("claimed %d times", claims)
	}
	if c.roller.calls != 1 || c.publishers[0].published.Load()+c.publishers[1].published.Load() != 0 {
		t.Fatal("claim replayed effects")
	}
}

func TestPackageContinuationLostActiveSetReplyOnlyObserves(t *testing.T) {
	c := newContinuationFixture(t)
	f := c.base
	c.stager.lost = true
	out, err := Deploy(f.ctx, f.deps[0], DeployRequest{PackageId: f.pkg, DeploymentId: f.run, Confirmed: true, Actor: actorFromContext(f.ctx), Placements: map[string]Placement{"storefront": {Skip: true}}})
	if err != nil || out == nil || out.Status != StatusRolling {
		t.Fatalf("lost pointer reply: %+v %v", out, err)
	}
	if c.stager.written != 1 || c.roller.calls != 0 {
		t.Fatal("unknown pointer crossed rollout gate")
	}
	c.resume(t)
	if c.stager.written != 1 || c.roller.calls != 1 {
		t.Fatal("pointer was repeated instead of observed")
	}
	c.roller.ready = true
	c.resume(t)
	if rowString(c.row(t), "status") != StatusSucceeded || c.stager.written != 1 {
		t.Fatal("observed exact pointer failed to finish")
	}
}

func TestPackageContinuationDeadlineKeepsUncertainEffectsAndTerminalHistory(t *testing.T) {
	c := newContinuationFixture(t)
	c.roller.lost = true
	c.start(t)
	f := c.base
	stale := inFlightRow(f.run, StatusRolling, time.Now().Add(-time.Hour))
	stale["packageId"] = f.pkg
	f.deps[1].Store.engine = staleSweepSnapshot{Engine: f.deps[1].Store.engine, row: stale}
	f.deps[1].Now = func() time.Time { return time.Now().Add(time.Hour) }
	result, err := SweepAbandoned(f.ctx, f.deps[1])
	if err != nil || result.Abandoned != 1 || rowString(c.row(t), "status") != StatusAbandoned {
		t.Fatalf("deadline: %+v %v", result, err)
	}
	if c.roller.calls != 1 || c.publishers[1].published.Load() != 0 {
		t.Fatal("deadline replayed unknown effects")
	}
	owned, err := f.deps[0].continueDeployment(f.ctx, f.run, f.pkg)
	if err != nil || owned || c.roller.calls != 1 {
		t.Fatal("terminal original attempt reopened")
	}
}

func TestPackageContinuationCompletedBookkeepingRequiresAllExactReceipts(t *testing.T) {
	for _, fault := range []string{"missing", "foreign"} {
		t.Run(fault, func(t *testing.T) {
			c, r, ctx, q := selectedPublication(t)
			f := c.base
			if _, err := f.deps[0].Store.engine.Execute(ctx, q); err != nil {
				t.Fatal(err)
			}
			if err := f.deps[0].Store.saveContinuation(f.ctx, r, "completed"); err != nil {
				t.Fatal(err)
			}
			db, _ := f.deps[0].Store.continuationDB()
			value := "{}"
			if fault == "foreign" {
				p := r.Selection.Publications[0]
				raw, _ := json.Marshal(map[string]memql.PackagePublicationReceipt{p.Key: {SiteID: "foreign", Target: p.Target, BundleRef: p.BundleRef, SiteVersion: time.Now()}})
				value = string(raw)
			}
			if _, err := db.ExecContext(f.ctx, `UPDATE package_deployment_continuations SET publications=$2 WHERE deployment_id=$1`, r.Selection.DeploymentID, value); err != nil {
				t.Fatal(err)
			}
			if err := f.deps[1].finishContinuation(f.ctx, r); err == nil {
				t.Fatal("unproven receipt closed deployment")
			}
			if IsTerminal(rowString(c.row(t), "status")) {
				t.Fatal("bookkeeping fabricated success")
			}
		})
	}
}

func TestPackageContinuationCancelWinsBeforeCheckpoint(t *testing.T) {
	c := newContinuationFixture(t)
	f := c.base
	if _, err := f.integration(1).handleCancelDeployment(f.ctx, map[string]any{"deploymentId": f.run}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := Deploy(f.ctx, f.deps[0], DeployRequest{PackageId: f.pkg, DeploymentId: f.run, Confirmed: true, Actor: actorFromContext(f.ctx)}); err == nil {
		t.Fatal("cancelled saved selection restarted")
	}
	r, err := f.deps[0].Store.readContinuation(f.ctx, f.run)
	if err != nil || r != nil || c.roller.calls != 0 || c.stager.written != 0 || len(f.h.builder.built) != 0 {
		t.Fatal("cancelled selection reached a continuation effect")
	}
}
