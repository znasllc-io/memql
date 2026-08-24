package datasync

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	langparser "github.com/znasllc-io/memql/component/language/parser"
	"github.com/znasllc-io/memql/component/memql"
	memqlsync "github.com/znasllc-io/memql/component/memql/sync"
)

// mirror_writer.go -- the generic write into a mirror.
//
// A connector returns MirrorWrites; something has to turn them into rows.
// That something cannot be per-concept code, because the runtime serves
// whatever concepts a connector declares and has never heard of any of
// them -- so it uses the engine's RAW insert form,
// `insert("<concept>", id=..., payload={...})`, which is concept-agnostic
// by construction.
//
// # Why a raw insert is the right instrument here and not a shortcut
//
// Everywhere else in this package the rule is the opposite: reach for a
// NAMED construct, because it carries its filter and its declared
// binding. That rule is about READS with a caller to scope. This is a
// write into a concept the runtime cannot know the mutations of, made by
// an actor the concept itself names, and it passes through exactly the
// gates a named mutation would: executeWrite is the single write
// chokepoint, so the mirror guard, the row-authz write guard, schema
// validation and relationship canonicalization all run on it.
//
// What a raw insert skips is the mutation TEMPLATE -- accept/stamp -- and
// there is no template to skip: the payload is the origin's, field for
// field, which is what a faithful mirror means.
//
// # The version the guard reads
//
// StoredVersion answers "what does MemQL hold for this row" in the two
// shapes DomainSpec allows, and the split is described in inbound.go. The
// row is read through a raw concept read for the same reason the write is
// raw: the runtime has no named query for a concept it has never heard
// of. Row admission is the guard on that read, and it admits the
// connector actor to exactly the concepts naming it.

// EngineMirrorWriter applies mirror writes through the engine.
type EngineMirrorWriter struct{ engine Engine }

// NewEngineMirrorWriter wraps an engine.
func NewEngineMirrorWriter(engine Engine) *EngineMirrorWriter {
	return &EngineMirrorWriter{engine: engine}
}

// WriteMirror applies one write. The ctx must already carry the
// connector actor -- the Applier stamps it, and without it the engine
// refuses with mirror_write_refused, which is the protection working.
func (w *EngineMirrorWriter) WriteMirror(ctx context.Context, connector string, mw memqlsync.MirrorWrite) error {
	if w == nil || w.engine == nil {
		return fmt.Errorf("datasync: no engine wired for mirror writes")
	}
	conceptID := strings.TrimSpace(mw.Concept)
	rowID := strings.TrimSpace(mw.RowId)
	if conceptID == "" || rowID == "" {
		return fmt.Errorf("datasync: connector %q returned a write naming no concept or no row", connector)
	}

	payload := map[string]any{}
	for k, v := range mw.Payload {
		payload[k] = v
	}
	if mw.Retire {
		// A retirement is an ordinary append that marks the row gone --
		// MemQL has no hard delete. Both conventions the tree uses are
		// written, because the runtime does not know which one the
		// concept declares and a field the schema does not have is
		// refused, not silently kept. See retireFieldsFor.
		for k, v := range retireFieldsFor(payload) {
			payload[k] = v
		}
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("datasync: encoding the payload for %s %q: %w", conceptID, rowID, err)
	}
	q := fmt.Sprintf("insert(%s, id=%s, payload=%s)",
		langparser.QuoteString(conceptID), langparser.QuoteString(rowID), string(raw))
	if _, err := w.engine.Execute(ctx, q); err != nil {
		return fmt.Errorf("datasync: writing %s %q: %w", conceptID, rowID, err)
	}
	return nil
}

// retireFieldsFor returns the fields that mark a row gone, given what
// the payload already carries.
//
// The connector is the authority on its own concept's shape, so if it
// already set a retirement field the runtime adds nothing. Otherwise it
// adds `deleted: true`, the tree's more common convention. It does NOT
// add both: a concept declaring only one would refuse the write for the
// other, and a refused retirement leaves a row the origin has deleted
// still live in the mirror.
func retireFieldsFor(payload map[string]any) map[string]any {
	if payload != nil {
		if _, has := payload["deleted"]; has {
			return nil
		}
		if _, has := payload["active"]; has {
			return nil
		}
		if _, has := payload["present"]; has {
			return nil
		}
	}
	return map[string]any{"deleted": true}
}

// StoredVersion returns the version MemQL holds for a row.
func (w *EngineMirrorWriter) StoredVersion(ctx context.Context, spec memqlsync.DomainSpec, rowID string) (string, bool, error) {
	if w == nil || w.engine == nil {
		return "", false, fmt.Errorf("datasync: no engine wired for mirror reads")
	}
	conceptID := strings.TrimSpace(spec.Concept)
	rowID = strings.TrimSpace(rowID)
	if conceptID == "" || rowID == "" {
		return "", false, nil
	}

	q := fmt.Sprintf("concept==%s && id==%s", conceptID, rowID)
	res, err := w.engine.Execute(ctx, q)
	if err != nil {
		return "", false, fmt.Errorf("datasync: reading the stored version of %s %q: %w", conceptID, rowID, err)
	}
	rows := memql.MaterializeRows(res)
	if len(rows) == 0 {
		return "", false, nil
	}
	row := rows[0]

	if field := strings.TrimSpace(spec.VersionField); field != "" {
		// The origin's own version, kept on the row. Exact.
		return stringField(row, field), true, nil
	}
	// The fallback (D6): the origin publishes no version, so the version
	// is when MemQL last applied a delivery for this row.
	if t := timeField(row, "createdAt"); !t.IsZero() {
		return t.UTC().Format(time.RFC3339Nano), true, nil
	}
	// A row whose createdAt cannot be read is a row we cannot order
	// against. Reported as EXISTING WITH NO VERSION, which the guard
	// reads as "not stale" -- applying a possibly-older write is
	// recoverable by the next reconciliation, while refusing every write
	// to such a row is not.
	return "", true, nil
}
