package memql

import (
	"context"
	"strings"
	"testing"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// An unconfigured engine must REFUSE, not panic.
//
// This is a regression guard for a typed nil. bunStore.db widened from
// *bun.DB to bun.IDB so the outbox append could share the store inside a
// transaction, and a nil *bun.DB assigned to an interface is NOT nil --
// so `s.db == nil` stopped catching an unconfigured engine and the guard
// fell through into a nil-pointer panic inside bun.
//
// It is asserted here rather than left to the test that caught it
// (TestRePromoteConcept_UnRetiresAndWritesResume, which is about
// something else entirely and found this by accident) because the trap
// reappears the moment anyone constructs a bunStore by hand.
func TestAnUnconfiguredStoreRefusesRatherThanPanicking(t *testing.T) {
	store := newBunStore(nil)
	if store.db != nil {
		t.Fatal("newBunStore(nil) stored a TYPED NIL in the interface field -- every nil check downstream now reads it as configured")
	}
	err := store.InsertMemoryNode(context.Background(), nil)
	if err == nil {
		t.Fatal("an unconfigured store accepted a write")
	}
	if _, err := store.QueryMemoryNodes(context.Background(), memorynodes.QueryParams{IDs: []string{"x"}, Limit: 1}); err == nil {
		t.Fatal("an unconfigured store accepted a read")
	}
}

// The version stamped on an entry is the row's own createdAt, because
// MemQL's primary key is (id, createdAt) and a second counter would be a
// second answer to "which write is newer".
func TestTheOutboxVersionIsTheRowsOwnCreatedAt(t *testing.T) {
	at := time.Date(2026, 8, 23, 12, 0, 0, 123456789, time.UTC)
	got := outboxRowVersion(at)
	if !strings.HasPrefix(got, "2026-08-23T12:00:00.123456789") {
		t.Errorf("outboxRowVersion = %q, want the row's createdAt in RFC3339 nanoseconds", got)
	}
}

// The idempotency key and the derived entry id are functions of
// (concept, row, version, target) and of nothing else -- which is what
// makes a replayed write append the same entry rather than a second one.
func TestTheEntryIdAndKeyDeriveFromTheSameFourValues(t *testing.T) {
	a := outboxEntryId("v1:x:thing", "row-1", "v1", "shopify")
	again := outboxEntryId("v1:x:thing", "row-1", "v1", "shopify")
	if a != again {
		t.Fatal("the entry id is not deterministic; a replayed write would append a second entry for one change")
	}
	for _, other := range []string{
		outboxEntryId("v1:x:other", "row-1", "v1", "shopify"),
		outboxEntryId("v1:x:thing", "row-2", "v1", "shopify"),
		outboxEntryId("v1:x:thing", "row-1", "v2", "shopify"),
		outboxEntryId("v1:x:thing", "row-1", "v1", "quickBooks"),
	} {
		if other == a {
			t.Error("two different (concept, row, version, target) tuples derived the SAME entry id -- one delivery would swallow the other")
		}
	}
	key := outboxIdempotencyKey("v1:x:thing", "row-1", "v1", "shopify")
	for _, part := range []string{"v1:x:thing", "row-1", "v1", "shopify"} {
		if !strings.Contains(key, part) {
			t.Errorf("the idempotency key %q does not carry %q", key, part)
		}
	}
}

// A retirement is recognised from the two conventions the tree uses, and
// anything else is an upsert. The default leans to upsert deliberately:
// a missed retirement is corrected by the next reconciliation, while a
// spurious one deletes live data at the origin.
func TestRetirementIsRecognisedFromTheTreesTwoConventions(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		want    bool
	}{
		{"deleted true", map[string]any{"deleted": true}, true},
		{"active false", map[string]any{"active": false}, true},
		{"active true", map[string]any{"active": true}, false},
		{"deleted false", map[string]any{"deleted": false}, false},
		{"an ordinary write", map[string]any{"label": "x"}, false},
		{"nothing at all", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := outboxPayloadRetires(tc.payload); got != tc.want {
				t.Errorf("outboxPayloadRetires(%v) = %v, want %v", tc.payload, got, tc.want)
			}
		})
	}
}
