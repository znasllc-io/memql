package installation

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"
)

// This gate reserves pool capacity only. PostgreSQL, not process memory,
// serializes effects across hosts. At most one effect may retain a connection
// from a pool while its callback borrows another for journal operations.
var effectPools = struct {
	sync.Mutex
	entries map[*sql.DB]*effectPool
}{entries: make(map[*sql.DB]*effectPool)}

type effectPool struct {
	permit chan struct{}
	refs   int
}

func effectPoolPermit(ctx context.Context, db *sql.DB) (func(), error) {
	effectPools.Lock()
	p := effectPools.entries[db]
	if p == nil {
		p = &effectPool{permit: make(chan struct{}, 1)}
		effectPools.entries[db] = p
	}
	p.refs++
	effectPools.Unlock()
	releaseRef := func() {
		effectPools.Lock()
		p.refs--
		if p.refs == 0 {
			delete(effectPools.entries, db)
		}
		effectPools.Unlock()
	}
	select {
	case p.permit <- struct{}{}:
		return func() { <-p.permit; releaseRef() }, nil
	case <-ctx.Done():
		releaseRef()
		return nil, ctx.Err()
	}
}

// Every external Application write and reversal classification uses this same
// database lock. A receiver process disappearing releases it; a live delayed
// request must finish or cancel before another host classifies an untouched
// baseline. The lock is never a grant: the callback reloads current journal
// authority after acquiring it and each Argo write still carries exact CAS.
func (j *revisionJournal) withEffectLock(ctx context.Context, key string, fn func(context.Context) (any, error)) (any, error) {
	if !internalDigest.MatchString(key) || fn == nil {
		return nil, errors.New("installation effect requires its exact plan")
	}
	db, err := j.database()
	if err != nil {
		return nil, err
	}
	if limit := db.Stats().MaxOpenConnections; limit == 1 {
		return nil, errors.New("installation effect coordination requires a concurrent journal connection")
	}
	release, err := effectPoolPermit(ctx, db)
	if err != nil {
		return nil, err
	}
	defer release()
	var tx *sql.Tx
	for {
		tx, err = db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		var acquired bool
		err = tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,0))`, "installation-effect:"+key).Scan(&acquired)
		if err == nil && acquired {
			break
		}
		// A lock waiter must never retain a journal-pool connection. This also
		// lets ordinary journal reads progress while another host owns the lock.
		_ = tx.Rollback()
		if err != nil {
			return nil, err
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	defer tx.Rollback()
	result, err := fn(ctx)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}
