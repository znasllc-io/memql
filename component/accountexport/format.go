// Package accountexport is the file an account leaves a cluster in: a tar
// stream holding one folder, with a manifest first, the account's rows as JSON
// lines in gzip parts, the bytes those rows point at, a report, a README and a
// checksum list last.
//
// The package holds the format and its checker and reads no database. The run
// that gathers the rows writes through Writer, reads its own file back through
// Verify, and the import piece will open files with the same Verify. The
// design record is docs/superpowers/specs/2026-11-12-account-export-design.md,
// sections D and E.
//
// THE COMPATIBILITY PROMISE, as for component/backup, in one direction only:
// a newer engine reads every older file; an older engine refuses a newer one
// outright, before reading anything past the manifest. testdata/v<N>/ keeps a
// sample file of each format version and compatibility_test.go checks every
// one of them on the current engine.
package accountexport

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// FormatVersion is the version of the file's shape: its layout, its member
// names and its line shapes. Adding an optional field does not bump it,
// because an older reader ignores unknown keys.
const FormatVersion = 1

// Line kinds. A row line carries its kind so it is unambiguous on its own,
// wherever it was cut from.
const (
	KindManifest = "memql.account.export"
	KindRow      = "memql.account.row"
)

// Member names inside the folder. Spelled here once; the writer and the
// checker both read them from here.
const (
	MemberManifest    = "manifest.json"
	MemberAccount     = "account.jsonl"
	MemberPeople      = "people.jsonl"
	MemberReferences  = "references.jsonl"
	MemberConnections = "connections.jsonl"
	MemberBlocks      = "blocks.jsonl"
	MemberBytesIndex  = "bytes.jsonl"
	MemberReport      = "report.json"
	MemberReadme      = "README.txt"
	MemberSums        = "SHA256SUMS"

	dirRows    = "rows"
	dirSchemas = "schemas"
	dirTables  = "tables"
	dirBytes   = "bytes"
)

// FolderSuffix ends the name of the one folder inside the tar.
const FolderSuffix = ".memql-account"

// Manifest is the first member. FormatVersion is read before anything else.
type Manifest struct {
	Kind          string `json:"kind"`
	FormatVersion int    `json:"formatVersion"`
	// EngineVersion is the release that wrote the file. Informational: a
	// reader gates on FormatVersion only.
	EngineVersion string `json:"engineVersion"`
	ExportID      string `json:"exportId"`
	// CreatedAt is the snapshot's instant: every line comes from it.
	CreatedAt  time.Time      `json:"createdAt"`
	Source     Source         `json:"source"`
	Account    Account        `json:"account"`
	Protected  bool           `json:"protected"`
	Kinds      []KindDecl     `json:"kinds"`
	Language   []LanguageLine `json:"language,omitempty"`
	Migrations []string       `json:"migrations,omitempty"`
	Embedder   string         `json:"embedder,omitempty"`
}

// Source is the instance the file came from.
type Source struct {
	Domain    string `json:"domain"`
	ClusterID string `json:"clusterId,omitempty"`
}

// Account is the account the file holds.
type Account struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Primary bool   `json:"primary"`
}

// KindDecl is the declaration of one kind in the file: its @belongsTo form,
// and @origin or @mirroredTo where it has one, so an import knows a mirror
// from an origin.
type KindDecl struct {
	Concept    string   `json:"concept"`
	BelongsTo  string   `json:"belongsTo"`
	Origin     string   `json:"origin,omitempty"`
	MirroredTo []string `json:"mirroredTo,omitempty"`
}

// LanguageLine is the language line of one mounted DSL domain.
type LanguageLine struct {
	Domain      string `json:"domain"`
	Memql       string `json:"memql"`
	Edition     string `json:"edition"`
	ContentHash string `json:"contentHash,omitempty"`
}

// Row is one version of one row, in the backup's row shape. The JSON columns
// travel as raw JSON so nothing the file does not understand is normalised.
type Row struct {
	Kind       string          `json:"kind"`
	Concept    string          `json:"concept"`
	ID         string          `json:"id"`
	CreatedAt  time.Time       `json:"createdAt"`
	CreatedBy  string          `json:"createdBy"`
	Type       string          `json:"type"`
	Schema     json.RawMessage `json:"schema"`
	Payload    json.RawMessage `json:"payload"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`
	Provenance json.RawMessage `json:"provenance"`
	// Observed marks a version held in the observation store.
	Observed bool `json:"observed,omitempty"`
}

// KindCount is one kind's rows and versions, as counted by a separate
// statement in the snapshot (expected) and as written to the file (written).
type KindCount struct {
	Concept          string `json:"concept"`
	ExpectedRows     int64  `json:"expectedRows"`
	ExpectedVersions int64  `json:"expectedVersions"`
	WrittenRows      int64  `json:"writtenRows"`
	WrittenVersions  int64  `json:"writtenVersions"`
}

// Reasons a kind stays on the cluster.
const (
	LeftOutSignin  = "signin"
	LeftOutTransit = "transit"
	LeftOutMachine = "machine"
)

// LeftOut is one kind that stays, with its reason and how many rows stayed.
type LeftOut struct {
	Concept string `json:"concept"`
	Reason  string `json:"reason"`
	Rows    int64  `json:"rows"`
}

// KindRows counts the rows of one kind.
type KindRows struct {
	Concept string `json:"concept"`
	Rows    int64  `json:"rows"`
}

// ScrubbedField names a @secret field whose value was removed.
type ScrubbedField struct {
	Concept string `json:"concept"`
	Field   string `json:"field"`
}

// BytesReport counts the bytes carried, missing at the source, and counted
// but not carried.
type BytesReport struct {
	Objects               int64 `json:"objects"`
	Bytes                 int64 `json:"bytes"`
	Missing               int64 `json:"missing"`
	NotCarriedObjects     int64 `json:"notCarriedObjects"`
	NotCarriedBytes       int64 `json:"notCarriedBytes"`
	NotCarriedUnknownSize int64 `json:"notCarriedUnknownSize"`
}

// MemberCount is the number of lines in one line member that is not a row
// member: people.jsonl, references.jsonl, connections.jsonl, a hand-over's
// single member, or a hand-over's parted folder under tables/.
type MemberCount struct {
	Name  string `json:"name"`
	Lines int64  `json:"lines"`
}

// Report is report.json: counts expected and written, and everything left out
// with its reason.
type Report struct {
	Kinds          []KindCount     `json:"kinds"`
	LeftOut        []LeftOut       `json:"leftOut"`
	InNoAccount    []KindRows      `json:"inNoAccount"`
	ScrubbedFields []ScrubbedField `json:"scrubbedFields"`
	// Rebuilt names, once, what the target rebuilds from what travels:
	// vectors, email statistics, site health.
	Rebuilt []string      `json:"rebuilt"`
	Bytes   BytesReport   `json:"bytes"`
	Members []MemberCount `json:"members"`
}

// BytesMeta describes one object of blob storage a carried row points at.
type BytesMeta struct {
	// Key is the object's key as stored.
	Key    string     `json:"key"`
	UsedBy []RowField `json:"usedBy"`
	// TreeOf is the prefix, for an object of a tree.
	TreeOf string `json:"treeOf,omitempty"`
	// Path is the object's path under that prefix.
	Path    string `json:"path,omitempty"`
	Missing bool   `json:"missing,omitempty"`
}

// RowField names the field of a row that points at an object.
type RowField struct {
	Concept string `json:"concept"`
	ID      string `json:"id"`
	Field   string `json:"field"`
}

// BytesLine is one line of bytes.jsonl: the object's description, and for a
// carried object the member holding it, its size and its SHA-256.
type BytesLine struct {
	BytesMeta
	File   string `json:"file,omitempty"`
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

// ErrFormatTooNew is returned for a file written by a later engine. The file
// is fine; this binary is too old.
type ErrFormatTooNew struct {
	Found     int
	Supported int
}

func (e *ErrFormatTooNew) Error() string {
	return fmt.Sprintf(
		"account export was written in format version %d and this engine reads up to %d: "+
			"open it with an engine at least as new as the one that wrote it",
		e.Found, e.Supported)
}

func checkFormatVersion(found int) error {
	if found <= 0 {
		return fmt.Errorf("account export manifest has no formatVersion: this is not an account export")
	}
	if found > FormatVersion {
		return &ErrFormatTooNew{Found: found, Supported: FormatVersion}
	}
	return nil
}

// RowsDir is the folder a kind's parts live in: "rows/<namespace>/<name>" for
// "v1:<namespace>:<name>". It answers "" for a concept of any other shape,
// which Writer.Parted refuses.
func RowsDir(concept string) string {
	parts := strings.Split(concept, ":")
	if len(parts) != 3 || parts[0] != "v1" || !safeSegment(parts[1]) || !safeSegment(parts[2]) {
		return ""
	}
	return dirRows + "/" + parts[1] + "/" + parts[2]
}

// safeSegment keeps every path inside the tar to a character set that
// SHA256SUMS can name without escaping and that no extractor reads as a
// traversal.
func safeSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}
