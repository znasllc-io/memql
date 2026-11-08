package pipelinerun

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/pipelinesteps"
)

// artifact_retention_legacy_db_test.go -- the scheduled enrollment of
// pre-receipt step logs (EnrollLegacyLogs) over a real Postgres: the list the
// 20261108150000 migration seeds, leased page by page across two replicas,
// and the receipts it writes retired afterwards by the ordinary sweep.

// legacyEnrollmentLibrary adopts as the Library store does in the one way the
// pass depends on: a verified adoption writes the receipt row, so the
// retention queue's trigger and the sweep see an ordinary receipted file.
// Fencing, retirement and Library cleanup are the retention fixture's.
type legacyEnrollmentLibrary struct {
	*artifactRetentionLibraryFixture
	failOnce, failAlways, kept map[string]bool
	afterAdopt                 func(file string)
	adopted                    []string
}

func newLegacyEnrollmentLibrary(l *artifactRetentionLibraryFixture) *legacyEnrollmentLibrary {
	return &legacyEnrollmentLibrary{artifactRetentionLibraryFixture: l, failOnce: map[string]bool{}, failAlways: map[string]bool{}, kept: map[string]bool{}}
}

func (l *legacyEnrollmentLibrary) AdoptLegacyRunLog(ctx context.Context, s pipelinesteps.RunFileReceiptScope, file string) (pipelinesteps.StoredFileReceipt, error) {
	// Owner actor, internal origin, and the producer's exact scope.
	l.check(ctx, s)
	l.adopted = append(l.adopted, file)
	switch {
	case l.failOnce[file]:
		delete(l.failOnce, file)
		return pipelinesteps.StoredFileReceipt{}, errors.New("object storage did not answer")
	case l.failAlways[file]:
		return pipelinesteps.StoredFileReceipt{}, errors.New("legacy log object does not match its Library record")
	case l.kept[file]:
		return pipelinesteps.StoredFileReceipt{}, fmt.Errorf("%w: the file was filed in a folder", pipelinesteps.ErrArtifactRetained)
	}
	sum := sha256.Sum256([]byte(s.OwnerUserID + "/" + file))
	intent := hex.EncodeToString(sum[:])
	identity, _ := json.Marshal(s)
	if _, err := l.db.Exec(`INSERT INTO pipeline_artifact_uploads(intent_id,owner_user_id,identity,file_id,object_key,size_bytes,generation,state)
VALUES($1,$2,$3::jsonb,$4,$5,11,1,'ready') ON CONFLICT (intent_id) DO NOTHING`, intent, s.OwnerUserID, string(identity), file, "legacy-enrollment-test/"+intent); err != nil {
		return pipelinesteps.StoredFileReceipt{}, err
	}
	if l.afterAdopt != nil {
		l.afterAdopt(file)
	}
	return pipelinesteps.StoredFileReceipt{IntentID: intent, FileID: file, OwnerUserID: s.OwnerUserID, WorkRunID: s.WorkRunID, StepKey: s.StepKey, Attempt: s.Attempt, Size: 11}, nil
}

// legacyEnrollmentDB is this test's own enrollment list, ahead of public on
// its search path. The shared test database's list also holds what its
// migration seeded from other packages' fixtures, which a claim of the least
// recently visited producer would otherwise reach. Receipts and the retention
// queue stay public, isolated by the harness's unique owner as every
// retention test isolates them.
func legacyEnrollmentDB(t *testing.T, admin *sql.DB) *sql.DB {
	t.Helper()
	schema := fmt.Sprintf("legacy_enrollment_%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE SCHEMA ` + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); err != nil {
			t.Error(err)
		}
	})
	if _, err := admin.Exec(`CREATE TABLE ` + schema + `.pipeline_legacy_log_enrollment (LIKE public.pipeline_legacy_log_enrollment INCLUDING ALL)`); err != nil {
		t.Fatal(err)
	}
	// One connection, so the session's search path is every statement's; the
	// driver's connection parameter would quote the list into one schema name.
	db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dbtest.DSN())))
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`SET search_path TO ` + schema + `, public`); err != nil {
		t.Fatal(err)
	}
	return db
}

func legacyEnrollmentHarness(t *testing.T) (*sarifHarness, *artifactRetentionLibraryFixture, *legacyEnrollmentLibrary, *sql.DB) {
	t.Helper()
	h, l, _ := artifactRetentionHarness(t, 0)
	db := legacyEnrollmentDB(t, h.db)
	lib := newLegacyEnrollmentLibrary(l)
	for _, integ := range []*Integration{h.i, h.peer} {
		integ.Configure(func(d *Deps) {
			d.ArtifactRetention = &ArtifactRetentionDependencies{Database: func() *sql.DB { return db }, Engine: h.engine, Library: lib}
		})
	}
	h.step["key"] = "tests.unit"
	return h, l, lib, db
}

func listLegacyLogs(t *testing.T, db *sql.DB, owner, work string, files ...string) {
	t.Helper()
	for _, file := range files {
		if _, err := db.Exec(`INSERT INTO pipeline_legacy_log_enrollment(file_id,owner_user_id,work_run_id,step_key) VALUES($1,$2,$3,'tests.unit')`, file, owner, work); err != nil {
			t.Fatal(err)
		}
	}
}

// legacyRows is the list as state[:tries][:leased] per file.
func legacyRows(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT file_id,state,tries,lease_id<>'' AND lease_until>clock_timestamp() FROM pipeline_legacy_log_enrollment`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var file, state string
		var tries int
		var leased bool
		if err := rows.Scan(&file, &state, &tries, &leased); err != nil {
			t.Fatal(err)
		}
		out[file] = fmt.Sprintf("%s:%d", state, tries)
		if leased {
			out[file] += ":leased"
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func sortedShortIDs(n int) []string {
	ids := make([]string, n)
	for k := range ids {
		ids[k] = id.NewShortId()
	}
	slices.Sort(ids)
	return ids
}

// TestLegacyLogEnrollmentReceiptsTheListAcrossReplicasThenRetentionRetiresIt:
// the measured shape (a cancelled step whose journal never named its log) is
// enrolled from the list alone; a file whose storage did not answer keeps its
// row with the try counted and is enrolled by another replica with none of the
// first one's state; and the receipts are then ordinary ones, retired by the
// scheduled sweep under the run's own window.
func TestLegacyLogEnrollmentReceiptsTheListAcrossReplicasThenRetentionRetiresIt(t *testing.T) {
	h, l, lib, db := legacyEnrollmentHarness(t)
	h.step["status"], h.step["logFileId"] = "cancelled", ""
	h.run.FinishedAt = testNow.Add(-10 * 24 * time.Hour)
	h.h.store.addRun(h.run)
	files := sortedShortIDs(3)
	listLegacyLogs(t, db, h.owner, h.run.WorkRunID, files...)
	lib.failOnce[files[1]] = true
	ctx := auth.ContextWithInternalOrigin(t.Context())

	if _, err := h.i.EnrollLegacyLogs(personCtx(h.owner)); err == nil || len(lib.adopted) != 0 {
		t.Fatal("a person reached the scheduled enrollment", err)
	}
	first, err := h.i.EnrollLegacyLogs(ctx)
	if err != nil || first["inspected"] != 3 || first["enrolled"] != 2 || first["retrying"] != 1 || first["refused"] != 0 {
		t.Fatalf("first pass %+v %v", first, err)
	}
	if got := legacyRows(t, db); len(got) != 1 || got[files[1]] != "pending:1" {
		t.Fatalf("after the first pass the list holds %v, want only the unanswered file, its try counted", got)
	}
	var reason string
	if err := db.QueryRow(`SELECT reason FROM pipeline_legacy_log_enrollment WHERE file_id=$1`, files[1]).Scan(&reason); err != nil || !strings.Contains(reason, "did not answer") {
		t.Fatalf("the failed try kept no reason: %q %v", reason, err)
	}
	second, err := h.peer.EnrollLegacyLogs(ctx)
	if err != nil || second["enrolled"] != 1 {
		t.Fatalf("the other replica's pass %+v %v", second, err)
	}
	if got := legacyRows(t, db); len(got) != 0 {
		t.Fatalf("a receipted file stayed listed: %v", got)
	}
	idle, err := h.i.EnrollLegacyLogs(ctx)
	if err != nil || idle["inspected"] != 0 {
		t.Fatalf("an empty list did work: %+v %v", idle, err)
	}

	prioritizeArtifactRetentionTest(t, h)
	swept, err := h.peer.SweepArtifacts(ctx)
	if err != nil || swept["purged"] != 3 || l.effects != 3 {
		t.Fatalf("retention did not retire the enrolled logs: %+v %v effects=%d", swept, err, l.effects)
	}
}

// TestLegacyLogEnrollmentWaitsOutADeadReplicasLeaseAndRefusesWhatItCannotProve:
// a row a vanished replica leased is left alone until its lease lapses, then
// taken by the next pass; a file its owner kept is refused at once, as
// retention would keep it; a file that never verifies is refused after its
// tries and is never adopted again.
func TestLegacyLogEnrollmentWaitsOutADeadReplicasLeaseAndRefusesWhatItCannotProve(t *testing.T) {
	h, _, lib, db := legacyEnrollmentHarness(t)
	files := sortedShortIDs(3)
	kept, unprovable, leased := files[0], files[1], files[2]
	listLegacyLogs(t, db, h.owner, h.run.WorkRunID, files...)
	lib.kept[kept], lib.failAlways[unprovable] = true, true
	if _, err := db.Exec(`UPDATE pipeline_legacy_log_enrollment SET lease_id='vanished-replica',lease_until=clock_timestamp()+interval '5 minutes' WHERE file_id=$1`, leased); err != nil {
		t.Fatal(err)
	}
	ctx := auth.ContextWithInternalOrigin(t.Context())

	first, err := h.i.EnrollLegacyLogs(ctx)
	if err != nil || first["inspected"] != 2 || first["refused"] != 1 || first["retrying"] != 1 || slices.Contains(lib.adopted, leased) {
		t.Fatalf("first pass %+v %v adopted %v", first, err, lib.adopted)
	}
	if got := legacyRows(t, db); got[kept] != "refused:1" || got[unprovable] != "pending:1" || got[leased] != "pending:0:leased" {
		t.Fatalf("after the first pass: %v", got)
	}
	if _, err := db.Exec(`UPDATE pipeline_legacy_log_enrollment SET lease_until=clock_timestamp()-interval '1 second' WHERE file_id=$1`, leased); err != nil {
		t.Fatal(err)
	}
	for pass := 2; pass <= legacyLogEnrollmentTries; pass++ {
		replica := h.i
		if pass%2 == 0 {
			replica = h.peer
		}
		if _, err := replica.EnrollLegacyLogs(ctx); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}
	got := legacyRows(t, db)
	if got[kept] != "refused:1" || got[unprovable] != fmt.Sprintf("refused:%d", legacyLogEnrollmentTries) || got[leased] != "" {
		t.Fatalf("after %d passes: %v", legacyLogEnrollmentTries, got)
	}
	tries := len(lib.adopted)
	idle, err := h.peer.EnrollLegacyLogs(ctx)
	if err != nil || idle["inspected"] != 0 || len(lib.adopted) != tries {
		t.Fatalf("a refused file was tried again: %+v %v", idle, err)
	}
}

// TestLegacyLogEnrollmentDoesNotAdoptForAnUnfinishedProducer: the producer's
// journals are reread under its owner on every pass, and a run whose journal
// is still open, or a step still running, adopts nothing; each file's try is
// counted with the reason.
func TestLegacyLogEnrollmentDoesNotAdoptForAnUnfinishedProducer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		unwind func(h *sarifHarness)
		reason string
	}{
		{"journal still open", func(h *sarifHarness) { h.work["status"] = "running" }, "still active"},
		{"step still running", func(h *sarifHarness) { h.step["status"] = "running" }, "not terminal"},
		{"another owner's run", func(h *sarifHarness) { h.work["ownerUserId"] = otherID }, "owner-scoped work journal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _, lib, db := legacyEnrollmentHarness(t)
			tc.unwind(h)
			files := sortedShortIDs(2)
			listLegacyLogs(t, db, h.owner, h.run.WorkRunID, files...)
			out, err := h.i.EnrollLegacyLogs(auth.ContextWithInternalOrigin(t.Context()))
			if err != nil || out["retrying"] != 2 || len(lib.adopted) != 0 {
				t.Fatalf("pass %+v %v adopted %v", out, err, lib.adopted)
			}
			var reason string
			if err := db.QueryRow(`SELECT reason FROM pipeline_legacy_log_enrollment WHERE file_id=$1`, files[0]).Scan(&reason); err != nil || !strings.Contains(reason, tc.reason) {
				t.Fatalf("reason %q %v, want it to say %q", reason, err, tc.reason)
			}
		})
	}
}

// TestLegacyLogEnrollmentResumesAPassThatStoppedAfterItsReceipt: a pass whose
// time runs out after a receipt committed leaves the row listed, unleased and
// uncounted, and the next replica resolves the same receipt and removes it.
func TestLegacyLogEnrollmentResumesAPassThatStoppedAfterItsReceipt(t *testing.T) {
	h, _, lib, db := legacyEnrollmentHarness(t)
	files := sortedShortIDs(2)
	listLegacyLogs(t, db, h.owner, h.run.WorkRunID, files...)
	ctx, stop := context.WithCancel(auth.ContextWithInternalOrigin(t.Context()))
	lib.afterAdopt = func(file string) {
		if file == files[0] {
			stop()
		}
	}
	if out, err := h.i.EnrollLegacyLogs(ctx); err != nil || out["enrolled"] != 0 {
		t.Fatalf("stopped pass %+v %v", out, err)
	}
	if got := legacyRows(t, db); got[files[0]] != "pending:0" || got[files[1]] != "pending:0" {
		t.Fatalf("a stopped pass kept its lease or counted a try: %v", got)
	}
	lib.afterAdopt = nil
	out, err := h.peer.EnrollLegacyLogs(auth.ContextWithInternalOrigin(t.Context()))
	if err != nil || out["enrolled"] != 2 {
		t.Fatalf("resumed pass %+v %v", out, err)
	}
	var receipts int
	if err := h.db.QueryRow(`SELECT count(*) FROM pipeline_artifact_uploads WHERE owner_user_id=$1`, h.owner).Scan(&receipts); err != nil || receipts != 2 {
		t.Fatalf("receipts %d %v, want one per file", receipts, err)
	}
	if got := legacyRows(t, db); len(got) != 0 {
		t.Fatalf("list after the resumed pass: %v", got)
	}
}
