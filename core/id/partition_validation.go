package id

import (
	"fmt"
	"regexp"
	"strings"
)

// MaxPartitionNameLen caps the partition slug length. 50 is generous;
// most real names fit in under 20. The same ceiling applies on the CLI
// keystroke-filter so users see the limit at input time rather than at
// save time. Kept in this package so server-side guards (the executor's
// pre-insert validator on v1:platform:partition) and any client-side
// form share one source of truth.
const MaxPartitionNameLen = 50

// partitionNameRE matches a DNS-label-style slug: starts and ends with
// lowercase alphanumeric, may contain dashes in between, no leading or
// trailing dashes, no separators. The slug becomes part of node IDs,
// PG row keys, and the {partition} segment in event topics
// (graph.node.created.{partition}.{concept}), so it must be safe in
// all of those layers. See docs/core/identifiers.md for the canonical
// format.
var partitionNameRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)

// reservedPartitionNames are slugs the system owns and will never let a
// caller create. Centralizing the list here keeps the CLI form, the
// engine pre-insert guard, and any future API validators in sync.
//
//   - "_system" -- where global-scoped concepts live; addressed by the
//     engine, never by clients.
//   - the empty string -- represents "no envelope partition"; the
//     engine substitutes DefaultPartition automatically and a row
//     literally named "" would be unaddressable.
//
// We don't reserve "default" because a deployment may legitimately
// want a partition with that name (e.g., the bootstrap automation
// seeds one). Adding to this list later requires a data migration if
// any deployment already used the name.
var reservedPartitionNames = map[string]struct{}{
	SystemPartition: {},
	"":              {},
}

// NormalizePartitionName lowercases and trims the input, matching the
// validator's expectations. Callers that want to persist the
// validated form should use ValidatePartitionName, which returns the
// normalized value on success.
func NormalizePartitionName(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// ValidatePartitionName checks a candidate partition slug against the
// shared rules: required, normalized lowercase, DNS-label-style
// (lowercase alphanumeric + inner dashes), <= MaxPartitionNameLen,
// not on the reserved list. Returns the normalized value on success
// so callers can persist the clean form. Used by:
//
//   - the executor pre-insert guard on v1:platform:partition (server
//     side; cannot be bypassed by going around a mutation);
//   - the createPartition / createWorkspace mutation paths;
//   - the memql-cockpit Add Partition form (defense in depth, plus
//     instant feedback at typing time).
//
// Returning the normalized value also means callers can write
// `name, err := ValidatePartitionName(raw); ...` and forward `name`
// to the row id without an extra normalize step.
func ValidatePartitionName(raw string) (string, error) {
	name := NormalizePartitionName(raw)
	if name == "" {
		return "", fmt.Errorf("partition name is required")
	}
	if len(name) > MaxPartitionNameLen {
		return "", fmt.Errorf("partition name is too long (max %d chars)", MaxPartitionNameLen)
	}
	if !partitionNameRE.MatchString(name) {
		return "", fmt.Errorf("partition name must be lowercase alphanumeric with inner dashes only (DNS-label rules)")
	}
	if _, reserved := reservedPartitionNames[name]; reserved {
		return "", fmt.Errorf("partition name %q is reserved", name)
	}
	return name, nil
}

// isInWordPunct reports whether the rune is a "skip me, I'm inside a
// word" character that should drop out of a slug rather than become a
// dash. Limited to apostrophe-family glyphs that show up in real
// display names ("Jose's Workspace", "François's Lab", etc.).
func isInWordPunct(r rune) bool {
	switch r {
	case '\'', '’', 'ʼ':
		return true
	}
	return false
}

// IsValidPartitionNameChar reports whether the given rune is allowed
// in the raw input stream of a partition-name field. The keystroke
// filter accepts upper- and lower-case letters and lowercases later,
// so the user doesn't have to hold shift / hunt for caps lock. Used
// by the memql-cockpit Add Partition form to silently drop disallowed
// keystrokes.
func IsValidPartitionNameChar(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z':
		return true
	case r >= 'A' && r <= 'Z':
		return true
	case r >= '0' && r <= '9':
		return true
	case r == '-':
		return true
	default:
		return false
	}
}

// SuggestPartitionSlug derives a candidate slug from a free-form
// display name -- e.g., "Jose's Workspace" -> "joses-workspace". The
// result is guaranteed to satisfy ValidatePartitionName except for
// uniqueness (the caller must check for slug collisions against
// existing v1:platform:partition rows).
//
// Behaviour:
//
//   - lowercases everything;
//   - strips diacritics by mapping to ASCII letters/digits;
//   - replaces runs of disallowed characters with a single dash;
//   - trims leading/trailing dashes;
//   - truncates to MaxPartitionNameLen with a trailing-dash trim.
//
// If the input has no usable characters (all whitespace, all
// punctuation, etc.) returns an empty string -- callers should
// surface a "please pick a name" message rather than retrying.
func SuggestPartitionSlug(displayName string) string {
	name := strings.ToLower(strings.TrimSpace(displayName))
	if name == "" {
		return ""
	}

	var b strings.Builder
	b.Grow(len(name))
	prevDash := true // suppress leading dash
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
			prevDash = false
		case isInWordPunct(r):
			// Apostrophes / smart quotes / acute accents are dropped
			// rather than turned into separators -- "Jose's Workspace"
			// becomes "joses-workspace", not "jose-s-workspace". Keeps
			// slugs readable when the display name carries possessives
			// or contractions.
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}

	out := strings.TrimRight(b.String(), "-")
	if out == "" {
		return ""
	}
	if len(out) > MaxPartitionNameLen {
		out = strings.TrimRight(out[:MaxPartitionNameLen], "-")
	}
	return out
}
