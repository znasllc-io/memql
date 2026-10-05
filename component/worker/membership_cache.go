package worker

// A PERSON'S GROUPS, REUSED BETWEEN CALLS (memql#5660).
//
// ===========================================================================
// WHY THERE IS A CACHE AT ALL
// ===========================================================================
// A machine lent to a GROUP is asked about a person's groups on every model
// call that might land on it, and the replica-hop receiver asked again for
// every call it re-checked: verifySharedRegistration built a fresh Person per
// call, so nothing it read survived to the next. A membership read is two full
// reads of the membership and group concepts (component/memql.
// resolveMemberships), so a person's agent loop on a colleague's lent machine
// paid them once per turn, for an answer that changes when somebody edits a
// group -- a human act, at human volume.
//
// ===========================================================================
// WHAT MAKES IT SAFE TO KEEP AN AUTHORIZATION ANSWER
// ===========================================================================
// The answer decides who may run a prompt on somebody else's hardware, so the
// rule is the one design G6 stated for having no cache: removing a person from
// a group, or archiving the group, must end their access on the next call, on
// every replica. Four things keep that true:
//
//   - EVERY CHANGE DROPS EVERYTHING. A write to v1:identity:groupMembership or
//     v1:identity:group, on ANY replica, reaches this one as a broadcast graph
//     event (component/node/routing.go forwards created and updated for both),
//     and the subscriber drops every entry. Not the one person's: an archived
//     group changes the answer for every member and its row names none of
//     them, and per-entry invalidation is a mapping that can be wrong where
//     dropping everything cannot. At human volume it costs one read per active
//     person per change.
//   - A READ IN FLIGHT ACROSS A CHANGE IS NOT KEPT. Each read notes the
//     generation it started under, and an invalidation bumps it, so an answer
//     read before a change landed is returned to its one caller and never
//     stored for the next.
//   - A CACHE NOBODY INVALIDATES DOES NOT ANSWER. Until its subscriber is
//     subscribed -- and after it stops -- every call reads the source. A node
//     whose wiring left the subscription out is slower, never stale.
//   - A CHANGE THAT NEVER ARRIVES EXPIRES. A replica whose every mesh stream
//     was down when the change was written hears nothing (memql#5338), so no
//     entry outlives MembershipCacheTTL. That is the bound on staleness, and
//     the only one.
//
// An EMPTY answer is not kept. On the receiver it is either a person in no
// group -- whose call a group share refuses anyway, and which the sender's
// plan has normally refused already -- or a membership read that failed, which
// answers nothing rather than an error (component/memql's activeGroupIdsForUser).
// Keeping the second would refuse a member for a whole TTL rather than for one
// call, and the two cannot be told apart from here.

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/core/common"
)

// MembershipCacheTTL is the longest a person's groups are reused when no
// change to a membership or a group reaches this replica.
//
// It is the BACKSTOP, not the mechanism: a change written anywhere drops every
// entry here within the time its event takes to cross the mesh. The TTL is for
// the change that never arrives -- a replica cut off from the mesh when it was
// written, or a version that committed after the read that cached its
// predecessor -- and it is the most a removed person keeps a lent machine
// through this cache.
//
// Thirty seconds, measured against what design D6 already accepts: a call
// admitted before a removal runs to completion, for up to
// ModelCallTimeoutDefault (ten minutes). A bound twenty times shorter than the
// window the design already grants a call in flight gives a removed person
// nothing they could use that they do not already have. It is also the fleet's
// own online window, a figure this subsystem already lives by.
const MembershipCacheTTL = 30 * time.Second

// membershipCacheMaxEntries bounds the map between invalidations. One entry is
// one person who made a call through this replica, so the bound is generous;
// reaching it drops the expired entries, and then everything.
const membershipCacheMaxEntries = 4096

// membershipConcepts are the two concepts whose writes change whom a group
// share admits: who is in a group, and whether the group is active.
var membershipConcepts = []string{"v1:identity:groupMembership", "v1:identity:group"}

// MembershipInvalidationTopics are the events that drop the cache: created and
// updated for both concepts, in the topic form the engine publishes
// (component/memql/executor_mutation.go). Every write publishes created --
// rows are append-only -- and an update publishes updated as well. Nothing
// publishes deleted for any concept (component/node/routing_reach_test.go).
//
// Every one of them must cross replicas, or a change written on one replica
// leaves the cache on another stale until the TTL; the agent build's tests
// hold this list against component/node's routing table.
func MembershipInvalidationTopics() []string {
	out := make([]string, 0, 2*len(membershipConcepts))
	for _, concept := range membershipConcepts {
		out = append(out,
			events.BuildTopicWithConcept(events.TopicGraphNodeCreated, concept),
			events.BuildTopicWithConcept(events.TopicGraphNodeUpdated, concept))
	}
	return out
}

// MembershipCache answers a person's active groups, reusing one read per
// person until a membership or a group changes. It is a GroupResolver through
// its Groups method.
type MembershipCache struct {
	source GroupResolver
	ttl    time.Duration
	now    func() time.Time

	mu      sync.Mutex
	live    bool
	gen     uint64
	entries map[string]cachedGroups
}

type cachedGroups struct {
	groups []string
	read   time.Time
}

// NewMembershipCache builds a cache over a source. A nil source is
// InstalledGroups, a non-positive ttl is MembershipCacheTTL, and a nil clock is
// time.Now. It does not answer from memory until a MembershipCacheSubscriber
// has subscribed it to membership changes.
func NewMembershipCache(source GroupResolver, ttl time.Duration, now func() time.Time) *MembershipCache {
	if source == nil {
		source = InstalledGroups
	}
	if ttl <= 0 {
		ttl = MembershipCacheTTL
	}
	if now == nil {
		now = time.Now
	}
	return &MembershipCache{source: source, ttl: ttl, now: now, entries: map[string]cachedGroups{}}
}

// Groups returns the person's active groups: from memory when this cache holds
// a live answer for them, from the source otherwise.
func (c *MembershipCache) Groups(ctx context.Context, userId string) []string {
	if c == nil {
		return InstalledGroups(ctx, userId)
	}
	userId = strings.TrimSpace(userId)
	if userId == "" {
		return nil
	}
	// One entry per person, whichever spelling of their id asked: the source
	// answers both spellings alike.
	key := bareSubjectId(userId)

	c.mu.Lock()
	if !c.live {
		c.mu.Unlock()
		return c.source(ctx, userId)
	}
	if hit, ok := c.entries[key]; ok {
		if c.now().Sub(hit.read) < c.ttl {
			out := append([]string(nil), hit.groups...)
			c.mu.Unlock()
			return out
		}
		delete(c.entries, key)
	}
	gen := c.gen
	started := c.now()
	c.mu.Unlock()

	groups := c.source(ctx, userId)
	if len(groups) == 0 {
		return groups
	}

	c.mu.Lock()
	// Kept only if nothing changed while it was being read, and stamped with
	// when the read STARTED, so the TTL measures the age of the answer.
	if c.live && c.gen == gen {
		if len(c.entries) >= membershipCacheMaxEntries {
			c.pruneLocked(started)
		}
		c.entries[key] = cachedGroups{groups: append([]string(nil), groups...), read: started}
	}
	c.mu.Unlock()
	return groups
}

// Invalidate drops every entry, and refuses every read still in flight the
// right to keep what it read.
func (c *MembershipCache) Invalidate() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.gen++
	clear(c.entries)
	c.mu.Unlock()
}

// setLive turns answering from memory on or off. Either way it drops every
// entry: an entry kept while nothing was listening for changes is not one to
// trust after.
func (c *MembershipCache) setLive(live bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.live = live
	c.gen++
	clear(c.entries)
	c.mu.Unlock()
}

// pruneLocked drops expired entries, then everything if that was not enough.
func (c *MembershipCache) pruneLocked(now time.Time) {
	for key, entry := range c.entries {
		if now.Sub(entry.read) >= c.ttl {
			delete(c.entries, key)
		}
	}
	if len(c.entries) >= membershipCacheMaxEntries {
		clear(c.entries)
	}
}

// installedMembershipCache is this process's cache, the one CachedGroups reads.
var installedMembershipCache = NewMembershipCache(InstalledGroups, MembershipCacheTTL, time.Now)

// InstalledMembershipCache returns this process's membership cache, for the
// wiring that subscribes it to membership changes.
func InstalledMembershipCache() *MembershipCache { return installedMembershipCache }

// CachedGroups resolves a person's groups through this process's membership
// cache: reused per person while the cache is subscribed to membership
// changes, a fresh read from the installed membership source otherwise.
func CachedGroups(ctx context.Context, userId string) []string {
	return installedMembershipCache.Groups(ctx, userId)
}

// ---------------------------------------------------------------------------
// The bus bridge
// ---------------------------------------------------------------------------

// MembershipCacheComponent is the bus-side ComponentName.
const MembershipCacheComponent = common.ComponentName("worker.membershipCache")

// MembershipCacheSubscriber drops a MembershipCache's entries on every
// membership or group change this replica hears. A Dependency, so app/ holds
// it beside the forward handler that reads the cache, in the
// RegistrationEventSubscriber shape.
type MembershipCacheSubscriber struct {
	logger      *slog.Logger
	bus         *events.Bus
	cache       *MembershipCache
	unsubscribe []func()
	readyCh     chan struct{}
	running     atomic.Bool
	startOnce   sync.Once
	stopOnce    sync.Once
}

// NewMembershipCacheSubscriber constructs the bridge. Subscribing happens in
// Start.
func NewMembershipCacheSubscriber(logger *slog.Logger, bus *events.Bus, cache *MembershipCache) *MembershipCacheSubscriber {
	if logger == nil {
		logger = slog.Default()
	}
	return &MembershipCacheSubscriber{
		logger:  logger.With("component", "worker.membershipCache"),
		bus:     bus,
		cache:   cache,
		readyCh: make(chan struct{}),
	}
}

// Start subscribes to every membership change and only then lets the cache
// answer from memory, so there is no moment when it answers and is not
// listening. Idempotent.
func (s *MembershipCacheSubscriber) Start(_ context.Context) {
	s.startOnce.Do(func() {
		defer close(s.readyCh)
		if s.bus == nil || s.cache == nil {
			s.logger.Warn("no event bus or cache -- a machine lent to a group re-reads the person's groups on every call")
			return
		}
		for _, topic := range MembershipInvalidationTopics() {
			s.unsubscribe = append(s.unsubscribe, s.bus.Subscribe(
				topic,
				func(events.Event) { s.cache.Invalidate() },
				events.WithSubscriberName("worker.membershipCache"),
			))
		}
		s.cache.setLive(true)
		s.running.Store(true)
		s.logger.Info("membership cache active", "ttl", MembershipCacheTTL.String())
	})
}

// Stop stops the cache answering from memory and only then unsubscribes: the
// reverse of Start, for the same reason.
func (s *MembershipCacheSubscriber) Stop(_ context.Context) {
	s.stopOnce.Do(func() {
		s.cache.setLive(false)
		for _, unsubscribe := range s.unsubscribe {
			unsubscribe()
		}
		s.running.Store(false)
	})
}

// Standard Dependency surface.
func (s *MembershipCacheSubscriber) IsRunning() bool { return s.running.Load() }
func (s *MembershipCacheSubscriber) Order() int      { return 10 }
func (s *MembershipCacheSubscriber) ComponentName() common.ComponentName {
	return MembershipCacheComponent
}
func (s *MembershipCacheSubscriber) Ready() <-chan struct{} { return s.readyCh }
