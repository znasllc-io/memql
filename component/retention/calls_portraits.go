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

const callsPortraitConcept = "v1:calls:agentPortrait"
const portraitPurgeBatchSize = 256

// Portrait metadata is eligible only after native reference checks and confirmed
// storage retirement. This sweeper performs no provider work. It removes bounded
// oldest-first batches, keeping the final retirement marker until all earlier
// versions are gone; opaque portrait IDs are never reused by uploads.
// staged-data: MUST-NOT-GATE -- every physical version needs row admission before
// deletion, including staged records. Exact latest fences preserve concurrent work.
func (s *Sweeper) sweepCallPortraits(ctx context.Context, now time.Time, dry bool, report *Report) error {
	if _, err := s.Concepts.Get(callsPortraitConcept); err != nil {
		return nil
	}
	var candidates []memorynodes.MemoryNode
	if err := s.DB.NewRaw(`SELECT latest.* FROM (
 SELECT DISTINCT ON (id) * FROM "MemoryNodes" WHERE concept=? ORDER BY id,"createdAt" DESC
 ) latest WHERE payload->>'status'='retired' AND NULLIF(payload->>'purgeAt','')::timestamptz <= ? ORDER BY id LIMIT 100`, callsPortraitConcept, now).Scan(ctx, &candidates); err != nil {
		return fmt.Errorf("call portrait retention candidates: %w", err)
	}
	report.Scanned += len(candidates)
	for _, candidate := range candidates {
		if !s.Admit(ctx, candidate) {
			return fmt.Errorf("call portrait retention row admission refused; records preserved")
		}
		binding, err := portraitBinding(candidate)
		if err != nil {
			return err
		}
		pool, err := memql.ResolveWriteGatePool(s.GateDB, "call portrait retention", "the retention plug-in takes it from PluginContext.GateDB")
		if err != nil {
			return err
		}
		release, err := memql.AcquireWriteGates(ctx, pool, "calls.account."+memql.BareShortId(binding.KeyID), "calls.key."+memql.BareShortId(binding.KeyID))
		if err != nil {
			return err
		}
		removed, err := s.purgeCallPortraitBatch(ctx, candidate.ID, binding, now, dry)
		release()
		if err != nil {
			return err
		}
		if removed > 0 {
			report.Eligible++
			if !dry {
				report.Deleted += removed
				s.Invalidate(callsPortraitConcept)
			}
		}
	}
	return nil
}

type callPortraitBinding struct {
	SiteID      string `json:"siteId"`
	OwnerUserID string `json:"ownerUserId"`
	AccountID   string `json:"accountId"`
	KeyID       string `json:"keyId"`
	Digest      string `json:"digest"`
	MimeType    string `json:"mimeType"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	IssuedAt    string `json:"issuedAt"`
}

func portraitBinding(row memorynodes.MemoryNode) (callPortraitBinding, error) {
	var binding callPortraitBinding
	if json.Unmarshal(row.Payload, &binding) != nil || binding.SiteID == "" || binding.OwnerUserID == "" || binding.KeyID == "" || len(binding.Digest) != 64 || binding.Width < 1 || binding.Height < 1 || (binding.MimeType != "image/png" && binding.MimeType != "image/jpeg") {
		return binding, fmt.Errorf("call portrait retention binding invalid; records preserved")
	}
	if _, err := time.Parse(time.RFC3339Nano, binding.IssuedAt); err != nil {
		return binding, fmt.Errorf("call portrait retention issue time invalid; records preserved")
	}
	return binding, nil
}
func portraitPurgeDeadline(row memorynodes.MemoryNode) (time.Time, error) {
	var state struct {
		Status          string `json:"status"`
		RetirementID    string `json:"retirementId"`
		RetirementReady bool   `json:"retirementReady"`
		RetiredAt       string `json:"retiredAt"`
		PurgeAt         string `json:"purgeAt"`
	}
	if json.Unmarshal(row.Payload, &state) != nil {
		return time.Time{}, fmt.Errorf("call portrait retention state invalid; records preserved")
	}
	if state.Status != "retired" {
		return time.Time{}, nil
	}
	retired, e1 := time.Parse(time.RFC3339Nano, state.RetiredAt)
	deadline, e2 := time.Parse(time.RFC3339Nano, state.PurgeAt)
	if e1 != nil || e2 != nil || len(state.RetirementID) != 36 || !state.RetirementReady || !deadline.Equal(retired.Add(24*time.Hour)) {
		return time.Time{}, fmt.Errorf("call portrait retirement is unconfirmed; records preserved")
	}
	return deadline, nil
}

// staged-data: MUST-NOT-GATE -- every selected version needs physical expiry,
// including staged rows. A changed binding or denied row preserves this batch.
func (s *Sweeper) purgeCallPortraitBatch(ctx context.Context, portraitID string, expected callPortraitBinding, now time.Time, dry bool) (int, error) {
	var latest memorynodes.MemoryNode
	err := s.DB.NewSelect().TableExpr(`"MemoryNodes"`).ColumnExpr("*").Where("concept=? AND id=?", callsPortraitConcept, portraitID).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(ctx, &latest)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !s.Admit(ctx, latest) {
		return 0, fmt.Errorf("call portrait retention row admission refused; records preserved")
	}
	binding, err := portraitBinding(latest)
	if err != nil {
		return 0, err
	}
	// A replacement cannot redirect deletion under a different account gate.
	if binding != expected {
		return 0, fmt.Errorf("call portrait retention binding changed; records preserved")
	}
	deadline, err := portraitPurgeDeadline(latest)
	if err != nil {
		return 0, err
	}
	if deadline.IsZero() || now.Before(deadline) {
		return 0, nil
	}
	var rows []memorynodes.MemoryNode
	if err := s.DB.NewSelect().TableExpr(`"MemoryNodes"`).ColumnExpr("*").Where("concept=? AND id=?", callsPortraitConcept, portraitID).OrderExpr(`"createdAt"`).Limit(portraitPurgeBatchSize).Scan(ctx, &rows); err != nil {
		return 0, err
	}
	for _, row := range rows {
		if !s.Admit(ctx, row) {
			return 0, fmt.Errorf("call portrait retention row admission refused; records preserved")
		}
		other, err := portraitBinding(row)
		if err != nil || other != binding || row.CreatedAt.After(latest.CreatedAt) {
			return 0, fmt.Errorf("call portrait retention version binding mismatch; records preserved")
		}
	}
	if dry {
		return len(rows), nil
	}
	removed := 0
	err = s.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var current memorynodes.MemoryNode
		if err := tx.NewSelect().TableExpr(`"MemoryNodes"`).ColumnExpr("*").Where("concept=? AND id=?", callsPortraitConcept, portraitID).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(ctx, &current); err != nil {
			return err
		}
		if !current.CreatedAt.Equal(latest.CreatedAt) || string(current.Payload) != string(latest.Payload) {
			return fmt.Errorf("call portrait retention snapshot changed; records preserved")
		}
		for _, row := range rows {
			result, err := tx.NewDelete().TableExpr(`"MemoryNodes" AS old`).Where(`old.concept=? AND old.id=? AND old."createdAt"=? AND old.payload=?::jsonb`, callsPortraitConcept, portraitID, row.CreatedAt, string(row.Payload)).
				Where(`NOT EXISTS (SELECT 1 FROM "MemoryNodes" newer WHERE newer.concept=? AND newer.id=? AND newer."createdAt">?)`, callsPortraitConcept, portraitID, latest.CreatedAt).Exec(ctx)
			if err != nil {
				return err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if n != 1 {
				return fmt.Errorf("call portrait retention snapshot changed; records preserved")
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
