package worker

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/znasllc-io/memql/component/events"
)

// groupSource is a membership source tests can change and count.
type groupSource struct {
	mu     sync.Mutex
	reads  int
	groups map[string][]string // bare user id -> groups
}

func newGroupSource(groups map[string][]string) *groupSource {
	return &groupSource{groups: groups}
}

func (s *groupSource) resolve(_ context.Context, userId string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	return append([]string(nil), s.groups[bareSubjectId(userId)]...)
}

func (s *groupSource) set(userId string, groups []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.groups[bareSubjectId(userId)] = groups
}

func (s *groupSource) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// liveCache is a cache subscribed to its own bus, the way app/ wires the
// installed one on an agent node.
func liveCache(t *testing.T, source GroupResolver, clock *testClock) (*MembershipCache, *events.Bus) {
	t.Helper()
	bus := events.NewBus()
	t.Cleanup(bus.Close)
	cache := NewMembershipCache(source, MembershipCacheTTL, clock.Now)
	sub := NewMembershipCacheSubscriber(nil, bus, cache)
	sub.Start(context.Background())
	t.Cleanup(func() { sub.Stop(context.Background()) })
	return cache, bus
}

func newClock() *testClock {
	return &testClock{now: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}
}

func TestTheMembershipCacheReusesOneReadPerPerson(t *testing.T) {
	source := newGroupSource(map[string][]string{"ana": {"v1:identity:group:design"}, "bo": {"v1:identity:group:ops"}})
	cache, _ := liveCache(t, source.resolve, newClock())

	for _, spelling := range []string{"ana", "v1:identity:user:ana", " ana "} {
		if got := cache.Groups(context.Background(), spelling); !slices.Equal(got, []string{"v1:identity:group:design"}) {
			t.Fatalf("Groups(%q) = %v", spelling, got)
		}
	}
	if n := source.readCount(); n != 1 {
		t.Fatalf("source reads = %d, want one for one person in any spelling of their id", n)
	}
	cache.Groups(context.Background(), "bo")
	if n := source.readCount(); n != 2 {
		t.Fatalf("source reads = %d, want a second person to cost one read of their own", n)
	}
}

// EVERY MEMBERSHIP CHANGE DROPS EVERY ENTRY, and this is the property the
// cache exists under: a person removed from a group stops being admitted by a
// machine lent to that group on the next call (design G6, memql#5660).
func TestEveryMembershipChangeDropsTheCache(t *testing.T) {
	for _, topic := range MembershipInvalidationTopics() {
		t.Run(topic, func(t *testing.T) {
			source := newGroupSource(map[string][]string{"ana": {"v1:identity:group:design"}})
			cache, bus := liveCache(t, source.resolve, newClock())
			if got := cache.Groups(context.Background(), "ana"); len(got) != 1 {
				t.Fatalf("Groups = %v", got)
			}
			cache.Groups(context.Background(), "ana")
			if n := source.readCount(); n != 1 {
				t.Fatalf("source reads = %d before the change, want 1", n)
			}

			// ana is removed from design -- on this replica or another; the
			// mesh hands this one the same event either way.
			source.set("ana", nil)
			bus.PublishSync(events.NewEvent(topic, events.KindNodeUpdated, map[string]any{"id": "membership-1", "status": "removed"}))

			if got := cache.Groups(context.Background(), "ana"); len(got) != 0 {
				t.Fatalf("after %s ana is still in %v: a removed person kept the machine lent to their group", topic, got)
			}
			if n := source.readCount(); n != 2 {
				t.Fatalf("source reads = %d, want the change to force a fresh read", n)
			}
		})
	}
}

func TestAChangeDeliveredAsynchronouslyStillDropsTheCache(t *testing.T) {
	// The mesh republishes a remote change with Publish, which hands each
	// subscriber its own goroutine. The drop happens, just not in the
	// publisher's call.
	source := newGroupSource(map[string][]string{"ana": {"v1:identity:group:design"}})
	cache, bus := liveCache(t, source.resolve, newClock())
	cache.Groups(context.Background(), "ana")
	source.set("ana", nil)
	bus.Publish(events.NewEvent(MembershipInvalidationTopics()[0], events.KindNodeCreated, map[string]any{"id": "membership-1"}))
	deadline := time.Now().Add(3 * time.Second)
	for len(cache.Groups(context.Background(), "ana")) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("an asynchronously delivered membership change never dropped the cache")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestAnUnrelatedEventLeavesTheCacheAlone(t *testing.T) {
	// The reachable negative for the test above: a subscription widened to
	// every identity event would pass it and empty the cache on every token
	// refresh.
	source := newGroupSource(map[string][]string{"ana": {"v1:identity:group:design"}})
	cache, bus := liveCache(t, source.resolve, newClock())
	cache.Groups(context.Background(), "ana")
	bus.PublishSync(events.NewEvent("graph.node.updated.v1:identity:user", events.KindNodeUpdated, map[string]any{"id": "ana"}))
	cache.Groups(context.Background(), "ana")
	if n := source.readCount(); n != 1 {
		t.Fatalf("source reads = %d: an unrelated event dropped the cache", n)
	}
}

func TestAReadInFlightAcrossAChangeIsNotKept(t *testing.T) {
	// The read starts, the change lands while it is still reading, and the
	// read then returns what it saw BEFORE the change. Kept, that answer would
	// outlive the invalidation that was meant to remove it -- for a whole TTL.
	clock := newClock()
	reading := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	source := func(context.Context, string) []string {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		if first {
			close(reading)
			<-release
			return []string{"v1:identity:group:design"} // the answer from before the change
		}
		return nil // ana is no longer in design
	}
	cache, bus := liveCache(t, source, clock)

	done := make(chan []string)
	go func() { done <- cache.Groups(context.Background(), "ana") }()
	<-reading
	bus.PublishSync(events.NewEvent(MembershipInvalidationTopics()[1], events.KindNodeUpdated, map[string]any{"id": "membership-1"}))
	close(release)
	<-done // the caller that started before the change gets what it read

	if got := cache.Groups(context.Background(), "ana"); len(got) != 0 {
		t.Fatalf("Groups = %v: a read that straddled a membership change was kept", got)
	}
}

func TestAMembershipAnswerExpiresWhenNoChangeArrives(t *testing.T) {
	// THE BACKSTOP. A replica cut off from the mesh when a change was written
	// never hears it; MembershipCacheTTL is the most an answer is reused.
	clock := newClock()
	source := newGroupSource(map[string][]string{"ana": {"v1:identity:group:design"}})
	cache, _ := liveCache(t, source.resolve, clock)
	cache.Groups(context.Background(), "ana")
	clock.advance(MembershipCacheTTL - time.Second)
	cache.Groups(context.Background(), "ana")
	if n := source.readCount(); n != 1 {
		t.Fatalf("source reads = %d inside the TTL, want 1", n)
	}
	clock.advance(time.Second)
	cache.Groups(context.Background(), "ana")
	if n := source.readCount(); n != 2 {
		t.Fatalf("source reads = %d at the TTL, want the answer re-read", n)
	}
}

func TestACacheNobodyInvalidatesNeverAnswersFromMemory(t *testing.T) {
	// Not subscribed -- a node whose wiring left the subscriber out, or one
	// not started yet -- the cache reads the source every time. Slower, never
	// stale.
	source := newGroupSource(map[string][]string{"ana": {"v1:identity:group:design"}})
	cache := NewMembershipCache(source.resolve, MembershipCacheTTL, newClock().Now)
	for range 3 {
		cache.Groups(context.Background(), "ana")
	}
	if n := source.readCount(); n != 3 {
		t.Fatalf("source reads = %d, want every call read through an unsubscribed cache", n)
	}

	// And once its subscriber STOPS, it stops answering from memory and
	// forgets what it held: nothing is listening for changes any more.
	bus := events.NewBus()
	t.Cleanup(bus.Close)
	sub := NewMembershipCacheSubscriber(nil, bus, cache)
	sub.Start(context.Background())
	cache.Groups(context.Background(), "ana")
	cache.Groups(context.Background(), "ana")
	if n := source.readCount(); n != 4 {
		t.Fatalf("source reads = %d once subscribed, want one more", n)
	}
	sub.Stop(context.Background())
	cache.Groups(context.Background(), "ana")
	if n := source.readCount(); n != 5 {
		t.Fatalf("source reads = %d after Stop, want the cache to read through again", n)
	}
}

func TestAnEmptyMembershipAnswerIsNotKept(t *testing.T) {
	// An empty answer is a person in no group or a read that failed, and the
	// two look the same. Kept, the second would refuse a member for a whole
	// TTL instead of for one call.
	source := newGroupSource(map[string][]string{})
	cache, _ := liveCache(t, source.resolve, newClock())
	cache.Groups(context.Background(), "ana")
	source.set("ana", []string{"v1:identity:group:design"}) // the read recovers
	if got := cache.Groups(context.Background(), "ana"); len(got) != 1 {
		t.Fatalf("Groups = %v: an empty answer was kept", got)
	}
}

func TestACallerCannotChangeWhatTheCacheHolds(t *testing.T) {
	source := newGroupSource(map[string][]string{"ana": {"v1:identity:group:design"}})
	cache, _ := liveCache(t, source.resolve, newClock())
	cache.Groups(context.Background(), "ana") // the read
	hit := cache.Groups(context.Background(), "ana")
	hit[0] = "v1:identity:group:everything"
	if got := cache.Groups(context.Background(), "ana"); !slices.Equal(got, []string{"v1:identity:group:design"}) {
		t.Fatalf("Groups = %v: a caller's slice aliased the cached answer", got)
	}
}

func TestTheInvalidationTopicsAreTheOnesTheEnginePublishes(t *testing.T) {
	// A subscription to a topic nothing publishes would pass every test above
	// that publishes it by hand. These are spelled through the same builder
	// the engine's write path uses, for both concepts and both verbs.
	want := []string{
		"graph.node.created.v1:identity:groupMembership",
		"graph.node.updated.v1:identity:groupMembership",
		"graph.node.created.v1:identity:group",
		"graph.node.updated.v1:identity:group",
	}
	if got := MembershipInvalidationTopics(); !slices.Equal(got, want) {
		t.Fatalf("MembershipInvalidationTopics() = %v, want %v", got, want)
	}
	if events.TopicNodeCreated("v1:identity:groupMembership") != want[0] || events.TopicNodeUpdated("v1:identity:group") != want[3] {
		t.Fatal("the topic builders disagree with the literal topics")
	}
}
