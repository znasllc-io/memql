package backup

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// The ledger: what this machine has already sent, per watched folder.
//
// ===========================================================================
// WHY THERE IS LOCAL STATE AT ALL
// ===========================================================================
// The engine can answer "is there a file at this (machine, path)" -- that is
// what libraryFileByUploadedFrom is for -- but it cannot answer "are the bytes
// there the bytes that are here now" without being sent them. Keeping the last
// stamp locally is what turns every sweep after the first into a few thousand
// stat calls instead of a few thousand uploads.
//
// IT IS A CACHE, NOT A SOURCE OF TRUTH. Losing it requires comparison with the
// remote digest before a version baseline can be adopted. A mismatch is a
// conflict rather than permission to overwrite an unknown remote revision.
//
// ONE FILE PER WATCH, named by the watch id, so a watch somebody stopped can
// be forgotten by deleting one file, and two watches sweeping concurrently
// never write the same file.

const (
	ledgerDirMode  os.FileMode = 0o700
	ledgerFileMode os.FileMode = 0o600
)

// Record is what the ledger remembers about one file.
type Record struct {
	TransferID string `json:"transferId,omitempty"`
	InBin      bool   `json:"inBin,omitempty"`
	Stamp      Stamp  `json:"stamp"`
	// FileID is the v1:library:file the last push produced. Kept so the
	// verify lane can report a state without asking the engine which row a
	// path belongs to on every sweep; an empty value simply means "ask".
	FileID        string `json:"fileId,omitempty"`
	VersionNumber int    `json:"versionNumber,omitempty"`
	// PushedAtUnix is when those bytes were accepted.
	PushedAtUnix int64 `json:"pushedAtUnix,omitempty"`
	// VerifiedAtUnix is when this file's link state was last REPORTED to the
	// engine. It paces the verify lane: an unchanged file's `synced` is
	// re-stamped occasionally so "checked 3 weeks ago" stops being true, but
	// not on every sweep, or a folder of ten thousand files would be ten
	// thousand writes every few minutes forever.
	VerifiedAtUnix int64 `json:"verifiedAtUnix,omitempty"`
	// LinkState is the last state reported for this file, so the lane can
	// tell a CHANGE (report it now) from a repeat (report it on the slow
	// cadence).
	LinkState string `json:"linkState,omitempty"`
	// UploadID is a chunked session left open by a push that did not finish,
	// and UploadSize the size it was opened for. Together they are what makes
	// the resume real: without the id, every interrupted push of a large file
	// starts again from chunk 1, which on a domestic uplink is the difference
	// between a backup that finishes and one that never does.
	//
	// Cleared on success. A stale pair costs one extra request and falls
	// through to a fresh session, so it can never stop a backup.
	UploadID     string `json:"uploadId,omitempty"`
	UploadSize   int64  `json:"uploadSize,omitempty"`
	UploadSHA256 string `json:"uploadSHA256,omitempty"`
}

// Ledger checkpoints changed paths in an append journal. A folder with many
// files must not rewrite every previous receipt for each completed upload.
// Checksummed records survive a torn tail; periodic snapshots carry a sequence
// so replay cannot roll them back after an interrupted compaction.
type Ledger struct {
	mu            sync.Mutex
	path          string
	files         map[string]Record
	dirty         map[string]*Record
	sequence      uint64
	journalBytes  int64
	snapshotBytes int64
}

type ledgerDoc struct {
	Version  int               `json:"version"`
	Sequence uint64            `json:"sequence,omitempty"`
	Files    map[string]Record `json:"files"`
}

// LoadLedger reads the ledger for one watch, or returns an empty one.
//
// EVERY FAILURE IS AN EMPTY LEDGER, not an error. A corrupt or unreadable
// cache must not prevent verification. The manager compares the remote
// digest before adopting the current version, and refuses unknown conflicts.
func LoadLedger(stateDir, watchID string) *Ledger {
	l := &Ledger{path: ledgerPath(stateDir, watchID), files: map[string]Record{}, dirty: map[string]*Record{}}
	data, err := os.ReadFile(l.path)
	if err == nil {
		var doc ledgerDoc
		if json.Unmarshal(data, &doc) == nil && doc.Version == 1 && doc.Files != nil {
			l.files = doc.Files
			l.sequence = doc.Sequence
			l.snapshotBytes = int64(len(data))
		}
	}
	l.replay()
	return l
}

type ledgerChange struct {
	Sequence uint64  `json:"sequence"`
	Path     string  `json:"path"`
	Record   *Record `json:"record"`
}
type ledgerEnvelope struct {
	Data     json.RawMessage `json:"data"`
	Checksum string          `json:"checksum"`
}

func (l *Ledger) replay() {
	f, err := os.Open(l.path + ".journal")
	if err != nil {
		return
	}
	defer f.Close()
	reader := bufio.NewReaderSize(f, 256<<10)
	for {
		line, err := reader.ReadSlice('\n')
		if err != nil {
			return
		}
		if len(line) > 256<<10 {
			return
		}
		var envelope ledgerEnvelope
		var change ledgerChange
		if json.Unmarshal(line, &envelope) != nil {
			return
		}
		digest := sha256.Sum256(envelope.Data)
		if hex.EncodeToString(digest[:]) != envelope.Checksum || json.Unmarshal(envelope.Data, &change) != nil || change.Path == "" || change.Sequence == 0 {
			return
		}
		l.journalBytes += int64(len(line))
		if change.Sequence <= l.sequence {
			continue
		}
		if change.Record == nil {
			delete(l.files, change.Path)
		} else {
			l.files[change.Path] = *change.Record
		}
		l.sequence = change.Sequence
	}
}

func ledgerPath(stateDir, watchID string) string {
	return filepath.Join(stateDir, "backup", watchID+".json")
}

// Get returns the record for a path, and whether one exists.
func (l *Ledger) Get(path string) (Record, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, ok := l.files[path]
	return rec, ok
}

// Put records a path.
func (l *Ledger) Put(path string, rec Record) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.files[path] = rec
	if l.dirty == nil {
		l.dirty = map[string]*Record{}
	}
	copy := rec
	l.dirty[path] = &copy
}

// Paths returns every path the ledger knows, so the verify lane can notice
// the ones that are no longer at the origin.
func (l *Ledger) Paths() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, 0, len(l.files))
	for path := range l.files {
		out = append(out, path)
	}
	return out
}

// Forget drops a path.
//
// USED FOR NOTHING THE ORIGIN DOES. A file deleted at the origin stays in the
// ledger with its state reported as origin_gone, because forgetting it would
// make the next sweep unable to tell "gone" from "never seen" -- and the whole
// invariant of this feature is that a deletion at the origin FLAGS the copy
// rather than removing it. This exists for a watch being reconfigured.
func (l *Ledger) Forget(path string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.files, path)
	if l.dirty == nil {
		l.dirty = map[string]*Record{}
	}
	l.dirty[path] = nil
}

// Save syncs only changed receipts before returning. A failed write retains the
// dirty set, and the next attempt truncates any uncertain tail before retrying.
func (l *Ledger) Save() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.dirty) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(l.path), ledgerDirMode); err != nil {
		return err
	}
	f, err := os.OpenFile(l.path+".journal", os.O_CREATE|os.O_RDWR, ledgerFileMode)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = f.Truncate(l.journalBytes); err != nil {
		return err
	}
	if _, err = f.Seek(l.journalBytes, io.SeekStart); err != nil {
		return err
	}
	var written int64
	for path, record := range l.dirty {
		l.sequence++
		data, e := json.Marshal(ledgerChange{l.sequence, path, record})
		if e != nil {
			return e
		}
		sum := sha256.Sum256(data)
		envelope, e := json.Marshal(ledgerEnvelope{data, hex.EncodeToString(sum[:])})
		if e != nil {
			return e
		}
		envelope = append(envelope, '\n')
		n, e := f.Write(envelope)
		if e != nil {
			return e
		}
		if n != len(envelope) {
			return io.ErrShortWrite
		}
		written += int64(n)
	}
	if err = f.Sync(); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(l.path))
	if err != nil {
		return err
	}
	err = d.Sync()
	d.Close()
	if err != nil {
		return err
	}
	l.journalBytes += written
	l.dirty = map[string]*Record{}
	if l.journalBytes < max(16<<20, 2*l.snapshotBytes) {
		return nil
	}
	data, err := json.Marshal(ledgerDoc{Version: 1, Sequence: l.sequence, Files: l.files})
	if err != nil {
		return err
	}
	if err = writeFileAtomic(l.path, data, ledgerFileMode); err != nil {
		return err
	}
	l.snapshotBytes = int64(len(data))
	if err = f.Truncate(0); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	l.journalBytes = 0
	return nil
}

// writeFileAtomic writes through a temp file in the SAME directory and
// renames, so a reader never sees a half-written ledger and a crash mid-write
// leaves the previous one intact. Same shape as appsession's own writer; not
// shared, because importing across those two packages for one helper would
// couple two subsystems that have nothing else to say to each other.
func writeFileAtomic(path string, body []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, ledgerDirMode); err != nil {
		return fmt.Errorf("backup: create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".ledger-*")
	if err != nil {
		return fmt.Errorf("backup: temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("backup: write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("backup: close %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return fmt.Errorf("backup: chmod %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("backup: rename into %s: %w", path, err)
	}
	// Persist the directory entry too: syncing only the file can still lose
	// the rename after a power failure.
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

// ErrNoLedgerDir is returned when a caller asks for ledgers with no state
// directory configured, which is a wiring mistake rather than a runtime one.
var ErrNoLedgerDir = errors.New("backup: no state directory configured")

// registrationFile is where the sweeper remembers which registration it swept
// as, so `memql worker backup --once` can act as the same machine the
// background worker does.
//
// WRITTEN BY THE LOOP, READ BY THE COMMAND. The id arrives on a RegisterAck,
// inside a connection only the running worker holds -- a separate process has
// no way to ask for it, and there is no read that answers "which registration
// am I", because that is a property of a connection rather than of the
// cluster. Persisting the one the loop already learned is the only honest way
// for the command to run the same sweep, and running a DIFFERENT sweep is
// exactly what the command exists not to do.
func registrationFile(stateDir string) string {
	return filepath.Join(stateDir, "backup", "registration")
}

// SaveRegistrationID records the machine this sweeper acts as. Best-effort:
// failing to write it costs `--once` its id and nothing else, so it must never
// stop a sweep.
func SaveRegistrationID(stateDir, id string) error {
	if strings.TrimSpace(stateDir) == "" || strings.TrimSpace(id) == "" {
		return ErrNoLedgerDir
	}
	return writeFileAtomic(registrationFile(stateDir), []byte(id), ledgerFileMode)
}

// LoadRegistrationID reads it back, or "" when this machine's worker has not
// registered since the feature landed.
func LoadRegistrationID(stateDir string) string {
	if strings.TrimSpace(stateDir) == "" {
		return ""
	}
	body, err := os.ReadFile(registrationFile(stateDir))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(body))
}
