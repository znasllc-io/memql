package memql

// account_assignment_write.go -- the statements the assignment of existing rows
// writes with (account_assignment.go): the account stamped into a row's newest
// version in place, the channel conversion, and the conflicts recorded.
//
// # In place, guarded, and silent
//
// Each statement updates rows by primary key, (id, "createdAt"), so only the
// version the batch judged is touched, and each is guarded on the row still
// having no account, so a write that stamped the row since the batch read it is
// never overwritten. A concept whose observation-only writes live in
// memory_liveness (memorynodes.HasLiveness) has its row's copy there stamped
// too: the copy is the row's newest observation, and the liveness trigger
// compares a new observation's business fields against it, so a copy without
// the account would send the next heartbeat to the versioned table as a
// business edit.
//
// # The Library fence
//
// pipeline_library_retirement_fence raises on any UPDATE of a Library file,
// index row or file version whose bytes were permanently retired. One such row
// would abort the whole batch, so the batch runs under a savepoint: on that
// error it is rolled back and retried one row at a time, each under its own
// savepoint, and the refused rows are left without an account and recorded as
// library_fence_refused conflicts. Any other error fails the batch.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lib/pq"
	"github.com/uptrace/bun"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// libraryFenceMessage is the text guard_retired_pipeline_library_file raises
// (migration 20261020020000_pipeline_artifact_library_fence).
const libraryFenceMessage = "pipeline artifact bytes were permanently retired"

func isLibraryFenceRefusal(err error) bool {
	return err != nil && strings.Contains(err.Error(), libraryFenceMessage)
}

// assignmentWrite is one row's write: its key, the account to stamp ("" for
// none), and whether the channel conversion runs first.
type assignmentWrite struct {
	ID      string `json:"id"`
	At      string `json:"at"`
	Account string `json:"account"`
	Move    bool   `json:"move"`
}

type assignmentConflict struct {
	ID, Kind, Detail string
}

// applyAssignmentPlan writes a batch's decisions and returns its counts and
// whether any row changed.
func applyAssignmentPlan(ctx context.Context, tx bun.Tx, meta *memorynodes.Concept, plan []assignmentDecision) (assignmentCounts, bool, error) {
	var counts assignmentCounts
	var writes []assignmentWrite
	var conflicts []assignmentConflict
	var placed []string
	for _, d := range plan {
		switch d.outcome {
		case outcomeKept:
			counts.kept++
			placed = append(placed, d.row.ID)
		case outcomeShared:
			counts.shared++
		case outcomeWaiting:
			counts.waiting++
		case outcomeConflict:
			conflicts = append(conflicts, assignmentConflict{ID: d.row.ID, Kind: d.kind, Detail: d.detail})
		}
		if d.outcome == outcomeAssigned || d.moveChannelAccount {
			writes = append(writes, assignmentWrite{
				ID: d.row.ID, At: d.row.At.UTC().Format("2006-01-02T15:04:05.999999Z07:00"),
				Account: d.account, Move: d.moveChannelAccount,
			})
		}
	}

	changed, applied := 0, writes
	if len(writes) > 0 {
		n, err := writeAssignmentsGuarded(ctx, tx, meta.Name, writes)
		if isLibraryFenceRefusal(err) {
			n, applied = 0, nil
			for _, w := range writes {
				one, err := writeAssignmentsGuarded(ctx, tx, meta.Name, []assignmentWrite{w})
				if isLibraryFenceRefusal(err) {
					conflicts = append(conflicts, assignmentConflict{ID: w.ID, Kind: conflictLibraryFenceRefused,
						Detail: "the Library retirement fence refused the update: the file's bytes were permanently retired"})
					continue
				}
				if err != nil {
					return counts, false, err
				}
				n += one
				applied = append(applied, w)
			}
		} else if err != nil {
			return counts, false, err
		}
		changed = n
	}
	for _, w := range applied {
		if w.Account != "" {
			counts.assigned++
			placed = append(placed, w.ID)
		}
	}
	counts.conflicts = len(conflicts)

	if err := recordAssignmentConflicts(ctx, tx, meta.Name, conflicts); err != nil {
		return counts, false, err
	}
	if err := resolveAssignmentConflicts(ctx, tx, meta.Name, placed); err != nil {
		return counts, false, err
	}
	return counts, changed > 0, nil
}

// writeAssignmentsGuarded runs writeAssignments under a savepoint, so a refusal
// rolls back only this attempt and leaves the transaction usable.
func writeAssignmentsGuarded(ctx context.Context, tx bun.Tx, concept string, writes []assignmentWrite) (int, error) {
	if _, err := tx.ExecContext(ctx, `SAVEPOINT account_assignment`); err != nil {
		return 0, err
	}
	n, err := writeAssignments(ctx, tx, concept, writes)
	if err != nil {
		if _, rbErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT account_assignment`); rbErr != nil {
			return 0, fmt.Errorf("%w (and the rollback to the savepoint failed: %v)", err, rbErr)
		}
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT account_assignment`); err != nil {
		return 0, err
	}
	return n, nil
}

// writeAssignments is the channel conversion, then the stamp, on the versioned
// row and its liveness copy. It returns how many updates of versioned rows it
// made (a converted and stamped row counts twice; callers ask only for > 0). It
// reads nothing: the rows were chosen by the batch, and FROM names the batch's
// own key list.
func writeAssignments(ctx context.Context, tx bun.Tx, concept string, writes []assignmentWrite) (int, error) {
	raw, err := json.Marshal(writes)
	if err != nil {
		return 0, err
	}
	tables := []string{`"MemoryNodes"`}
	if memorynodes.HasLiveness(concept) {
		tables = append(tables, `memory_liveness`)
	}
	changed := 0
	for i, table := range tables {
		// The versioned row is matched by its full key, so only the judged
		// version changes. The liveness copy is matched by id, its key there:
		// it is the row's one current observation, newer than that version.
		version := ` AND n."createdAt" = k.at`
		if table == `memory_liveness` {
			version = ""
		}
		// The conversion first: a channel row's old value is not an account,
		// and the stamp's guard would otherwise read it as one and skip the
		// row. The value moves to sourceAccountId when that is absent; when it
		// is present (a re-synced row) the newer copy there is kept and the old
		// value is only dropped from accountId (account_assignment_rules.go).
		moved, err := tx.ExecContext(ctx, `UPDATE `+table+` AS n
			   SET payload = CASE WHEN jsonb_exists(n.payload, 'sourceAccountId') THEN n.payload - 'accountId'
			                 ELSE (n.payload - 'accountId') || jsonb_build_object('sourceAccountId', n.payload->'accountId') END
			  FROM jsonb_to_recordset(?::jsonb) AS k(id text, at timestamptz, account text, move boolean)
			 WHERE k.move AND n.concept = ? AND n.id = k.id`+version+`
			   AND COALESCE(n.payload->>'accountId', '') <> ''`, string(raw), concept)
		if err != nil {
			return 0, err
		}
		if i == 0 {
			n, err := moved.RowsAffected()
			if err != nil {
				return 0, err
			}
			changed += int(n)
		}
		res, err := tx.ExecContext(ctx, `UPDATE `+table+` AS n
			   SET payload = jsonb_set(n.payload, '{accountId}', to_jsonb(k.account), true)
			  FROM jsonb_to_recordset(?::jsonb) AS k(id text, at timestamptz, account text, move boolean)
			 WHERE k.account <> '' AND n.concept = ? AND n.id = k.id`+version+`
			   AND COALESCE(n.payload->>'accountId', '') = ''`, string(raw), concept)
		if err != nil {
			return 0, err
		}
		if i == 0 {
			n, err := res.RowsAffected()
			if err != nil {
				return 0, err
			}
			changed += int(n)
		}
	}
	return changed, nil
}

// recordAssignmentConflicts records rows the job will not guess at. A conflict
// found again keeps the time it was first found and is reopened if it had been
// marked resolved.
func recordAssignmentConflicts(ctx context.Context, tx bun.Tx, concept string, conflicts []assignmentConflict) error {
	for _, c := range conflicts {
		if _, err := tx.ExecContext(ctx, `INSERT INTO account_assignment_conflict (concept, row_id, kind, detail)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (concept, row_id, kind) DO UPDATE SET detail = EXCLUDED.detail, resolved_at = NULL`,
			concept, c.ID, c.Kind, c.Detail); err != nil {
			return fmt.Errorf("account assignment: recording a conflict on %s: %w", c.ID, err)
		}
	}
	return nil
}

// resolveAssignmentConflicts marks the open conflicts of rows that now have an
// account as resolved: somebody placed the row, or what made it ambiguous went
// away and the job assigned it.
func resolveAssignmentConflicts(ctx context.Context, tx bun.Tx, concept string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE account_assignment_conflict SET resolved_at = clock_timestamp()
		WHERE concept = ? AND row_id = ANY(?::text[]) AND resolved_at IS NULL`, concept, pq.Array(ids))
	return err
}
