package memoryNodes

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const conceptsRoot = "."

var (
	// Concept directory segments are camelCase identifiers: lowercase
	// start (so `v1`, `v2` ... still resolve as version dirs via the
	// separate version pattern below), then any mix of letters and
	// digits. No separators (no `_`, no `-`, no `.`). Allows
	// `audioOverride` and `accessRequest`; rejects `audio-override`
	// and `Audio_Override`.
	conceptDirSegmentPattern = regexp.MustCompile(`^[a-z][a-zA-Z0-9]*$`)
	conceptVersionPattern    = regexp.MustCompile(`^v[0-9]+$`)
)

func ensureReservedFieldsNotDeclared(conceptName string, schemaBytes []byte) error {
	var definition struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []any                      `json:"required"`
	}
	if err := json.Unmarshal(schemaBytes, &definition); err != nil {
		return fmt.Errorf("parse definition schema for %s: %w", conceptName, err)
	}

	for name := range definition.Properties {
		if IsReservedPayloadField(name) {
			return fmt.Errorf("concept %s definition schema declares reserved property %q", conceptName, name)
		}
	}

	for _, entry := range definition.Required {
		if field, ok := entry.(string); ok && IsReservedPayloadField(field) {
			return fmt.Errorf("concept %s definition schema marks reserved property %q as required", conceptName, field)
		}
	}

	return nil
}

func deriveConceptName(dir string) (string, error) {
	cleanDir := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(dir), "./"), "/")
	if cleanDir == "" {
		return "", fmt.Errorf("concept schema directory path %q is invalid", dir)
	}

	trimmed := cleanDir
	if conceptsRoot != "." {
		if !strings.HasPrefix(cleanDir, conceptsRoot) {
			return "", fmt.Errorf("concept path %q must reside under %s/", dir, conceptsRoot)
		}
		trimmed = strings.TrimPrefix(cleanDir, conceptsRoot)
		trimmed = strings.TrimPrefix(trimmed, "/")
		if trimmed == "" {
			return "", fmt.Errorf("concept path %q must include a subdirectory under %s", dir, conceptsRoot)
		}
	}

	segments := strings.Split(trimmed, "/")
	if len(segments) < 3 {
		return "", fmt.Errorf("concept path %q must include version, environment, and concept directories", dir)
	}

	validated := make([]string, 0, len(segments))
	for idx, segment := range segments {
		if err := validateConceptDirSegment(segment); err != nil {
			return "", fmt.Errorf("concept path %q segment %q: %w", dir, segment, err)
		}
		if idx == 0 && !conceptVersionPattern.MatchString(segment) {
			return "", fmt.Errorf("concept path %q version directory %q must match v<integer>", dir, segment)
		}
		validated = append(validated, segment)
	}

	return strings.Join(validated, ":"), nil
}

func validateConceptDirSegment(segment string) error {
	trimmed := strings.TrimSpace(segment)
	if trimmed == "" {
		return fmt.Errorf("segment is empty")
	}
	if !conceptDirSegmentPattern.MatchString(trimmed) {
		return fmt.Errorf("must be a single lowercase alphanumeric word (no separators)")
	}
	return nil
}

// ConceptNameFromPath derives the canonical concept identifier from a directory path under concepts/.
func ConceptNameFromPath(dir string) (string, error) {
	return deriveConceptName(dir)
}
