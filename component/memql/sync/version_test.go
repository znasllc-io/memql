package sync

import (
	"testing"
	"time"
)

// THE GUARD'S ONE RULE, case by case: whether a record exists is judged by
// when the origin was read, and which of two live states is newer by the
// origin's version, never by a version of another kind.
func TestTheGuardJudgesExistenceByReadTimeAndOrderByVersion(t *testing.T) {
	read := func(clock string) string {
		tm, _ := time.Parse(time.RFC3339, "2026-08-23T"+clock+"Z")
		return ReadTime(tm)
	}
	live := func(source, version, readAt string) Stamp {
		return Stamp{Held: true, Source: source, Version: version, ReadAt: readAt}
	}
	gone := func(readAt string) Stamp {
		return Stamp{Held: true, Retired: true, Source: VersionFromRead, Version: "2026-08-23T23:59:59Z", ReadAt: readAt}
	}
	cases := []struct {
		name          string
		stored, write Stamp
		refuses       bool
	}{
		{"live over a tombstone, read after it", gone(read("10:00:00")), live(VersionFromOrigin, "2026-08-23T09:00:00Z", read("10:00:01")), false},
		{"live over a tombstone, read before it, its version later", gone(read("10:00:00")), live(VersionFromOrigin, "2026-08-24T00:00:00Z", read("09:59:59")), true},
		{"a tombstone over live, after its last read", live(VersionFromOrigin, "2026-08-24T00:00:00Z", read("10:00:00")), gone(read("10:00:01")), false},
		{"a tombstone over live, before its last read", live(VersionFromOrigin, "2026-08-23T09:00:00Z", read("10:00:00")), gone(read("09:59:59")), true},
		{"a later tombstone over a tombstone", gone(read("10:00:00")), gone(read("10:00:01")), false},
		{"an earlier or equal tombstone over a tombstone", gone(read("10:00:00")), gone(read("10:00:00")), true},
		{"live over live, older origin version, read later", live(VersionFromOrigin, "2026-08-23T10:00:00Z", read("10:00:00")), live(VersionFromOrigin, "2026-08-23T09:00:00Z", read("11:00:00")), true},
		{"a stored read time never refuses an origin version", live(VersionFromRead, "2026-08-23T12:00:00Z", read("12:00:00")), live(VersionFromOrigin, "2026-08-20T00:00:00Z", read("11:00:00")), false},
		{"a read-time write read before the row's read", live(VersionFromOrigin, "2026-08-20T00:00:00Z", read("12:00:00")), live(VersionFromRead, "2026-08-23T11:00:00Z", read("11:00:00")), true},
		{"a read-time write read after the row's read", live(VersionFromOrigin, "2026-08-24T00:00:00Z", read("12:00:00")), live(VersionFromRead, "2026-08-23T13:00:00Z", read("13:00:00")), false},
		{"a row that holds no version", Stamp{}, live(VersionFromOrigin, "2026-08-20T00:00:00Z", read("09:00:00")), false},
	}
	for _, tc := range cases {
		if got := Refuses(tc.stored, tc.write); got != tc.refuses {
			t.Errorf("%s: refuses = %t, want %t", tc.name, got, tc.refuses)
		}
	}
}

// A marker counts only beside the read time of the write that set it.
func TestAMarkerCountsOnlyBesideItsOwnReadTime(t *testing.T) {
	spec := DomainSpec{VersionField: "updatedAt", VersionSourceField: "versionSource", ReadAtField: "syncedAt", VersionReadAtField: "versionReadAt"}
	at := ReadTime(time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC))
	paired := map[string]any{"updatedAt": "2026-08-23T09:00:00Z", "versionSource": "origin", "syncedAt": at, "versionReadAt": at}
	if st := StoredStamp(spec, paired); !st.Held || st.Source != VersionFromOrigin || st.Version != "2026-08-23T09:00:00Z" {
		t.Errorf("a paired marker reads %+v, want the origin's version", st)
	}
	rewritten := map[string]any{"updatedAt": "2026-08-23T12:00:00Z", "versionSource": "origin", "syncedAt": "2026-08-23T12:00:00Z", "versionReadAt": at}
	if st := StoredStamp(spec, rewritten); st.Held {
		t.Errorf("a marker beside another write's read time reads %+v, want no version held", st)
	}
	retired := map[string]any{"deleted": true, "syncedAt": "2026-08-23T12:00:00Z"}
	if st := StoredStamp(spec, retired); !st.Held || !st.Retired || st.ReadAt != "2026-08-23T12:00:00Z" {
		t.Errorf("an unmarked tombstone reads %+v, want its read time kept", st)
	}
}
