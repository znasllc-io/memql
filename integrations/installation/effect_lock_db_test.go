package installation

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestInstallationEffectLockLeavesJournalCapacityAcrossCompetingHosts(t *testing.T) {
	db, peerDB := journalDB(t)
	db.SetMaxOpenConns(2)
	peerDB.SetMaxOpenConns(2)
	first := revisionJournal{db: func() *sql.DB { return db }}
	peer := revisionJournal{db: func() *sql.DB { return peerDB }}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	key := "memql-id:" + strings.Repeat("a", 64)
	other := "memql-id:" + strings.Repeat("b", 64)
	entered, proceed := make(chan struct{}), make(chan struct{})
	results := make(chan error, 4)
	var active atomic.Int32
	journalRead := func(ctx context.Context, pool *sql.DB) (any, error) {
		var value int
		err := pool.QueryRowContext(ctx, `SELECT 1`).Scan(&value)
		return value, err
	}
	go func() {
		_, err := first.withEffectLock(ctx, key, func(ctx context.Context) (any, error) {
			active.Add(1)
			defer active.Add(-1)
			close(entered)
			select {
			case <-proceed:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return journalRead(ctx, db)
		})
		results <- err
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for _, competingKey := range []string{key, other} {
		go func() {
			_, err := first.withEffectLock(ctx, competingKey, func(ctx context.Context) (any, error) {
				return journalRead(ctx, db)
			})
			results <- err
		}()
	}
	go func() {
		_, err := peer.withEffectLock(ctx, key, func(ctx context.Context) (any, error) {
			if active.Load() != 0 {
				return nil, errors.New("another host entered the same effect")
			}
			return journalRead(ctx, peerDB)
		})
		results <- err
	}()
	require.Eventually(t, func() bool {
		effectPools.Lock()
		defer effectPools.Unlock()
		p, otherPool := effectPools.entries[db], effectPools.entries[peerDB]
		return p != nil && p.refs == 3 && otherPool != nil && otherPool.refs == 1
	}, time.Second, time.Millisecond)
	// Both same-plan and distinct-plan waiters share this bounded pool. Neither
	// may reserve its second connection while the holder needs a journal read.
	var value int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT 1`).Scan(&value))
	close(proceed)
	for range 4 {
		require.NoError(t, <-results)
	}
	effectPools.Lock()
	defer effectPools.Unlock()
	require.Nil(t, effectPools.entries[db])
	require.Nil(t, effectPools.entries[peerDB])
}

func TestInstallationEffectLockCancellationReleasesPoolAndCanRetry(t *testing.T) {
	db, peerDB := journalDB(t)
	db.SetMaxOpenConns(2)
	journal := revisionJournal{db: func() *sql.DB { return db }}
	key := "memql-id:" + strings.Repeat("c", 64)
	blocker, err := peerDB.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = blocker.Rollback() })
	_, err = blocker.ExecContext(t.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "installation-effect:"+key)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err = journal.withEffectLock(ctx, key, func(context.Context) (any, error) {
		t.Error("cancelled lock waiter entered its effect")
		return nil, nil
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Eventually(t, func() bool { return db.Stats().InUse == 0 }, time.Second, time.Millisecond)
	require.NoError(t, blocker.Rollback())
	_, err = journal.withEffectLock(t.Context(), key, func(ctx context.Context) (any, error) {
		return db.ExecContext(ctx, `SELECT 1`)
	})
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	_, err = journal.withEffectLock(t.Context(), key, func(context.Context) (any, error) {
		t.Error("insufficient journal capacity entered its effect")
		return nil, nil
	})
	require.ErrorContains(t, err, "concurrent journal connection")
}
