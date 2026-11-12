package accounts

// export_run.go -- what an export run writes (design record
// docs/superpowers/specs/2026-11-12-account-export-design.md, sections B, D, E
// and F, "The run"). The runner (export_runner.go) claims an export and calls
// the ExportWork this file builds; everything the file holds is decided here.
//
// # The phases
//
//   - checking: the five conditions (Readiness), then "Caught up": note the
//     database's time T0, wait until every kind whose account the engine
//     records is verified through T0 with nothing waiting and no requested
//     full pass unfinished (asking the job for a full pass of each kind with
//     rows waiting), open the snapshot, and ask
//     the job's own judgment whether any row written since T0 is still one it
//     would assign. If so the snapshot is released and the wait repeats. Ten
//     minutes in all, then export_rows_waiting with the count per kind.
//   - rows: ONE `REPEATABLE READ, READ ONLY` transaction on a maintenance
//     session named AccountExportApplicationName before it begins, so the
//     assignment's late-commit horizon leaves this long transaction out
//     (component/memql/account_assignment.go). The census, the carried set,
//     the account's own row, every kind that travels in name order, the
//     schemas, the people, the references, the connections and the
//     hand-overs, all from that one instant. Sixty minutes at most, else
//     export_rows_too_slow, so a stalled upload cannot hold the snapshot open.
//   - bytes: after the transaction has ended. Each object or tree the newest
//     version of a carried row names, read only when the field's declaration
//     admits the key (export_bytes.go).
//   - sealing: the report, the README and the checksum list (Writer.Finish),
//     age closed when protected, the object committed.
//   - verifying: the committed object read back, opened with the passphrase
//     when protected, and accountexport.Verify run over it. A mismatch is
//     export_verify_failed and the object is deleted.
//
// # What is held in memory
//
// Nothing grows with the account but: a 64-bit hash per carried row (the
// carried set, so a reference is judged the moment its row is read), the ids
// of ONE kind at a time (the reader's, and a hand-over's request), the
// distinct schemas, and the list of byte references. Held for the whole run
// besides, each bounded by a single member the writer holds in memory anyway:
// every distinct target outside the file with its count per pointing field
// (references.jsonl), every secret NAME with its fields (connections.jsonl),
// and the people the rows name (people.jsonl). The cluster's people are read
// once, five fields each, so a version's writer is resolved while the walk
// streams; that grows with the cluster's people, not with the account. A
// kind's versions stream from the database through the writer's one part,
// gzip, tar, age and the blob stream's one block; one row's versions are
// never held, only its newest, which is the version the row's people,
// references and bytes are read from.
//
// # Waiting for the assignment
//
// "Caught up" asks the job about every engine-managed kind, not about this
// account's rows alone: the job's progress is kept per kind, and the rows
// written since T0 are judged before anyone knows which account they will
// get. So rows of OTHER accounts still being assigned can hold a run back. It
// never proceeds early; on a busy cluster it can wait until
// export_rows_waiting, and is started again.
//
// The order of ids is the database's. A row is complete when the id CHANGES in
// the stream; no two ids are ever compared in Go to decide an order.
//
// # A failed attempt leaves nothing
//
// Each attempt writes its own key, exports/accounts/<exportId>/<attempt>/<file>.
// Anything that fails or is cancelled before the commit ends the stream with
// Abort, which removes what it staged. A committed object that does not verify
// is deleted (Delete), which retires that attempt's key for good: the next
// attempt writes under its own key, and the retired one refuses a block from a
// replica that lost its claim. A file that cannot be read back, or that reads
// back wrong, is export_verify_failed. A panic anywhere in the run becomes an
// error carrying its type and message, so the same cleanup runs and the node
// goes on. The passphrase reaches Protect and Unprotect and nothing else: no
// log line, error or report carries it.
//
// # Who may read these rows
//
// Every statement here reads rows directly, behind the export's capability
// (execute on app:accounts/export), which is the wall (design section H and
// integrations/CLAUDE.md). Each one says why it applies no per-row gate.
//
// Tests: export_run_db_test.go (the fixture file, member by member),
// export_sweep_db_test.go (every account-scoped kind of the registry) and
// export_failure_db_test.go (each way a run stops).

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/accountexport"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/buildinfo"
	"github.com/znasllc-io/memql/integrations/azureblob"
)

// The run's limits (design "Decided without asking").
const (
	exportCaughtUpTimeout = 10 * time.Minute
	exportCaughtUpPoll    = 5 * time.Second
	exportRowsTimeout     = 60 * time.Minute
	// exportCleanupTimeout bounds the Abort or Delete that follows a failed
	// attempt, under a context of its own: the run's context may be the
	// cancelled one.
	exportCleanupTimeout = 2 * time.Minute
)

// The phases, as the record's `phase` column shows them.
const (
	phaseChecking  = "checking"
	phaseRows      = "rows"
	phaseBytes     = "bytes"
	phaseSealing   = "sealing"
	phaseVerifying = "verifying"
)

// rebuiltOnTarget is named once in every report (design section B, "What is
// left out").
var rebuiltOnTarget = []string{"vectors", "email statistics", "site health"}

// exportFiles is where a run writes its file and reads it back:
// azureblob.AccountExportStore, or an in-memory store in a test.
type exportFiles interface {
	Create(ctx context.Context, key string) (exportFileStream, error)
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
}

// exportFileStream is one file being written: azureblob.ExportStream.
type exportFileStream interface {
	Write(p []byte) (int, error)
	Commit(ctx context.Context) (size int64, sha256 string, err error)
	Abort(ctx context.Context) error
}

// accountBytesSource reads the bytes an account's rows point at:
// azureblob.AccountBytesReader, or an in-memory store in a test.
type accountBytesSource interface {
	Stat(ctx context.Context, ref string, under []string) (info azureblob.BlobInfo, ok, shipped bool, err error)
	List(ctx context.Context, treeRef string, under []string) ([]azureblob.BlobInfo, error)
	Open(ctx context.Context, key string, under []string) (io.ReadCloser, error)
	// Resolve is the reader's own parse of a stored reference, with no request.
	Resolve(ref string) (key string, tree, shipped bool, err error)
}

// azureExportFiles adapts azureblob.AccountExportStore, whose Create answers
// its concrete stream.
type azureExportFiles struct{ s *azureblob.AccountExportStore }

func (a azureExportFiles) Create(ctx context.Context, key string) (exportFileStream, error) {
	return a.s.Create(ctx, key)
}
func (a azureExportFiles) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	return a.s.Open(ctx, key)
}
func (a azureExportFiles) Delete(ctx context.Context, key string) error { return a.s.Delete(ctx, key) }

// exportWork is the run. Every outside question is a field, so a test answers
// it without an engine, and a node that cannot answer one fails the run
// rather than guessing.
type exportWork struct {
	exports  *Exports
	concepts func() []*memorynodes.Concept
	db       func() (*bun.DB, error)
	// openSession is PluginContext.OpenMaintenanceSession: no statement
	// timeout and no client read deadline beyond the run's context.
	openSession func(ctx context.Context) (*sql.Conn, func(), error)

	states          func(ctx context.Context, q memql.AccountQuerier) ([]memql.AccountAssignmentState, error)
	stillAssignable func(ctx context.Context, q memql.AccountQuerier, since time.Time, concepts []string) (map[string]int64, error)
	requestFullPass func(ctx context.Context, concepts []string) error
	census          func(ctx context.Context, q memql.AccountQuerier, account string, concepts []string) ([]memql.AccountCensusLine, error)
	handOvers       func() []string

	files func() (exportFiles, error)
	bytes func() (accountBytesSource, error)
	// recordObject is Exports.recordObject: the committed file's key, written
	// on the row by a statement guarded on this attempt.
	recordObject func(ctx context.Context, x Export, key string) (bool, error)

	engineVersion, domain, clusterID string

	caughtUpTimeout, caughtUpPoll, rowsTimeout time.Duration

	// inSnapshot is a test's look inside the open snapshot; nil in a node.
	inSnapshot func(ctx context.Context, q memql.AccountQuerier) error
	// wrapQuerier lets a test see every statement the snapshot runs; nil in
	// a node.
	wrapQuerier func(memql.AccountQuerier) memql.AccountQuerier
	// onRow is called when the walk completes a row; nil in a node.
	onRow func(concept string)
	// accountCensus is the account's own row counted by a statement of its
	// own; a field so a test can make it disagree.
	accountCensus func(ctx context.Context, q memql.AccountQuerier, account string) (rows, versions int64, err error)

	logger *slog.Logger
}

// newExportWork builds the run a node installs with SetExportWork. Every seam
// the plug-in context cannot fill stays nil, and the run fails on the first
// question it cannot ask.
func newExportWork(x *Exports, pctx memql.PluginContext) *exportWork {
	w := &exportWork{
		exports:         x,
		concepts:        x.inv.conceptList,
		db:              x.inv.database,
		openSession:     pctx.OpenMaintenanceSession,
		states:          x.assignmentStates,
		recordObject:    x.recordObject,
		census:          memql.AccountCensus,
		handOvers:       accountexport.HandOvers,
		engineVersion:   buildinfo.Version(),
		domain:          strings.TrimSpace(os.Getenv("MEMQL_DOMAIN")),
		clusterID:       strings.TrimSpace(os.Getenv("MEMQL_CLUSTER_ID")),
		caughtUpTimeout: exportCaughtUpTimeout,
		caughtUpPoll:    exportCaughtUpPoll,
		rowsTimeout:     exportRowsTimeout,
		logger:          x.logger,
	}
	if e, ok := pctx.Engine.(*memql.MemQLEngine); ok && e != nil {
		w.stillAssignable = e.AccountRowsStillAssignable
	}
	w.requestFullPass = func(ctx context.Context, concepts []string) error {
		db, err := w.db()
		if err != nil {
			return err
		}
		return memql.RequestAccountFullPass(ctx, db, concepts)
	}
	uploader := lazyUploader()
	w.files = func() (exportFiles, error) {
		u, err := uploader()
		if err != nil {
			return nil, err
		}
		return azureExportFiles{u.AccountExports()}, nil
	}
	w.bytes = func() (accountBytesSource, error) {
		u, err := uploader()
		if err != nil {
			return nil, err
		}
		return u.AccountBytes(), nil
	}
	return w
}

// lazyUploader builds the storage client on first use, as lazyBlobs does for
// the export half, and does not keep a failure: one transient error must not
// leave the node unable to run an export until it restarts.
func lazyUploader() func() (*azureblob.AzureBlobUploader, error) {
	var mu sync.Mutex
	var u *azureblob.AzureBlobUploader
	return func() (*azureblob.AzureBlobUploader, error) {
		mu.Lock()
		defer mu.Unlock()
		if u != nil {
			return u, nil
		}
		built, err := azureblob.New(context.Background())
		if err != nil {
			return nil, err
		}
		u = built
		return u, nil
	}
}

// Work is the ExportWork this run installs.
func (w *exportWork) Work() ExportWork { return w.run }

// failure is an ExportFailure, which the runner records with its code.
func failure(code, format string, a ...any) error {
	return &ExportFailure{Code: code, Detail: fmt.Sprintf(format, a...)}
}

// attempt is one run of one export: its writer, its stream and its counts.
type attempt struct {
	w          *exportWork
	x          Export
	account    string // bare
	primary    bool
	passphrase string
	progress   func(ExportProgress)
	prog       ExportProgress

	concepts  []*memorynodes.Concept
	byName    map[string]*memorynodes.Concept
	travel    []string // the kinds that travel, sorted
	scoped    []string // every account-scoped kind, sorted
	instant   time.Time
	accountNm string

	key, fileName, stem string
	stream              exportFileStream
	age                 io.WriteCloser
	out                 *accountexport.Writer
	committed           bool

	snap   *snapshot
	report accountexport.Report
}

func (w *exportWork) run(ctx context.Context, x Export, passphrase string, progress func(ExportProgress)) (res ExportResult, err error) {
	if progress == nil {
		progress = func(ExportProgress) {}
	}
	if x.Protected && passphrase == "" {
		return ExportResult{}, errors.New("a protected export reached the run with no passphrase")
	}
	a := &attempt{w: w, x: x, passphrase: passphrase, progress: progress}
	if !x.Protected {
		a.passphrase = ""
	}
	if a.account, err = memql.BareAccountID(x.AccountID); err != nil {
		return ExportResult{}, err
	}
	a.primary = a.account == primaryAccount
	a.accountNm = x.AccountName
	defer func() {
		if err != nil {
			a.cleanUp()
		}
	}()
	// Registered after the cleanup, so it runs first: a panic becomes an
	// error the cleanup then acts on, and the node goes on. The error keeps
	// the panic's type and message and no stack, which could name values.
	defer func() {
		if p := recover(); p != nil {
			res, err = ExportResult{}, fmt.Errorf("account export: the run panicked: %T: %v", p, p)
		}
	}()
	return a.run(ctx)
}

func (a *attempt) setPhase(p string) {
	a.prog.Phase = p
	a.progress(a.prog)
}

func (a *attempt) run(ctx context.Context) (ExportResult, error) {
	a.setPhase(phaseChecking)
	if err := a.loadConcepts(); err != nil {
		return ExportResult{}, err
	}
	if err := a.checkReady(ctx); err != nil {
		return ExportResult{}, err
	}
	conn, closeConn, err := a.openSession(ctx)
	if err != nil {
		return ExportResult{}, err
	}
	defer closeConn()
	if err := a.caughtUp(ctx, conn); err != nil {
		return ExportResult{}, err
	}
	// From here the snapshot is open; rows ends it either way.
	a.setPhase(phaseRows)
	if err := a.rows(ctx, conn); err != nil {
		return ExportResult{}, err
	}
	closeConn()
	if err := ctx.Err(); err != nil {
		return ExportResult{}, err
	}
	a.setPhase(phaseBytes)
	if err := a.copyBytes(ctx); err != nil {
		return ExportResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return ExportResult{}, err
	}
	a.setPhase(phaseSealing)
	size, sum, err := a.seal(ctx)
	if err != nil {
		return ExportResult{}, err
	}
	a.setPhase(phaseVerifying)
	if err := a.verify(ctx); err != nil {
		return ExportResult{}, err
	}
	report, err := json.Marshal(a.report)
	if err != nil {
		return ExportResult{}, err
	}
	return ExportResult{ObjectKey: a.key, FileName: a.fileName, SHA256: sum, SizeBytes: size, Report: report}, nil
}

func (a *attempt) loadConcepts() error {
	if a.w.concepts == nil {
		return errors.New("the concept registry is not wired on this node")
	}
	a.concepts = a.w.concepts()
	a.byName = map[string]*memorynodes.Concept{}
	for _, c := range a.concepts {
		if c != nil {
			if _, dup := a.byName[c.Name]; !dup {
				a.byName[c.Name] = c
			}
		}
	}
	a.travel = memql.AccountKinds(a.concepts, true)
	a.scoped = memql.AccountKinds(a.concepts, false)
	return nil
}

// checkReady asks the five conditions again. Rows waiting on a parent are the
// one unmet answer the run itself waits out ("Caught up"); every other is a
// refusal that names what is not met.
func (a *attempt) checkReady(ctx context.Context) error {
	if a.w.exports == nil {
		return errors.New("the export's readiness is not wired on this node")
	}
	r, err := a.w.exports.Readiness(ctx, a.account)
	if err != nil {
		return err
	}
	var unmet []string
	for _, name := range r.unmet() {
		// Once every kind has finished its first pass, only rows waiting
		// and requested passes not yet finished keep `assigned` unmet; the
		// caught-up wait below waits both out, asking for the full passes
		// that release waiting rows.
		if name == conditionAssigned && r.assignedRead && r.KindsFinished == r.KindsTotal {
			continue
		}
		unmet = append(unmet, name)
	}
	if len(unmet) > 0 {
		return failure(FailureNotReady, "%s", notReadyDetail(unmet))
	}
	return nil
}

func (a *attempt) openSession(ctx context.Context) (*sql.Conn, func(), error) {
	if a.w.openSession == nil {
		return nil, nil, errors.New("no maintenance session can be opened on this node")
	}
	conn, closeFn, err := a.w.openSession(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("open the export's session: %w", err)
	}
	closed := false
	closeOnce := func() {
		if !closed {
			closed = true
			closeFn()
		}
	}
	// The name is set BEFORE any transaction begins: the assignment reads it
	// from pg_stat_activity to leave this session out of its horizon, and the
	// session writes nothing while it carries it.
	if _, err := conn.ExecContext(ctx, `SELECT set_config('application_name', $1, false)`, memql.AccountExportApplicationName); err != nil {
		closeOnce()
		return nil, nil, fmt.Errorf("name the export's session: %w", err)
	}
	return conn, closeOnce, nil
}

// engineManaged is the kinds whose account the engine records: the only ones a
// row can be waiting on the job for.
func (a *attempt) engineManaged() []string {
	var out []string
	for _, name := range a.scoped {
		if a.byName[name].AccountEngineManaged() {
			out = append(out, name)
		}
	}
	return out
}

// caughtUp is design section F, "Caught up". It leaves the snapshot OPEN on
// conn when it returns nil.
func (a *attempt) caughtUp(ctx context.Context, conn *sql.Conn) error {
	if a.w.states == nil || a.w.stillAssignable == nil || a.w.requestFullPass == nil {
		return errors.New("the assignment job's answers are not wired on this node")
	}
	var t0 time.Time
	// The database's clock, which verified_through is written in.
	if err := conn.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&t0); err != nil {
		return fmt.Errorf("read the database's time: %w", err)
	}
	deadline := time.Now().Add(a.w.caughtUpTimeout)
	managed := a.engineManaged()
	requested := map[string]bool{}
	var lastWaiting map[string]int64
	var lastBehind map[string]string
	for {
		waiting, behind, err := a.jobStanding(ctx, conn, t0)
		if err != nil {
			return err
		}
		var ask []string
		for concept := range waiting {
			if !requested[concept] {
				requested[concept] = true
				ask = append(ask, concept)
			}
		}
		if len(ask) > 0 {
			sort.Strings(ask)
			if err := a.w.requestFullPass(ctx, ask); err != nil {
				return err
			}
		}
		if len(waiting) == 0 && len(behind) == 0 {
			if err := a.beginSnapshot(ctx, conn); err != nil {
				return err
			}
			still, err := a.w.stillAssignable(ctx, conn, t0, managed)
			if err != nil {
				_ = a.endSnapshot(conn)
				return err
			}
			if len(still) == 0 {
				// Two conditions can change during the wait, and the full
				// passes this run asked for are what change the first: a pass
				// records a conflict for a row whose candidates are ambiguous,
				// and a move writes a change record. Both are read again here,
				// on the snapshot's own connection, so the file is taken from
				// an instant at which both still hold.
				if err := a.stillReady(ctx, conn); err != nil {
					_ = a.endSnapshot(conn)
					return err
				}
				return nil
			}
			// A row written since T0 is one the job would still assign:
			// release the snapshot and wait for the job again.
			if err := a.endSnapshot(conn); err != nil {
				return err
			}
			waiting = still
		}
		lastWaiting, lastBehind = waiting, behind
		if time.Now().After(deadline) {
			return failure(FailureRowsWaiting, "Rows are still waiting for their account after %s: %s.",
				a.w.caughtUpTimeout, describeCounts(lastWaiting, lastBehind))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(a.w.caughtUpPoll):
		}
	}
}

// stillReady asks, inside the open snapshot, the two conditions the wait can
// change: no open conflict names the account (decided), and no change record
// naming it is unsettled (followed). Either unmet fails the run export_not_ready
// in the words the Export tab uses; an unwired count fails closed.
func (a *attempt) stillReady(ctx context.Context, q memql.AccountQuerier) error {
	if a.w.exports == nil || a.w.exports.followPending == nil {
		return errors.New("the export's readiness is not wired on this node")
	}
	conflicts, err := memql.ConflictsNaming(ctx, q, a.account)
	if err != nil {
		return err
	}
	pending, err := a.w.exports.followPending(ctx, q, a.account)
	if err != nil {
		return fmt.Errorf("count the change records naming the account: %w", err)
	}
	var unmet []string
	if conflicts > 0 {
		unmet = append(unmet, conditionDecided)
	}
	if pending > 0 {
		unmet = append(unmet, conditionFollowed)
	}
	if len(unmet) > 0 {
		return failure(FailureNotReady, "%s", notReadyDetail(unmet))
	}
	return nil
}

// Why a kind holds the run back, other than rows waiting on a parent.
const (
	behindNotChecked  = "not yet checked"
	behindPassPending = "full pass pending"
)

// jobStanding answers, per engine-managed kind, the rows waiting on a parent
// and the kinds the run must wait for besides: not yet verified through t0,
// or with a requested full pass not finished. Such a pass is what carries the
// followers of a row that changed account (component/memql/account_follow.go),
// so the run does not start a snapshot before it ends.
func (a *attempt) jobStanding(ctx context.Context, q memql.AccountQuerier, t0 time.Time) (waiting map[string]int64, behind map[string]string, err error) {
	states, err := a.w.states(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	waiting, behind = map[string]int64{}, map[string]string{}
	for _, st := range states {
		if st.Waiting > 0 {
			waiting[st.Concept] = st.Waiting
		}
		switch {
		case st.FullPassPending:
			behind[st.Concept] = behindPassPending
		case !st.FirstPassFinished || st.VerifiedThrough.Before(t0):
			behind[st.Concept] = behindNotChecked
		}
	}
	return waiting, behind, nil
}

func describeCounts(waiting map[string]int64, behind map[string]string) string {
	seen := map[string]bool{}
	var names []string
	for k := range waiting {
		seen[k] = true
		names = append(names, k)
	}
	for k := range behind {
		if !seen[k] {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, k := range names {
		var why []string
		if waiting[k] > 0 {
			why = append(why, fmt.Sprintf("%d", waiting[k]))
		}
		if reason := behind[k]; reason != "" {
			why = append(why, reason)
		}
		parts = append(parts, k+" "+strings.Join(why, ", "))
	}
	return strings.Join(parts, "; ")
}

// beginSnapshot opens the one transaction every line comes from. The limits
// are the run's, set for this transaction only: the maintenance session has
// no statement timeout, but it keeps the pools' idle-in-transaction limit,
// which a statement-free stretch of the walk (a part cut and uploaded between
// two kinds) could pass.
func (a *attempt) beginSnapshot(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, `BEGIN ISOLATION LEVEL REPEATABLE READ READ ONLY`); err != nil {
		return fmt.Errorf("open the snapshot: %w", err)
	}
	ms := a.w.rowsTimeout.Milliseconds()
	for _, setting := range []string{"statement_timeout", "idle_in_transaction_session_timeout"} {
		if _, err := conn.ExecContext(ctx, `SELECT set_config($1, $2, true)`, setting, fmt.Sprintf("%d", ms)); err != nil {
			_ = a.endSnapshot(conn)
			return fmt.Errorf("limit the snapshot: %w", err)
		}
	}
	return nil
}

// endSnapshot ends the read-only transaction. It wrote nothing, so a rollback
// and a commit are the same; a rollback is what an error path can always send.
func (a *attempt) endSnapshot(conn *sql.Conn) error {
	ctx, cancel := context.WithTimeout(context.Background(), exportCleanupTimeout)
	defer cancel()
	_, err := conn.ExecContext(ctx, `ROLLBACK`)
	return err
}

// rows is the rows phase: everything inside the snapshot, ended before it
// returns.
func (a *attempt) rows(ctx context.Context, conn *sql.Conn) (err error) {
	started := time.Now()
	rowsCtx, cancel := context.WithTimeout(ctx, a.w.rowsTimeout)
	defer cancel()
	ended := false
	defer func() {
		if !ended {
			_ = a.endSnapshot(conn)
		}
		// The limit is held twice, by the context and by the server's
		// statement and idle limits set on the snapshot; whichever ends the
		// phase first, a phase that ran its limit out was too slow.
		overran := errors.Is(rowsCtx.Err(), context.DeadlineExceeded) || time.Since(started) >= a.w.rowsTimeout
		if err != nil && ctx.Err() == nil && overran {
			err = failure(FailureRowsTooSlow, "Reading the rows took longer than %s.", a.w.rowsTimeout)
		}
	}()
	if a.w.inSnapshot != nil {
		if err := a.w.inSnapshot(rowsCtx, conn); err != nil {
			return err
		}
	}
	if err := conn.QueryRowContext(rowsCtx, `SELECT now()`).Scan(&a.instant); err != nil {
		return fmt.Errorf("read the snapshot's instant: %w", err)
	}
	a.instant = a.instant.UTC()
	var q memql.AccountQuerier = conn
	if a.w.wrapQuerier != nil {
		q = a.w.wrapQuerier(q)
	}
	s, err := openSnapshot(rowsCtx, a, q)
	if err != nil {
		return err
	}
	a.snap = s
	if err := a.openFile(rowsCtx); err != nil {
		return err
	}
	if err := s.write(rowsCtx); err != nil {
		return err
	}
	if err := rowsCtx.Err(); err != nil {
		return err
	}
	ended = true
	if err := a.endSnapshot(conn); err != nil {
		return fmt.Errorf("end the snapshot: %w", err)
	}
	return nil
}

// stemFor is the folder's name: the account's id where it can name a path,
// and the snapshot's day.
func stemFor(account string, at time.Time) string {
	var b strings.Builder
	for _, c := range account {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			b.WriteRune(c)
		default:
			b.WriteRune('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		name = "account"
	}
	return name + "-" + at.Format("2006-01-02") + accountexport.FolderSuffix
}

// openFile creates the attempt's object and the writer over it. The manifest
// is written at once, so it needs the snapshot's kinds and instant.
func (a *attempt) openFile(ctx context.Context) error {
	if a.w.files == nil {
		return errors.New("export storage is not wired on this node")
	}
	files, err := a.w.files()
	if err != nil {
		return fmt.Errorf("export storage: %w", err)
	}
	a.stem = stemFor(a.account, a.instant)
	a.fileName = a.stem + ".tar"
	if a.x.Protected {
		a.fileName += ".age"
	}
	a.key = fmt.Sprintf("exports/accounts/%s/%d/%s", a.x.ID, a.x.Attempt, a.fileName)
	stream, err := files.Create(ctx, a.key)
	if err != nil {
		return fmt.Errorf("create the export's file: %w", err)
	}
	a.stream = stream
	var dst io.Writer = stream
	if a.x.Protected {
		a.age, err = accountexport.Protect(stream, a.passphrase)
		if err != nil {
			return err
		}
		dst = a.age
	}
	a.out, err = accountexport.NewWriter(dst, a.stem, a.manifest())
	return err
}

func (a *attempt) manifest() accountexport.Manifest {
	kinds := []accountexport.KindDecl{{Concept: accountConcept, BelongsTo: "cluster"}}
	for _, name := range a.travel {
		c := a.byName[name]
		kinds = append(kinds, accountexport.KindDecl{
			Concept: name, BelongsTo: renderBelongsTo(c), Origin: c.Origin,
			MirroredTo: append([]string(nil), c.MirroredTo...),
		})
	}
	return accountexport.Manifest{
		EngineVersion: a.w.engineVersion,
		ExportID:      a.x.ID,
		CreatedAt:     a.instant,
		Source:        accountexport.Source{Domain: a.w.domain, ClusterID: a.w.clusterID},
		Account:       accountexport.Account{ID: a.account, Name: a.accountNm, Primary: a.primary},
		Protected:     a.x.Protected,
		Kinds:         kinds,
	}
}

// renderBelongsTo writes a kind's @belongsTo the way it is declared, so an
// import reads the form and not this engine's struct.
func renderBelongsTo(c *memorynodes.Concept) string {
	if c == nil || c.BelongsTo == nil {
		return ""
	}
	d := c.BelongsTo
	parts := []string{string(d.Scope)}
	if d.Via != "" {
		parts = append(parts, fmt.Sprintf("via=%q", d.Via))
	}
	if d.Parent != "" {
		parts = append(parts, fmt.Sprintf("parent=%q", d.Parent))
	}
	if d.Shared {
		parts = append(parts, "shared")
	}
	if d.Stated {
		parts = append(parts, "stated")
	}
	if d.Stays != "" {
		parts = append(parts, fmt.Sprintf("stays=%q", d.Stays))
	}
	return strings.Join(parts, ", ")
}

// seal writes the report, the README and the checksum list and commits.
func (a *attempt) seal(ctx context.Context) (int64, string, error) {
	// The writer writes an empty list as [], so the report the record keeps
	// is spelled the same as the one in the file.
	r := &a.report
	if r.Kinds == nil {
		r.Kinds = []accountexport.KindCount{}
	}
	if r.LeftOut == nil {
		r.LeftOut = []accountexport.LeftOut{}
	}
	if r.InNoAccount == nil {
		r.InNoAccount = []accountexport.KindRows{}
	}
	if r.ScrubbedFields == nil {
		r.ScrubbedFields = []accountexport.ScrubbedField{}
	}
	if r.Rebuilt == nil {
		r.Rebuilt = []string{}
	}
	if r.Members == nil {
		r.Members = []accountexport.MemberCount{}
	}
	if err := a.out.Finish(a.report); err != nil {
		return 0, "", err
	}
	if a.age != nil {
		if err := a.age.Close(); err != nil {
			return 0, "", fmt.Errorf("protect: %w", err)
		}
	}
	// A cancel that arrived while the file was being finished commits
	// nothing; the stream is aborted by the cleanup.
	if err := ctx.Err(); err != nil {
		return 0, "", err
	}
	size, sum, err := a.stream.Commit(ctx)
	if err != nil {
		return 0, "", fmt.Errorf("commit the export's file: %w", err)
	}
	a.committed = true
	// The key goes on the record before anything else is done with the file.
	// Reading it back takes as long as writing it did, and a node that dies in
	// that window runs no cleanup: the recorded key is what lets the take-over
	// delete the file (export_store.go, reclaimStale). A statement that
	// matches nothing means this attempt no longer holds the run, so the file
	// is deleted by the cleanup and nothing more is done.
	if a.w.recordObject == nil {
		return 0, "", errors.New("the export's record cannot be written on this node")
	}
	recorded, err := a.w.recordObject(ctx, a.x, a.key)
	if err != nil {
		return 0, "", fmt.Errorf("record the export's file: %w", err)
	}
	if !recorded {
		return 0, "", errExportLeaseLost
	}
	return size, sum, nil
}

// verify reads the committed object back and checks it (design section E,
// point 7). Its counts must also be the report this run wrote.
func (a *attempt) verify(ctx context.Context) error {
	files, err := a.w.files()
	if err != nil {
		return failure(FailureVerifyFailed, "The file could not be read back: %v", err)
	}
	rc, err := files.Open(ctx, a.key)
	if err != nil {
		// A file that cannot be read back is no more known to be complete
		// than one that reads back wrong.
		return failure(FailureVerifyFailed, "The file could not be read back: %v", err)
	}
	defer rc.Close()
	var r io.Reader = rc
	if a.x.Protected {
		if r, err = accountexport.Unprotect(rc, a.passphrase); err != nil {
			return failure(FailureVerifyFailed, "The file read back does not open: %v", err)
		}
	}
	res, err := accountexport.Verify(r)
	if err != nil {
		return failure(FailureVerifyFailed, "The file read back does not check: %v", err)
	}
	want, _ := json.Marshal(a.report)
	got, _ := json.Marshal(res.Report)
	if string(want) != string(got) {
		return failure(FailureVerifyFailed, "The file read back holds a different report than the run wrote.")
	}
	return nil
}

// cleanUp leaves nothing of a failed or cancelled attempt in storage.
func (a *attempt) cleanUp() {
	if a.stream == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), exportCleanupTimeout)
	defer cancel()
	log := a.w.logger
	if log == nil {
		log = slog.Default()
	}
	if !a.committed {
		if err := a.stream.Abort(ctx); err != nil {
			log.Error("account export: the attempt's staged file could not be removed", "exportId", a.x.ID, "attempt", a.x.Attempt, "error", err)
		}
		return
	}
	files, err := a.w.files()
	if err == nil {
		err = files.Delete(ctx, a.key)
	}
	if err != nil {
		log.Error("account export: the attempt's file could not be removed", "exportId", a.x.ID, "attempt", a.x.Attempt, "error", err)
	}
}

// ---------------------------------------------------------------------------
// Inside the snapshot

// snapshot is everything the rows phase reads, from one instant.
type snapshot struct {
	a      *attempt
	q      memql.AccountQuerier
	census map[string]memql.AccountCensusLine
	// carried is the 64-bit hash of every carried row's (concept, short id):
	// what a reference is judged against the moment its row is read. A
	// collision can only make a reference to a row outside the file look
	// carried, which leaves one line out of references.jsonl; it never adds
	// or drops a row.
	carried map[uint64]struct{}
	// accountRows and accountVersions are the account's own row counted by a
	// statement of their own, held to what account.jsonl holds.
	accountRows, accountVersions int64
	people                       *peopleBook
	refs                         *referenceBook
	bytes                        *bytesBook
	schemas                      map[string]bool
	scrubbed                     map[accountexport.ScrubbedField]bool
	// schemaDocs are the shared schema documents the carried kinds' versions
	// were written under, read once before the walk; versionRules holds each
	// one's secret paths, and each distinct inline schema's, by fingerprint or
	// by the schema's own text.
	schemaDocs   map[string]json.RawMessage
	versionRules map[string]*secretRules
}

func openSnapshot(ctx context.Context, a *attempt, q memql.AccountQuerier) (*snapshot, error) {
	s := &snapshot{
		a: a, q: q, census: map[string]memql.AccountCensusLine{}, carried: map[uint64]struct{}{},
		schemas: map[string]bool{}, scrubbed: map[accountexport.ScrubbedField]bool{},
		schemaDocs: map[string]json.RawMessage{}, versionRules: map[string]*secretRules{},
	}
	// 1. The census, of every account-scoped kind, travelling or not: the
	// kinds that stay need their count for connections.jsonl, and every kind
	// its rows in no account for the report.
	if a.w.census == nil {
		return nil, errors.New("the account census is not wired on this node")
	}
	lines, err := a.w.census(ctx, q, a.account, a.scoped)
	if err != nil {
		return nil, err
	}
	for _, l := range lines {
		s.census[l.Concept] = l
	}
	count := a.w.accountCensus
	if count == nil {
		count = countAccountRow
	}
	if s.accountRows, s.accountVersions, err = count(ctx, q, a.account); err != nil {
		return nil, err
	}
	// The manifest, written first, names the account as the snapshot holds it.
	err = memql.AccountRowVersions(ctx, q, accountConcept, []string{canonicalPrefix + a.account, a.account},
		func(n memorynodes.MemoryNode, _ bool) error {
			if name := accountName(n.Payload); name != "" {
				a.accountNm = name
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	// 2. The carried set, before a row is written, one kind's ids at a time.
	s.carry(accountConcept, canonicalPrefix+a.account)
	for _, kind := range a.travel {
		if s.census[kind].Rows == 0 {
			continue
		}
		ids, err := memql.AccountKindRowIDs(ctx, q, a.account, kind)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			s.carry(kind, id)
		}
	}
	if err := s.loadVersionSchemas(ctx); err != nil {
		return nil, err
	}
	if s.people, err = loadPeople(ctx, q, a.account, a.primary, a.byName); err != nil {
		return nil, err
	}
	s.refs = newReferenceBook(s)
	s.bytes = newBytesBook()
	// The reader is asked only to parse during the walk; storage is read
	// after the snapshot ends.
	if a.w.bytes != nil {
		if src, err := a.w.bytes(); err == nil {
			s.bytes.resolve = src.Resolve
		} else {
			s.bytes.resolve = func(string) (string, bool, bool, error) { return "", false, false, err }
		}
	}
	return s, nil
}

// accountRowCensusSQL counts the account's own row, either spelling of its
// id, in both stores: the second count of account.jsonl.
//
// staged-data: MUST-NOT-GATE -- the count account.jsonl is proven against; the account is the account whether or not its kind's data is staged.
const accountRowCensusSQL = `SELECT count(DISTINCT id), count(*) FROM (
    SELECT id FROM "MemoryNodes" WHERE concept = $1 AND id = ANY($2::text[])
    UNION ALL
    SELECT id FROM memory_liveness WHERE concept = $1 AND id = ANY($2::text[])
  ) v`

func countAccountRow(ctx context.Context, q memql.AccountQuerier, account string) (int64, int64, error) {
	rows, err := q.QueryContext(ctx, accountRowCensusSQL, accountConcept, pq.Array([]string{canonicalPrefix + account, account}))
	if err != nil {
		return 0, 0, fmt.Errorf("count the account's row: %w", err)
	}
	defer rows.Close()
	var n, v int64
	if rows.Next() {
		if err := rows.Scan(&n, &v); err != nil {
			return 0, 0, err
		}
	}
	return n, v, rows.Err()
}

func (s *snapshot) carry(concept, id string) { s.carried[rowHash(concept, id)] = struct{}{} }

func (s *snapshot) isCarried(concept, id string) bool {
	_, ok := s.carried[rowHash(concept, id)]
	return ok
}

// rowHash is FNV-1a over the concept and the row's short id, so both
// spellings of one id hash alike.
func rowHash(concept, id string) uint64 {
	const offset, prime = 14695981039346656037, 1099511628211
	h := uint64(offset)
	for _, part := range []string{concept, "\x00", memql.BareShortId(id)} {
		for i := 0; i < len(part); i++ {
			h ^= uint64(part[i])
			h *= prime
		}
	}
	return h
}

// kindWalk is one kind's stream: the row in progress, its newest version so
// far, and what was written.
type kindWalk struct {
	concept        *memorynodes.Concept
	id             string
	newest         memorynodes.MemoryNode
	have           bool
	older          map[string][]string // this row's older bytes references
	rows, versions int64
}

// version handles one version of the stream: the row before it is complete
// when the id changes, which is how the order of ids is left to the database.
//
// Between two rows the run's context is asked: a cancel stops the walk at the
// next row rather than at the end of the kind's statement.
func (s *snapshot) version(ctx context.Context, k *kindWalk, n memorynodes.MemoryNode, observed bool) (accountexport.Row, error) {
	if !k.have || n.ID != k.id {
		if k.have {
			if err := s.rowDone(k); err != nil {
				return accountexport.Row{}, err
			}
		}
		if err := ctx.Err(); err != nil {
			return accountexport.Row{}, err
		}
		k.id, k.have, k.older = n.ID, true, nil
		k.rows++
	} else if s.bytes.tracks(k.concept) {
		// The version before this one was not the newest after all.
		for ref, under := range s.bytes.references(k.concept, k.newest) {
			if k.older == nil {
				k.older = map[string][]string{}
			}
			k.older[ref] = under
		}
	}
	k.newest = n
	k.versions++
	return s.line(k.concept, n, observed)
}

// rowDone reads what the newest version of a complete row names.
func (s *snapshot) rowDone(k *kindWalk) error {
	payload, err := payloadMap(k.newest.Payload)
	if err != nil {
		return fmt.Errorf("%s %s: %w", k.concept.Name, k.newest.ID, err)
	}
	s.people.fromRow(k.concept, payload)
	s.refs.fromRow(k.concept, k.newest.ID, payload)
	s.bytes.fromNewest(k.concept, k.newest.ID, payload, k.older)
	if s.bytes.err != nil {
		return s.bytes.err
	}
	if s.a.w.onRow != nil {
		s.a.w.onRow(k.concept.Name)
	}
	return nil
}

// payloadMap decodes a payload; an empty one is an empty object.
func payloadMap(raw json.RawMessage) (map[string]any, error) {
	out := map[string]any{}
	if len(raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("the payload is not an object: %w", err)
	}
	return out, nil
}

// line is one carried version as it goes in the file: @secret fields removed,
// and an operator's request context removed (export_people.go).
func (s *snapshot) line(c *memorynodes.Concept, n memorynodes.MemoryNode, observed bool) (accountexport.Row, error) {
	s.people.noteWriter(n)
	payload, err := s.removeSecrets(c, n)
	if err != nil {
		return accountexport.Row{}, err
	}
	metadata, createdBy, err := s.people.scrub(n)
	if err != nil {
		return accountexport.Row{}, fmt.Errorf("%s %s: %w", c.Name, n.ID, err)
	}
	if fp := schemaFingerprint(n.Schema); fp != "" {
		s.schemas[fp] = true
	}
	return accountexport.Row{
		Concept: c.Name, ID: n.ID, CreatedAt: n.CreatedAt, CreatedBy: createdBy, Type: n.Type,
		Schema: n.Schema, Payload: payload, Metadata: metadata, Provenance: n.Provenance, Observed: observed,
	}, nil
}

// schemaReferencePrefix is how a row names a shared schema document
// (memory-nodes shared_schema.go); an inline schema names none.
const schemaReferencePrefix = "memql-schema:"

func schemaFingerprint(schema json.RawMessage) string {
	var ref struct {
		Ref string `json:"$ref"`
	}
	if len(schema) == 0 || json.Unmarshal(schema, &ref) != nil {
		return ""
	}
	if fp, ok := strings.CutPrefix(ref.Ref, schemaReferencePrefix); ok {
		return fp
	}
	return ""
}

// write is the rows phase's file, member by member in the layout's order.
func (s *snapshot) write(ctx context.Context) error {
	a := s.a
	if err := s.writeAccount(ctx); err != nil {
		return err
	}
	a.prog.KindsTotal = len(a.travel)
	a.progress(a.prog)
	for _, kind := range a.travel {
		if err := s.writeKind(ctx, kind); err != nil {
			return err
		}
		a.prog.KindsDone++
		a.progress(a.prog)
	}
	if err := s.checkCensus(); err != nil {
		return err
	}
	if err := s.writeSchemas(ctx); err != nil {
		return err
	}
	people := s.people.lines()
	for _, p := range people {
		if err := a.out.Line(accountexport.MemberPeople, p); err != nil {
			return err
		}
	}
	refs, err := s.refs.resolve(ctx)
	if err != nil {
		return err
	}
	for _, r := range refs {
		if err := a.out.Line(accountexport.MemberReferences, r); err != nil {
			return err
		}
	}
	conns := s.refs.connections(s.census)
	for _, c := range conns {
		if err := a.out.Line(accountexport.MemberConnections, c); err != nil {
			return err
		}
	}
	a.report.Members = append(a.report.Members,
		accountexport.MemberCount{Name: accountexport.MemberPeople, Lines: int64(len(people))},
		accountexport.MemberCount{Name: accountexport.MemberReferences, Lines: int64(len(refs))},
		accountexport.MemberCount{Name: accountexport.MemberConnections, Lines: int64(len(conns))},
	)
	s.leftOut()
	return s.handOver(ctx)
}

// leftOut is the report's account of what the file does not hold: the kinds
// that stay, the rows in no account, the secret fields removed.
//
// A `shared` kind's rows with no account are the cluster's, provided to every
// account, not rows that belong to nobody, so they are not counted as in no
// account (the readiness count leaves them out for the same reason).
func (s *snapshot) leftOut() {
	r := &s.a.report
	for _, kind := range s.a.scoped {
		c := s.a.byName[kind]
		if reason := c.Stays(); reason != "" {
			r.LeftOut = append(r.LeftOut, accountexport.LeftOut{Concept: kind, Reason: reason, Rows: s.census[kind].Rows})
		}
		if n := s.census[kind].InNoAccount; n > 0 && !c.SharedRows() {
			r.InNoAccount = append(r.InNoAccount, accountexport.KindRows{Concept: kind, Rows: n})
		}
	}
	for f := range s.scrubbed {
		r.ScrubbedFields = append(r.ScrubbedFields, f)
	}
	sort.Slice(r.ScrubbedFields, func(i, j int) bool {
		if r.ScrubbedFields[i].Concept != r.ScrubbedFields[j].Concept {
			return r.ScrubbedFields[i].Concept < r.ScrubbedFields[j].Concept
		}
		return r.ScrubbedFields[i].Field < r.ScrubbedFields[j].Field
	})
	r.Rebuilt = append([]string(nil), rebuiltOnTarget...)
}

// writeAccount writes account.jsonl: every version of the account's own row,
// either spelling of its id.
func (s *snapshot) writeAccount(ctx context.Context) error {
	a := s.a
	c := a.byName[accountConcept]
	if c == nil {
		c = &memorynodes.Concept{Name: accountConcept}
	}
	k := &kindWalk{concept: c}
	var lineErr error
	err := memql.AccountRowVersions(ctx, s.q, accountConcept, []string{canonicalPrefix + a.account, a.account},
		func(n memorynodes.MemoryNode, observed bool) error {
			row, err := s.version(ctx, k, n, observed)
			if err != nil {
				return err
			}
			if err := a.out.AccountRow(row); err != nil {
				lineErr = err
				return err
			}
			return nil
		})
	if err != nil {
		if lineErr != nil {
			return lineErr
		}
		return err
	}
	if k.have {
		if err := s.rowDone(k); err != nil {
			return err
		}
	}
	a.report.Kinds = append(a.report.Kinds, accountexport.KindCount{
		Concept: accountConcept, ExpectedRows: s.accountRows, ExpectedVersions: s.accountVersions, WrittenRows: k.rows, WrittenVersions: k.versions,
	})
	return nil
}

func accountName(payload json.RawMessage) string {
	var p struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(payload, &p)
	return strings.TrimSpace(p.Name)
}

// writeKind streams one kind's versions into its folder.
func (s *snapshot) writeKind(ctx context.Context, kind string) error {
	a := s.a
	k := &kindWalk{concept: a.byName[kind]}
	pw := a.out.Parted(accountexport.RowsDir(kind))
	err := memql.AccountKindVersions(ctx, s.q, a.account, kind, func(n memorynodes.MemoryNode, observed bool) error {
		row, err := s.version(ctx, k, n, observed)
		if err != nil {
			return err
		}
		if err := pw.Write(row); err != nil {
			return err
		}
		a.prog.VersionsWritten++
		return nil
	})
	if err != nil {
		return err
	}
	if k.have {
		if err := s.rowDone(k); err != nil {
			return err
		}
	}
	if err := pw.Close(); err != nil {
		return err
	}
	a.prog.RowsWritten += k.rows
	census := s.census[kind]
	a.report.Kinds = append(a.report.Kinds, accountexport.KindCount{
		Concept: kind, ExpectedRows: census.Rows, ExpectedVersions: census.Versions, WrittenRows: k.rows, WrittenVersions: k.versions,
	})
	return nil
}

// checkCensus is design section E, point 2: the two counts of every kind agree,
// or there is no file.
func (s *snapshot) checkCensus() error {
	var off []string
	for _, k := range s.a.report.Kinds {
		if k.ExpectedRows != k.WrittenRows || k.ExpectedVersions != k.WrittenVersions {
			off = append(off, fmt.Sprintf("%s counted %d rows and %d versions, %d and %d written",
				k.Concept, k.ExpectedRows, k.ExpectedVersions, k.WrittenRows, k.WrittenVersions))
		}
	}
	if len(off) > 0 {
		return failure(FailureCensusMismatch, "The rows written differ from the rows counted: %s.", strings.Join(off, "; "))
	}
	return nil
}

// versionSchemaRefsSQL is every shared schema the versions of the given
// kinds name, in both stores: one pass over the kinds' versions, like the
// census, so the documents are read before the walk streams.
//
// staged-data: MUST-NOT-GATE -- schema references, not row content; each kind is one the file carries.
const versionSchemaRefsSQL = `SELECT DISTINCT substr(schema->>'$ref', 14) FROM (
    SELECT schema FROM "MemoryNodes" WHERE concept = ANY($1::text[])
    UNION ALL
    SELECT schema FROM memory_liveness WHERE concept = ANY($1::text[])
  ) v WHERE left(schema->>'$ref', 13) = 'memql-schema:'`

// memoryNodeSchemasSQL reads shared schema documents by fingerprint.
//
// staged-data: MUST-NOT-GATE -- schema documents, not rows of a concept; each is read because a carried kind's version names it.
const memoryNodeSchemasSQL = `SELECT fingerprint, document FROM memory_node_schemas WHERE fingerprint = ANY($1::text[])`

// loadVersionSchemas reads every shared schema the carried kinds' versions
// were written under, before the walk, so each distinct schema is read once
// per run. A version is stripped of what its OWN schema marked secret as well
// as what the kind's current schema marks (removeSecrets): a field secret when
// the version was written may since have moved, been renamed or been dropped,
// and its value is still in that version.
func (s *snapshot) loadVersionSchemas(ctx context.Context) error {
	kinds := append([]string{accountConcept}, s.a.travel...)
	rows, err := s.q.QueryContext(ctx, versionSchemaRefsSQL, pq.Array(kinds))
	if err != nil {
		return fmt.Errorf("read the versions' schemas: %w", err)
	}
	var fps []string
	for rows.Next() {
		var fp string
		if err := rows.Scan(&fp); err != nil {
			rows.Close()
			return err
		}
		fps = append(fps, fp)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(fps) == 0 {
		return nil
	}
	docs, err := s.q.QueryContext(ctx, memoryNodeSchemasSQL, pq.Array(fps))
	if err != nil {
		return fmt.Errorf("read the schemas: %w", err)
	}
	defer docs.Close()
	for docs.Next() {
		var fp string
		var doc []byte
		if err := docs.Scan(&fp, &doc); err != nil {
			return err
		}
		s.schemaDocs[fp] = doc
	}
	return docs.Err()
}

// versionRulesFor is the secret paths of the schema a version was written
// under, each distinct schema parsed once. A fingerprint with no stored
// document, or an inline schema that is not a JSON object, fails the run: the
// version cannot be checked, so it is not written.
func (s *snapshot) versionRulesFor(c *memorynodes.Concept, n memorynodes.MemoryNode) (*secretRules, error) {
	var key string
	var doc json.RawMessage
	if fp := schemaFingerprint(n.Schema); fp != "" {
		key = "ref:" + fp
		if r, ok := s.versionRules[key]; ok {
			return r, nil
		}
		d, ok := s.schemaDocs[fp]
		if !ok {
			return nil, fmt.Errorf("%s %s was written under the schema %s, which is not stored: the version cannot be checked for secrets", c.Name, n.ID, fp)
		}
		doc = d
	} else {
		if len(bytes.TrimSpace(n.Schema)) == 0 {
			return &secretRules{}, nil
		}
		// A schema stored on the row is its own key: the few distinct
		// documents a kind was written under are parsed once each.
		key = "inline:" + string(n.Schema)
		if r, ok := s.versionRules[key]; ok {
			return r, nil
		}
		doc = n.Schema
	}
	r := &secretRules{}
	if err := json.Unmarshal(doc, &r.root); err != nil {
		return nil, fmt.Errorf("%s %s: the schema it was written under is not a JSON object: %w", c.Name, n.ID, err)
	}
	collectSecretPaths(r.root, "", &r.paths)
	s.versionRules[key] = r
	return r, nil
}

func (s *snapshot) writeSchemas(ctx context.Context) error {
	if len(s.schemas) == 0 {
		return nil
	}
	want := make([]string, 0, len(s.schemas))
	for fp := range s.schemas {
		want = append(want, fp)
	}
	// Written in Go's byte order, which is the file's own and the same on
	// every run; the database's order only decides the order of rows.
	sort.Strings(want)
	for _, fp := range want {
		doc, ok := s.schemaDocs[fp]
		if !ok {
			return fmt.Errorf("the schema %s a carried version names is not stored", fp)
		}
		if err := s.a.out.Schema(fp, doc); err != nil {
			return err
		}
	}
	return nil
}

// handOver runs every registered hand-over against the snapshot, in name
// order, each told which rows travel. A hand-over asks for one kind's ids at
// a time; they are read again from the same snapshot, which answers exactly
// the ids the walk wrote, and only the last kind asked is kept.
func (s *snapshot) handOver(ctx context.Context) error {
	if s.a.w.handOvers == nil {
		return nil
	}
	var lastKind string
	var lastIDs []string
	var idsErr error
	in := accountexport.HandOverInput{
		Account: s.a.account,
		RowIDs: func(concept string) []string {
			if concept == lastKind {
				return lastIDs
			}
			c := s.a.byName[concept]
			if !c.Travels() || s.census[concept].Rows == 0 {
				lastKind, lastIDs = concept, nil
				return nil
			}
			ids, err := memql.AccountKindRowIDs(ctx, s.q, s.a.account, concept)
			if err != nil && idsErr == nil {
				idsErr = err
			}
			lastKind, lastIDs = concept, ids
			return ids
		},
	}
	sink := &countingSink{inner: s.a.out.HandOverSink(), counts: map[string]int64{}}
	for _, name := range s.a.w.handOvers() {
		// Asked here, not left to the statements: a query on the snapshot's
		// connection can still be sent and answered after a cancel.
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := accountexport.RunHandOver(ctx, name, s.q, in, sink); err != nil {
			return err
		}
		if idsErr != nil {
			return fmt.Errorf("hand-over %q: the carried rows could not be read: %w", name, idsErr)
		}
	}
	s.a.report.Members = append(s.a.report.Members, sink.members()...)
	return nil
}

// countingSink counts what the hand-overs write, for the report's members.
type countingSink struct {
	inner  accountexport.HandOverSink
	counts map[string]int64
	order  []string
}

func (c *countingSink) add(name string) {
	if _, ok := c.counts[name]; !ok {
		c.order = append(c.order, name)
	}
	c.counts[name]++
}

func (c *countingSink) Parted(dir string) accountexport.LineSink {
	return &countingLines{inner: c.inner.Parted(dir), sink: c, name: "tables/" + dir}
}

func (c *countingSink) Line(member string, v any) error {
	if err := c.inner.Line(member, v); err != nil {
		return err
	}
	c.add(member)
	return nil
}

func (c *countingSink) members() []accountexport.MemberCount {
	out := make([]accountexport.MemberCount, 0, len(c.order))
	for _, name := range c.order {
		out = append(out, accountexport.MemberCount{Name: name, Lines: c.counts[name]})
	}
	return out
}

type countingLines struct {
	inner accountexport.LineSink
	sink  *countingSink
	name  string
}

func (l *countingLines) Write(v any) error {
	if err := l.inner.Write(v); err != nil {
		return err
	}
	l.sink.add(l.name)
	return nil
}

func (l *countingLines) Close() error { return l.inner.Close() }

// ---------------------------------------------------------------------------
// Secret values

// A @secret value never travels (design section C). The engine marks a secret
// field `x-secret` on the kind's definition schema at any depth: a top-level
// field, a field of an object block, of an array's objects, of a map's values
// or of one branch of a union (memory-nodes concept_parser.go,
// propertyToJSONSchema). The walk below reads the schema the way the engine's
// own redaction does (memory-nodes concept_secret_redaction.go,
// schemaMarksSecret and childSchemas): named properties first, a map's values
// for a key no property names, an array's items, and every branch of oneOf,
// anyOf and allOf. A marked node is secret with everything beneath it.
//
// Paths are written with "." between fields, "[]" for an array's items and
// "*" for a map's values: `settings.apiKey`, `entries[].pin`, `extra.*.code`.

// secretRules is one kind's definition schema and its secret paths.
type secretRules struct {
	root  map[string]any
	paths []string
}

// secretCache holds each concept's rules by pointer: a kind is read once, and
// two concepts of one name (a test's copy) never share an answer.
//
// IT IS BOUNDED, NOT PER RUN. A registry reload (a pack enabled, a bundle
// reloaded) builds new concept values, so a cache keyed by pointer and never
// emptied would keep a full set for every reload for the life of the process.
// The rules are asked for by functions that hold only the concept (the
// readers of references, people and bytes, and SecretPaths, which a build gate
// calls), and an entry is a pure function of the concept it is keyed by, so a
// cache that is emptied when it reaches secretCacheMax entries answers the
// same as one per run, without a run's cache threaded through each of them.
// secretCacheMax is far above the number of kinds one registry holds; the
// price of emptying it is reading each kind's schema once more.
const secretCacheMax = 1024

type rulesCache struct {
	mu      sync.Mutex
	entries map[*memorynodes.Concept]*secretRules
}

func (c *rulesCache) load(k *memorynodes.Concept) (*secretRules, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.entries[k]
	return r, ok
}

// store keeps r unless another caller stored one first, emptying the cache
// when it is full, and answers the entry kept.
func (c *rulesCache) store(k *memorynodes.Concept, r *secretRules) *secretRules {
	c.mu.Lock()
	defer c.mu.Unlock()
	if have, ok := c.entries[k]; ok {
		return have
	}
	if c.entries == nil || len(c.entries) >= secretCacheMax {
		c.entries = map[*memorynodes.Concept]*secretRules{}
	}
	c.entries[k] = r
	return r
}

func (c *rulesCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = nil
}

func (c *rulesCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

var secretCache rulesCache

func rulesFor(c *memorynodes.Concept) *secretRules {
	if c == nil {
		return &secretRules{}
	}
	if r, ok := secretCache.load(c); ok {
		return r
	}
	r := &secretRules{}
	if raw := c.Schemas["definition"]; len(raw) > 0 {
		_ = json.Unmarshal(raw, &r.root)
	}
	collectSecretPaths(r.root, "", &r.paths)
	sort.Strings(r.paths)
	r.paths = dedupe(r.paths)
	return secretCache.store(c, r)
}

func dedupe(in []string) []string {
	out := in[:0]
	for i, v := range in {
		if i == 0 || v != in[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// SecretPaths is every path at which the kind declares a @secret value, at
// any depth, sorted. The export removes each from every carried version, and
// the build gate that keeps the engine's travelling kinds free of secrets
// (test/dslconformance/account_export_declarations_test.go) asks the same.
func SecretPaths(c *memorynodes.Concept) []string {
	return append([]string(nil), rulesFor(c).paths...)
}

func secretMarked(node map[string]any) bool {
	v, ok := node["x-secret"].(bool)
	return ok && v
}

func joinPath(path, seg string) string {
	if path == "" {
		return seg
	}
	if seg == "[]" {
		return path + seg
	}
	return path + "." + seg
}

func collectSecretPaths(node map[string]any, path string, out *[]string) {
	if node == nil {
		return
	}
	if secretMarked(node) {
		if path != "" {
			*out = append(*out, path)
		}
		return
	}
	if props, ok := node["properties"].(map[string]any); ok {
		for k, child := range props {
			if m, ok := child.(map[string]any); ok {
				collectSecretPaths(m, joinPath(path, k), out)
			}
		}
	}
	if extra, ok := node["additionalProperties"].(map[string]any); ok {
		collectSecretPaths(extra, joinPath(path, "*"), out)
	}
	if items, ok := node["items"].(map[string]any); ok {
		collectSecretPaths(items, joinPath(path, "[]"), out)
	}
	for _, branch := range unionBranches(node) {
		collectSecretPaths(branch, path, out)
	}
}

func unionBranches(node map[string]any) []map[string]any {
	var out []map[string]any
	for _, comb := range []string{"oneOf", "anyOf", "allOf"} {
		branches, _ := node[comb].([]any)
		for _, b := range branches {
			if m, ok := b.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}
	return out
}

// schemaChild is a schema node that governs a key or an item, and the path
// segment it is reached by.
type schemaChild struct {
	node map[string]any
	seg  string
}

// keyChildren is every node that can govern key beneath node.
func keyChildren(node map[string]any, key string) []schemaChild {
	var out []schemaChild
	named := false
	if props, ok := node["properties"].(map[string]any); ok {
		if m, ok := props[key].(map[string]any); ok {
			out = append(out, schemaChild{m, key})
			named = true
		}
	}
	if !named {
		if extra, ok := node["additionalProperties"].(map[string]any); ok {
			out = append(out, schemaChild{extra, "*"})
		}
	}
	for _, b := range unionBranches(node) {
		out = append(out, keyChildren(b, key)...)
	}
	return out
}

// itemChildren is every node that can govern an array's items beneath node.
func itemChildren(node map[string]any) []schemaChild {
	var out []schemaChild
	if items, ok := node["items"].(map[string]any); ok {
		out = append(out, schemaChild{items, "[]"})
	}
	for _, b := range unionBranches(node) {
		out = append(out, itemChildren(b)...)
	}
	return out
}

// stripSecrets removes every secret value beneath v, as node governs it,
// calling removed with the path of each one it removes.
func stripSecrets(node map[string]any, v any, path string, removed func(string)) (any, bool) {
	if node == nil {
		return v, false
	}
	changed := false
	switch val := v.(type) {
	case map[string]any:
		for k, child := range val {
			children := keyChildren(node, k)
			secret := false
			for _, c := range children {
				if secretMarked(c.node) {
					secret = true
					removed(joinPath(path, c.seg))
				}
			}
			if secret {
				delete(val, k)
				changed = true
				continue
			}
			for _, c := range children {
				if nv, ch := stripSecrets(c.node, child, joinPath(path, c.seg), removed); ch {
					child, changed = nv, true
				}
			}
			val[k] = child
		}
		return val, changed
	case []any:
		children := itemChildren(node)
		for _, c := range children {
			if secretMarked(c.node) {
				if len(val) == 0 {
					return val, false
				}
				removed(joinPath(path, c.seg))
				return []any{}, true
			}
		}
		for i, e := range val {
			for _, c := range children {
				if ne, ch := stripSecrets(c.node, e, joinPath(path, c.seg), removed); ch {
					e, changed = ne, true
				}
			}
			val[i] = e
		}
		return val, changed
	}
	return v, false
}

// isSecretPath reports whether a dotted field path reaches a secret node: such
// a field is never read as a reference, a person or bytes.
func isSecretPath(c *memorynodes.Concept, dotted string) bool {
	root := rulesFor(c).root
	if root == nil {
		return false
	}
	nodes := []map[string]any{root}
	for _, tok := range strings.Split(dotted, ".") {
		var next []map[string]any
		for _, n := range nodes {
			for _, ch := range keyChildren(n, tok) {
				if secretMarked(ch.node) {
					return true
				}
				next = append(next, ch.node)
			}
		}
		nodes = next
	}
	return false
}

// removeSecrets answers the version's payload with every secret value
// removed, judged by BOTH the kind's current definition and the schema the
// version was written under, noting each removed path for the report. A payload with nothing
// removed travels byte for byte as stored; one with something removed is
// re-encoded with its numbers kept exactly.
func (s *snapshot) removeSecrets(c *memorynodes.Concept, n memorynodes.MemoryNode) (json.RawMessage, error) {
	own, err := s.versionRulesFor(c, n)
	if err != nil {
		return nil, err
	}
	var all []*secretRules
	for _, r := range []*secretRules{rulesFor(c), own} {
		if len(r.paths) > 0 {
			all = append(all, r)
		}
	}
	if len(all) == 0 || len(n.Payload) == 0 {
		return n.Payload, nil
	}
	dec := json.NewDecoder(bytes.NewReader(n.Payload))
	dec.UseNumber()
	var payload any
	if err := dec.Decode(&payload); err != nil {
		return nil, fmt.Errorf("%s %s: the payload is not JSON: %w", c.Name, n.ID, err)
	}
	changed := false
	stripped := payload
	for _, r := range all {
		var ch bool
		stripped, ch = stripSecrets(r.root, stripped, "", func(path string) {
			s.scrubbed[accountexport.ScrubbedField{Concept: c.Name, Field: path}] = true
		})
		changed = changed || ch
	}
	if !changed {
		return n.Payload, nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(stripped); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
}
