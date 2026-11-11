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
	// VersionFromOrigin: the version is the origin's own, its updated_at or
	// created_at.
	VersionFromOrigin = "origin"
	// VersionFromRead: the version is a time MemQL read the origin -- for a
	// row the origin publishes no version for, and for a tombstone, whose
	// standing is its read time (Refuses), not a version.
	VersionFromRead = "read"
)

// Stamp is what the guard knows of one side of a write: the row MemQL holds,
// or the write that would replace it.
type Stamp struct {
	// Held is false for a row MemQL does not hold, and for one it cannot
	// order anything against: a live row written before its connector said
	// where its versions come from.
	Held bool
	// Retired is a tombstone: the record gone at the origin.
	Retired bool
	// Source is where Version came from: VersionFromOrigin, VersionFromRead,
	// or "" for a row that holds no version.
	Source  string
	Version string
	// ReadAt is when the origin was read for it (ReadTime), "" when unknown.
	ReadAt string
}

// StoredStamp is how the guard reads a stored row, and the ONE place a row
// written under the connector's rule is told from one written before it.
//
// A spec that names no VersionSourceField compares every stored version, as
// before the field existed. One that names it counts the marker only when it
// was written by the same write as the row's read time (VersionReadAtField
// equal to ReadAtField): an unmarked row, or one whose marker an earlier
// build's write left behind, holds no version. Such a LIVE row orders nothing
// and is not Held; such a TOMBSTONE still records when the record was found
// gone, by its read time, so an older snapshot does not bring it back.
func StoredStamp(spec DomainSpec, payload map[string]any) Stamp {
	if payload == nil {
		return Stamp{}
	}
	out := Stamp{Held: true, Retired: payload["deleted"] == true, ReadAt: field(payload, spec.ReadAtField)}
	if strings.TrimSpace(spec.VersionField) == "" {
		return out
	}
	if strings.TrimSpace(spec.VersionSourceField) == "" {
		out.Source, out.Version = VersionFromOrigin, field(payload, spec.VersionField)
		return out
	}
	source := field(payload, spec.VersionSourceField)
	paired := source != "" && (strings.TrimSpace(spec.VersionReadAtField) == "" ||
		(out.ReadAt != "" && field(payload, spec.VersionReadAtField) == out.ReadAt))
	if !paired {
		if out.Retired {
			return out
		}
		return Stamp{}
	}
	out.Source, out.Version = source, field(payload, spec.VersionField)
	return out
}

// WriteStamp is how the guard reads a write.
func WriteStamp(spec DomainSpec, w MirrorWrite) Stamp {
	out := Stamp{Held: true, Retired: w.Retire, Version: strings.TrimSpace(w.Version), ReadAt: field(w.Payload, spec.ReadAtField)}
	if strings.TrimSpace(spec.VersionSourceField) == "" {
		out.Source = VersionFromOrigin
	} else {
		out.Source = field(w.Payload, spec.VersionSourceField)
	}
	return out
}

// Refuses is the guard: whether a write must not replace the row MemQL holds.
// The runtime's applier and a connector's own sweep both ask it.
//
// WHETHER A RECORD EXISTS is judged by when the origin was read, never by
// the record's version -- a record's updatedAt does not move when it is
// deleted, and a read that wrongly found it gone must not outrank every
// later read that finds it. When either side is a tombstone and the row's
// read time is known:
//   - a live write over a tombstone applies only when the origin was read
//     for it AFTER the tombstone's time;
//   - a tombstone over a live row applies only when its time is AFTER the
//     time the origin was last read for that row;
//   - a tombstone over a tombstone: the later time wins, and an earlier or
//     equal one writes nothing, since it changes nothing.
//
// WHICH OF TWO LIVE STATES IS NEWER is the origin's version, when both sides
// carry one. Versions of different kinds are never compared: a stored read
// time does not refuse a write carrying the origin's version (it is no
// evidence of being newer by the origin's clock). A write carrying only a
// read time is ordered by when it was read, against the row's read time,
// whatever the row's version: that is the only evidence it carries.
func Refuses(stored, incoming Stamp) bool {
	if !stored.Held {
		return false
	}
	if (stored.Retired || incoming.Retired) && stored.ReadAt != "" {
		return !readAfter(incoming.ReadAt, stored.ReadAt)
	}
	switch {
	case stored.Source == "":
		return false
	case stored.Source == VersionFromOrigin && incoming.Source == VersionFromOrigin:
		return Stale(incoming.Version, stored.Version)
	case incoming.Source == VersionFromOrigin:
		return false
	case incoming.ReadAt == "" || stored.ReadAt == "":
		// Two versions of the connector's own clock with no read time to
		// order them by: compared as the versions they are.
		return Stale(incoming.Version, stored.Version)
	default:
		return readAfter(stored.ReadAt, incoming.ReadAt)
	}
}

// readAfter reports whether read time a is strictly after b; an unknown or
// unreadable a is after nothing.
func readAfter(a, b string) bool {
	ta, aok := VersionTime(a)
	tb, bok := VersionTime(b)
	return aok && bok && ta.After(tb)
}

func field(payload map[string]any, name string) string {
	if payload == nil || strings.TrimSpace(name) == "" {
		return ""
	}
	return strings.TrimSpace(stringOf(payload[name]))
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

// readTimeLayout writes every read time to the nanosecond, with every digit:
// two reads in one second stay ordered, and no read time written this way is
// ever spelled the way an earlier build spelled one (whole seconds), which is
// what lets a marker be paired with the write that set it (StoredStamp).
const readTimeLayout = "2006-01-02T15:04:05.000000000Z07:00"

// ReadTime is how a connector writes the time the origin was read for a
// write, on this engine's clock.
func ReadTime(t time.Time) string { return t.UTC().Format(readTimeLayout) }
