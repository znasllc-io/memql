package storagequota

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var ErrInventoryPending = errors.New("storage inventory is preparing; uploads will retry when accounting is ready")

type inventoryFlight struct {
	done chan struct{}
	err  error
}

// Initialized is a fresh, non-blocking read. Uncommitted partial inventories
// never advertise capacity, even to another process or after a restart.
func (s *Store) Initialized(ctx context.Context, account string) (bool, error) {
	db, err := s.Database()
	if err != nil {
		return false, err
	}
	var ready bool
	var blocked string
	err = db.QueryRowContext(ctx, `SELECT inventoried,blocked_reason FROM storage_capacity_accounts WHERE account=$1`, account).Scan(&ready, &blocked)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if blocked != "" {
		return false, errors.New(blocked)
	}
	return ready, nil
}

// EnsureInitialized keeps the bounded, read-only provider scan independent of
// an HTTP/RPC request, its actor and any borrowed gate. One flight per account
// runs on a node; Initialize's PostgreSQL try-lock coordinates other replicas.
// A process crash rolls the whole import back and the next request retries it.
// Callers wait at most a second, and no storage write may precede readiness.
func (s *Store) EnsureInitialized(ctx context.Context, account string, inventory func(context.Context, func(string, string, int64) error) error) error {
	ready, err := s.Initialized(ctx, account)
	if err != nil || ready {
		return err
	}
	s.inventoryMu.Lock()
	if s.inventories == nil {
		s.inventories = make(map[string]*inventoryFlight)
	}
	f := s.inventories[account]
	if f == nil {
		f = &inventoryFlight{done: make(chan struct{})}
		s.inventories[account] = f
		go func() {
			background, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			f.err = s.Initialize(background, account, func(add func(string, string, int64) error) error { return inventory(background, add) })
			s.inventoryMu.Lock()
			delete(s.inventories, account)
			close(f.done)
			s.inventoryMu.Unlock()
		}()
	}
	s.inventoryMu.Unlock()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-f.done:
		return f.err
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ErrInventoryPending
	}
}
