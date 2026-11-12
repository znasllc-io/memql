package accounts

// internal_origin_db_test.go -- what this package sends the engine under
// internal origin.
//
// The repository's gate lists the package for three stamps
// (call_origin_conformance_test.go at the root): the export's audit event, the
// move's audit event, and the read of the owners' addresses for the email an
// export's start sends. Each is the argument of one Execute and is handed to
// nothing else. The gate counts the sites; these tests hold them to the two
// statements they open, and show that a caller the person gate refuses reaches
// the engine with no statement at all.

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/integrations/email"
)

const ownersRead = `query activeUsers(role: "owner")`

// originRecorder stands in for the engine: it records every statement with
// the origin it arrived under, and answers nothing.
type originRecorder struct {
	mu      sync.Mutex
	stamped []string
	plain   []string
}

func (r *originRecorder) Execute(ctx context.Context, query string) (*memql.ExecuteResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if auth.OriginFromContext(ctx) == auth.OriginInternal {
		r.stamped = append(r.stamped, query)
	} else {
		r.plain = append(r.plain, query)
	}
	return nil, nil
}

func (r *originRecorder) sent() (stamped, plain []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.stamped...), append([]string(nil), r.plain...)
}

func (r *originRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stamped, r.plain = nil, nil
}

// sortStamped counts the stamped statements by what they are, and fails on one
// that is neither the audit event nor the owners' read.
func sortStamped(t *testing.T, stamped []string) (audits, owners int) {
	t.Helper()
	for _, q := range stamped {
		switch {
		case strings.HasPrefix(q, "mutation createAuditEvent("):
			audits++
		case q == ownersRead:
			owners++
		default:
			t.Errorf("a statement that is neither the audit event nor the owners' read carried internal origin:\n%s", q)
		}
	}
	return audits, owners
}

func TestExportStampsOnlyItsAuditEventsAndTheOwnersRead(t *testing.T) {
	f := newExportFixture(t)
	rec := &originRecorder{}
	f.x.engine = rec
	sender := &capturingSender{}
	f.x.notifyOwners = newOwnerNotice(activeOwnerEmails(rec), func() email.Sender { return sender }, f.x.logger)
	acme := f.account(t, "aex-acme", "Acme")

	// A person prepares, starts and cancels one export...
	first := f.prepare(t, acme, false)["id"].(string)
	f.start(t, first)
	f.x.notices.Wait()
	_, err := f.x.handleCancel(person(exportPerson), map[string]any{"exportId": first}, 0)
	require.NoError(t, err)

	// ...then a second one that a node finishes, is downloaded and is removed.
	second := f.prepare(t, acme, false)["id"].(string)
	f.start(t, second)
	f.x.notices.Wait()
	key := "exports/accounts/" + second + "/1/file"
	f.blobs.content[key] = []byte("0123456789")
	r := f.runner(t, "agent-a", func(context.Context, Export, string, func(ExportProgress)) (ExportResult, error) {
		return ExportResult{ObjectKey: key, FileName: "acme.tar.gz", SizeBytes: 10, SHA256: "s"}, nil
	})
	r.tick(context.Background())
	f.waitState(t, second, "complete")
	r.wait()
	x, _ := f.row(t, second)
	require.NoError(t, f.x.RecordExportDownload(person(exportPerson), x, exportPerson, "bytes=0-"))
	_, err = f.x.handleRemove(person(exportPerson), map[string]any{"exportId": second}, 0)
	require.NoError(t, err)

	stamped, _ := rec.sent()
	audits, owners := sortStamped(t, stamped)
	// prepared, started and cancelled; prepared, started, finished, downloaded
	// and removed. Fewer would mean an act stopped writing its event, and the
	// test would then be holding nothing to its shape.
	require.Equal(t, 8, audits, "the audit events of eight acts:\n%s", strings.Join(stamped, "\n"))
	require.Equal(t, 2, owners, "one read of the owners for each start")
}

func TestARefusedExportCallerSendsTheEngineNothing(t *testing.T) {
	f := newExportFixture(t)
	rec := &originRecorder{}
	f.x.engine = rec
	f.x.notifyOwners = newOwnerNotice(activeOwnerEmails(rec), func() email.Sender { return &capturingSender{} }, f.x.logger)
	acme := f.account(t, "aex-acme", "Acme")
	id := f.prepare(t, acme, false)["id"].(string)
	rec.reset()

	notPeople := map[string]context.Context{
		"nobody":   context.Background(),
		"system":   auth.ContextWithSystemActor(context.Background(), "accounts"),
		"borrowed": auth.ContextWithUserActor(context.Background(), exportPerson),
		"pat": auth.ContextWithAccess(auth.ContextWithClaims(context.Background(), map[string]any{"sub": exportPerson, "pat_id": "p"}),
			&auth.AccessContext{UserId: exportPerson, Role: auth.RoleOwner}),
	}
	for who, ctx := range notPeople {
		for act, call := range map[string]func() error{
			"prepare": func() error {
				_, err := f.x.handlePrepare(ctx, map[string]any{"accountId": acme}, 0)
				return err
			},
			"start": func() error { _, err := f.x.handleStart(ctx, map[string]any{"exportId": id}, 0); return err },
			"cancel": func() error {
				_, err := f.x.handleCancel(ctx, map[string]any{"exportId": id}, 0)
				return err
			},
			"remove": func() error {
				_, err := f.x.handleRemove(ctx, map[string]any{"exportId": id}, 0)
				return err
			},
		} {
			err := call()
			require.Error(t, err, "%s by %s", act, who)
			require.True(t, auth.IsPersonSessionRequired(err), "%s by %s: %v", act, who, err)
		}
	}
	f.x.notices.Wait()
	stamped, plain := rec.sent()
	require.Empty(t, stamped, "a refused caller reached the engine under internal origin")
	require.Empty(t, plain, "a refused caller reached the engine")
}

func TestExportDownloadRecordKeepsARangeToTheLengthOfARealOne(t *testing.T) {
	f := newExportFixture(t)
	rec := &originRecorder{}
	f.x.engine = rec
	acme := f.account(t, "aex-acme", "Acme")
	id := f.prepare(t, acme, false)["id"].(string)
	x, _ := f.row(t, id)
	rec.reset()

	long := "bytes=0-" + strings.Repeat("9", 100_000)
	require.NoError(t, f.x.RecordExportDownload(person(exportPerson), x, exportPerson, long))
	stamped, _ := rec.sent()
	require.Len(t, stamped, 1)
	require.Contains(t, stamped[0], "bytes=0-999")
	require.Less(t, len(stamped[0]), 4_000, "the audit event carries the header as sent, whatever its length")

	// A real header is recorded whole.
	rec.reset()
	require.NoError(t, f.x.RecordExportDownload(person(exportPerson), x, exportPerson, "bytes=1048576-2097151"))
	stamped, _ = rec.sent()
	require.Len(t, stamped, 1)
	require.Contains(t, stamped[0], "bytes=1048576-2097151")
}

func TestMoveStampsOnlyItsAuditEvents(t *testing.T) {
	eng, db := moveEngine(t)
	f := newMvFixture(t, db)
	rec := &originRecorder{}
	inv := New(func() *bun.DB { return db }, memorynodes.List, false, nil)
	m := NewMoves(inv, rec, eng, nil)
	g := f.goal(t, db, "g", f.a, "stamp")
	rows := []any{map[string]any{"concept": "v1:work:goal", "id": g}}

	// A caller the person gate refuses reaches the engine with nothing.
	_, err := m.handleRowsMove(mvPAT(f.owner), map[string]any{"rows": rows, "toAccountId": f.b}, 0)
	require.Error(t, err)
	require.True(t, auth.IsPersonSessionRequired(err), "%v", err)
	_, err = m.handleConflictLeaveOut(mvPAT(f.owner), map[string]any{"concept": "v1:work:goal", "rowId": g, "conflict": "store_bound_twice"}, 0)
	require.Error(t, err)
	require.True(t, auth.IsPersonSessionRequired(err), "%v", err)
	stamped, plain := rec.sent()
	require.Empty(t, stamped)
	require.Empty(t, plain)
	require.Equal(t, f.a, mvNewestAccount(t, db, g))

	// A preview is no act and writes no event.
	_, err = m.handleRowsMove(mvRole(f.owner, "owner"), map[string]any{"rows": rows, "toAccountId": f.b, "preview": true}, 0)
	require.NoError(t, err)
	stamped, _ = rec.sent()
	require.Empty(t, stamped, "a preview wrote an audit event")

	// A person's move writes one, and it is the only statement under the stamp.
	_, err = m.handleRowsMove(mvRole(f.owner, "owner"), map[string]any{"rows": rows, "toAccountId": f.b, "note": "why"}, 0)
	require.NoError(t, err)
	require.Equal(t, f.b, mvNewestAccount(t, db, g))
	stamped, _ = rec.sent()
	audits, owners := sortStamped(t, stamped)
	require.Equal(t, 1, audits, "%v", stamped)
	require.Zero(t, owners)
}

func TestMoveRefusesANoteLongerThanItsLimit(t *testing.T) {
	eng, db := moveEngine(t)
	f := newMvFixture(t, db)
	g := f.goal(t, db, "g", f.a, "note")
	rows := []any{map[string]any{"concept": "v1:work:goal", "id": g}}

	// The note is stored once with every row moved, so its length is bounded
	// before anything is read. Counted in characters, not bytes.
	atLimit := strings.Repeat("é", moveNoteMax)
	for _, preview := range []bool{true, false} {
		_, err := mvCall(mvRole(f.owner, "owner"), eng, "accountRowsMove",
			map[string]any{"rows": rows, "toAccountId": f.b, "note": atLimit + "x", "preview": preview})
		require.Error(t, err, "preview=%v", preview)
		require.Contains(t, err.Error(), "A note is at most")
	}
	require.Equal(t, f.a, mvNewestAccount(t, db, g))
	require.Empty(t, mvLedger(t, db, g))

	mvMust(t, mvRole(f.owner, "owner"), eng, "accountRowsMove", map[string]any{"rows": rows, "toAccountId": f.b, "note": atLimit})
	require.Equal(t, f.b, mvNewestAccount(t, db, g))
}
