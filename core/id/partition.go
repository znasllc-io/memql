package id

import (
	"fmt"
	"strings"
	"unicode"
)

// DefaultPartition is used when no partition is explicitly specified
// for a partition-scoped concept. All tenant/user data falls through
// to this partition when the envelope doesn't override it.
const DefaultPartition = "default"

// SystemPartition is the reserved partition where global-scoped
// concepts live (those marked with @scope("global") in their .memql
// file). The name starts with an underscore so it can never collide
// with a user-chosen partition -- the partition-name regex in the
// parser disallows leading underscores. System-scoped rows are
// written here regardless of the request envelope's partition, and
// reads for those concepts always target this partition. See
// docs/core/arch.md for the scope model.
const SystemPartition = "_system"

// ParseNodeId splits a full node ID into its partition, concept, and short ID components.
//
// Full ID format: {partition}:{concept}:{shortId}
// Example: "acme:v1:cognition:participant:a9f3b7c2..."
//
// The first colon-delimited segment is the partition unless it matches a version
// prefix (e.g., "v1", "v2"). Concepts always begin with a version prefix.
//
// If no partition is present, partition is returned as empty string.
func ParseNodeId(fullId string) (partition, concept, shortId string, err error) {
	trimmed := strings.TrimSpace(fullId)
	if trimmed == "" {
		return "", "", "", fmt.Errorf("empty node ID")
	}

	parts := strings.Split(trimmed, ":")
	if len(parts) < 1 {
		return "", "", "", fmt.Errorf("malformed node ID: %q", fullId)
	}

	// Find the version segment index.
	// The version segment starts the concept portion and matches "v" + digit(s).
	versionIdx := -1
	for i, part := range parts {
		if isVersionSegment(part) {
			versionIdx = i
			break
		}
	}

	if versionIdx < 0 {
		// No version segment found. Treat entire string as a short/opaque ID.
		return "", "", trimmed, nil
	}

	// Partition is everything before the version segment.
	if versionIdx > 0 {
		partition = strings.Join(parts[:versionIdx], ":")
	}

	// Concept is the version segment + next 2 segments (version:domain:entity).
	conceptEnd := versionIdx + 3
	if conceptEnd > len(parts) {
		return "", "", "", fmt.Errorf("malformed concept in node ID: %q (need version:domain:entity)", fullId)
	}
	concept = strings.Join(parts[versionIdx:conceptEnd], ":")

	// Short ID is everything after the concept.
	if conceptEnd < len(parts) {
		shortId = strings.Join(parts[conceptEnd:], ":")
	}

	return partition, concept, shortId, nil
}

// BuildNodeId assembles a partition-qualified node ID.
//
// Returns: {partition}:{concept}:{shortId}
// Example: BuildNodeId("acme", "v1:cognition:participant", "a9f3b7c2") → "acme:v1:cognition:participant:a9f3b7c2"
//
// If partition is empty, it defaults to DefaultPartition.
// If shortId is empty, returns {partition}:{concept}.
func BuildNodeId(partition, concept, shortId string) string {
	partition = strings.TrimSpace(partition)
	if partition == "" {
		partition = DefaultPartition
	}

	concept = strings.TrimSpace(concept)
	if concept == "" {
		return partition
	}

	if shortId = strings.TrimSpace(shortId); shortId == "" {
		return partition + ":" + concept
	}

	return partition + ":" + concept + ":" + shortId
}

// HasPartition returns true if the full ID contains a partition prefix
// (i.e., the first segment is NOT a version prefix).
func HasPartition(fullId string) bool {
	trimmed := strings.TrimSpace(fullId)
	if trimmed == "" {
		return false
	}

	firstColon := strings.IndexByte(trimmed, ':')
	if firstColon < 0 {
		return false
	}

	firstSegment := trimmed[:firstColon]
	return !isVersionSegment(firstSegment)
}

// PrependPartition adds a partition prefix to an existing concept:shortId.
// If the ID already has a partition prefix, it is returned unchanged.
func PrependPartition(partition, existingId string) string {
	existingId = strings.TrimSpace(existingId)
	if existingId == "" {
		return ""
	}

	if HasPartition(existingId) {
		return existingId
	}

	partition = strings.TrimSpace(partition)
	if partition == "" {
		partition = DefaultPartition
	}

	return partition + ":" + existingId
}

// StripPartition removes the partition prefix from a full ID,
// returning (partition, remainder) where remainder is concept:shortId.
// If no partition is present, returns ("", fullId).
func StripPartition(fullId string) (string, string) {
	trimmed := strings.TrimSpace(fullId)
	if trimmed == "" {
		return "", ""
	}

	if !HasPartition(trimmed) {
		return "", trimmed
	}

	parts := strings.Split(trimmed, ":")
	for i, part := range parts {
		if isVersionSegment(part) {
			return strings.Join(parts[:i], ":"), strings.Join(parts[i:], ":")
		}
	}

	return "", trimmed
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
