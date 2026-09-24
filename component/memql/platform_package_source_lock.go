package memql

import (
	"context"
	"database/sql/driver"
	"fmt"
	"time"
)

func packageSourceClaims(delta map[string]any) bool {
	for _, key := range []string{"sourceKind", "repoUrl", "repoRef", "credentialId", "sourceConnectionId", "accountId", "status"} {
		if _, present := delta[key]; present {
			return true
		}
	}
	return false
}

// Serialize the small registration/repoint/archive critical section through its
// final append. Ordinary analysis/heartbeat writes do not take this lock. A
// single namespace lock avoids deadlocks when a repoint changes its tuple.
func (e *MemQLEngine) lockPackageSourceClaim(ctx context.Context) (func(), error) {
	db := e.database()
	if db == nil {
		return nil, fmt.Errorf("package source claim database unavailable")
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := db.DB.Conn(bounded)
	if err != nil {
		return nil, err
	}
	const key int64 = 0x4d514c50535243
	if _, err := conn.ExecContext(bounded, "SELECT pg_advisory_lock($1)", key); err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		_ = conn.Close()
		return nil, fmt.Errorf("package source claim lock: %w", err)
	}
	return func() {
		release, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := conn.ExecContext(release, "SELECT pg_advisory_unlock($1)", key); err != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
		_ = conn.Close()
	}, nil
}
