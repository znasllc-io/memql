package backup

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// keyFingerprintLabel is the fixed message the master key signs to produce the
// manifest's fingerprint. A constant label means the fingerprint identifies the
// KEY and nothing else -- two clusters holding the same key produce the same
// fingerprint, which is exactly the comparison restore needs to make.
const keyFingerprintLabel = "memql.backup.master-key-fingerprint.v1"

// KeyFingerprint returns a short, non-reversible identifier for a master key,
// or "" when there is no key.
//
// HMAC rather than a plain hash of the key: a plain digest of a secret is a
// verifier for that secret, and this value is written into a file people will
// email around. HMAC of a fixed label under the key discloses nothing about it
// while still colliding only for the same key.
func KeyFingerprint(masterKey string) string {
	if masterKey == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(masterKey))
	mac.Write([]byte(keyFingerprintLabel))
	return hex.EncodeToString(mac.Sum(nil))[:16]
}

// Writer streams a backup out as newline-delimited JSON.
//
// The manifest is written LAST despite appearing FIRST, which needs explaining:
// counts are only known once the rows have been streamed, and holding every row
// in memory to count them first would make a backup's cost scale with the data
// rather than stay constant. So rows are buffered to the caller's writer while
// counts accumulate, and Close writes the manifest to the FRONT via the
// two-file assembly in WriteTo. Callers use WriteTo; this type is not exported
// for direct use.
type Writer struct {
	rows   *bufio.Writer
	counts map[string]int
	err    error
}

func newWriter(w io.Writer) *Writer {
	return &Writer{rows: bufio.NewWriterSize(w, 1<<16), counts: map[string]int{}}
}

// WriteRow appends one row. The first error is sticky: a stream that failed
// half way is not a backup, and continuing to append to it would produce a file
// that looks complete.
func (w *Writer) WriteRow(r Row) error {
	if w.err != nil {
		return w.err
	}
	r.Kind = KindRow
	b, err := json.Marshal(r)
	if err != nil {
		w.err = fmt.Errorf("backup: marshal row %s/%s: %w", r.Concept, r.ID, err)
		return w.err
	}
	if _, err := w.rows.Write(append(b, '\n')); err != nil {
		w.err = fmt.Errorf("backup: write row %s/%s: %w", r.Concept, r.ID, err)
		return w.err
	}
	w.counts[r.Table]++
	return nil
}

func (w *Writer) flush() error {
	if w.err != nil {
		return w.err
	}
	return w.rows.Flush()
}

// Counts reports rows written per table.
func (w *Writer) Counts() map[string]int {
	out := make(map[string]int, len(w.counts))
	for k, v := range w.counts {
		out[k] = v
	}
	return out
}

// Reader consumes a backup stream.
type Reader struct {
	scanner  *bufio.Scanner
	manifest Manifest
}

// NewReader reads and validates the manifest line, and refuses a format this
// binary does not fully understand BEFORE any row is handed back.
func NewReader(r io.Reader) (*Reader, error) {
	sc := bufio.NewScanner(r)
	// Rows carry arbitrary JSONB payloads; the default 64KiB line cap is far
	// too small for a document chunk or an embedding.
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)

	if !sc.Scan() {
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("backup: read manifest: %w", err)
		}
		return nil, errors.New("backup: the file is empty")
	}
	var m Manifest
	if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
		return nil, fmt.Errorf("backup: the first line is not a manifest: %w", err)
	}
	if m.Kind != KindManifest {
		return nil, fmt.Errorf("backup: the first line is kind %q, want %q", m.Kind, KindManifest)
	}
	if err := checkFormatVersion(m.FormatVersion); err != nil {
		return nil, err
	}
	return &Reader{scanner: sc, manifest: m}, nil
}

// Manifest returns the validated header.
func (r *Reader) Manifest() Manifest { return r.manifest }

// Next returns the next row, or io.EOF.
//
// Unknown record kinds are SKIPPED rather than rejected. That is what lets a
// later format add a record type -- a checksum trailer, say -- without breaking
// this reader, which is half of how the compatibility promise is kept cheaply.
func (r *Reader) Next() (Row, error) {
	for r.scanner.Scan() {
		line := r.scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var probe struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			return Row{}, fmt.Errorf("backup: malformed line: %w", err)
		}
		if probe.Kind != KindRow {
			continue // forward compatibility: a kind we were not told about
		}
		var row Row
		if err := json.Unmarshal(line, &row); err != nil {
			return Row{}, fmt.Errorf("backup: malformed row: %w", err)
		}
		if row.Table != TableMemoryNodes && row.Table != TableSecretMemoryNodes {
			return Row{}, fmt.Errorf("backup: row %s names unknown table %q", row.ID, row.Table)
		}
		return row, nil
	}
	if err := r.scanner.Err(); err != nil {
		return Row{}, fmt.Errorf("backup: read: %w", err)
	}
	return Row{}, io.EOF
}

// ManifestFor builds the header for a completed export.
func ManifestFor(engineVersion, domain, keyFingerprint string, includesSecrets bool, counts map[string]int, now time.Time) Manifest {
	return Manifest{
		Kind:                 KindManifest,
		FormatVersion:        FormatVersion,
		EngineVersion:        engineVersion,
		CreatedAt:            now.UTC(),
		Domain:               domain,
		Counts:               counts,
		SecretKeyFingerprint: keyFingerprint,
		IncludesSecrets:      includesSecrets,
	}
}

// WriteManifest emits the header line.
func WriteManifest(w io.Writer, m Manifest) error {
	m.Kind = KindManifest
	m.FormatVersion = FormatVersion
	b, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("backup: marshal manifest: %w", err)
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("backup: write manifest: %w", err)
	}
	return nil
}
