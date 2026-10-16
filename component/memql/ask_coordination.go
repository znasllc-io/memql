package memql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/znasllc-io/memql/core/id"
)

// SetAskCoordinationPool wires the existing bounded write-gate pool at boot.
// Ask must never reserve request connections while waiting for its own reads.
func (e *MemQLEngine) SetAskCoordinationPool(resolve func() *sql.DB) {
	e.askCoordinationPool = resolve
}

const (
	askObserverNamespace = "memql.ask.observer"
	askObserverTTL       = 2 * time.Minute
	askObserverHeartbeat = 20 * time.Second
	askCoordinationIO    = 3 * time.Second
)

type askConversationHold struct {
	conn *sql.Conn
	tx   *sql.Tx
	key  int64
	once sync.Once
	done atomic.Bool

	// Observers keep a renewable row, not a connection. The random token is
	// per acquisition: a late release can never remove a successor's lease.
	db        *sql.DB
	token     string
	leaseCtx  context.Context
	loseLease context.CancelCauseFunc
	stop      chan struct{}
	stopped   chan struct{}
}

func (e *MemQLEngine) holdAskConversation(ctx context.Context, conversationID, kind string) (*askConversationHold, error) {
	_, bareID, err := id.ParseNodeId(conversationID)
	if err != nil {
		return nil, err
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte("ask:" + kind + ":" + bareID))
	key := int64(hash.Sum64())
	if kind == "voice" || kind == "turn" || strings.HasPrefix(kind, "observer:") {
		return e.claimAskObserver(ctx, key)
	}
	return e.holdAskWriter(ctx, key, kind == "write" || kind == "memory-index")
}

func (e *MemQLEngine) holdAskWriter(ctx context.Context, key int64, wait bool) (*askConversationHold, error) {
	db, err := ResolveWriteGatePool(e.askCoordinationPool, "Ask", "wire SetAskCoordinationPool with database.Database.GateDB")
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(writeGateWait)
	if caller, ok := ctx.Deadline(); ok && caller.Before(deadline) {
		deadline = caller.Add(-gateCallerMargin)
	}
	acquire, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if err = checkGatePool(acquire, db); err != nil {
		return nil, gateWaitError(err, acquire)
	}
	conn, err := db.Conn(acquire)
	if err != nil {
		return nil, gateWaitError(err, acquire)
	}
	// A canceled request may still finish its bounded transcript commit. Its
	// lock must outlive that request, until the caller explicitly releases it.
	tx, err := conn.BeginTx(context.WithoutCancel(ctx), nil)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	hold := &askConversationHold{conn: conn, tx: tx, key: key}
	remaining := time.Until(deadline)
	if remaining <= 0 {
		hold.release()
		return nil, fmt.Errorf("%w: Ask lock acquisition timed out", ErrWriteGateBusy)
	}
	lockCtx, cancelLock := context.WithDeadline(ctx, deadline.Add(writeGateGrace))
	defer cancelLock()
	_, err = tx.ExecContext(lockCtx, "SELECT set_config('lock_timeout', $1, true)", strconv.FormatInt(max(1, remaining.Milliseconds()), 10))
	var held bool
	if err == nil {
		// Keep the original bigint key: these locks serialize with an older
		// replica's session-scoped transcript lock during a rolling update.
		if wait {
			_, err = tx.ExecContext(lockCtx, "SELECT pg_advisory_xact_lock($1)", key)
			held = err == nil
		} else {
			err = tx.QueryRowContext(lockCtx, "SELECT pg_try_advisory_xact_lock($1)", key).Scan(&held)
		}
	}
	if err != nil || !held {
		hold.release()
		if err != nil {
			return nil, gateWaitError(err, lockCtx, acquire)
		}
		return nil, fmt.Errorf("MemQL is already working in this conversation")
	}
	return hold, nil
}

func (e *MemQLEngine) claimAskObserver(ctx context.Context, key int64) (*askConversationHold, error) {
	db := e.database()
	if db == nil {
		return nil, fmt.Errorf("conversation storage is unavailable")
	}
	h := &askConversationHold{db: db.DB, key: key, token: id.NewShortId(), stop: make(chan struct{}), stopped: make(chan struct{})}
	query, cancel := context.WithTimeout(ctx, askCoordinationIO)
	defer cancel()
	var token string
	err := h.db.QueryRowContext(query, `INSERT INTO automation_execution_claims
		(automation_name, dedup_key, claimed_by, claimed_at, expires_at)
		VALUES ($1, $2, $3, clock_timestamp(), clock_timestamp() + $4 * interval '1 second')
		ON CONFLICT (automation_name, dedup_key) DO UPDATE
		SET claimed_by=EXCLUDED.claimed_by, claimed_at=EXCLUDED.claimed_at, expires_at=EXCLUDED.expires_at
		WHERE automation_execution_claims.expires_at <= clock_timestamp()
		RETURNING claimed_by`, askObserverNamespace, strconv.FormatInt(key, 10), h.token, int(askObserverTTL/time.Second)).Scan(&token)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("MemQL is already working in this conversation")
	}
	if err != nil {
		return nil, err
	}
	h.leaseCtx, h.loseLease = context.WithCancelCause(context.Background())
	go h.heartbeat()
	return h, nil
}

func (h *askConversationHold) heartbeat() {
	defer close(h.stopped)
	ticker := time.NewTicker(askObserverHeartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-ticker.C:
			if err := h.renew(); err != nil {
				h.loseLease(err)
				return
			}
		}
	}
}

func (h *askConversationHold) renew() (err error) {
	defer func() {
		if err != nil {
			h.loseLease(err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), askCoordinationIO)
	defer cancel()
	result, err := h.db.ExecContext(ctx, `UPDATE automation_execution_claims
		SET claimed_at=clock_timestamp(), expires_at=clock_timestamp() + $4 * interval '1 second'
		WHERE automation_name=$1 AND dedup_key=$2 AND claimed_by=$3 AND expires_at > clock_timestamp()`,
		askObserverNamespace, strconv.FormatInt(h.key, 10), h.token, int(askObserverTTL/time.Second))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrWriteGateLost, err)
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return ErrWriteGateLost
	}
	return nil
}

type askObserverContextKey struct{}

func (h *askConversationHold) observationContext(ctx context.Context) context.Context {
	observing, cancel := context.WithCancelCause(context.WithValue(ctx, askObserverContextKey{}, h))
	stop := context.AfterFunc(h.leaseCtx, func() { cancel(context.Cause(h.leaseCtx)) })
	context.AfterFunc(observing, func() { stop() })
	return observing
}

func (h *askConversationHold) verify(ctx context.Context) error {
	if h == nil || h.done.Load() {
		return ErrWriteGateLost
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var held bool
	var err error
	if h.db != nil {
		query, cancel := context.WithTimeout(ctx, askCoordinationIO)
		defer cancel()
		err = h.db.QueryRowContext(query, `SELECT EXISTS (SELECT 1 FROM automation_execution_claims
			WHERE automation_name=$1 AND dedup_key=$2 AND claimed_by=$3 AND expires_at > clock_timestamp())`,
			askObserverNamespace, strconv.FormatInt(h.key, 10), h.token).Scan(&held)
	} else {
		err = h.tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype='advisory' AND granted AND pid=pg_backend_pid() AND objsubid=1 AND classid::bigint=(($1::bigint >> 32) & 4294967295) AND objid::bigint=($1::bigint & 4294967295))`, h.key).Scan(&held)
	}
	if err != nil {
		return fmt.Errorf("%w: %v", ErrWriteGateLost, err)
	}
	if !held {
		return ErrWriteGateLost
	}
	return nil
}

func (h *askConversationHold) release() {
	if h == nil {
		return
	}
	h.once.Do(func() {
		h.done.Store(true)
		if h.db == nil {
			_ = h.tx.Rollback()
			_ = h.conn.Close()
			return
		}
		close(h.stop)
		<-h.stopped
		h.loseLease(context.Canceled)
		ctx, cancel := context.WithTimeout(context.Background(), askCoordinationIO)
		defer cancel()
		// Failed cleanup is safe: the row expires, and automation claim pruning
		// removes expired rows. Never delete a lease acquired by a successor.
		_, _ = h.db.ExecContext(ctx, `DELETE FROM automation_execution_claims
			WHERE automation_name=$1 AND dedup_key=$2 AND claimed_by=$3`,
			askObserverNamespace, strconv.FormatInt(h.key, 10), h.token)
	})
}
