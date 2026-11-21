package retention

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

const callsMessageReceiptConcept = "v1:calls:messageReceipt"
const messagePurgeBatchSize = 256

// Message receipts contain opaque replay/rate metadata, never message bodies.
// Unlike report cases, their version count depends on traffic. Expired receipts
// are therefore removed in bounded, oldest-first transactions, keeping the latest
// expired row until the final batch. This allows progress even when ordinary
// one-hour version pruning has been unavailable. The API rejects expired rooms
// independently; deletion cannot reset a live admission's sequence.
// staged-data: MUST-NOT-GATE -- expiry includes every version; row admission is
// checked on the selected latest row and on every physical row before deletion.
func (s *Sweeper) sweepCallMessages(ctx context.Context, now time.Time, dry bool, report *Report) error {
	if _, err := s.Concepts.Get(callsMessageReceiptConcept); err != nil {
		return nil
	}
	var candidates []memorynodes.MemoryNode
	if err := s.DB.NewRaw(`SELECT latest.* FROM (
 SELECT DISTINCT ON (id) * FROM "MemoryNodes" WHERE concept=? ORDER BY id,"createdAt" DESC
 ) latest WHERE NULLIF(payload->>'expiresAt','')::timestamptz <= ? ORDER BY id LIMIT 100`, callsMessageReceiptConcept, now).Scan(ctx, &candidates); err != nil {
		return fmt.Errorf("call message retention candidates: %w", err)
	}
	report.Scanned += len(candidates)
	for _, candidate := range candidates {
		if !s.Admit(ctx, candidate) {
			return fmt.Errorf("call message retention row admission refused; records preserved")
		}
		binding, _, err := messageExpiry(candidate)
		if err != nil {
			return err
		}
		pool, err := memql.ResolveWriteGatePool(s.GateDB, "call message retention", "the retention plug-in takes it from PluginContext.GateDB")
		if err != nil {
			return err
		}
		release, err := memql.AcquireWriteGate(ctx, pool, "calls.room."+memql.BareShortId(binding.RoomID))
		if err != nil {
			return err
		}
		removed, err := s.purgeCallMessageBatch(ctx, candidate.ID, binding, now, dry)
		release()
		if err != nil {
			return err
		}
		if removed > 0 {
			report.Eligible++
			if !dry {
				report.Deleted += removed
				s.Invalidate(callsMessageReceiptConcept)
			}
		}
	}
	return nil
}

type callMessageExpiry struct {
	SiteID      string `json:"siteId"`
	OwnerUserID string `json:"ownerUserId"`
	AccountID   string `json:"accountId"`
	RoomID      string `json:"roomId"`
	SeatID      string `json:"seatId"`
	Identity    string `json:"identity"`
	JoinedAt    string `json:"joinedAt"`
	ExpiresAt   string `json:"expiresAt"`
}

func messageExpiry(row memorynodes.MemoryNode) (callMessageExpiry, time.Time, error) {
	var binding callMessageExpiry
	if json.Unmarshal(row.Payload, &binding) != nil || binding.SiteID == "" || binding.OwnerUserID == "" || binding.RoomID == "" || binding.SeatID == "" || binding.Identity == "" {
		return binding, time.Time{}, fmt.Errorf("call message retention binding invalid; records preserved")
	}
	joined, joinedErr := time.Parse(time.RFC3339Nano, binding.JoinedAt)
	expires, expiryErr := time.Parse(time.RFC3339Nano, binding.ExpiresAt)
	if joinedErr != nil || expiryErr != nil || !expires.After(joined) {
		return binding, time.Time{}, fmt.Errorf("call message retention expiry invalid; records preserved")
	}
	return binding, expires, nil
}

// staged-data: MUST-NOT-GATE -- every selected version needs physical expiry,
// including staged rows. A changed binding or denied row preserves this batch.
func (s *Sweeper) purgeCallMessageBatch(ctx context.Context, receiptID string, expected callMessageExpiry, now time.Time, dry bool) (int, error) {
	var latest memorynodes.MemoryNode
	err := s.DB.NewSelect().TableExpr(`"MemoryNodes"`).ColumnExpr("*").Where("concept=? AND id=?", callsMessageReceiptConcept, receiptID).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(ctx, &latest)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !s.Admit(ctx, latest) {
		return 0, fmt.Errorf("call message retention row admission refused; records preserved")
	}
	binding, deadline, err := messageExpiry(latest)
	if err != nil {
		return 0, err
	}
	// In particular, a replacement cannot redirect the purge into a different
	// room after its original room gate was acquired.
	if binding != expected {
		return 0, fmt.Errorf("call message retention binding changed; records preserved")
	}
	if now.Before(deadline) {
		return 0, nil
	}
	var rows []memorynodes.MemoryNode
	if err := s.DB.NewSelect().TableExpr(`"MemoryNodes"`).ColumnExpr("*").Where("concept=? AND id=?", callsMessageReceiptConcept, receiptID).OrderExpr(`"createdAt"`).Limit(messagePurgeBatchSize).Scan(ctx, &rows); err != nil {
		return 0, err
	}
	for _, row := range rows {
		if !s.Admit(ctx, row) {
			return 0, fmt.Errorf("call message retention row admission refused; records preserved")
		}
		other, _, err := messageExpiry(row)
		if err != nil || other != binding || row.CreatedAt.After(latest.CreatedAt) {
			return 0, fmt.Errorf("call message retention version binding mismatch; records preserved")
		}
	}
	if dry {
		return len(rows), nil
	}
	removed := 0
	err = s.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var current memorynodes.MemoryNode
		if err := tx.NewSelect().TableExpr(`"MemoryNodes"`).ColumnExpr("*").Where("concept=? AND id=?", callsMessageReceiptConcept, receiptID).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(ctx, &current); err != nil {
			return err
		}
		if !current.CreatedAt.Equal(latest.CreatedAt) || string(current.Payload) != string(latest.Payload) {
			return fmt.Errorf("call message retention snapshot changed; records preserved")
		}
		for _, row := range rows {
			result, err := tx.NewDelete().TableExpr(`"MemoryNodes" AS old`).Where(`old.concept=? AND old.id=? AND old."createdAt"=? AND old.payload=?::jsonb`, callsMessageReceiptConcept, receiptID, row.CreatedAt, string(row.Payload)).
				Where(`NOT EXISTS (SELECT 1 FROM "MemoryNodes" newer WHERE newer.concept=? AND newer.id=? AND newer."createdAt">?)`, callsMessageReceiptConcept, receiptID, latest.CreatedAt).Exec(ctx)
			if err != nil {
				return err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if n != 1 {
				return fmt.Errorf("call message retention snapshot changed; records preserved")
			}
			removed++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}
