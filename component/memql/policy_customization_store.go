package memql

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/uptrace/bun"
)

// A single append-only configuration document uses the established PostgreSQL
// transaction/advisory-lock pattern. All nodes read the same committed revision.
// The lock serializes graph validation/CAS publication; readers never block.
type databasePolicyStore struct{ database func() *bun.DB }

func (s databasePolicyStore) Read(ctx context.Context) (PolicyDocument, error) {
	db := s.database()
	if db == nil {
		return PolicyDocument{}, fmt.Errorf("policy database is unavailable")
	}
	return readPolicyDocument(ctx, db)
}
func readPolicyDocument(ctx context.Context, db bun.IDB) (PolicyDocument, error) {
	var row struct {
		Revision  int64
		Overrides json.RawMessage
		Rules     json.RawMessage
		UpdatedBy string
	}
	err := db.NewRaw(`SELECT revision, overrides, rules, updated_by FROM router_policy_revision ORDER BY revision DESC LIMIT 1`).Scan(ctx, &row)
	if err != nil {
		return PolicyDocument{}, err
	}
	doc := PolicyDocument{Revision: row.Revision, UpdatedBy: row.UpdatedBy}
	if err := json.Unmarshal(row.Overrides, &doc.Overrides); err != nil {
		return PolicyDocument{}, err
	}
	if len(row.Rules) > 0 {
		if err := json.Unmarshal(row.Rules, &doc.Rules); err != nil {
			return PolicyDocument{}, err
		}
	}
	return doc, nil
}
func (s databasePolicyStore) CompareAndSwap(ctx context.Context, expected int64, next PolicyDocument) error {
	db := s.database()
	if db == nil {
		return fmt.Errorf("policy database is unavailable")
	}
	return db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(71320964152026)`); err != nil {
			return err
		}
		current, err := readPolicyDocument(ctx, tx)
		if err != nil {
			return err
		}
		if current.Revision != expected || next.Revision != expected+1 {
			return fmt.Errorf("routing policies changed; refresh and review before saving")
		}
		raw, err := json.Marshal(next.Overrides)
		if err != nil {
			return err
		}
		ruleJSON, err := json.Marshal(next.Rules)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO router_policy_revision (revision,overrides,rules,updated_by) VALUES (?,?::jsonb,?::jsonb,?)`, next.Revision, string(raw), string(ruleJSON), next.UpdatedBy)
		return err
	})
}
