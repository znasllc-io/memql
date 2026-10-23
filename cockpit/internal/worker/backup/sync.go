package backup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/znasllc-io/memql/cockpit/internal/worker/transfer"
	"github.com/znasllc-io/memql/core/id"
)

func (m *Manager) pushSync(ctx context.Context, w Watch, l *Ledger, e Entry) (bool, error) {
	prior, known := l.Get(e.Path)
	if prior.InBin {
		// A local re-creation is a new sync generation. Keep the old Bin copy;
		// if it was explicitly restored, use its exact recorded baseline below.
		remote, err := m.graph.FileAt(ctx, m.workerID, e.Path)
		if err != nil {
			return false, err
		}
		if remote == nil {
			prior = Record{}
		} else {
			prior.InBin = false
		}
		known = false
	}
	if known && prior.Stamp.Unchanged(e.Stamp) && prior.TransferID == "" {
		return false, nil
	}
	if m.opts.CheckPath == nil {
		return false, errors.New("no local backup permission")
	}
	if err := m.opts.CheckPath(e.Path); err != nil {
		return false, err
	}
	if prior.TransferID != "" {
		current, err := m.transfers.Read(ctx, prior.TransferID)
		if err != nil {
			return false, err
		}
		if current.State == "cancelled" || current.State == "deleted" {
			// Cancellation from Files pauses this watch first. A later explicit
			// watch resume starts a new generation with the original baseline.
			prior.TransferID = ""
			l.Put(e.Path, prior)
			if err = l.Save(); err != nil {
				return false, err
			}
		}
	}
	if prior.TransferID == "" {
		remote, err := m.graph.FileAt(ctx, m.workerID, e.Path)
		if err != nil {
			return false, err
		}
		expected := prior.VersionNumber
		if remote != nil {
			digest, err := Digest(e.Path)
			if err != nil {
				return false, err
			}
			remoteDigest := rowString(remote, "sha256")
			version, _ := strconv.Atoi(fmt.Sprint(rowField(remote, "versionNumber")))
			if version < 1 {
				version = 1
			}
			if remoteDigest == digest {
				e.Stamp.SHA256 = digest
				prior.Stamp = e.Stamp
				prior.FileID = rowString(remote, "id")
				prior.VersionNumber = version
				prior.LinkState = "synced"
				l.Put(e.Path, prior)
				return false, l.Save()
			}
			if prior.FileID == "" || prior.Stamp.SHA256 != remoteDigest || expected != version {
				return false, errors.New("the cloud copy differs from this machine's last synced version; resolve the conflict first")
			}
		} else if prior.FileID != "" {
			return false, errors.New("the cloud copy was removed; relink this path before syncing again")
		}
		prior.TransferID = string(id.NewShortId())
		l.Put(e.Path, prior)
		if err := l.Save(); err != nil {
			return false, err
		}
	}
	job, err := m.transfers.Create(ctx, transfer.Job{ID: prior.TransferID, WorkerID: m.workerID, Path: e.Path, Kind: "sync_file", Metadata: transfer.Metadata{WatchID: w.ID, FolderID: w.FolderID, ExpectedVersion: prior.VersionNumber}})
	if err != nil {
		return false, err
	}
	job, err = m.transfers.Run(ctx, job)
	if err != nil {
		return false, err
	}
	if job.State != "complete" {
		return false, transfer.ErrPaused
	}
	current, err := statOf(e.Path)
	if err != nil {
		return true, err
	}
	// Only the stamp observed before this upload may be marked synced. A later
	// source edit must be noticed by the next scan, not adopted without uploading.
	current = e.Stamp
	current.SHA256 = job.SHA256
	l.Put(e.Path, Record{Stamp: current, FileID: job.Metadata.FileID, VersionNumber: job.Metadata.VersionNumber, LinkState: "synced", PushedAtUnix: time.Now().Unix(), VerifiedAtUnix: time.Now().Unix()})
	return true, l.Save()
}

func (m *Manager) syncDeletions(ctx context.Context, w Watch, l *Ledger, scan ScanResult) {
	// A partial scan, lost drive, cancelled walk or permission refusal never
	// means deletion. Resolve every absent path again and require its parent.
	if scan.Skipped != 0 || ctx.Err() != nil {
		return
	}
	root, err := os.Stat(w.LocalPath)
	if err != nil || !root.IsDir() || scan.RootIdentity != "" && fileIdentity(root) != scan.RootIdentity {
		return
	}
	present := make(map[string]bool, len(scan.Entries))
	for _, e := range scan.Entries {
		present[e.Path] = true
	}
	for _, path := range l.Paths() {
		rec, _ := l.Get(path)
		if present[path] || rec.InBin || rec.FileID == "" || rec.TransferID != "" {
			continue
		}
		if m.opts.CheckPath == nil || m.opts.CheckPath(path) != nil {
			continue
		}
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			continue
		}
		// A disappeared nested mount is not a deleted directory. The closest
		// surviving ancestor must still be on the file's original filesystem.
		ancestor := filepath.Dir(path)
		sameDevice := false
		for {
			info, e := os.Stat(ancestor)
			if e == nil {
				sameDevice = strings.SplitN(fileIdentity(info), ":", 2)[0] == strings.SplitN(rec.Stamp.Identity, ":", 2)[0]
				break
			}
			if !errors.Is(e, fs.ErrNotExist) || ancestor == w.LocalPath || filepath.Dir(ancestor) == ancestor {
				break
			}
			ancestor = filepath.Dir(ancestor)
		}
		if !sameDevice {
			continue
		}
		// Root identity is checked again immediately before each cloud deletion.
		current, err := os.Stat(w.LocalPath)
		if err != nil || !os.SameFile(root, current) {
			return
		}
		err = m.transfers.Control.Call(ctx, "SyncDelete", map[string]any{"watchId": w.ID, "workerId": m.workerID, "path": path, "fileId": rec.FileID, "sha256": rec.Stamp.SHA256, "version": rec.VersionNumber}, nil)
		if err != nil {
			m.logger.Warn("sync: cloud copy kept", "error", err)
			continue
		}
		rec.InBin = true
		rec.LinkState = "origin_gone"
		l.Put(path, rec)
		if err = l.Save(); err != nil {
			m.logger.Warn("sync: deletion receipt could not be saved", "error", err)
			return
		}
	}
}
