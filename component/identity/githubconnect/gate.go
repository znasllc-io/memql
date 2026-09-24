package githubconnect

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"hash/fnv"
	"strings"
	"time"
)

// WithGate serializes a GitHub lifecycle operation across replicas. A direct
// Postgres connection is required; transaction-pooling cannot hold this lock.
// Unavailable locking fails closed before fn runs.
func WithGate(ctx context.Context, db *sql.DB, key string, fn func(context.Context) error) error {
	if db == nil {
		return fmt.Errorf("github: lifecycle database unavailable")
	}
	lockCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := db.Conn(lockCtx)
	if err != nil {
		return fmt.Errorf("github: lifecycle gate connection: %w", err)
	}
	defer conn.Close()
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	obj := int32(h.Sum32())
	const class int32 = 0x47484342
	if _, err = conn.ExecContext(lockCtx, "SELECT pg_advisory_lock($1, $2)", class, obj); err != nil {
		// A cancelled acquire can have reached Postgres before its reply was lost.
		// Discard the session so an uncertain lock never returns to the pool.
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		return fmt.Errorf("github: lifecycle lock: %w", err)
	}
	defer func() {
		release, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := conn.ExecContext(release, "SELECT pg_advisory_unlock($1, $2)", class, obj); err != nil {
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	return fn(ctx)
}

// GrantKey is shared by callback upsert, refresh, and disconnect. The immutable
// provider identity scopes the lock; display logins and canonical ID spelling
// must not split one grant into separate critical sections.
func GrantKey(owner, externalID string) string {
	return "grant:" + strings.TrimPrefix(strings.TrimSpace(owner), "v1:identity:user:") + "\x00" + strings.TrimSpace(externalID)
}

func WithGrantGate(ctx context.Context, db *sql.DB, owner, externalID string, fn func(context.Context) error) error {
	if strings.TrimSpace(owner) == "" || strings.TrimSpace(externalID) == "" {
		return fmt.Errorf("github: grant lifecycle identity missing")
	}
	return WithGate(ctx, db, GrantKey(owner, externalID), fn)
}
