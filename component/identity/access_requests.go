package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	langparser "github.com/znasllc-io/memql/component/language/parser"
	memqlengine "github.com/znasllc-io/memql/component/memql"
)

type AccessRequestRow struct{ ID, Email, Name, Status string }

func (s *Store) ReadAccessRequest(ctx context.Context, id string) (*AccessRequestRow, error) {
	rows, err := s.executeAndExtractInternal(memqlengine.ContextWithFreshRead(ctx), fmt.Sprintf(`query accessRequestById(requestId: %s)`, langparser.QuoteString(id)))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	g := newFieldGetter(rows[0])
	return &AccessRequestRow{ID: rows[0].Id, Email: g.str("email"), Name: g.str("name"), Status: g.str("status")}, nil
}

// Notification throttling is shared by every identity replica. A failed send
// releases its claim; it never erases the access request awaiting review.
func (s *Store) NotifyAccessRequests(ctx context.Context, cfg Config, send func(context.Context, string) error) error {
	if s == nil || s.DirectDB == nil || s.DirectDB() == nil {
		return errors.New("access-request notification storage is unavailable")
	}
	recipients := cfg.AccessRequestNotifyEmails
	if len(recipients) == 0 {
		for _, role := range []string{"owner", "admin"} {
			rows, err := s.executeAndExtract(auth.ContextWithInternalOrigin(ctx), fmt.Sprintf(`query activeUsers(role: %s)`, langparser.QuoteString(role)))
			if err != nil {
				return err
			}
			for _, row := range rows {
				if address := newFieldGetter(row).str("primaryEmail"); address != "" {
					recipients = append(recipients, address)
				}
			}
		}
	}
	db := s.DirectDB()
	throttle := cfg.AccessRequestNotifyThrottle
	if throttle <= 0 {
		throttle = DefaultAccessRequestNotifyThrottleMinutes * time.Minute
	}
	var failures []error
	for _, recipient := range recipients {
		recipient = strings.ToLower(strings.TrimSpace(recipient))
		claimed := time.Now().UTC()
		result, err := db.ExecContext(ctx, `INSERT INTO memql_access_request_notices (recipient, claimed_at) VALUES ($1, $2)
ON CONFLICT (recipient) DO UPDATE SET claimed_at = EXCLUDED.claimed_at WHERE memql_access_request_notices.claimed_at <= $3`, recipient, claimed, claimed.Add(-throttle))
		if err != nil {
			failures = append(failures, err)
			continue
		}
		count, err := result.RowsAffected()
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if count == 0 {
			continue
		}
		if err := send(ctx, recipient); err != nil {
			_, _ = db.ExecContext(ctx, `DELETE FROM memql_access_request_notices WHERE recipient = $1 AND claimed_at = $2`, recipient, claimed)
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// Reviews serialize across replicas and refuse without shared coordination.
func (s *Store) WithAccessRequestGate(ctx context.Context, id string, run func(context.Context) error) error {
	if s == nil || s.DirectDB == nil || s.DirectDB() == nil {
		return errors.New("access-request coordination is unavailable")
	}
	tx, err := s.DirectDB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	lockCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := tx.ExecContext(lockCtx, "SELECT pg_advisory_xact_lock($1, $2)", int32(0x41524551), magicLinkGateKey(memqlengine.BareShortId(id))); err != nil {
		return err
	}
	return run(memqlengine.ContextWithFreshRead(ctx))
}
