package accounts

// export_store.go -- the export's record: one row of account_exports per export
// (design record docs/superpowers/specs/2026-11-12-account-export-design.md,
// section F, "The record"; the table is
// component/database/memory-nodes/migrations/20261112130000_account_export.up.sql).
//
// # Why every statement here is SQL on the table
//
// The record is the cluster's own and not a row in the graph, for the reasons
// the migration gives. The lease is part of the record: the claim, the
// heartbeat, the take-over of a stale run and every final state are single
// statements guarded on (claimed_by, attempt, state), so a node that lost its
// run to another replica writes nothing over the new attempt. Nothing in
// memory decides who runs what.
//
// # The passphrase
//
// `sealed_passphrase` is read in exactly one place (claim, which hands it to
// the runner) and every statement that writes a final state sets it NULL in
// the same statement. The plain passphrase never passes through here: Prepare
// seals it before insertDraft and the runner opens it after claim.
//
// # The file a stopped attempt committed
//
// object_key is written twice: by the run just before it commits its file
// (recordObject), and with the complete state. The key is recorded first so
// that storage never holds a file no record names. Between the two writes, a
// node that dies leaves a whole file that only that key names, or a key that
// names nothing when it died before the commit. The rules for such a key:
//
//   - The take-over, the claim and the hand-back delete a recorded file
//     through the blob store, and clear the key when the delete succeeds.
//   - ENDING a row never waits on that delete. A row past its attempt limit or
//     with a cancel requested ends failed or cancelled, its sealed passphrase
//     cleared, whether or not the delete succeeded; when it failed the row
//     keeps its key. One undeletable file cannot hold the installation's
//     one-at-a-time queue, and a person's cancel always ends a run.
//   - REQUEUEING a row waits for the delete. The next attempt needs storage
//     anyway, and it would record its own key over the only record of the old
//     file. Such a row stays as it is (running with no lease after a take-over,
//     running under its node after a hand-back) and is tried again each tick.
//   - The runner's sweep (sweepKeptObjects) tries again, each tick, to delete
//     the file of a failed or cancelled row that still has a key: a bounded
//     number, oldest first, the key cleared (guarded on itself) once the
//     delete succeeds. That also covers a run whose own cleanup could not
//     delete its file before it returned.
//   - Only a complete row's key is ever served (ResolveExportForDownload).
//
// Tests: export_db_test.go, export_object_db_test.go.

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"
)

// The states of account_exports.state.
const (
	exportDraft     = "draft"
	exportQueued    = "queued"
	exportRunning   = "running"
	exportComplete  = "complete"
	exportFailed    = "failed"
	exportCancelled = "cancelled"
)

// The lease's timings (design section F). The sweep and the take-over compare
// against the database's clock, so replicas with skewed clocks agree.
const (
	exportDraftLifetime = 15 * time.Minute
	exportStaleAfter    = 2 * time.Minute
	exportMaxAttempts   = 3
)

// The failure codes a run can end with (spec section L). ExportFailure.Code is
// one of these; anything else a work function returns is export_failed.
const (
	FailureNotReady       = "export_not_ready"
	FailureRowsWaiting    = "export_rows_waiting"
	FailureCensusMismatch = "export_census_mismatch"
	FailureVerifyFailed   = "export_verify_failed"
	FailureRowsTooSlow    = "export_rows_too_slow"
	FailureFailed         = "export_failed"
)

// Export is one row of account_exports. The JSON names are the answer row's
// fields; ObjectKey stays server-side (the download route asks for it through
// ResolveExportForDownload) and Report is sent only by accountExportStatus.
type Export struct {
	ID              string          `json:"id"`
	AccountID       string          `json:"accountId"`
	AccountName     string          `json:"accountName"`
	RequestedBy     string          `json:"requestedBy"`
	RequestedAt     time.Time       `json:"requestedAt"`
	State           string          `json:"state"`
	Phase           string          `json:"phase"`
	Protected       bool            `json:"protected"`
	Attempt         int             `json:"attempt"`
	CancelRequested bool            `json:"cancelRequested"`
	StartedAt       *time.Time      `json:"startedAt,omitempty"`
	FinishedAt      *time.Time      `json:"finishedAt,omitempty"`
	KindsTotal      int             `json:"kindsTotal"`
	KindsDone       int             `json:"kindsDone"`
	RowsWritten     int64           `json:"rowsWritten"`
	VersionsWritten int64           `json:"versionsWritten"`
	BytesTotal      int64           `json:"bytesTotal"`
	BytesDone       int64           `json:"bytesDone"`
	ObjectKey       string          `json:"-"`
	FileName        string          `json:"fileName"`
	SizeBytes       int64           `json:"sizeBytes"`
	SHA256          string          `json:"sha256"`
	Report          json.RawMessage `json:"report,omitempty"`
	FailureCode     string          `json:"failureCode"`
	FailureDetail   string          `json:"failureDetail"`
	RemovedAt       *time.Time      `json:"removedAt,omitempty"`
	RemovedBy       string          `json:"removedBy"`
	// ClaimedBy is the node holding a running attempt; the run's own writes
	// are guarded on it. Server-side only.
	ClaimedBy string `json:"-"`
}

// active reports whether the export is prepared, queued or running: the states
// the one-active-per-account index counts.
func (x Export) active() bool {
	return x.State == exportDraft || x.State == exportQueued || x.State == exportRunning
}

// ExportFailure is a failure the wizard can show. Code is one of the Failure*
// constants; Detail is a sentence for a person and must not carry the
// passphrase (the runner scrubs it all the same).
type ExportFailure struct{ Code, Detail string }

func (f *ExportFailure) Error() string { return f.Code + ": " + f.Detail }

// exportColumns is every column an Export is read from, in scanExport's order.
// sealed_passphrase is NOT among them: only claim reads it.
const exportColumns = `id, account_id, account_name, requested_by, requested_at, state, phase, protected,
	attempt, cancel_requested, started_at, finished_at, kinds_total, kinds_done, rows_written, versions_written,
	bytes_total, bytes_done, object_key, file_name, size_bytes, sha256, report, failure_code, failure_detail,
	removed_at, removed_by, claimed_by`

type rowScanner interface{ Scan(dest ...any) error }

func scanExport(r rowScanner, extra ...any) (Export, error) {
	var x Export
	var started, finished, removed sql.NullTime
	var report []byte
	dest := []any{&x.ID, &x.AccountID, &x.AccountName, &x.RequestedBy, &x.RequestedAt, &x.State, &x.Phase, &x.Protected,
		&x.Attempt, &x.CancelRequested, &started, &finished, &x.KindsTotal, &x.KindsDone, &x.RowsWritten, &x.VersionsWritten,
		&x.BytesTotal, &x.BytesDone, &x.ObjectKey, &x.FileName, &x.SizeBytes, &x.SHA256, &report, &x.FailureCode, &x.FailureDetail,
		&removed, &x.RemovedBy, &x.ClaimedBy}
	if err := r.Scan(append(dest, extra...)...); err != nil {
		return Export{}, err
	}
	x.RequestedAt = x.RequestedAt.UTC()
	x.StartedAt, x.FinishedAt, x.RemovedAt = timePtr(started), timePtr(finished), timePtr(removed)
	if len(report) > 0 {
		x.Report = json.RawMessage(report)
	}
	return x, nil
}

func timePtr(t sql.NullTime) *time.Time {
	if !t.Valid {
		return nil
	}
	u := t.Time.UTC()
	return &u
}

// errExportInProgress carries the active export's id out of insertDraft.
type errExportInProgress struct{ active Export }

func (e *errExportInProgress) Error() string {
	return fmt.Sprintf("export_in_progress: This account already has an export that is %s (%s). Finish or cancel it first.", e.active.State, e.active.ID)
}

// activeExport is the account's draft, queued or running export, if any.
func (s *Exports) activeExport(ctx context.Context, db *bun.DB, account string) (Export, bool, error) {
	row := db.DB.QueryRowContext(ctx, `SELECT `+exportColumns+` FROM account_exports
		 WHERE account_id = $1 AND state IN ('draft', 'queued', 'running') LIMIT 1`, account)
	x, err := scanExport(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Export{}, false, nil
	}
	return x, err == nil, err
}

// insertDraft writes a new draft. The unique index allows one active export
// per account; a second prepare that races the first past the check below is
// refused by the index and answered the same way.
func (s *Exports) insertDraft(ctx context.Context, x Export, sealed sql.NullString) (Export, error) {
	db, err := s.inv.database()
	if err != nil {
		return Export{}, err
	}
	if active, ok, err := s.activeExport(ctx, db, x.AccountID); err != nil {
		return Export{}, fmt.Errorf("accountExportPrepare: look for an active export: %w", err)
	} else if ok {
		return Export{}, &errExportInProgress{active: active}
	}
	row := db.DB.QueryRowContext(ctx, `INSERT INTO account_exports
		(id, account_id, account_name, requested_by, state, protected, sealed_passphrase)
		VALUES ($1, $2, $3, $4, 'draft', $5, $6)
		RETURNING `+exportColumns, x.ID, x.AccountID, x.AccountName, x.RequestedBy, x.Protected, sealed)
	out, err := scanExport(row)
	if err != nil {
		if strings.Contains(err.Error(), "account_exports_one_active_idx") {
			if active, ok, aerr := s.activeExport(ctx, db, x.AccountID); aerr == nil && ok {
				return Export{}, &errExportInProgress{active: active}
			}
		}
		return Export{}, fmt.Errorf("accountExportPrepare: write the draft: %w", err)
	}
	return out, nil
}

// get reads one export.
func (s *Exports) get(ctx context.Context, id string) (Export, bool, error) {
	db, err := s.inv.database()
	if err != nil {
		return Export{}, false, err
	}
	x, err := scanExport(db.DB.QueryRowContext(ctx, `SELECT `+exportColumns+` FROM account_exports WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Export{}, false, nil
	}
	if err != nil {
		return Export{}, false, fmt.Errorf("read export %s: %w", id, err)
	}
	return x, true, nil
}

// update runs one guarded UPDATE ... RETURNING and answers the row, or ok=false
// when the guard matched nothing.
func (s *Exports) update(ctx context.Context, query string, args ...any) (Export, bool, error) {
	db, err := s.inv.database()
	if err != nil {
		return Export{}, false, err
	}
	x, err := scanExport(db.DB.QueryRowContext(ctx, query+` RETURNING `+exportColumns, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return Export{}, false, nil
	}
	if err != nil {
		return Export{}, false, err
	}
	return x, true, nil
}

// queue moves a draft that is still within its lifetime to queued.
func (s *Exports) queue(ctx context.Context, id string) (Export, bool, error) {
	return s.update(ctx, `UPDATE account_exports SET state = 'queued'
		 WHERE id = $1 AND state = 'draft' AND requested_at > clock_timestamp() - make_interval(secs => $2)`,
		id, exportDraftLifetime.Seconds())
}

// cancel ends a draft or a queued export at once, and asks a running one to
// stop: the runner holding it sees cancel_requested at its next heartbeat and
// writes the final state itself.
func (s *Exports) cancel(ctx context.Context, id string) (Export, bool, error) {
	return s.update(ctx, `UPDATE account_exports SET
		   cancel_requested = true,
		   state = CASE WHEN state = 'running' THEN state ELSE 'cancelled' END,
		   finished_at = CASE WHEN state = 'running' THEN finished_at ELSE clock_timestamp() END,
		   sealed_passphrase = CASE WHEN state = 'running' THEN sealed_passphrase ELSE NULL END
		 WHERE id = $1 AND state IN ('draft', 'queued', 'running')`, id)
}

// markRemoved records that a person deleted the file. The row stays.
func (s *Exports) markRemoved(ctx context.Context, id, by string) (Export, bool, error) {
	return s.update(ctx, `UPDATE account_exports SET removed_at = clock_timestamp(), removed_by = $2
		 WHERE id = $1 AND state = 'complete' AND removed_at IS NULL`, id, by)
}

// exportCursor is the keyset of the list, (requested_at, id), opaque to the
// client.
func encodeExportCursor(x Export) string {
	return base64.RawURLEncoding.EncodeToString([]byte(x.RequestedAt.UTC().Format(time.RFC3339Nano) + "|" + x.ID))
}

func decodeExportCursor(raw string) (time.Time, string, error) {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return time.Time{}, "", errors.New("that page cursor is not one this list gave out")
	}
	at, id, ok := strings.Cut(string(b), "|")
	if !ok {
		return time.Time{}, "", errors.New("that page cursor is not one this list gave out")
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return time.Time{}, "", errors.New("that page cursor is not one this list gave out")
	}
	return t, id, nil
}

// list answers up to limit exports of the account, newest first, after the
// cursor, and whether there are more.
func (s *Exports) list(ctx context.Context, account, after string, limit int) ([]Export, bool, error) {
	db, err := s.inv.database()
	if err != nil {
		return nil, false, err
	}
	query := `SELECT ` + exportColumns + ` FROM account_exports WHERE account_id = $1`
	args := []any{account}
	if after != "" {
		at, id, err := decodeExportCursor(after)
		if err != nil {
			return nil, false, err
		}
		query += ` AND (requested_at, id) < ($2, $3)`
		args = append(args, at, id)
	}
	query += fmt.Sprintf(` ORDER BY requested_at DESC, id DESC LIMIT %d`, limit+1)
	rows, err := db.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("accountExports: %w", err)
	}
	defer rows.Close()
	var out []Export
	for rows.Next() {
		x, err := scanExport(rows)
		if err != nil {
			return nil, false, fmt.Errorf("accountExports: %w", err)
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("accountExports: %w", err)
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}

// ---------------------------------------------------------------------------
// The lease. Every statement below is the runner's.

// sweepDrafts deletes drafts not started within their lifetime. The row goes,
// and its sealed passphrase with it.
func (s *Exports) sweepDrafts(ctx context.Context) error {
	db, err := s.inv.database()
	if err != nil {
		return err
	}
	_, err = db.DB.ExecContext(ctx, `DELETE FROM account_exports
		 WHERE state = 'draft' AND requested_at <= clock_timestamp() - make_interval(secs => $1)`, exportDraftLifetime.Seconds())
	if err != nil {
		return fmt.Errorf("account export: sweep drafts: %w", err)
	}
	return nil
}

// keptObjectsPerTick bounds how many kept files one sweep tries to delete, so
// storage that answers slowly cannot stretch a tick without end.
const keptObjectsPerTick = 20

// sweepKeptObjects clears the key of a row that ended failed or cancelled and
// still records one. Most such keys name nothing: the run records its key
// before it commits and deletes its own file when it fails, so the delete here
// answers at once. The rest are files a delete could not reach when the row
// ended (the take-over, the claim or the hand-back, or the run's own cleanup).
// At most keptObjectsPerTick a tick, in no fixed order, so a file that cannot
// be deleted does not hold the others back. The key is cleared, guarded on
// itself, once the delete succeeds; what failed is logged once for the tick
// and tried again at a later one. A complete row's key is its file, served and
// removed by a person, and is never touched here.
func (s *Exports) sweepKeptObjects(ctx context.Context) error {
	db, err := s.inv.database()
	if err != nil {
		return err
	}
	if err := s.dropRecordedObjects(ctx, db,
		`SELECT id, object_key FROM account_exports
		 WHERE state IN ('failed', 'cancelled') AND object_key <> ''
		 ORDER BY random() LIMIT $1`,
		`UPDATE account_exports SET object_key = ''
		 WHERE id = $1 AND object_key = $2 AND state IN ('failed', 'cancelled')`, keptObjectsPerTick); err != nil {
		return fmt.Errorf("account export: sweep kept files: %w", err)
	}
	return nil
}

// reclaimStale takes back every running export whose heartbeat is older than
// exportStaleAfter: the node running it stopped. A snapshot cannot be handed
// over, so the run is queued again and starts from the beginning on whichever
// node claims it next; after its last attempt it fails instead, and one a
// person asked to cancel is cancelled. Both final states clear the sealed
// passphrase in the same statement. The rows that ended are audited and
// counted by the caller.
//
// A STOPPED ATTEMPT MAY HAVE COMMITTED ITS FILE. The run records the key just
// before it commits (recordObject), so a node that died while reading the file
// back left a whole file that only that key names. For such a row, in
// this order: its lease is taken (claimed_by cleared, so no late write of the
// stopped node lands, and it cannot finish the run over a file being deleted),
// the file is deleted through the blob store, and the key is cleared. Then:
//
//   - a row that ENDS (a cancel requested, or its last attempt) ends whether
//     or not the delete succeeded, keeping its key when it did not; the sweep
//     of kept files tries again (sweepKeptObjects).
//   - a row that would be REQUEUED waits for the delete: it stays running with
//     its key and no lease, and the next take-over tries again. The next
//     attempt needs storage anyway, and it writes under a key of its own, so
//     requeueing would leave nothing to reach the old file. A person's cancel
//     turns it into a row that ends, which frees the installation's queue.
func (s *Exports) reclaimStale(ctx context.Context) error {
	db, err := s.inv.database()
	if err != nil {
		return err
	}
	stale := exportStaleAfter.Seconds()
	if err := s.dropRecordedObjects(ctx, db,
		`UPDATE account_exports SET claimed_by = ''
		 WHERE state = 'running' AND object_key <> '' AND heartbeat_at < clock_timestamp() - make_interval(secs => $1)
		 RETURNING id, object_key`,
		`UPDATE account_exports SET object_key = ''
		 WHERE id = $1 AND object_key = $2 AND state = 'running' AND claimed_by = ''`, stale); err != nil {
		return fmt.Errorf("account export: take over stale runs: %w", err)
	}
	rows, err := db.DB.QueryContext(ctx, `UPDATE account_exports SET
		   state = CASE WHEN cancel_requested THEN 'cancelled'
		                WHEN attempt >= $2 THEN 'failed' ELSE 'queued' END,
		   failure_code = CASE WHEN NOT cancel_requested AND attempt >= $2 THEN $3 ELSE failure_code END,
		   failure_detail = CASE WHEN NOT cancel_requested AND attempt >= $2
		                         THEN 'The node running this export stopped, and it was started three times.' ELSE failure_detail END,
		   finished_at = CASE WHEN cancel_requested OR attempt >= $2 THEN clock_timestamp() ELSE finished_at END,
		   sealed_passphrase = CASE WHEN cancel_requested OR attempt >= $2 THEN NULL ELSE sealed_passphrase END,
		   claimed_by = '', heartbeat_at = NULL, phase = ''
		 WHERE state = 'running' AND heartbeat_at < clock_timestamp() - make_interval(secs => $1)
		   AND (object_key = '' OR cancel_requested OR attempt >= $2)
		 RETURNING `+exportColumns, stale, exportMaxAttempts, FailureFailed)
	if err != nil {
		return fmt.Errorf("account export: take over stale runs: %w", err)
	}
	var ended []Export
	for rows.Next() {
		x, err := scanExport(rows)
		if err != nil {
			rows.Close()
			return fmt.Errorf("account export: take over stale runs: %w", err)
		}
		if x.State == exportQueued {
			s.logger.Warn("account export: a run stopped heartbeating and is queued again", "exportId", x.ID, "attempt", x.Attempt)
			continue
		}
		ended = append(ended, x)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("account export: take over stale runs: %w", err)
	}
	rows.Close()
	for _, x := range ended {
		s.finished(ctx, x)
	}
	return nil
}

// dropRecordedObjects deletes the file each row named by find records, then
// clears the row's key with clear ($1 the id, $2 the key, guarded so a key that
// changed meanwhile is left alone). find's own arguments are args. A delete
// that fails keeps the key, and the row is left for a later pass, which tries
// again. The failures of one call are logged in one line, with their number
// and the first export's id: storage that is not answering fails every delete
// of the call the same way.
func (s *Exports) dropRecordedObjects(ctx context.Context, db *bun.DB, find, clear string, args ...any) error {
	rows, err := db.DB.QueryContext(ctx, find, args...)
	if err != nil {
		return err
	}
	type recorded struct{ id, key string }
	var found []recorded
	for rows.Next() {
		var r recorded
		if err := rows.Scan(&r.id, &r.key); err != nil {
			rows.Close()
			return err
		}
		found = append(found, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	failed, firstID := 0, ""
	var firstErr error
	for _, r := range found {
		if err := s.deleteRecordedObject(ctx, r.key); err != nil {
			if failed == 0 {
				firstID, firstErr = r.id, err
			}
			failed++
			continue
		}
		if _, err := db.DB.ExecContext(ctx, clear, r.id, r.key); err != nil {
			return err
		}
	}
	if failed > 0 {
		s.logger.Error("account export: files of attempts that did not finish could not be deleted; their keys stay on the record and they are tried again",
			"failed", failed, "firstExportId", firstID, "error", firstErr)
	}
	return nil
}

// deleteRecordedObject deletes one recorded file through the blob store Remove
// uses, which answers a missing object as done.
func (s *Exports) deleteRecordedObject(ctx context.Context, key string) error {
	blobs, err := s.blobStore()
	if err != nil {
		return err
	}
	return blobs.Delete(ctx, key)
}

// exportClaimLock is the transaction-level advisory lock the claim takes, so
// "no export is running" and "this one is now running" are one decision across
// replicas. Two nodes that each saw nothing running would otherwise each take
// a different queued row under SKIP LOCKED. Transaction-scoped, so it holds on
// the pooled endpoint and is released at commit with the row it wrote.
const exportClaimLock = `hashtext('memql.accounts.export.claim')`

// claim takes the oldest queued export when none is running on the whole
// installation, and answers it with its sealed passphrase (empty when not
// protected). The attempt is counted here and the progress is reset: a new
// attempt starts from the beginning.
//
// THE ATTEMPT LIMIT HOLDS AT THE CLAIM TOO. A run a stopping node hands back
// keeps its attempt counted (requeue), so a queued row can already be at the
// limit. Such a row is failed export_failed in this transaction, its sealed
// passphrase cleared in the same statement, and is never claimed: rolling
// restarts cannot run one export without end. A queued row a person asked to
// cancel is cancelled the same way, never claimed and never failed. The rows it
// ended are audited and counted after the commit.
//
// A queued row that still records a file (an earlier attempt's, which the
// hand-back and the take-over delete before they requeue, so this is the
// leftover of a delete that failed) has the file deleted and the key cleared
// first. One the claim ends is ended either way, keeping its key for the sweep
// of kept files when the delete failed; one it would run is not claimed while
// its key stays, since a new attempt would record its own key over the only
// record of the old file.
//
// Every value goes as a server parameter on the transaction's own *sql.Tx, as
// every other statement here does.
func (s *Exports) claim(ctx context.Context, nodeID string) (Export, string, bool, error) {
	db, err := s.inv.database()
	if err != nil {
		return Export{}, "", false, err
	}
	if err := s.dropRecordedObjects(ctx, db,
		`SELECT id, object_key FROM account_exports WHERE state = 'queued' AND object_key <> '' ORDER BY id LIMIT $1`,
		`UPDATE account_exports SET object_key = '' WHERE id = $1 AND object_key = $2 AND state = 'queued'`, keptObjectsPerTick); err != nil {
		return Export{}, "", false, fmt.Errorf("account export: claim: %w", err)
	}
	var out Export
	var sealed sql.NullString
	var exhausted []Export
	claimed := false
	err = db.RunInTx(ctx, nil, func(ctx context.Context, btx bun.Tx) error {
		tx := btx.Tx
		out, sealed, exhausted, claimed = Export{}, sql.NullString{}, nil, false
		var locked bool
		if err := tx.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(`+exportClaimLock+`)`).Scan(&locked); err != nil {
			return err
		}
		if !locked {
			return nil // another replica is claiming now
		}
		var running bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM account_exports WHERE state = 'running')`).Scan(&running); err != nil {
			return err
		}
		if running {
			return nil
		}
		rows, err := tx.QueryContext(ctx, `UPDATE account_exports SET
			   state = CASE WHEN cancel_requested THEN 'cancelled' ELSE 'failed' END,
			   failure_code = CASE WHEN cancel_requested THEN failure_code ELSE $2 END,
			   failure_detail = CASE WHEN cancel_requested THEN failure_detail
			                         ELSE 'This export was started three times and did not finish.' END,
			   finished_at = clock_timestamp(), sealed_passphrase = NULL, claimed_by = '', heartbeat_at = NULL, phase = ''
			 WHERE state = 'queued' AND (cancel_requested OR attempt >= $1)
			 RETURNING `+exportColumns, exportMaxAttempts, FailureFailed)
		if err != nil {
			return err
		}
		for rows.Next() {
			x, err := scanExport(rows)
			if err != nil {
				rows.Close()
				return err
			}
			exhausted = append(exhausted, x)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		x, err := scanExport(tx.QueryRowContext(ctx, `UPDATE account_exports SET
			   state = 'running', attempt = attempt + 1, claimed_by = $1, heartbeat_at = clock_timestamp(),
			   started_at = COALESCE(started_at, clock_timestamp()), phase = '',
			   kinds_total = 0, kinds_done = 0, rows_written = 0, versions_written = 0, bytes_total = 0, bytes_done = 0
			 WHERE id = (SELECT id FROM account_exports WHERE state = 'queued' AND attempt < $2
			                AND NOT cancel_requested AND object_key = ''
			              ORDER BY requested_at, id LIMIT 1 FOR UPDATE SKIP LOCKED)
			 RETURNING `+exportColumns+`, sealed_passphrase`, nodeID, exportMaxAttempts), &sealed)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		out, claimed = x, true
		return nil
	})
	if err != nil {
		return Export{}, "", false, fmt.Errorf("account export: claim: %w", err)
	}
	for _, x := range exhausted {
		if x.State == exportFailed {
			s.logger.Warn("account export: a run reached its attempt limit and is failed", "exportId", x.ID, "attempt", x.Attempt)
		}
		s.finished(ctx, x)
	}
	return out, sealed.String, claimed, nil
}

// heartbeat renews the lease of this node's attempt and answers whether a
// person asked to cancel. ok=false means the lease is gone (taken over, or the
// row is no longer running): the run must stop and write nothing.
func (s *Exports) heartbeat(ctx context.Context, x Export, nodeID string) (cancelRequested, ok bool, err error) {
	db, err := s.inv.database()
	if err != nil {
		return false, false, err
	}
	err = db.DB.QueryRowContext(ctx, `UPDATE account_exports SET heartbeat_at = clock_timestamp()
		 WHERE id = $1 AND claimed_by = $2 AND attempt = $3 AND state = 'running'
		 RETURNING cancel_requested`, x.ID, nodeID, x.Attempt).Scan(&cancelRequested)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return cancelRequested, true, nil
}

// writeProgress records where the attempt stands.
func (s *Exports) writeProgress(ctx context.Context, x Export, nodeID string, p ExportProgress) error {
	db, err := s.inv.database()
	if err != nil {
		return err
	}
	_, err = db.DB.ExecContext(ctx, `UPDATE account_exports SET phase = $4, kinds_total = $5, kinds_done = $6,
		   rows_written = $7, versions_written = $8, bytes_total = $9, bytes_done = $10
		 WHERE id = $1 AND claimed_by = $2 AND attempt = $3 AND state = 'running'`,
		x.ID, nodeID, x.Attempt, p.Phase, p.KindsTotal, p.KindsDone, p.RowsWritten, p.VersionsWritten, p.BytesTotal, p.BytesDone)
	return err
}

// finishComplete, finishFailed and finishCancelled write the attempt's final
// state, each clearing the sealed passphrase in the same statement. They are
// guarded on this node's attempt, so a node that lost its run writes nothing.
func (s *Exports) finishComplete(ctx context.Context, x Export, nodeID string, r ExportResult) (Export, bool, error) {
	var report any
	if len(r.Report) > 0 {
		report = string(r.Report)
	}
	return s.update(ctx, `UPDATE account_exports SET state = 'complete', sealed_passphrase = NULL, finished_at = clock_timestamp(),
		   phase = '', object_key = $4, file_name = $5, sha256 = $6, size_bytes = $7, report = $8::jsonb
		 WHERE id = $1 AND claimed_by = $2 AND attempt = $3 AND state = 'running'`,
		x.ID, nodeID, x.Attempt, r.ObjectKey, r.FileName, r.SHA256, r.SizeBytes, report)
}

func (s *Exports) finishFailed(ctx context.Context, x Export, nodeID, code, detail string) (Export, bool, error) {
	return s.update(ctx, `UPDATE account_exports SET state = 'failed', sealed_passphrase = NULL, finished_at = clock_timestamp(),
		   phase = '', failure_code = $4, failure_detail = $5
		 WHERE id = $1 AND claimed_by = $2 AND attempt = $3 AND state = 'running'`,
		x.ID, nodeID, x.Attempt, code, detail)
}

func (s *Exports) finishCancelled(ctx context.Context, x Export, nodeID string) (Export, bool, error) {
	return s.update(ctx, `UPDATE account_exports SET state = 'cancelled', sealed_passphrase = NULL, finished_at = clock_timestamp(), phase = ''
		 WHERE id = $1 AND claimed_by = $2 AND attempt = $3 AND state = 'running'`,
		x.ID, nodeID, x.Attempt)
}

// recordObject writes the key of the file this attempt is about to commit,
// guarded on the attempt as every write of a running attempt is (id, the node
// holding it, attempt, running). It answers false when the attempt no longer
// holds the run; the caller then commits nothing and stops, as for any lost
// lease. An attempt that names no node holds nothing: the
// take-over clears claimed_by while it deletes a stopped attempt's file, and
// an empty guard would match that row.
func (s *Exports) recordObject(ctx context.Context, x Export, key string) (bool, error) {
	if strings.TrimSpace(x.ClaimedBy) == "" || strings.TrimSpace(key) == "" {
		return false, nil
	}
	db, err := s.inv.database()
	if err != nil {
		return false, err
	}
	res, err := db.DB.ExecContext(ctx, `UPDATE account_exports SET object_key = $4
		 WHERE id = $1 AND claimed_by = $2 AND attempt = $3 AND state = 'running'`,
		x.ID, x.ClaimedBy, x.Attempt, key)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// requeue hands this node's attempt back when the node is stopping, so another
// replica starts it at once instead of after the stale window. The attempt
// stays counted. A run a person asked to cancel ends cancelled instead, its
// sealed passphrase cleared in the same statement, and is audited here.
//
// A file the attempt recorded is deleted first and the key cleared with the
// hand-back. When the delete fails, a cancelled run still ends (keeping its key
// for the sweep of kept files), and one that would be queued again is not
// handed back (false): the row stays running with its key, and the take-over
// deals with it once the lease is stale.
func (s *Exports) requeue(ctx context.Context, x Export, nodeID string) (bool, error) {
	db, err := s.inv.database()
	if err != nil {
		return false, err
	}
	var key string
	var cancelRequested bool
	err = db.DB.QueryRowContext(ctx, `SELECT object_key, cancel_requested FROM account_exports
		 WHERE id = $1 AND claimed_by = $2 AND attempt = $3 AND state = 'running'`, x.ID, nodeID, x.Attempt).Scan(&key, &cancelRequested)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	deleted := key == ""
	if !deleted {
		if derr := s.deleteRecordedObject(ctx, key); derr != nil {
			s.logger.Error("account export: the file of an attempt being handed back could not be deleted; its key stays on the record and it is tried again",
				"exportId", x.ID, "error", derr)
		} else {
			deleted = true
		}
	}
	if !deleted && !cancelRequested {
		return false, nil
	}
	out, ok, err := s.update(ctx, `UPDATE account_exports SET
		   state = CASE WHEN cancel_requested THEN 'cancelled' ELSE 'queued' END,
		   finished_at = CASE WHEN cancel_requested THEN clock_timestamp() ELSE finished_at END,
		   sealed_passphrase = CASE WHEN cancel_requested THEN NULL ELSE sealed_passphrase END,
		   object_key = CASE WHEN $5 THEN '' ELSE object_key END,
		   claimed_by = '', heartbeat_at = NULL, phase = ''
		 WHERE id = $1 AND claimed_by = $2 AND attempt = $3 AND state = 'running' AND object_key = $4
		   AND (cancel_requested OR $5)`,
		x.ID, nodeID, x.Attempt, key, deleted)
	if err != nil || !ok {
		return ok, err
	}
	if out.State == exportCancelled {
		s.finished(ctx, out)
	}
	return true, nil
}
