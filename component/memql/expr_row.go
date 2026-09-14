package memql

import (
	"encoding/json"
	"fmt"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// expr_row.go -- a stored row as an edition-2026 expression reads it
// (epic memql#5363, D1).
//
// In v1 a payload field is never a bare name: it is always `<param>.field`,
// where the lambda parameter is the row. So `row.status` and `row.id` are both
// member reads on one value, and the row has to answer both -- the six row
// intrinsics from its own columns, everything else from its payload. The SQL
// twin draws the same line (a FieldReference on an intrinsic column versus a
// JSONB path under payload), and ExprRow draws it with the SAME registry,
// resolveIntrinsicField, so the two evaluators cannot disagree about which
// names are columns.

// ExprRow is a row as `row.x` reads it: ID, Concept, Type, CreatedBy,
// CreatedAt and Provenance are the intrinsics (`row.id`, `row.concept`,
// `row.type`, `row.createdBy`, `row.createdAt`, `row.provenance`); any other
// name reads Payload.
type ExprRow struct {
	ID, Concept, Type, CreatedBy string
	CreatedAt                    time.Time
	Provenance                   map[string]any
	Payload                      map[string]any
}

// NewExprRow builds the ExprRow of a stored node: its columns as the
// intrinsics and its payload and provenance decoded the way the engine
// decodes them everywhere else (payloadToMap: numbers as float64). An empty
// payload or provenance is a nil map -- a row with nothing in it, not an
// error -- and malformed JSON is an error, never a silently empty row.
func NewExprRow(node memorynodes.MemoryNode) (ExprRow, error) {
	row := ExprRow{
		ID:        node.ID,
		Concept:   node.Concept,
		Type:      node.Type,
		CreatedBy: node.CreatedBy,
		CreatedAt: node.CreatedAt,
	}
	if len(node.Payload) > 0 {
		payload, err := payloadToMap(node.Payload)
		if err != nil {
			return ExprRow{}, fmt.Errorf("row %s: decode payload: %w", node.ID, err)
		}
		row.Payload = payload
	}
	if len(node.Provenance) > 0 {
		var provenance map[string]any
		if err := json.Unmarshal(node.Provenance, &provenance); err != nil {
			return ExprRow{}, fmt.Errorf("row %s: decode provenance: %w", node.ID, err)
		}
		row.Provenance = provenance
	}
	return row, nil
}

// exprMember reads one field of the row. An intrinsic name reads the column;
// case-insensitively, because resolveIntrinsicField -- the SQL side's registry
// -- matches that way, and a name the two sides resolved differently would be
// a column in SQL and a payload key in process. Every other name reads the
// payload, and a missing key is Absent.
//
// A zero CreatedAt is Absent rather than "0001-01-01T00:00:00Z": a stored row
// always has one, so a zero value is a hand-built row that never set it, and
// reporting the year 1 as its creation time would be a value nobody wrote.
func (r ExprRow) exprMember(field string) any {
	if info, ok := resolveIntrinsicField(field); ok {
		switch info.kind {
		case intrinsicFieldId:
			return r.ID
		case intrinsicFieldConcept:
			return r.Concept
		case intrinsicFieldType:
			return r.Type
		case intrinsicFieldCreatedBy:
			return r.CreatedBy
		case intrinsicFieldCreatedAt:
			if r.CreatedAt.IsZero() {
				return Absent
			}
			return r.CreatedAt.UTC().Format(time.RFC3339Nano)
		case intrinsicFieldProvenance:
			if r.Provenance == nil {
				return Absent
			}
			return r.Provenance
		}
	}
	if v, ok := r.Payload[field]; ok {
		return v
	}
	return Absent
}
