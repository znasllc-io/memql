package memql

import (
	"fmt"
	"sort"
	"strings"
)

func copyFieldReferences(src []FieldReference) []FieldReference {
	if len(src) == 0 {
		return nil
	}
	out := make([]FieldReference, len(src))
	copy(out, src)
	return out
}

func intersectFieldReferences(global, concept []FieldReference) []FieldReference {
	if len(global) == 0 || len(concept) == 0 {
		return nil
	}
	lookup := make(map[string]FieldReference, len(concept))
	for _, ref := range concept {
		if key := canonicalField(ref); key != "" {
			if _, exists := lookup[key]; !exists {
				lookup[key] = ref
			}
		}
	}
	result := make([]FieldReference, 0, len(global))
	seen := make(map[string]struct{})
	for _, ref := range global {
		key := canonicalField(ref)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		if matched, ok := lookup[key]; ok {
			result = append(result, matched)
			seen[key] = struct{}{}
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func projectionSignature(global []FieldReference, concept map[string][]FieldReference, meta metadataSelection) string {
	if len(global) == 0 && len(concept) == 0 && meta.IncludeAll {
		return "meta=all"
	}
	parts := make([]string, 0, len(concept)+2)
	if len(global) > 0 {
		globalFields := canonicalFieldReferenceStrings(global)
		if len(globalFields) > 0 {
			parts = append(parts, fmt.Sprintf("global=%s", strings.Join(globalFields, ",")))
		}
	}
	if len(concept) > 0 {
		keys := make([]string, 0, len(concept))
		for key := range concept {
			if len(concept[key]) == 0 {
				continue
			}
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fields := canonicalFieldReferenceStrings(concept[key])
			if len(fields) == 0 {
				continue
			}
			parts = append(parts, fmt.Sprintf("%s=%s", strings.ToLower(strings.TrimSpace(key)), strings.Join(fields, ",")))
		}
	}
	metaPart := metadataSignature(meta)
	if metaPart != "" {
		parts = append(parts, metaPart)
	}
	if len(parts) == 0 {
		return "meta=all"
	}
	return strings.Join(parts, "|")
}

func effectiveProjectionFields(concept string, global []FieldReference, conceptFields map[string][]FieldReference, hasGlobal bool) ([]FieldReference, bool) {
	conceptList := conceptFields[concept]
	switch {
	case len(global) == 0 && len(conceptList) == 0:
		if hasGlobal {
			return nil, true
		}
		return nil, false
	case len(global) == 0:
		return copyFieldReferences(conceptList), true
	case len(conceptList) == 0:
		if hasGlobal {
			return copyFieldReferences(global), true
		}
		return nil, false
	default:
		intersection := intersectFieldReferences(global, conceptList)
		if len(intersection) == 0 {
			return nil, true
		}
		return intersection, true
	}
}

func metadataReferenceInfo(ref FieldReference) (string, bool, bool) {
	if len(ref.Parts) == 0 {
		return "", false, false
	}
	first := strings.ToLower(strings.TrimSpace(ref.Parts[0]))
	switch first {
	case "meta":
		if len(ref.Parts) != 2 {
			return "", false, false
		}
		if ref.Parts[1] == "*" {
			return "", true, true
		}
		name, ok := canonicalMetadataFieldName(ref.Parts[1])
		return name, false, ok
	default:
		if len(ref.Parts) == 1 {
			name, ok := canonicalMetadataFieldName(ref.Parts[0])
			return name, false, ok
		}
		return "", false, false
	}
}

func canonicalMetadataFieldName(name string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "id":
		return "id", true
	case "concept":
		return "concept", true
	case "type":
		return "type", true
	case "createdat":
		return "createdAt", true
	case "createdby":
		return "createdBy", true
	case "schema":
		return "schema", true
	default:
		return "", false
	}
}

func metadataSignature(sel metadataSelection) string {
	if sel.IncludeAll {
		return "meta=all"
	}
	if len(sel.Fields) == 0 {
		return "meta=id"
	}
	fields := make([]string, 0, len(sel.Fields))
	for name := range sel.Fields {
		fields = append(fields, name)
	}
	sort.Strings(fields)
	return fmt.Sprintf("meta=%s", strings.Join(fields, ","))
}
