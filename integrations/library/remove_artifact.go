package library

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

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
// # Why raw SQL, and on which session
//
// "Every version of a row" is exactly what the engine's append-only writes
// will not touch. Archiving, the Library's removal for a person, is one more
// version that says archived. The statements run in one transaction on the
// engine's maintenance session (SetMaintenanceSession), not on the request
// pool: two of them read every comment and every snapshot that names the
// artifact, and the pool ends a statement at eight seconds.

const (
	conceptArtifact        = "v1:library:artifact"
	conceptGeneratedOutput = "v1:library:generatedOutput"
	conceptDocumentVersion = "v1:library:documentVersion"
	conceptDocumentComment = "v1:library:documentComment"
	kindGeneratedOutput    = "generated_output"
)

// SetMaintenanceSession attaches the opener of the engine's maintenance
// session, which the removal runs its statements on. Without one the removal
// refuses: it does not run on the request pool instead.
func (i *Integration) SetMaintenanceSession(open func(context.Context) (*sql.Conn, func(), error)) {
	i.maintain = open
}

// removalStatementLimit is how long the server lets one statement of a
// removal run, and removalStatementGrace how much later the client gives up
// on it. Two of the removal's statements read every comment that names the
// artifact and every snapshot of its history; the request pool ends a
// statement at eight seconds, so the removal runs on the maintenance session,
// as the erasure's own statements do, and bounds each statement itself.
const (
	removalStatementLimit = 10 * time.Minute
	removalStatementGrace = 15 * time.Second
)

// handleRemoveArtifact is libraryRemoveArtifact: it removes one generated
// output entirely and answers how many row versions went (`removed`) and how
// many a second look still found (`remaining`, 0). The output is named by its
// own reference (sourceConceptRef), which is what an index row indexes, and by
// nothing else: named by an index row's id, an output indexed under its bare
// id was left where it was while its index row went. Safe to make twice: a
// second call finds nothing and answers 0 and 0.
//
// staged-data: MUST-NOT-GATE -- a staged version of the artifact skipped here
// SURVIVES ITS REMOVAL, and publishes later as if nothing had been removed.
func (i *Integration) handleRemoveArtifact(ctx context.Context, args map[string]any, _ int) ([]memorynodes.MemoryNode, error) {
	if !auth.OriginFromContext(ctx).IsInternal() {
		return nil, fmt.Errorf("library: libraryRemoveArtifact is the engine's own and refuses a client-origin call")
	}
	ref := strings.TrimSpace(stringArg(args, "sourceConceptRef"))
	if ref == "" {
		return nil, fmt.Errorf("library: libraryRemoveArtifact needs sourceConceptRef, the reference of the output to remove")
	}
	if strings.Contains(ref, ":") && !strings.HasPrefix(ref, conceptGeneratedOutput+":") {
		return nil, fmt.Errorf("library: libraryRemoveArtifact removes a generated output, whose whole content is its own row; this reference is to a row of another kind, and nothing was removed")
	}
	if i.maintain == nil {
		return nil, fmt.Errorf("library: libraryRemoveArtifact needs the engine's maintenance session, and this integration was given none; it does not run on the request pool instead")
	}
	conn, done, err := i.maintain(ctx)
	if err != nil || conn == nil || done == nil {
		return nil, fmt.Errorf("library: libraryRemoveArtifact could not open the maintenance session: %w", err)
	}
	defer done()
	bounded, cancel := context.WithTimeout(ctx, 4*removalStatementLimit+removalStatementGrace)
	defer cancel()
	if _, err := conn.ExecContext(bounded, "SET statement_timeout = "+strconv.FormatInt(removalStatementLimit.Milliseconds(), 10)); err != nil {
		return nil, fmt.Errorf("library: remove artifact: bound its statements: %w", err)
	}
	tx, err := conn.BeginTx(bounded, nil)
	if err != nil {
		return nil, fmt.Errorf("library: remove artifact: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	found, err := findArtifact(bounded, tx, ref)
	if err != nil {
		return nil, err
	}
	if why := found.contentElsewhere(); why != "" {
		return nil, fmt.Errorf("library: libraryRemoveArtifact removes a generated output, whose whole content is its own row; this artifact %s, and nothing was removed", why)
	}
	var removed int64
	for _, part := range found.parts() {
		res, err := tx.ExecContext(bounded, `DELETE FROM "MemoryNodes" WHERE concept = $1 AND `+part.where, part.args...)
		if err != nil {
			return nil, fmt.Errorf("library: remove artifact: %s: %w", part.args[0], err)
		}
		n, _ := res.RowsAffected()
		removed += n
	}
	// The second look, before the removal is made final: what a read of the
	// same rows still finds.
	var remaining int64
	for _, part := range found.parts() {
		var n int64
		if err := tx.QueryRowContext(bounded, `SELECT count(*) FROM "MemoryNodes" WHERE concept = $1 AND `+part.where, part.args...).Scan(&n); err != nil {
			return nil, fmt.Errorf("library: remove artifact: look again at %s: %w", part.args[0], err)
		}
		remaining += n
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("library: remove artifact: %w", err)
	}
	return reviewResult(map[string]any{"removed": removed, "remaining": remaining})
}

// foundArtifact is what one output is in the table: its own row, its owner,
// and the owner's index rows of it.
type foundArtifact struct {
	// output is the output's id as the table spells it, and bare the same
	// without its concept; owner is who it belongs to, empty when the table
	// holds no row of it.
	output, bare, owner string
	// artifacts are the ids of the OWNER'S index rows that index it, and
	// kinds the kinds they carry.
	artifacts, kinds []string
	// attached is set when a version of the output keeps its bytes in an
	// attachment.
	attached bool
}

// ownerOf is a row's owner without the concept a user's id can be written
// with, so the two spellings of one person compare equal.
const ownerOf = `regexp_replace(COALESCE(payload->>'ownerUserId', ''), '^v1:identity:user:', '')`

// findArtifact reads the output the caller named, and the index rows that
// are ITS OWNER'S.
//
// ONLY THE OWNER'S INDEX ROWS COUNT. An index row is anybody's to file, and it
// names what it indexes by a reference its writer chose. A row another person
// filed that names this output is not this artifact: it is neither removed,
// nor looked at for its kind. Were it, a signed-in person who learned an
// export's id could file a row of another kind that names it, and the removal
// would refuse for ever.
//
// staged-data: MUST-NOT-GATE -- a staged index row or output not seen here is
// left behind by the removal, and publishes later as if nothing was removed.
func findArtifact(ctx context.Context, tx *sql.Tx, ref string) (foundArtifact, error) {
	found := foundArtifact{bare: memql.BareShortId(ref)}
	found.output = conceptGeneratedOutput + ":" + found.bare
	rows, err := tx.QueryContext(ctx,
		`SELECT `+ownerOf+`, COALESCE(payload->>'attachmentId', '') <> '' FROM "MemoryNodes" WHERE concept = $1 AND id = $2 ORDER BY "createdAt"`,
		conceptGeneratedOutput, found.output)
	if err != nil {
		return found, fmt.Errorf("library: remove artifact: read the output: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var attached bool
		if err := rows.Scan(&found.owner, &attached); err != nil {
			return found, fmt.Errorf("library: remove artifact: read the output: %w", err)
		}
		found.attached = found.attached || attached
	}
	if err := rows.Err(); err != nil {
		return found, fmt.Errorf("library: remove artifact: read the output: %w", err)
	}
	if found.owner == "" {
		// No row of the output: nothing of it is the Library's to remove.
		return found, nil
	}
	index, err := tx.QueryContext(ctx,
		`SELECT DISTINCT id, COALESCE(payload->>'kind', '') FROM "MemoryNodes"
		  WHERE concept = $1 AND (payload->>'sourceConceptRef' = $2 OR payload->>'sourceConceptRef' = $3) AND `+ownerOf+` = $4`,
		conceptArtifact, found.output, found.bare, found.owner)
	if err != nil {
		return found, fmt.Errorf("library: remove artifact: read the index row: %w", err)
	}
	defer index.Close()
	for index.Next() {
		var id, kind string
		if err := index.Scan(&id, &kind); err != nil {
			return found, fmt.Errorf("library: remove artifact: read the index row: %w", err)
		}
		found.artifacts = appendOnce(found.artifacts, id)
		found.kinds = appendOnce(found.kinds, kind)
	}
	return found, index.Err()
}

// contentElsewhere says why this removal cannot remove the artifact whole, or
// nothing when it can.
func (f foundArtifact) contentElsewhere() string {
	for _, kind := range f.kinds {
		if kind != kindGeneratedOutput {
			return fmt.Sprintf("is indexed by its owner as a %s, whose content is kept outside the rows this removes", kind)
		}
	}
	if f.attached {
		return "keeps its bytes in an attachment"
	}
	return ""
}

// artifactPart is one kind of row the Library keeps for an artifact: the
// condition that names this artifact's rows of it, and its arguments, the
// concept first.
type artifactPart struct {
	where string
	args  []any
}

// parts is every row the Library keeps for the artifact, as the deletes and
// the second look both name them: what hangs from it first, the index row
// last. Nothing when the table holds no row of the output.
func (f foundArtifact) parts() []artifactPart {
	if f.owner == "" {
		return nil
	}
	var parts []artifactPart
	if len(f.artifacts) > 0 {
		// A comment names the index row, bare or by its full id; whoever
		// wrote it, it is about the document that goes.
		parts = append(parts, artifactPart{`payload->>'artifactId' IN (SELECT jsonb_array_elements_text($2::jsonb))`,
			[]any{conceptDocumentComment, jsonList(bothSpellings(f.artifacts))}})
	}
	parts = append(parts,
		// A snapshot of the history names the document, bare or by its full
		// id. Each spelling is asked by itself, which the index on a
		// version's document can answer; and only the owner's snapshots are
		// the document's.
		artifactPart{`(payload->>'documentId' = $2 OR payload->>'documentId' = $3) AND ` + ownerOf + ` = $4`,
			[]any{conceptDocumentVersion, f.bare, f.output, f.owner}},
		artifactPart{`id = $2`, []any{conceptGeneratedOutput, f.output}})
	if len(f.artifacts) > 0 {
		// The owner's versions of the index row, and no other's. An index
		// row's id is made from what it indexes, so another person who files
		// a row naming this output writes a version under the SAME id; that
		// version is theirs and stays.
		parts = append(parts, artifactPart{`id IN (SELECT jsonb_array_elements_text($2::jsonb)) AND ` + ownerOf + ` = $3`,
			[]any{conceptArtifact, jsonList(f.artifacts), f.owner}})
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
