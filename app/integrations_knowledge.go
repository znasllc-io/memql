package app

import (
	"context"
	"sync"
	"time"

	"github.com/znasllc-io/memql/core/common"
	"github.com/znasllc-io/memql/integrations/knowledge"
)

// Publication maintenance repairs only exact existing registered seed chunks.
// It deliberately does not revive catalog creation or generate new knowledge.
func (a *App) registerKnowledgeSeedMaintenance() {
	integration, ok := a.engine.IntegrationByName("knowledge").(*knowledge.Integration)
	if !ok {
		return
	}
	a.Dependencies = append(a.Dependencies, &knowledgeSeedMaintenance{
		ready: make(chan struct{}), reconcile: integration.ReconcileCanonicalSeeds,
		warn: func(err error) { a.Logger.Warn("knowledge: canonical publication repair deferred", "error", err) },
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
func (s *knowledgeSeedMaintenance) Start(ctx context.Context) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	run, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.cancel, s.running = cancel, true
	s.mu.Unlock()
	s.once.Do(func() { close(s.ready) })
	go s.run(run)
}
func (s *knowledgeSeedMaintenance) run(ctx context.Context) {
	pass, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s.reconcile(pass); err != nil && ctx.Err() == nil {
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
