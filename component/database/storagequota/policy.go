package storagequota

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

func limitFromPayload(ctx context.Context, tx *sql.Tx, owner string, payload []byte) (int64, bool, error) {
	var user struct {
		Role     string `json:"role"`
		Active   *bool  `json:"active"`
		Archived bool   `json:"archived"`
	}
	if err := json.Unmarshal(payload, &user); err != nil {
		return 0, false, err
	}
	if user.Archived || (user.Active != nil && !*user.Active) {
		return 0, false, errors.New("storage owner is inactive")
	}
	if user.Role == "owner" || user.Role == "developer" {
		return ClusterBytes, true, nil
	}
	var limit int64
	err := tx.QueryRowContext(ctx, `SELECT limit_bytes FROM storage_capacity_users WHERE owner_id=$1`, owner).Scan(&limit)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultUserBytes, false, nil
	}
	return limit, false, err
}

// SetLimit requires the caller to have freshly established owner/developer
// authority. Lowering a quota preserves stored bytes and existing reservations.
func (s *Store) SetLimit(ctx context.Context, actor, owner string, limit int64) error {
	if actor == "" || owner == "" || limit < 0 || limit > ClusterBytes {
		return errors.New("invalid storage allowance")
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		_, exempt, err := userLimit(ctx, tx, actor)
		if err != nil {
			return err
		}
		if !exempt {
			return errors.New("only owners and developers can change storage allowances")
		}
		_, targetExempt, err := userLimit(ctx, tx, owner)
		if err != nil {
			return err
		}
		if targetExempt {
			return errors.New("owners and developers use the shared cluster allowance")
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO storage_capacity_users(owner_id,limit_bytes,changed_by) VALUES($1,$2,$3)
ON CONFLICT(owner_id) DO UPDATE SET limit_bytes=$2,changed_by=$3,updated_at=clock_timestamp()`, owner, limit, actor)
		return err
	})
}
