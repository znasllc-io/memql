package accounts

// export_readiness.go -- accountExportReadiness: the five conditions an export
// of an account waits for (design record
// docs/superpowers/specs/2026-11-12-account-export-design.md, section F,
// "Ready"), the count of rows in no account beside them, and the verdict.
//
// Every condition is read, never assumed. A seam that was not wired on this
// node (assignmentStates, undeclared, followPending) reads as NOT MET: an
// account is called ready only on questions that were asked.
//
// Prepare asks the same Readiness and refuses with export_not_ready naming
// what is not met; the run asks it again when it starts.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// The condition names, in the order the answer gives them.
const (
	conditionRecording = "recording"
	conditionAssigned  = "assigned"
	conditionDeclared  = "declared"
	conditionDecided   = "decided"
	conditionFollowed  = "followed"
)

// conditionLabels are the words the Export tab names each condition with
// (clients/os/src/apps/accounts/exports.ts, CONDITION_LABEL). A refusal a
// person reads names an unmet condition the same way the tab does, never by
// its name on the wire.
var conditionLabels = map[string]string{
	conditionRecording: "Accounts recorded on rows",
	conditionAssigned:  "Existing rows assigned",
	conditionDeclared:  "Every kind says whose it is",
	conditionDecided:   "Rows awaiting a decision",
	conditionFollowed:  "Rows following a moved parent",
}

// notReadyDetail is the export_not_ready sentence for the unmet conditions,
// in answer order.
func notReadyDetail(unmet []string) string {
	words := make([]string, 0, len(unmet))
	for _, name := range unmet {
		label, ok := conditionLabels[name]
		if !ok {
			label = "Another condition"
		}
		words = append(words, label)
	}
	return "This account cannot be exported yet. Not met: " + strings.Join(words, "; ") + "."
}

// Readiness is the answer for one account.
type Readiness struct {
	// Recording: the cluster records the account of every row (its switch is
	// on, which happens only once every node reads the field) and the node
	// answering is not held by MEMQL_ACCOUNT_STAMPING_ACTIVATE=false.
	RecordingOn, Held bool
	// Assigned: every kind whose account the engine records has finished its
	// first pass, and none has rows waiting on a parent.
	KindsTotal, KindsFinished int
	Waiting                   int64
	// PendingPasses counts the kinds with a requested full pass not finished:
	// such a pass carries the followers of a row that changed account
	// (component/memql/account_follow.go), so a file taken before it ends
	// could miss them.
	PendingPasses int
	assignedRead  bool
	// Declared: no loaded concept lacks @belongsTo.
	Undeclared   []string
	declaredRead bool
	// Decided: open conflicts whose candidates include the account.
	Conflicts int64
	// Followed: follow tasks not done that name the account.
	FollowTasks  int64
	followedRead bool
	// Unplaced is the quiet count of rows in no account (decision C below).
	Unplaced int64
}

func (r Readiness) recordingMet() bool { return r.RecordingOn && !r.Held }
func (r Readiness) assignedMet() bool {
	return r.assignedRead && r.KindsFinished == r.KindsTotal && r.Waiting == 0 && r.PendingPasses == 0
}
func (r Readiness) declaredMet() bool { return r.declaredRead && len(r.Undeclared) == 0 }
func (r Readiness) decidedMet() bool  { return r.Conflicts == 0 }
func (r Readiness) followedMet() bool { return r.followedRead && r.FollowTasks == 0 }

// Ready reports whether every condition holds.
func (r Readiness) Ready() bool { return len(r.unmet()) == 0 }

// unmet names the conditions that do not hold, in answer order.
func (r Readiness) unmet() []string {
	var out []string
	for _, c := range []struct {
		name string
		met  bool
	}{
		{conditionRecording, r.recordingMet()},
		{conditionAssigned, r.assignedMet()},
		{conditionDeclared, r.declaredMet()},
		{conditionDecided, r.decidedMet()},
		{conditionFollowed, r.followedMet()},
	} {
		if !c.met {
			out = append(out, c.name)
		}
	}
	return out
}

func (r Readiness) nodes() ([]memorynodes.MemoryNode, error) {
	undeclared := r.Undeclared
	if undeclared == nil {
		undeclared = []string{}
	}
	rows := []map[string]any{
		{"kind": "condition", "name": conditionRecording, "met": r.recordingMet(), "enabled": r.RecordingOn, "held": r.Held},
		{"kind": "condition", "name": conditionAssigned, "met": r.assignedMet(), "kindsTotal": r.KindsTotal, "kindsFinished": r.KindsFinished, "waiting": r.Waiting, "pendingPasses": r.PendingPasses},
		{"kind": "condition", "name": conditionDeclared, "met": r.declaredMet(), "concepts": undeclared},
		{"kind": "condition", "name": conditionDecided, "met": r.decidedMet(), "rows": r.Conflicts},
		{"kind": "condition", "name": conditionFollowed, "met": r.followedMet(), "tasks": r.FollowTasks},
		{"kind": "unplaced", "rows": r.Unplaced},
		{"kind": "ready", "ready": r.Ready()},
	}
	out := make([]memorynodes.MemoryNode, 0, len(rows))
	for _, p := range rows {
		suffix := "readiness:" + p["kind"].(string)
		if name, ok := p["name"].(string); ok {
			suffix += ":" + name
		}
		n, err := node(suffix, p)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// unplacedSQL reads the assignment job's own counters. `shared` is the rows
// the current (or last) pass left without an account on purpose -- left alone,
// and conflicts an owner left out for good (account_assignment_write.go counts
// outcomeShared, outcomeLeftAlone and outcomeLeftOut there) -- and `waiting` the
// rows held up behind a parent. They are exclusive outcomes of one judgment, so
// their sum counts no row twice. OPEN conflicts are left out: a row awaiting a
// decision is shown under `decided`, and counting it here too would count it
// twice on the same tab.
const unplacedSQL = `SELECT concept, shared, waiting FROM account_assignment_progress`

// Readiness reads the five conditions for the bare account.
func (s *Exports) Readiness(ctx context.Context, account string) (Readiness, error) {
	db, err := s.inv.database()
	if err != nil {
		return Readiness{}, err
	}
	// An account that does not exist is refused in words, never answered
	// ready: every other condition would hold for an id nothing names.
	if _, err := s.accountName(ctx, account); err != nil {
		return Readiness{}, err
	}
	r := Readiness{Held: s.inv.held}

	// The cluster's switch, read the way the assignment status reads it. It
	// turns on only once every live node has advertised that it reads the
	// field (account_stamping_readers), so "on" covers the readers too.
	if err := db.DB.QueryRowContext(ctx, switchSQL).Scan(&r.RecordingOn); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Readiness{}, fmt.Errorf("accountExportReadiness: read the switch: %w", err)
	}

	if s.assignmentStates != nil {
		states, err := s.assignmentStates(ctx, db.DB)
		if err != nil {
			return Readiness{}, fmt.Errorf("accountExportReadiness: read the assignment: %w", err)
		}
		r.assignedRead = true
		r.KindsTotal = len(states)
		for _, st := range states {
			if st.FirstPassFinished {
				r.KindsFinished++
			}
			r.Waiting += st.Waiting
			if st.FullPassPending {
				r.PendingPasses++
			}
		}
	}

	if s.undeclared != nil {
		r.Undeclared = append([]string(nil), s.undeclared(s.inv.conceptList())...)
		sort.Strings(r.Undeclared)
		r.declaredRead = true
	}

	if r.Conflicts, err = memql.ConflictsNaming(ctx, db.DB, account); err != nil {
		return Readiness{}, fmt.Errorf("accountExportReadiness: %w", err)
	}

	if s.followPending != nil {
		if r.FollowTasks, err = s.followPending(ctx, db.DB, account); err != nil {
			return Readiness{}, fmt.Errorf("accountExportReadiness: count follow tasks: %w", err)
		}
		r.followedRead = true
	}

	if r.Unplaced, err = s.unplaced(ctx, db.DB); err != nil {
		return Readiness{}, err
	}
	return r, nil
}

// unplaced counts the rows in no account: for every account-scoped kind not
// declared `shared` (where a row with no account is a statement, not a gap),
// the rows the job left without one (left alone, or left out for good) plus
// those still waiting. Open conflicts are counted under `decided` instead. It is AS OF THE
// JOB'S LAST PASS of each kind, from its counters, and walks no row at request
// time; a kind the job has not reached yet counts nothing until it does, which
// the `assigned` condition already reports.
func (s *Exports) unplaced(ctx context.Context, q memql.AccountQuerier) (int64, error) {
	counted := map[string]bool{}
	for _, c := range s.inv.conceptList() {
		if c.AccountScoped() && !c.SharedRows() {
			counted[c.Name] = true
		}
	}
	rows, err := q.QueryContext(ctx, unplacedSQL)
	if err != nil {
		return 0, fmt.Errorf("accountExportReadiness: read the assignment counters: %w", err)
	}
	defer rows.Close()
	var total int64
	for rows.Next() {
		var concept string
		var shared, waiting int64
		if err := rows.Scan(&concept, &shared, &waiting); err != nil {
			return 0, fmt.Errorf("accountExportReadiness: read the assignment counters: %w", err)
		}
		if counted[concept] {
			total += shared + waiting
		}
	}
	return total, rows.Err()
}
