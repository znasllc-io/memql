// Package backup is memQL's portable data export: a whole cluster's graph
// written as a stream a LATER engine can still read.
//
// WHY NOT pg_dump. A dump is coupled to the physical schema -- the two node
// tables, the TimescaleDB hypertable layout, the migration state the engine
// happens to be on. Restoring one into a newer engine is precisely the fragile
// case, and it cannot be made version-portable without heroics. That is the
// opposite of what a backup is for: the moment it matters is the moment the
// thing that wrote it is gone.
//
// WHAT THIS IS INSTEAD. memQL's data model is not tables. A row is
// (concept, id, createdAt, payload) and the CONCEPT is defined in the DSL, not
// in DDL. So the honest unit of export is the row as the engine understands it,
// and the format is one JSON object per line: a manifest first, then every row.
// A later engine reads that without caring how this one stored it, and
// compatibility becomes a per-concept payload question -- which is where the
// DSL already lets fields evolve.
//
// THE COMPATIBILITY PROMISE, STATED IN ONE DIRECTION ONLY.
//
//	A newer engine MUST read every older backup.
//	An older engine is NOT required to read a newer backup, and refuses it
//	outright rather than importing half of it.
//
// Only the first half is a promise anyone can keep. Guaranteeing the reverse
// would mean today's engine understanding fields invented after it shipped,
// which is not a thing to promise -- and a half-restore is worse than a refusal
// because it looks like it worked. FormatVersion is what makes the refusal
// possible, and it is the first field on the first line so a reader can decide
// before parsing anything else.
//
// The promise is ENFORCED, not documented: testdata/ carries a real backup from
// each format version, and a test restores every one of them on the current
// engine. A compatibility rule with no gate decays within two releases -- see
// memql#3600, where the rule existed in a comment and nothing checked it.
package backup

import (
	"encoding/json"
	"fmt"
	"time"
)

// FormatVersion is the version of the STREAM SHAPE, not of the engine and not
// of any concept's payload. It changes only when the envelope changes in a way
// an older reader could not survive -- a renamed record field, a new required
// field, a different framing. Adding an OPTIONAL field does not bump it,
// because an older reader ignores unknown keys.
const FormatVersion = 1

// Record kinds. The kind is on every line so a reader never has to infer a
// line's meaning from its position -- a stream that gets truncated, split or
// concatenated is still unambiguous line by line.
const (
	KindManifest = "memql.backup.manifest"
	KindRow      = "memql.backup.row"
)

// Table names as they appear in a record. Spelled here rather than imported
// from the database package so the FORMAT does not move when the storage layer
// does -- that independence is the whole point of the file.
const (
	TableMemoryNodes       = "MemoryNodes"
	TableSecretMemoryNodes = "SecretMemoryNodes"
)

// Manifest is the first line of every backup.
type Manifest struct {
	Kind string `json:"kind"`
	// FormatVersion is checked before anything else is parsed.
	FormatVersion int `json:"formatVersion"`
	// EngineVersion is the engine that WROTE the backup. Informational: the
	// reader gates on FormatVersion, never on this. It exists so a human
	// looking at a file six months later can tell where it came from.
	EngineVersion string    `json:"engineVersion"`
	CreatedAt     time.Time `json:"createdAt"`
	// Domain the source cluster served, for the same human reason.
	Domain string `json:"domain,omitempty"`
	// Counts per table, written AFTER the rows are streamed and therefore
	// present only on a COMPLETE backup -- see Writer.Close. A reader that
	// finds them absent is looking at a truncated file.
	Counts map[string]int `json:"counts,omitempty"`
	// SecretKeyFingerprint identifies the master key whose encryption the
	// SecretMemoryNodes payloads are under, WITHOUT disclosing the key: it is
	// a truncated HMAC of a fixed label under that key.
	//
	// Restoring secret rows into a cluster with a different master key
	// produces rows nobody can decrypt -- present, counted, and useless. The
	// fingerprint is what lets restore say so plainly instead of leaving the
	// operator to discover it at the next sign-in.
	SecretKeyFingerprint string `json:"secretKeyFingerprint,omitempty"`
	// IncludesSecrets records whether SecretMemoryNodes rows are in the
	// stream at all, so their absence reads as a choice rather than as a
	// truncation.
	IncludesSecrets bool `json:"includesSecrets"`
}

// Row is one graph row, in the shape the engine understands it.
//
// The JSONB columns travel as RAW JSON rather than as decoded maps: a backup
// must not silently normalise a payload it does not understand. Key order,
// number formatting and unknown fields all survive a round trip untouched.
type Row struct {
	Kind      string          `json:"kind"`
	Table     string          `json:"table"`
	ID        string          `json:"id"`
	CreatedAt time.Time       `json:"createdAt"`
	CreatedBy string          `json:"createdBy"`
	Concept   string          `json:"concept"`
	Type      string          `json:"type"`
	Schema    json.RawMessage `json:"schema"`
	Payload   json.RawMessage `json:"payload"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
	// Provenance is NOT NULL in the database: every row carries where it came
	// from. A backup that dropped it would restore rows the engine considers
	// unattributed.
	Provenance json.RawMessage `json:"provenance"`
}

// ErrFormatTooNew is returned when a backup was written by a later engine.
//
// A distinct error type because it is the one failure an operator can act on
// without reading code: the file is fine, this binary is too old.
type ErrFormatTooNew struct {
	Found     int
	Supported int
}

func (e *ErrFormatTooNew) Error() string {
	return fmt.Sprintf(
		"backup was written in format version %d and this engine reads up to %d: "+
			"restore it with an engine at least as new as the one that wrote it. "+
			"A newer engine reads every older backup; the reverse is not promised, "+
			"and importing part of a stream this binary does not fully understand "+
			"would look like success",
		e.Found, e.Supported)
}

// checkFormatVersion applies the one-directional promise.
func checkFormatVersion(found int) error {
	if found <= 0 {
		return fmt.Errorf("backup manifest has no formatVersion: this is not a memQL backup, or it is truncated")
	}
	if found > FormatVersion {
		return &ErrFormatTooNew{Found: found, Supported: FormatVersion}
	}
	return nil
}
