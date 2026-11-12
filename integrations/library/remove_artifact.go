package library

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/znasllc-io/memql/component/auth"
	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
	"github.com/znasllc-io/memql/component/memql"
)

// remove_artifact.go -- libraryRemoveArtifact, the Library's one removal that
// removes.
//
// # What an artifact is, in rows
//
// An item of the Library is an INDEX ROW, a v1:library:artifact, that points
// at the row holding the content (sourceConceptRef) and carries what the list
// shows: title, summary, labels, folder, the accounts it is shared with. What
// else there is depends on the item's kind:
//
//   - generated_output: the content is a v1:library:generatedOutput row, whose
//     body IS the document. Its history is v1:library:documentVersion rows
//     that name it (documentId, spelled bare or as the full id), and the
//     feedback and notes on it are v1:library:documentComment rows that name
//     the index row (artifactId).
//   - file: a v1:library:file row, v1:library:fileVersion rows for its earlier
//     versions, v1:library:fileChunk rows for its extracted text, and
//     v1:library:uploadSession rows for uploads of it. ITS BYTES ARE NOT IN
//     THE TABLE: they are objects in blob storage, one for each version.
//   - memory: a v1:library:memory row.
//   - document, note, todo, calendar_event, live_source: the content is a row
//     of ANOTHER domain; the Library keeps the index row and nothing else.
//
// Every one of those rows is append-only: each edit, each archive, each label
// is another version under the same id.
//
// # What this removes, and what it refuses
//
// It removes a GENERATED OUTPUT, entirely: every version of the output's row,
// of its index row, of its history and of the comments on it. That is the one
// kind whose whole content is rows the Library keeps, and the one the engine
// has to remove: a data request's export is a generated output.
//
// Every other kind is REFUSED, with nothing removed. A file's bytes would
// stay in blob storage and a document's or a note's content in another
// domain's row, and a removal that left them and answered "gone" would be
// worse than no removal: the caller is discharging an obligation and would
// record it met. So is a generated output that keeps its bytes in an
// attachment (attachmentId), for the same reason.
//
// # Who may call it
//
// The engine, and nobody else. A builtin carries no wire bar of its own, so
// without the refusal below any signed-in user could remove any artifact by
// naming it. The refusal comes BEFORE anything is read: not the artifact's
// owner, and not a cluster owner, is told whether an id names something.
//
// # Why raw SQL
//
// "Every version of a row" is exactly what the engine's append-only writes
// will not touch. Archiving, the Library's removal for a person, is one more
// version that says archived.

const (
	conceptArtifact        = "v1:library:artifact"
	conceptGeneratedOutput = "v1:library:generatedOutput"
	conceptDocumentVersion = "v1:library:documentVersion"
	conceptDocumentComment = "v1:library:documentComment"
	kindGeneratedOutput    = "generated_output"
)

// SetDatabase attaches the raw handle the removal deletes on.
func (i *Integration) SetDatabase(db func() *sql.DB) { i.removalDB = db }

// handleRemoveArtifact is libraryRemoveArtifact: it removes one generated
// output entirely and answers how many row versions went (`removed`) and how
// many a second look still found (`remaining`, 0). Named by what the index
// row indexes (sourceConceptRef) or by the index row's id (artifactId). Safe
// to make twice: a second call finds nothing and answers 0 and 0.
//
// staged-data: MUST-NOT-GATE -- a staged version of the artifact skipped here
// SURVIVES ITS REMOVAL, and publishes later as if nothing had been removed.
func (i *Integration) handleRemoveArtifact(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if !auth.OriginFromContext(ctx).IsInternal() {
		return nil, fmt.Errorf("library: libraryRemoveArtifact is the engine's own and refuses a client-origin call")
	}
	ref, artifactID := strings.TrimSpace(stringArg(args, "sourceConceptRef")), strings.TrimSpace(stringArg(args, "artifactId"))
	if ref == "" && artifactID == "" {
		return nil, fmt.Errorf("library: libraryRemoveArtifact needs sourceConceptRef or artifactId")
	}
	if i.removalDB == nil || i.removalDB() == nil {
		return nil, fmt.Errorf("library: libraryRemoveArtifact has no database handle to remove on")
	}
	db := i.removalDB()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("library: remove artifact: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	found, err := findArtifact(ctx, tx, ref, artifactID)
	if err != nil {
		return nil, err
	}
	if why := found.contentElsewhere(); why != "" {
		return nil, fmt.Errorf("library: libraryRemoveArtifact removes a generated output, whose whole content is its own row; this artifact %s, and nothing was removed", why)
	}
	var removed int64
	for _, part := range found.parts() {
		res, err := tx.ExecContext(ctx, `DELETE FROM "MemoryNodes" WHERE concept = $1 AND `+part.where, part.concept, part.named)
		if err != nil {
			return nil, fmt.Errorf("library: remove artifact: %s: %w", part.concept, err)
		}
		n, _ := res.RowsAffected()
		removed += n
	}
	// The second look, before the removal is made final: what a read of the
	// same rows still finds.
	var remaining int64
	for _, part := range found.parts() {
		var n int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM "MemoryNodes" WHERE concept = $1 AND `+part.where, part.concept, part.named).Scan(&n); err != nil {
			return nil, fmt.Errorf("library: remove artifact: look again at %s: %w", part.concept, err)
		}
		remaining += n
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("library: remove artifact: %w", err)
	}
	return reviewResult(map[string]any{"removed": removed, "remaining": remaining})
}

// foundArtifact is what one artifact is in the table: its index rows, the
// rows that hold its content, and what says its content is elsewhere.
type foundArtifact struct {
	// artifacts are the ids of its index rows, and outputs the ids of the
	// generated outputs they index, as the table spells them.
	artifacts, outputs []string
	// kinds are the kinds its index rows carry, and refs what they index.
	kinds, refs []string
	// attached is set when a version of the output keeps its bytes in an
	// attachment.
	attached bool
}

// findArtifact reads what the caller named: the index rows by id or by what
// they index, and from them the output.
//
// staged-data: MUST-NOT-GATE -- a staged index row or output not seen here is
// left behind by the removal, and publishes later as if nothing was removed.
func findArtifact(ctx context.Context, tx *sql.Tx, ref, artifactID string) (foundArtifact, error) {
	var found foundArtifact
	named := []string{}
	if ref != "" {
		named = append(named, ref, memql.BareShortId(ref))
	}
	id := ""
	if artifactID != "" {
		id = conceptArtifact + ":" + memql.BareShortId(artifactID)
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT DISTINCT id, COALESCE(payload->>'kind', ''), COALESCE(payload->>'sourceConceptRef', '')
		   FROM "MemoryNodes"
		  WHERE concept = $1 AND (id = $2 OR payload->>'sourceConceptRef' IN (SELECT jsonb_array_elements_text($3::jsonb)))`,
		conceptArtifact, id, jsonList(named))
	if err != nil {
		return found, fmt.Errorf("library: remove artifact: read the index row: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var rowID, kind, indexed string
		if err := rows.Scan(&rowID, &kind, &indexed); err != nil {
			return found, fmt.Errorf("library: remove artifact: read the index row: %w", err)
		}
		found.artifacts = appendOnce(found.artifacts, rowID)
		found.kinds = appendOnce(found.kinds, kind)
		found.refs = appendOnce(found.refs, indexed)
	}
	if err := rows.Err(); err != nil {
		return found, fmt.Errorf("library: remove artifact: read the index row: %w", err)
	}
	if ref != "" {
		found.refs = appendOnce(found.refs, ref)
	}
	for _, indexed := range found.refs {
		if indexed == "" {
			continue
		}
		// A reference with no concept in it is a generated output's bare id
		// only when the caller said so by naming it as one; a bare id is
		// otherwise not something this can place.
		if strings.HasPrefix(indexed, conceptGeneratedOutput+":") || (!strings.Contains(indexed, ":") && ref == indexed) {
			found.outputs = appendOnce(found.outputs, conceptGeneratedOutput+":"+memql.BareShortId(indexed))
		}
	}
	if len(found.outputs) > 0 {
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM "MemoryNodes" WHERE concept = $1 AND id IN (SELECT jsonb_array_elements_text($2::jsonb)) AND COALESCE(payload->>'attachmentId', '') <> '')`,
			conceptGeneratedOutput, jsonList(found.outputs)).Scan(&found.attached); err != nil {
			return found, fmt.Errorf("library: remove artifact: read the output: %w", err)
		}
	}
	return found, nil
}

// contentElsewhere says why this removal cannot remove the artifact whole, or
// nothing when it can.
func (f foundArtifact) contentElsewhere() string {
	for _, kind := range f.kinds {
		if kind != kindGeneratedOutput {
			return fmt.Sprintf("is a %s, whose content is kept outside the rows this removes", kind)
		}
	}
	for _, indexed := range f.refs {
		if strings.Contains(indexed, ":") && !strings.HasPrefix(indexed, conceptGeneratedOutput+":") {
			return "indexes a row that is not a generated output"
		}
	}
	if f.attached {
		return "keeps its bytes in an attachment"
	}
	return ""
}

// artifactPart is one kind of row the Library keeps for an artifact: its
// concept, and the condition that names this artifact's rows of it by $2.
type artifactPart struct {
	concept, where, named string
}

// parts is every row the Library keeps for the artifact, as the deletes and
// the second look both name them: what hangs from it first, the index row
// last.
func (f foundArtifact) parts() []artifactPart {
	const byID = `id IN (SELECT jsonb_array_elements_text($2::jsonb))`
	var parts []artifactPart
	if len(f.artifacts) > 0 {
		// A comment names the index row, bare or by its full id.
		parts = append(parts, artifactPart{conceptDocumentComment, `payload->>'artifactId' IN (SELECT jsonb_array_elements_text($2::jsonb))`, jsonList(bothSpellings(f.artifacts))})
	}
	if len(f.outputs) > 0 {
		// A snapshot of the history names the document, bare or by its full id.
		parts = append(parts,
			artifactPart{conceptDocumentVersion, `payload->>'documentId' IN (SELECT jsonb_array_elements_text($2::jsonb))`, jsonList(bothSpellings(f.outputs))},
			artifactPart{conceptGeneratedOutput, byID, jsonList(f.outputs)})
	}
	if len(f.artifacts) > 0 {
		parts = append(parts, artifactPart{conceptArtifact, byID, jsonList(f.artifacts)})
	}
	return parts
}

// bothSpellings is each id as the table holds it and as a reference to it is
// often written, without its concept.
func bothSpellings(ids []string) []string {
	var out []string
	for _, id := range ids {
		out = appendOnce(appendOnce(out, id), memql.BareShortId(id))
	}
	return out
}

func appendOnce(list []string, item string) []string {
	for _, have := range list {
		if have == item {
			return list
		}
	}
	return append(list, item)
}

// jsonList is a list of strings as the one text parameter a statement takes
// it in.
func jsonList(items []string) string {
	if items == nil {
		items = []string{}
	}
	raw, _ := json.Marshal(items)
	return string(raw)
}

func stringArg(args map[string]any, name string) string {
	s, _ := args[name].(string)
	return s
}
