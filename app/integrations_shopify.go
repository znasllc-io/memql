package app

import (
	"context"
	stdsync "sync"
	"time"

	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/integrations/shopify"
)

// integrations_shopify.go -- the Shopify connector's start-up work.
//
// The connector REGISTERS itself as a plug-in like every other integration
// (integrations/shopify/plugin.go). What cannot go in a factory is the two
// things it has to do once the node is actually up: seed the first store row
// from the environment, and bring every store's webhook subscriptions in
// line. Both talk to the network and to the database, and a factory that did
// either would make plug-in registration -- and therefore boot -- fail
// because a merchant's store happened to be unreachable.
//
// So it is a DEPENDENCY, started after the engine, and its failures are
// warnings. The daily reconcile automation is the backstop: a store the
// connector could not reach at boot is picked up within a day, and the
// store's health says that it happened.

// shopifyBootstrapDelay lets the node finish coming up before the connector
// makes outbound calls. A subscription reconcile across every store is the
// largest burst of Admin traffic the connector ever produces, and running it
// while transports are still wiring competes with the readiness probe for no
// benefit.
const shopifyBootstrapDelay = 15 * time.Second

// shopifyBootstrap is the connector's lifecycle wrapper.
type shopifyBootstrap struct {
	app     *App
	ready   chan struct{}
	once    stdsync.Once
	mu      stdsync.Mutex
	running bool
	cancel  context.CancelFunc
}

func (a *App) registerShopifyConnector() {
	provider := a.engine.IntegrationByName(shopify.ConnectorName)
	if provider == nil {
		return
	}
	if _, ok := provider.(*shopify.Integration); !ok {
		return
	}
	a.Dependencies = append(a.Dependencies, &shopifyBootstrap{app: a, ready: make(chan struct{})})
}

func (s *shopifyBootstrap) ComponentName() common.ComponentName { return "shopify" }

// Order places the bootstrap after the engine and the database. It reads
// store rows and writes secrets, so neither being up yet is not a degraded
// mode, it is a crash.
func (s *shopifyBootstrap) Order() int { return 90 }

func (s *shopifyBootstrap) Ready() <-chan struct{} { return s.ready }

func (s *shopifyBootstrap) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

func (s *shopifyBootstrap) Start(ctx context.Context) {
	s.mu.Lock()
	s.running = true
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.cancel = cancel
	s.mu.Unlock()

	// READY IMMEDIATELY. The bootstrap is best-effort background work, and
	// a node whose readiness waited on a merchant's store would fail its
	// probe whenever Shopify was slow.
	s.once.Do(func() { close(s.ready) })

	go func() {
		select {
		case <-runCtx.Done():
			return
		case <-time.After(shopifyBootstrapDelay):
		}
		provider := s.app.engine.IntegrationByName(shopify.ConnectorName)
		integration, ok := provider.(*shopify.Integration)
		if !ok {
			return
		}
		shopify.Bootstrap(runCtx, integration, s.app.Logger)
	}()
}

func (s *shopifyBootstrap) Stop(context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = false
	if s.cancel != nil {
		s.cancel()
	}
}
