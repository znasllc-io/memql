package repositoryauthority

import (
	"context"
	"encoding/json"
	"time"

	"github.com/znasllc-io/memql/core/id"
)

// Lock only eligible records and skip a peer's locked row. The durable lease
// survives process loss; every provider write also checks this exact lease.
func (i *Integration) claimDeliveries(ctx context.Context, limit int) ([]record, error) {
	db, err := i.database()
	if err != nil {
		return nil, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT body FROM repository_change_requests
 WHERE body->>'DeliveryState' IN ('reporting','pending','waiting','attention','queued')
 AND COALESCE((body->>'DeliveryLeaseUntil')::timestamptz,'-infinity') <= $1
 AND (body->>'ExpiresAt')::timestamptz > $1
 ORDER BY updated_at,id LIMIT $2 FOR UPDATE SKIP LOCKED`, i.now(), limit)
	if err != nil {
		return nil, err
	}
	records := []record{}
	for rows.Next() {
		var b []byte
		var r record
		if err = rows.Scan(&b); err != nil {
			rows.Close()
			return nil, err
		}
		if err = json.Unmarshal(b, &r); err != nil {
			rows.Close()
			return nil, err
		}
		records = append(records, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for n := range records {
		r := &records[n]
		r.DeliveryLease = id.NewShortId()
		r.DeliveryLeaseUntil = i.now().Add(3 * time.Minute)
		b, e := json.Marshal(r)
		if e != nil {
			return nil, e
		}
		if _, err = tx.ExecContext(ctx, `UPDATE repository_change_requests SET body=$2::jsonb,updated_at=clock_timestamp() WHERE id=$1`, r.ID, string(b)); err != nil {
			return nil, err
		}
	}
	return records, tx.Commit()
}
