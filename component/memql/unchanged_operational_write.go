package memql

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

const inboundRequestConcept = "v1:platform:inboundRequest"

func skipUnchangedOperationalWrite(concept string) bool {
	switch concept {
	case inboundRequestConcept, "v1:platform:customDomain", "v1:platform:syncState":
		return true
	default:
		return false
	}
}

// Operational status records and retried deliveries write only changed content.
// A repeated delivery must not redispatch. Compare only after the normal
// merge, create-only fields, ownership guards and schema validation, inside
// the transaction holding the target's observed-version fence.
type unchangedOperationalStore struct {
	memorynodes.Store
	unchanged bool
}

func (s *unchangedOperationalStore) InsertMemoryNode(ctx context.Context, node *memorynodes.MemoryNode) error {
	prior, err := s.Store.QueryMemoryNodes(ctx, memorynodes.QueryParams{IDs: []string{node.ID}, Concept: node.Concept, Limit: 1})
	if err != nil {
		return err
	}
	if len(prior) == 1 && prior[0].Type == node.Type && sameJSON(prior[0].Payload, node.Payload) && sameJSON(prior[0].Schema, node.Schema) {
		*node = prior[0]
		s.unchanged = true
		return nil
	}
	return s.Store.InsertMemoryNode(ctx, node)
}

// UseNumber keeps adjacent large integer values distinct. JSONB changes key
// order, so byte equality is not the stored payload's equality.
func sameJSON(a, b []byte) bool {
	decode := func(raw []byte) (any, error) {
		var value any
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		err := d.Decode(&value)
		return value, err
	}
	left, err := decode(a)
	if err != nil {
		return false
	}
	right, err := decode(b)
	return err == nil && reflect.DeepEqual(left, right)
}
