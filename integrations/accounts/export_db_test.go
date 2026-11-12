package accounts

// export_db_test.go -- the export's record, its calls, its readiness and the
// runner, against a real Postgres (design record
// docs/superpowers/specs/2026-11-12-account-export-design.md, sections F and H).
//
// Unlike the inventory's tests these use the REAL tables on a pool of several
// connections: the runner's claim, heartbeat and take-over are statements that
// must hold across sessions (a second runner on another node is another
// session), which a temp table on one connection cannot show. Each test starts
// by emptying the tables it reads, so the tests run one after another
// (`-p=1`, and none is parallel).

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/metrics"
	"github.com/znasllc-io/memql/component/secret"
	"github.com/znasllc-io/memql/integrations/email"
)

// exportFixtureConcepts: two kinds the engine records, one of them declared
// shared, and a kind whose rows state their own account.
var exportFixtureConcepts = []*memorynodes.Concept{
	{Name: "v1:testa:note", BelongsTo: &parser.BelongsToDecl{Scope: parser.BelongsToAccount}},
	{Name: "v1:testa:skill", BelongsTo: &parser.BelongsToDecl{Scope: parser.BelongsToAccount, Shared: true}},
	{Name: "v1:testa:channel", BelongsTo: &parser.BelongsToDecl{Scope: parser.BelongsToAccount, Stated: true}},
}

// recordingExecutor stands in for the engine: it records every statement the
// integration sends (the audit events among them) and answers nothing.
type recordingExecutor struct {
	mu      sync.Mutex
	queries []string
}

func (r *recordingExecutor) Execute(_ context.Context, query string) (*memql.ExecuteResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queries = append(r.queries, query)
	return nil, nil
}

func (r *recordingExecutor) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.queries...)
}

// audits returns the createAuditEvent statements whose action is `action`.
func (r *recordingExecutor) audits(action string) []string {
	var out []string
	for _, q := range r.all() {
		if strings.Contains(q, "createAuditEvent(") && strings.Contains(q, `"`+action+`"`) {
			out = append(out, q)
		}
	}
	return out
}

// fakeBlobs is the export blob store. failDelete makes every Delete fail, as
// storage that cannot be reached does.
type fakeBlobs struct {
	mu         sync.Mutex
	deleted    []string
	content    map[string][]byte
	failDelete bool
}

func (f *fakeBlobs) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failDelete {
		return errors.New("storage is unavailable")
	}
	f.deleted = append(f.deleted, key)
	delete(f.content, key)
	return nil
}

func (f *fakeBlobs) Open(_ context.Context, key string) (io.ReadCloser, error) {
	return f.OpenRange(context.Background(), key, 0, int64(len(f.content[key])))
}

func (f *fakeBlobs) OpenRange(_ context.Context, key string, offset, count int64) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.content[key]
	if !ok {
		return nil, errors.New("no such object")
	}
	return io.NopCloser(bytes.NewReader(b[offset : offset+count])), nil
}

// exportFixture is one test's integration, database and captured output.
type exportFixture struct {
	db     *bun.DB
	x      *Exports
	exec   *recordingExecutor
	blobs  *fakeBlobs
	logs   *lockedBuffer
	states []memql.AccountAssignmentState
	undecl []string
	follow int64
	mu     sync.Mutex
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func exportPool(t *testing.T) *bun.DB {
	t.Helper()
	ctx := context.Background()
	reachable, err := dbtest.EnsureSchema(ctx)
	require.NoError(t, err)
	if !reachable {
		dbtest.Unreachable(t, "account export", dbtest.DSN(), sql.ErrConnDone)
	}
	db := bun.NewDB(sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN()))), pgdialect.New())
	t.Cleanup(func() { _ = db.Close() })
	for _, stmt := range []string{
		`TRUNCATE account_exports, account_assignment_progress, account_assignment_conflict, account_follow_tasks`,
		`DELETE FROM "MemoryNodes" WHERE concept = 'v1:accounts:account' AND id LIKE 'v1:accounts:account:aex-%'`,
		`INSERT INTO account_stamping_storage DEFAULT VALUES ON CONFLICT DO NOTHING`,
		`UPDATE account_stamping_storage SET enabled = true`,
	} {
		_, err := db.ExecContext(ctx, stmt)
		require.NoError(t, err, stmt)
	}
	return db
}

func newExportFixture(t *testing.T) *exportFixture {
	t.Helper()
	t.Setenv(secret.EnvMasterKey, strings.Repeat("ab", 32))
	db := exportPool(t)
	f := &exportFixture{db: db, exec: &recordingExecutor{}, blobs: &fakeBlobs{content: map[string][]byte{}}, logs: &lockedBuffer{}}
	logger := slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	inv := New(func() *bun.DB { return db }, func() []*memorynodes.Concept { return exportFixtureConcepts }, false, logger)
	f.x = NewExports(inv, f.exec, logger)
	f.x.blobs = func() (ExportBlobs, error) { return f.blobs, nil }
	// Every kind finished and nothing waiting, until a test says otherwise.
	f.states = []memql.AccountAssignmentState{
		{Concept: "v1:testa:note", FirstPassFinished: true, VerifiedThrough: time.Now()},
		{Concept: "v1:testa:skill", FirstPassFinished: true, VerifiedThrough: time.Now()},
	}
	f.x.assignmentStates = func(context.Context, memql.AccountQuerier) ([]memql.AccountAssignmentState, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return append([]memql.AccountAssignmentState(nil), f.states...), nil
	}
	f.x.undeclared = func([]*memorynodes.Concept) []string {
		f.mu.Lock()
		defer f.mu.Unlock()
		return append([]string(nil), f.undecl...)
	}
	f.x.followPending = func(context.Context, memql.AccountQuerier, string) (int64, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.follow, nil
	}
	return f
}

// account writes an account row so prepare can name it.
func (f *exportFixture) account(t *testing.T, bare, name string) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"name": name})
	require.NoError(t, err)
	_, err = f.db.ExecContext(context.Background(),
		`INSERT INTO "MemoryNodes" (id, "createdAt", "createdBy", schema, payload, concept) VALUES (?, clock_timestamp(), 'test', '{}', ?, 'v1:accounts:account')`,
		canonicalPrefix+bare, string(encoded))
	require.NoError(t, err)
	return bare
}

const exportPerson = "v1:identity:user:exporter"

// person is a person's own signed-in session holding the export part.
func person(id string) context.Context {
	ctx := auth.ContextWithClaims(context.Background(), map[string]any{"sub": id, "role": "owner"})
	return auth.ContextWithAccess(ctx, &auth.AccessContext{UserId: id, Role: auth.RoleOwner, PrimaryEmail: "exporter@example.test"})
}

// exportPayloads decodes an answer.
func exportPayloads(t *testing.T, nodes []memorynodes.MemoryNode) []map[string]any {
	t.Helper()
	out := make([]map[string]any, 0, len(nodes))
	ids := map[string]bool{}
	for _, n := range nodes {
		require.False(t, ids[n.ID], "two answer rows share the id %s", n.ID)
		ids[n.ID] = true
		var p map[string]any
		require.NoError(t, json.Unmarshal(n.Payload, &p))
		out = append(out, p)
	}
	return out
}

func one(t *testing.T, nodes []memorynodes.MemoryNode, err error) map[string]any {
	t.Helper()
	require.NoError(t, err)
	rows := exportPayloads(t, nodes)
	require.Len(t, rows, 1)
	return rows[0]
}

func (f *exportFixture) prepare(t *testing.T, account string, protect bool) map[string]any {
	t.Helper()
	nodes, err := f.x.handlePrepare(person(exportPerson), map[string]any{"accountId": account, "protect": protect}, 0)
	return one(t, nodes, err)
}

func (f *exportFixture) start(t *testing.T, id string) map[string]any {
	t.Helper()
	nodes, err := f.x.handleStart(person(exportPerson), map[string]any{"exportId": id}, 0)
	return one(t, nodes, err)
}

func (f *exportFixture) row(t *testing.T, id string) (Export, sql.NullString) {
	t.Helper()
	x, ok, err := f.x.get(context.Background(), id)
	require.NoError(t, err)
	require.True(t, ok, "export %s is gone", id)
	var sealed sql.NullString
	require.NoError(t, f.db.QueryRowContext(context.Background(), `SELECT sealed_passphrase FROM account_exports WHERE id = ?`, id).Scan(&sealed))
	return x, sealed
}

// runner builds a runner for one node with test intervals.
func (f *exportFixture) runner(t *testing.T, node string, work ExportWork) *ExportRunner {
	t.Helper()
	r, err := NewExportRunner(f.x, func() string { return node })
	require.NoError(t, err)
	r.heartbeatEvery = 50 * time.Millisecond
	r.work = work
	return r
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (f *exportFixture) waitState(t *testing.T, id, state string) Export {
	t.Helper()
	var x Export
	waitFor(t, "export "+id+" to be "+state, func() bool {
		x, _ = f.row(t, id)
		return x.State == state
	})
	return x
}

func conditions(t *testing.T, nodes []memorynodes.MemoryNode) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, p := range exportPayloads(t, nodes) {
		switch p["kind"] {
		case "condition":
			out[p["name"].(string)] = p
		default:
			out[p["kind"].(string)] = p
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Readiness

func TestExportReadinessEachConditionMetAndUnmet(t *testing.T) {
	f := newExportFixture(t)
	ctx := context.Background()
	acme := f.account(t, "aex-acme", "Acme")
	read := func() map[string]map[string]any {
		nodes, err := f.x.handleReadiness(ctx, map[string]any{"accountId": acme}, 0)
		require.NoError(t, err)
		return conditions(t, nodes)
	}

	all := read()
	for _, name := range []string{"recording", "assigned", "declared", "decided", "followed"} {
		require.Equal(t, true, all[name]["met"], "%s should be met: %v", name, all[name])
	}
	require.Equal(t, true, all["ready"]["ready"])
	require.Equal(t, false, all["recording"]["held"])
	require.EqualValues(t, 2, all["assigned"]["kindsTotal"])
	require.EqualValues(t, 2, all["assigned"]["kindsFinished"])
	require.EqualValues(t, 0, all["assigned"]["waiting"])
	require.Equal(t, []any{}, all["declared"]["concepts"])
	require.EqualValues(t, 0, all["unplaced"]["rows"])

	// recording: the cluster's switch off.
	_, err := f.db.ExecContext(ctx, `UPDATE account_stamping_storage SET enabled = false`)
	require.NoError(t, err)
	got := read()
	require.Equal(t, false, got["recording"]["met"])
	require.Equal(t, false, got["ready"]["ready"])
	_, err = f.db.ExecContext(ctx, `UPDATE account_stamping_storage SET enabled = true`)
	require.NoError(t, err)

	// recording: this node held.
	f.x.inv.held = true
	got = read()
	require.Equal(t, false, got["recording"]["met"])
	require.Equal(t, true, got["recording"]["held"])
	f.x.inv.held = false

	// assigned: one kind not finished, then one with rows waiting.
	f.mu.Lock()
	f.states[1].FirstPassFinished = false
	f.mu.Unlock()
	got = read()
	require.Equal(t, false, got["assigned"]["met"])
	require.EqualValues(t, 1, got["assigned"]["kindsFinished"])
	f.mu.Lock()
	f.states[1].FirstPassFinished = true
	f.states[0].Waiting = 4
	f.mu.Unlock()
	got = read()
	require.Equal(t, false, got["assigned"]["met"])
	require.EqualValues(t, 4, got["assigned"]["waiting"])
	f.mu.Lock()
	f.states[0].Waiting = 0
	f.mu.Unlock()

	// declared.
	f.mu.Lock()
	f.undecl = []string{"v1:testz:loose"}
	f.mu.Unlock()
	got = read()
	require.Equal(t, false, got["declared"]["met"])
	require.Equal(t, []any{"v1:testz:loose"}, got["declared"]["concepts"])
	f.mu.Lock()
	f.undecl = nil
	f.mu.Unlock()

	// decided: an open conflict naming the account holds it back; one naming
	// another account, and one an owner left out, do not.
	_, err = f.db.ExecContext(ctx, `INSERT INTO account_assignment_conflict (concept, row_id, kind, candidates) VALUES
		('v1:testa:note', 'n1', 'k', '{aex-acme,aex-other}'),
		('v1:testa:note', 'n2', 'k', '{aex-other}')`)
	require.NoError(t, err)
	_, err = f.db.ExecContext(ctx, `INSERT INTO account_assignment_conflict (concept, row_id, kind, candidates, left_out_at, left_out_by) VALUES
		('v1:testa:note', 'n3', 'k', '{aex-acme}', clock_timestamp(), 'someone')`)
	require.NoError(t, err)
	got = read()
	require.Equal(t, false, got["decided"]["met"])
	require.EqualValues(t, 1, got["decided"]["rows"])
	_, err = f.db.ExecContext(ctx, `UPDATE account_assignment_conflict SET resolved_at = clock_timestamp()`)
	require.NoError(t, err)

	// followed.
	f.mu.Lock()
	f.follow = 3
	f.mu.Unlock()
	got = read()
	require.Equal(t, false, got["followed"]["met"])
	require.EqualValues(t, 3, got["followed"]["tasks"])
	f.mu.Lock()
	f.follow = 0
	f.mu.Unlock()

	require.Equal(t, true, read()["ready"]["ready"])
}

// A seam that is not wired is a condition not met: readiness fails closed.
func TestExportReadinessFailsClosedOnAnUnwiredSeam(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	f.x.undeclared = nil
	f.x.followPending = nil
	f.x.assignmentStates = nil
	nodes, err := f.x.handleReadiness(context.Background(), map[string]any{"accountId": acme}, 0)
	require.NoError(t, err)
	got := conditions(t, nodes)
	require.Equal(t, false, got["declared"]["met"])
	require.Equal(t, false, got["followed"]["met"])
	require.Equal(t, false, got["assigned"]["met"])
	require.Equal(t, false, got["ready"]["ready"])
}

// The quiet count: for every account-scoped kind not declared shared, the rows
// the job left without an account (its `shared` counter: left alone and left
// out for good) plus those waiting, read from its counters. OPEN conflicts are
// shown under `decided` and are not counted here a second time.
func TestExportReadinessCountsRowsInNoAccountFromTheJobsCounters(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	_, err := f.db.ExecContext(context.Background(), `INSERT INTO account_assignment_progress (concept, shared, conflicts, waiting) VALUES
		('v1:testa:note', 5, 2, 1),
		('v1:testa:skill', 100, 0, 0),
		('v1:testz:other', 9, 9, 9)`)
	require.NoError(t, err)
	nodes, err := f.x.handleReadiness(context.Background(), map[string]any{"accountId": acme}, 0)
	require.NoError(t, err)
	require.EqualValues(t, 6, conditions(t, nodes)["unplaced"]["rows"])
}

// ---------------------------------------------------------------------------
// Prepare and the passphrase

func TestExportPrepareRefusesWhenNotReady(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	f.mu.Lock()
	f.follow = 1
	f.mu.Unlock()
	_, err := f.x.handlePrepare(person(exportPerson), map[string]any{"accountId": acme, "protect": true}, 0)
	require.Error(t, err)
	require.True(t, strings.HasPrefix(err.Error(), "export_not_ready: "), err.Error())
	require.Contains(t, err.Error(), "Rows following a moved parent")
	var n int
	require.NoError(t, f.db.QueryRowContext(context.Background(), `SELECT count(*) FROM account_exports`).Scan(&n))
	require.Zero(t, n)
}

func TestExportPrepareRefusesWhileOneIsActive(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	first := f.prepare(t, acme, false)
	for _, state := range []string{"draft", "queued", "running"} {
		_, err := f.db.ExecContext(context.Background(), `UPDATE account_exports SET state = ? WHERE id = ?`, state, first["id"])
		require.NoError(t, err)
		_, err = f.x.handlePrepare(person(exportPerson), map[string]any{"accountId": acme}, 0)
		require.Error(t, err, state)
		require.True(t, strings.HasPrefix(err.Error(), "export_in_progress: "), err.Error())
		require.Contains(t, err.Error(), first["id"].(string))
	}
	// Once it is over, another may be prepared.
	_, err := f.db.ExecContext(context.Background(), `UPDATE account_exports SET state = 'complete' WHERE id = ?`, first["id"])
	require.NoError(t, err)
	second := f.prepare(t, acme, false)
	require.NotEqual(t, first["id"], second["id"])
}

func TestExportPassphraseIsReturnedOnceAndStoredSealed(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	answer := f.prepare(t, acme, true)
	require.Equal(t, "export", answer["kind"])
	require.Equal(t, "draft", answer["state"])
	require.Equal(t, true, answer["protected"])
	require.Equal(t, "Acme", answer["accountName"])
	require.Equal(t, exportPerson, answer["requestedBy"])
	pass, _ := answer["passphrase"].(string)
	require.Len(t, pass, 29, "five groups of five joined by dashes")
	id := answer["id"].(string)

	_, sealed := f.row(t, id)
	require.True(t, sealed.Valid)
	require.NotContains(t, sealed.String, pass)
	opened, err := secret.Decrypt(sealed.String)
	require.NoError(t, err)
	require.Equal(t, pass, opened)

	// No other answer carries it.
	for name, call := range map[string]func() ([]memorynodes.MemoryNode, error){
		"status": func() ([]memorynodes.MemoryNode, error) {
			return f.x.handleStatus(person(exportPerson), map[string]any{"exportId": id}, 0)
		},
		"list": func() ([]memorynodes.MemoryNode, error) {
			return f.x.handleList(person(exportPerson), map[string]any{"accountId": acme}, 0)
		},
		"start": func() ([]memorynodes.MemoryNode, error) {
			return f.x.handleStart(person(exportPerson), map[string]any{"exportId": id}, 0)
		},
	} {
		nodes, err := call()
		require.NoError(t, err, name)
		for _, n := range nodes {
			require.NotContains(t, string(n.Payload), pass, name)
			require.NotContains(t, string(n.Payload), "passphrase", name)
		}
	}

	// Unprotected: no passphrase and nothing sealed.
	_, err = f.db.ExecContext(context.Background(), `UPDATE account_exports SET state = 'complete' WHERE id = ?`, id)
	require.NoError(t, err)
	plain := f.prepare(t, acme, false)
	require.NotContains(t, plain, "passphrase")
	require.Equal(t, false, plain["protected"])
	_, sealed = f.row(t, plain["id"].(string))
	require.False(t, sealed.Valid)
}

// ---------------------------------------------------------------------------
// Start, the runner and the final states

func TestExportRunClaimsHeartbeatsAndCompletes(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	prepared := f.prepare(t, acme, true)
	pass := prepared["passphrase"].(string)
	id := prepared["id"].(string)
	started := f.start(t, id)
	require.Equal(t, "queued", started["state"])
	require.Len(t, f.exec.audits("account_export_started"), 1)
	require.Len(t, f.exec.audits("account_export_prepared"), 1)

	before := metrics.AccountExportsValue(metrics.AccountExportComplete)
	var gotPass string
	var gotAttempt int
	var beats atomic.Int32
	work := func(ctx context.Context, x Export, passphrase string, progress func(ExportProgress)) (ExportResult, error) {
		gotPass, gotAttempt = passphrase, x.Attempt
		progress(ExportProgress{Phase: "rows", KindsTotal: 3, KindsDone: 1, RowsWritten: 10, VersionsWritten: 12})
		first, _ := f.row(t, x.ID)
		require.Equal(t, "rows", first.Phase)
		require.EqualValues(t, 10, first.RowsWritten)
		var hb0 time.Time
		require.NoError(t, f.db.QueryRowContext(ctx, `SELECT heartbeat_at FROM account_exports WHERE id = ?`, x.ID).Scan(&hb0))
		waitFor(t, "a heartbeat", func() bool {
			var hb time.Time
			require.NoError(t, f.db.QueryRowContext(ctx, `SELECT heartbeat_at FROM account_exports WHERE id = ?`, x.ID).Scan(&hb))
			if hb.After(hb0) {
				beats.Add(1)
				return true
			}
			return false
		})
		return ExportResult{ObjectKey: "exports/accounts/" + x.ID + "/1/file", FileName: "acme.tar.gz.age", SHA256: "abc", SizeBytes: 42,
			Report: json.RawMessage(`{"rows":10}`)}, nil
	}
	r := f.runner(t, "agent-a", work)
	r.tick(context.Background())
	x := f.waitState(t, id, "complete")
	r.wait()

	require.Equal(t, pass, gotPass)
	require.Equal(t, 1, gotAttempt)
	require.EqualValues(t, 1, beats.Load())
	require.Equal(t, "exports/accounts/"+id+"/1/file", x.ObjectKey)
	require.Equal(t, "acme.tar.gz.age", x.FileName)
	require.EqualValues(t, 42, x.SizeBytes)
	require.NotNil(t, x.FinishedAt)
	require.Equal(t, "", x.Phase)
	_, sealed := f.row(t, id)
	require.False(t, sealed.Valid, "the sealed passphrase is cleared at complete")
	require.Len(t, f.exec.audits("account_export_completed"), 1)
	require.Equal(t, before+1, metrics.AccountExportsValue(metrics.AccountExportComplete))

	status := answer(t)(f.x.handleStatus(person(exportPerson), map[string]any{"exportId": id}, 0))
	require.Equal(t, map[string]any{"rows": float64(10)}, status["report"])
	require.Equal(t, "complete", status["state"])
}

func TestExportPassphraseIsClearedAtFailedAndCancelled(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	globex := f.account(t, "aex-globex", "Globex")

	// Failed, with the work's own code.
	failing := f.prepare(t, acme, true)
	f.start(t, failing["id"].(string))
	r := f.runner(t, "agent-a", func(context.Context, Export, string, func(ExportProgress)) (ExportResult, error) {
		return ExportResult{}, &ExportFailure{Code: "export_census_mismatch", Detail: "2 rows of v1:testa:note differ"}
	})
	r.tick(context.Background())
	x := f.waitState(t, failing["id"].(string), "failed")
	r.wait()
	require.Equal(t, "export_census_mismatch", x.FailureCode)
	require.Equal(t, "2 rows of v1:testa:note differ", x.FailureDetail)
	_, sealed := f.row(t, x.ID)
	require.False(t, sealed.Valid)
	require.Len(t, f.exec.audits("account_export_failed"), 1)

	// Cancelled while running: the work's context ends.
	cancelling := f.prepare(t, globex, true)
	cid := cancelling["id"].(string)
	f.start(t, cid)
	entered := make(chan struct{})
	r = f.runner(t, "agent-a", func(ctx context.Context, _ Export, _ string, _ func(ExportProgress)) (ExportResult, error) {
		close(entered)
		<-ctx.Done()
		return ExportResult{}, ctx.Err()
	})
	r.tick(context.Background())
	<-entered
	answer := answer(t)(f.x.handleCancel(person(exportPerson), map[string]any{"exportId": cid}, 0))
	require.Equal(t, true, answer["cancelRequested"])
	x = f.waitState(t, cid, "cancelled")
	r.wait()
	_, sealed = f.row(t, cid)
	require.False(t, sealed.Valid)
	require.Len(t, f.exec.audits("account_export_cancelled"), 1)
}

func TestExportStaleRunIsTakenOverAndFailsAfterThreeAttempts(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	id := f.prepare(t, acme, true)["id"].(string)
	f.start(t, id)
	ctx := context.Background()

	// A node claims and dies: nothing heartbeats.
	for attempt := 1; attempt <= 3; attempt++ {
		claimed, _, ok, err := f.x.claim(ctx, "agent-dead")
		require.NoError(t, err)
		require.True(t, ok, "attempt %d", attempt)
		require.Equal(t, attempt, claimed.Attempt)
		_, err = f.db.ExecContext(ctx, `UPDATE account_exports SET heartbeat_at = clock_timestamp() - interval '3 minutes' WHERE id = ?`, id)
		require.NoError(t, err)
		require.NoError(t, f.x.reclaimStale(ctx))
		x, sealed := f.row(t, id)
		if attempt < 3 {
			require.Equal(t, "queued", x.State, "attempt %d is queued again", attempt)
			require.True(t, sealed.Valid, "the passphrase survives a take-over")
		} else {
			require.Equal(t, "failed", x.State)
			require.Equal(t, "export_failed", x.FailureCode)
			require.False(t, sealed.Valid)
		}
	}
	require.Len(t, f.exec.audits("account_export_failed"), 1)
}

func TestExportSecondExportWaitsWhileOneRuns(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	globex := f.account(t, "aex-globex", "Globex")
	first := f.prepare(t, acme, false)["id"].(string)
	second := f.prepare(t, globex, false)["id"].(string)
	f.start(t, first)
	f.start(t, second)

	release := make(chan struct{})
	var ran []string
	var mu sync.Mutex
	work := func(ctx context.Context, x Export, _ string, _ func(ExportProgress)) (ExportResult, error) {
		mu.Lock()
		ran = append(ran, x.ID)
		mu.Unlock()
		if x.ID == first {
			<-release
		}
		return ExportResult{ObjectKey: "exports/accounts/" + x.ID + "/1/f", FileName: "f", SizeBytes: 1}, nil
	}
	a := f.runner(t, "agent-a", work)
	b := f.runner(t, "agent-b", work)
	a.tick(context.Background())
	f.waitState(t, first, "running")
	// Another node, and the same node again, both leave the second queued.
	b.tick(context.Background())
	a.tick(context.Background())
	x, _ := f.row(t, second)
	require.Equal(t, "queued", x.State)

	close(release)
	f.waitState(t, first, "complete")
	a.wait()
	b.tick(context.Background())
	f.waitState(t, second, "complete")
	b.wait()
	require.Equal(t, []string{first, second}, ran)
}

// Step 3: a run prepared and started on one handle is taken over by a runner
// with another node id once the first stops heartbeating, and the first one's
// late answer changes nothing.
func TestExportRunIsTakenOverByAnotherNode(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	id := f.prepare(t, acme, true)["id"].(string)
	f.start(t, id)

	// Node A claims, then stops heartbeating while its work goes on.
	stuck, releaseA := make(chan struct{}), make(chan struct{})
	a := f.runner(t, "agent-a", func(context.Context, Export, string, func(ExportProgress)) (ExportResult, error) {
		close(stuck)
		<-releaseA
		return ExportResult{ObjectKey: "exports/accounts/late", FileName: "late", SizeBytes: 1}, nil
	})
	a.heartbeatEvery = time.Hour
	a.tick(context.Background())
	<-stuck
	_, err := f.db.ExecContext(context.Background(), `UPDATE account_exports SET heartbeat_at = clock_timestamp() - interval '3 minutes' WHERE id = ?`, id)
	require.NoError(t, err)

	// Node B takes it over: attempt 2, with the passphrase.
	var bAttempt int
	var bPass string
	bEntered, releaseB := make(chan struct{}), make(chan struct{})
	b := f.runner(t, "agent-b", func(_ context.Context, x Export, passphrase string, _ func(ExportProgress)) (ExportResult, error) {
		bAttempt, bPass = x.Attempt, passphrase
		close(bEntered)
		<-releaseB
		return ExportResult{ObjectKey: fmt.Sprintf("exports/accounts/%s/%d/f", x.ID, x.Attempt), FileName: "f", SizeBytes: 7}, nil
	})
	b.tick(context.Background())
	<-bEntered

	// Node A's late answer, while B runs, writes nothing over B's attempt.
	close(releaseA)
	a.wait()
	// ...and the file it wrote, which nothing records, is removed.
	require.Equal(t, []string{"exports/accounts/late"}, f.blobs.deletedKeys())
	x, sealed := f.row(t, id)
	require.Equal(t, "running", x.State)
	require.Equal(t, 2, x.Attempt)
	require.EqualValues(t, 0, x.SizeBytes)
	require.True(t, sealed.Valid)
	require.Empty(t, f.exec.audits("account_export_completed"))
	var claimedBy string
	require.NoError(t, f.db.QueryRowContext(context.Background(), `SELECT claimed_by FROM account_exports WHERE id = ?`, id).Scan(&claimedBy))
	require.Equal(t, "agent-b", claimedBy)

	close(releaseB)
	x = f.waitState(t, id, "complete")
	b.wait()
	require.Equal(t, 2, bAttempt)
	require.Len(t, bPass, 29)
	require.Equal(t, fmt.Sprintf("exports/accounts/%s/2/f", id), x.ObjectKey)
	require.EqualValues(t, 7, x.SizeBytes)
	require.Len(t, f.exec.audits("account_export_completed"), 1)
}

// ---------------------------------------------------------------------------
// Cancel, remove, the sweep, and the download methods

func TestExportCancelQueuedAndDraft(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	draft := f.prepare(t, acme, true)["id"].(string)
	got := answer(t)(f.x.handleCancel(person(exportPerson), map[string]any{"exportId": draft}, 0))
	require.Equal(t, "cancelled", got["state"])
	_, sealed := f.row(t, draft)
	require.False(t, sealed.Valid)

	queued := f.prepare(t, acme, true)["id"].(string)
	f.start(t, queued)
	got = answer(t)(f.x.handleCancel(person(exportPerson), map[string]any{"exportId": queued}, 0))
	require.Equal(t, "cancelled", got["state"])
	_, sealed = f.row(t, queued)
	require.False(t, sealed.Valid)
	require.Len(t, f.exec.audits("account_export_cancelled"), 2)

	_, err := f.x.handleCancel(person(exportPerson), map[string]any{"exportId": queued}, 0)
	require.Error(t, err, "a cancelled export cannot be cancelled again")
}

func TestExportRemoveDeletesTheFileAndKeepsTheRecord(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	id := f.prepare(t, acme, false)["id"].(string)
	f.start(t, id)
	key := "exports/accounts/" + id + "/1/file"
	f.blobs.content[key] = []byte("0123456789")
	r := f.runner(t, "agent-a", func(context.Context, Export, string, func(ExportProgress)) (ExportResult, error) {
		return ExportResult{ObjectKey: key, FileName: "acme.tar.gz", SizeBytes: 10, SHA256: "s"}, nil
	})
	r.tick(context.Background())
	f.waitState(t, id, "complete")
	r.wait()

	// The download route's three methods.
	x, ok, err := f.x.ResolveExportForDownload(context.Background(), id)
	require.NoError(t, err)
	require.True(t, ok)
	body, err := f.x.OpenExport(context.Background(), x, 3, -1)
	require.NoError(t, err)
	got, _ := io.ReadAll(body)
	require.Equal(t, "3456789", string(got))
	body, err = f.x.OpenExport(context.Background(), x, 2, 3)
	require.NoError(t, err)
	got, _ = io.ReadAll(body)
	require.Equal(t, "234", string(got))
	require.NoError(t, f.x.RecordExportDownload(person(exportPerson), x, exportPerson, "bytes=2-4"))
	downloads := f.exec.audits("account_export_downloaded")
	require.Len(t, downloads, 1)
	require.Contains(t, downloads[0], "bytes=2-4")
	require.Contains(t, downloads[0], `"data"`)
	require.Contains(t, downloads[0], `"accountExport"`)

	removed := answer(t)(f.x.handleRemove(person(exportPerson), map[string]any{"exportId": id}, 0))
	require.Equal(t, "complete", removed["state"])
	require.NotEmpty(t, removed["removedAt"])
	require.Equal(t, exportPerson, removed["removedBy"])
	require.Equal(t, []string{key}, f.blobs.deleted)
	require.Len(t, f.exec.audits("account_export_removed"), 1)

	_, ok, err = f.x.ResolveExportForDownload(context.Background(), id)
	require.NoError(t, err)
	require.False(t, ok, "a removed file is not served")
	_, err = f.x.handleRemove(person(exportPerson), map[string]any{"exportId": id}, 0)
	require.Error(t, err, "a removed file cannot be removed twice")

	// A draft is not served either.
	draft := f.prepare(t, acme, false)["id"].(string)
	_, ok, err = f.x.ResolveExportForDownload(context.Background(), draft)
	require.NoError(t, err)
	require.False(t, ok)
}

func TestExportDraftIsSweptAfterFifteenMinutes(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	globex := f.account(t, "aex-globex", "Globex")
	old := f.prepare(t, acme, true)["id"].(string)
	fresh := f.prepare(t, globex, true)["id"].(string)
	_, err := f.db.ExecContext(context.Background(), `UPDATE account_exports SET requested_at = clock_timestamp() - interval '16 minutes' WHERE id = ?`, old)
	require.NoError(t, err)
	require.NoError(t, f.x.sweepDrafts(context.Background()))
	_, ok, err := f.x.get(context.Background(), old)
	require.NoError(t, err)
	require.False(t, ok)
	_, ok, err = f.x.get(context.Background(), fresh)
	require.NoError(t, err)
	require.True(t, ok)
	_, err = f.x.handleStart(person(exportPerson), map[string]any{"exportId": old}, 0)
	require.Error(t, err)
}

func TestExportListIsNewestFirstAndPaged(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	var ids []string
	for i := 0; i < 3; i++ {
		id := f.prepare(t, acme, false)["id"].(string)
		_, err := f.db.ExecContext(context.Background(), `UPDATE account_exports SET state = 'complete', requested_at = clock_timestamp() - make_interval(mins => ?) WHERE id = ?`, 10-i, id)
		require.NoError(t, err)
		ids = append(ids, id)
	}
	nodes, err := f.x.handleList(person(exportPerson), map[string]any{"accountId": acme, "limit": float64(2)}, 0)
	require.NoError(t, err)
	page := exportPayloads(t, nodes)
	require.Len(t, page, 3)
	require.Equal(t, ids[2], page[0]["id"])
	require.Equal(t, ids[1], page[1]["id"])
	require.Equal(t, "page", page[2]["kind"])
	cursor := page[2]["nextCursor"].(string)
	require.NotEmpty(t, cursor)
	nodes, err = f.x.handleList(person(exportPerson), map[string]any{"accountId": acme, "limit": 2, "after": cursor}, 0)
	require.NoError(t, err)
	page = exportPayloads(t, nodes)
	require.Len(t, page, 2)
	require.Equal(t, ids[0], page[0]["id"])
	require.Equal(t, "", page[1]["nextCursor"])
}

// ---------------------------------------------------------------------------
// The person gate, the owners' email, and the passphrase in nothing

func TestExportActsNeedAPersonsOwnSession(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	id := f.prepare(t, acme, false)["id"].(string)
	notPeople := map[string]context.Context{
		"system":   auth.ContextWithSystemActor(context.Background(), "accounts"),
		"borrowed": auth.ContextWithUserActor(context.Background(), exportPerson),
		"pat": auth.ContextWithAccess(auth.ContextWithClaims(context.Background(), map[string]any{"sub": exportPerson, "pat_id": "p"}),
			&auth.AccessContext{UserId: exportPerson, Role: auth.RoleOwner}),
	}
	acts := map[string]func(context.Context) error{
		"prepare": func(ctx context.Context) error {
			_, err := f.x.handlePrepare(ctx, map[string]any{"accountId": "aex-globex"}, 0)
			return err
		},
		"start": func(ctx context.Context) error {
			_, err := f.x.handleStart(ctx, map[string]any{"exportId": id}, 0)
			return err
		},
		"cancel": func(ctx context.Context) error {
			_, err := f.x.handleCancel(ctx, map[string]any{"exportId": id}, 0)
			return err
		},
		"remove": func(ctx context.Context) error {
			_, err := f.x.handleRemove(ctx, map[string]any{"exportId": id}, 0)
			return err
		},
	}
	for who, ctx := range notPeople {
		for act, call := range acts {
			err := call(ctx)
			require.Error(t, err, "%s by %s", act, who)
			require.True(t, auth.IsPersonSessionRequired(err), "%s by %s: %v", act, who, err)
		}
	}
	x, _ := f.row(t, id)
	require.Equal(t, "draft", x.State)

	// The reads need only the capability, which the engine checks before them.
	_, err := f.x.handleStatus(auth.ContextWithSystemActor(context.Background(), "accounts"), map[string]any{"exportId": id}, 0)
	require.NoError(t, err)
}

type capturingSender struct {
	mu   sync.Mutex
	sent []email.Message
	fail bool
}

func (c *capturingSender) Send(_ context.Context, m email.Message, _ email.SendAs) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail {
		return errors.New("the mail service is down")
	}
	c.sent = append(c.sent, m)
	return nil
}

func TestExportStartMailsEveryOwnerWithNoPassphraseAndNoLink(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	sender := &capturingSender{}
	f.x.notifyOwners = newOwnerNotice(func(context.Context) ([]string, error) {
		return []string{"one@example.test", "two@example.test"}, nil
	}, func() email.Sender { return sender }, f.x.logger)
	prepared := f.prepare(t, acme, true)
	pass := prepared["passphrase"].(string)
	f.start(t, prepared["id"].(string))
	f.x.notices.Wait()
	require.Len(t, sender.sent, 2)
	require.ElementsMatch(t, []string{"one@example.test", "two@example.test"}, []string{sender.sent[0].To, sender.sent[1].To})
	for _, m := range sender.sent {
		body := m.Subject + m.TextBody + m.HTMLBody
		require.Contains(t, body, "Acme")
		require.Contains(t, body, "exporter@example.test")
		require.NotContains(t, body, pass)
		require.NotContains(t, body, "http")
		require.NotContains(t, body, "href")
	}

	// A failure to send is logged and the start stands.
	sender.fail = true
	globex := f.account(t, "aex-globex", "Globex")
	second := f.prepare(t, globex, false)
	started := f.start(t, second["id"].(string))
	require.Equal(t, "queued", started["state"])
	f.x.notices.Wait()
	require.Contains(t, f.logs.String(), "the mail service is down")
}

func TestExportPassphraseIsInNoLogLineAuditOrError(t *testing.T) {
	f := newExportFixture(t)
	acme := f.account(t, "aex-acme", "Acme")
	globex := f.account(t, "aex-globex", "Globex")
	var passes []string

	// Complete.
	p := f.prepare(t, acme, true)
	passes = append(passes, p["passphrase"].(string))
	f.start(t, p["id"].(string))
	r := f.runner(t, "agent-a", func(context.Context, Export, string, func(ExportProgress)) (ExportResult, error) {
		return ExportResult{ObjectKey: "exports/accounts/k", FileName: "f", SizeBytes: 1}, nil
	})
	r.tick(context.Background())
	f.waitState(t, p["id"].(string), "complete")
	r.wait()

	// Failed, by a work function careless enough to name it in its error.
	p = f.prepare(t, globex, true)
	pass := p["passphrase"].(string)
	passes = append(passes, pass)
	f.start(t, p["id"].(string))
	r = f.runner(t, "agent-a", func(_ context.Context, _ Export, passphrase string, _ func(ExportProgress)) (ExportResult, error) {
		return ExportResult{}, fmt.Errorf("could not seal with %s", passphrase)
	})
	r.tick(context.Background())
	x := f.waitState(t, p["id"].(string), "failed")
	r.wait()
	require.Equal(t, "export_failed", x.FailureCode)
	require.NotContains(t, x.FailureDetail, pass)

	// Cancelled from draft.
	p = f.prepare(t, globex, true)
	passes = append(passes, p["passphrase"].(string))
	_, err := f.x.handleCancel(person(exportPerson), map[string]any{"exportId": p["id"]}, 0)
	require.NoError(t, err)

	logs := f.logs.String()
	statements := strings.Join(f.exec.all(), "\n")
	require.NotEmpty(t, f.exec.audits("account_export_failed"))
	for _, pass := range passes {
		require.NotContains(t, logs, pass)
		require.NotContains(t, statements, pass)
		// Its last four characters are the fingerprint secret.Encrypt answers;
		// neither is kept anywhere.
		require.NotContains(t, statements, "..."+pass[len(pass)-4:])
	}
}

// answer is one for a call's two results.
func answer(t *testing.T) func([]memorynodes.MemoryNode, error) map[string]any {
	return func(nodes []memorynodes.MemoryNode, err error) map[string]any {
		t.Helper()
		return one(t, nodes, err)
	}
}
