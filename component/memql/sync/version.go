package sync

import (
	"fmt"
	"strings"
	"time"
)

// version.go -- the version guard's one comparison and its one reading of a
// stored row.
//
// Two writers put mirror rows down: the runtime's applier (component/datasync),
// which writes what Apply and Backfill return, and a connector's own sweep,
// which heals as it goes because ReconcileReport carries counts rather than
// writes. Both must refuse a write older than the row they would replace, by
// the same rule, or the sweep that heals drift becomes the sweep that
// reintroduces it. The rule lives here, in the contract both already import,
// so neither keeps a copy.

// The values a connector writes in DomainSpec.VersionSourceField.
const (
	// VersionFromOrigin: the version is the origin's own -- its updated_at,
	// its event time -- or, for a row retired because the origin was found
	// not to have it, the moment that was found.
	VersionFromOrigin = "origin"
	// VersionFromRead: the origin publishes no version for the row, and the
	// version is when MemQL read it.
	VersionFromRead = "read"
)

// StoredVersion is the version a stored mirror row holds, as the guard
// compares it: the payload's VersionField, or "" when the row holds none --
// the spec names no VersionField, or names a VersionSourceField the row does
// not carry, which is a row written before the connector said where its
// versions come from.
//
// This is the ONE place a row written under the old rule is told from one
// written under the new: the applier's read (datasync) and a connector's
// heal both call it.
func StoredVersion(spec DomainSpec, payload map[string]any) string {
	field := strings.TrimSpace(spec.VersionField)
	if field == "" || payload == nil {
		return ""
	}
	// A row whose connector says where its versions come from, written
	// before it said so. Its version is a clock of MemQL's -- for Shopify,
	// the time an object was read or a bulk line streamed -- and is later
	// than the origin's own for the same state, so compared against the
	// origin's it would refuse every honest write. The marker is a field the
	// connector adds to every write, not a property of the values (an
	// updatedAt equal to syncedAt): an origin version can equal the read
	// time to the second, and a tombstone's two stamps are one instant.
	if source := strings.TrimSpace(spec.VersionSourceField); source != "" && strings.TrimSpace(stringOf(payload[source])) == "" {
		return ""
	}
	return strings.TrimSpace(stringOf(payload[field]))
}

// Stale reports whether a write carrying incoming is older than a row holding
// stored. A write with no version, or a row holding none, is never stale: a
// connector that cannot say when a change happened gets last-write-wins, and
// a row with nothing to order against cannot refuse anything.
func Stale(incoming, stored string) bool {
	incoming, stored = strings.TrimSpace(incoming), strings.TrimSpace(stored)
	if incoming == "" || stored == "" {
		return false
	}
	return CompareVersions(incoming, stored) < 0
}

// CompareVersions orders two version strings.
//
// RFC3339 timestamps are compared as INSTANTS, not as strings: the two
// orders agree only while both sides use the same precision and offset,
// and an origin that sends "+00:00" where MemQL stored "Z" would compare
// wrong for every row. Anything that does not parse as a timestamp falls
// back to a lexicographic compare, which is the right answer for a
// zero-padded sequence and an honest guess for anything else.
func CompareVersions(a, b string) int {
	ta, aok := VersionTime(a)
	tb, bok := VersionTime(b)
	if aok && bok {
		switch {
		case ta.Before(tb):
			return -1
		case ta.After(tb):
			return 1
		default:
			return 0
		}
	}
	return strings.Compare(a, b)
}

// VersionTime reads a version as an instant, when it is an RFC3339 time.
func VersionTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func stringOf(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	default:
		return fmt.Sprint(x)
	}
}
