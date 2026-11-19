package calls

import (
	"sort"
	"time"
)

// QueueClass ranks who drains first when seats free up.
type QueueClass int

const (
	// ClassRunningCall: a call already in progress that needs a seat for
	// someone joining it. Finishing a started conversation beats starting one.
	ClassRunningCall QueueClass = 0
	// ClassScheduledDue: a scheduled call whose time has come.
	ClassScheduledDue QueueClass = 1
	// ClassNewCall: an ad hoc call that nobody is waiting inside.
	ClassNewCall QueueClass = 2
)

// QueueEntry is one waiter. LastPollAt is the client's last poll; a zero
// value means it has not polled yet.
type QueueEntry struct {
	ID                     string
	Class                  QueueClass
	EnqueuedAt, LastPollAt time.Time
}

// OrderQueue returns the drain order: class, then arrival, then ID so equal
// arrivals never flip between calls. The input is not modified.
func OrderQueue(entries []QueueEntry) []QueueEntry {
	out := append([]QueueEntry(nil), entries...)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Class != b.Class {
			return a.Class < b.Class
		}
		if !a.EnqueuedAt.Equal(b.EnqueuedAt) {
			return a.EnqueuedAt.Before(b.EnqueuedAt)
		}
		return a.ID < b.ID
	})
	return out
}

// Position is the zero-based place of id in an ordered queue.
func Position(ordered []QueueEntry, id string) (int, bool) {
	for i, e := range ordered {
		if e.ID == id {
			return i, true
		}
	}
	return 0, false
}

// ExpiredEntries names the entries whose client has gone quiet for ttl. The
// clock is the LAST POLL, not the enqueue time: a person who has waited an
// hour but is still polling is waiting, and one who closed the tab is not. An
// entry that never polled is measured from its enqueue time.
func ExpiredEntries(entries []QueueEntry, now time.Time, ttl time.Duration) []string {
	var ids []string
	for _, e := range entries {
		seen := e.LastPollAt
		if seen.IsZero() {
			seen = e.EnqueuedAt
		}
		if !now.Before(seen.Add(ttl)) {
			ids = append(ids, e.ID)
		}
	}
	return ids
}
