package filebackups

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun/driver/pgdriver"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/dbtest"
	"github.com/znasllc-io/memql/component/database/storagequota"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/integrations/azureblob"
)

type storageFixture struct {
	mu        sync.Mutex
	lease     string
	staged    map[string]int64
	committed []azureblob.DirectBlock
	retired   bool
	commits   []int
}

func (p *storageFixture) ReserveStorage(context.Context, string, string, int64) error { return nil }
func (p *storageFixture) PrepareDirect(_ context.Context, _, _, lease string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lease != "" && p.lease != lease {
		return errors.New("lease differs")
	}
	p.lease = lease
	return nil
}
func (p *storageFixture) FenceDirect(_ context.Context, _, _, old, next string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lease == next {
		return nil
	}
	if p.lease != old {
		return errors.New("stale lease")
	}
	p.lease = next
	return nil
}
func (p *storageFixture) DirectBlockGrant(_ context.Context, _, _, lease, id string, size int64, _ string) (azureblob.DirectRequest, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if lease != p.lease || p.retired {
		return azureblob.DirectRequest{}, errors.New("stale lease")
	}
	p.staged[id] = size
	return azureblob.DirectRequest{}, nil
}
func (p *storageFixture) InspectDirect(_ context.Context, _, _, lease string) (azureblob.DirectInventory, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if lease != p.lease {
		return azureblob.DirectInventory{}, errors.New("stale lease")
	}
	m := map[string]int64{}
	for k, v := range p.staged {
		m[k] = v
	}
	return azureblob.DirectInventory{Committed: append([]azureblob.DirectBlock(nil), p.committed...), Staged: m}, nil
}
func (p *storageFixture) UncommittedBlocks(context.Context, string, string) (map[string]int64, error) {
	return p.staged, nil
}
func (p *storageFixture) CommitDirect(_ context.Context, _, _, lease string, ids []string, _ bool) (azureblob.DirectReceipt, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if lease != p.lease {
		return azureblob.DirectReceipt{}, errors.New("stale lease")
	}
	sizes := map[string]int64{}
	for _, b := range p.committed {
		sizes[b.ID] = b.Size
	}
	for id, size := range p.staged {
		sizes[id] = size
	}
	var total int64
	blocks := []azureblob.DirectBlock{}
	for _, id := range ids {
		n, ok := sizes[id]
		if !ok {
			return azureblob.DirectReceipt{}, errors.New("missing block")
		}
		blocks = append(blocks, azureblob.DirectBlock{ID: id, Size: n})
		total += n
	}
	p.committed = blocks
	p.staged = map[string]int64{}
	p.commits = append(p.commits, len(ids))
	return azureblob.DirectReceipt{ETag: `"version"`, Size: total}, nil
}
func (p *storageFixture) DirectDownloadGrant(context.Context, string, string, string, int64, int64) (azureblob.DirectRequest, error) {
	return azureblob.DirectRequest{}, nil
}
func (p *storageFixture) RetireDirect(context.Context, string, string, string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.retired = true
	p.committed = nil
	p.staged = map[string]int64{}
	return nil
}
func (p *storageFixture) StorageUsage(context.Context) (storagequota.Usage, error) {
	return storagequota.Usage{}, nil
}

type workerEngine struct{ revoked bool }

func (w workerEngine) Execute(_ context.Context, q string) (any, error) {
	if strings.Contains(q, "worker") || strings.Contains(q, "Worker") {
		row := map[string]any{"id": "worker-001", "name": "Test computer", "ownerUserId": "owner-001"}
		if w.revoked {
			row["revokedAt"] = "2026-10-15T00:00:00Z"
		}
		return map[string]any{"data": []any{row}}, nil
	}
	return map[string]any{"data": []any{}}, nil
}
func serviceFixture(t *testing.T) (*Service, func() *Service, context.Context, *storageFixture) {
	t.Helper()
	dsn := os.Getenv("MEMQL_STORAGE_TEST_DSN")
	if dsn == "" {
		dsn = dbtest.DSN()
	}
	admin := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn)))
	t.Cleanup(func() { admin.Close() })
	pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	err := admin.PingContext(pingCtx)
	cancel()
	if err != nil {
		dbtest.Unreachable(t, "backup transfer journal", dsn, err)
	}
	schema := fmt.Sprintf("backups_%d", time.Now().UnixNano())
	if _, e := admin.Exec(`CREATE SCHEMA ` + schema); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`) })
	open := func() *sql.DB {
		db := sql.OpenDB(pgdriver.NewConnector(pgdriver.WithDSN(dsn), pgdriver.WithConnParams(map[string]any{"search_path": schema})))
		t.Cleanup(func() { db.Close() })
		return db
	}
	p := &storageFixture{staged: map[string]int64{}}
	makeService := func() *Service {
		db, gates := open(), open()
		s := &Service{DB: func() *sql.DB { return db }, GateDB: func() *sql.DB { return gates }, Container: "fixture", Provider: p, Engine: workerEngine{}}
		s.Library = server.NewEngineLibraryStore(s.Engine, s.GateDB)
		return s
	}
	s := makeService()
	migration, e := os.ReadFile("../database/memory-nodes/migrations/20261015010000_storage_capacity.up.sql")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.DB().Exec(string(migration)); e != nil {
		t.Fatal(e)
	}
	ctx := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "owner-001", Role: auth.RoleOwner})
	return s, makeService, ctx, p
}
func createFixtureJob(t *testing.T, s *Service, ctx context.Context, size int64) Transfer {
	t.Helper()
	j, e := s.Create(ctx, "backup-001", "worker-001", "/source/test.zip", "archive", Metadata{})
	if e != nil {
		t.Fatal(e)
	}
	j, e = s.Prepare(ctx, j.ID, size, strings.Repeat("a", 64))
	if e != nil {
		t.Fatal(e)
	}
	return j
}
func TestBackupLostDatabaseReceiptRecoversOnAnotherReplica(t *testing.T) {
	s, another, ctx, p := serviceFixture(t)
	j := createFixtureJob(t, s, ctx, 4)
	_, e := s.Grant(ctx, j.ID, 0, base64.StdEncoding.EncodeToString(make([]byte, 16)), strings.Repeat("b", 64))
	if e != nil {
		t.Fatal(e)
	}
	// Kill the database receipt after the storage effect: the next node has no
	// local transfer state and must adopt the provider's longer durable prefix.
	_, e = s.DB().Exec(`CREATE FUNCTION lose_receipt() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.committed_bytes>0 THEN RAISE EXCEPTION 'lost receipt'; END IF; RETURN NEW; END $$; CREATE TRIGGER fail_receipt BEFORE UPDATE ON file_backup_transfers FOR EACH ROW EXECUTE FUNCTION lose_receipt()`)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Checkpoint(ctx, j.ID, false); e == nil {
		t.Fatal("expected lost receipt")
	}
	if _, e = s.DB().Exec(`DROP TRIGGER fail_receipt ON file_backup_transfers`); e != nil {
		t.Fatal(e)
	}
	j, e = another().Checkpoint(ctx, j.ID, true)
	if e != nil {
		t.Fatal(e)
	}
	if j.Committed != 4 || j.State != "complete" {
		t.Fatalf("receipt %+v", j)
	}
	for _, n := range p.commits {
		if n != 1 {
			t.Fatal("recovery truncated committed bytes")
		}
	}
}
func TestBackupPauseResumeCancelAreDurableAndOwnerBound(t *testing.T) {
	s, another, ctx, p := serviceFixture(t)
	j := createFixtureJob(t, s, ctx, 4)
	old := p.lease
	j, e := s.Control(ctx, j.ID, "pause")
	if e != nil {
		t.Fatal(e)
	}
	if j.State != "paused" || p.lease == old {
		t.Fatal("pause did not fence outstanding grants")
	}
	if _, e = s.Grant(ctx, j.ID, 0, base64.StdEncoding.EncodeToString(make([]byte, 16)), strings.Repeat("b", 64)); e == nil {
		t.Fatal("paused grant accepted")
	}
	stranger := auth.ContextWithAccess(context.Background(), &auth.AccessContext{UserId: "other-001", Role: auth.RoleOwner})
	if _, e = another().Control(stranger, j.ID, "resume"); e == nil {
		t.Fatal("another owner changed a private backup")
	}
	j, e = another().Control(ctx, j.ID, "resume")
	if e != nil {
		t.Fatal(e)
	}
	j, e = another().Prepare(ctx, j.ID, 4, strings.Repeat("a", 64))
	if e != nil {
		t.Fatal(e)
	}
	j, e = another().Control(ctx, j.ID, "cancel")
	if e != nil {
		t.Fatal(e)
	}
	if !p.retired || j.State != "cancelled" {
		t.Fatal("cancel lacked a retirement receipt")
	}
	if _, e = s.Prepare(ctx, j.ID, 4, strings.Repeat("a", 64)); e == nil {
		t.Fatal("cancelled job resumed")
	}
}
func TestBackupLimitsAndZeroByteArchive(t *testing.T) {
	s, _, ctx, _ := serviceFixture(t)
	j, e := s.Create(ctx, "backup-001", "worker-001", "/source/test.zip", "archive", Metadata{})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Prepare(ctx, j.ID, storagequota.BackupBytes+1, strings.Repeat("a", 64)); e == nil {
		t.Fatal("1 TB ceiling bypassed")
	}
	if blockCount(storagequota.BackupBytes) > 50000 || blockSize(storagequota.BackupBytes, blockCount(storagequota.BackupBytes)-1) <= 0 {
		t.Fatal("virtual 1 TB layout does not fit provider limits")
	}
	j, e = s.Prepare(ctx, j.ID, 0, strings.Repeat("a", 64))
	if e != nil {
		t.Fatal(e)
	}
	j, e = s.Checkpoint(ctx, j.ID, true)
	if e != nil {
		t.Fatal(e)
	}
	if j.State != "complete" || j.Committed != 0 {
		t.Fatal(j)
	}
	b, _ := json.Marshal(j)
	if strings.Contains(string(b), "lease") || strings.Contains(string(b), "object_key") {
		t.Fatal("private capability escaped job projection")
	}
}

func TestPauseAfterLostUploadResponseFencesAndKeepsBytes(t *testing.T) {
	s, another, ctx, p := serviceFixture(t)
	j := createFixtureJob(t, s, ctx, 4)
	if _, err := s.Grant(ctx, j.ID, 0, base64.StdEncoding.EncodeToString(make([]byte, 16)), strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Report(ctx, j.ID, "waiting", "response interrupted", 0); err != nil {
		t.Fatal(err)
	}
	old := p.lease
	j, err := another().Control(ctx, j.ID, "pause")
	if err != nil {
		t.Fatal(err)
	}
	if j.State != "paused" || j.Committed != 4 || old == p.lease {
		t.Fatalf("pause lost the staged prefix or left its grant valid: %+v", j)
	}
}

func TestCancelRecoversLostFenceReceiptOnAnotherReplica(t *testing.T) {
	s, another, ctx, p := serviceFixture(t)
	j := createFixtureJob(t, s, ctx, 4)
	// The provider changed its lease but the SQL receipt was lost. The durable
	// next lease is the only state a different server can use to reconcile.
	j.NextLease = "97510126-212d-411c-99b4-a4494d028aff"
	if err := s.save(ctx, j); err != nil {
		t.Fatal(err)
	}
	if err := p.FenceDirect(ctx, s.Container, j.Object, j.Lease, j.NextLease); err != nil {
		t.Fatal(err)
	}
	j, err := another().Control(ctx, j.ID, "cancel")
	if err != nil {
		t.Fatal(err)
	}
	if !p.retired || j.State != "cancelled" || j.NextLease != "" {
		t.Fatalf("cancel did not recover its fence: %+v", j)
	}
	if _, err = another().Control(ctx, j.ID, "cancel"); err != nil {
		t.Fatal("cancel retry", err)
	}
}

func TestUnavailableStorageRefusesWithoutLosingPauseIntent(t *testing.T) {
	s, _, ctx, _ := serviceFixture(t)
	j := createFixtureJob(t, s, ctx, 4)
	s.Provider = nil
	if _, err := s.Grant(ctx, j.ID, 0, base64.StdEncoding.EncodeToString(make([]byte, 16)), strings.Repeat("b", 64)); err == nil {
		t.Fatal("missing provider accepted a grant")
	}
	if _, err := s.Control(ctx, j.ID, "pause"); err == nil {
		t.Fatal("missing provider claimed a completed pause")
	}
	j, err := s.read(ctx, j.ID, j.Owner)
	if err != nil {
		t.Fatal(err)
	}
	if j.Desired != "paused" {
		t.Fatal("lost durable pause intent")
	}
}

func TestCheckpointAdoptsSeveralStagedBlocksWithoutAllocatingLargeFiles(t *testing.T) {
	s, another, ctx, p := serviceFixture(t)
	size := storagequota.BlockBytes * 3
	j := createFixtureJob(t, s, ctx, size)
	digest := strings.Repeat("b", 64)
	md5 := base64.StdEncoding.EncodeToString(make([]byte, 16))
	for n := 0; n < 3; n++ {
		if _, err := s.Grant(ctx, j.ID, n, md5, digest); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Grant(ctx, j.ID, checkpointBlocks, md5, digest); err == nil {
		t.Fatal("grant escaped the checkpoint window")
	}
	j, err := another().Control(ctx, j.ID, "pause")
	if err != nil {
		t.Fatal(err)
	}
	if j.Committed != size || len(p.committed) != 3 {
		t.Fatal("pause failed to commit the full staged batch")
	}
	if _, err = another().Control(ctx, j.ID, "resume"); err != nil {
		t.Fatal(err)
	}
	if _, err = another().Prepare(ctx, j.ID, size, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	j, err = another().Checkpoint(ctx, j.ID, true)
	if err != nil || j.State != "complete" {
		t.Fatal("batch did not recover", err)
	}
}

func TestFolderCapacityCounterIncludesConcurrentPendingPaths(t *testing.T) {
	s, another, ctx, _ := serviceFixture(t)
	db := s.DB()
	if _, err := db.Exec(`INSERT INTO file_backup_directories(watch_id,owner_id,worker_id,root_path) VALUES('watch-001','owner-001','worker-001','/source')`); err != nil {
		t.Fatal(err)
	}
	// Virtual byte counts exercise the real constraint without creating files.
	if _, err := db.Exec(`INSERT INTO file_backup_directory_files(watch_id,path,live_bytes) VALUES('watch-001','/source/a',900000000000)`); err != nil {
		t.Fatal(err)
	}
	if _, err := another().DB().Exec(`INSERT INTO file_backup_directory_files(watch_id,path,pending_bytes,pending_id) VALUES('watch-001','/source/b',100000000001,'transfer-b')`); err == nil {
		t.Fatal("another replica exceeded the folder ceiling")
	}
	var total int64
	if err := db.QueryRowContext(ctx, `SELECT logical_bytes FROM file_backup_directories WHERE watch_id='watch-001'`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 900000000000 {
		t.Fatal("refused reservation changed the counter")
	}
	if _, err := db.Exec(`UPDATE file_backup_directory_files SET live_bytes=0 WHERE watch_id='watch-001'`); err != nil {
		t.Fatal(err)
	}
	if _, err := another().DB().Exec(`INSERT INTO file_backup_directory_files(watch_id,path,pending_bytes,pending_id) VALUES('watch-001','/source/b',1000000000000,'transfer-b')`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT logical_bytes FROM file_backup_directories WHERE watch_id='watch-001'`).Scan(&total); err != nil || total != 1000000000000 {
		t.Fatal("wrong pending total", total, err)
	}
}

func TestBackupFreshReplicaRefusesRevokedMachine(t *testing.T) {
	s, another, ctx, _ := serviceFixture(t)
	j := createFixtureJob(t, s, ctx, 4)
	revoked := another()
	revoked.Engine = workerEngine{revoked: true}
	revoked.Library = server.NewEngineLibraryStore(revoked.Engine, revoked.GateDB)
	if _, err := revoked.Grant(ctx, j.ID, 0, base64.StdEncoding.EncodeToString(make([]byte, 16)), strings.Repeat("b", 64)); err == nil {
		t.Fatal("revoked machine received a storage grant")
	}
	if _, err := revoked.Prepare(ctx, j.ID, 4, strings.Repeat("a", 64)); err == nil {
		t.Fatal("revoked machine prepared storage")
	}
}

type deniedAdmissionProvider struct{ *storageFixture }

func (p deniedAdmissionProvider) ReserveStorage(context.Context, string, string, int64) error {
	return storagequota.ErrLimit
}
func (p deniedAdmissionProvider) DirectUnreserved(context.Context, string, string) (bool, error) {
	return true, nil
}
func TestBackupDeniedAdmissionCanPauseAndCancelWithoutNewStorage(t *testing.T) {
	s, _, ctx, p := serviceFixture(t)
	s.Provider = deniedAdmissionProvider{p}
	j, err := s.Create(ctx, "backup-001", "worker-001", "/source/test.zip", "archive", Metadata{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Prepare(ctx, j.ID, 4, strings.Repeat("a", 64)); !errors.Is(err, storagequota.ErrLimit) {
		t.Fatal(err)
	}
	j, err = s.Control(ctx, j.ID, "pause")
	if err != nil || j.State != "paused" {
		t.Fatalf("pause: %+v %v", j, err)
	}
	j, err = s.Control(ctx, j.ID, "cancel")
	if err != nil || j.State != "cancelled" {
		t.Fatalf("cancel: %+v %v", j, err)
	}
	if p.lease != "" || p.retired {
		t.Fatal("failed admission created storage while cancelling")
	}
}
