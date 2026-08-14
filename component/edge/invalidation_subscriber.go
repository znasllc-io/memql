// component/edge/invalidation_subscriber.go -- cross-node cache invalidation
// for the site Resolver (memql#3714, Task 9).
package edge

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/znasllc-io/memql/component/events"
	"github.com/znasllc-io/memql/core/common"
)

// SiteInvalidationSubscriberComponent is the bus-side ComponentName.
const SiteInvalidationSubscriberComponent = common.ComponentName("edge.siteInvalidationSubscriber")

// sitePattern matches both graph.node.created.v1:platform:site and
// graph.node.updated.v1:platform:site in one subscription: "*" matches
// exactly one segment, and "v1:platform:site" carries no dots (concept ids
// use colons), so it is one logical segment either way. Both verbs matter --
// createSite is an insert(), while updateSiteBundle (the deploy/rollback
// operation) and updateSiteStatus (draft/live/disabled) both go through
// update() -- see dsl/platform/mutations.memql. graph.node.deleted is
// deliberately covered too, forward-compatibly: there is no deleteSite
// mutation today, so no such event can fire yet, but a resolver holding a
// stale row after a future delete would be the same bug class this file
// exists to prevent.
const sitePattern = "graph.node.*.v1:platform:site"

// invalidator is the narrow write the subscriber needs -- Resolver.Invalidate
// only, not the read side, so a test can stub it without a QueryExecutor.
type invalidator interface {
	Invalidate(hostname string)
}

// SiteInvalidationSubscriber bridges v1:platform:site concept events to the
// edge's per-replica Resolver cache. The flow:
//
//  1. A site row is created or updated -- wherever an admin surface writes
//     it, which is not necessarily this replica, or even this node type.
//  2. component/node's routing rules (graph.node.created.v1:platform:site /
//     graph.node.updated.v1:platform:site, see component/node/routing.go)
//     forward the event to EVERY node in the mesh, edge replicas included --
//     the cross-node hop that makes this more than a same-process cache
//     invalidation.
//  3. This subscriber matches the pattern, extracts the row's hostname from
//     the event payload, and calls Resolver.Invalidate so the NEXT request
//     for that hostname on THIS replica re-queries the engine instead of
//     serving a cached pre-write Site.
//
// The event payload carries the FULL merged row, not just the changed
// fields -- executeUpdate's read-merge-write publishes the merged payload on
// graph.node.updated (component/memql/executor_mutation.go), so `hostname`
// is present even when a write (e.g. updateSiteStatus) never touched it.
//
// This is the backstop's complement, not a replacement for it:
// MEMQL_EDGE_SITE_CACHE_TTL_SECONDS (resolve.go's NewResolver) still bounds
// staleness if an event is ever dropped -- a network partition during the
// forward, a replica that was down when it fired and misses the durable
// backbone's redelivery window. The TTL was the ONLY mechanism before this
// file; now it is the fallback for the invalidation path this file adds.
//
// Lifecycle is a Dependency so app/transport_edge.go can wire it in
// alongside the resolver it invalidates, the same shape
// observe.CodeProfileSubscriber uses for v1:observability:codeProfile.
type SiteInvalidationSubscriber struct {
	logger      *slog.Logger
	bus         *events.Bus
	resolver    invalidator
	unsubscribe func()
	doneCh      chan struct{}
	readyCh     chan struct{}
	running     atomic.Bool
	startOnce   sync.Once
	stopOnce    sync.Once
}

// NewSiteInvalidationSubscriber constructs the bridge. The events.Bus and
// Resolver are taken at construction time; the actual Subscribe call runs in
// Start so a not-yet-ready bus surfaces as a startup no-op (logged), not a
// constructor panic. Takes the narrow invalidator interface -- the only
// method this file calls -- rather than the full Resolver, mirroring
// edge.go's Engine interface ("kept to one method"); a caller passes its
// Resolver value in directly, since Resolver satisfies invalidator
// structurally.
func NewSiteInvalidationSubscriber(logger *slog.Logger, bus *events.Bus, resolver invalidator) *SiteInvalidationSubscriber {
	if logger == nil {
		logger = slog.Default()
	}
	return &SiteInvalidationSubscriber{
		logger:   logger.With("component", "edge.siteInvalidationSubscriber"),
		bus:      bus,
		resolver: resolver,
		doneCh:   make(chan struct{}),
		readyCh:  make(chan struct{}),
	}
}

// Start subscribes to the site event pattern. Idempotent.
func (s *SiteInvalidationSubscriber) Start(_ context.Context) {
	s.startOnce.Do(func() {
		defer close(s.readyCh)
		if s.bus == nil || s.resolver == nil {
			s.logger.Warn("no event bus or resolver available -- skipping site invalidation subscription")
			close(s.doneCh)
			return
		}
		s.running.Store(true)
		s.unsubscribe = s.bus.Subscribe(
			sitePattern,
			s.handle,
			events.WithSubscriberName("edge.siteInvalidationSubscriber"),
		)
		close(s.doneCh)
		s.logger.Info("site invalidation subscriber active", "pattern", sitePattern)
	})
}

// Stop closes the subscription.
func (s *SiteInvalidationSubscriber) Stop(_ context.Context) {
	s.stopOnce.Do(func() {
		if s.unsubscribe != nil {
			s.unsubscribe()
		}
		s.running.Store(false)
	})
}

// Standard Dependency surface so the app bootstrap can hold this alongside
// the rest of the component graph.
func (s *SiteInvalidationSubscriber) IsRunning() bool { return s.running.Load() }
func (s *SiteInvalidationSubscriber) Order() int      { return 10 }
func (s *SiteInvalidationSubscriber) ComponentName() common.ComponentName {
	return SiteInvalidationSubscriberComponent
}
func (s *SiteInvalidationSubscriber) Ready() <-chan struct{} { return s.readyCh }

// handle decodes the event payload and evicts the resolver's cached entry
// for the row's hostname. Tolerates missing / mistyped fields: log + drop
// rather than panicking on an unexpected shape. The payload follows the
// standard graph event envelope -- the row's payload object is under
// "payload" (see executor_mutation.go / executor_write.go).
func (s *SiteInvalidationSubscriber) handle(ev events.Event) {
	payload, ok := ev.Payload["payload"].(map[string]any)
	if !ok {
		s.logger.Debug("site event missing payload object", "topic", ev.Topic)
		return
	}
	hostname := stringField(payload, "hostname")
	if hostname == "" {
		s.logger.Debug("site event missing hostname", "topic", ev.Topic)
		return
	}
	s.resolver.Invalidate(hostname)
	s.logger.Info("site resolver cache invalidated",
		"hostname", hostname,
		"topic", ev.Topic,
		"originNode", ev.OriginNodeId,
	)
}

func stringField(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	if str, ok := v.(string); ok {
		return str
	}
	return fmt.Sprintf("%v", v)
}
