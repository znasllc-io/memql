package memoryNodes

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/uptrace/bun"
)

const schemaReferencePrefix = "memql-schema:"

// HydrateSchemas restores the exact schema version referenced by each row.
// Call at public read and archive boundaries, never substitute the currently
// loaded concept: older versions can have a different definition or be deletes.
// Inline schemas remain readable while the background conversion progresses.
func HydrateSchemas(ctx context.Context, db bun.IDB, nodes []MemoryNode) error {
	refs := make(map[string][]int)
	for n := range nodes {
		var ref struct {
			Ref string `json:"$ref"`
		}
		if err := json.Unmarshal(nodes[n].Schema, &ref); err != nil {
			if len(nodes[n].Schema) == 0 {
				continue
			}
			return fmt.Errorf("decode row schema: %w", err)
		}
		if strings.HasPrefix(ref.Ref, schemaReferencePrefix) {
			key := strings.TrimPrefix(ref.Ref, schemaReferencePrefix)
			refs[key] = append(refs[key], n)
		}
	}
	if len(refs) == 0 {
		return nil
	}
	if db == nil {
		return ErrBunNotConfigured
	}
	keys := make([]string, 0, len(refs))
	for key := range refs {
		keys = append(keys, key)
	}
	var schemas []struct {
		Fingerprint string
		Document    json.RawMessage
	}
	if err := db.NewSelect().Table("memory_node_schemas").Column("fingerprint", "document").
		Where("fingerprint IN (?)", bun.In(keys)).Scan(ctx, &schemas); err != nil {
		return fmt.Errorf("read shared row schemas: %w", err)
	}
	for _, schema := range schemas {
		for _, n := range refs[schema.Fingerprint] {
			nodes[n].Schema = append(json.RawMessage(nil), schema.Document...)
		}
		delete(refs, schema.Fingerprint)
	}
	if len(refs) != 0 {
		return fmt.Errorf("shared row schema missing; refusing to return incomplete records")
	}
	return nil
}
