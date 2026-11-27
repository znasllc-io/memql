package retention

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/uptrace/bun"
	callrules "github.com/znasllc-io/memql/component/calls"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

const callsQuestionConcept = "v1:calls:workflowQuestion"
const questionVersionLimit = 3

type callQuestionBinding struct {
	SiteID          string `json:"siteId"`
	OwnerUserID     string `json:"ownerUserId"`
	AccountID       string `json:"accountId"`
	RoomID          string `json:"roomId"`
	RunID           string `json:"runId"`
	AuthorKeyID     string `json:"authorKeyId"`
	AuthorAdmission string `json:"authorAdmission"`
	SubmittedAt     string `json:"submittedAt"`
	RoomDeadline    string `json:"roomDeadline"`
	ExpiresAt       string `json:"expiresAt"`
}

type callQuestionRecord struct {
	callQuestionBinding
	Revision  int                           `json:"revision"`
	Status    string                        `json:"status"`
	Body      string                        `json:"body"`
	StateData string                        `json:"stateData"`
	State     callrules.QuestionStoredState `json:"-"`
}

func questionRetentionRecord(row memorynodes.MemoryNode) (callQuestionRecord, time.Time, error) {
	var record callQuestionRecord
	invalid := func() (callQuestionRecord, time.Time, error) {
		return record, time.Time{}, fmt.Errorf("call question retention record invalid; records preserved")
	}
	if row.Concept != callsQuestionConcept || row.Type != "object" || !strings.HasPrefix(row.ID, callsQuestionConcept+":") || !callrules.ValidWorkflowID(memql.BareShortId(row.ID)) || row.CreatedAt.IsZero() || json.Unmarshal(row.Payload, &record) != nil || !callrules.ValidWorkflowID(record.RunID) {
		return invalid()
	}
	for _, value := range []string{record.SiteID, record.OwnerUserID, record.RoomID, record.RunID, record.AuthorKeyID} {
		if value == "" || len(value) > 256 || strings.ContainsAny(value, " \t\r\n\x00") {
			return invalid()
		}
	}
	if len(record.AccountID) > 256 || strings.ContainsAny(record.AccountID, " \t\r\n\x00") {
		return invalid()
	}
	if !callrules.QuestionDigest(record.AuthorAdmission) {
		return invalid()
	}
	submitted, e1 := time.Parse(time.RFC3339Nano, record.SubmittedAt)
	roomDeadline, e2 := time.Parse(time.RFC3339Nano, record.RoomDeadline)
	expires, e3 := time.Parse(time.RFC3339Nano, record.ExpiresAt)
	if e1 != nil || e2 != nil || e3 != nil || submitted.UTC().Format(time.RFC3339Nano) != record.SubmittedAt || roomDeadline.UTC().Format(time.RFC3339Nano) != record.RoomDeadline || expires.UTC().Format(time.RFC3339Nano) != record.ExpiresAt || roomDeadline.Before(submitted) || !expires.After(submitted) || expires.After(roomDeadline.Add(time.Hour)) || expires.After(submitted.Add(25*time.Hour)) {
		return invalid()
	}
	if record.Revision < 1 || record.Revision > questionVersionLimit || !utf8.ValidString(record.Body) || len(record.Body) > 2000 || len(record.StateData) > 8192 {
		return invalid()
	}
	var valid bool
	record.State, valid = callrules.ParseQuestionStoredState(record.StateData, int64(record.Revision), record.Status, submitted, expires)
	if !valid {
		return invalid()
	}
	switch record.Status {
	case "pending":
		if record.Revision != 1 || !callrules.ValidQuestionText(record.Body) {
			return invalid()
		}
	case "selected":
		if record.Revision != 2 || !callrules.ValidQuestionText(record.Body) {
			return invalid()
		}
	case "withdrawn", "dismissed", "resolved":
		if record.Revision < 2 || record.Body != "" {
			return invalid()
		}
	default:
		return invalid()
	}
	return record, expires, nil
}

// The native queue clears terminal bodies in its latest row. Earlier private
// versions remain until this bounded expiry pass removes the entire question.
// No archive or recording is created. Expiry is fixed at submission, no later
// than the saved room deadline plus one hour (and submission plus 25 hours).
// staged-data: MUST-NOT-GATE -- expiry covers all physical private versions;
// every candidate and version still passes the maintenance row-admission check.
func (s *Sweeper) sweepCallQuestions(ctx context.Context, now time.Time, dry bool, report *Report) error {
	if _, err := s.Concepts.Get(callsQuestionConcept); err != nil {
		return nil
	}
	var candidates []memorynodes.MemoryNode
	// Native dates are canonical UTC. Compare only the whole-second prefix for
	// candidate selection, then validate the full instant in Go. Never cast an
	// untrusted date in SQL, where malformed data can leak into a driver error.
	err := s.DB.NewRaw(`SELECT latest.* FROM (
 SELECT DISTINCT ON (id) * FROM "MemoryNodes" WHERE concept=? ORDER BY id,"createdAt" DESC
 ) latest WHERE left(payload->>'expiresAt',19) <= ? ORDER BY id LIMIT 100`, callsQuestionConcept, now.UTC().Format("2006-01-02T15:04:05")).Scan(ctx, &candidates)
	if err != nil {
		return fmt.Errorf("call question retention candidates unavailable; records preserved")
	}
	report.Scanned += len(candidates)
	var failures []error
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		if !s.Admit(ctx, candidate) {
			failures = append(failures, fmt.Errorf("call question retention row admission refused; records preserved"))
			continue
		}
		record, expires, err := questionRetentionRecord(candidate)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if now.Before(expires) {
			continue
		}
		pool, err := memql.ResolveWriteGatePool(s.GateDB, "call question retention", "the retention plug-in takes it from PluginContext.GateDB")
		if err != nil {
			return errors.Join(append(failures, err)...)
		}
		release, err := memql.AcquireWriteGate(ctx, pool, "calls.room."+memql.BareShortId(record.RoomID))
		if err != nil {
			failures = append(failures, err)
			continue
		}
		removed, err := s.purgeCallQuestion(ctx, candidate, record.callQuestionBinding, now, dry)
		release()
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if removed > 0 {
			report.Eligible++
			if !dry {
				report.Deleted += removed
				s.Invalidate(callsQuestionConcept)
			}
		}
	}
	return errors.Join(failures...)
}

// staged-data: MUST-NOT-GATE -- private prior/staged versions must expire too.
// The shared room gate serializes native writers. Every physical version must
// agree on ownership and immutable expiry; unexpected storage is preserved.
func (s *Sweeper) purgeCallQuestion(ctx context.Context, candidate memorynodes.MemoryNode, expected callQuestionBinding, now time.Time, dry bool) (int, error) {
	var latest memorynodes.MemoryNode
	err := s.DB.NewSelect().TableExpr(`"MemoryNodes"`).ColumnExpr("*").Where("concept=? AND id=?", callsQuestionConcept, candidate.ID).OrderExpr(`"createdAt" DESC`).Limit(1).Scan(ctx, &latest)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("call question retention latest unavailable; records preserved")
	}
	if !s.Admit(ctx, latest) {
		return 0, fmt.Errorf("call question retention row admission refused; records preserved")
	}
	record, expires, err := questionRetentionRecord(latest)
	if err != nil {
		return 0, err
	}
	if record.callQuestionBinding != expected {
		return 0, fmt.Errorf("call question retention binding changed; records preserved")
	}
	if !sameQuestionSnapshot(latest, candidate) {
		return 0, fmt.Errorf("call question retention snapshot changed; records preserved")
	}
	if now.Before(expires) {
		return 0, nil
	}
	var rows []memorynodes.MemoryNode
	if err := s.DB.NewSelect().TableExpr(`"MemoryNodes"`).ColumnExpr("*").Where("concept=? AND id=?", callsQuestionConcept, candidate.ID).OrderExpr(`"createdAt"`).Limit(questionVersionLimit+1).Scan(ctx, &rows); err != nil {
		return 0, fmt.Errorf("call question retention versions unavailable; records preserved")
	}
	if len(rows) == 0 || len(rows) > questionVersionLimit || len(rows) != record.Revision {
		return 0, fmt.Errorf("call question retention exceeds bounded versions; records preserved")
	}
	previousRevision := 0
	previousStatus := ""
	var previousState callrules.QuestionStoredState
	for _, row := range rows {
		if !s.Admit(ctx, row) {
			return 0, fmt.Errorf("call question retention row admission refused; records preserved")
		}
		other, _, err := questionRetentionRecord(row)
		if err != nil || other.callQuestionBinding != expected || other.Revision != previousRevision+1 || row.CreatedAt.After(latest.CreatedAt) || previousStatus != "" && previousStatus != "pending" && previousStatus != "selected" || other.Status == "resolved" && previousStatus != "selected" {
			return 0, fmt.Errorf("call question retention version binding or revision mismatch; records preserved")
		}
		if previousRevision > 0 {
			priorOperations := previousState.Operations
			state := other.State
			previousState.Operations, state.Operations = nil, nil
			if !reflect.DeepEqual(previousState, state) || !reflect.DeepEqual(priorOperations, other.State.Operations[:previousRevision]) {
				return 0, fmt.Errorf("call question retention metadata binding changed; records preserved")
			}
		}
		previousRevision = other.Revision
		previousStatus = other.Status
		previousState = other.State
	}
	// @secret annotates body redaction; this is not a secret-table concept. A
	// second physical location is unexpected, so do not leave an orphaned copy.
	if count, err := s.DB.NewSelect().TableExpr(`"SecretMemoryNodes"`).Where("concept=? AND id=?", callsQuestionConcept, candidate.ID).Count(ctx); err != nil || count != 0 {
		return 0, fmt.Errorf("call question retention unexpected storage; records preserved")
	}
	if dry {
		return len(rows), nil
	}
	removed := 0
	err = s.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var current []memorynodes.MemoryNode
		if err := tx.NewSelect().TableExpr(`"MemoryNodes"`).ColumnExpr("*").Where("concept=? AND id=?", callsQuestionConcept, candidate.ID).OrderExpr(`"createdAt"`).Limit(questionVersionLimit+1).Scan(ctx, &current); err != nil || len(current) != len(rows) {
			return fmt.Errorf("call question retention snapshot changed; records preserved")
		}
		for i := range rows {
			if !sameQuestionSnapshot(current[i], rows[i]) || !s.Admit(ctx, current[i]) {
				return fmt.Errorf("call question retention snapshot changed or admission refused; records preserved")
			}
		}
		for _, row := range rows {
			result, err := tx.NewDelete().TableExpr(`"MemoryNodes" AS old`).Where(`old.concept=? AND old.id=? AND old."createdAt"=? AND old.type=? AND old."createdBy"=? AND old.payload=?::jsonb`, callsQuestionConcept, row.ID, row.CreatedAt, row.Type, row.CreatedBy, string(row.Payload)).
				Where(`old.schema=?::jsonb AND old.metadata=?::jsonb AND old.provenance=?::jsonb`, string(row.Schema), string(row.Metadata), string(row.Provenance)).
				Where(`NOT EXISTS (SELECT 1 FROM "MemoryNodes" newer WHERE newer.concept=? AND newer.id=? AND newer."createdAt">?)`, callsQuestionConcept, candidate.ID, latest.CreatedAt).
				Where(`NOT EXISTS (SELECT 1 FROM "SecretMemoryNodes" unexpected WHERE unexpected.concept=? AND unexpected.id=?)`, callsQuestionConcept, candidate.ID).Exec(ctx)
			if err != nil {
				return fmt.Errorf("call question retention delete unavailable; records preserved")
			}
			n, err := result.RowsAffected()
			if err != nil || n != 1 {
				return fmt.Errorf("call question retention snapshot changed; records preserved")
			}
			removed++
		}
		if count, err := tx.NewSelect().TableExpr(`"MemoryNodes"`).Where("concept=? AND id=?", callsQuestionConcept, candidate.ID).Count(ctx); err != nil || count != 0 {
			return fmt.Errorf("call question retention incomplete inventory; records preserved")
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

func sameQuestionSnapshot(a, b memorynodes.MemoryNode) bool {
	return a.ID == b.ID && a.Concept == b.Concept && a.Type == b.Type && a.CreatedBy == b.CreatedBy && a.CreatedAt.Equal(b.CreatedAt) && bytes.Equal(a.Payload, b.Payload) && bytes.Equal(a.Metadata, b.Metadata) && bytes.Equal(a.Schema, b.Schema) && bytes.Equal(a.Provenance, b.Provenance)
}
