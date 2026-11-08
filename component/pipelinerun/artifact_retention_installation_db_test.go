package pipelinerun

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

// installationArtifactLibrary is the Library a sweep sees when it reaches an
// installation preparation's source captures. Their own cleanup recipe fences
// and retires them (integrations/installation), so the only effect the sweep
// may have here is removing the Library entries of a file already retired.
type installationArtifactLibrary struct {
	ArtifactRetentionLibrary
	t           *testing.T
	db          *sql.DB
	owner, work string
	mu          sync.Mutex
	calls       int
	failCall    int // a purge call (1-based) that loses its reply before committing
	purged      []string
	forbidden   []string
}

func (l *installationArtifactLibrary) inScope(ctx context.Context, s pipelinesteps.RunFileReceiptScope) error {
	actor, ok := auth.AccessFromContext(ctx)
	if !ok || actor.UserId != l.owner || !auth.OriginFromContext(ctx).IsInternal() || s.OwnerUserID != l.owner || s.WorkRunID != l.work || s.Attempt != 1 || !strings.HasPrefix(s.StepKey, "source-") {
		return fmt.Errorf("artifact operation escaped its owner/producer scope: %+v", s)
	}
	return nil
}

func (l *installationArtifactLibrary) FenceRunFileScope(_ context.Context, s pipelinesteps.RunFileReceiptScope) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.forbidden = append(l.forbidden, "fence "+s.StepKey)
	return errors.New("the sweep fenced an installation preparation's capture")
}

func (l *installationArtifactLibrary) RetireRunFile(_ context.Context, _ pipelinesteps.RunFileReceiptScope, key string) (pipelinesteps.RetiredFileReceipt, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.forbidden = append(l.forbidden, "retire "+key)
	return pipelinesteps.RetiredFileReceipt{}, errors.New("the sweep retired an installation preparation's capture")
}

func (l *installationArtifactLibrary) PurgeRetiredRunFile(ctx context.Context, s pipelinesteps.RunFileReceiptScope, key string) error {
	if err := l.inScope(ctx, s); err != nil {
		l.t.Error(err)
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	if l.calls == l.failCall {
		return errors.New("lost Library cleanup reply")
	}
	// The real store refuses a file whose retirement it cannot confirm.
	res, err := l.db.ExecContext(ctx, `UPDATE pipeline_artifact_uploads SET library_purged_at=coalesce(library_purged_at,clock_timestamp())
WHERE intent_id=$1 AND state='retired' AND retired_etag<>'' AND retirement_lease_id<>''`, key)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return errors.New("Library cleanup requires a confirmed permanent storage fence")
	}
	l.purged = append(l.purged, key)
	return nil
}

func (l *installationArtifactLibrary) snapshot() (purged, forbidden []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.purged...), append([]string(nil), l.forbidden...)
}

type installationProducerFixture struct {
	h            *sarifHarness
	installation string
	preparation  string
	run          string
	ids          []string
}

// seedInstallationProducer records an installation preparation the way its
// journal does (source_run_id is the run every one of its captures uploads
// under, and is not a v1:work:run) and uploads count of its files.
func seedInstallationProducer(t *testing.T, h *sarifHarness, count int) *installationProducerFixture {
	t.Helper()
	p := &installationProducerFixture{h: h, installation: "installation-" + id.NewShortId(), run: "source-run-" + id.NewShortId()}
	sum := sha256.Sum256([]byte(p.run))
	p.preparation = "memql-id:" + hex.EncodeToString(sum[:])
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM installation_preparations WHERE preparation_id=$1`,
			`DELETE FROM installation_revision_heads WHERE installation_id=$2`,
		} {
			if _, err := h.db.Exec(q, p.preparation, p.installation); err != nil {
				t.Error(err)
			}
		}
	})
	if _, err := h.db.Exec(`INSERT INTO installation_revision_heads(installation_id) VALUES($1)`, p.installation); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`INSERT INTO installation_preparations(preparation_id,installation_id,request_id,requested_by,source_run_id,slot_epoch,scope,captures,state)
VALUES($1,$2,'request-one',$3,$4,1,'{}'::jsonb,'{}'::jsonb,'preparing')`, p.preparation, p.installation, h.owner, p.run); err != nil {
		t.Fatal(err)
	}
	for n := range count {
		key := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s:%d", p.run, n))))
		step := "source-candidate"
		if n%2 == 1 {
			step = "source-rollback"
		}
		identity, _ := json.Marshal(pipelinesteps.RunFileReceiptScope{OwnerUserID: h.owner, WorkRunID: p.run, StepKey: step, Attempt: 1})
		if _, err := h.db.Exec(`INSERT INTO pipeline_artifact_uploads(intent_id,owner_user_id,identity,file_id,object_key,size_bytes,generation,state)
VALUES($1,$2,$3::jsonb,$4,$5,1024,1,'ready')`, key, h.owner, string(identity), id.NewShortId(), "installation-retention-test/"+key); err != nil {
			t.Fatal(err)
		}
		p.ids = append(p.ids, key)
	}
	slices.Sort(p.ids)
	return p
}

// retire stands in for the preparation's cleanup having retired these files:
// a permanent provider tombstone and its lease, and the preparation cancelled.
func (p *installationProducerFixture) retire(t *testing.T, ids ...string) {
	t.Helper()
	for _, key := range ids {
		if _, err := p.h.db.Exec(`UPDATE pipeline_artifact_uploads SET state='retired',retired_etag='tombstone-etag',retirement_lease_id='tombstone-lease'
WHERE intent_id=$1`, key); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.h.db.Exec(`UPDATE installation_preparations SET state='cancelled' WHERE preparation_id=$1`, p.preparation); err != nil {
		t.Fatal(err)
	}
}

func (p *installationProducerFixture) prioritize(t *testing.T) {
	t.Helper()
	if _, err := p.h.db.Exec(`UPDATE pipeline_artifact_retention_queue SET visited_at='-infinity' WHERE owner_user_id=$1 AND work_run_id=$2`, p.h.owner, p.run); err != nil {
		t.Fatal(err)
	}
}

// visitedSincePrioritized reports whether a sweep claimed the producer after
// prioritize put it at the head of the rotation.
func (p *installationProducerFixture) visitedSincePrioritized(t *testing.T) bool {
	t.Helper()
	var untouched bool
	if err := p.h.db.QueryRow(`SELECT visited_at='-infinity' FROM pipeline_artifact_retention_queue WHERE owner_user_id=$1 AND work_run_id=$2`, p.h.owner, p.run).Scan(&untouched); err != nil {
		t.Fatal(err)
	}
	return !untouched
}

func (p *installationProducerFixture) unpurged(t *testing.T) int {
	t.Helper()
	var n int
	if err := p.h.db.QueryRow(`SELECT count(*) FROM pipeline_artifact_uploads WHERE owner_user_id=$1 AND identity->>'WorkRunID'=$2 AND library_purged_at IS NULL`, p.h.owner, p.run).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func installationRetentionHarness(t *testing.T, count int) (*sarifHarness, *installationProducerFixture, [2]*installationArtifactLibrary) {
	t.Helper()
	h, _, _ := artifactRetentionHarness(t, 0)
	p := seedInstallationProducer(t, h, count)
	var libraries [2]*installationArtifactLibrary
	for n, integ := range []*Integration{h.i, h.peer} {
		l := &installationArtifactLibrary{t: t, db: h.db, owner: h.owner, work: p.run}
		libraries[n] = l
		integ.Configure(func(d *Deps) {
			d.ArtifactRetention = &ArtifactRetentionDependencies{Database: func() *sql.DB { return h.db }, Engine: h.engine, Library: l}
		})
	}
	return h, p, libraries
}

func (h *sarifHarness) askedAbout(work string) bool {
	for _, call := range h.engine.recorded() {
		if strings.Contains(call.query, work) {
			return true
		}
	}
	return false
}

// A preparation's source captures are uploaded under its own run, which is
// not a pipeline's work run, so the sweep can never resolve a retention window
// for them. Until their cleanup has retired them, the sweep must not claim
// the producer at all: claiming it was a failed pass on every rotation.
func TestArtifactRetentionLeavesALivePreparationsCapturesToItsCleanup(t *testing.T) {
	h, p, libraries := installationRetentionHarness(t, 2)
	ctx := auth.ContextWithInternalOrigin(t.Context())
	for _, integ := range []*Integration{h.i, h.peer} {
		p.prioritize(t)
		// Another test's producer may be next in the shared rotation; only this
		// producer's journal is asserted.
		_, _ = integ.SweepArtifacts(ctx)
		if p.visitedSincePrioritized(t) {
			t.Fatal("the sweep claimed a preparation whose captures are not retired")
		}
	}
	for _, l := range libraries {
		if purged, forbidden := l.snapshot(); len(purged)+len(forbidden) != 0 {
			t.Fatalf("the sweep acted on live captures: purged=%v forbidden=%v", purged, forbidden)
		}
	}
	if h.askedAbout(p.run) {
		t.Fatal("the sweep looked for a pipeline work journal the preparation does not have")
	}
	if p.unpurged(t) != 2 {
		t.Fatal("live captures lost their Library entries")
	}
}

// Once the preparation's own cleanup has retired its files (fence, retire),
// their Library entries still have to go; preparations cancelled before the
// cleanup recipe removed them have nothing else that will. The sweep removes
// only those entries: it never fences or retires the preparation's files, and
// it never needs a retention window, because the bytes are already gone.
func TestArtifactRetentionPurgesARetiredPreparationsCapturesAcrossReplicas(t *testing.T) {
	h, p, libraries := installationRetentionHarness(t, 36)
	retired := p.ids[:35]
	p.retire(t, retired...) // the 36th was never retired: still the preparation's
	ctx := auth.ContextWithInternalOrigin(t.Context())

	// The first replica purges two files and loses the third purge's reply.
	libraries[0].failCall = 3
	p.prioritize(t)
	if _, err := h.i.SweepArtifacts(ctx); err == nil {
		t.Fatal("an unconfirmed Library removal reported success")
	}
	if n := p.unpurged(t); n != 36-2 {
		t.Fatalf("unpurged after a lost reply = %d, want 34", n)
	}

	// A second replica resumes from the upload journal, not from the first
	// replica's memory, one bounded page of 32 at a time.
	p.prioritize(t)
	out, err := h.peer.SweepArtifacts(ctx)
	if err != nil || out["purged"] != 32 || out["retired"] != 0 || out["bytes"] != int64(0) || out["hasMore"] != true {
		t.Fatalf("second replica %+v %v", out, err)
	}
	p.prioritize(t)
	out, err = h.i.SweepArtifacts(ctx)
	if err != nil || out["purged"] != 1 || out["hasMore"] != false {
		t.Fatalf("last page %+v %v", out, err)
	}
	if n := p.unpurged(t); n != 1 {
		t.Fatalf("unpurged after the last page = %d, want only the unretired file", n)
	}

	// Nothing retired is left, so the producer leaves the rotation for good.
	p.prioritize(t)
	_, _ = h.peer.SweepArtifacts(ctx)
	if p.visitedSincePrioritized(t) {
		t.Fatal("the sweep kept claiming a preparation with nothing left to remove")
	}

	var all []string
	for _, l := range libraries {
		purged, forbidden := l.snapshot()
		if len(forbidden) != 0 {
			t.Fatalf("the sweep made a retirement decision for the preparation: %v", forbidden)
		}
		all = append(all, purged...)
	}
	slices.Sort(all)
	if !slices.Equal(all, retired) {
		t.Fatalf("each retired file must lose its Library entries exactly once: got %d, want %d", len(all), len(retired))
	}
	if h.askedAbout(p.run) {
		t.Fatal("the sweep looked for a pipeline work journal the preparation does not have")
	}
}

// Two replicas sweeping at once lease one producer each: the preparation's
// retired captures are purged by exactly one of them.
func TestArtifactRetentionPreparationPurgeHoldsItsLeaseAgainstAConcurrentReplica(t *testing.T) {
	h, p, libraries := installationRetentionHarness(t, 4)
	p.retire(t, p.ids...)
	p.prioritize(t)
	ctx := auth.ContextWithInternalOrigin(t.Context())
	var group sync.WaitGroup
	for _, integ := range []*Integration{h.i, h.peer} {
		group.Go(func() { _, _ = integ.SweepArtifacts(ctx) })
	}
	group.Wait()
	a, _ := libraries[0].snapshot()
	b, _ := libraries[1].snapshot()
	if len(a)+len(b) != 4 || (len(a) != 0 && len(b) != 0) {
		t.Fatalf("one leased pass must purge the page: first=%d second=%d", len(a), len(b))
	}
	if p.unpurged(t) != 0 {
		t.Fatal("retired captures kept their Library entries")
	}
}

// The page's order is the installed recipe's; the gates are native. A recipe
// substituted for it cannot make the sweep retire an installation
// preparation's files, or report a removal it never confirmed.
func TestArtifactRetentionPreparationPageRefusesRetirementAndForgedReports(t *testing.T) {
	for name, body := range map[string]string{
		"retires": `items := builtin pipelineRetentionItems()
 for item in items {
  builtin pipelineRetentionFence(intentId: item.intentId)
  builtin pipelineRetentionRetire(intentId: item.intentId)
  builtin pipelineRetentionPurge(intentId: item.intentId)
 }
 return builtin pipelineRetentionReport()`,
		"forges": `return builtin pipelineRetentionReport()`,
	} {
		t.Run(name, func(t *testing.T) {
			h, p, libraries := installationRetentionHarness(t, 2)
			p.retire(t, p.ids[0])
			a := workflowSource(t, "@template\nautomation purgeRetiredPipelineArtifacts {\n "+body+"\n}")
			h.i.Configure(func(d *Deps) { d.LoadWorkflow = workflowLoader(a) })
			p.prioritize(t)
			if _, err := h.i.SweepArtifacts(auth.ContextWithInternalOrigin(t.Context())); err == nil {
				t.Fatal("a substituted recipe completed a page it did not confirm")
			}
			if purged, forbidden := libraries[0].snapshot(); len(purged)+len(forbidden) != 0 {
				t.Fatalf("the refused page still acted: purged=%v forbidden=%v", purged, forbidden)
			}
			if p.unpurged(t) != 2 {
				t.Fatal("a refused page removed Library entries")
			}
		})
	}
}
