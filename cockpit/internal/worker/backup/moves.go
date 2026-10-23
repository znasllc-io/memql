package backup

import (
	"context"
	"errors"
	"io/fs"
	"os"
)

// followMoves uses both native device/inode identity AND content identity,
// restricted to a single authorized watch. Names and equal contents alone do
// not prove a move. Hard-link ambiguity and moves outside the watch are left
// alone: the durable old copy remains, and explicit relinking is required.
func (m *Manager) followMoves(ctx context.Context, ledger *Ledger, scan ScanResult) {
	present := map[string]bool{}
	identities := map[string]int{}
	for _, entry := range scan.Entries {
		present[entry.Path] = true
		identities[entry.Stamp.Identity]++
	}
	for _, entry := range scan.Entries {
		if ctx.Err() != nil {
			return
		}
		if _, known := ledger.Get(entry.Path); known || entry.Stamp.Identity == "" || identities[entry.Stamp.Identity] != 1 {
			continue
		}
		candidate := ""
		for _, old := range ledger.Paths() {
			rec, _ := ledger.Get(old)
			if old == entry.Path || present[old] || rec.Stamp.Identity != entry.Stamp.Identity || rec.FileID == "" || rec.Stamp.SHA256 == "" {
				continue
			}
			if m.opts.CheckPath == nil || m.opts.CheckPath(old) != nil {
				continue
			}
			if _, err := os.Lstat(old); !errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if candidate != "" {
				candidate = ""
				break
			}
			candidate = old
		}
		if candidate == "" {
			continue
		}
		rec, _ := ledger.Get(candidate)
		digest, err := Digest(entry.Path)
		if err != nil || digest != rec.Stamp.SHA256 {
			continue
		}
		current, err := statOf(entry.Path)
		if err != nil || !current.Unchanged(entry.Stamp) {
			continue
		}
		if err := m.graph.Relink(ctx, rec.FileID, m.workerID, candidate, entry.Path); err != nil {
			m.logger.Warn("backup: could not follow moved file", "error", err)
			continue
		}
		rec.Stamp = current
		rec.Stamp.SHA256 = digest
		ledger.Put(entry.Path, rec)
		ledger.Forget(candidate)
		if err := ledger.Save(); err != nil {
			m.logger.Warn("backup: could not record moved file", "error", err)
		}
	}
}
