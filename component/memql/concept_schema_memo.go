package memql

// concept_schema_memo.go -- a concept's definition schema, decoded once per
// process per distinct document.
//
// Every construct the load checks asks two questions of its bound concept's
// schema -- which fields it declares (flattenConceptFields) and which object
// paths it closes (closedObjectPaths) -- and the lowerer, the condition and
// type checks, the sort check (memql#5429), the write-shaping check and the
// tool-gate enum read each asked by decoding the whole JSON document again. A
// boot decoded the same few hundred documents thousands of times: ~0.08s of an
// Init under GOMAXPROCS=2, grown by the two checks this branch added (PR
// #5693).
//
// The key is the DOCUMENT, not the concept's name: an authored bundle or a
// re-registered fixture that changes a schema is a new key and is decoded
// anew, so the memo cannot answer for a schema it has not read. The bound and
// the clear-on-overflow policy are sourceMemo's.
//
// Each caller gets its own copy of what was decoded, so nothing a caller does
// with its map can reach the memo or another caller.

import (
	"encoding/json"
	"sync"
	"sync/atomic"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// conceptSchemaMemoMaxBytes bounds the memo by the bytes of the distinct
// schema documents it holds.
const conceptSchemaMemoMaxBytes = 16 << 20

// conceptSchemaDecodes counts document decodes, so the sharing is asserted
// as a count (TestConceptSchemaDecodesOncePerDocument) rather than inferred
// from wall time.
var conceptSchemaDecodes atomic.Int64

var conceptSchemaMemo struct {
	mu         sync.Mutex
	byDocument map[string]*decodedConceptSchema
	bytes      int
}

// decodedConceptSchema is what the load reads off one schema document.
type decodedConceptSchema struct {
	once   sync.Once
	raw    []byte
	fields map[string]conceptFieldShape
	closed map[string]bool
	err    error
}

func (d *decodedConceptSchema) decode() *decodedConceptSchema {
	d.once.Do(func() {
		conceptSchemaDecodes.Add(1)
		var doc map[string]any
		err := json.Unmarshal(d.raw, &doc)
		d.raw = nil // the key holds the document; this copy was for the decode
		if err != nil {
			d.err = err
			return
		}
		d.fields = map[string]conceptFieldShape{}
		collectConceptSchemaFields("", doc, d.fields)
		d.closed = map[string]bool{}
		collectClosedObjectPaths("", doc, d.closed)
	})
	return d
}

// decodedSchemaOf is c's definition schema, decoded.
func decodedSchemaOf(c *memorynodes.Concept) (*decodedConceptSchema, error) {
	raw, err := c.DefinitionSchema()
	if err != nil {
		return nil, err
	}
	m := &conceptSchemaMemo
	m.mu.Lock()
	d := m.byDocument[string(raw)]
	if d == nil {
		// A copy: the concept's own buffer is not the memo's to hold.
		d = &decodedConceptSchema{raw: append([]byte(nil), raw...)}
		if len(raw) <= conceptSchemaMemoMaxBytes {
			if m.byDocument == nil || m.bytes+len(raw) > conceptSchemaMemoMaxBytes {
				m.byDocument = map[string]*decodedConceptSchema{}
				m.bytes = 0
			}
			m.byDocument[string(raw)] = d
			m.bytes += len(raw)
		}
	}
	m.mu.Unlock()
	return d.decode(), nil
}

// copyFieldShapes is fields as the caller's own map, enum lists included.
func copyFieldShapes(fields map[string]conceptFieldShape) map[string]conceptFieldShape {
	out := make(map[string]conceptFieldShape, len(fields))
	for path, shape := range fields {
		if shape.Enum != nil {
			shape.Enum = append([]string(nil), shape.Enum...)
		}
		out[path] = shape
	}
	return out
}

// copyClosedPaths is closed as the caller's own map.
func copyClosedPaths(closed map[string]bool) map[string]bool {
	out := make(map[string]bool, len(closed))
	for path, v := range closed {
		out[path] = v
	}
	return out
}
