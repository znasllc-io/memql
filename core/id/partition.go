package id

import (
	"fmt"
	"strings"
	"unicode"
)

// ParseNodeId splits a full node ID into its concept and short ID
// components.
//
// Canonical ID format: {concept}:{shortId}
// Example: "v1:cognition:participant:a9f3b7c2..."
//
// Concepts always begin with a version prefix ("v1", "v2", ...). If
// no version prefix is found the entire string is treated as a bare
// short id.
func ParseNodeId(fullId string) (concept, shortId string, err error) {
	trimmed := strings.TrimSpace(fullId)
	if trimmed == "" {
		return "", "", fmt.Errorf("empty node ID")
	}

	parts := strings.Split(trimmed, ":")

	versionIdx := -1
	for i, part := range parts {
		if isVersionSegment(part) {
			versionIdx = i
			break
		}
	}

	if versionIdx < 0 {
		// No version segment found. Treat entire string as a short/opaque ID.
		return "", trimmed, nil
	}

	// Concept is the version segment + next 2 segments (version:domain:entity).
	conceptEnd := versionIdx + 3
	if conceptEnd > len(parts) {
		return "", "", fmt.Errorf("malformed concept in node ID: %q (need version:domain:entity)", fullId)
	}
	concept = strings.Join(parts[versionIdx:conceptEnd], ":")

	if conceptEnd < len(parts) {
		shortId = strings.Join(parts[conceptEnd:], ":")
	}

	return concept, shortId, nil
}

// BuildNodeId assembles a node ID from its concept and short id.
//
// Returns: {concept}:{shortId}
// Example: BuildNodeId("v1:cognition:participant", "a9f3b7c2") →
//          "v1:cognition:participant:a9f3b7c2"
//
// If shortId is empty, returns concept unchanged.
func BuildNodeId(concept, shortId string) string {
	concept = strings.TrimSpace(concept)
	shortId = strings.TrimSpace(shortId)
	if concept == "" {
		return shortId
	}
	if shortId == "" {
		return concept
	}
	return concept + ":" + shortId
}

// isVersionSegment returns true if the segment matches "v" followed by one or more digits.
func isVersionSegment(s string) bool {
	if len(s) < 2 {
		return false
	}
	if s[0] != 'v' {
		return false
	}
	for _, r := range s[1:] {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}
