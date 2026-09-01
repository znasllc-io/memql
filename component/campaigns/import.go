package campaigns

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
	"github.com/znasllc-io/memql/core/id"
	"github.com/znasllc-io/memql/integrations/azureblob"
)

// import.go -- turning an uploaded CSV into audience rows (memql#4822,
// design D9).
//
// # The artifact id is not a capability
//
// The file arrives through the Library's existing upload, and the import is
// handed only its artifact id. Every read on the way to the bytes runs under
// the CALLER'S OWN ACTOR against owner-gated queries, so a file the caller
// cannot read is a file this cannot import -- the same answer as it not
// existing. Reading under the engine's own identity and trusting the id would
// turn one integer into a read primitive for every upload in the cluster.
//
// # A header is REQUIRED, and that is a refusal rather than a limitation
//
// The column mapping IS the header. Without one, something has to guess which
// column holds addresses -- and the cost of guessing wrong is a campaign
// mailed to a list assembled from the wrong column, which cannot be recalled.
// `email` is required (case-insensitive); `displayName` / `name` are
// recognized; EVERY other column lands verbatim in the recipient's `fields`,
// reachable from a template as {{fields.<key>}}.
//
// # The cap is checked as a WHOLE, before a single row is written
//
// MEMQL_CAMPAIGNS_MAX_AUDIENCE is measured against the roster the import
// WOULD produce -- the server-side count plus what the file adds -- and the
// whole import is refused rather than truncated. A partially imported list is
// one nobody knows is partial: the audience looks complete, the send looks
// complete, and the missing tail is invisible at every surface. That is the
// same "a bounded read of an unbounded set is a truncation" rule the send
// path already refuses on.
//
// # What "duplicate" means here, and why first occurrence wins
//
// Dedup runs against BOTH the audience's existing rows and earlier rows of
// the same file. A re-import of a corrected list therefore adds only what is
// new, which is the behaviour that makes re-importing safe -- and it is why a
// hard bounce SUPPRESSES rather than deleting the membership (capabilities.go
// states that at length): a deleted row would be re-added as a fresh
// `subscribed` recipient by exactly this path.
//
// First occurrence wins because a CSV's later rows are, in practice, appended
// corrections of unknown provenance; taking the last would mean a file that
// ends with a stale export silently overwrites a fresher entry.

const (
	// importSampleCap bounds how many rejected lines come back in the reply.
	// Twenty is enough to see the PATTERN -- a shifted column, a whole
	// section with no address -- which is what the operator needs to fix the
	// file. A full list would be the file again.
	importSampleCap = 20

	// importMaxBytes bounds the CSV this reads into memory. The Library's own
	// per-file ceiling is far higher (4 GiB), and a recipient list is not
	// that: at ~100 bytes a row this is over a million addresses, which is
	// four times the default audience ceiling. It exists so a mis-selected
	// artifact -- a video, a database dump -- is a refusal rather than an
	// OOM on a shared node.
	importMaxBytes = 128 << 20

	// importSourceValue is the `source` every imported recipient carries.
	// The enum has had this value since memql#3323 and nothing wrote it
	// until now.
	importSourceValue = "import"

	// utf8BOM is what Excel writes at the start of a CSV it saves as UTF-8.
	// Left on the first header cell it makes the `email` column match
	// nothing, and the import refuses a file whose header is visibly correct
	// -- spelled as an escape rather than as the character, because a literal
	// BOM in a Go source file is a compile error and in any other file is
	// invisible.
	utf8BOM = "\ufeff"
)

// blobReader is the read half of object storage. Its own narrow interface
// rather than a dependency on component/edge's BlobClient, because this
// package needs exactly one method and a test substitutes an in-memory map.
type blobReader interface {
	Download(ctx context.Context, container, objectName string) ([]byte, error)
}

// importResult is the reply. Counts plus a bounded sample, so the operator's
// next action is fixing the file rather than guessing at it.
type importResult struct {
	Added        int
	Duplicates   int
	Invalid      int
	Total        int
	InvalidLines []invalidLine
}

// invalidLine is one rejected row WITH its line number, which is the whole
// point: "3 rows were invalid" sends somebody hunting through a spreadsheet.
type invalidLine struct {
	Line   int
	Reason string
	// Value is the offending cell, REDACTED to its domain when it looks like
	// an address. An import log that echoes mailboxes is a mailing list in a
	// log file.
	Value string
}

// handleImportRecipients implements integration.campaigns.importRecipients.
func (w *Worker) handleImportRecipients(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	audienceID := memql.BareShortId(strings.TrimSpace(argString(args, "audienceId")))
	artifactID := strings.TrimSpace(argString(args, "artifactId"))
	if audienceID == "" {
		return nil, errors.New("campaigns.importRecipients: audienceId is required")
	}
	if artifactID == "" {
		return nil, errors.New("campaigns.importRecipients: artifactId is required")
	}
	// hasHeader is read in BOTH spellings a caller can produce -- the declared
	// bool, and the string a JSON client sends for the same field. Reading
	// only the bool would let `"false"` fall through to the permissive branch
	// and import a headerless file as though the caller had asked for a
	// header, which is the one direction this refusal must not fail in.
	if !headerAsserted(args["hasHeader"]) {
		return nil, errors.New("campaigns.importRecipients: hasHeader=false is refused. The column mapping " +
			"IS the header, and guessing which column holds addresses is how an import mails the wrong " +
			"list -- which cannot be recalled. Add a header row naming at least `email`")
	}
	if strings.TrimSpace(callerUserID(ctx)) == "" {
		return nil, errors.New("campaigns.importRecipients: no caller identity; an import is always run by somebody")
	}

	// The bytes, under the CALLER's actor from the first read to the last.
	ref, refusal, err := w.store.LibraryFileForArtifact(ctx, artifactID)
	if err != nil {
		return nil, fmt.Errorf("campaigns.importRecipients: %w", err)
	}
	if refusal != "" {
		return nil, fmt.Errorf("campaigns.importRecipients: %s", refusal)
	}
	if ref.Archived {
		return nil, fmt.Errorf("campaigns.importRecipients: file %q is archived", ref.FileID)
	}
	if ref.Size > importMaxBytes {
		return nil, fmt.Errorf(
			"campaigns.importRecipients: file %q is %d bytes, over this import's %d-byte ceiling. "+
				"A recipient list is text; a file this size is more often a mis-selected artifact than a list",
			ref.FileID, ref.Size, importMaxBytes)
	}
	raw, err := w.readBlob(ctx, ref.BlobURL)
	if err != nil {
		return nil, fmt.Errorf("campaigns.importRecipients: reading file %q: %w", ref.FileID, err)
	}

	parsed, err := parseRecipientCSV(raw)
	if err != nil {
		return nil, fmt.Errorf("campaigns.importRecipients: %w", err)
	}

	// EXISTING MEMBERSHIP, via the server-side count for the ceiling and the
	// roster walk for the dedup set. The count is what the cap is measured
	// against -- measuring a page and calling it a total is the memql#3460
	// mistake -- while the walk is unavoidable, because "is this address
	// already here" is not a question a count can answer.
	existingSize, err := w.store.RosterSize(ctx, audienceID)
	if err != nil {
		return nil, fmt.Errorf("campaigns.importRecipients: %w", err)
	}
	roster, err := w.store.Roster(ctx, audienceID)
	if err != nil {
		return nil, fmt.Errorf("campaigns.importRecipients: %w", err)
	}
	held := make(map[string]struct{}, len(roster))
	for _, r := range roster {
		if normalized := NormalizeEmail(r.Email); normalized != "" {
			held[normalized] = struct{}{}
		}
	}

	result := importResult{Total: len(parsed.rows), InvalidLines: parsed.invalid}
	result.Invalid = parsed.invalidCount

	// Decide the whole import BEFORE writing anything.
	pending := make([]parsedRow, 0, len(parsed.rows))
	for _, row := range parsed.rows {
		if _, duplicate := held[row.normalized]; duplicate {
			result.Duplicates++
			continue
		}
		held[row.normalized] = struct{}{}
		pending = append(pending, row)
	}
	if would := existingSize + len(pending); would > w.cfg.MaxAudience {
		return nil, fmt.Errorf(
			"campaigns.importRecipients: this import would take audience %q to %d recipients, over the "+
				"%d ceiling (MEMQL_CAMPAIGNS_MAX_AUDIENCE). The import is refused WHOLE rather than "+
				"truncated: a partially imported list is one nobody knows is partial -- the audience "+
				"looks complete, the send looks complete, and the missing tail is invisible everywhere. "+
				"Split the file, or raise the ceiling if you mean a list this size",
			audienceID, would, w.cfg.MaxAudience)
	}

	now := w.nowUTC()
	for _, row := range pending {
		recipientID := id.NewShortId()
		if err := w.store.AddRecipient(ctx, recipientID, audienceID, row.normalized, row.displayName, importSourceValue, row.fields); err != nil {
			return nil, fmt.Errorf("campaigns.importRecipients: adding %s: %w", redactAddress(row.normalized), err)
		}
		result.Added++
		// A consent GRANT per added recipient, source "import". The stream is
		// what an export answers "when did this person opt in, and how" from,
		// and an import that added rows without one leaves every imported
		// address with no consent history at all -- which reads as "we never
		// had permission" to whoever has to answer for the list.
		if err := w.store.RecordConsent(ctx, ConsentGrant, ConsentRecord{
			EventID:     id.NewShortId(),
			EmailDigest: EmailDigest(row.normalized),
			Source:      importSourceValue,
			RecipientID: recipientID,
			OccurredAt:  now,
		}); err != nil {
			// The recipient IS added. A missing consent row is a gap in the
			// audit trail, not a reason to abandon a half-written import --
			// and re-running the import is safe, because dedup skips what is
			// already there.
			w.logger.Warn("campaigns: imported a recipient but could not record its consent grant",
				"audience", audienceID, "error", err)
		}
	}

	return resultNode("campaignImport", map[string]any{
		"audienceId":   audienceID,
		"artifactId":   artifactID,
		"added":        result.Added,
		"duplicates":   result.Duplicates,
		"invalid":      result.Invalid,
		"total":        result.Total,
		"invalidLines": invalidLinePayload(result.InvalidLines),
	})
}

// headerAsserted reports whether the caller left hasHeader alone or said
// true. ABSENT means true, which is the documented default and what every
// caller that has never heard of the flag sends.
func headerAsserted(raw any) bool {
	switch v := raw.(type) {
	case nil:
		return true
	case bool:
		return v
	case string:
		return !strings.EqualFold(strings.TrimSpace(v), "false")
	default:
		return true
	}
}

func invalidLinePayload(lines []invalidLine) []map[string]any {
	out := make([]map[string]any, 0, len(lines))
	for _, l := range lines {
		out = append(out, map[string]any{"line": l.Line, "reason": l.Reason, "value": l.Value})
	}
	return out
}

// --- the CSV ------------------------------------------------------------

type parsedRow struct {
	normalized  string
	displayName string
	fields      map[string]string
}

type parsedCSV struct {
	rows         []parsedRow
	invalid      []invalidLine
	invalidCount int
}

// parseRecipientCSV streams the file and classifies every row.
//
// FieldsPerRecord is -1 so a ragged row is a per-row rejection rather than an
// error that abandons the file. One mis-quoted line in a ten-thousand-row
// export is the common case, and refusing the whole import over it would send
// the operator back to a spreadsheet with no idea which line to look at --
// which is exactly what the per-line sample exists to prevent.
func parseRecipientCSV(raw []byte) (parsedCSV, error) {
	reader := csv.NewReader(bytes.NewReader(raw))
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true
	// LazyQuotes, because a real export contains a stray quote inside an
	// unquoted field more often than it contains a deliberately malformed
	// record, and the strict reader's answer to one is to abandon the file.
	reader.LazyQuotes = true

	header, err := reader.Read()
	if err == io.EOF {
		return parsedCSV{}, errors.New("the file is empty; a header row naming at least `email` is required")
	}
	if err != nil {
		return parsedCSV{}, fmt.Errorf("could not read the header row: %w", err)
	}
	mapping, err := mapColumns(header)
	if err != nil {
		return parsedCSV{}, err
	}

	out := parsedCSV{}
	line := 1 // the header
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		line++
		if err != nil {
			out.invalidCount++
			out.recordInvalid(line, "the row could not be read as CSV", "")
			continue
		}
		if blankRecord(record) {
			// A trailing blank line is not an error and must not be counted
			// as one -- every spreadsheet export has them, and reporting them
			// would put a permanent "3 invalid" on a clean file.
			continue
		}

		address := columnValue(record, mapping.email)
		normalized := NormalizeEmail(address)
		if normalized == "" || !plausibleAddress(normalized) {
			out.invalidCount++
			out.recordInvalid(line, "not a usable email address", redactAddress(address))
			continue
		}

		row := parsedRow{normalized: normalized, displayName: columnValue(record, mapping.displayName)}
		if len(mapping.extras) > 0 {
			row.fields = make(map[string]string, len(mapping.extras))
			for index, key := range mapping.extras {
				if v := strings.TrimSpace(columnValue(record, index)); v != "" {
					row.fields[key] = v
				}
			}
			if len(row.fields) == 0 {
				row.fields = nil
			}
		}
		out.rows = append(out.rows, row)
	}
	return out, nil
}

func (p *parsedCSV) recordInvalid(line int, reason, value string) {
	if len(p.invalid) >= importSampleCap {
		return
	}
	p.invalid = append(p.invalid, invalidLine{Line: line, Reason: reason, Value: value})
}

type columnMapping struct {
	email       int
	displayName int
	// extras maps a column INDEX to the key its values land under. Indexed
	// rather than named because two columns can share a header, and the
	// index is what disambiguates them at read time.
	extras map[int]string
}

// mapColumns reads the header.
//
// The address column is matched case-insensitively on `email`, which is what
// every export tool emits. A display name is `displayname` or `name`. A
// column with a BLANK header contributes no field: a key of "" is not
// addressable from a template, so carrying it would store data nothing can
// reach.
func mapColumns(header []string) (columnMapping, error) {
	mapping := columnMapping{email: -1, displayName: -1, extras: map[int]string{}}
	for i, raw := range header {
		name := strings.TrimSpace(strings.TrimPrefix(raw, utf8BOM))
		switch strings.ToLower(name) {
		case "email":
			if mapping.email < 0 {
				mapping.email = i
				continue
			}
		case "displayname", "name":
			if mapping.displayName < 0 {
				mapping.displayName = i
				continue
			}
		}
		if name != "" {
			mapping.extras[i] = name
		}
	}
	if mapping.email < 0 {
		return columnMapping{}, fmt.Errorf(
			"the header row has no `email` column (found: %s). The column mapping IS the header, and "+
				"guessing which column holds addresses is how an import mails the wrong list",
			strings.Join(quotedHeaders(header), ", "))
	}
	return mapping, nil
}

func quotedHeaders(header []string) []string {
	out := make([]string, 0, len(header))
	for _, h := range header {
		out = append(out, fmt.Sprintf("%q", strings.TrimSpace(h)))
	}
	return out
}

func columnValue(record []string, index int) string {
	if index < 0 || index >= len(record) {
		return ""
	}
	return strings.TrimSpace(record[index])
}

func blankRecord(record []string) bool {
	for _, cell := range record {
		if strings.TrimSpace(cell) != "" {
			return false
		}
	}
	return true
}

// plausibleAddress is the RFC-SHAPE check NormalizeEmail deliberately does
// not do.
//
// NormalizeEmail is a STORED FORMAT -- changing it changes what an existing
// suppression digest matches -- so it stays minimal: trim, lowercase, and
// refuse only what cannot be an address at all. Import validation is a
// different job with no such constraint, and it belongs here rather than
// there.
//
// Deliberately shape-only. There is no MX lookup and no syntax library: a
// stricter check refuses addresses that deliver, and the honest verdict on
// whether an address exists comes from the bounce path.
func plausibleAddress(normalized string) bool {
	at := strings.LastIndex(normalized, "@")
	if at <= 0 || at == len(normalized)-1 {
		return false
	}
	local, domain := normalized[:at], normalized[at+1:]
	if strings.ContainsAny(normalized, " \t\r\n,;<>\"") {
		return false
	}
	if strings.Contains(local, "@") {
		return false
	}
	// A domain needs a dot and a label on each side of it. "user@localhost"
	// is refused because it cannot receive mail from outside this machine,
	// and an audience full of them is an import somebody made from the wrong
	// column.
	dot := strings.LastIndex(domain, ".")
	if dot <= 0 || dot == len(domain)-1 {
		return false
	}
	if strings.Contains(domain, "..") || strings.HasPrefix(domain, ".") {
		return false
	}
	return true
}

// --- object storage -----------------------------------------------------

// readBlob fetches the file's bytes.
//
// Storage is resolved LAZILY and once, the shape component/sitepublish uses
// and for the same reason: the worker is constructed on every node type and
// receives no blob client, so resolving eagerly would either build an Azure
// client on nodes that never import or fail construction on a cluster with no
// blob storage at all. Unconfigured storage is therefore a per-call refusal,
// not a boot failure.
func (w *Worker) readBlob(ctx context.Context, blobURL string) ([]byte, error) {
	key := blobStorageKey(blobURL)
	if key == "" {
		return nil, errors.New("the file row records no storage location")
	}
	reader, container, err := w.objectStore(ctx)
	if err != nil {
		return nil, err
	}
	return reader.Download(ctx, container, key)
}

func (w *Worker) objectStore(ctx context.Context) (blobReader, string, error) {
	w.blobOnce.Do(func() {
		if w.newBlobReader == nil {
			w.newBlobReader = defaultBlobReader
		}
		w.blob, w.blobContainer, w.blobErr = w.newBlobReader(ctx)
	})
	return w.blob, w.blobContainer, w.blobErr
}

// defaultBlobReader builds the production reader from the same env the
// storage plug-in and the Library's own downloader already read -- never a
// second pair of storage variables, because an upload and the read that
// follows it must land in one container.
func defaultBlobReader(ctx context.Context) (blobReader, string, error) {
	container := azureblob.ContainerFromEnv()
	if strings.TrimSpace(container) == "" {
		return nil, "", errors.New("MEMQL_AZURE_BLOB_CONTAINER is not set on this node, so no uploaded file can be read")
	}
	uploader, err := azureblob.New(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("azure blob client: %w", err)
	}
	return uploader, container, nil
}

// blobStorageKey normalizes v1:library:file.blobUrl into the
// container-relative key. The field is documented as a STORAGE PATH
// ("library/{userId}/{fileId}/{name}") rather than a fetchable URL, so the
// only normalization is the two prefixes a caller might reasonably have
// stored.
func blobStorageKey(blobURL string) string {
	key := strings.TrimSpace(blobURL)
	key = strings.TrimPrefix(key, "blob://")
	return strings.TrimPrefix(key, "/")
}
