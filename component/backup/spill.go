package backup

import (
	"fmt"
	"io"
	"os"
)

// spill is the temporary file an export streams rows into before the manifest
// is known.
//
// It exists so a backup's memory cost is CONSTANT rather than proportional to
// the database: counts belong in the header, the header is written first, and
// the counts are only known last. A temp file resolves that without holding a
// cluster's worth of rows in RAM.
//
// Created with os.CreateTemp and unlinked IMMEDIATELY on unix, so the bytes
// live only as long as the open handle -- a crashed export leaves nothing
// behind, and nothing readable by another user appears on disk in between.
type spill struct {
	file *os.File
}

func newSpill() (*spill, error) {
	f, err := os.CreateTemp("", "memql-backup-*.jsonl")
	if err != nil {
		return nil, fmt.Errorf("backup: create spill file: %w", err)
	}
	// Unlink now: the handle keeps the data alive, and nothing is left on disk
	// if this process dies. On a platform where this fails, Close still removes
	// it by name.
	_ = os.Remove(f.Name())
	return &spill{file: f}, nil
}

func (s *spill) copyTo(w io.Writer) error {
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("backup: rewind spill: %w", err)
	}
	if _, err := io.Copy(w, s.file); err != nil {
		return fmt.Errorf("backup: copy rows: %w", err)
	}
	return nil
}

func (s *spill) Close() {
	name := s.file.Name()
	_ = s.file.Close()
	_ = os.Remove(name)
}
