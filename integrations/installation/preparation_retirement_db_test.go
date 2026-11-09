package installation

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

type retirementFilesFixture struct {
	mu       sync.Mutex
	ids      map[pipelinesteps.RunFileReceiptScope][]string
	fenced   map[pipelinesteps.RunFileReceiptScope]bool
	released map[string]bool
	retired  map[string]pipelinesteps.RetiredFileReceipt
	purged   map[string]int
	// kept names intents the Library refuses to retire because their owner
	// filed, labelled, edited, versioned or trained them; refused the way the
	// store refuses them, before any storage effect.
	kept map[string]bool
	// beforeRetire and afterRetire each run once, outside the lock, around the
	// next retirement call: a second cleanup pass racing this one.
	beforeRetire, afterRetire func()
	fail                      string
	calls                     int
}

func retirementFiles(scope preparationScope) *retirementFilesFixture {
	return &retirementFilesFixture{ids: map[pipelinesteps.RunFileReceiptScope][]string{
		retirementArtifactScope(scope.Captures["candidate"]): {fmt.Sprintf("%064x", 1), fmt.Sprintf("%064x", 2), fmt.Sprintf("%064x", 3)},
		retirementArtifactScope(scope.Captures["rollback"]):  {fmt.Sprintf("%064x", 4)},
	}, fenced: map[pipelinesteps.RunFileReceiptScope]bool{}, released: map[string]bool{}, retired: map[string]pipelinesteps.RetiredFileReceipt{}, purged: map[string]int{}, kept: map[string]bool{}}
}

func (f *retirementFilesFixture) effect(ctx context.Context, scope pipelinesteps.RunFileReceiptScope, operation string) error {
	f.calls++
	a, _ := auth.AccessFromContext(ctx)
	if auth.OriginFromContext(ctx) != auth.OriginInternal || a == nil || !strings.HasSuffix(a.UserId, ":"+scope.OwnerUserID) {
		return errors.New("wrong native owner")
	}
	if _, ok := f.ids[scope]; !ok {
		return errors.New("foreign cleanup scope")
	}
	if f.fail == operation {
		f.fail = ""
		return errors.New("lost committed " + operation + " response")
	}
	return nil
}
func (f *retirementFilesFixture) FenceRunFileScope(ctx context.Context, scope pipelinesteps.RunFileReceiptScope) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fenced[scope] = true
	return f.effect(ctx, scope, "fence")
}
func (f *retirementFilesFixture) ReadFencedRunFileIntents(ctx context.Context, scope pipelinesteps.RunFileReceiptScope, cursor string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.fenced[scope] {
		return nil, errors.New("unfenced")
	}
	if err := f.effect(ctx, scope, "inventory"); err != nil {
		return nil, err
	}
	var result []string
	for _, id := range f.ids[scope] {
		if id > cursor {
			result = append(result, id)
			if len(result) == 2 {
				break
			}
		}
	}
	return result, nil
}
func (f *retirementFilesFixture) PinRunFileReceipts(context.Context, pipelinesteps.RunFileReference) ([]pipelinesteps.StoredFileReceipt, error) {
	return nil, errors.New("retirement may not pin")
}
func (f *retirementFilesFixture) ReleaseRunFileReference(ctx context.Context, ref pipelinesteps.RunFileReference) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.fenced[ref.Scope] {
		return errors.New("unfenced release")
	}
	f.released[ref.ReferenceID] = true
	return f.effect(ctx, ref.Scope, "release")
}
func (f *retirementFilesFixture) RetireRunFile(ctx context.Context, scope pipelinesteps.RunFileReceiptScope, id string) (pipelinesteps.RetiredFileReceipt, error) {
	f.race(&f.beforeRetire)
	receipt, err := f.retire(ctx, scope, id)
	f.race(&f.afterRetire)
	return receipt, err
}

func (f *retirementFilesFixture) race(hook *func()) {
	f.mu.Lock()
	run := *hook
	*hook = nil
	f.mu.Unlock()
	if run != nil {
		run()
	}
}

// Like the store, an already retired file answers its receipt, and a kept one
// is refused with pipelinesteps.ErrArtifactRetained and its reason.
func (f *retirementFilesFixture) retire(ctx context.Context, scope pipelinesteps.RunFileReceiptScope, id string) (pipelinesteps.RetiredFileReceipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.fenced[scope] || !slices.Contains(f.ids[scope], id) {
		return pipelinesteps.RetiredFileReceipt{}, errors.New("foreign/unfenced artifact")
	}
	if _, retired := f.retired[id]; !retired && f.kept[id] {
		if err := f.effect(ctx, scope, "keep"); err != nil {
			return pipelinesteps.RetiredFileReceipt{}, err
		}
		return pipelinesteps.RetiredFileReceipt{}, fmt.Errorf("%w: the file was filed in a folder", pipelinesteps.ErrArtifactRetained)
	}
	receipt := pipelinesteps.RetiredFileReceipt{IntentID: id, FileID: "file-" + id, TombstoneETag: "permanent-etag"}
	f.retired[id] = receipt
	return receipt, f.effect(ctx, scope, "retire")
}

// Like the Library store, removal is idempotent and refuses an artifact whose
// bytes were not retired first. A lost reply still committed the removal.
func (f *retirementFilesFixture) PurgeRetiredRunFile(ctx context.Context, scope pipelinesteps.RunFileReceiptScope, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, retired := f.retired[id]; !retired || !f.fenced[scope] || !slices.Contains(f.ids[scope], id) {
		return errors.New("Library cleanup requires a retired artifact in its fenced scope")
	}
	f.purged[id]++
	return f.effect(ctx, scope, "purge")
}

type retirementStopFixture struct {
	captureExecutorFixture
	fail  bool
	calls int
}

func (f *retirementStopFixture) Cancel(_ context.Context, run string) error {
	f.calls++
	f.cancelledRun = run
	if f.fail {
		f.fail = false
		return errors.New("lost stop response")
	}
	return nil
}

func TestPreparationRetirementReconcilesLostRepliesAndReleasesOnlyItsHead(t *testing.T) {
	db, peerDB := journalDB(t)
	j, peer := preparationConnection(db), preparationConnection(peerDB)
	scope, _ := preparationFixture(t)
	ctx := captureOperator(auth.RoleOwner, scope.RequestedBy)
	cleanup := "memql-id:" + strings.Repeat("e", 64)
	r, err := j.reserve(ctx, scope)
	require.NoError(t, err)
	for _, role := range []string{"candidate", "rollback"} {
		_, _, err = j.beginCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, role)
		require.NoError(t, err)
	}
	capture, err := newSourceCapture(scope.Captures["candidate"])
	require.NoError(t, err)
	receipt := sourceCaptureReceipt{ScopeDigest: capture.digest, IntentID: fmt.Sprintf("%064x", 1)}
	_, err = j.recordReceipt(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, "candidate", receipt)
	require.NoError(t, err)
	_, err = j.beginRetirement(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup)
	require.NoError(t, err)
	r, err = peer.beginRetirement(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup)
	require.NoError(t, err)
	require.Equal(t, "retiring", r.State)
	_, _, err = j.beginCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, "rollback")
	require.Error(t, err)
	_, err = j.recordReceipt(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, "candidate", receipt)
	require.Error(t, err)
	_, err = j.cancelBeforeCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest)
	require.Error(t, err)
	_, err = j.finishRetirement(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup)
	require.Error(t, err)
	competingPlan := testPlan()
	competingPlan.RequestedBy = scope.RequestedBy
	_, err = (&revisionJournal{db: func() *sql.DB { return peerDB }}).reserve(ctx, competingPlan)
	require.ErrorIs(t, err, errBusy)
	files := retirementFiles(scope)
	_, err = j.fenceRetiringCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, "candidate", files)
	require.Error(t, err)
	require.Zero(t, files.calls)
	executor := &retirementStopFixture{fail: true}
	_, err = j.stopRetiringProducer(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, executor)
	require.Error(t, err)
	r, err = peer.get(ctx, scope.InstallationID, r.ID)
	require.NoError(t, err)
	require.False(t, r.Retirement.ProducerStopped)
	r, err = peer.stopRetiringProducer(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, executor)
	require.NoError(t, err)
	require.Equal(t, 2, executor.calls)
	require.Equal(t, scope.Captures["candidate"].RunID, executor.cancelledRun)
	for _, role := range []string{"candidate", "rollback"} {
		_, err = j.releaseRetiringCapturePins(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, files)
		require.Error(t, err)
		files.fail = "fence"
		_, err = j.fenceRetiringCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, files)
		require.Error(t, err)
		_, err = peer.fenceRetiringCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, files)
		require.NoError(t, err)
		_, err = j.nextRetiringArtifacts(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, files)
		require.Error(t, err)
		if role == "candidate" {
			files.fail = "release"
			_, err = j.releaseRetiringCapturePins(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, files)
			require.Error(t, err)
		}
		_, err = peer.releaseRetiringCapturePins(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, files)
		require.NoError(t, err)
		files.fail = "inventory"
		_, err = j.nextRetiringArtifacts(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, files)
		require.Error(t, err)
		for {
			r, err = peer.nextRetiringArtifacts(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, files)
			require.NoError(t, err)
			if r.Retirement.Captures[role].Complete {
				break
			}
			page := r.Retirement.Captures[role].Page
			recoveredPage, err := j.nextRetiringArtifacts(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, files)
			require.NoError(t, err)
			require.Equal(t, page, recoveredPage.Retirement.Captures[role].Page, "lost page response must recover its pending intents without advancing")
			_, err = j.retireCaptureArtifact(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, strings.Repeat("f", 64), files)
			require.Error(t, err)
			files.fail = "retire"
			_, err = j.retireCaptureArtifact(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, page[0].IntentID, files)
			require.Error(t, err)
			current, err := peer.get(ctx, scope.InstallationID, r.ID)
			require.NoError(t, err)
			require.Empty(t, current.Retirement.Captures[role].Page[0].ReceiptDigest, "uncertain provider result cannot advance the cursor")
			// Fence, retire, then purge: Library removal needs this journal's own
			// retirement receipt, not only the store's tombstone.
			_, err = j.purgeRetiredCaptureArtifact(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, page[0].IntentID, files)
			require.ErrorContains(t, err, "retirement receipt")
			for _, artifact := range page {
				_, err = peer.retireCaptureArtifact(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, artifact.IntentID, files)
				require.NoError(t, err)
			}
			held, err := j.nextRetiringArtifacts(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, files)
			require.NoError(t, err)
			require.Equal(t, page[0].IntentID, held.Retirement.Captures[role].Page[0].IntentID, "retired artifacts keep their page until their Library entries are gone")
			files.fail = "purge"
			_, err = j.purgeRetiredCaptureArtifact(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, page[0].IntentID, files)
			require.Error(t, err)
			current, err = peer.get(ctx, scope.InstallationID, r.ID)
			require.NoError(t, err)
			require.False(t, current.Retirement.Captures[role].Page[0].Purged, "an unconfirmed Library removal cannot advance the cursor")
			for _, artifact := range page {
				_, err = peer.purgeRetiredCaptureArtifact(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, artifact.IntentID, files)
				require.NoError(t, err)
			}
			again, err := j.purgeRetiredCaptureArtifact(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, page[0].IntentID, files)
			require.NoError(t, err)
			require.True(t, again.Retirement.Captures[role].Page[0].Purged)
		}
	}
	require.Len(t, files.released, 1, "only the capture with a committed receipt could have pinned")
	require.Len(t, files.retired, 4, "lost rollback capture receipt did not lose its upload")
	require.Len(t, files.purged, 4, "every retired artifact left the Library")
	require.Equal(t, 2, files.purged[fmt.Sprintf("%064x", 1)], "a lost removal reply is repeated, never skipped")
	r, err = j.finishRetirement(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup)
	require.NoError(t, err)
	require.Equal(t, "cancelled", r.State)
	require.True(t, r.Captures["rollback"].Started, "retirement must preserve dispatch history")
	next := successorPreparation(t)
	successor, err := peer.reserve(ctx, next)
	require.NoError(t, err)
	again, err := peer.finishRetirement(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup)
	require.NoError(t, err)
	require.Equal(t, r, again)
	var active string
	require.NoError(t, db.QueryRow("SELECT active_preparation_id FROM installation_revision_heads WHERE installation_id=$1", scope.InstallationID).Scan(&active))
	require.Equal(t, successor.ID, active)
	_, err = j.stopRetiringProducer(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, executor)
	require.Error(t, err)
}

// successorPreparation is the installation's next request. It has its own
// source run, so none of its capture scopes is one of the first preparation's.
func successorPreparation(t *testing.T) preparationScope {
	t.Helper()
	next, _ := preparationFixture(t)
	next.RequestID = "next-request"
	for role, spec := range next.Captures {
		spec.RunID = preparationSourceRun(next.InstallationID, next.RequestID, next.RequestedBy)
		spec.WorkRunID = spec.RunID
		next.Captures[role] = spec
	}
	return next
}

func activePreparation(t *testing.T, db *sql.DB, installation string) string {
	t.Helper()
	var active sql.NullString
	require.NoError(t, db.QueryRow("SELECT active_preparation_id FROM installation_revision_heads WHERE installation_id=$1", installation).Scan(&active))
	return active.String
}

// readyToRetire reserves a preparation and brings its cleanup to the first
// artifact page: retiring, producer stopped, both captures fenced and released.
func readyToRetire(t *testing.T, ctx context.Context, j *preparationJournal, scope preparationScope, cleanup string, files *retirementFilesFixture) preparationRecord {
	t.Helper()
	r, err := j.reserve(ctx, scope)
	require.NoError(t, err)
	_, err = j.beginRetirement(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup)
	require.NoError(t, err)
	_, err = j.stopRetiringProducer(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, &retirementStopFixture{})
	require.NoError(t, err)
	for _, role := range []string{"candidate", "rollback"} {
		_, err = j.fenceRetiringCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, files)
		require.NoError(t, err)
		r, err = j.releaseRetiringCapturePins(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, files)
		require.NoError(t, err)
	}
	return r
}

// A capture its owner kept in the Library -- filed, labelled, edited, given
// another version or trained into a knowledge domain -- is refused by the
// store as retained (pipelinesteps.ErrArtifactRetained). That refusal is the
// store's answer, not an unconfirmed effect. Read as unconfirmed, it held the
// page, the preparation and the installation head for good (2026-11-08):
// cleanup records it as kept, leaves the file's bytes and Library entries,
// and completion releases the head.
func TestPreparationRetirementLeavesAKeptCaptureAndReleasesItsHead(t *testing.T) {
	db, peerDB := journalDB(t)
	j, peer := preparationConnection(db), preparationConnection(peerDB)
	scope, _ := preparationFixture(t)
	ctx := captureOperator(auth.RoleOwner, scope.RequestedBy)
	cleanup := "memql-id:" + strings.Repeat("e", 64)
	files := retirementFiles(scope)
	r := readyToRetire(t, ctx, j, scope, cleanup, files)
	r, err := j.nextRetiringArtifacts(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, "candidate", files)
	require.NoError(t, err)
	first, kept, third, rollback := fmt.Sprintf("%064x", 1), fmt.Sprintf("%064x", 2), fmt.Sprintf("%064x", 3), fmt.Sprintf("%064x", 4)
	require.Equal(t, []preparationArtifactRetirement{{IntentID: first}, {IntentID: kept}}, r.Retirement.Captures["candidate"].Page)
	files.kept[kept] = true

	// A lost reply records nothing, so the head stays held.
	files.fail = "keep"
	_, err = j.retireCaptureArtifact(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, "candidate", kept, files)
	require.Error(t, err)
	current, err := peer.get(ctx, scope.InstallationID, r.ID)
	require.NoError(t, err)
	require.Equal(t, preparationArtifactRetirement{IntentID: kept}, current.Retirement.Captures["candidate"].Page[1])
	next := successorPreparation(t)
	_, err = peer.reserve(ctx, next)
	require.ErrorIs(t, err, errBusy)

	// The other connection repeats the call and records the store's answer.
	r, err = peer.retireCaptureArtifact(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, "candidate", kept, files)
	require.NoError(t, err)
	require.Equal(t, preparationArtifactRetirement{IntentID: kept, Retained: true}, r.Retirement.Captures["candidate"].Page[1])
	calls := files.calls
	again, err := j.retireCaptureArtifact(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, "candidate", kept, files)
	require.NoError(t, err)
	require.Equal(t, r.Retirement, again.Retirement)
	require.Equal(t, calls, files.calls, "a recorded answer is final for this cleanup; the store is not asked again")
	_, err = j.purgeRetiredCaptureArtifact(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, "candidate", kept, files)
	require.Error(t, err, "a kept file keeps its Library entries")
	require.Equal(t, calls, files.calls)
	held, err := peer.nextRetiringArtifacts(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, "candidate", files)
	require.NoError(t, err)
	require.Equal(t, r.Retirement.Captures["candidate"].Page, held.Retirement.Captures["candidate"].Page, "the page still waits for its other file")

	// Every other file is retired and leaves the Library as before. Four page
	// reads cover a capture's two pages and its empty one; a page that never
	// moves past the kept file runs out of them.
	for n, role := range []string{"candidate", "rollback"} {
		for range 4 {
			r, err = []*preparationJournal{j, peer}[n].nextRetiringArtifacts(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, files)
			require.NoError(t, err)
			if r.Retirement.Captures[role].Complete {
				break
			}
			for _, artifact := range r.Retirement.Captures[role].Page {
				if artifact.Retained {
					continue
				}
				_, err = peer.retireCaptureArtifact(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, artifact.IntentID, files)
				require.NoError(t, err)
				_, err = j.purgeRetiredCaptureArtifact(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, role, artifact.IntentID, files)
				require.NoError(t, err)
			}
		}
		require.True(t, r.Retirement.Captures[role].Complete, role)
	}
	r, err = peer.finishRetirement(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup)
	require.NoError(t, err)
	require.Equal(t, "cancelled", r.State)
	retired := slices.Sorted(maps.Keys(files.retired))
	require.Equal(t, []string{first, third, rollback}, retired, "the kept file keeps its bytes")
	require.Equal(t, map[string]int{first: 1, third: 1, rollback: 1}, files.purged, "and its Library entries")
	successor, err := peer.reserve(ctx, next)
	require.NoError(t, err)
	require.Equal(t, successor.ID, activePreparation(t, db, scope.InstallationID))
}

// Two cleanup passes can ask the store about one file either side of its
// owner unfiling it, so one hears it was kept and the other retires it. The
// retirement receipt is the stronger fact, whichever reply is recorded first:
// the bytes are gone, so the entry is retired and must leave the Library, or
// it would point at a zero-byte tombstone after the page moved on.
func TestPreparationRetirementReceiptOutranksARacingKeptAnswer(t *testing.T) {
	for name, keptFirst := range map[string]bool{"kept answer recorded first": true, "receipt recorded first": false} {
		t.Run(name, func(t *testing.T) {
			db, peerDB := journalDB(t)
			j, peer := preparationConnection(db), preparationConnection(peerDB)
			scope, _ := preparationFixture(t)
			ctx := captureOperator(auth.RoleOwner, scope.RequestedBy)
			cleanup := "memql-id:" + strings.Repeat("e", 64)
			files := retirementFiles(scope)
			r := readyToRetire(t, ctx, j, scope, cleanup, files)
			r, err := j.nextRetiringArtifacts(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, "rollback", files)
			require.NoError(t, err)
			intent := fmt.Sprintf("%064x", 4)
			files.kept[intent] = true
			retire := func(journal *preparationJournal) {
				_, err := journal.retireCaptureArtifact(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, "rollback", intent, files)
				require.NoError(t, err)
			}
			unfile := func() { files.mu.Lock(); files.kept[intent] = false; files.mu.Unlock() }
			if keptFirst {
				// peer read the page, then j heard "kept" and recorded it before
				// the owner unfiled the file and peer's store call retired it.
				files.beforeRetire = func() { retire(j); unfile() }
				retire(peer)
			} else {
				// j heard "kept"; before j recorded it, the owner unfiled the
				// file and peer retired it and recorded its receipt.
				files.afterRetire = func() { unfile(); retire(peer) }
				retire(j)
			}
			r, err = peer.get(ctx, scope.InstallationID, r.ID)
			require.NoError(t, err)
			entry := r.Retirement.Captures["rollback"].Page[0]
			require.NotEmpty(t, entry.ReceiptDigest)
			require.False(t, entry.Retained, "retired bytes cannot be kept")
			require.False(t, entry.Purged)
			_, err = j.purgeRetiredCaptureArtifact(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, "rollback", intent, files)
			require.NoError(t, err)
			require.Equal(t, map[string]int{intent: 1}, files.purged)
		})
	}
}

func TestPreparationRetirementAuthorityHistoryAndParallelPageReceipts(t *testing.T) {
	db, peerDB := journalDB(t)
	j, peer := preparationConnection(db), preparationConnection(peerDB)
	scope, _ := preparationFixture(t)
	ctx := captureOperator(auth.RoleOwner, scope.RequestedBy)
	cleanup := "memql-id:" + strings.Repeat("e", 64)
	r, err := j.reserve(ctx, scope)
	require.NoError(t, err)
	_, err = j.beginRetirement(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup)
	require.NoError(t, err)
	files := retirementFiles(scope)
	executor := &retirementStopFixture{}
	for _, bad := range []context.Context{context.Background(), operator(auth.RoleOwner, scope.RequestedBy), captureOperator(auth.RoleReader, scope.RequestedBy), captureOperator(auth.RoleOwner, "other")} {
		_, err = j.stopRetiringProducer(bad, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, executor)
		require.Error(t, err)
		_, err = j.fenceRetiringCapture(bad, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, "candidate", files)
		require.Error(t, err)
	}
	_, err = j.stopRetiringProducer(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, scope.ConfigurationDigest, executor)
	require.Error(t, err)
	_, err = j.beginRetirement(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, scope.ConfigurationDigest)
	require.Error(t, err)
	require.Zero(t, executor.calls)
	require.Zero(t, files.calls)
	_, err = j.stopRetiringProducer(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, executor)
	require.NoError(t, err)
	_, err = j.fenceRetiringCapture(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, "candidate", files)
	require.NoError(t, err)
	_, err = j.releaseRetiringCapturePins(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, "candidate", files)
	require.NoError(t, err)
	r, err = j.nextRetiringArtifacts(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, "candidate", files)
	require.NoError(t, err)
	results := make(chan error, 2)
	for n, a := range r.Retirement.Captures["candidate"].Page {
		writer := j
		if n%2 == 0 {
			writer = peer
		}
		go func() {
			_, err := writer.retireCaptureArtifact(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, "candidate", a.IntentID, files)
			results <- err
		}()
	}
	for range 2 {
		require.NoError(t, <-results)
	}
	for n, a := range r.Retirement.Captures["candidate"].Page {
		writer := j
		if n%2 == 1 {
			writer = peer
		}
		go func() {
			_, err := writer.purgeRetiredCaptureArtifact(ctx, scope.InstallationID, r.ID, scope.WorkflowDigest, cleanup, "candidate", a.IntentID, files)
			results <- err
		}()
	}
	for range 2 {
		require.NoError(t, <-results)
	}
	r, err = peer.get(ctx, scope.InstallationID, r.ID)
	require.NoError(t, err)
	for _, a := range r.Retirement.Captures["candidate"].Page {
		require.NotEmpty(t, a.ReceiptDigest)
		require.True(t, a.Purged, "parallel removals on two connections keep each other's receipts")
	}
	body, err := os.ReadFile(retirementMigrationPath + ".down.sql")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, string(body))
	require.ErrorContains(t, err, "cannot remove installation preparation retirement history")
	_, err = db.ExecContext(ctx, `UPDATE installation_preparations SET retirement=jsonb_set(retirement,'{captures,candidate,complete}','true') WHERE preparation_id=$1`, r.ID)
	require.NoError(t, err)
	_, err = peer.get(ctx, scope.InstallationID, r.ID)
	require.Error(t, err, "invented completion must not hide a retained artifact page")
}
