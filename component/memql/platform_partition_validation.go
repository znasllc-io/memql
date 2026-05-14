package memql

import (
	"fmt"
	"strings"

	"github.com/visionarys-io/memql/core/id"
)

// validatePlatformPartitionPayload runs pre-insert checks on rows
// targeting v1:platform:partition. Three things have to be true for
// the row to be safe:
//
//  1. The slug (payload.name) must satisfy the DNS-label rules in
//     core/id.ValidatePartitionName -- the slug becomes the row id,
//     a database PK column value, and a segment in event topics, so
//     anything that breaks those rules silently corrupts downstream
//     systems. The validator also rejects reserved names (_system).
//  2. The row id (the `id=` arg on insert(), surfaced here as
//     mutationId) must equal the slug. Skipping this check would let
//     a caller create rows whose id and payload.name disagree, which
//     would leave grant lookups (v1:identity:partitionAccess uses the
//     name) referencing a row id that doesn't match.
//  3. The displayName (if provided) is trimmed and length-capped so a
//     pathological multi-megabyte value can't land via this path.
//
// Auto-derives displayName from the slug when the caller omits it,
// so every workspace row has a non-empty user-facing label.
//
// Called from executor.executeInsert; not part of the public API.
func validatePlatformPartitionPayload(payload map[string]any, mutationId string) error {
	rawName, _ := payload["name"].(string)
	name, err := id.ValidatePartitionName(rawName)
	if err != nil {
		return fmt.Errorf("v1:platform:partition: %w", err)
	}
	payload["name"] = name

	if mutationId != "" {
		gotId := strings.TrimSpace(mutationId)
		if gotId != name {
			return fmt.Errorf(
				"v1:platform:partition: insert id %q must equal payload.name %q "+
					"(the slug is the row id; partitionAccess grants reference the name)",
				gotId, name)
		}
	}

	if dn, ok := payload["displayName"].(string); ok {
		dn = strings.TrimSpace(dn)
		const maxDisplayNameLen = 80
		if len(dn) > maxDisplayNameLen {
			return fmt.Errorf("v1:platform:partition: displayName too long (max %d chars)", maxDisplayNameLen)
		}
		if dn == "" {
			delete(payload, "displayName")
		} else {
			payload["displayName"] = dn
		}
	}
	if _, present := payload["displayName"]; !present {
		payload["displayName"] = defaultDisplayNameForSlug(name)
	}

	if desc, ok := payload["description"].(string); ok {
		desc = strings.TrimSpace(desc)
		const maxDescLen = 240
		if len(desc) > maxDescLen {
			return fmt.Errorf("v1:platform:partition: description too long (max %d chars)", maxDescLen)
		}
		if desc == "" {
			delete(payload, "description")
		} else {
			payload["description"] = desc
		}
	}

	return nil
}

// defaultDisplayNameForSlug turns a slug like "joses-workspace" into a
// title-cased "Joses Workspace" suitable for the displayName fallback.
// Cheap and deterministic; better than leaving the field empty so the
// UI doesn't have to do its own derivation.
func defaultDisplayNameForSlug(slug string) string {
	if slug == "" {
		return ""
	}
	parts := strings.Split(slug, "-")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}
