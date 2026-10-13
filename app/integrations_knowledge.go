package app

import (
	"context"
	"sync"
	"time"

	"github.com/znasllc-io/memql/component/auth"
	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/integrations/knowledge"
)

// Publish the explicit release seed selection before an owner or embedder exists.
// Indexing is retried by the bounded installed DSL schedule.
func (a *App) registerKnowledgeSeedMaintenance() {
	integration, ok := a.engine.IntegrationByName("knowledge").(*knowledge.Integration)
	if !ok {
		return
	}
	a.Dependencies = append(a.Dependencies, &knowledgeSeedMaintenance{
		ready: make(chan struct{}), reconcile: func(ctx context.Context) error {
			if err := integration.ReconcileCanonicalSeeds(ctx); err != nil {
				return err
			}
			return integration.ReconcileReleaseSeeds(ctx, false)
		},
		warn: func(err error) { a.Logger.Warn("knowledge: release knowledge publication deferred", "error", err) },
	})
}

type knowledgeSeedMaintenance struct {
	ready     chan struct{}
	once      sync.Once
	mu        sync.Mutex
	cancel    context.CancelFunc
	running   bool
	reconcile func(context.Context) error
	warn      func(error)
}

func (*knowledgeSeedMaintenance) ComponentName() common.ComponentName { return "knowledge-seed" }
func (*knowledgeSeedMaintenance) Order() int                          { return 90 }
func (s *knowledgeSeedMaintenance) Ready() <-chan struct{}            { return s.ready }
func (s *knowledgeSeedMaintenance) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}
func (s *knowledgeSeedMaintenance) Start(context.Context) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	// This is a server-started pass, never a continuation of an inbound actor
	// or origin. Stop owns its lifetime; the pass adds its own bounded deadline.
	run, cancel := context.WithCancel(context.Background())
	s.cancel, s.running = cancel, true
	s.mu.Unlock()
	s.once.Do(func() { close(s.ready) })
	go s.run(run)
}
func (s *knowledgeSeedMaintenance) run(ctx context.Context) {
	pass, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s.reconcile(auth.ContextWithInternalOrigin(pass)); err != nil && ctx.Err() == nil {
		s.warn(err)
	}
}
func (s *knowledgeSeedMaintenance) Stop(context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = false
	if s.cancel != nil {
		s.cancel()
	}
}
