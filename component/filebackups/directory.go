package filebackups

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/znasllc-io/memql/component/database/storagequota"
	"github.com/znasllc-io/memql/component/memql"
)

// A folder's live/pending logical footprint is independent of the physical
// ledger: superseded versions and Bin bytes continue to count there.
// staged-data: MUST-NOT-GATE -- capacity admission must see the latest watch
// pause/removal; hiding it could keep admitting bytes for a stopped folder.
func (s *Service) directory(ctx context.Context, t Transfer, action string) error {
	if t.Kind != "sync_file" {
		return nil
	}
	root, err := s.Library.BackupDirectory(memql.ContextWithFreshRead(ctx), t.Metadata.WatchID, t.WorkerID)
	if err != nil {
		return err
	}
	if action == "reserve" && root == "" {
		return errors.New("this folder sync is paused or stopped")
	}
	db, err := s.database()
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "backup-directory:"+t.Metadata.WatchID); err != nil {
		return err
	}
	if action == "reserve" {
		var owner, worker, storedRoot string
		err = tx.QueryRowContext(ctx, `SELECT owner_id,worker_id,root_path FROM file_backup_directories WHERE watch_id=$1`, t.Metadata.WatchID).Scan(&owner, &worker, &storedRoot)
		if errors.Is(err, sql.ErrNoRows) {
			if _, err = tx.ExecContext(ctx, `INSERT INTO file_backup_directories(watch_id,owner_id,worker_id,root_path) VALUES($1,$2,$3,$4)`, t.Metadata.WatchID, t.Owner, t.WorkerID, root); err != nil {
				return err
			}
			// Import existing retained-mode copies when an explicit sync is first used.
			// Current file rows, never historical versions or archived copies.
			_, err = tx.ExecContext(ctx, `INSERT INTO file_backup_directory_files(watch_id,path,file_id,live_bytes)
SELECT $1,payload->>'backupPath',regexp_replace(id,'^.*:',''),(payload->>'size')::bigint
FROM (SELECT DISTINCT ON(id) id,payload FROM "MemoryNodes" WHERE concept='v1:library:file' ORDER BY id,"createdAt" DESC) f
WHERE regexp_replace(payload->>'ownerUserId','^.*:','')=$2 AND regexp_replace(payload->>'backupWorkerId','^.*:','')=$3
AND starts_with(replace(payload->>'backupPath',chr(92),'/'),$4) AND COALESCE((payload->>'archived')::boolean,false)=false
ON CONFLICT(watch_id,path) DO NOTHING`, t.Metadata.WatchID, t.Owner, t.WorkerID, strings.TrimRight(strings.ReplaceAll(root, `\`, "/"), "/")+"/")
			if err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if owner != t.Owner || worker != t.WorkerID || storedRoot != root {
			return errors.New("the folder sync identity changed")
		}
		var live, pending int64
		var pendingID string
		err = tx.QueryRowContext(ctx, `SELECT live_bytes,pending_bytes,pending_id FROM file_backup_directory_files WHERE watch_id=$1 AND path=$2`, t.Metadata.WatchID, t.Path).Scan(&live, &pending, &pendingID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if pendingID != "" && pendingID != t.ID {
			return errors.New("this path already has an unfinished sync transfer")
		}
		var total int64
		if err = tx.QueryRowContext(ctx, `SELECT logical_bytes FROM file_backup_directories WHERE watch_id=$1`, t.Metadata.WatchID).Scan(&total); err != nil {
			return err
		}
		if max(live, t.Size)-max(live, pending) > storagequota.BackupBytes-total {
			return errors.New("a synced folder cannot exceed 1 TB")
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO file_backup_directory_files(watch_id,path,pending_id,pending_bytes) VALUES($1,$2,$3,$4) ON CONFLICT(watch_id,path) DO UPDATE SET pending_id=$3,pending_bytes=$4`, t.Metadata.WatchID, t.Path, t.ID, t.Size)
	} else if action == "complete" {
		_, err = tx.ExecContext(ctx, `UPDATE file_backup_directory_files SET live_bytes=$3,file_id=$4,pending_id='',pending_bytes=0 WHERE watch_id=$1 AND path=$2 AND pending_id=$5`, t.Metadata.WatchID, t.Path, t.Size, t.Metadata.FileID, t.ID)
	} else if action == "cancel" {
		_, err = tx.ExecContext(ctx, `UPDATE file_backup_directory_files SET pending_id='',pending_bytes=0 WHERE watch_id=$1 AND path=$2 AND pending_id=$3`, t.Metadata.WatchID, t.Path, t.ID)
	} else if action == "bin" {
		_, err = tx.ExecContext(ctx, `UPDATE file_backup_directory_files SET live_bytes=0 WHERE watch_id=$1 AND path=$2 AND file_id=$3`, t.Metadata.WatchID, t.Path, t.Metadata.FileID)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
