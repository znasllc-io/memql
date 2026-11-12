package accounts

// export_object_db_test.go -- the file an attempt committed is never left
// without a record: the key is written on the row once the commit lands, and
// every path that ends or requeues a run without its node (the take-over, the
// claim's attempt limit, the hand-back) deletes it first. Also: a cancelled run
// handed back ends cancelled, the sweep on prepare, and the start email's log.
// Fixture: export_db_test.go.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/integrations/email"
)

// committedThenStopped claims a started export on agent-a, commits a file for
// the attempt and records its key the way the run does, and then does nothing
// more: the node stopped with no cleanup. It answers the claimed row and key.
func (f *exportFixture) committedThenStopped(t *testing.T, id string) (Export, string) {
	t.Helper()
	ctx := context.Background()
	x, _, ok, err := f.x.claim(ctx, "agent-a")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, id, x.ID)
	key := fmt.Sprintf("exports/accounts/%s/%d/acme.tar", id, x.Attempt)
	f.blobs.mu.Lock()
	f.blobs.content[key] = []byte("a whole file")
	f.blobs.mu.Unlock()
	_, err = f.db.ExecContext(ctx, `UPDATE account_exports SET object_key = ? WHERE id = ?`, key, id)
	require.NoError(t, err)
	return x, key
}

func (f *exportFixture) stale(t *testing.T, id string) {
	t.Helper()
	_, err := f.db.ExecContext(context.Background(), `UPDATE account_exports SET heartbeat_at = clock_timestamp() - interval '3 minutes' WHERE id = ?`, id)
	require.NoError(t, err)
}

func (f *exportFixture) holds(key string) bool {
	f.blobs.mu.Lock()
	defer f.blobs.mu.Unlock()
	_, ok := f.blobs.content[key]
	return ok
}

func TestExportTakeOverDeletesTheFileAStoppedNodeCommitted(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	id := f.prepare(t, acme, true)["id"].(string)
	f.start(t, id)
	_, key := f.committedThenStopped(t, id)
	f.stale(t, id)

	// Another node takes the run over.
	require.NoError(t, f.x.reclaimStale(context.Background()))

	require.False(t, f.holds(key), "the store holds no object of the stopped attempt")
	require.Contains(t, f.blobs.deletedKeys(), key)
	x, sealed := f.row(t, id)
	require.Equal(t, "queued", x.State)
	require.Equal(t, "", x.ObjectKey, "the row's key is clear")
	require.True(t, sealed.Valid, "a requeued run keeps its passphrase")
}

func TestExportRunEndedAtItsAttemptLimitDeletesItsFile(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	id := f.prepare(t, acme, true)["id"].(string)
	f.start(t, id)
	_, err := f.db.ExecContext(context.Background(), `UPDATE account_exports SET attempt = 2 WHERE id = ?`, id)
	require.NoError(t, err)
	_, key := f.committedThenStopped(t, id) // attempt 3
	f.stale(t, id)
	require.NoError(t, f.x.reclaimStale(context.Background()))

	require.False(t, f.holds(key))
	x, sealed := f.row(t, id)
	require.Equal(t, "failed", x.State)
	require.Equal(t, "", x.ObjectKey)
	require.False(t, sealed.Valid)
	require.Len(t, f.exec.audits("account_export_failed"), 1)
}

func TestExportClaimAtTheAttemptLimitDeletesARecordedFile(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	id := f.prepare(t, acme, true)["id"].(string)
	f.start(t, id)
	key := "exports/accounts/" + id + "/3/acme.tar"
	f.blobs.content[key] = []byte("a whole file")
	_, err := f.db.ExecContext(context.Background(), `UPDATE account_exports SET attempt = 3, object_key = ? WHERE id = ?`, key, id)
	require.NoError(t, err)
	_, _, ok, err := f.x.claim(context.Background(), "agent-a")
	require.NoError(t, err)
	require.False(t, ok)

	require.False(t, f.holds(key))
	x, sealed := f.row(t, id)
	require.Equal(t, "failed", x.State)
	require.Equal(t, "", x.ObjectKey)
	require.False(t, sealed.Valid)
}

func TestExportHandBackDeletesTheFileItsAttemptCommitted(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	id := f.prepare(t, acme, true)["id"].(string)
	f.start(t, id)
	x, key := f.committedThenStopped(t, id)
	ok, err := f.x.requeue(context.Background(), x, "agent-a")
	require.NoError(t, err)
	require.True(t, ok)

	require.False(t, f.holds(key))
	got, sealed := f.row(t, id)
	require.Equal(t, "queued", got.State)
	require.Equal(t, "", got.ObjectKey)
	require.True(t, sealed.Valid)
}

// (d) A run that would be requeued keeps its key and stays where it is while
// its delete fails, and is requeued on the tick after the delete succeeds. The
// log names the export.
func TestExportTakeOverWhoseDeleteFailsTriesAgain(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	id := f.prepare(t, acme, true)["id"].(string)
	f.start(t, id)
	x, key := f.committedThenStopped(t, id)
	f.setFailDelete(true)

	// The hand-back does not requeue a run whose file it could not delete.
	ok, err := f.x.requeue(context.Background(), x, "agent-a")
	require.NoError(t, err)
	require.False(t, ok)
	got, _ := f.row(t, id)
	require.Equal(t, "running", got.State)
	require.Equal(t, key, got.ObjectKey)

	f.stale(t, id)
	require.NoError(t, f.x.reclaimStale(context.Background()))
	got, sealed := f.row(t, id)
	require.Equal(t, "running", got.State, "a run whose file is still there is not requeued")
	require.Equal(t, key, got.ObjectKey)
	require.True(t, f.holds(key))
	require.Contains(t, f.logs.String(), id)
	require.NotContains(t, f.logs.String(), sealed.String)
	_, renewed, err := f.x.heartbeat(context.Background(), x, "agent-a")
	require.NoError(t, err)
	require.False(t, renewed, "the stopped node's lease is taken before its file is deleted")

	f.setFailDelete(false)
	require.NoError(t, f.x.reclaimStale(context.Background()))
	got, _ = f.row(t, id)
	require.Equal(t, "queued", got.State)
	require.Equal(t, "", got.ObjectKey)
	require.False(t, f.holds(key))
}

// The run records its key with a statement guarded on its own attempt.
func TestExportRecordObjectIsGuardedOnTheAttempt(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	id := f.prepare(t, acme, false)["id"].(string)
	f.start(t, id)
	x, _, ok, err := f.x.claim(context.Background(), "agent-a")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "agent-a", x.ClaimedBy)

	other := x
	other.ClaimedBy = "agent-b"
	recorded, err := f.x.recordObject(context.Background(), other, "exports/accounts/"+id+"/1/f")
	require.NoError(t, err)
	require.False(t, recorded, "another node's attempt records nothing")
	got, _ := f.row(t, id)
	require.Equal(t, "", got.ObjectKey)

	recorded, err = f.x.recordObject(context.Background(), x, "exports/accounts/"+id+"/1/f")
	require.NoError(t, err)
	require.True(t, recorded)
	got, _ = f.row(t, id)
	require.Equal(t, "exports/accounts/"+id+"/1/f", got.ObjectKey)

	// A key recorded on a row that is not complete is never served.
	_, served, err := f.x.ResolveExportForDownload(context.Background(), id)
	require.NoError(t, err)
	require.False(t, served, "a running row's key is not served")
	_, err = f.db.ExecContext(context.Background(), `UPDATE account_exports SET state = 'failed' WHERE id = ?`, id)
	require.NoError(t, err)
	_, served, err = f.x.ResolveExportForDownload(context.Background(), id)
	require.NoError(t, err)
	require.False(t, served, "a failed row's key is not served")
}

// ---------------------------------------------------------------------------
// A cancelled run that is handed back ends cancelled

func TestExportHandBackOfACancelledRunEndsCancelled(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	id := f.prepare(t, acme, true)["id"].(string)
	f.start(t, id)
	x, _, ok, err := f.x.claim(context.Background(), "agent-a")
	require.NoError(t, err)
	require.True(t, ok)
	_, err = f.db.ExecContext(context.Background(), `UPDATE account_exports SET cancel_requested = true WHERE id = ?`, id)
	require.NoError(t, err)
	ok, err = f.x.requeue(context.Background(), x, "agent-a")
	require.NoError(t, err)
	require.True(t, ok)
	got, sealed := f.row(t, id)
	require.Equal(t, "cancelled", got.State)
	require.NotNil(t, got.FinishedAt)
	require.False(t, sealed.Valid)
	require.Len(t, f.exec.audits("account_export_cancelled"), 1)
}

func TestExportClaimEndsAQueuedRunWhoseCancelWasRequested(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	id := f.prepare(t, acme, true)["id"].(string)
	f.start(t, id)
	_, err := f.db.ExecContext(context.Background(), `UPDATE account_exports SET cancel_requested = true, attempt = 3 WHERE id = ?`, id)
	require.NoError(t, err)
	_, _, ok, err := f.x.claim(context.Background(), "agent-a")
	require.NoError(t, err)
	require.False(t, ok, "a cancelled run is not claimed")
	got, sealed := f.row(t, id)
	require.Equal(t, "cancelled", got.State, "cancelled, not failed at its attempt limit")
	require.Equal(t, "", got.FailureCode)
	require.False(t, sealed.Valid)
	require.Len(t, f.exec.audits("account_export_cancelled"), 1)
	require.Empty(t, f.exec.audits("account_export_failed"))
}

// ---------------------------------------------------------------------------
// The sweep on prepare, the start email's log, and the words

func TestExportPrepareSweepsExpiredDrafts(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	old := f.prepare(t, acme, true)["id"].(string)
	_, err := f.db.ExecContext(context.Background(), `UPDATE account_exports SET requested_at = clock_timestamp() - interval '16 minutes' WHERE id = ?`, old)
	require.NoError(t, err)
	// No runner runs. Preparing again sweeps the draft that was never started,
	// and its sealed passphrase with it.
	fresh := f.prepare(t, acme, true)["id"].(string)
	require.NotEqual(t, old, fresh)
	_, ok, err := f.x.get(context.Background(), old)
	require.NoError(t, err)
	require.False(t, ok)
}

type failingSender struct{}

func (failingSender) Send(context.Context, email.Message, email.SendAs) error {
	return errors.New("the mail provider refused")
}

func TestExportStartEmailFailureLogsNoAddress(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	addresses := []string{"first.owner@example.test", "second.owner@example.test"}
	notify := newOwnerNotice(func(context.Context) ([]string, error) { return addresses, nil },
		func() email.Sender { return failingSender{} }, logger)
	notify(context.Background(), Export{ID: "aex-mail", AccountID: "acme", AccountName: "Acme"}, "someone")
	out := buf.String()
	for _, a := range addresses {
		require.NotContains(t, out, a)
		require.NotContains(t, out, strings.Split(a, "@")[0])
	}
	require.Contains(t, out, "aex-mail")
	require.Contains(t, out, "failed=2")
}

func TestExportNotReadySaysEachConditionInTheTabsWords(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	f.mu.Lock()
	f.follow = 1
	f.undecl = []string{"v1:testa:loose"}
	f.mu.Unlock()
	_, err := f.x.handlePrepare(person(exportPerson), map[string]any{"accountId": acme}, 0)
	require.Error(t, err)
	require.Equal(t, "export_not_ready: This account cannot be exported yet. Not met: Every kind says whose it is; Rows following a moved parent.", err.Error())
}

// ---------------------------------------------------------------------------
// The parsed-secret cache is bounded

func TestSecretRulesCacheIsBounded(t *testing.T) {
	secretCache.reset()
	for i := 0; i < secretCacheMax+50; i++ {
		rulesFor(&memorynodes.Concept{Name: fmt.Sprintf("v1:testa:c%d", i)})
		require.LessOrEqual(t, secretCache.size(), secretCacheMax)
	}
	c := &memorynodes.Concept{Name: "v1:testa:kept"}
	require.Same(t, rulesFor(c), rulesFor(c), "an entry is reused while it is held")
}

// ---------------------------------------------------------------------------
// Ending a row never waits on the delete; requeueing one does

// (a) A stale run with a cancel requested whose delete fails ends cancelled
// and keeps its key, and the queue moves on.
func TestExportStaleCancelledRunWhoseDeleteFailsEndsAndFreesTheQueue(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	globex := f.account(t, "aex-globex", "Globex")
	id := f.prepare(t, acme, true)["id"].(string)
	f.start(t, id)
	_, key := f.committedThenStopped(t, id)
	next := f.prepare(t, globex, false)["id"].(string)
	f.start(t, next)
	f.setFailDelete(true)
	_, err := f.x.handleCancel(person(exportPerson), map[string]any{"exportId": id}, 0)
	require.NoError(t, err)
	f.stale(t, id)
	require.NoError(t, f.x.reclaimStale(context.Background()))

	got, sealed := f.row(t, id)
	require.Equal(t, "cancelled", got.State)
	require.Equal(t, key, got.ObjectKey, "the key stays while its file is there")
	require.False(t, sealed.Valid)
	require.True(t, f.holds(key))

	claimed, _, ok, err := f.x.claim(context.Background(), "agent-b")
	require.NoError(t, err)
	require.True(t, ok, "the next export is claimed")
	require.Equal(t, next, claimed.ID)
}

// (b) A stale run past its attempt limit whose delete fails ends failed and
// keeps its key; so does a queued row the claim ends.
func TestExportRunEndedAtItsLimitWhoseDeleteFailsEndsAndKeepsItsKey(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	id := f.prepare(t, acme, true)["id"].(string)
	f.start(t, id)
	_, err := f.db.ExecContext(context.Background(), `UPDATE account_exports SET attempt = 2 WHERE id = ?`, id)
	require.NoError(t, err)
	_, key := f.committedThenStopped(t, id)
	f.setFailDelete(true)
	f.stale(t, id)
	require.NoError(t, f.x.reclaimStale(context.Background()))
	got, sealed := f.row(t, id)
	require.Equal(t, "failed", got.State)
	require.Equal(t, key, got.ObjectKey)
	require.False(t, sealed.Valid)

	globex := f.account(t, "aex-globex", "Globex")
	queued := f.prepare(t, globex, true)["id"].(string)
	f.start(t, queued)
	qkey := "exports/accounts/" + queued + "/3/globex.tar"
	f.blobs.mu.Lock()
	f.blobs.content[qkey] = []byte("a whole file")
	f.blobs.mu.Unlock()
	_, err = f.db.ExecContext(context.Background(), `UPDATE account_exports SET attempt = 3, object_key = ? WHERE id = ?`, qkey, queued)
	require.NoError(t, err)
	_, _, ok, err := f.x.claim(context.Background(), "agent-a")
	require.NoError(t, err)
	require.False(t, ok)
	got, sealed = f.row(t, queued)
	require.Equal(t, "failed", got.State, "the claim ends the row whether or not its file could be deleted")
	require.Equal(t, qkey, got.ObjectKey)
	require.False(t, sealed.Valid)
}

// The hand-back of a cancelled run ends it whether or not the delete succeeds.
func TestExportHandBackOfACancelledRunWhoseDeleteFailsEndsCancelled(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	id := f.prepare(t, acme, true)["id"].(string)
	f.start(t, id)
	x, key := f.committedThenStopped(t, id)
	_, err := f.db.ExecContext(context.Background(), `UPDATE account_exports SET cancel_requested = true WHERE id = ?`, id)
	require.NoError(t, err)
	f.setFailDelete(true)
	ok, err := f.x.requeue(context.Background(), x, "agent-a")
	require.NoError(t, err)
	require.True(t, ok)
	got, sealed := f.row(t, id)
	require.Equal(t, "cancelled", got.State)
	require.Equal(t, key, got.ObjectKey)
	require.False(t, sealed.Valid)
}

// (c) The runner's sweep deletes a kept file once storage answers, and clears
// the key, for a failed row and a cancelled row.
func TestExportSweepDeletesKeptFilesOfEndedRows(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	globex := f.account(t, "aex-globex", "Globex")
	keys := map[string]string{}
	for _, c := range []struct{ account, state string }{{acme, "failed"}, {globex, "cancelled"}} {
		id := f.prepare(t, c.account, false)["id"].(string)
		key := "exports/accounts/" + id + "/1/kept.tar"
		f.blobs.mu.Lock()
		f.blobs.content[key] = []byte("a whole file")
		f.blobs.mu.Unlock()
		_, err := f.db.ExecContext(context.Background(), `UPDATE account_exports SET state = ?, object_key = ?, finished_at = clock_timestamp() WHERE id = ?`, c.state, key, id)
		require.NoError(t, err)
		keys[id] = key
	}
	r := f.runner(t, "agent-a", nil)

	f.setFailDelete(true)
	r.tick(context.Background())
	for id, key := range keys {
		got, _ := f.row(t, id)
		require.Equal(t, key, got.ObjectKey, "a failed delete keeps the key")
		require.True(t, f.holds(key))
		require.Contains(t, f.logs.String(), id)
	}

	f.setFailDelete(false)
	r.tick(context.Background())
	for id, key := range keys {
		got, _ := f.row(t, id)
		require.Equal(t, "", got.ObjectKey)
		require.False(t, f.holds(key))
		_, served, err := f.x.ResolveExportForDownload(context.Background(), id)
		require.NoError(t, err)
		require.False(t, served)
	}
}

func (f *exportFixture) setFailDelete(fail bool) {
	f.blobs.mu.Lock()
	defer f.blobs.mu.Unlock()
	f.blobs.failDelete = fail
}
