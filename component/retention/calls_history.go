package retention

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

const callsHistoryConcept = "v1:calls:history"
const callsHistoryReceiptConcept = "v1:calls:historyReceipt"

// sweepCallHistory physically removes every version of expired PRIVATE history,
// never room, seat, wallet, safety or account records. Generic @retention cannot
// express a user-confirmed per-row deadline. The minimal keyed receipt survives
// so bounded backfill cannot recreate what the person chose to remove.
//
// Each entry takes the same gate as archive/restore, then reads the newest row
// again. A restore before the deadline wins; at the deadline the service refuses
// restore even before this bounded hourly pass has reclaimed physical storage.
func (s *Sweeper) sweepCallHistory(ctx context.Context, now time.Time, dry bool, report *Report) error {
	if _, err := s.Concepts.Get(callsHistoryConcept); err != nil {
		return nil
	}
	var candidates []memorynodes.MemoryNode
	err := s.DB.NewRaw(`SELECT latest.* FROM (
 SELECT DISTINCT ON (id) * FROM "MemoryNodes" WHERE concept=? ORDER BY id,"createdAt" DESC
 ) latest WHERE NULLIF(payload->>'archivedAt','') IS NOT NULL
 AND NULLIF(payload->>'purgeAt','')::timestamptz <= ? ORDER BY id LIMIT 100`, callsHistoryConcept, now).Scan(ctx, &candidates)
	if err != nil {
		return fmt.Errorf("call history retention candidates: %w", err)
	}
	report.Scanned += len(candidates)
	for _, candidate := range candidates {
		if !s.Admit(ctx, candidate) {
			return fmt.Errorf("call history retention row admission refused; records preserved")
		}
		pool, err := memql.ResolveWriteGatePool(s.GateDB, "call history retention", "the retention plug-in takes it from PluginContext.GateDB")
		if err != nil {
			return err
		}
		release, err := memql.AcquireWriteGate(ctx, pool, "calls.history."+memql.BareShortId(candidate.ID))
		if err != nil {
			return err
		}
		removed, err := s.purgeCallHistoryEntry(ctx, candidate.ID, now, dry)
		release()
		if err != nil {
			return err
		}
		if removed > 0 {
			report.Eligible++
			if !dry {
				report.Deleted += removed
				s.Invalidate(callsHistoryConcept)
			}
		}
	}
	return nil
}

type historyRetentionPayload struct {
	SiteID      string `json:"siteId"`
	OwnerUserID string `json:"ownerUserId"`
	ArchivedAt  string `json:"archivedAt"`
	PurgeAt     string `json:"purgeAt"`
}

func (s *Sweeper) purgeCallHistoryEntry(ctx context.Context, id string, now time.Time, dry bool) (int, error) {
	var row memorynodes.MemoryNode
	err := s.DB.NewSelect().TableExpr(`"MemoryNodes"`).ColumnExpr("*").Where("concept=? AND id=?", callsHistoryConcept, id).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(ctx, &row)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !s.Admit(ctx, row) {
		return 0, fmt.Errorf("call history retention row admission refused")
	}
	var payload historyRetentionPayload
	if json.Unmarshal(row.Payload, &payload) != nil {
		return 0, fmt.Errorf("call history retention payload is invalid; records preserved")
	}
	deadline, err := time.Parse(time.RFC3339Nano, payload.PurgeAt)
	if payload.ArchivedAt == "" || err != nil || now.Before(deadline) {
		return 0, nil
	}
	// The receipt is created by archive before the metadata's archive stamp. A
	// missing or mismatched receipt retains the projection: deletion must never
	// make a later repair resurrect it. Restore only clears metadata's archive.
	var receipt memorynodes.MemoryNode
	receiptID := callsHistoryReceiptConcept + ":" + memql.BareShortId(id)
	err = s.DB.NewSelect().TableExpr(`"MemoryNodes"`).ColumnExpr("*").Where("concept=? AND id=?", callsHistoryReceiptConcept, receiptID).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(ctx, &receipt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !s.Admit(ctx, receipt) {
		return 0, fmt.Errorf("call history receipt admission refused")
	}
	var proof historyRetentionPayload
	if json.Unmarshal(receipt.Payload, &proof) != nil || proof.SiteID != payload.SiteID || proof.OwnerUserID != payload.OwnerUserID || proof.PurgeAt != payload.PurgeAt {
		return 0, nil
	}
	if dry {
		return 1, nil // One proven eligible entry; no physical mutation or invalidation.
	}
	// Snapshot predicate plus the shared gate protects both a replica racing this
	// pass and a version written after the snapshot. No caller-controlled SQL is
	// interpolated; concept names and all row values are bound parameters.
	result, err := s.DB.NewDelete().TableExpr(`"MemoryNodes" AS old`).Where("old.concept=? AND old.id=?", callsHistoryConcept, id).
		Where(`NOT EXISTS (SELECT 1 FROM "MemoryNodes" newer WHERE newer.concept=old.concept AND newer.id=old.id AND newer."createdAt">?)`, row.CreatedAt).
		Where(`EXISTS (SELECT 1 FROM "MemoryNodes" exact WHERE exact.concept=old.concept AND exact.id=old.id AND exact."createdAt"=? AND exact.payload=?::jsonb)`, row.CreatedAt, string(row.Payload)).Exec(ctx)
	if err != nil {
		return 0, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	// At most one entry's versions. This is a count, not an external numeric payload.
	return int(n), nil
}
