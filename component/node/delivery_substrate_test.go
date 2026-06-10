package node

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

// fakeOutboxStore is an in-memory outboxStore for unit-testing the substrate's
// ordering / replay / dedup / cursor logic without a live Postgres. It mirrors
// the durable contract exactly: idempotent append by EventID, per-key
// monotonic seq, ascending replay reads, monotonic cursor. The real bun-backed
// path (delivery_store_pg.go) is exercised by #1264's cluster test against the
// live tables.
type fakeOutboxStore struct {
	mu       sync.Mutex
	rows     map[string][]Deliverable // routing-key -> rows in seq order
	byEvent  map[string]int64         // event-id -> seq (idempotency)
	nextSeq  map[string]int64         // routing-key -> next seq
	cursors  map[string]int64         // "key|consumer" -> cursor seq
	appendCb func(d Deliverable)      // optional hook fired after each new append
}

func newFakeOutboxStore() *fakeOutboxStore {
	return &fakeOutboxStore{
		rows:    make(map[string][]Deliverable),
		byEvent: make(map[string]int64),
		nextSeq: make(map[string]int64),
		cursors: make(map[string]int64),
	}
}

func (f *fakeOutboxStore) Append(_ context.Context, d Deliverable) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if seq, ok := f.byEvent[d.EventID]; ok {
		return seq, true, nil
	}
	k := d.Key.String()
	f.nextSeq[k]++
	seq := f.nextSeq[k]
	d.Seq = seq
	f.rows[k] = append(f.rows[k], d)
	f.byEvent[d.EventID] = seq
	if f.appendCb != nil {
		f.appendCb(d)
	}
	return seq, false, nil
}

func (f *fakeOutboxStore) ReadAfter(_ context.Context, key RoutingKey, afterSeq int64, limit int) ([]Deliverable, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Deliverable
	for _, d := range f.rows[key.String()] {
		if d.Seq > afterSeq {
			out = append(out, d)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (f *fakeOutboxStore) LoadCursor(_ context.Context, key RoutingKey, consumerID string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cursors[key.String()+"|"+consumerID], nil
}

func (f *fakeOutboxStore) SaveCursor(_ context.Context, key RoutingKey, consumerID string, seq int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	ck := key.String() + "|" + consumerID
	if seq > f.cursors[ck] {
		f.cursors[ck] = seq // monotonic, like the GREATEST() guard in SQL
	}
	return nil
}

// recordingFastPath captures the mesh hints the substrate emits and (optionally)
// loops them straight back into the substrate's HandleFastPath, simulating a
// mesh copy racing the durable pull.
type recordingFastPath struct {
	mu   sync.Mutex
	hits []Deliverable
	loop *Substrate // if set, feed each hint back via HandleFastPath
}

func (r *recordingFastPath) PublishHint(d Deliverable) {
	r.mu.Lock()
	r.hits = append(r.hits, d)
	loop := r.loop
	r.mu.Unlock()
	if loop != nil {
		loop.HandleFastPath(d)
	}
}

func (r *recordingFastPath) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.hits)
}

func testKey(id string) RoutingKey { return RoutingKey{Kind: "space", ID: id} }

func mkDeliverable(key RoutingKey, eventID, topic string) Deliverable {
	return Deliverable{
		EventID: eventID,
		Key:     key,
		Topic:   topic,
		Kind:    events.KindNodeCreated,
		Payload: map[string]any{"eid": eventID},
	}
}

// recvN reads n deliverables from ch (acking each via the substrate) or fails
// after timeout. Returns the deliverables in arrival order.
func recvN(t *testing.T, ch <-chan Deliverable, n int, timeout time.Duration) []Deliverable {
	t.Helper()
	var got []Deliverable
	deadline := time.After(timeout)
	for len(got) < n {
		select {
		case d, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed after %d/%d deliverables", len(got), n)
			}
			got = append(got, d)
		case <-deadline:
			t.Fatalf("timed out waiting for %d deliverables; got %d", n, len(got))
		}
	}
	return got
}

func TestSubstratePublishSubscribeAck(t *testing.T) {
	store := newFakeOutboxStore()
	sub := NewSubstrate(store, time.Minute, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	key := testKey("happy")
	ch, err := sub.Subscribe(ctx, key, "consumerA")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	for i := 1; i <= 3; i++ {
		seq, err := sub.Publish(ctx, mkDeliverable(key, fmt.Sprintf("e%d", i), "t"))
		if err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
		if seq != int64(i) {
			t.Fatalf("Publish %d: expected seq %d, got %d", i, i, seq)
		}
	}

	got := recvN(t, ch, 3, 2*time.Second)
	for i, d := range got {
		want := fmt.Sprintf("e%d", i+1)
		if d.EventID != want {
			t.Fatalf("deliverable %d: expected %s, got %s", i, want, d.EventID)
		}
		if d.Seq != int64(i+1) {
			t.Fatalf("deliverable %d: expected seq %d, got %d", i, i+1, d.Seq)
		}
		if err := sub.Ack(ctx, key, "consumerA", d.Seq); err != nil {
			t.Fatalf("Ack: %v", err)
		}
	}

	// Cursor advanced to 3.
	if c, _ := store.LoadCursor(ctx, key, "consumerA"); c != 3 {
		t.Fatalf("expected cursor 3, got %d", c)
	}
}

func TestSubstrateReplayFromCursor(t *testing.T) {
	store := newFakeOutboxStore()
	sub := NewSubstrate(store, time.Minute, nil, nil)
	ctx := context.Background()
	key := testKey("replay")

	// Produce 5 events while no one is subscribed (they sit in the outbox).
	for i := 1; i <= 5; i++ {
		if _, err := sub.Publish(ctx, mkDeliverable(key, fmt.Sprintf("e%d", i), "t")); err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}

	// First consumer connects, replays all 5 from the watermark (cursor 0),
	// acks through e3, then disconnects.
	ctx1, cancel1 := context.WithCancel(ctx)
	ch1, err := sub.Subscribe(ctx1, key, "consumerA")
	if err != nil {
		t.Fatalf("Subscribe 1: %v", err)
	}
	got1 := recvN(t, ch1, 5, 2*time.Second)
	if got1[0].EventID != "e1" || got1[4].EventID != "e5" {
		t.Fatalf("replay order wrong: %s..%s", got1[0].EventID, got1[4].EventID)
	}
	if err := sub.Ack(ctx1, key, "consumerA", got1[2].Seq); err != nil { // ack through e3
		t.Fatalf("Ack: %v", err)
	}
	cancel1()

	// A NEW consumer reconnects under the SAME logical consumer id (e.g. a
	// different BFF replica taking over the key). It MUST replay exactly the
	// rows after its cursor (e4, e5), exactly once -- the star-topology bug
	// this design removes.
	ctx2, cancel2 := context.WithCancel(ctx)
	defer cancel2()
	// give tail-1 a moment to fully exit so its dedup state is irrelevant
	time.Sleep(20 * time.Millisecond)

	ch2, err := sub.Subscribe(ctx2, key, "consumerA")
	if err != nil {
		t.Fatalf("Subscribe 2: %v", err)
	}
	got2 := recvN(t, ch2, 2, 2*time.Second)
	if got2[0].EventID != "e4" || got2[1].EventID != "e5" {
		t.Fatalf("expected replay of e4,e5 after cursor; got %s,%s", got2[0].EventID, got2[1].EventID)
	}

	// And no third deliverable arrives (exactly the backlog after the cursor).
	select {
	case d := <-ch2:
		t.Fatalf("unexpected extra deliverable after replay: %s", d.EventID)
	case <-time.After(300 * time.Millisecond):
	}
}

func TestSubstrateDedupAcrossPaths(t *testing.T) {
	store := newFakeOutboxStore()
	// fastPath loops every hint back into HandleFastPath, simulating a mesh
	// copy of the same EventID racing the durable pull to the same consumer.
	fp := &recordingFastPath{}
	sub := NewSubstrate(store, time.Minute, fp, nil)
	fp.loop = sub

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	key := testKey("dedup")

	ch, err := sub.Subscribe(ctx, key, "consumerA")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	// Let the subscription register before publishing so the mesh hint routes.
	time.Sleep(20 * time.Millisecond)

	// Publish: the durable row lands AND the fast-path loops a mesh copy of the
	// SAME EventID straight back to the live subscription. The consumer must
	// receive dupEvent EXACTLY ONCE -- whichever path arrives first wins, the
	// other is deduped (ADR 4.2).
	if _, err := sub.Publish(ctx, mkDeliverable(key, "dupEvent", "t")); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	got := recvN(t, ch, 1, 2*time.Second)
	if got[0].EventID != "dupEvent" {
		t.Fatalf("expected dupEvent, got %s", got[0].EventID)
	}
	// No second copy of dupEvent (the other path was deduped).
	select {
	case d := <-ch:
		t.Fatalf("double-delivery across paths: %s", d.EventID)
	case <-time.After(400 * time.Millisecond):
	}

	// A fresh event is still delivered exactly once.
	if _, err := sub.Publish(ctx, mkDeliverable(key, "fresh", "t")); err != nil {
		t.Fatalf("Publish fresh: %v", err)
	}
	got = recvN(t, ch, 1, 2*time.Second)
	if got[0].EventID != "fresh" {
		t.Fatalf("expected fresh, got %s", got[0].EventID)
	}
	select {
	case d := <-ch:
		t.Fatalf("double-delivery of %s", d.EventID)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestSubstrateIdempotentPublish proves a duplicate EventID is a no-op produce
// that returns the original seq and inserts no second row (ADR 4.2).
func TestSubstrateIdempotentPublish(t *testing.T) {
	store := newFakeOutboxStore()
	sub := NewSubstrate(store, time.Minute, nil, nil)
	ctx := context.Background()
	key := testKey("idem")

	seq1, err := sub.Publish(ctx, mkDeliverable(key, "same", "t"))
	if err != nil {
		t.Fatalf("Publish 1: %v", err)
	}
	seq2, err := sub.Publish(ctx, mkDeliverable(key, "same", "t"))
	if err != nil {
		t.Fatalf("Publish 2: %v", err)
	}
	if seq1 != seq2 {
		t.Fatalf("idempotent publish should return same seq: %d vs %d", seq1, seq2)
	}
	rows, _ := store.ReadAfter(ctx, key, 0, 0)
	if len(rows) != 1 {
		t.Fatalf("duplicate produce inserted %d rows, want 1", len(rows))
	}
}

func TestSubstratePerKeyOrdering(t *testing.T) {
	store := newFakeOutboxStore()
	sub := NewSubstrate(store, time.Minute, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	key := testKey("order")

	ch, err := sub.Subscribe(ctx, key, "consumerA")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	const n = 50
	for i := 1; i <= n; i++ {
		if _, err := sub.Publish(ctx, mkDeliverable(key, fmt.Sprintf("e%d", i), "t")); err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}

	got := recvN(t, ch, n, 3*time.Second)
	var lastSeq int64
	for i, d := range got {
		if d.Seq <= lastSeq {
			t.Fatalf("ordering violation at %d: seq %d not > %d", i, d.Seq, lastSeq)
		}
		lastSeq = d.Seq
		if d.EventID != fmt.Sprintf("e%d", i+1) {
			t.Fatalf("ordering violation at %d: got %s", i, d.EventID)
		}
	}
}

// TestSubstrateOrderingIsPerKey proves different keys are independent streams:
// a subscriber to key A sees only A's events, in A's order, regardless of
// interleaved B publishes.
func TestSubstrateOrderingIsPerKey(t *testing.T) {
	store := newFakeOutboxStore()
	sub := NewSubstrate(store, time.Minute, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	keyA := testKey("A")
	keyB := testKey("B")

	chA, err := sub.Subscribe(ctx, keyA, "cA")
	if err != nil {
		t.Fatalf("Subscribe A: %v", err)
	}
	chB, err := sub.Subscribe(ctx, keyB, "cB")
	if err != nil {
		t.Fatalf("Subscribe B: %v", err)
	}

	// Interleave A and B publishes.
	for i := 1; i <= 4; i++ {
		if _, err := sub.Publish(ctx, mkDeliverable(keyA, fmt.Sprintf("a%d", i), "t")); err != nil {
			t.Fatalf("Publish A%d: %v", i, err)
		}
		if _, err := sub.Publish(ctx, mkDeliverable(keyB, fmt.Sprintf("b%d", i), "t")); err != nil {
			t.Fatalf("Publish B%d: %v", i, err)
		}
	}

	gotA := recvN(t, chA, 4, 2*time.Second)
	for i, d := range gotA {
		if d.EventID != fmt.Sprintf("a%d", i+1) {
			t.Fatalf("key A stream contaminated at %d: %s", i, d.EventID)
		}
		if d.Seq != int64(i+1) {
			t.Fatalf("key A seq should be per-key %d, got %d", i+1, d.Seq)
		}
	}
	gotB := recvN(t, chB, 4, 2*time.Second)
	for i, d := range gotB {
		if d.EventID != fmt.Sprintf("b%d", i+1) {
			t.Fatalf("key B stream contaminated at %d: %s", i, d.EventID)
		}
		if d.Seq != int64(i+1) {
			t.Fatalf("key B seq should be per-key %d, got %d", i+1, d.Seq)
		}
	}
}

// TestSubstrateMeshDownStillDelivers proves the durable path delivers with NO
// fast-path wired at all (mesh-down is a supported mode, ADR 4.5 rule 3).
func TestSubstrateMeshDownStillDelivers(t *testing.T) {
	store := newFakeOutboxStore()
	sub := NewSubstrate(store, time.Minute, nil /* no mesh */, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	key := testKey("meshdown")

	if _, err := sub.Publish(ctx, mkDeliverable(key, "e1", "t")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	ch, err := sub.Subscribe(ctx, key, "c")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	got := recvN(t, ch, 1, 2*time.Second)
	if got[0].EventID != "e1" {
		t.Fatalf("expected e1, got %s", got[0].EventID)
	}
}

// TestSubstrateFastPathEmittedSameEventID proves Publish emits a best-effort
// mesh hint carrying the SAME EventID as the durable row (ADR 4.5 rule 1), and
// that a duplicate produce does NOT re-emit the hint.
func TestSubstrateFastPathEmittedSameEventID(t *testing.T) {
	store := newFakeOutboxStore()
	fp := &recordingFastPath{} // no loop-back; just record
	sub := NewSubstrate(store, time.Minute, fp, nil)
	ctx := context.Background()
	key := testKey("fp")

	if _, err := sub.Publish(ctx, mkDeliverable(key, "e1", "t")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if fp.count() != 1 {
		t.Fatalf("expected 1 fast-path hint, got %d", fp.count())
	}
	fp.mu.Lock()
	if fp.hits[0].EventID != "e1" || fp.hits[0].Seq != 1 {
		t.Fatalf("hint must carry same EventID+seq: %+v", fp.hits[0])
	}
	fp.mu.Unlock()

	// Duplicate produce: no second hint (idempotent no-op).
	if _, err := sub.Publish(ctx, mkDeliverable(key, "e1", "t")); err != nil {
		t.Fatalf("Publish dup: %v", err)
	}
	if fp.count() != 1 {
		t.Fatalf("duplicate produce should not re-emit hint; got %d", fp.count())
	}
}

func TestRoutingKeyString(t *testing.T) {
	if got := (RoutingKey{Kind: "space", ID: "abc"}).String(); got != "space:abc" {
		t.Fatalf("expected space:abc, got %s", got)
	}
	if (RoutingKey{Kind: "", ID: "x"}).Valid() {
		t.Fatal("empty kind should be invalid")
	}
	if (RoutingKey{Kind: "user", ID: ""}).Valid() {
		t.Fatal("empty id should be invalid")
	}
	if !(RoutingKey{Kind: "session", ID: "s1"}).Valid() {
		t.Fatal("session:s1 should be valid")
	}
}

func TestSubstrateRejectsInvalidInput(t *testing.T) {
	sub := NewSubstrate(newFakeOutboxStore(), time.Minute, nil, nil)
	ctx := context.Background()
	if _, err := sub.Publish(ctx, Deliverable{EventID: "x", Key: RoutingKey{}}); err == nil {
		t.Fatal("expected error for invalid key")
	}
	if _, err := sub.Publish(ctx, Deliverable{Key: testKey("k")}); err == nil {
		t.Fatal("expected error for missing EventID")
	}
	if _, err := sub.Subscribe(ctx, RoutingKey{}, "c"); err == nil {
		t.Fatal("expected error for invalid subscribe key")
	}
	if _, err := sub.Subscribe(ctx, testKey("k"), ""); err == nil {
		t.Fatal("expected error for missing consumerID")
	}
}
