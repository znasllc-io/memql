package automations

import (
	"context"
	"fmt"
	"time"
)

// HasRecentClaim observes real arbitration, without acquiring or refreshing a
// lease. Recovery can briefly preserve a run claimed on a peer even before its
// first heartbeat; an old claim is never counted as progress indefinitely.
func (c StrictClusterClaimer) HasRecentClaim(ctx context.Context, name string, keys []string, maxAge time.Duration) (bool, error) {
	if c.guard == nil || c.guard.dbGetter == nil {
		return false, fmt.Errorf("claim observation unavailable")
	}
	db := c.guard.dbGetter()
	if db == nil || db.DB == nil {
		return false, fmt.Errorf("claim observation database unavailable")
	}
	if len(keys) == 0 || len(keys) > 6 {
		return false, fmt.Errorf("claim observation requires one to six exact keys")
	}
	if maxAge <= 0 {
		return false, fmt.Errorf("claim observation requires a positive age bound")
	}
	// Compare both timestamps on the database clock that stamps the claim.
	// Pod clock skew cannot turn a fresh peer claim into an abandoned run.
	exact := [6]string{keys[0], keys[0], keys[0], keys[0], keys[0], keys[0]}
	copy(exact[:], keys)
	var exists bool
	err := db.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM automation_execution_claims
 WHERE automation_name = $1 AND dedup_key IN ($2, $3, $4, $5, $6, $7)
 AND claimed_at >= now() - make_interval(secs => $8)
 AND (expires_at IS NULL OR expires_at > now()))`, name, exact[0], exact[1], exact[2], exact[3], exact[4], exact[5], maxAge.Seconds()).Scan(&exists)
	return exists, err
}

// HasLiveClaim applies the same conflict test as ClaimWithTTL to exact aliases
// of an existing claim. It does not acquire, renew, release or rename a claim.
// Once-only callers honor any existing row; recovery honors its stored lease,
// or the ordinary takeover interval for a row created before leases existed.
func (c StrictClusterClaimer) HasLiveClaim(ctx context.Context, name string, keys []string, ttl time.Duration) (bool, error) {
	if c.guard == nil || c.guard.dbGetter == nil {
		return false, fmt.Errorf("claim observation unavailable")
	}
	db := c.guard.dbGetter()
	if db == nil || db.DB == nil || len(keys) != 1 {
		return false, fmt.Errorf("alias observation requires its database and one exact key")
	}
	var exists bool
	err := db.DB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM automation_execution_claims
 WHERE automation_name=$1 AND dedup_key=$2 AND ($3 <= 0 OR COALESCE(expires_at,claimed_at+make_interval(secs=>$3)) >= now()))`, name, keys[0], ttl.Seconds()).Scan(&exists)
	return exists, err
}
