package memql

// account_assignment_write.go -- the statements the assignment of existing rows
// writes with (account_assignment.go): the account stamped into a row's newest
// version in place, the channel conversion, the account lists held to the row's
// own account, and the conflicts recorded with their candidates. The move and
// the follow tasks (account_move.go, account_follow.go) write a row's account
// through the same stamp statement, so every in-place account write, and every
// compressed chunk it reaches, is handled one way.
//
// # In place, guarded, and silent
//
// The stamp updates rows by primary key, (id, "createdAt"), so only the version
// the batch judged is touched, and it is guarded on the row still holding the
// account it was read with (assignmentWrite.From: none, for the assignment), so
// a write that changed the row's account since the batch read it is never
// overwritten. The list trim is the one statement that touches older versions
// (trimAccountLists), because every version of a row travels with it. A concept whose observation-only writes live in
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
// library_fence_refused conflicts, each naming the account the rules chose as
// its candidate. Any other error fails the batch.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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
// none), whether the channel conversion runs first, and the account the row
// must still hold for the stamp to land (From, bare; "" for none). The
// assignment writes with From "": it only ever gives an account to a row that
// has none. The move and the follow tasks (account_move.go, account_follow.go)
// write with the account they read the row in, so a row whose account changed
// since that read is left as it is: the same statement, guarded on what was
// read, for every in-place account write.
type assignmentWrite struct {
	ID      string `json:"id"`
	At      string `json:"at"`
	Account string `json:"account"`
	Move    bool   `json:"move"`
	From    string `json:"held"`
}

type assignmentConflict struct {
	ID, Kind, Detail string
	// Candidates are the accounts the row could belong to, bare.
	Candidates []string
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
		case outcomeShared, outcomeLeftAlone, outcomeLeftOut:
			// The rows the job keeps without an account on purpose.
			counts.shared++
		case outcomeWaiting:
			counts.waiting++
		case outcomeConflict:
			conflicts = append(conflicts, assignmentConflict{ID: d.row.ID, Kind: d.kind, Detail: d.detail, Candidates: d.candidates})
		}
		if d.outcome == outcomeAssigned || d.moveChannelAccount {
			writes = append(writes, assignmentWrite{
				ID: d.row.ID, At: versionKey(d.row.At),
				Account: d.account, Move: d.moveChannelAccount,
			})
		}
	}

	written, err := writeInPlaceAroundFence(ctx, tx, meta.Name, writes)
	if err != nil {
		return counts, false, err
	}
	changed, applied := written.changed, written.applied
	for _, w := range written.refused {
		// The candidate is the account the rules chose: placing the row there
		// is what the job would have done.
		conflicts = append(conflicts, assignmentConflict{ID: w.ID, Kind: conflictLibraryFenceRefused,
			Detail:     "the Library retirement fence refused the update: the file's bytes were permanently retired",
			Candidates: fenceRefusalCandidates(w.Account)})
	}
	for _, w := range applied {
		if w.Account != "" {
			counts.assigned++
			placed = append(placed, w.ID)
		}
	}
	counts.conflicts = len(conflicts)

	if conceptDeclaresField(meta, accountListField) {
		refusedIDs := map[string]bool{}
		for _, w := range written.refused {
			refusedIDs[w.ID] = true
		}
		if err := trimAccountLists(ctx, tx, meta.Name, listTrimsFor(plan, applied, refusedIDs)); err != nil {
			return counts, false, err
		}
	}

	if err := recordAssignmentConflicts(ctx, tx, meta.Name, conflicts); err != nil {
		return counts, false, err
	}
	if err := resolveAssignmentConflicts(ctx, tx, meta.Name, placed); err != nil {
		return counts, false, err
	}
	return counts, changed > 0, nil
}

// fenceRefusalCandidates is the candidates of a row the Library fence refused.
// A write that chose no account (a conversion alone) names none: an empty
// string in the list would be an account nobody can look up.
func fenceRefusalCandidates(account string) []string {
	if account == "" {
		return []string{}
	}
	return []string{account}
}

// inPlaceOutcome is what one in-place batch did: how many versioned rows
// changed, the writes that landed, the ids whose account the stamp changed
// (a write whose `held` guard no longer matched is not among them), and the
// writes the Library fence refused, which changed nothing.
type inPlaceOutcome struct {
	changed int
	applied []assignmentWrite
	stamped []string
	refused []assignmentWrite
}

// writeInPlaceAroundFence writes a batch in place (writeAssignments) and steps
// around the Library fence (file header). Any other error is returned and the
// caller fails its batch.
func writeInPlaceAroundFence(ctx context.Context, tx bun.Tx, concept string, writes []assignmentWrite) (inPlaceOutcome, error) {
	var out inPlaceOutcome
	if len(writes) == 0 {
		return out, nil
	}
	n, stamped, err := writeAssignmentsGuarded(ctx, tx, concept, writes)
	if err == nil {
		return inPlaceOutcome{changed: n, applied: writes, stamped: stamped}, nil
	}
	if !isLibraryFenceRefusal(err) {
		return out, err
	}
	for _, w := range writes {
		one, ids, err := writeAssignmentsGuarded(ctx, tx, concept, []assignmentWrite{w})
		if isLibraryFenceRefusal(err) {
			out.refused = append(out.refused, w)
			continue
		}
		if err != nil {
			return inPlaceOutcome{}, err
		}
		out.changed += one
		out.applied = append(out.applied, w)
		out.stamped = append(out.stamped, ids...)
	}
	return out, nil
}

// listTrim is one row whose every version's list is held to its own account.
type listTrim struct {
	ID      string `json:"id"`
	Account string `json:"account"`
}

// listTrimsFor is the rows of a batch whose lists the job holds to their
// account: those that have one (kept, or assigned now) and tombstones, which
// are decided by their newest version like any row. A row with no account yet
// (waiting, left alone, shared, in conflict) is left as it is until it gets
// one: trimming it now would empty a list that may still be what files it.
func listTrimsFor(plan []assignmentDecision, applied []assignmentWrite, refused map[string]bool) []listTrim {
	assigned := map[string]string{}
	for _, w := range applied {
		if w.Account != "" {
			assigned[w.ID] = w.Account
		}
	}
	var out []listTrim
	for _, d := range plan {
		if refused[d.row.ID] {
			continue
		}
		switch {
		case d.outcome == outcomeKept:
			out = append(out, listTrim{ID: d.row.ID, Account: bareAccount(d.row.Payload[accountField])})
		case d.outcome == outcomeAssigned && assigned[d.row.ID] != "":
			out = append(out, listTrim{ID: d.row.ID, Account: assigned[d.row.ID]})
		case d.outcome == outcomeSkip:
			out = append(out, listTrim{ID: d.row.ID, Account: bareAccount(d.row.Payload[accountField])})
		}
	}
	return out
}

// trimAccountLists holds the list of EVERY version of each row, and of its
// observation copy, to the row's own account: the account when the list names
// it (either spelling), otherwise empty (design record
// docs/superpowers/specs/2026-11-12-account-export-design.md, section I, "The
// account lists"). Older versions too, because every version of a row travels
// with it and no file may carry another account's id through a list. In place,
// like the stamp, and guarded on the list not already being as the rule leaves
// it, so running it twice changes nothing. A row the Library fence refuses is
// stepped around as writeInPlaceAroundFence does, and left as it is.
func trimAccountLists(ctx context.Context, tx bun.Tx, concept string, trims []listTrim) error {
	if len(trims) == 0 {
		return nil
	}
	err := trimAccountListsGuarded(ctx, tx, concept, trims)
	if !isLibraryFenceRefusal(err) {
		return err
	}
	for _, one := range trims {
		if err := trimAccountListsGuarded(ctx, tx, concept, []listTrim{one}); err != nil && !isLibraryFenceRefusal(err) {
			return err
		}
	}
	return nil
}

func trimAccountListsGuarded(ctx context.Context, tx bun.Tx, concept string, trims []listTrim) error {
	raw, err := json.Marshal(trims)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT account_list_trim`); err != nil {
		return err
	}
	tables := []string{`"MemoryNodes"`}
	if memorynodes.HasLiveness(concept) {
		tables = append(tables, `memory_liveness`)
	}
	for _, table := range tables {
		_, err = tx.ExecContext(ctx, `UPDATE `+table+` AS n
			   SET payload = jsonb_set(n.payload, '{accountIds}',
			         CASE WHEN k.account <> '' AND (jsonb_exists(n.payload->'accountIds', k.account)
			                                    OR jsonb_exists(n.payload->'accountIds', ? || k.account))
			              THEN jsonb_build_array(k.account) ELSE '[]'::jsonb END)
			  FROM jsonb_to_recordset(?::jsonb) AS k(id text, account text)
			 WHERE n.concept = ? AND n.id = k.id
			   AND jsonb_typeof(n.payload->'accountIds') = 'array'
			   AND n.payload->'accountIds' <> '[]'::jsonb
			   AND n.payload->'accountIds' <> jsonb_build_array(k.account)`,
			accountCanonicalPrefix, string(raw), concept)
		if err != nil {
			break
		}
	}
	if err != nil {
		if _, rbErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT account_list_trim`); rbErr != nil {
			return fmt.Errorf("%w (and the rollback to the savepoint failed: %v)", err, rbErr)
		}
		return err
	}
	_, err = tx.ExecContext(ctx, `RELEASE SAVEPOINT account_list_trim`)
	return err
}

// writeAssignmentsGuarded runs writeAssignments under a savepoint, so a refusal
// rolls back only this attempt and leaves the transaction usable.
func writeAssignmentsGuarded(ctx context.Context, tx bun.Tx, concept string, writes []assignmentWrite) (int, []string, error) {
	if _, err := tx.ExecContext(ctx, `SAVEPOINT account_assignment`); err != nil {
		return 0, nil, err
	}
	n, stamped, err := writeAssignments(ctx, tx, concept, writes)
	if err != nil {
		if _, rbErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT account_assignment`); rbErr != nil {
			return 0, nil, fmt.Errorf("%w (and the rollback to the savepoint failed: %v)", err, rbErr)
		}
		return 0, nil, err
	}
	if _, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT account_assignment`); err != nil {
		return 0, nil, err
	}
	return n, stamped, nil
}

// versionKey formats a version's createdAt for the key list writeAssignments
// matches on.
func versionKey(at time.Time) string {
	return at.UTC().Format("2006-01-02T15:04:05.999999Z07:00")
}

// writeAssignments is the channel conversion, then the stamp, on the versioned
// row and its liveness copy. It returns how many updates of versioned rows it
// made (a converted and stamped row counts twice; callers ask only for > 0),
// and the ids whose versioned row the stamp changed, which is what a write
// guarded on `held` needs: a row whose account changed since it was read is
// not among them. It reads nothing: the rows were chosen by the batch, and FROM
// names the batch's own key list.
func writeAssignments(ctx context.Context, tx bun.Tx, concept string, writes []assignmentWrite) (int, []string, error) {
	raw, err := json.Marshal(writes)
	if err != nil {
		return 0, nil, err
	}
	var stamped []string
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
			  FROM jsonb_to_recordset(?::jsonb) AS k(id text, at timestamptz, account text, move boolean, held text)
			 WHERE k.move AND n.concept = ? AND n.id = k.id`+version+`
			   AND COALESCE(n.payload->>'accountId', '') <> ''`, string(raw), concept)
		if err != nil {
			return 0, nil, err
		}
		if i == 0 {
			n, err := moved.RowsAffected()
			if err != nil {
				return 0, nil, err
			}
			changed += int(n)
		}
		rows, err := tx.QueryContext(ctx, `UPDATE `+table+` AS n
			   SET payload = jsonb_set(n.payload, '{accountId}', to_jsonb(k.account), true)
			  FROM jsonb_to_recordset(?::jsonb) AS k(id text, at timestamptz, account text, move boolean, held text)
			 WHERE k.account <> '' AND n.concept = ? AND n.id = k.id`+version+`
			   AND COALESCE(n.payload->>'accountId', '') IN (k.held, CASE WHEN k.held = '' THEN '' ELSE ? || k.held END)
			RETURNING n.id`, string(raw), concept, accountCanonicalPrefix)
		if err != nil {
			return 0, nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return 0, nil, err
			}
			if i == 0 {
				changed++
				stamped = append(stamped, id)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return 0, nil, err
		}
	}
	return changed, stamped, nil
}

// recordAssignmentConflicts records rows the job will not guess at, with their
// candidates. A conflict found again keeps the time it was first found, takes
// the candidates found now, and is reopened if it had been marked resolved --
// unless an owner left it out, a decision the job never undoes. (The rules
// already leave such a row alone; the guard holds should a left-out row's
// conflict be of another kind than the one found now.)
func recordAssignmentConflicts(ctx context.Context, tx bun.Tx, concept string, conflicts []assignmentConflict) error {
	for _, c := range conflicts {
		candidates := c.Candidates
		if candidates == nil {
			candidates = []string{} // the column is NOT NULL, and a nil array is NULL
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO account_assignment_conflict (concept, row_id, kind, detail, candidates)
			VALUES (?, ?, ?, ?, ?::text[])
			ON CONFLICT (concept, row_id, kind) DO UPDATE
			   SET detail = EXCLUDED.detail, candidates = EXCLUDED.candidates, resolved_at = NULL
			 WHERE account_assignment_conflict.left_out_at IS NULL`,
			concept, c.ID, c.Kind, c.Detail, pq.Array(candidates)); err != nil {
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
