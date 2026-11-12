package accounts

// export_fix_db_test.go -- the cases the first review asked for: an unknown
// account, the owners' email off the request, the node that fences itself,
// the result nothing records, the expired draft, the people's names, the
// attempt limit at the claim, two claims at once, the hand-back on stop, and a
// passphrase that cannot be opened. Fixture: export_db_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/znasllc-io/memql/integrations/email"
)

func (f *fakeBlobs) deletedKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.deleted...)
}

// user writes a person's row so their name can be resolved.
func (f *exportFixture) user(t *testing.T, id, displayName string) {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"displayName": displayName, "primaryEmail": "p@example.test"})
	require.NoError(t, err)
	_, err = f.db.ExecContext(context.Background(), `DELETE FROM "MemoryNodes" WHERE concept = 'v1:identity:user' AND id = ?`, id)
	require.NoError(t, err)
	_, err = f.db.ExecContext(context.Background(),
		`INSERT INTO "MemoryNodes" (id, "createdAt", "createdBy", schema, payload, concept) VALUES (?, clock_timestamp(), 'test', '{}', ?, 'v1:identity:user')`,
		id, string(encoded))
	require.NoError(t, err)
}

// 1. An unknown account is refused, never answered ready.
func TestExportReadinessRefusesAnUnknownAccount(t *testing.T) {
	f := newExportFixture(t)
	_, err := f.x.handleReadiness(context.Background(), map[string]any{"accountId": "aex-made-up"}, 0)
	require.Error(t, err)
	require.Contains(t, err.Error(), `there is no account "aex-made-up"`)

	// The primary account always exists, row or not.
	nodes, err := f.x.handleReadiness(context.Background(), map[string]any{"accountId": "self"}, 0)
	require.NoError(t, err)
	require.Equal(t, true, conditions(t, nodes)["ready"]["ready"])
}

type blockingSender struct{ release chan struct{} }

func (b *blockingSender) Send(ctx context.Context, _ email.Message, _ email.SendAs) error {
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// 2. Start answers at once while the mail provider hangs.
func TestExportStartDoesNotWaitOnTheOwnersEmail(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	sender := &blockingSender{release: make(chan struct{})}
	defer func() { close(sender.release); f.x.notices.Wait() }()
	f.x.notifyOwners = newOwnerNotice(func(context.Context) ([]string, error) {
		return []string{"one@example.test"}, nil
	}, func() email.Sender { return sender }, f.x.logger)
	id := f.prepare(t, acme, false)["id"].(string)
	began := time.Now()
	started := f.start(t, id)
	require.Equal(t, "queued", started["state"])
	require.Less(t, time.Since(began), 2*time.Second)
}

// 3. A node whose heartbeat keeps failing cancels its own work before the
// stale window ends, and records nothing.
func TestExportRunnerFencesItselfWhenItCannotRenew(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	id := f.prepare(t, acme, false)["id"].(string)
	f.start(t, id)
	cancelled := make(chan time.Time, 1)
	r := f.runner(t, "agent-a", func(ctx context.Context, x Export, _ string, _ func(ExportProgress)) (ExportResult, error) {
		<-ctx.Done()
		cancelled <- time.Now()
		return ExportResult{ObjectKey: "exports/accounts/" + x.ID + "/1/f", FileName: "f", SizeBytes: 1}, nil
	})
	r.heartbeatEvery = 20 * time.Millisecond
	r.fenceAfter = 300 * time.Millisecond
	var calls int
	var mu sync.Mutex
	r.renew = func(context.Context, Export, string) (bool, bool, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return false, false, errors.New("the database is unreachable")
	}
	began := time.Now()
	r.tick(context.Background())
	select {
	case at := <-cancelled:
		require.Less(t, at.Sub(began), exportStaleAfter, "fenced before the stale window")
		require.GreaterOrEqual(t, at.Sub(began), r.fenceAfter)
	case <-time.After(10 * time.Second):
		t.Fatal("the work was never cancelled")
	}
	r.wait()
	mu.Lock()
	require.Greater(t, calls, 1)
	mu.Unlock()
	x, _ := f.row(t, id)
	require.Equal(t, "running", x.State, "a fenced node writes nothing; the take-over ends the run")
	require.Equal(t, []string{"exports/accounts/" + id + "/1/f"}, f.blobs.deletedKeys(), "and removes what it could not record")
	require.Contains(t, f.logs.String(), "lease")
}

// 3. Each heartbeat statement has a deadline of its own.
func TestExportRunnerBoundsEachHeartbeat(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	id := f.prepare(t, acme, false)["id"].(string)
	f.start(t, id)
	r := f.runner(t, "agent-a", func(ctx context.Context, _ Export, _ string, _ func(ExportProgress)) (ExportResult, error) {
		<-ctx.Done()
		return ExportResult{}, ctx.Err()
	})
	r.heartbeatEvery = 20 * time.Millisecond
	r.heartbeatTimeout = 50 * time.Millisecond
	r.fenceAfter = 400 * time.Millisecond
	deadlines := make(chan time.Duration, 100)
	r.renew = func(ctx context.Context, _ Export, _ string) (bool, bool, error) {
		d, ok := ctx.Deadline()
		require.True(t, ok, "a heartbeat carries a deadline")
		deadlines <- time.Until(d)
		<-ctx.Done() // a statement that hangs
		return false, false, ctx.Err()
	}
	r.tick(context.Background())
	r.wait()
	got := <-deadlines
	require.LessOrEqual(t, got, 50*time.Millisecond)
	x, _ := f.row(t, id)
	require.Equal(t, "running", x.State)
}

// 4. A cancel that lands after the work finished: the result is not recorded,
// so its file is removed.
func TestExportRunnerRemovesAResultItCannotRecord(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	id := f.prepare(t, acme, false)["id"].(string)
	f.start(t, id)
	r := f.runner(t, "agent-a", func(_ context.Context, x Export, _ string, _ func(ExportProgress)) (ExportResult, error) {
		_, err := f.db.ExecContext(context.Background(), `UPDATE account_exports SET cancel_requested = true WHERE id = ?`, x.ID)
		require.NoError(t, err)
		return ExportResult{ObjectKey: "exports/accounts/" + x.ID + "/1/f", FileName: "f", SizeBytes: 1}, nil
	})
	r.tick(context.Background())
	f.waitState(t, id, "cancelled")
	r.wait()
	require.Equal(t, []string{"exports/accounts/" + id + "/1/f"}, f.blobs.deletedKeys())
}

// 5. Starting a draft that is gone or expired names the code the screen keys on.
func TestExportStartOfAnExpiredDraftSaysSo(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	id := f.prepare(t, acme, false)["id"].(string)
	_, err := f.db.ExecContext(context.Background(), `UPDATE account_exports SET requested_at = clock_timestamp() - interval '16 minutes' WHERE id = ?`, id)
	require.NoError(t, err)
	_, err = f.x.handleStart(person(exportPerson), map[string]any{"exportId": id}, 0)
	require.Error(t, err)
	require.True(t, strings.HasPrefix(err.Error(), "export_draft_expired: "), err.Error())

	_, err = f.x.handleStart(person(exportPerson), map[string]any{"exportId": "aex-gone"}, 0)
	require.Error(t, err)
	require.True(t, strings.HasPrefix(err.Error(), "export_draft_expired: "), err.Error())
}

// 6. The row names the people, resolved when answered.
func TestExportRowsNameThePeople(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	f.user(t, exportPerson, "Pat Exporter")
	prepared := f.prepare(t, acme, false)
	require.Equal(t, "Pat Exporter", prepared["requestedByName"])
	require.Equal(t, "", prepared["removedByName"])
	id := prepared["id"].(string)
	_, err := f.db.ExecContext(context.Background(), `UPDATE account_exports SET state = 'complete', removed_at = clock_timestamp(), removed_by = 'v1:identity:user:nobody' WHERE id = ?`, id)
	require.NoError(t, err)
	status := answer(t)(f.x.handleStatus(person(exportPerson), map[string]any{"exportId": id}, 0))
	require.Equal(t, "Pat Exporter", status["requestedByName"])
	require.Equal(t, "", status["removedByName"], "a person who cannot be resolved has no name")
	nodes, err := f.x.handleList(person(exportPerson), map[string]any{"accountId": acme}, 0)
	require.NoError(t, err)
	require.Equal(t, "Pat Exporter", exportPayloads(t, nodes)[0]["requestedByName"])
}

// 9. The claim refuses a row at or past the attempt limit and fails it.
func TestExportClaimFailsARowPastTheAttemptLimit(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	id := f.prepare(t, acme, true)["id"].(string)
	f.start(t, id)
	_, err := f.db.ExecContext(context.Background(), `UPDATE account_exports SET attempt = 3 WHERE id = ?`, id)
	require.NoError(t, err)
	_, _, ok, err := f.x.claim(context.Background(), "agent-a")
	require.NoError(t, err)
	require.False(t, ok)
	x, sealed := f.row(t, id)
	require.Equal(t, "failed", x.State)
	require.Equal(t, "export_failed", x.FailureCode)
	require.False(t, sealed.Valid)
	require.Len(t, f.exec.audits("account_export_failed"), 1)
}

// 11. Two claims at the same instant over two queued accounts: one claims.
func TestExportTwoClaimsAtOnceClaimOne(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	globex := f.account(t, "aex-globex", "Globex")
	f.start(t, f.prepare(t, acme, false)["id"].(string))
	f.start(t, f.prepare(t, globex, false)["id"].(string))
	for round := 0; round < 5; round++ {
		_, err := f.db.ExecContext(context.Background(), `UPDATE account_exports SET state = 'queued', claimed_by = '', attempt = 0`)
		require.NoError(t, err)
		barrier := make(chan struct{})
		var wg sync.WaitGroup
		var mu sync.Mutex
		claimed := 0
		for _, node := range []string{"agent-a", "agent-b"} {
			wg.Add(1)
			go func(node string) {
				defer wg.Done()
				<-barrier
				_, _, ok, err := f.x.claim(context.Background(), node)
				require.NoError(t, err)
				if ok {
					mu.Lock()
					claimed++
					mu.Unlock()
				}
			}(node)
		}
		close(barrier)
		wg.Wait()
		require.Equal(t, 1, claimed, "round %d", round)
		var running int
		require.NoError(t, f.db.QueryRowContext(context.Background(), `SELECT count(*) FROM account_exports WHERE state = 'running'`).Scan(&running))
		require.Equal(t, 1, running)
	}
}

// 11. A node that stops hands its run back, attempt kept.
func TestExportRunIsHandedBackWhenItsNodeStops(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	id := f.prepare(t, acme, true)["id"].(string)
	f.start(t, id)
	entered := make(chan struct{})
	r := f.runner(t, "agent-a", func(ctx context.Context, _ Export, _ string, _ func(ExportProgress)) (ExportResult, error) {
		close(entered)
		<-ctx.Done()
		return ExportResult{}, ctx.Err()
	})
	ctx, stop := context.WithCancel(context.Background())
	r.tick(ctx)
	<-entered
	stop()
	r.wait()
	x, sealed := f.row(t, id)
	require.Equal(t, "queued", x.State)
	require.Equal(t, 1, x.Attempt)
	require.True(t, sealed.Valid)
	var claimedBy string
	require.NoError(t, f.db.QueryRowContext(context.Background(), `SELECT claimed_by FROM account_exports WHERE id = ?`, id).Scan(&claimedBy))
	require.Equal(t, "", claimedBy)
}

// 11. A passphrase that cannot be opened fails the run and clears the column.
func TestExportRunFailsWhenThePassphraseCannotBeOpened(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	id := f.prepare(t, acme, true)["id"].(string)
	f.start(t, id)
	_, err := f.db.ExecContext(context.Background(), `UPDATE account_exports SET sealed_passphrase = 'not-a-sealed-value' WHERE id = ?`, id)
	require.NoError(t, err)
	ran := false
	r := f.runner(t, "agent-a", func(context.Context, Export, string, func(ExportProgress)) (ExportResult, error) {
		ran = true
		return ExportResult{}, nil
	})
	r.tick(context.Background())
	x := f.waitState(t, id, "failed")
	r.wait()
	require.False(t, ran)
	require.Equal(t, "export_failed", x.FailureCode)
	_, sealed := f.row(t, id)
	require.False(t, sealed.Valid)
}

// 12. A removal whose record could not be updated after the file went says
// to try again.
func TestExportRemoveThatCannotMarkTheRowSaysToTryAgain(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	id := f.prepare(t, acme, false)["id"].(string)
	_, err := f.db.ExecContext(context.Background(), `UPDATE account_exports SET state = 'complete', object_key = 'exports/accounts/k' WHERE id = ?`, id)
	require.NoError(t, err)
	f.x.markRemovedHook = func(context.Context, string, string) (Export, bool, error) {
		return Export{}, false, errors.New("connection reset")
	}
	_, err = f.x.handleRemove(person(exportPerson), map[string]any{"exportId": id}, 0)
	require.Error(t, err)
	require.Contains(t, err.Error(), "try the removal again")
}

// 8. A blob store that failed to build is built again on the next use.
func TestExportBlobStoreIsBuiltAgainAfterAFailure(t *testing.T) {
	calls := 0
	blobs := lazyBlobs(func() (ExportBlobs, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("not yet")
		}
		return &fakeBlobs{}, nil
	})
	_, err := blobs()
	require.Error(t, err)
	b, err := blobs()
	require.NoError(t, err)
	require.NotNil(t, b)
	_, _ = blobs()
	require.Equal(t, 2, calls, "a store that was built is kept")
}
