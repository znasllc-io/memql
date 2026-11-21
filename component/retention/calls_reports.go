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

const callsReportCaseConcept = "v1:calls:reportCase"
const callsReportAuditConcept = "v1:calls:reportAccessAudit"
const reportPurgeRowLimit = 300 // Four case versions and at most 256 immutable audit rows.

// sweepCallReports enforces the explicitly configured, immutable metadata expiry.
// It has no evidence, legal hold or external reporting semantics. Every API write
// takes the same case gate and refuses an expired case before creating an audit.
// staged-data: MUST-NOT-GATE -- expiry covers all private metadata versions; each
// candidate and every physical row still passes the caller's row-admission gate.
func (s *Sweeper) sweepCallReports(ctx context.Context, now time.Time, dry bool, report *Report) error {
	if _, err := s.Concepts.Get(callsReportCaseConcept); err != nil {
		return nil
	}
	if _, err := s.Concepts.Get(callsReportAuditConcept); err != nil {
		return nil
	}
	var candidates []memorynodes.MemoryNode
	if err := s.DB.NewRaw(`SELECT latest.* FROM (
 SELECT DISTINCT ON (id) * FROM "MemoryNodes" WHERE concept=? ORDER BY id,"createdAt" DESC
 ) latest WHERE NULLIF(payload->>'expiresAt','')::timestamptz <= ? ORDER BY id LIMIT 100`, callsReportCaseConcept, now).Scan(ctx, &candidates); err != nil {
		return fmt.Errorf("call report retention candidates: %w", err)
	}
	report.Scanned += len(candidates)
	for _, candidate := range candidates {
		if !s.Admit(ctx, candidate) {
			return fmt.Errorf("call report retention row admission refused; records preserved")
		}
		pool, err := memql.ResolveWriteGatePool(s.GateDB, "call report retention", "the retention plug-in takes it from PluginContext.GateDB")
		if err != nil {
			return err
		}
		release, err := memql.AcquireWriteGate(ctx, pool, "calls.report."+memql.BareShortId(candidate.ID))
		if err != nil {
			return err
		}
		removed, err := s.purgeCallReport(ctx, candidate.ID, now, dry)
		release()
		if err != nil {
			return err
		}
		if removed > 0 {
			report.Eligible++
			if !dry {
				report.Deleted += removed
				s.Invalidate(callsReportCaseConcept)
				s.Invalidate(callsReportAuditConcept)
			}
		}
	}
	return nil
}

type callReportExpiry struct {
	SiteID      string `json:"siteId"`
	OwnerUserID string `json:"ownerUserId"`
	CaseID      string `json:"caseId"`
	ExpiresAt   string `json:"expiresAt"`
}

// staged-data: MUST-NOT-GATE -- omitting a prior or staged version would retain
// private allegations/audit data after expiry. Admission covers every selected row.
func (s *Sweeper) purgeCallReport(ctx context.Context, caseID string, now time.Time, dry bool) (int, error) {
	var latest memorynodes.MemoryNode
	err := s.DB.NewSelect().TableExpr(`"MemoryNodes"`).ColumnExpr("*").Where("concept=? AND id=?", callsReportCaseConcept, caseID).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(ctx, &latest)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !s.Admit(ctx, latest) {
		return 0, fmt.Errorf("call report retention row admission refused")
	}
	var header callReportExpiry
	if json.Unmarshal(latest.Payload, &header) != nil || header.SiteID == "" || header.OwnerUserID == "" {
		return 0, fmt.Errorf("call report retention payload invalid; records preserved")
	}
	deadline, err := time.Parse(time.RFC3339Nano, header.ExpiresAt)
	if err != nil {
		return 0, fmt.Errorf("call report retention expiry invalid; records preserved")
	}
	if now.Before(deadline) {
		return 0, nil
	}
	var rows []memorynodes.MemoryNode
	err = s.DB.NewSelect().TableExpr(`"MemoryNodes"`).ColumnExpr("*").Where(`(concept=? AND id=?) OR (concept=? AND payload->>'caseId'=?)`, callsReportCaseConcept, caseID, callsReportAuditConcept, caseID).OrderExpr(`concept,id,"createdAt"`).Limit(reportPurgeRowLimit+1).Scan(ctx, &rows)
	if err != nil {
		return 0, err
	}
	if len(rows) > reportPurgeRowLimit {
		return 0, fmt.Errorf("call report metadata exceeds bounded purge; records preserved")
	}
	for _, row := range rows {
		if !s.Admit(ctx, row) {
			return 0, fmt.Errorf("call report retention row admission refused; records preserved")
		}
		var metadata callReportExpiry
		if json.Unmarshal(row.Payload, &metadata) != nil || metadata.SiteID != header.SiteID || metadata.OwnerUserID != header.OwnerUserID || metadata.ExpiresAt != header.ExpiresAt || (row.Concept == callsReportAuditConcept && metadata.CaseID != caseID) {
			return 0, fmt.Errorf("call report metadata ownership or expiry mismatch; records preserved")
		}
	}
	if dry {
		return len(rows), nil
	}
	// The shared gate serializes ordinary review/appeal/access with this pass. The
	// exact latest-case snapshot additionally refuses a concurrent replacement. A
	// single transaction means a refusal cannot leave just the audit or case deleted.
	removed := 0
	err = s.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var current memorynodes.MemoryNode
		if err := tx.NewSelect().TableExpr(`"MemoryNodes"`).ColumnExpr("*").Where("concept=? AND id=?", callsReportCaseConcept, caseID).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(ctx, &current); err != nil {
			return err
		}
		if !current.CreatedAt.Equal(latest.CreatedAt) || string(current.Payload) != string(latest.Payload) {
			return fmt.Errorf("call report retention snapshot changed; records preserved")
		}
		// Audit first; the latest-case predicate must remain present for every delete.
		for _, concept := range []string{callsReportAuditConcept, callsReportCaseConcept} {
			for _, row := range rows {
				if row.Concept != concept {
					continue
				}
				result, err := tx.NewDelete().TableExpr(`"MemoryNodes" AS old`).Where(`old.concept=? AND old.id=? AND old."createdAt"=? AND old.payload=?::jsonb`, row.Concept, row.ID, row.CreatedAt, string(row.Payload)).
					Where(`NOT EXISTS (SELECT 1 FROM "MemoryNodes" newer WHERE newer.concept=? AND newer.id=? AND newer."createdAt">?)`, callsReportCaseConcept, caseID, latest.CreatedAt).Exec(ctx)
				if err != nil {
					return err
				}
				n, err := result.RowsAffected()
				if err != nil {
					return err
				}
				if n != 1 {
					return fmt.Errorf("call report retention snapshot changed; records preserved")
				}
				removed++
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}
