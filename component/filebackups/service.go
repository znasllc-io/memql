// Package filebackups implements bounded storage effects behind Files backups.
// Job identity, authorization, byte ceilings, leases and receipts remain native;
// the worker selects and reconciles the explicitly requested local source.
package filebackups

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/component/database/storagequota"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/component/server"
	"github.com/znasllc-io/memql/integrations/azureblob"
)

type Provider interface {
	ReserveStorage(context.Context, string, string, int64) error
	PrepareDirect(context.Context, string, string, string) error
	FenceDirect(context.Context, string, string, string, string) error
	DirectBlockGrant(context.Context, string, string, string, string, int64, string) (azureblob.DirectRequest, error)
	CommitDirect(context.Context, string, string, string, []string, bool) (azureblob.DirectReceipt, error)
	UncommittedBlocks(context.Context, string, string) (map[string]int64, error)
	InspectDirect(context.Context, string, string, string) (azureblob.DirectInventory, error)
	DirectDownloadGrant(context.Context, string, string, string, int64, int64) (azureblob.DirectRequest, error)
	RetireDirect(context.Context, string, string, string) error
	StorageUsage(context.Context) (storagequota.Usage, error)
}

type Service struct {
	DB        func() *sql.DB
	GateDB    func() *sql.DB
	Provider  Provider
	Container string
	Library   *server.EngineLibraryStore
	Engine    server.MemQLExecutor
}

type Metadata struct {
	WatchID          string `json:"watchId,omitempty"`
	FolderID         string `json:"folderId,omitempty"`
	FileID           string `json:"fileId,omitempty"`
	WorkerName       string `json:"workerName,omitempty"`
	ExpectedVersion  int    `json:"expectedVersion,omitempty"`
	SourceTransferID string `json:"sourceTransferId,omitempty"`
	MimeType         string `json:"mimeType,omitempty"`
	VersionNumber    int    `json:"versionNumber,omitempty"`
}

const checkpointBlocks = 16

type Transfer struct {
	CheckpointBlocks int       `json:"checkpointBlocks"`
	ID               string    `json:"id"`
	Owner            string    `json:"-"`
	WorkerID         string    `json:"workerId"`
	Path             string    `json:"path"`
	Kind             string    `json:"kind"`
	Name             string    `json:"name"`
	Size             int64     `json:"size"`
	SHA256           string    `json:"sha256"`
	Object           string    `json:"-"`
	State            string    `json:"state"`
	Desired          string    `json:"desiredState"`
	Lease            string    `json:"-"`
	NextLease        string    `json:"-"`
	Committed        int64     `json:"committedBytes"`
	ETag             string    `json:"etag"`
	Metadata         Metadata  `json:"metadata"`
	Failure          string    `json:"failure"`
	Updated          time.Time `json:"updatedAt"`
}

const columns = `transfer_id,owner_id,worker_id,source_path,source_kind,name,size_bytes,source_sha256,object_key,state,desired_state,lease_id,next_lease_id,committed_bytes,etag,metadata,failure,updated_at`

type scanner interface{ Scan(...any) error }

func scanTransfer(row scanner) (Transfer, error) {
	t := Transfer{CheckpointBlocks: checkpointBlocks}
	var meta []byte
	err := row.Scan(&t.ID, &t.Owner, &t.WorkerID, &t.Path, &t.Kind, &t.Name, &t.Size, &t.SHA256, &t.Object, &t.State, &t.Desired, &t.Lease, &t.NextLease, &t.Committed, &t.ETag, &meta, &t.Failure, &t.Updated)
	if err == nil {
		err = json.Unmarshal(meta, &t.Metadata)
	}
	return t, err
}

var validID = regexp.MustCompile(`^[a-zA-Z0-9_-]{6,64}$`)

func principal(ctx context.Context, write bool) (string, error) {
	ac, ok := auth.AccessFromContext(ctx)
	if !ok || ac == nil || ac.UserId == "" || ac.Synthetic || ac.IsAnonymous || ac.ConnectorName != "" {
		return "", errors.New("sign in to manage backups")
	}
	if write {
		subject, ok := auth.SubjectFromContext(ctx)
		if !ok || !auth.CapableFor(ctx, subject, auth.VerbCreate, auth.ResourceData) {
			return "", errors.New("you do not have permission to write files")
		}
	}
	return memql.BareShortId(ac.UserId), nil
}
func (s *Service) database() (*sql.DB, error) {
	if s.DB == nil || s.DB() == nil {
		return nil, errors.New("backup journal is unavailable")
	}
	return s.DB(), nil
}
func (s *Service) gate(ctx context.Context, id string) (func(), error) {
	if !validID.MatchString(id) {
		return nil, errors.New("invalid backup identifier")
	}
	if s.GateDB == nil || s.GateDB() == nil {
		return nil, errors.New("backup write gate is unavailable")
	}
	return memql.AcquireWriteGate(ctx, s.GateDB(), "file-backup:"+id)
}
func (s *Service) read(ctx context.Context, id, owner string) (Transfer, error) {
	db, err := s.database()
	if err != nil {
		return Transfer{}, err
	}
	t, err := scanTransfer(db.QueryRowContext(ctx, "SELECT "+columns+" FROM file_backup_transfers WHERE transfer_id=$1 AND owner_id=$2", id, owner))
	if errors.Is(err, sql.ErrNoRows) {
		return t, errors.New("backup not found")
	}
	return t, err
}
func (s *Service) save(ctx context.Context, t Transfer) error {
	db, err := s.database()
	if err != nil {
		return err
	}
	meta, err := json.Marshal(t.Metadata)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `UPDATE file_backup_transfers SET size_bytes=$3,source_sha256=$4,state=$5,desired_state=$6,lease_id=$7,next_lease_id=$8,committed_bytes=$9,etag=$10,metadata=$11::jsonb,failure=$12,updated_at=clock_timestamp() WHERE transfer_id=$1 AND owner_id=$2`, t.ID, t.Owner, t.Size, t.SHA256, t.State, t.Desired, t.Lease, t.NextLease, t.Committed, t.ETag, string(meta), t.Failure)
	return err
}

func archiveName(p string) bool {
	p = strings.ToLower(p)
	return strings.HasSuffix(p, ".zip") || strings.HasSuffix(p, ".tar") || strings.HasSuffix(p, ".tar.gz") || strings.HasSuffix(p, ".tgz")
}
func sourcePath(p string) bool {
	p = strings.ReplaceAll(p, `\`, "/")
	return len(p) > 1 && len(p) <= 4096 && !strings.ContainsAny(p, "\x00\r\n") && (strings.HasPrefix(p, "/") || (len(p) > 3 && p[1] == ':' && p[2] == '/')) && !strings.Contains(p, "/../") && !strings.HasSuffix(p, "/..")
}
func digestValid(v string, n int) bool {
	b, e := hex.DecodeString(v)
	return e == nil && len(b) == n && hex.EncodeToString(b) == v
}

// Create records an immutable request before the worker touches its source.
// Retrying the same id must name exactly the same source, machine and mode.
func (s *Service) Create(ctx context.Context, id, workerID, p, kind string, meta Metadata) (Transfer, error) {
	owner, err := principal(ctx, true)
	if err != nil {
		return Transfer{}, err
	}
	meta.WatchID = memql.BareShortId(meta.WatchID)
	meta.FolderID = memql.BareShortId(meta.FolderID)
	meta.SourceTransferID = memql.BareShortId(meta.SourceTransferID)
	unlock, err := s.gate(ctx, id)
	if err != nil {
		return Transfer{}, err
	}
	defer unlock()
	if !validID.MatchString(workerID) || !sourcePath(p) || (kind != "archive" && kind != "sync_file" && kind != "download") {
		return Transfer{}, errors.New("select a machine and an absolute source path")
	}
	if kind == "archive" && !archiveName(p) {
		return Transfer{}, errors.New("select a ZIP, TAR, TAR.GZ or TGZ archive; select a folder to create a sync")
	}
	worker, err := s.Library.OwnedWorker(memql.ContextWithFreshRead(ctx), workerID)
	if err != nil {
		return Transfer{}, err
	}
	if worker == nil {
		return Transfer{}, errors.New("that machine is not yours")
	}
	db, err := s.database()
	if err != nil {
		return Transfer{}, err
	}
	if existing, err := scanTransfer(db.QueryRowContext(ctx, "SELECT "+columns+" FROM file_backup_transfers WHERE transfer_id=$1", id)); err == nil {
		if existing.Owner != owner || existing.WorkerID != workerID || existing.Path != p || existing.Kind != kind || existing.Metadata.WatchID != meta.WatchID || existing.Metadata.SourceTransferID != meta.SourceTransferID || existing.Metadata.FolderID != meta.FolderID || existing.Metadata.ExpectedVersion != meta.ExpectedVersion {
			return Transfer{}, errors.New("backup identity already names a different request")
		}
		return existing, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Transfer{}, err
	}
	meta.WorkerName = worker.Name
	if kind == "sync_file" {
		if err := s.checkSync(ctx, workerID, p, meta.WatchID); err != nil {
			return Transfer{}, err
		}
	}
	t := Transfer{CheckpointBlocks: checkpointBlocks, ID: id, Owner: owner, WorkerID: workerID, Path: p, Kind: kind, Name: path.Base(strings.ReplaceAll(p, `\`, "/")), State: "checking", Desired: "running", Lease: uuid.NewString(), Metadata: meta}
	t.Object = fmt.Sprintf("backups/%s/%s/%s", owner, id, t.Name)
	if kind == "sync_file" {
		t.Metadata.FileID = id
		t.Object = fmt.Sprintf("library/%s/%s/%s", owner, id, t.Name)
	}
	if kind == "download" {
		source, err := s.read(ctx, meta.SourceTransferID, owner)
		if err != nil {
			return Transfer{}, err
		}
		if source.Kind == "download" || source.State != "complete" {
			return Transfer{}, errors.New("only a completed backup can be downloaded")
		}
		t.Object, t.Size, t.SHA256, t.ETag, t.State = source.Object, source.Size, source.SHA256, source.ETag, "downloading"
	}
	payload, _ := json.Marshal(t.Metadata)
	_, err = db.ExecContext(ctx, `INSERT INTO file_backup_transfers(transfer_id,owner_id,worker_id,source_path,source_kind,name,size_bytes,source_sha256,object_key,state,desired_state,lease_id,etag,metadata) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14::jsonb)`, t.ID, owner, workerID, p, kind, t.Name, t.Size, t.SHA256, t.Object, t.State, t.Desired, t.Lease, t.ETag, string(payload))
	return t, err
}

func (s *Service) storageWorker(ctx context.Context, t Transfer) error {
	if s.Provider == nil {
		return errors.New("backup storage is unavailable")
	}
	if s.Library == nil {
		return errors.New("backup ownership is unavailable")
	}
	worker, err := s.Library.OwnedWorker(memql.ContextWithFreshRead(ctx), t.WorkerID)
	if err != nil {
		return err
	}
	if worker == nil {
		return errors.New("that machine is no longer yours")
	}
	return nil
}

func (s *Service) checkSync(ctx context.Context, worker, p, watchID string) error {
	root, err := s.Library.BackupDirectory(memql.ContextWithFreshRead(ctx), watchID, worker)
	if err != nil {
		return err
	}
	rows, err := s.execute(ctx, "query", "libraryWatchedFolders", map[string]any{"workerId": worker})
	if err != nil {
		return err
	}
	syncMode := false
	for _, row := range rows {
		if memql.BareShortId(fmt.Sprint(row["id"])) == memql.BareShortId(watchID) && row["mode"] == "sync" {
			syncMode = true
		}
	}
	if !syncMode {
		return errors.New("this folder has not been set to sync")
	}
	root = strings.TrimRight(strings.ReplaceAll(root, `\`, "/"), "/")
	p = strings.ReplaceAll(p, `\`, "/")
	if root == "" || !strings.HasPrefix(p, root+"/") || strings.Contains(p, "/../") || strings.Contains(p, "/./") {
		return errors.New("this file is outside an active folder sync")
	}
	return nil
}

func (s *Service) Prepare(ctx context.Context, id string, size int64, digest string) (Transfer, error) {
	owner, err := principal(ctx, true)
	if err != nil {
		return Transfer{}, err
	}
	unlock, err := s.gate(ctx, id)
	if err != nil {
		return Transfer{}, err
	}
	defer unlock()
	t, err := s.read(ctx, id, owner)
	if err != nil {
		return t, err
	}
	if err = s.storageWorker(ctx, t); err != nil {
		return t, err
	}
	if size < 0 || size > storagequota.BackupBytes || !digestValid(digest, 32) || t.Kind == "download" {
		return t, errors.New("a backup must be at most 1 TB and have a SHA-256 checksum")
	}
	if t.SHA256 != "" && (t.Size != size || t.SHA256 != digest) {
		return t, errors.New("the source changed; cancel this transfer before starting a new backup")
	}
	if t.State == "complete" || t.State == "uploading" || t.State == "paused" {
		return t, nil
	}
	if t.Desired != "running" || t.State == "cancelled" || t.State == "deleted" {
		return t, errors.New("this backup is not accepting bytes")
	}
	if t.Kind == "sync_file" {
		if err = s.checkSync(ctx, t.WorkerID, t.Path, t.Metadata.WatchID); err != nil {
			return t, err
		}
	}
	t.Size, t.SHA256, t.State = size, digest, "preparing"
	if err = s.save(ctx, t); err != nil {
		return t, err
	}
	if s.Provider == nil {
		return t, errors.New("backup storage is unavailable")
	}
	if err = s.directory(ctx, t, "reserve"); err != nil {
		return t, err
	}
	if err = s.Provider.ReserveStorage(ctx, s.Container, t.Object, size); err != nil {
		return t, err
	}
	if t.NextLease != "" {
		if err = s.fence(ctx, &t); err != nil {
			return t, err
		}
	}
	if err = s.Provider.PrepareDirect(ctx, s.Container, t.Object, t.Lease); err != nil {
		return t, err
	}
	t.State = "uploading"
	err = s.save(ctx, t)
	return t, err
}

func blockID(n int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("backup-v1-%08d", n)))
}
func blockCount(size int64) int {
	return int((size + storagequota.BlockBytes - 1) / storagequota.BlockBytes)
}
func blockSize(size int64, n int) int64 {
	if n < 0 || n >= blockCount(size) {
		return 0
	}
	return min(storagequota.BlockBytes, size-int64(n)*storagequota.BlockBytes)
}

func (s *Service) Grant(ctx context.Context, id string, n int, md5, digest string) (azureblob.DirectRequest, error) {
	owner, err := principal(ctx, true)
	if err != nil {
		return azureblob.DirectRequest{}, err
	}
	unlock, err := s.gate(ctx, id)
	if err != nil {
		return azureblob.DirectRequest{}, err
	}
	defer unlock()
	t, err := s.read(ctx, id, owner)
	if err != nil {
		return azureblob.DirectRequest{}, err
	}
	if err = s.storageWorker(ctx, t); err != nil {
		return azureblob.DirectRequest{}, err
	}
	if t.Kind == "download" || t.State != "uploading" || t.Desired != "running" || (n < blockCount(t.Committed) || n >= blockCount(t.Committed)+checkpointBlocks) || t.NextLease != "" {
		return azureblob.DirectRequest{}, errors.New("this backup is paused or its checkpoint needs recovery")
	}
	if t.Kind == "sync_file" {
		if err = s.checkSync(ctx, t.WorkerID, t.Path, t.Metadata.WatchID); err != nil {
			return azureblob.DirectRequest{}, err
		}
	}
	size := blockSize(t.Size, n)
	sum, err := base64.StdEncoding.DecodeString(md5)
	if size == 0 || err != nil || len(sum) != 16 || !digestValid(digest, 32) {
		return azureblob.DirectRequest{}, errors.New("invalid backup block")
	}
	db, _ := s.database()
	var oldSize int64
	var oldMD5, oldSHA string
	err = db.QueryRowContext(ctx, `SELECT size_bytes,md5,sha256 FROM file_backup_blocks WHERE transfer_id=$1 AND ordinal=$2`, id, n).Scan(&oldSize, &oldMD5, &oldSHA)
	if err == nil && (oldSize != size || oldMD5 != md5 || oldSHA != digest) {
		return azureblob.DirectRequest{}, errors.New("the source block changed")
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return azureblob.DirectRequest{}, err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO file_backup_blocks(transfer_id,ordinal,size_bytes,md5,sha256) VALUES($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, id, n, size, md5, digest)
	if err != nil {
		return azureblob.DirectRequest{}, err
	}
	return s.Provider.DirectBlockGrant(ctx, s.Container, t.Object, t.Lease, blockID(n), size, md5)
}

func (s *Service) fence(ctx context.Context, t *Transfer) error {
	if t.NextLease == "" {
		t.NextLease = uuid.NewString()
		if err := s.save(ctx, *t); err != nil {
			return err
		}
	}
	if err := s.Provider.FenceDirect(ctx, s.Container, t.Object, t.Lease, t.NextLease); err != nil {
		return err
	}
	t.Lease, t.NextLease = t.NextLease, ""
	return s.save(ctx, *t)
}

// checkpoint commits an ordered durable prefix after fencing all prior grants.
// It never assumes an HTTP response was received by the machine.
func (s *Service) checkpoint(ctx context.Context, t *Transfer, final bool) error {
	if s.Provider == nil {
		return errors.New("backup storage is unavailable")
	}
	if err := s.fence(ctx, t); err != nil {
		return err
	}
	inventory, err := s.Provider.InspectDirect(ctx, s.Container, t.Object, t.Lease)
	if err != nil {
		return err
	}
	count := len(inventory.Committed)
	if count < blockCount(t.Committed) || count > blockCount(t.Committed)+checkpointBlocks {
		return errors.New("stored checkpoint differs from the transfer journal")
	}
	for n, b := range inventory.Committed {
		if b.ID != blockID(n) || b.Size != blockSize(t.Size, n) {
			return errors.New("stored checkpoint has an unexpected block")
		}
	}
	for count < blockCount(t.Committed)+checkpointBlocks {
		n := blockSize(t.Size, count)
		if n == 0 || inventory.Staged[blockID(count)] != n {
			break
		}
		count++
	}
	ids := make([]string, count)
	for n := range ids {
		ids[n] = blockID(n)
	}
	// Empty files have zero blocks and still get a pinned, immutable receipt.
	if final && count != blockCount(t.Size) {
		return errors.New("the backup is missing blocks")
	}
	receipt, err := s.Provider.CommitDirect(ctx, s.Container, t.Object, t.Lease, ids, final)
	if err != nil {
		return err
	}
	expected := min(t.Size, int64(count)*storagequota.BlockBytes)
	if receipt.Size != expected || receipt.ETag == "" {
		return errors.New("storage returned an inconsistent backup receipt")
	}
	t.Committed, t.ETag = receipt.Size, receipt.ETag
	return s.save(ctx, *t)
}

func (s *Service) Checkpoint(ctx context.Context, id string, final bool) (Transfer, error) {
	owner, err := principal(ctx, true)
	if err != nil {
		return Transfer{}, err
	}
	unlock, err := s.gate(ctx, id)
	if err != nil {
		return Transfer{}, err
	}
	defer unlock()
	t, err := s.read(ctx, id, owner)
	if err != nil {
		return t, err
	}
	if t.State == "complete" {
		return t, nil
	}
	if t.Kind == "download" || t.Desired != "running" || t.State != "uploading" {
		return t, errors.New("this backup is not uploading")
	}
	if err = s.checkpoint(ctx, &t, final); err != nil {
		return t, err
	}
	if final {
		if t.Kind == "sync_file" {
			if err = s.publishSyncFile(ctx, &t); err != nil {
				return t, err
			}
		}
		if err = s.directory(ctx, t, "complete"); err != nil {
			return t, err
		}
		t.State = "complete"
		t.Failure = ""
		err = s.save(ctx, t)
	}
	return t, err
}

func (s *Service) Control(ctx context.Context, id, action string) (Transfer, error) {
	owner, err := principal(ctx, true)
	if err != nil {
		return Transfer{}, err
	}
	unlock, err := s.gate(ctx, id)
	if err != nil {
		return Transfer{}, err
	}
	defer unlock()
	t, err := s.read(ctx, id, owner)
	if err != nil {
		return t, err
	}
	switch action {
	case "pause":
		if t.State == "complete" || t.State == "bin" || t.State == "deleted" || t.State == "cancelled" {
			return t, errors.New("this backup cannot be paused")
		}
		t.Desired = "paused"
		if err = s.save(ctx, t); err != nil {
			return t, err
		}
		unreserved, checkErr := s.unreserved(ctx, t)
		if checkErr != nil {
			return t, checkErr
		}
		if t.Kind != "download" && t.SHA256 != "" && !unreserved {
			if s.Provider == nil {
				return t, errors.New("backup storage is unavailable")
			}
			if t.NextLease != "" {
				if err = s.fence(ctx, &t); err != nil {
					return t, err
				}
			}
			if err = s.Provider.PrepareDirect(ctx, s.Container, t.Object, t.Lease); err != nil {
				return t, err
			}
			if err = s.checkpoint(ctx, &t, false); err != nil {
				return t, err
			}
		}
		t.State = "paused"
	case "resume":
		if t.State != "paused" && t.State != "waiting" && t.State != "error" && t.Desired != "paused" {
			return t, errors.New("this backup is not paused")
		}
		t.Desired = "running"
		t.Failure = ""
		if t.Kind == "download" {
			t.State = "downloading"
		} else if t.SHA256 == "" {
			t.State = "checking"
		} else {
			t.State = "preparing"
		}
	case "cancel", "purge":
		if t.State == "cancelled" || t.State == "deleted" {
			return t, nil
		}
		if action == "purge" && (t.State != "bin" || t.Kind != "archive") {
			return t, errors.New("move this backup to the Bin before purging")
		}
		if action == "cancel" && (t.State == "complete" || t.State == "bin") {
			return t, errors.New("completed backups must be moved to the Bin")
		}
		t.Desired = "cancelled"
		if err = s.save(ctx, t); err != nil {
			return t, err
		}
		unreserved, checkErr := s.unreserved(ctx, t)
		if checkErr != nil {
			return t, checkErr
		}
		if t.Kind != "download" && t.SHA256 != "" && !unreserved {
			if s.Provider == nil {
				return t, errors.New("backup storage is unavailable")
			}
			if t.NextLease != "" {
				if err = s.fence(ctx, &t); err != nil {
					return t, err
				}
			}
			if err = s.Provider.PrepareDirect(ctx, s.Container, t.Object, t.Lease); err != nil {
				return t, err
			}
			if err = s.fence(ctx, &t); err != nil {
				return t, err
			}
			if err = s.Provider.RetireDirect(ctx, s.Container, t.Object, t.Lease); err != nil {
				return t, err
			}
		}
		if err = s.directory(ctx, t, "cancel"); err != nil {
			return t, err
		}
		t.State = "cancelled"
		if action == "purge" {
			t.State = "deleted"
		}
	case "trash":
		if t.State != "complete" || t.Kind != "archive" {
			return t, errors.New("only a completed archive can move to the Bin")
		}
		t.State = "bin"
	case "restore":
		if t.State != "bin" || t.Kind != "archive" {
			return t, errors.New("this backup is not in the Bin")
		}
		t.State = "complete"
	default:
		return t, errors.New("unknown backup action")
	}
	err = s.save(ctx, t)
	return t, err
}

func (s *Service) List(ctx context.Context, workerID, after, scope string) ([]Transfer, error) {
	owner, err := principal(ctx, false)
	if err != nil {
		return nil, err
	}
	db, err := s.database()
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, "SELECT "+columns+` FROM file_backup_transfers WHERE owner_id=$1 AND ($2='' OR worker_id=$2) AND ($3='' OR (created_at,transfer_id)<(SELECT created_at,transfer_id FROM file_backup_transfers WHERE transfer_id=$3 AND owner_id=$1)) AND state NOT IN ('deleted','cancelled') AND ($4!='archives' OR source_kind!='sync_file') AND ($4!='active' OR state NOT IN ('complete','bin')) ORDER BY created_at DESC, transfer_id DESC LIMIT 101`, owner, workerID, after, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Transfer{}
	for rows.Next() {
		t, e := scanTransfer(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

type DownloadBlock struct {
	Request azureblob.DirectRequest `json:"request"`
	SHA256  string                  `json:"sha256"`
	Size    int64                   `json:"size"`
	Offset  int64                   `json:"offset"`
}

func (s *Service) DownloadGrant(ctx context.Context, id string, n int) (DownloadBlock, error) {
	owner, err := principal(ctx, false)
	if err != nil {
		return DownloadBlock{}, err
	}
	t, err := s.read(ctx, id, owner)
	if err != nil {
		return DownloadBlock{}, err
	}
	if err = s.storageWorker(ctx, t); err != nil {
		return DownloadBlock{}, err
	}
	if t.Kind != "download" || t.State != "downloading" || t.Desired != "running" {
		return DownloadBlock{}, errors.New("this download is not running")
	}
	source, err := s.read(ctx, t.Metadata.SourceTransferID, owner)
	if err != nil {
		return DownloadBlock{}, err
	}
	if source.State != "complete" || source.ETag != t.ETag {
		return DownloadBlock{}, errors.New("the stored backup is no longer available at this version")
	}
	size := blockSize(t.Size, n)
	if size == 0 {
		return DownloadBlock{}, errors.New("invalid download block")
	}
	db, _ := s.database()
	var digest string
	if err = db.QueryRowContext(ctx, `SELECT sha256 FROM file_backup_blocks WHERE transfer_id=$1 AND ordinal=$2`, source.ID, n).Scan(&digest); err != nil {
		return DownloadBlock{}, err
	}
	offset := int64(n) * storagequota.BlockBytes
	request, err := s.Provider.DirectDownloadGrant(ctx, s.Container, t.Object, t.ETag, offset, size)
	return DownloadBlock{Request: request, SHA256: digest, Size: size, Offset: offset}, err
}

func (s *Service) Report(ctx context.Context, id, state, message string, offset int64) (Transfer, error) {
	owner, err := principal(ctx, true)
	if err != nil {
		return Transfer{}, err
	}
	unlock, err := s.gate(ctx, id)
	if err != nil {
		return Transfer{}, err
	}
	defer unlock()
	t, err := s.read(ctx, id, owner)
	if err != nil {
		return t, err
	}
	if len(message) > 2048 {
		return t, errors.New("backup message is too long")
	}
	if t.Desired != "running" || t.State == "complete" || t.State == "bin" || t.State == "deleted" || t.State == "cancelled" {
		return t, nil
	}
	switch state {
	case "waiting", "error":
		t.State = state
		t.Failure = message
	case "downloading", "complete":
		if t.Kind != "download" || offset < t.Committed || offset > t.Size || (state == "complete" && offset != t.Size) {
			return t, errors.New("invalid verified download progress")
		}
		t.Committed = offset
		t.State = state
		t.Failure = ""
	default:
		return t, errors.New("invalid backup progress state")
	}
	err = s.save(ctx, t)
	return t, err
}

// Failed admission may have recorded the source before reserving any capacity.
// A provider proof that no reservation or object exists permits pause/cancel
// without creating a new tombstone that the user's zero allowance would refuse.
func (s *Service) unreserved(ctx context.Context, t Transfer) (bool, error) {
	if t.Kind == "download" || t.SHA256 == "" {
		return true, nil
	}
	p, ok := s.Provider.(interface {
		DirectUnreserved(context.Context, string, string) (bool, error)
	})
	if !ok {
		return false, nil
	}
	return p.DirectUnreserved(ctx, s.Container, t.Object)
}
