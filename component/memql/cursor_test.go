package memql

import (
	"context"
	"errors"
	"testing"
	"time"

	memorynodes "github.com/znasllc-io/memql/component/database/memory-nodes"
)

// cursor_test.go covers the keyset-cursor codec + sort-signature guard +
// keyset-predicate composition (epic 5, issue 5.12 / memql#1985) without a DB.
// The DB-backed first-page / follow-cursor / concurrent-insert proofs live in
// keyset_pagination_db_test.go; the cross-node proof in test/clustere2e.

func TestCursor_RoundTripPreservesKeysetPosition(t *testing.T) {
	at := time.Date(2026, 6, 22, 10, 30, 0, 1234, time.UTC)
	row := memorynodes.MemoryNode{ID: "v1:cognition:utterance:abc", CreatedAt: at}
	const sig = "createdAt:desc"

	token, err := encodeCursor(row, sig)
	if err != nil {
		t.Fatalf("encodeCursor: %v", err)
	}
	if token == "" {
		t.Fatal("encodeCursor returned an empty token")
	}

	pos, err := decodeCursor(token, sig)
	if err != nil {
		t.Fatalf("decodeCursor: %v", err)
	}
	if !pos.createdAt.Equal(at) {
		t.Errorf("createdAt round-trip = %v, want %v", pos.createdAt, at)
	}
	if pos.id != row.ID {
		t.Errorf("id round-trip = %q, want %q", pos.id, row.ID)
	}
}

func TestCursor_SortSignatureMismatchRejected(t *testing.T) {
	row := memorynodes.MemoryNode{ID: "v1:cognition:utterance:abc", CreatedAt: time.Now().UTC()}
	token, err := encodeCursor(row, "createdAt:desc")
	if err != nil {
		t.Fatalf("encodeCursor: %v", err)
	}
	// Replaying the same cursor against a different ordering must be a typed
	// rejection, not a silently-wrong page.
	if _, err := decodeCursor(token, "payload.title:asc"); !errors.Is(err, ErrCursorSortMismatch) {
		t.Fatalf("decodeCursor under different sort = %v, want ErrCursorSortMismatch", err)
	}
	// The matching signature still decodes.
	if _, err := decodeCursor(token, "createdAt:desc"); err != nil {
		t.Fatalf("decodeCursor under matching sort = %v, want nil", err)
	}
}

func TestCursor_MalformedRejected(t *testing.T) {
	for _, tok := range []string{"", "not-base64url-!!!", "YWJj" /* "abc", not JSON */} {
		if _, err := decodeCursor(tok, "createdAt:desc"); !errors.Is(err, ErrCursorMalformed) {
			t.Errorf("decodeCursor(%q) = %v, want ErrCursorMalformed", tok, err)
		}
	}
}

func TestCursor_ContextPlumbing(t *testing.T) {
	ctx := context.Background()
	if got := cursorFromContext(ctx); got != "" {
		t.Fatalf("empty context cursor = %q, want \"\"", got)
	}
	// Empty cursor is a no-op (offset / first-page path stays unchanged).
	if c := ContextWithCursor(ctx, "  "); cursorFromContext(c) != "" {
		t.Fatal("ContextWithCursor with blank must be a no-op")
	}
	c := ContextWithCursor(ctx, "tok-123")
	if got := cursorFromContext(c); got != "tok-123" {
		t.Fatalf("cursorFromContext = %q, want tok-123", got)
	}
}

func TestKeysetWhere_DirectionMatchesSort(t *testing.T) {
	at := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)
	pos := keysetPosition{createdAt: at, id: "v1:x:1"}

	// Descending createdAt: continuation is strictly-older, ties broken by id ASC.
	descSQL, descArgs := keysetWhere(pos, true)
	if descSQL != `("createdAt" < ? OR ("createdAt" = ? AND id > ?))` {
		t.Errorf("desc keyset SQL = %q", descSQL)
	}
	if len(descArgs) != 3 || descArgs[2] != "v1:x:1" {
		t.Errorf("desc keyset args = %v", descArgs)
	}

	ascSQL, _ := keysetWhere(pos, false)
	if ascSQL != `("createdAt" > ? OR ("createdAt" = ? AND id > ?))` {
		t.Errorf("asc keyset SQL = %q", ascSQL)
	}
}

func TestKeysetEligibleSort(t *testing.T) {
	// Default (nil) sort => createdAt DESC, id ASC keyset.
	if ok, desc := keysetEligibleSort(nil); !ok || !desc {
		t.Errorf("nil sorter: eligible=%v desc=%v, want true,true", ok, desc)
	}

	descSorter, _ := compileSortFields([]SortField{{Field: "createdAt", Direction: SortDirectionDesc}})
	if ok, desc := keysetEligibleSort(descSorter); !ok || !desc {
		t.Errorf("createdAt desc: eligible=%v desc=%v, want true,true", ok, desc)
	}

	ascSorter, _ := compileSortFields([]SortField{{Field: "createdAt", Direction: SortDirectionAsc}})
	if ok, desc := keysetEligibleSort(ascSorter); !ok || desc {
		t.Errorf("createdAt asc: eligible=%v desc=%v, want true,false", ok, desc)
	}

	// A leading payload sort forces a full in-memory scan -> not SQL-keyset
	// eligible (the cursor falls back to the in-memory comparator path).
	payloadSorter, _ := compileSortFields([]SortField{{Field: "payload.title", Direction: SortDirectionAsc}})
	if ok, _ := keysetEligibleSort(payloadSorter); ok {
		t.Error("payload-sorted query must not be SQL-keyset eligible")
	}
}
