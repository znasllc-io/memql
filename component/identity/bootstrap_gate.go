package identity

import (
	"context"
	"database/sql/driver"
	"errors"
	"time"
)

// AcquireBootstrapGate serializes setup submissions and owner creation across
// identity replicas. Unlike an ordinary sign-in, ownership never proceeds
// without its lock. The dedicated connection must bypass transaction pooling.
func (s *Store) AcquireBootstrapGate(ctx context.Context) (func(), error) {
	if s == nil || s.DirectDB == nil || s.DirectDB() == nil {
		return nil, errors.New("ownership coordination is unavailable")
	}
	lockCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := s.DirectDB().Conn(lockCtx)
	if err != nil {
		return nil, err
	}
	const namespace = 0x434C414D // CLAM, distinct from request-scoped magic-link locks.
	if _, err = conn.ExecContext(lockCtx, "SELECT pg_advisory_lock($1, $2)", namespace, 1); err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		_ = conn.Close()
		return nil, err
	}
	return func() {
		unlockCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := conn.ExecContext(unlockCtx, "SELECT pg_advisory_unlock($1, $2)", namespace, 1); err != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		_ = conn.Close()
	}, nil
}
